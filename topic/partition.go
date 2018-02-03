package topic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sirupsen/logrus"

	"github.com/tylertreat/BoomFilters"

	"github.com/celrenheit/sandflake"

	"github.com/celrenheit/sandglass-grpc/go/sgproto"
	"github.com/celrenheit/sandglass/sgutils"
	"github.com/celrenheit/sandglass/storage"
	"github.com/celrenheit/sandglass/storage/badger"
	"github.com/celrenheit/sandglass/storage/rocksdb"
	"github.com/celrenheit/sandglass/storage/scommons"
	"github.com/gogo/protobuf/proto"
	"github.com/willf/bloom"
)

var (
	ErrNoKeySet = errors.New("ErrNoKeySet")
)

type Partition struct {
	db       storage.Storage
	Id       string
	Replicas []string
	idgen    sandflake.Generator
	basepath string
	topic    *Topic

	bf  *bloom.BloomFilter // TODO: persist bloom filter
	ibf boom.Filter

	lastIndex  *uint64
	pendingKey []byte

	ctxPending    context.Context
	cancelPending context.CancelFunc
	wg            sync.WaitGroup
}

func (t *Partition) InitStore(basePath string) error {
	t.bf = bloom.NewWithEstimates(1e3, 1e-2)
	t.ibf = boom.NewInverseBloomFilter(1e3)
	msgdir := filepath.Join(basePath, t.Id)
	if err := sgutils.MkdirIfNotExist(msgdir); err != nil {
		return err
	}

	t.pendingKey = scommons.PrependPrefix(scommons.PendingPrefix, []byte{0})

	mo := &storage.MergeOperator{
		Key:       t.pendingKey,
		MergeFunc: mergeFunc,
	}

	var err error
	switch t.topic.StorageDriver {
	case sgproto.StorageDriver_Badger:
		t.db, err = badger.NewStorage(msgdir, mo)
	case sgproto.StorageDriver_RocksDB:
		t.db, err = rocksdb.NewStorage(msgdir, mo)
	default:
		return fmt.Errorf("unknown storage driver: %v for topic: %v", t.topic.StorageDriver, t.topic.Name)
	}
	if err != nil {
		return err
	}

	t.basepath = basePath

	var index uint64
	msg, err := t.LastMessage()
	if err != nil {
		return fmt.Errorf("unable to fetch last message for init partition: %v", err)
	}

	if msg != nil {
		index = msg.Index
	}
	t.lastIndex = &index

	t.LaunchPendingLoop()
	return nil
}

func mergeFunc(existing, value []byte) ([]byte, bool) {
	var operation sgproto.MergeOperation
	err := proto.Unmarshal(value, &operation)
	if err != nil {
		return nil, false
	}

	var state sgproto.MergeState
	err = proto.Unmarshal(existing, &state)
	if err != nil {
		return nil, false
	}

	switch operation.Operation {
	case sgproto.MergeOperation_APPEND:
		state.Messages = append(state.Messages, operation.Messages...)
	case sgproto.MergeOperation_CUT:
		if len(operation.Messages) <= int(operation.N) {
			state.Messages = nil
		} else {
			state.Messages = state.Messages[operation.N:]
		}
	}

	newState, err := proto.Marshal(&state)
	if err != nil {
		return nil, false
	}

	return newState, true
}

func (p *Partition) LaunchPendingLoop() {
	p.ctxPending, p.cancelPending = context.WithCancel(context.Background())
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		for {
			select {
			case <-p.ctxPending.Done():
				return
			case <-time.After(100 * time.Millisecond):
				if err := p.applyPendingToWal(); err != nil {
					logrus.WithError(err).Printf("apply pending loop")
				}
			}
		}
	}()
}

func (p *Partition) applyPendingToWal() error {
	fn := func(val []byte) ([]*storage.Entry, []byte, error) {
		var state sgproto.MergeState
		err := proto.Unmarshal(val, &state)
		if err != nil {
			return nil, nil, err
		}

		entries := []*storage.Entry{}
		for _, msg := range state.Messages {
			storagekey := p.getStorageKey(msg)
			val, err := proto.Marshal(msg)
			if err != nil {
				return nil, nil, err
			}
			//    t.bf.Add(storagekey)
			//    t.ibf.Add(storagekey)
			// entries = append(entries, &storage.Entry{
			// 	Key:   storagekey,
			// 	Value: val,
			// })
			entries = append(entries, &storage.Entry{
				Key:   p.newWALKey(storagekey, msg.Index),
				Value: val,
			})
		}

		operation, err := proto.Marshal(&sgproto.MergeOperation{
			Operation: sgproto.MergeOperation_CUT,
			N:         int32(len(state.Messages)),
		})
		if err != nil {
			return nil, nil, err
		}

		return entries, operation, nil
	}

	return p.db.ProcessMergedKey(p.pendingKey, fn)
}

func (p *Partition) WalToView(start, end uint64) error {
	entries := []*storage.Entry{}
	err := p.db.ForRangeWAL(start, end, func(msg *sgproto.Message) error {
		storagekey := p.getStorageKey(msg)

		b, err := proto.Marshal(msg)
		if err != nil {
			return err
		}

		entries = append(entries, &storage.Entry{
			Key:   storagekey,
			Value: b,
		})

		return nil
	})
	if err != nil {
		return err
	}

	return p.db.BatchPut(entries)
}

func (t *Partition) String() string {
	return t.Id
}

func (t *Partition) getStorageKey(msg *sgproto.Message) []byte {
	var storekey []byte
	switch t.topic.Kind {
	case sgproto.TopicKind_TimerKind:
		storekey = msg.Offset[:]
	case sgproto.TopicKind_KVKind:
		storekey = msg.Key
		if len(msg.ClusteringKey) > 0 {
			storekey = joinKeys(msg.Key, msg.ClusteringKey)
		}
	default:
		panic("INVALID STORAGE KIND: " + t.topic.Kind.String())
	}
	return scommons.PrependPrefix(scommons.ViewPrefix, storekey)
}

func (s *Partition) GetMessage(offset sgproto.Offset, k, suffix []byte) (*sgproto.Message, error) {
	switch s.topic.Kind {
	case sgproto.TopicKind_TimerKind:
		val, err := s.db.Get(scommons.PrependPrefix(scommons.ViewPrefix, offset[:]))
		if err != nil {
			return nil, err
		}

		var msg sgproto.Message
		err = proto.Unmarshal(val, &msg)
		if err != nil {
			return nil, err
		}

		return &msg, nil
	case sgproto.TopicKind_KVKind:
		val := s.db.LastKVForPrefix(scommons.PrependPrefix(scommons.ViewPrefix, k), suffix)
		if val == nil {
			return nil, nil
		}

		var msg sgproto.Message
		err := proto.Unmarshal(val, &msg)
		if err != nil {
			return nil, err
		}

		return &msg, nil
	default:
		return nil, fmt.Errorf("invalid storage kind: %s", s.topic.Kind.String())
	}
}

func (t *Partition) HasKey(key, clusterKey []byte) (bool, error) {
	switch t.topic.Kind {
	case sgproto.TopicKind_KVKind:
	default:
		return false, errors.New("HasKey should be used only with a KV topic")
	}

	pk := joinKeys(key, clusterKey)
	existKey := scommons.PrependPrefix(scommons.ViewPrefix, pk)

	if t.ibf.Test(existKey) {
		return true, nil
	}

	if !t.bf.Test(existKey) {
		return false, nil
	}

	msg, err := t.GetMessage(sgproto.Nil, pk, nil)
	if err != nil {
		return false, err
	}

	if msg == nil {
		return false, nil
	}

	return msg.Offset != sgproto.Nil, nil
}

func (t *Partition) newWALKey(prefix []byte, index uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, index)
	return bytes.Join([][]byte{scommons.WalPrefix, prefix, b}, storage.Separator)
}

func (t *Partition) PutMessage(msg *sgproto.Message) error {
	return t.BatchPutMessages([]*sgproto.Message{msg})
}

func (t *Partition) BatchPutMessages(msgs []*sgproto.Message) error {
	if len(msgs) == 0 {
		return nil
	}

	now := time.Now().UTC()

	var mo sgproto.MergeOperation
	mo.Operation = sgproto.MergeOperation_APPEND

	for _, msg := range msgs {
		if msg.Index == 0 {
			msg.Index = t.NextIndex()
		}
		if msg.Offset == sgproto.Nil {
			msg.Offset = sgproto.NewOffset(msg.Index, now.Add(msg.ConsumeIn))
		}
		msg.ProducedAt = now
	}
	mo.Messages = msgs

	b, err := proto.Marshal(&mo)
	if err != nil {
		return err
	}

	return t.db.Merge(t.pendingKey, b)
}

func (t *Partition) NextIndex() uint64 {
	return atomic.AddUint64(t.lastIndex, 1)
}

func (p *Partition) ForRange(min, max sgproto.Offset, fn func(msg *sgproto.Message) error) error {
	var lastKey []byte
	switch p.topic.Kind {
	case sgproto.TopicKind_TimerKind:
		return p.db.ForRange(min, max, func(msg *sgproto.Message) error {
			err := fn(msg)
			return err
		})
	case sgproto.TopicKind_KVKind:
		it := scommons.NewMessageIterator(p.db, &storage.IterOptions{
			FetchValues: true,
			Reverse:     true,
		})
		defer it.Close()
		for m := it.Rewind(); it.Valid(); m = it.Next() {
			if lastKey == nil || !bytes.Equal(m.Key, lastKey) {
				err := fn(m)
				if err != nil {
					return err
				}
				lastKey = m.Key
			}
		}
	default:
		panic("unknown topic kind: " + p.topic.Kind.String())
	}
	return nil
}

func (p *Partition) Close() error {
	if p.cancelPending != nil {
		p.cancelPending()
	}
	p.wg.Wait()
	return p.db.Close()
}

func (p *Partition) Iter() storage.MessageIterator {
	return scommons.NewMessageIterator(p.db, &storage.IterOptions{
		FetchValues: true,
		Reverse:     false,
	})
}

func (p *Partition) RangeFromWAL(min []byte, fn func(*sgproto.Message) error) error {
	return p.db.ForEachWALEntry(min, fn)
}

func (p *Partition) LastWALEntry() []byte {
	return p.db.LastKeyForPrefix(scommons.WalPrefix)
}

func (p *Partition) LastMessage() (*sgproto.Message, error) {
	value := p.db.LastKVForPrefix(scommons.WalPrefix, nil)
	if len(value) == 0 {
		return nil, nil
	}

	var msg sgproto.Message
	if err := proto.Unmarshal(value, &msg); err != nil {
		return nil, err
	}

	return &msg, nil
}

func (p *Partition) getMessageByStorageKey(k []byte) (*sgproto.Message, error) {
	b, err := p.db.Get(k)
	if err != nil {
		return nil, err
	}

	if b == nil {
		panic("should not happend, key in wal should always be also in msgs")
	}

	var msg sgproto.Message
	err = proto.Unmarshal(b, &msg)
	if err != nil {
		return nil, err
	}

	return &msg, nil
}

func joinKeys(key, clusterKey []byte) []byte {
	return bytes.Join([][]byte{
		key,
		clusterKey,
	}, storage.Separator)
}
