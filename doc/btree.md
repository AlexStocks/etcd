# bree 

## API

> func New

* func New(degree int) *BTree
 
 创建一个度数为 @degree 的 B 树。例如 New(2), 能创建出一个 2-3-4 树，每个 node 包含 1-3 items 和 2-4 children。

* func NewWithFreeList(degree int, f *FreeList) *BTree

> func (*BTree) Ascend

* func (t *BTree) Ascend(iterator ItemIterator)

遍历范围是 [first, last]。

* func (t *BTree) AscendGreaterOrEqual(pivot Item, iterator ItemIterator)

遍历范围 [pivot, last]

* func (t *BTree) AscendLessThan(pivot Item, iterator ItemIterator)

遍历范围 [first, pivot)。

* func (t *BTree) AscendRange(greaterOrEqual, lessThan Item, iterator ItemIterator)

遍历范围是 [greaterOrEqual, lessThan)。

> func (*BTree) Clear

* func (t *BTree) Clear(addNodesToFreelist bool)

如果 addNodesToFreelist 设置为 true，则所有 node 都被放入 freelist，直到 freelist 溢出为止，否则就让 Go gc 自己回首所有的 node。

这将远比逐个 Delete element 快的多，因为删除一个 element 的过程牵涉到 查找/删除 element 并更新 B 树。这也将比创建一个新 B 树替代老 B 树快得多，因为老 B 树释放到 freelist 的 node 可被新 B 树使用，而不是释放到 gc。

有如下操作的复杂度：

O(1): addNodesToFreelist 被设定为 false 时
O(1): 当 freelist 为空时
O(freelist size):  当 freelist 为空是。
O(tree size): 当所有的 node 都被另外一个 tree 使用时，所有的 node 都会被遍历查找后加入 freelist。

> func (*BTree) Clone 

func (t *BTree) Clone() (t2 *BTree)

以 lazy 方式复制 BTree。Clone 并行调用并不安全，但是 Clone() 被调用完毕后，两个 tree 可以并行的使用。

复制后，t2 和 t 以类似于 fork 的方式指向同一个 btree，这个 btree 是 read-only 的。当在 t 和 t2 中有写动作时，通过 copy-on-write 方式创建出新的 node。读性能并不会下降，但是 t 和 t2 的写动作则会有稍微的下降，因为写动作涉及到分配 node 并进行了 copy。

> func (*BTree) Delete 

* func (t *BTree) Delete(item Item) Item

返回被删除的对象，如果不存在则返回值为 nil。

* func (t *BTree) DeleteMax() Item

* func (t *BTree) DeleteMin() Item

> func (*BTree) Descend 

* func (t *BTree) DescendGreaterThan(pivot Item, iterator ItemIterator)

[last, pivot)

* func (t *BTree) DescendLessOrEqual(pivot Item, iterator ItemIterator)

[pivot, first]

* func (t *BTree) DescendRange(lessOrEqual, greaterThan Item, iterator ItemIterator)

[lessOrEqual, greaterThan)

> func (*BTree) Get 

* func (t *BTree) Get(key Item) Item
*  func (t *BTree) Has(key Item) bool
*  func (t *BTree) Len() int
*  func (t *BTree) Max() Item
*  func (t *BTree) Min() Item

>  func (*BTree) ReplaceOrInsert

* func (t *BTree) ReplaceOrInsert(item Item) Item

删除旧 element 并添加新的 @item。如果旧 element 存在，则返回旧的 element，否则返回 nil。

如果 @item 是 nil，则该函数就会 panic。

> type FreeList

type FreeList struct {
	// contains filtered or unexported fields
}

FreeList 是一个 btree node list。一般情况下，每个 BTree 都有各自的 FreeList，但多个 BTree 可以共享一个 FreeList，可以安全地并发访问。 

* func NewFreeList(size int) *FreeList

> type Int int

Int 实现了整数类型的 Item 接口。

* func (Int) Less 
* func (a Int) Less(b Item) bool

Less returns true if int(a) < int(b).

> type Item

type Item interface {
	// Less tests whether the current item is less than the given argument.
	//
	// This must provide a strict weak ordering.
	// If !a.Less(b) && !b.Less(a), we treat this to mean a == b (i.e. we can only
	// hold one of either a or b in the tree).
	Less(than Item) bool
}

Item 代表了 btree 中的一个独立的 object。

> type ItemIterator

* type ItemIterator func(i Item) bool

ItemIterator 是遍历时候用的迭代算子，如果这个函数返回 false，则遍历过程会终止。
