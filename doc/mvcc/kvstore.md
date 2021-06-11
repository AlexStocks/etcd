# kvstore

```go
  type RangeOptions struct {
	Limit int64
	Rev   int64
	Count bool
  }
 
  type RangeResult struct {
	KVs   []mvccpb.KeyValue
	Rev   int64
	Count int
  }

  type ReadView interface {
    // 返回在开始事务开始时的第一个 kV 的 revision。如果发生了 compaction，则这个值也随之增加
	FirstRev() int64

    // 返回在打开事务时的 KV 的 revision
	Rev() int64

	// 返回在一个 rangeRev 范围内的 key 的集合。
	// 返回的 rev 是 KV 的当前的 revision。
	// If rangeRev <=0, range gets the keys at currentRev.
	// If `end` is nil, the request returns the key.
	// If `end` is not nil and not empty, it gets the keys in range [key, range_end).
	// If `end` is not nil and empty, it gets the keys greater than or equal to key.
	// Limit limits the number of keys returned.
	// If the required rev is compacted, ErrCompacted will be returned.
	Range(key, end []byte, ro RangeOptions) (r *RangeResult, err error)
  }

  // TxnRead 代表了只读事务，多个 TxnRead 可以并行。
  type TxnRead interface {
	ReadView
	// 代表标记事务结束，并准备 commit。
	End()
  }

  type WriteView interface {
	// DeleteRange 从 store 中删除给定范围内的 key。
	// A deleteRange increases the rev of the store if any key in the range exists.
	// The number of key deleted will be returned.
	// The returned rev is the current revision of the KV when the operation is executed.
	// It also generates one event for each key delete in the event history.
	// if the `end` is nil, deleteRange deletes the key.
	// if the `end` is not nil, deleteRange deletes the keys in range [key, range_end).
	DeleteRange(key, end []byte) (n, rev int64)

    // Put 把 @key 和 @value 存入 store 中。Put 会把 @lease 作为 metadata 的一部分存入。KV 实现并不检验 lease id 的有效性。
    // put 动作会增加 store 的 rev，并在 event history 中添加一个 event。
    // 返回值是当 Put 执行时的 revision 值。
	Put(key, value []byte, lease lease.LeaseID) (rev int64)
  }

  // TxnWrite represents a transaction that can modify the store.
  type TxnWrite interface {
	TxnRead
	WriteView
	// Changes gets the changes made since opening the write txn.
	Changes() []mvccpb.KeyValue
  }

  type KV interface {
	ReadView
	WriteView

	// Read creates a read transaction.
	Read(trace *traceutil.Trace) TxnRead

	// Write creates a write transaction.
	Write(trace *traceutil.Trace) TxnWrite

	// Hash computes the hash of the KV's backend.
	Hash() (hash uint32, revision int64, err error)

	// HashByRev computes the hash of all MVCC revisions up to a given revision.
	HashByRev(rev int64) (hash uint32, revision int64, compactRev int64, err error)

	// Compact frees all superseded keys with revisions less than rev.
	Compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error)

	// Commit commits outstanding txns into the underlying backend.
	Commit()

	// Restore restores the KV store from a backend.
	Restore(b backend.Backend) error
	Close() error
  }
```

## 2 写事务 storeTxnWrite

创建写事务，在事务开始时加锁，在结束时释放锁。

```go
  // mvcc/kvstore_txn.go
  func (s *store) Write(trace *traceutil.Trace) TxnWrite {
	s.mu.RLock()
	tx := s.b.BatchTx()
	tx.Lock() // 加锁
	tw := &storeTxnWrite{
		storeTxnRead: storeTxnRead{s, tx, 0, 0, trace},
		tx:           tx,
		beginRev:     s.currentRev,
		changes:      make([]mvccpb.KeyValue, 0, 4),
	}
	return newMetricsTxnWrite(tw)
  }

  func (tw *storeTxnWrite) End() {
	// only update index if the txn modifies the mvcc state.
	if len(tw.changes) != 0 {
		tw.s.saveIndex(tw.tx)
		// hold revMu lock to prevent new read txns from opening until writeback.
		tw.s.revMu.Lock()
		tw.s.currentRev++ // 结束时增加，用于赋值 Revision
	}
	tw.tx.Unlock() // 解锁
	if len(tw.changes) != 0 {
		tw.s.revMu.Unlock()
	}
	tw.s.mu.RUnlock()
  }
```

### 添加

```go
  // mvcc/kvstore_txn.go
  type storeTxnWrite struct {
	storeTxnRead
	tx backend.BatchTx
	// beginRev is the revision where the txn begins; it will write to the next revision.
	beginRev int64
	changes  []mvccpb.KeyValue
  }

  func (tw *storeTxnWrite) put(key, value []byte, leaseID lease.LeaseID) {
    // 因为写是串行执行，且 storeTxnWrite 开始的时候已经加锁，所以此处可以安全增 1
	rev := tw.beginRev + 1
	c := rev
	oldLease := lease.NoLease

    // 如果 key 已经存在，则使用其创建时候的 lease
	_, created, ver, err := tw.s.kvindex.Get(key, rev)
	if err == nil {
		c = created.main
		oldLease = tw.s.le.GetLease(lease.LeaseItem{Key: string(key)})
	}
	ibytes := newRevBytes()
    // main 取值最新事务 ID，sub 则取值写次数
	idxRev := revision{main: rev, sub: int64(len(tw.changes))}
	revToBytes(idxRev, ibytes)

	ver = ver + 1
	kv := mvccpb.KeyValue{
		Key:            key,
		Value:          value,
		CreateRevision: c,     // 创建时的 revision
		ModRevision:    rev,   // 本次修改的 revision
		Version:        ver,
		Lease:          int64(leaseID),
	}

	d := kv.Marshal()
	tw.trace.Step("marshal mvccpb.KeyValue")
    // 把版本 revision + kv 数据 d 存入 bolt 的 "key" bucket 内
	tw.tx.UnsafeSeqPut(keyBucketName, ibytes, d)
    // 把 key + 版本 revision 存入 treeIndex 中 
	tw.s.kvindex.Put(key, idxRev)
    // 把 kv 存入 tw.changes 中
	tw.changes = append(tw.changes, kv)
	tw.trace.Step("store kv pair into bolt db")

    // 与老的 lease 解绑 
	if oldLease != lease.NoLease {
		tw.s.le.Detach(oldLease, []lease.LeaseItem{{Key: string(key)}})
	}
    // 与新的 lease 绑定
	if leaseID != lease.NoLease {
		tw.s.le.Attach(leaseID, []lease.LeaseItem{{Key: string(key)}})
	}
	tw.trace.Step("attach lease to kv pair")
}
```

### 删除 

即使是删除，也被 etcd 当做一次写入操作，凑成有特殊标记的 key 存入 keyIndex 和 bolt 中。后面使用 compact 这个 gc 机制进行回收。

```go
  // 在revision 末尾加上 tombstone 标记 't'
  func appendMarkTombstone(lg *zap.Logger, b []byte) []byte {
	return append(b, markTombstone)
  }

  func (tw *storeTxnWrite) delete(key []byte) {
    // 因为写是串行执行，且 storeTxnWrite 开始的时候已经加锁，所以此处可以安全增 1
	ibytes := newRevBytes()
	idxRev := revision{main: tw.beginRev + 1, sub: int64(len(tw.changes))}
	revToBytes(idxRev, ibytes)
    // 在 key 上标记 tombstone 标记
	ibytes = appendMarkTombstone(tw.storeTxnRead.s.lg, ibytes)

    // 这里只需要存储 key 即可，因为这个 key 马上要被删掉了
	kv := mvccpb.KeyValue{Key: key}

	d := kv.Marshal()
    // 把版本号作为 key ，把用户的 key 作为 value，存入 boltdb 的 "key" bucket
	tw.tx.UnsafeSeqPut(keyBucketName, ibytes, d)
    // 对 kvindex 进行 tombstone 设定：结束一个 generation 
	tw.s.kvindex.Tombstone(key, idxRev)
    // 存入 changes 记录
	tw.changes = append(tw.changes, kv)

    // 与 lease 解绑
	item := lease.LeaseItem{Key: string(key)}
	leaseID := tw.s.le.GetLease(item)
	if leaseID != lease.NoLease {
		tw.s.le.Detach(leaseID, []lease.LeaseItem{item})
	}
  }

  func (tw *storeTxnWrite) deleteRange(key, end []byte) int64 {
	rrev := tw.beginRev
	if len(tw.changes) > 0 {
		rrev++
	}
	keys, _ := tw.s.kvindex.Range(key, end, rrev)
	if len(keys) == 0 {
		return 0
	}
	for _, key := range keys {
		tw.delete(key)
	}
	return int64(len(keys))
  }
```

## 读事务 storeTxnRead

```go
  // mvcc/kvstore_txn.go
  type storeTxnRead struct {
	s  *store // 所在的存储
	tx backend.ReadTx

	firstRev int64
	rev      int64
  }

  // mvcc/kvstore_txn.go
  // 在读开始时，加上并行读锁，在 mvcc/backend/backend.go:backend.ConcurrentReadTx() 返回的是一个指针事务对象
  // 所以下面是先生成事务锁，上锁后，在把事务锁放入 storeTxnRead 中进行存储
  func (s *store) Read(trace *traceutil.Trace) TxnRead {
	s.mu.RLock()
	s.revMu.RLock()
	// backend holds b.readTx.RLock() only when creating the concurrentReadTx. After
	// ConcurrentReadTx is created, it will not block write transaction.
	tx := s.b.ConcurrentReadTx()
	tx.RLock() // RLock is no-op. concurrentReadTx does not need to be locked after it is created.
	firstRev, rev := s.compactMainRev, s.currentRev
	s.revMu.RUnlock()
	return newMetricsTxnRead(&storeTxnRead{s, tx, firstRev, rev, trace})
  }

  // mvcc/kvstore_txn.go
  // 事务结束，释放锁 
  func (tr *storeTxnRead) End() {
	tr.tx.RUnlock() // RUnlock signals the end of concurrentReadTx.
	tr.s.mu.RUnlock()
  }
```

### 遍历

```go
  func (tr *storeTxnRead) rangeKeys(key, end []byte, curRev int64, ro RangeOptions) (*RangeResult, error) {
    // 矫正版本号
	rev := ro.Rev
	if rev > curRev {
		return &RangeResult{KVs: nil, Count: -1, Rev: curRev}, ErrFutureRev
	}
	if rev <= 0 {
		rev = curRev
	}
	if rev < tr.s.compactMainRev {
		return &RangeResult{KVs: nil, Count: -1, Rev: 0}, ErrCompacted
	}

    // 读取[key, end) 范围内在 @rev 后的所有版本号序列
	revpairs := tr.s.kvindex.Revisions(key, end, rev)
	tr.trace.Step("range keys from in-memory index tree")
	if len(revpairs) == 0 {
		return &RangeResult{KVs: nil, Count: 0, Rev: curRev}, nil
	}
	if ro.Count {
		return &RangeResult{KVs: nil, Count: len(revpairs), Rev: curRev}, nil
	}

	limit := int(ro.Limit)
	if limit <= 0 || limit > len(revpairs) {
		limit = len(revpairs)
	}

	kvs := make([]mvccpb.KeyValue, limit)
	revBytes := newRevBytes()
	for i, revpair := range revpairs[:len(kvs)] {
		revToBytes(revpair, revBytes)
		_, vs := tr.tx.UnsafeRange(keyBucketName, revBytes, nil, 0)
		kvs[i].Unmarshal(vs[0])
	}
	tr.trace.Step("range keys from bolt db")
	return &RangeResult{KVs: kvs, Count: len(revpairs), Rev: curRev}, nil
  }
```
 


