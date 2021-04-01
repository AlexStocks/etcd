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

## 3 BatchTx & batchTxBuffered

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

### 3.1 bucketBuffer

bucketBuffer 是一个有序 kv 数组，bucketBuffer.used 指示当前 bucketBuffer.buf 的 size。bucketBuffer 每次扩容只扩 1.5 倍容量。

```go
// mvcc/backend/tx_buffer.go
type kv struct {
	key []byte
	val []byte
}

// bucketBuffer buffers key-value pairs that are pending commit.
type bucketBuffer struct {
	buf []kv
	// used tracks number of elements in use so buf can be reused without reallocation.
	used int
}
```

bucketBuffer 的 range 和 merge 函数要求 buffer 中的 kv 数组是有序的。

```go
// 枚举 [key, endKey) 内的 kv 对，翻页限定为 @limit

// 返回 [key, endKey) 范围内的 kv；
// 如果 @key 找不到，则退出；
// 如果 @endKey 为空，则返回 @key 对应的 kv；
// 如果 endKey <= key，则返回空；
// key < endKey，则按照翻页参数 @limit 返回相应的 kv 对。
func (bb *bucketBuffer) Range(key, endKey []byte, limit int64) (keys [][]byte, vals [][]byte) {
	f := func(i int) bool { return bytes.Compare(bb.buf[i].key, key) >= 0 }
	// 找到 key 第一次出现的位置
	idx := sort.Search(bb.used, f)
	if idx < 0 { // 没找到，则直接退出
		return nil, nil
	}
	if len(endKey) == 0 { // endKey 为空，则说明只取值 @key 对应的 kv
		if bytes.Equal(key, bb.buf[idx].key) {
			keys = append(keys, bb.buf[idx].key)
			vals = append(vals, bb.buf[idx].val)
		}
		return keys, vals
	}
	// 如果 @endKey 小于 @key，则参数不合法，退出
	if bytes.Compare(endKey, bb.buf[idx].key) <= 0 {
		return nil, nil
	}
	// 返回 [key, endKey) 内的 kv
	for i := idx; i < bb.used && int64(len(keys)) < limit; i++ {
		if bytes.Compare(endKey, bb.buf[i].key) <= 0 {
			break
		}
		keys = append(keys, bb.buf[i].key)
		vals = append(vals, bb.buf[i].val)
	}
	return keys, vals
}

// merge merges data from bb into bbsrc.
func (bb *bucketBuffer) merge(bbsrc *bucketBuffer) {
	for i := 0; i < bbsrc.used; i++ {
		bb.add(bbsrc.buf[i].key, bbsrc.buf[i].val)
	}
	// bb 原来为空，直接退出即可
	if bb.used == bbsrc.used {
		return
	}
	// 比较 bb 原来最后一个 elem 与 bbsrc 的第一个 elem，如果 bb.last < bbsrc.first 则
	// 可以保证新顺序就是有序的，不必排序退出即可
	if bytes.Compare(bb.buf[(bb.used-bbsrc.used)-1].key, bbsrc.buf[0].key) < 0 {
		return
	}

	sort.Stable(bb)

	// remove duplicates, using only newest update
	// 去重
	widx := 0
	for ridx := 1; ridx < bb.used; ridx++ {
		if !bytes.Equal(bb.buf[ridx].key, bb.buf[widx].key) {
			// 举个栗子，初始 widx 为 0，ridx 为 1，如果前后两个 idx 不相等，则 widx 自增后也为 1
			widx++
		}
		bb.buf[widx] = bb.buf[ridx]
	}
	bb.used = widx + 1
}
```

### 3.2 txBuffer

txBuffer 只有一个函数 reset()，这个函数会删除容量为 0 的 bucketBuffer。

```go
// txBuffer handles functionality shared between txWriteBuffer and txReadBuffer.
type txBuffer struct {
	buckets map[string]*bucketBuffer
}

func (txb *txBuffer) reset() {
	for k, v := range txb.buckets {
		if v.used == 0 {
			// demote
			delete(txb.buckets, k)
		}
		v.used = 0
	}
}
```

### 3.3 txWriteBuffer

txWriteBuffer 用于 etcd 延迟写，其中的 seq 决定了对 txBuffer 中的 elem 是否进行排序。seq 为 true，则不排序，按照先后哦加入的顺序就是有序。

```go
// txWriteBuffer buffers writes of pending updates that have not yet committed.
type txWriteBuffer struct {
	txBuffer
	seq bool
}

type batchTxBuffered struct {
	batchTx
	buf txWriteBuffer
}
```

`newBatchTxBuffered()` 函数创建 batchTxBuffered.buf 时，其中的 batchTxBuffered.buf.seq 为 true。 

在函数 `txWriteBuffer.put()` 中，设置 `txWriteBuffer.seq` 为 false，在 `txWriteBuffer.putSeq()` 中则直接调用 `bucketBuffer.add()`，把 kv 添加到数组末尾。

```go
func (txw *txWriteBuffer) writeback(txr *txReadBuffer) {
	for k, wb := range txw.buckets {
		rb, ok := txr.buckets[k]
        // 如果 txr 中不存在同样的 key，则把 bucket 从 txw 中删除并存入 @txr 中
		if !ok {
			delete(txw.buckets, k)
			txr.buckets[k] = wb
			continue
		}

        // txr 中存在同样的 key，txw.seq 为false，则先确保 txw 中该 buffer 有序
		if !txw.seq && wb.used > 1 {
			// assume no duplicate keys
			sort.Sort(wb)
		}
		// 把 wb 合并进 rb
		rb.merge(wb)
	}
	txw.reset()
}
```

### 3.4 txReadBuffer

txReadBuffer 等同于 txBuffer，但是只有只读接口，没有提供写接口。

```go
// txReadBuffer accesses buffered updates.
type txReadBuffer struct{ txBuffer }
```

### 3.5 batchTxBuffered

`batchTxBuffered` 是在 `txWriteBuffer` 之上，加入了 `batchTx`。

```go
type batchTxBuffered struct {
	batchTx
	buf txWriteBuffer
}
```

写数据时同时写入 `batchTxBuffered.batchTx` 和 `batchTxBuffered.txWriteBuffer` 中，如：

```go
func (t *batchTxBuffered) UnsafePut(bucketName []byte, key []byte, value []byte) {
	t.batchTx.UnsafePut(bucketName, key, value)
	t.buf.put(bucketName, key, value)
}

func (t *batchTxBuffered) UnsafeSeqPut(bucketName []byte, key []byte, value []byte) {
	t.batchTx.UnsafeSeqPut(bucketName, key, value)
	t.buf.putSeq(bucketName, key, value)
}
```

在执行 `batchTxBuffered.Unlock()` 函数时，`batchTxBuffered.txWriteBuffer` 中的数据交换到 `t.backend.readTx.buf` 中。

```go
func (t *batchTxBuffered) Unlock() {
	if t.pending != 0 {
		t.backend.readTx.Lock() // blocks txReadBuffer for writing.
		t.buf.writeback(&t.backend.readTx.buf)
		t.backend.readTx.Unlock()
		if t.pending >= t.backend.batchLimit {
			t.commit(false)
		}
	}
	t.batchTx.Unlock()
}
```

## ReadTx

```go
type readTx struct {
	// mu protects accesses to the txReadBuffer
	mu  sync.RWMutex
	buf txReadBuffer

	// TODO: group and encapsulate {txMu, tx, buckets, txWg}, as they share the same lifecycle.
	// txMu protects accesses to buckets and tx on Range requests.
	txMu    sync.RWMutex
	tx      *bolt.Tx
	buckets map[string]*bolt.Bucket
	// txWg protects tx from being rolled back at the end of a batch interval until all reads using this tx are done.
	txWg *sync.WaitGroup
}
```

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