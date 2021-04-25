# keyIndex

keyIndex 存储了一个 key 相关的所有的版本的集合。 

generation 则记录了一次 key 从创建到删除的所有 revision 的集合，删除时候的 revision 称之为 Tombstone。 每个 generation 的 keyIndex 中的 version 创建时为零，随着每次 put 操作增 1，delete 时 清零。

keyIndex 中的 generations 数组不会在一个数组的 index 上不断膨胀下去，一旦发生删除就会结束当前的Generation，生成新的Generation。同时 version 也会归零，每次 put 操作会让其从 1 重新开始增长。

```go
// keyIndex 存储了一个 key 相关的所有的版本的集合。 
// 一个 keyIndex 至少包含一个 generation，每个 generation 则包含了众多的版本号。
// 一个 key 的Tombstone 是在 key 当前 generation 中添加一个版本作为 tombstone version，
// 并创建一个新的 generation。 
// key 的每个版本号都都有一个 index 指向 backend。
//
// 例如: put(1.0);put(2.0);tombstone(3.0);put(4.0);tombstone(5.0) on key "foo"
// generate a keyIndex:
// key:     "foo"
// rev: 5
// generations:
//    {empty}
//    {4.0, 5.0(t)}
//    {1.0, 2.0, 3.0(t)}
//
// 压缩 keyIndex 就是把所有小于等于目标版本号的 keyIndex 全部删除，把更高级版本号的数据保留。
// 如果压缩后 generation 集合为空，则删除这个 generation。如果 generation 为空，则删除这个 keyIndex。
//
// 例如 compact(2) 之后，剩余的 generation 集合为：
//    {empty}
//    {4.0, 5.0(t)}
//    {2.0, 3.0(t)}
//
// compact(4)
// generations:
//    {empty}
//    {4.0, 5.0(t)}
//
// compact(5):
// generations:
//    {empty} -> key SHOULD be removed.
//
// compact(6):
// generations:
//    {empty} -> key SHOULD be removed.

type keyIndex struct {
	key         []byte
	modified    revision // the main rev of the last modification
	generations []generation
}

// generation contains multiple revisions of a key.
type generation struct {
	ver     int64
	created revision // when the generation is created (put in first revision).
	revs    []revision
}
```

> 添加

```go
// put puts a revision to the keyIndex.
func (ki *keyIndex) put(lg *zap.Logger, main int64, sub int64) {
	rev := revision{main: main, sub: sub}

    // 检查 @rev 是否比 key 当前的最新的 version 更新
	if !rev.GreaterThan(ki.modified) {
		plog.Panicf("store.keyindex: put with unexpected smaller revision [%v / %v]", rev, ki.modified)
	}
	if len(ki.generations) == 0 {
		ki.generations = append(ki.generations, generation{})
	}
	g := &ki.generations[len(ki.generations)-1]
	if len(g.revs) == 0 { // create a new key
        // 记录创建版本号
		g.created = rev
	}
	g.revs = append(g.revs, rev)
	g.ver++ // 每次 put 时加 1
	ki.modified = rev
}
```

> 删除

```go
func (ki *keyIndex) tombstone(lg *zap.Logger, main int64, sub int64) error {
    // 如果最后一个 generation 为空，则没有可删除的 revision
	if ki.generations[len(ki.generations)-1].isEmpty() {
		return ErrRevisionNotFound
	}
	ki.put(lg, main, sub)
    // 创建一个新的 generation，初始 ver 为空
	ki.generations = append(ki.generations, generation{})
	return nil
}
```

> 查找

```go
  func (ki *keyIndex) findGeneration(rev int64) *generation {
	lastg := len(ki.generations) - 1
	cg := lastg

    // 倒查 @rev 所在的 generation
	for cg >= 0 {
		if len(ki.generations[cg].revs) == 0 {
			cg--
			continue
		}
		g := ki.generations[cg]
		if cg != lastg {
			// tomb version 的 main version 小于 @rev，则说明本 generation 不包含 @rev
			if tomb := g.revs[len(g.revs)-1].main; tomb <= rev {
				return nil
			}
		}
        // [ki.generations[cg].rev, ki.generations[cg+1].rev)
		if g.revs[0].main <= rev {
			return &ki.generations[cg]
		}
		cg--
	}
	return nil
  }

  // 获取 @atRev 所在 generation 的 create revision、modified revision 和 version
  func (ki *keyIndex) get(lg *zap.Logger, atRev int64) (modified, created revision, ver int64, err error) {
    // 后去 @atRev 所在的 generation
	g := ki.findGeneration(atRev)
    // 后去 @atRev 在 generation 中对应的 index
	n := g.walk(func(rev revision) bool { return rev.main > atRev })
    // ver = g.ver - [len(g.vers) - (n + 1)]
	return g.revs[n], g.created, g.ver - int64(len(g.revs)-n-1), nil
  }

  // 获取所有大于等于 @rev 的 revision 集合
  func (ki *keyIndex) since(lg *zap.Logger, rev int64) []revision {
	since := revision{rev, 0}
	var gi int
	// find the generations to start checking
	for gi = len(ki.generations) - 1; gi > 0; gi-- {
		g := ki.generations[gi]
		if g.isEmpty() {
			continue
		}
        // 查找小于等于 revision{@rev, 0} 的第一个 generation
		if since.GreaterThan(g.created) {
			break
		}
	}

	var revs []revision
	var last int64
	for ; gi < len(ki.generations); gi++ {
		for _, r := range ki.generations[gi].revs {
			if since.GreaterThan(r) {
				continue
			}
			if r.main == last {
				// replace the revision with a new one that has higher sub value,
				// because the original one should not be seen by external
				revs[len(revs)-1] = r
				continue
			}
			revs = append(revs, r)
			last = r.main
		}
	}
	return revs
  }
```

> 压缩

```go

// 倒排查找
  func (g *generation) walk(f func(rev revision) bool) int {
	l := len(g.revs)
	for i := range g.revs {
		ok := f(g.revs[l-i-1])
		if !ok {
			return l - i - 1
		}
	}
	return -1
  }

  func (ki *keyIndex) doCompact(atRev int64, available map[revision]struct{}) (genIdx int, revIndex int) {
    // 倒排查找 generation 所有的 revision，直到有 revision.main <= @atRev。
    // 把 generation 中所有大于 @atRev 的 revision 都存到 @available
	f := func(rev revision) bool {
		if rev.main <= atRev {
			available[rev] = struct{}{}
			return false
		}
		return true
	}

	genIdx, g := 0, &ki.generations[0]
    // 查找 @atRev 所在的或者 @atRev 以后的 generation：generation.tomb.main > @atRev
	for genIdx < len(ki.generations)-1 {
		if tomb := g.revs[len(g.revs)-1].main; tomb > atRev {
			break
		}
		genIdx++
		g = &ki.generations[genIdx]
	}
	revIndex = g.walk(f)

	// 返回 @atRev 所在的或者在 @atRev 以后创建的 generation、@atRev 在 generation 中的下标位置
	return genIdx, revIndex
  }

  // 把 @ki 中所有小于等于 @atRev 的 revision 删掉，即便其中包含其中 tombstone。
  // 如果一个 generation 被压缩后变为空，则会被删掉。 
  func (ki *keyIndex) compact(lg *zap.Logger, atRev int64, available map[revision]struct{}) {
	genIdx, revIndex := ki.doCompact(atRev, available)

	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		// 删掉旧的 revision
		if revIndex != -1 {
			g.revs = g.revs[revIndex:]
		}
		// remove any tombstone
        // 如果 generation 并非 keyIndex 最后一代，且 generation 就剩下 tombstone，则清空之 
		if len(g.revs) == 1 && genIdx != len(ki.generations)-1 {
			delete(available, g.revs[0])
			genIdx++
		}
	}

	// 删除旧的 generation
	ki.generations = ki.generations[genIdx:]
  }

  // keep 保存 compact 时某个 generation 可保留的 revision 集合，
  // 如果所在的 generation 只剩下 tombstone，则从 @available 中删除该 tombstone
  func (ki *keyIndex) keep(atRev int64, available map[revision]struct{}) {
	genIdx, revIndex := ki.doCompact(atRev, available)
	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		// remove any tombstone
		if revIndex == len(g.revs)-1 && genIdx != len(ki.generations)-1 {
			delete(available, g.revs[revIndex])
		}
	}
  }
```
