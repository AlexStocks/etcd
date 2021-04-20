# compaction

## 1 compaction type

对 k8s 集群的 etcd 进行 compaction，参照 kube-apiserver ，可以在 kube-apiserver 的启动命令中设定 compaction interval：

```go
spec:
  containers:
  - command:
    - kube-apiserver
    - --etcd-compaction-interval=10m
```

etcd 自身当然也可以配置 compaction，etcd 有两种 compaction 方式：periodical compaction 和 revision compaction，periodical  compaction 配置参数如下：

```bash
--auto-compaction-mode='periodic' --auto-compaction-retention='10m'
```

 revision compaction 配置参数如下：

```bash
--auto-compaction-mode='revision.' --auto-compaction-retention='10m' 
```

etcd 默认的 compaction 方式是 `periodic`，相关配置参见如下选项：

```go
// etcdmain/config.go
func newConfig() *config {
	cfg := &config{}
	cfg.cf = configFlags{}
	fs := cfg.cf.flagSet
	fs.StringVar(&cfg.ec.AutoCompactionRetention, "auto-compaction-retention", "0", "Auto compaction retention for mvcc key value store. 0 means disable auto compaction.")
	fs.StringVar(&cfg.ec.AutoCompactionMode, "auto-compaction-mode", "periodic", "interpret 'auto-compaction-retention' one of: periodic|revision. 'periodic' for duration based retention, defaulting to hours if no time unit is provided (e.g. '5m'). 'revision' for revision number based retention.")
	return cfg
}

// etcdmain/config.go
func (cfg *config) parse(arguments []string) error {
	cfg.cf.flagSet.Parse(arguments)
}

// etcdmain/etcd.go
func startEtcdOrProxyV2() {
    cfg := newConfig()
    startEtcd(&cfg.ec)
}

// etcdserver/server.go
func NewServer(cfg ServerConfig) (srv *EtcdServer, err error) {
	if num := cfg.AutoCompactionRetention; num != 0 {
		srv.compactor, err = v3compactor.New(cfg.Logger, cfg.AutoCompactionMode, num, srv.kv, srv)
		if err != nil {
			return nil, err
		}
		srv.compactor.Run()
	}

	return srv, nil
}
```

## 2 Periodic Compaction

不管是何种类型的 compact 请求，都使用如下类型的 compaction 请求：

```go
  // etcd 中每个 key 都存储了若干历史版本的 value，compaction 会把所有版本小于 @revision 的历史 value 都删除
  type CompactionRequest struct {
    // compaction 最终的最小历史版本号
  	Revision int64
    // 是否等待底层 db compaction 完成，etcd 一般场景下设置该值为 false
  	Physical bool
  }
```

etcd 在每个 compaction 周期内如果 compact 成功，则在下一个时间窗口内继续 compact。否则，把一个时间窗口划分成 10 份 [ const retryDivisor = 10 ]，然后不断尝试。

```go
// etcdserver/api/v3compactor/periodic.go

// newPeriodic creates a new instance of Periodic compactor that purges
// the log older than h Duration.
func newPeriodic(lg *zap.Logger, clock clockwork.Clock, h time.Duration, rg RevGetter, c Compactable) *Periodic {
	pc := &Periodic{
		lg:     lg,
		clock:  clock,
		period: h,
		rg:     rg,
		c:      c,
		revs:   make([]int64, 0),
	}
	return pc
}

// compaction interval 如果大于 1 小时，则下面函数返回 1 小时；否则使用用户设定的 interval 
// if given compaction period x is <1-hour, compact every x duration.
// (e.g. --auto-compaction-mode 'periodic' --auto-compaction-retention='10m', then compact every 10-minute)
// if given compaction period x is >1-hour, compact every hour.
// (e.g. --auto-compaction-mode 'periodic' --auto-compaction-retention='2h', then compact every 1-hour)
func (pc *Periodic) getCompactInterval() time.Duration {
	itv := pc.period
	if itv > time.Hour {
		itv = time.Hour
	}
	return itv
}

/*
Compaction period 1-hour:
  1. compute compaction period, which is 1-hour
  2. record revisions for every 1/10 of 1-hour (6-minute)
  3. keep recording revisions with no compaction for first 1-hour
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 1-hour (6-minute)

Compaction period 24-hour:
  1. compute compaction period, which is 1-hour
  2. record revisions for every 1/10 of 1-hour (6-minute)
  3. keep recording revisions with no compaction for first 24-hour
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 1-hour (6-minute)

Compaction period 59-min:
  1. compute compaction period, which is 59-min
  2. record revisions for every 1/10 of 59-min (5.9-min)
  3. keep recording revisions with no compaction for first 59-min
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 59-min (5.9-min)

Compaction period 5-sec:
  1. compute compaction period, which is 5-sec
  2. record revisions for every 1/10 of 5-sec (0.5-sec)
  3. keep recording revisions with no compaction for first 5-sec
  4. do compact with revs[0]
	- success? contiue on for-loop and move sliding window; revs = revs[1:]
	- failure? update revs, and retry after 1/10 of 5-sec (0.5-sec)
*/

// Run runs periodic compactor.
func (pc *Periodic) Run() {
	compactInterval := pc.getCompactInterval() // == pc.period == cfg.AutoCompactionRetention
	retryInterval := pc.getRetryInterval()  // compactInterval / 10
	retentions := pc.getRetentions() // 11

	go func() {
		lastSuccess := pc.clock.Now()
		baseInterval := pc.period
		for {
			pc.revs = append(pc.revs, pc.rg.Rev())
			if len(pc.revs) > retentions {
			   // 下面英语注释的意思是说，pc.revs[0] 在上个  compact 时间周期已经处理过了 
				pc.revs = pc.revs[1:] // pc.revs[0] is always the rev at pc.period ago
			}

			select {
			case <-pc.clock.After(retryInterval): // 每次 sleep 0.1 * compactInterval
			}

          // 如果当前时间减去上次成功执行 compact 时间间隔小于 cfg.AutoCompactionRetention，则退出本次 compact
                  //  达到的效果是：如果上个时间窗口执行 compact 成功，lastSuccess 更新，则下次执行一定是在 cfg.AutoCompactionRetention 以后；
                  // 如果上层 compact 失败，则 lastSuccess 不更新，则下个 retry 时间窗口就是 0.1 * cfg.AutoCompactionRetention 以后
			if pc.clock.Now().Sub(lastSuccess) < baseInterval {
				continue
			}

			// wait up to initial given period
			if baseInterval == pc.period {
				baseInterval = compactInterval
			}
			rev := pc.revs[0]

			_, err := pc.c.Compact(pc.ctx, &pb.CompactionRequest{Revision: rev})
			if err == nil || err == mvcc.ErrCompacted {
              // 执行 compact 成功，则更新 lastSuccess
				lastSuccess = pc.clock.Now()
			}
		}
	}()
}
```

通过上面代码，可见其主要逻辑步骤是：

* 1 把用户设定的 compact interval 划分为 10 份，for-loop sleep interval 是 0.1 * compactInterval，且每个 sleep 时间窗口都会执行 revision 记录动作；
* 2 在第一个 compact interval 内，只收集 revision 记录，在第二个 compact interval 开始才进行 compact；
* 3 如果上个 compact interval  执行 compact 失败，则后面每个 sleep window 内都尝试执行 compact；否则用 lastSuccess 记录上次执行成功的时间点，把 compact 时间窗口后移一个 compact interval。

## 3 Revision Compaction

```go
// etcdserver/api/v3compactor/revision.go

// newRevision creates a new instance of Revisonal compactor that purges
// the log older than retention revisions from the current revision.
func newRevision(lg *zap.Logger, clock clockwork.Clock, retention int64, rg RevGetter, c Compactable) *Revision {
	rc := &Revision{
		retention: retention,
	}
	return rc
}

const revInterval = 5 * time.Minute

// Run runs revision-based compactor.
func (rc *Revision) Run() {
	prev := int64(0)
	go func() {
		for {
			select {
			case <-rc.ctx.Done():
				return
			case <-rc.clock.After(revInterval):
			}

			rev := rc.rg.Rev() - rc.retention
			if rev <= 0 || rev == prev {
				continue
			}

			now := time.Now()
			_, err := rc.c.Compact(rc.ctx, &pb.CompactionRequest{Revision: rev})
			if err == nil || err == mvcc.ErrCompacted {
				prev = rev
			} 
		}
	}()
}
```

从代码可见，Revision Compaction 逻辑很简单，每次 sleep 5分钟，compaction 到 revision 值为： 当前最新的 revision - retention。

## 4 etcd compact

submit request:

```go
  // etcdserver/v3_server.go
  func (s *EtcdServer) Compact(r *pb.CompactionRequest) (*pb.CompactionResponse, error) {
	result, err := s.processInternalRaftRequestOnce(ctx, pb.InternalRaftRequest{Compaction: r})
	if r.Physical && result != nil && result.physc != nil {
		<-result.physc
    	s.be.ForceCommit()
	}
  }

  func (s *EtcdServer) processInternalRaftRequestOnce(r pb.InternalRaftRequest) (*applyResult, error) {
	data := r.Marshal()
	id := r.ID
	if id == 0 {
		id = r.Header.ID
	}
	ch := s.w.Register(id)
	err := s.r.Propose(cctx, data)
	if err != nil {
		s.w.Trigger(id, nil) // 如果提议失败，则删除 id 相关的请求 
		return nil, err
	}
	select {
	case x := <-ch: // 等待 response
		return x.(*applyResult), nil
    }
  }
```

compact:

```go
// mvcc/key_index.go
// compact compacts a keyIndex by removing the versions with smaller or equal
// revision than the given atRev except the largest one (If the largest one is
// a tombstone, it will not be kept).
// If a generation becomes empty during compaction, it will be removed.
func (ki *keyIndex) compact(lg *zap.Logger, atRev int64, available map[revision]struct{}) {
	genIdx, revIndex := ki.doCompact(atRev, available)

	g := &ki.generations[genIdx]
	if !g.isEmpty() {
		// remove the previous contents.
		if revIndex != -1 {
			g.revs = g.revs[revIndex:]
		}
		// remove any tombstone
		if len(g.revs) == 1 && genIdx != len(ki.generations)-1 {
			delete(available, g.revs[0])
			genIdx++
		}
	}

	// remove the previous generations.
	ki.generations = ki.generations[genIdx:]
}

func (ti *treeIndex) Compact(rev int64) map[revision]struct{} {
	available := make(map[revision]struct{})
	ti.Lock()
	clone := ti.tree.Clone()
	ti.Unlock()
	clone.Ascend(func(item btree.Item) bool {
		keyi := item.(*keyIndex)
		//Lock is needed here to prevent modification to the keyIndex while
		//compaction is going on or revision added to empty before deletion
		ti.Lock()
		keyi.compact(ti.lg, rev, available)
		if keyi.isEmpty() {
			item := ti.tree.Delete(keyi)
		}
		ti.Unlock()
		return true
	})
	return available
}

// mvcc/kvstore.go
func (s *store) compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error) {
	ch := make(chan struct{})
	var j = func(ctx context.Context) {
		start := time.Now()
		keep := s.kvindex.Compact(rev)
		indexCompactionPauseMs.Observe(float64(time.Since(start) / time.Millisecond))
		if !s.scheduleCompaction(rev, keep) {
			s.compactBarrier(nil, ch)
			return
		}
		close(ch)
	}

	s.fifoSched.Schedule(j)
	return ch, nil
}

// mvcc/kvstore.go
func (s *store) Compact(trace *traceutil.Trace, rev int64) (<-chan struct{}, error) {
	ch, err := s.updateCompactRev(rev)
	return s.compact(trace, rev)
}

func (a *applierV3backend) Compaction(compaction *pb.CompactionRequest) (*pb.CompactionResponse, <-chan struct{}, *traceutil.Trace, error) {
	resp := &pb.CompactionResponse{}
	resp.Header = &pb.ResponseHeader{}

	ch, err := a.s.KV().Compact(trace, compaction.Revision)
	if err != nil {
		return nil, ch, nil, err
	}
	// get the current revision. which key to get is not important.
	rr, _ := a.s.KV().Range([]byte("compaction"), nil, mvcc.RangeOptions{})
	resp.Header.Revision = rr.Rev
	return resp, ch, trace, err
}

func (a *applierV3backend) Apply(r *pb.InternalRaftRequest) *applyResult {
	switch {
	case r.Compaction != nil:
		ar.resp, ar.physc, ar.trace, ar.err = a.s.applyV3.Compaction(r.Compaction)	
	}
}
```

etcd 从启动到接收 compact 请求的流程：

```go
// etcdserver/server.go
// applyEntryNormal apples an EntryNormal type raftpb request to the EtcdServer
func (s *EtcdServer) applyEntryNormal(e *raftpb.Entry) {
    var raftReq pb.InternalRaftRequest
	pbutil.MaybeUnmarshal(&raftReq, e.Data)
	s.applyV3.Apply(&raftReq)
}

// apply takes entries received from Raft (after it has been committed) and
// applies them to the current state of the EtcdServer.
// The given entries should not be empty.
func (s *EtcdServer) apply(
	es []raftpb.Entry,
	confState *raftpb.ConfState,
) (appliedt uint64, appliedi uint64, shouldStop bool) {
	for i := range es {
		e := es[i]
		switch e.Type {
		case raftpb.EntryNormal:
			s.applyEntryNormal(&e)
			s.setAppliedIndex(e.Index)
			s.setTerm(e.Term)
        }
    }
}

func (s *EtcdServer) applyEntries(ep *etcdProgress, apply *apply) {
	firsti := apply.entries[0].Index
	var ents []raftpb.Entry
	if ep.appliedi+1-firsti < uint64(len(apply.entries)) {
		ents = apply.entries[ep.appliedi+1-firsti:]
	}
    s.apply(ents, &ep.confState)
}

func (s *EtcdServer) applyAll(ep *etcdProgress, apply *apply) {
	s.applyEntries(ep, apply)	
}

func (s *EtcdServer) run() {
	for {
		select {
		case ap := <-s.r.apply():
			f := func(context.Context) { s.applyAll(&ep, &ap) }
			sched.Schedule(f)	
        }
    }
}

// start prepares and starts server in a new goroutine. It is no longer safe to
// modify a server's fields after it has been sent to Start.
// This function is just used for testing.
func (s *EtcdServer) start() {
	go s.run()	
}

// Start performs any initialization of the Server necessary for it to
// begin serving requests. It must be called before Do or Process.
// Start must be non-blocking; any long-running server functionality
// should be implemented in goroutines.
func (s *EtcdServer) Start() {
    s.start()
}

// StartEtcd launches the etcd server and HTTP handlers for client/server communication.
// The returned Etcd.Server is not guaranteed to have joined the cluster. Wait
// on the Etcd.Server.ReadyNotify() channel to know when it completes and is ready for use.
func StartEtcd(inCfg *Config) (e *Etcd, err error) {
	srvcfg := etcdserver.ServerConfig{}	
    e.Server = etcdserver.NewServer(srvcfg)
	e.Server.Start()
}
```
