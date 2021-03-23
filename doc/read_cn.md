## Etcd 线性一致性读

## 1 线性一致性读

线性一致性读解释的比价好的一篇 blog 是 [线性一致性和 Raft](https://pingcap.com/blog-cn/linearizability-and-raft/)，文中要点如下：

* 1 Raft 只保证了 log 是一致的(committed)，不保证其后面的状态机也是一致的，Raft 只处理 log，与状态机分离。
* 2 LogRead，把读写流程统一，把读请求当做一次不修改状态机的写请求，则状态机应用后返回的值肯定能否满足线性一致性语义。缺点是不仅有 RPC 开销，还有 Raft Log 开销，时间成本太高。
* 3 ReadIndex：优化 LogRead 的最基本方法是 ReadIndex，其流程如下：
    + 1 记录当前 commit index 为 ReadIndex；
    + 2 向 follower 发送一次心跳，如果大多数 follower 回应了，则可以确认现在 leader 的身份；
    + 3 等待本地状态机 apply index >= ReadIndex；
        - 上面的条件很关键，因为此时不管 leader 是否飘走，本地状态机已经应用了 ReadIndex 对应的 log entry。
    + 4 执行读请求，讲结果返回给 Client。
    + 总结：相比于 LogRead，它省略了记录 Log 步骤，减少了磁盘开销，可提升吞吐减小一定量的延时。
* LeaseRead: 是对 ReadIndex 的优化，省去了第二步，其基本思路是取一个比 Election Timeout 小租期，这个租期内是不会发生选举的，leader 不会变。这样可以大举减少延时。
    + 1 缺点是何时间的正确性挂钩，如果发生了时间漂移就会有问题。
    + 2 需要注意的一点就是，只有 Leader 的状态机应用了当前 term 的第一个 Log 后才能进行 LeaseRead。因为新 leader 虽然拥有全部的 committed log，但是其状态机有可能落后于之前的 Leader。只有状态机应用了新 leader 的 term log 后，才能保证新 leader 的状态机的数据一定会比旧的 leader 新。
* WaitFree：在 ReadIndex 之上，省略了 ReadIndex 的第三步骤。Raft 的强 leader 特性，在租期内 Client 收到的 Resp 都是由 Leader 的状态机产生的，所以只要状态机满足线性一致性，则 Lease 内所有读都能满足线性一致性。
 
etcd 实现了 ReadIndex 和 LeaseRead 两个算法。

## 2 etcd 的读流程 

etcd v3 版本，客户端和服务端采用 gRPC 相互之间进行交互。

### 2.1 客户端发起请求流程

客户端的读请求是向 server 发起一个 Get 请求，而 etcd 内部会把请求转为 verb 为 OpGet 的 kv.Do() 调用。

```go
// clientv3/kv.go
func (kv *kv) Get(ctx context.Context, key string, opts ...OpOption) (*GetResponse, error) {
	r, err := kv.Do(ctx, OpGet(key, opts...))
	return r.get, toErr(ctx, err)
}
```

OpGet 是构造一个与 gRPC 相关的结构体 Op 的对象。需要注意的是，Op 的 verb 是 tRange，对象是一个单 key。

```go
func OpGet(key string, opts ...OpOption) Op {
	ret := Op{t: tRange, key: []byte(key)}
	ret.applyOpts(opts)
	return ret
}
```

kv.Do() 的实际处理流程如下：

```go
func (kv *kv) Do(ctx context.Context, op Op) (OpResponse, error) {
	var err error
	switch op.t {
	case tRange:
		var resp *pb.RangeResponse
        // 把请求转化为 range 请求。
		resp, err = kv.remote.Range(ctx, op.toRangeRequest(), kv.callOpts...)
		if err == nil {
			return OpResponse{get: (*GetResponse)(resp)}, nil
		}
}
```

其请求对应的 range service 定义如下：

```go
service KV {
  // Range gets the keys in the range from the key-value store.
  rpc Range(RangeRequest) returns (RangeResponse) {
      option (google.api.http) = {
        post: "/v3/kv/range"
        body: "*"
    };
  }
}
```

### 2.2 服务端读请求处理过程

服务端 Range RPC 处理接口定义如下：

```go
func (s *EtcdServer) Range(ctx context.Context, r *pb.RangeRequest) (*pb.RangeResponse, error) {
	if !r.Serializable {
		err = s.linearizableReadNotify(ctx)
		trace.Step("agreement among raft nodes before linearized reading")
		if err != nil {
			return nil, err
		}
	}
	chk := func(ai *auth.AuthInfo) error {
		return s.authStore.IsRangePermitted(ai, r.Key, r.RangeEnd)
	}

	get := func() { resp, err = s.applyV3Base.Range(ctx, nil, r) }
	if serr := s.doSerialize(ctx, chk, get); serr != nil {
		err = serr
		return nil, err
	}
	return resp, err
}

func (s *EtcdServer) linearizableReadNotify(ctx context.Context) error {
    // 获取 read notify channel
	s.readMu.RLock()
	nc := s.readNotifier
	s.readMu.RUnlock()

	select {
    // 如果可以顺利发送成功，说明 read loop 协程已准备好处理此次读请求
	case s.readwaitc <- struct{}{}:
	default:
	}

	select {
    // 等待接收一致性读信号
	case <-nc.c:
		return nc.err
	case <-ctx.Done():
		return ctx.Err()
	case <-s.done:
		return ErrStopped
	}
}
```

从 EtcdServer.linearizableReadNotify() 代码的核心可见，它是等待 read loop：

```go
func (s *EtcdServer) linearizableReadLoop() {
	for {
        // 产生请求 ID 
		ctxToSend := make([]byte, 8)
		id1 := s.reqIDGen.Next()
		binary.BigEndian.PutUint64(ctxToSend, id1)
        // leader 身份变换通知
		leaderChangedNotifier := s.leaderChangedNotify()
		select {
        // 如果 leader 身份发生了变化，则不再等待
		case <-leaderChangedNotifier:
			continue
        // 当有新请求到来时，才会跳过阻塞
		case <-s.readwaitc:
		case <-s.stopping:
			return
		}

		// 产生新的 read notifier，以供下一次读 verb 使用
		nextnr := newNotifier()

		s.readMu.Lock()
        // 获取当前的读通知 channel
		nr := s.readNotifier
		s.readNotifier = nextnr
		s.readMu.Unlock()

        // 调用 raft 的 ReadIndex() 接口发出获取 read index 请求
		cctx, cancel := context.WithTimeout(context.Background(), s.Cfg.ReqTimeout())
		if err := s.r.ReadIndex(cctx, ctxToSend); err != nil {}

		var (
			timeout bool
			done    bool
		)
        // 阻塞等待 read index 请求完成，请求完成说明已经读取到准确的 read index
		for !timeout && !done {
			select {
            // 读取到 read index 响应
			case rs = <-s.r.readStateC:
				// 验证 request id 是否正确
				done = bytes.Equal(rs.RequestCtx, ctxToSend)
			}
		}
		if !done {
			continue
		}

		// 等待 apply index >= read index 
		if ai := s.getAppliedIndex(); ai < rs.Index {
            // 进入这个分支，说明 apply index < read index，使用 select-case 在此阻塞等待条件被满足 
			select {
			case <-s.applyWait.Wait(rs.Index):
			case <-s.stopping:
				return
			}
		}
        // 发出可以读取状态机的信号
		nr.notify(nil)
	}
}
```

### 2.3 raft 对 read index 请求的处理过程

获取 Read Index 的核心是 raft 算法库的 ReadIndex 函数，它把请求转成类型为 MsgReadIndex 的请求消息转发到 raft：

```go
func (n *node) ReadIndex(ctx context.Context, rctx []byte) error {
	return n.step(ctx, pb.Message{Type: pb.MsgReadIndex, Entries: []pb.Entry{{Data: rctx}}})
}
```

raft 对待 read index 请求的处理，因为 role 为 leader or follower 不同处理方式有所区别。 follower 把请求转发到 leader，leader 则先把请求放入请求队列，然后把请求转发给所有 follower。

```go
func stepFollower(r *raft, m pb.Message) error {
	switch m.Type {
	case pb.MsgReadIndex:
        // 没有 leader，则返回
		if r.lead == None {
			return nil
		}
        // 转发请求到 leader
		m.To = r.lead
		r.send(m)
	case pb.MsgReadIndexResp:
		if len(m.Entries) != 1 {
			r.logger.Errorf("%x invalid format of MsgReadIndexResp from %x, entries count: %d", r.id, m.From, len(m.Entries))
			return nil
		}
		r.readStates = append(r.readStates, ReadState{Index: m.Index, RequestCtx: m.Entries[0].Data})
	}
	return nil
}

func stepLeader(r *raft, m pb.Message) error {
	// 这个 switch 分支处理的消息类型，不需要获取 m.From 的 progress
	switch m.Type {
	case pb.MsgReadIndex:
        // 如果集群中 peer 有多个，则执行广播通知给所有 follower
		if !r.prs.IsSingleton() {
			switch r.readOnly.option {
            // 执行 Read Index 算法的逻辑
			case ReadOnlySafe:
                // 将请求放入队列中
				r.readOnly.addRequest(r.raftLog.committed, m)
				// The local node automatically acks the request.
				r.readOnly.recvAck(r.id, m.Entries[0].Data)
                // 将请求广播给其他节点
				r.bcastHeartbeatWithCtx(m.Entries[0].Data)
            // lease read 请求处理
			case ReadOnlyLeaseBased:
			}
		} else { // only one voting member (the leader) in the cluster
		}

		return nil
	}
}
```

#### 2.3.1 raft 只读请求 pipeline 

raft 有个 readOnly 队列，对客户端的只读请求进行排队，体现了 pipeline 处理请求的思想。

```go
// raft/read_only.go
type readOnly struct {
	// 线性一致性数据处理方法
	option           ReadOnlyOption
    // 待处理的读请求 map。key 是请求 ID，value 则是客户端发来的读请求的请求内容、read index 等读请求 context
	pendingReadIndex map[string]*readIndexStatus
    // 请求 ID 队列
	readIndexQueue   []string
}
```

> 添加请求

```go
// raft/read_only.go
// 读请求上下文
type readIndexStatus struct {
	req   pb.Message // 请求内容
	index uint64  // raft 收到请求时候的 commit ID，作为 read Index
    // check quorum 的 ack 记录。英文注释称为了兼容 quorum.VoteResult，此处没有用 map[uint64]struct{} 
	acks map[uint64]bool 
}

// addRequest adds a read only reuqest into readonly struct.
// `index` 接收到请求时的 raft commit index
// `m` 只读请求内容
func (ro *readOnly) addRequest(index uint64, m pb.Message) {
	s := string(m.Entries[0].Data)
    // 查验请求 ID 是否已经存入 map
	if _, ok := ro.pendingReadIndex[s]; ok {
		return
	}
	ro.pendingReadIndex[s] = &readIndexStatus{index: index, req: m, acks: make(map[uint64]bool)}
	ro.readIndexQueue = append(ro.readIndexQueue, s)
}
```

> 处理响应

```go
// raft/read_only.go
// advance 函数处理只读请求队列，它根据 @m 从 readOnly.pendingReadIndex 中查询请求 context
// 收到 m 对应的 readIndex 的 quorum，意味着 readOnly.readIndexQueue 中 @m 对应的 request ID 前面的所有 readIndex 条件都被满足了。
// 所以此函数会返回 readOnly.readIndexQueue 中 @m 对应的所有请求 ID 前面所有元素都清空，并把这些请求 ID 对应的请求 ctx 数组返回，同
// 时清空 readOnly.pendingReadIndex 中对应的 kv。
func (ro *readOnly) advance(m pb.Message) []*readIndexStatus {
	ctx := string(m.Context)
	rss := []*readIndexStatus{}

	// 过滤数组，找到请求在 数组队列 中的位置，并把请求以前所有的内容都存入 rss 中
	for _, okctx := range ro.readIndexQueue {
		i++
		rs, ok := ro.pendingReadIndex[okctx]
		if !ok {
			panic("cannot find corresponding read state from pending map")
		}
        // 把请求队列 readIndexQueue 中请求 ID 以前的所有请求 ctx 都放入 rss 中
		rss = append(rss, rs)
        // 请求 ID 相等，则意味着找到请求
		if okctx == ctx {
			found = true
			break
		}
	}

	if found {
        // 找到请求，则只保留队列中 @m 在队列中以后的所有的 elements
		ro.readIndexQueue = ro.readIndexQueue[i:]
        // 把 rss 中所有请求 ctx 从 readOnly.pendingReadIndex map 中清理掉 
		for _, rs := range rss {
			delete(ro.pendingReadIndex, string(rs.req.Entries[0].Data))
		}
		return rss
	}

	return nil
}

// raft/raft.go 
func stepLeader(r *raft, m pb.Message) error {
	// These message types do not require any progress for m.From.
	switch m.Type {
		case pb.MsgHeartbeatResp:
    		rss := r.readOnly.advance(m)
            // 对所有满足了 applyIndex >= readIndex 条件的请求进行响应
    		for _, rs := range rss {
    			req := rs.req
    			if req.From == None || req.From == r.id { // from local member
                    // 如果请求是本地发出的，则把请求放入 raft.readStates 中，作为 ready 数据的一部分 
    				r.readStates = append(r.readStates, ReadState{Index: rs.index, RequestCtx: req.Entries[0].Data})
    			} else {
    				r.send(pb.Message{To: req.From, Type: pb.MsgReadIndexResp, Index: rs.index, Entries: req.Entries})
    			}
    		}
	}
}
```

已完成的读请求队列会被存入 raft.readStates 中。node.run() 函数会将它作为 Ready 数据结构的一部分透传给应用层。

```go
// raft/node.go
func newReady(r *raft, prevSoftSt *SoftState, prevHardSt pb.HardState) Ready {
	rd := Ready{}
	if len(r.readStates) != 0 {
        // 将已完成 read index 的读请求队列传给 Ready 数据结构
		rd.ReadStates = r.readStates
	}
	return rd
}

// raft/rawnode.go
func (rn *RawNode) readyWithoutAccept() Ready {
	return newReady(rn.raft, rn.prevSoftSt, rn.prevHardSt)
}

// 通过 for-loop 源源不断地将 raft.readStates 包装为 Ready 数据一部分透传给应用层
func (n *node) run() {
	for {
		if n.rn.HasReady() {
			rd = n.rn.readyWithoutAccept()
		}
    }
}
```

应用层对 readState 数据的处理：

```go
// etcdserver/raft.go
// start prepares and starts raftNode in a new goroutine. It is no longer safe
// to modify the fields after it has been started.
func (r *raftNode) start(rh *raftReadyHandler) {
	go func() {
		for {
			select {
			case rd := <-r.Ready():
                // 处理已完成 read index 请求的读
				if len(rd.ReadStates) != 0 {
					select {
                    // 每次只将最后一个  read state 发送给 r.readStateC
                    // EtcdServer.linearizableReadLoop() 函数收到响应后就不再阻塞，给客户端返回响应。
					case r.readStateC <- rd.ReadStates[len(rd.ReadStates)-1]:
					}
				}
			}
		}
	}()
}
```

至此，一个读请求流程处理完毕。

## 3 其他可改进项

[TiDB 新特性漫谈：从 Follower Read 说起](https://pingcap.com/blog-cn/follower-read-the-new-features-of-tidb/) 一文中有优化项如下：

* 1 Follower Read 流程为 Leader 只要告诉 Follower 当前最新的 Commit Index 就够了，因为无论如何，即使这个 Follower 本地没有这条日志，最终这条日志迟早都会在本地 Apply。
    + 当客户端对一个 Follower 发起读请求的时候，这个 Follower 会请求此时 Leader 的 Commit Index，拿到 Leader 的最新的 Commit Index 后，等本地 Apply 到 Leader 最新的 Commit Index 后，然后将这条数据返回给客户端，非常简洁。

改进的相关问题：

* 1 因为 TiKV 的异步 Apply 机制，可能会出现一个比较诡异的情况：破坏线性一致性，本质原因是由于 Leader 虽然告诉了 Follower 最新的 Commit Index，但是 Leader 对这条 Log 的 Apply 是异步进行的，在 Follower 那边可能在 Leader Apply 前已经将这条记录 Apply 了，这样在 Follower 上就能读到这条记录，但是在 Leader 上可能过一会才能读取到。
* 2 这种 Follower Read 的实现方式仍然会有一次到 Leader 请求 Commit Index 的 RPC，所以目前的 Follower read 实现在降低延迟上不会有太多的效果。

平衡：
* 1 对于第一点，虽然确实不满足线性一致性了，但是好在是永远返回最新的数据，另外我们也证明了这种情况并不会破坏我们的事务隔离级别（Snapshot Isolation）。
* 2 虽然对于延迟来说，不会有太多的提升，但是对于提升读的吞吐，减轻 Leader 的负担还是很有帮助。

更进一步：
* 需要 Follower Read 作为基础，就是 Geo-Replication 后的 Local Read。
* 就近读
 但是对于部分的读请求，如果能就近读，总是能极大的降低延迟，提升吞吐。但是细心的朋友肯定能注意到，目前这个 Follower Read 对于降低延迟来说，并不明显，因为仍然要去 Leader 那边通信一下。不过仍然是有办法的，还记得上面留给大家的悬念嘛？能不能不问 Leader 就返回本地的 committed log？其实有些情况下是可以的。大家知道 TiDB 是基于 MVCC 的，每条记录都会一个全局唯一单调递增的版本号，下一步 Follower Read 会和数据本身的 MVCC 结合起来，如果客户端这边发起的事务的版本号，本地最新的提交日志中的数据的版本大于这个版本，那么其实是可以安全的直接返回，不会破坏 ACID 的语义。另外对于一些对一致性要求不高的场景，未来直接支持低隔离级别的读，也未尝不可。到那时候，TiDB 的跨数据中心的性能将会又有一个飞跃。