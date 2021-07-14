// Copyright 2017 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backend

import (
	"bytes"
	"math"
	"sync"

	bolt "go.etcd.io/bbolt"
)

// safeRangeBucket is a hack to avoid inadvertently reading duplicate keys;
// overwrites on a bucket should only fetch with limit=1, but safeRangeBucket
// is known to never overwrite any key so range is safe.
var safeRangeBucket = []byte("key")

type ReadTx interface {
	Lock()
	Unlock()
	RLock()
	RUnlock()

	UnsafeRange(bucketName []byte, key, endKey []byte, limit int64) (keys [][]byte, vals [][]byte)
	UnsafeForEach(bucketName []byte, visitor func(k, v []byte) error) error
}

// 提供了对各个 bucket 读操作的封装。
// 其中包含了两块，一部分是 cache，另一部分是真实 db 的 bucket 的句柄。
// readTx 中有两个 lock：mu 和 txMu，readTx.mu 保护对象是 readTx.buf，readTx.txMu 保护 readTx.buckets
// txMu 只有在查询 bucket 或者变更 bucket 的时候才会进去临界区。
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
	// boltdb  的读写不是并行的，进行写之前先保证所有读动作都结束
	txWg *sync.WaitGroup
}

// readTx 把 readTx.buf 锁的使用交给了上层，提供的两个 range 函数是 unsafe 的。
func (rt *readTx) Lock()    { rt.mu.Lock() }
func (rt *readTx) Unlock()  { rt.mu.Unlock() }
func (rt *readTx) RLock()   { rt.mu.RLock() }
func (rt *readTx) RUnlock() { rt.mu.RUnlock() }

func (rt *readTx) UnsafeRange(bucketName, key, endKey []byte, limit int64) ([][]byte, [][]byte) {
	if endKey == nil {
		// forbid duplicates for single keys
		limit = 1
	}
	if limit <= 0 {
		limit = math.MaxInt64
	}
	if limit > 1 && !bytes.Equal(bucketName, safeRangeBucket) {
		panic("do not use unsafeRange on non-keys bucket")
	}
	// 先读取内存读 buffer
	keys, vals := rt.buf.Range(bucketName, key, endKey, limit)
	if int64(len(keys)) == limit {
		// 如果 key 数目达到 @limit，则返回
		return keys, vals
	}

	// find/cache bucket
	bn := string(bucketName)
	rt.txMu.RLock()
	bucket, ok := rt.buckets[bn]
	rt.txMu.RUnlock()
	if !ok {
		rt.txMu.Lock()
		bucket = rt.tx.Bucket(bucketName)
		rt.buckets[bn] = bucket
		rt.txMu.Unlock()
	}

	// ignore missing bucket since may have been created in this batch
	if bucket == nil {
		return keys, vals
	}
	rt.txMu.Lock()
	c := bucket.Cursor()
	rt.txMu.Unlock()

	// 从磁盘读取一部分数据
	k2, v2 := unsafeRange(c, key, endKey, limit-int64(len(keys)))
	return append(k2, keys...), append(v2, vals...)
}

func (rt *readTx) UnsafeForEach(bucketName []byte, visitor func(k, v []byte) error) error {
    // block1：标识在 rt.buf 中存在的 kv，并存入 map dups 中
	// 标识 rt.buf 中存在的数据
	dups := make(map[string]struct{})
	getDups := func(k, v []byte) error {
		dups[string(k)] = struct{}{}
		return nil
	}
	// 如果 rt.buf 中存在该 kv，则先不访问
	visitNoDup := func(k, v []byte) error {
		if _, ok := dups[string(k)]; ok {
			return nil
		}
		return visitor(k, v)
	}
	if err := rt.buf.ForEach(bucketName, getDups); err != nil {
		return err
	}

	// block2: 遍历 boltdb 中的 kv，但是 rt.buf 中存在的，先不访问
	rt.txMu.Lock()
	err := unsafeForEach(rt.tx, bucketName, visitNoDup)
	rt.txMu.Unlock()
	if err != nil {
		return err
	}

	// block3: 访问 rt.buf 中的 kv
	return rt.buf.ForEach(bucketName, visitor)
}

func (rt *readTx) reset() {
	rt.buf.reset()
	rt.buckets = make(map[string]*bolt.Bucket)
	rt.tx = nil
	rt.txWg = new(sync.WaitGroup)
}

// TODO: create a base type for readTx and concurrentReadTx to avoid duplicated function implementation?
// concurrentReadTx 是每次读单独使用，使用完毕即销毁，所以对其 buf 不用加锁
type concurrentReadTx struct {
	buf     txReadBuffer
	txMu    *sync.RWMutex
	tx      *bolt.Tx
	buckets map[string]*bolt.Bucket
	txWg    *sync.WaitGroup
}

func (rt *concurrentReadTx) Lock()   {}
func (rt *concurrentReadTx) Unlock() {}

// RLock is no-op. concurrentReadTx does not need to be locked after it is created.
func (rt *concurrentReadTx) RLock() {}

// RUnlock signals the end of concurrentReadTx.
func (rt *concurrentReadTx) RUnlock() { rt.txWg.Done() }

func (rt *concurrentReadTx) UnsafeForEach(bucketName []byte, visitor func(k, v []byte) error) error {
	dups := make(map[string]struct{})
	getDups := func(k, v []byte) error {
		dups[string(k)] = struct{}{}
		return nil
	}
	visitNoDup := func(k, v []byte) error {
		if _, ok := dups[string(k)]; ok {
			return nil
		}
		return visitor(k, v)
	}
	if err := rt.buf.ForEach(bucketName, getDups); err != nil {
		return err
	}
	rt.txMu.Lock()
	err := unsafeForEach(rt.tx, bucketName, visitNoDup)
	rt.txMu.Unlock()
	if err != nil {
		return err
	}
	return rt.buf.ForEach(bucketName, visitor)
}

func (rt *concurrentReadTx) UnsafeRange(bucketName, key, endKey []byte, limit int64) ([][]byte, [][]byte) {
	if endKey == nil {
		// forbid duplicates for single keys
		limit = 1
	}
	if limit <= 0 {
		limit = math.MaxInt64
	}
	if limit > 1 && !bytes.Equal(bucketName, safeRangeBucket) {
		panic("do not use unsafeRange on non-keys bucket")
	}
	keys, vals := rt.buf.Range(bucketName, key, endKey, limit)
	if int64(len(keys)) == limit {
		return keys, vals
	}

	// find/cache bucket
	bn := string(bucketName)
	rt.txMu.RLock()
	bucket, ok := rt.buckets[bn]
	rt.txMu.RUnlock()
	if !ok {
		rt.txMu.Lock()
		bucket = rt.tx.Bucket(bucketName)
		rt.buckets[bn] = bucket
		rt.txMu.Unlock()
	}

	// ignore missing bucket since may have been created in this batch
	if bucket == nil {
		return keys, vals
	}
	rt.txMu.Lock()
	c := bucket.Cursor()
	rt.txMu.Unlock()

	k2, v2 := unsafeRange(c, key, endKey, limit-int64(len(keys)))
	return append(k2, keys...), append(v2, vals...)
}
