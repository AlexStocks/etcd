## Etcd MVCC

## 1 核心问题

* etcd 如何使用 bbolt 存储 key-value？
* etcd 如何保证数据读写的事务性？

## 2 关键数据结构

磁盘存储层分为内存的 MVCC 和 磁盘的 Backend(bbolt) 两层，Backend 下层具体的读写支持是由 BatchTx 提供的。Batch 的意思是写事务的批量提交。

```go
// mvcc/backend/backend.go 
type Backend interface {
	// 创建一个批量事务
	BatchTx() BatchTx
}

// mvcc/backend/batch_tx.go
type BatchTx interface {
	ReadTx
	UnsafeCreateBucket(name []byte)
	UnsafePut(bucketName []byte, key []byte, value []byte)
	UnsafeSeqPut(bucketName []byte, key []byte, value []byte)
	UnsafeDelete(bucketName []byte, key []byte)
	// Commit commits a previous tx and begins a new writable one.
	Commit()
	// CommitAndStop commits the previous tx and does not create a new one.
	CommitAndStop()
}
```

BatchTx 所有的 Unsafe* 都是基于 bbolt 的读写接口的 wrapper。之所以是 Unsafe，这些因为直接调用该接口结束后的事务不一定能立即提交，而是异步提交，数据并没有立即罗盘，所以相关操作是 unsafe 的。

etcd 的所有的 key 都保存在名为 `key` 的 bucket 中，而元数据则保存在名为 `meta` 的 bucket 中。

`BatchTx.UnsafePut()` 和 `BatchTx.UnsafeSeqPut` 之间的区别在于是否顺序写，如果是顺序写则 将 bbolt 中 bucket 的填充率（fill percent）设置为 90%，这在大部分都是 append-only 的操作中可有效暂缓 page 的分裂并减少存储空间（这部分细节后面专门聊 B+ Tree 和 bbolt 的时候再说，可以认为标记是否为顺序写可有效提升性能）。

## 3 BatchTx

BatchTx 接口的实现者是 batchTx：

```go
// mvcc/backend/batch_tx.go
type batchTx struct {
	sync.Mutex
	tx      *bolt.Tx // 更底层的 bolt tx
	backend *backend // 引用其所对应的 backend

	pending int
}
```

batchTx 其他所有的实现都是对 bolt 相关函数的封装，这里不再详述。此处主要描述下 commit 函数的逻辑：

```go
// mvcc/backend/batch_tx.go
// stop 为 true，不开启新事务，其为 false 则开启新事务
func (t *batchTx) commit(stop bool) {
	// commit the last tx
	if t.tx != nil {
		if t.pending == 0 && !stop {
			return
		}
        // tx 存在，则直接 commit
		err := t.tx.Commit()
		atomic.AddInt64(&t.backend.commits, 1)
		t.pending = 0
	}
	if !stop { // tx 不存在，且 !stop，则创建 tx
		t.tx = t.backend.begin(true)
	}
}
```

batchTx.Commit() 和 batchTx.CommitAndStop() 之间的区别在于前者完成以前的事务并开启新的事务，而后者在完成以前的事务后就结束了。

## 4 Backend

Backend 通过批量化和异步化提高写性能：

* 1 执行 BatchTx 接口时候必须先获取锁 BatchTx.Lock() 和 BatchTx.UnLock()；
* 2 每次写操作都累积一个操作数，当累积的操作数量达到一定阈值的时候，执行 commit 动作提交一个事务，这动作发生在 BatchTx.UnLock() 阶段；

达到一定阈值进行写事务 commit 的代码如下： 
```go
// mvcc/backend/batch_tx.go
func (t *batchTx) Unlock() {
	if t.pending >= t.backend.batchLimit {
		t.commit(false)
	}
	t.Mutex.Unlock()
}
```

* 3 每间隔一段时间（默认是 100ms）批量提交事务。这部分逻辑是启动一个 goroutine 进行的，所以不会阻塞主逻辑，达到异步化的目的；

backend 定时地批量提交写事务，定时时长一般为 100ms，定时逻辑如下：

```go
// mvcc/backend/backend.go
func newBackend(bcfg BackendConfig) *backend {
	db := bolt.Open(bcfg.Path, 0600, bopts)
	b := &backend{
		db: db,
	}
	b.batchTx = newBatchTxBuffered(b)
	go b.run() // 在一个独立的 gr 中执行 backend.run()
	return b
}

func (b *backend) run() {
	defer close(b.donec)
	t := time.NewTimer(b.batchInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-b.stopc:
			// 如果 backend 面临退出，则会在最后执行 b.batchTx.CommitAndStop()。
			b.batchTx.CommitAndStop()
			return
		}
		if b.batchTx.safePending() != 0 {
			b.batchTx.Commit()
		}
		t.Reset(b.batchInterval)
	}
}
```