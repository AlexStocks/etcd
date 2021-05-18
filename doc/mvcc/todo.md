todo：

Q : version 有何用？

```go
// get gets the modified, created revision and version of the key that satisfies the given atRev.
// Rev must be higher than or equal to the given atRev.
func (ki *keyIndex) get(lg *zap.Logger, atRev int64) (modified, created revision, ver int64, err error) {
	g := ki.findGeneration(atRev)
	n := g.walk(func(rev revision) bool { return rev.main > atRev })
	if n != -1 {
		return g.revs[n], g.created, g.ver - int64(len(g.revs)-n-1), nil
	}
	return revision{}, revision{}, 0, ErrRevisionNotFound
}


func (ti *treeIndex) Get(key []byte, atRev int64) (modified, created revision, ver int64, err error) {
	keyi := &keyIndex{key: key}
	ti.RLock()
	defer ti.RUnlock()
	keyi = ti.keyIndex(keyi)
	return keyi.get(ti.lg, atRev)
}

 func (tw *storeTxnWrite) put(key, value []byte, leaseID lease.LeaseID) {
 	ver = ver + 1
 	kv := mvccpb.KeyValue{
		Key:            key,
		Value:          value,
		CreateRevision: c,     // 创建时的 revision
		ModRevision:    rev,   // 本次修改的 revision
		Version:        ver,
		Lease:          int64(leaseID),
	}
}
```

A: 从 keyIndex.get() 来说，感觉应该是在 g.revs 中的下标。

--- ---

Q: Revision 的 main 和 sub 的意义如何解释？

A: 举个栗子可能很快就能说明白。假定全局版本号 currentRevision = 2，第⼀次执⾏ put hello world1，此时版本号为 {2, 0} = {major, sub}，2 是 etcd mvcc 事务版本号全局递增，0 是事务内⼦版本号随修改操作递增（⽐如⼀个 txn 事务中多个 put/delete 操作，其会从 0 递增); 第⼆次执⾏ put hello world2 后版本号应是  {3, 0}。

也就是说，如果一次事务内只有一次写动作，则 sub 为 0，如果一次事务内有批量的写动作，则 sub 才会一直递增。

