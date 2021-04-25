# treeIndex

treeIndex 是一个 B 树，存储了每个 key 的 keyIndex。整个实例被一个读写锁保护。

```go
  type treeIndex struct {
	sync.RWMutex
	tree *btree.BTree
  }
```

## 新示例

新建 treeIndex 的度数为 32。

```go
  func newTreeIndex(lg *zap.Logger) index {
	return &treeIndex{tree: btree.New(32)}
  }
```

## 添加

```go
  func (ti *treeIndex) Put(key []byte, rev revision) {
	keyi := &keyIndex{key: key}

	ti.Lock()
	defer ti.Unlock()
	item := ti.tree.Get(keyi)
	if item == nil { // item 不存在
	    // 构建一个新的 keyIndex
		keyi.put(ti.lg, rev.main, rev.sub)
		// 把 keyIndex 插入 tr.tree 
		ti.tree.ReplaceOrInsert(keyi)
		return
	}
	// 旧的 item 存在，则 append @rev 即可
	okeyi := item.(*keyIndex)
	okeyi.put(ti.lg, rev.main, rev.sub)
  }

  func (ti *treeIndex) Insert(ki *keyIndex) {
	ti.Lock()
	defer ti.Unlock()
	ti.tree.ReplaceOrInsert(ki)
  }
```

## 查找

```go
  func (ti *treeIndex) Get(key []byte, atRev int64) (modified, created revision, ver int64, err error) {
	keyi := &keyIndex{key: key}
	ti.RLock()
	defer ti.RUnlock()
	if keyi = ti.keyIndex(keyi); keyi == nil {
		return revision{}, revision{}, 0, ErrRevisionNotFound
	}
	return keyi.get(ti.lg, atRev)
  }
```

### 遍历

```go
  func (ti *treeIndex) Range(key, end []byte, atRev int64) (keys [][]byte, revs []revision) {
	if end == nil {
		rev, _, _, err := ti.Get(key, atRev)
		if err != nil {
			return nil, nil
		}
		return [][]byte{key}, []revision{rev}
	}
	ti.visit(key, end, func(ki *keyIndex) {
		if rev, _, _, err := ki.get(ti.lg, atRev); err == nil {
			revs = append(revs, rev)
			keys = append(keys, ki.key)
		}
	})
	return keys, revs
  }
```

## 压缩 

Compact 时先复制一遍 B 树，然后在旧 B 树上进行遍历，删除和更新操作则是在原有的 B 树上进行的。

compact 之后，如果 keyIndex 为空，则在 B 树上删除 key。

```go
  func (ti *treeIndex) Compact(rev int64) map[revision]struct{} {
	available := make(map[revision]struct{})

    // 复制 ti.tree 
	ti.Lock()
	clone := ti.tree.Clone()
	ti.Unlock()

    // 遍历 clone tree 的数据，但是删除/压缩动作是在 tr.tree 上进行的
	clone.Ascend(func(item btree.Item) bool {
		keyi := item.(*keyIndex)
		//Lock is needed here to prevent modification to the keyIndex while
		//compaction is going on or revision added to empty before deletion
		ti.Lock()
		keyi.compact(ti.lg, rev, available)
        // compact 之后，keyIndex 如果为空，则把其对应的 kv 删掉
		if keyi.isEmpty() {
			ti.tree.Delete(keyi)
		}
		ti.Unlock()
		return true
	})
	return available
  }
```

压缩还有一个比较奇特的函数，@treeIndex.Keep() 会遍历所有的 keyIndex，获取在每个 keyIndex 上 @rev 所在的 generation 压缩后剩余所有的 revision 的集合。 

```go
// Keep finds all revisions to be kept for a Compaction at the given rev.
func (ti *treeIndex) Keep(rev int64) map[revision]struct{} {
	available := make(map[revision]struct{})
	ti.RLock()
	defer ti.RUnlock()
	ti.tree.Ascend(func(i btree.Item) bool {
		keyi := i.(*keyIndex)
		keyi.keep(rev, available)
		return true
	})
	return available
}
```
