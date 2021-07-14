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
	"sort"
)

// txBuffer handles functionality shared between txWriteBuffer and txReadBuffer.
type txBuffer struct {
	buckets map[string]*bucketBuffer // key 是 bucket 名称
}

func (txb *txBuffer) reset() {
	for k, v := range txb.buckets {
		if v.used == 0 {
			// demote
			// 为空，则直接从 txb.buckets 中删除该 bucket
			delete(txb.buckets, k)
		}
		v.used = 0
	}
}

// txWriteBuffer buffers writes of pending updates that have not yet committed.
// 是否要求按照写入顺序，如果为 false，则要求 txBuffer 中的每个 bucketBuffer 按照字母顺序有序
// 默认为 true，详见 newBatchTxBuffered 函数
type txWriteBuffer struct {
	txBuffer
	seq bool
}

func (txw *txWriteBuffer) put(bucket, k, v []byte) {
	txw.seq = false
	txw.putSeq(bucket, k, v)
}

func (txw *txWriteBuffer) putSeq(bucket, k, v []byte) {
	b, ok := txw.buckets[string(bucket)]
	if !ok {
		b = newBucketBuffer()
		txw.buckets[string(bucket)] = b
	}
	b.add(k, v)
}

// 把 txw 中的内容清理掉，合并到 @txr
func (txw *txWriteBuffer) writeback(txr *txReadBuffer) {
	for k, wb := range txw.buckets {
		// 如果 txw 中的 bucket 在 txr 中不存在，则把 txw 中该 bucket 删除后放入 txr 中
		rb, ok := txr.buckets[k]
		if !ok {
			delete(txw.buckets, k)
			txr.buckets[k] = wb
			continue
		}
		// 如果 txw 中 buffer 讲究顺序，则进行排序
		if !txw.seq && wb.used > 1 {
			// assume no duplicate keys
			sort.Sort(wb)
		}
		rb.merge(wb)
	}
	txw.reset()
}

// txReadBuffer accesses buffered updates.
type txReadBuffer struct{ txBuffer }

func (txr *txReadBuffer) Range(bucketName, key, endKey []byte, limit int64) ([][]byte, [][]byte) {
	if b := txr.buckets[string(bucketName)]; b != nil {
		return b.Range(key, endKey, limit)
	}
	return nil, nil
}

func (txr *txReadBuffer) ForEach(bucketName []byte, visitor func(k, v []byte) error) error {
	if b := txr.buckets[string(bucketName)]; b != nil {
		return b.ForEach(visitor)
	}
	return nil
}

// unsafeCopy returns a copy of txReadBuffer, caller should acquire backend.readTx.RLock()
func (txr *txReadBuffer) unsafeCopy() txReadBuffer {
	txrCopy := txReadBuffer{
		txBuffer: txBuffer{
			buckets: make(map[string]*bucketBuffer, len(txr.txBuffer.buckets)),
		},
	}
	for bucketName, bucket := range txr.txBuffer.buckets {
		txrCopy.txBuffer.buckets[bucketName] = bucket.Copy()
	}
	return txrCopy
}

type kv struct {
	key []byte
	val []byte
}

// bucketBuffer buffers key-value pairs that are pending commit.
// bucketBuffer 中 kv 数目
type bucketBuffer struct {
	buf []kv
	// used tracks number of elements in use so buf can be reused without reallocation.
	used int // buf 的实际长度
}

func newBucketBuffer() *bucketBuffer {
	return &bucketBuffer{buf: make([]kv, 512), used: 0}
}

// 枚举 [key, endKey) 内的 kv 对，翻页限定为 @limit
//
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

func (bb *bucketBuffer) ForEach(visitor func(k, v []byte) error) error {
	for i := 0; i < bb.used; i++ {
		if err := visitor(bb.buf[i].key, bb.buf[i].val); err != nil {
			return err
		}
	}
	return nil
}

func (bb *bucketBuffer) add(k, v []byte) {
	bb.buf[bb.used].key, bb.buf[bb.used].val = k, v
	bb.used++
	if bb.used == len(bb.buf) {
		buf := make([]kv, (3*len(bb.buf))/2)
		copy(buf, bb.buf)
		bb.buf = buf
	}
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

func (bb *bucketBuffer) Len() int { return bb.used }
func (bb *bucketBuffer) Less(i, j int) bool {
	return bytes.Compare(bb.buf[i].key, bb.buf[j].key) < 0
}
func (bb *bucketBuffer) Swap(i, j int) { bb.buf[i], bb.buf[j] = bb.buf[j], bb.buf[i] }

func (bb *bucketBuffer) Copy() *bucketBuffer {
	bbCopy := bucketBuffer{
		buf:  make([]kv, len(bb.buf)),
		used: bb.used,
	}
	copy(bbCopy.buf, bb.buf)
	return &bbCopy
}
