# list

索引部分：

```
├── btree.md       # google b 树 API
├── keyindex.md    # keyIndex，是 MVCC 的基石 
└── treeindex.md   # index 接口的实现 treeIndex，构成了 kv 存储的内存索引
```

GC 部分：

```
├── compaction.md  # 压缩 index 和 kvs store 的流程
```

测试文件：

```
// 从测试用例看相关代码
├── kv_test.go     # 从代码里可见 Read/Write 是对上的事务接口，具体的 CRUD 接口是 Put/Range/DeleteRange
```
