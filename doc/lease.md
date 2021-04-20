# lease

## 1 Lease

Lease 租约。

```go

  type LeaseItem struct {
	Key string
  }

  type Lease struct {
  	ID           LeaseID 
  	ttl          int64 // lease 的生命周期，以秒为单位
  	remainingTTL int64 // 剩余生命周期，以秒为单位。如果为 0，则说明整个 Lease 是 unset 的，已经过期
  	expiryMu sync.RWMutex
  	// expiry is time when lease should expire. no expiration when expiry.IsZero() is true
  	expiry time.Time // 过期时间点
  
  	// mu protects concurrent accesses to itemSet
  	mu      sync.RWMutex
  	itemSet map[LeaseItem]struct{} // 跟这个 Lease 绑定的所有 kv 的 key 的集合
  	revokec chan struct{} // Lease 被撤销时，会关闭此通道，从而实现监听效果
  }

  // 查看其是否过期
  func (l *Lease) expired() bool
  // 如果 l.expiry 未定义，则返回无限大，否则返回剩余时长 duration
  func (l *Lease) Remaining() time.Duration 
  // 存储与 Lease 绑定的所有 kv 的 key 的集合
  func (l *Lease) Keys() []string
  // 把 Lease 的过期时间点 expiry 设置为 0 时刻
  func (l *Lease) forever()
  // 把 Lease.expiry 延长 extend 秒
  func (l *Lease) refresh(extend time.Duration)
  // 构造一个 leasepb.Lease{ID: int64(l.ID), TTL: l.ttl, RemainingTTL: l.remainingTTL}，
  // 存储到 bolt 的 "lease" bucket 中
  func (l *Lease) persistTo(b backend.Backend)
```

## 2 etcd 时钟 

etcd 时钟是一个时间小顶堆，在堆顶存放着即将过期的 LeaseWithTime。

时钟堆只存放与 lease 相关的 ID 及其过期时间，lease 全部内容则存与 lessor.Leasemap 中。

### 2.1 LeaseQueue

小顶堆是基于 队列实现的，其基本 item 是 LeaseWithTime，是 struct Lease 的简略项。

```go
  // 包含了一个lease 对象，以及其过期时间
  type LeaseWithTime struct {
	id LeaseID  // lease 对象的 id
	// Unix nanos timestamp.
	time  int64 // 过期时间
	index int   // 在 LeaseQueue 中的数组下标
  }

  // lease 时间堆，其大小比较是通过过期时间的绝对值比较大小
  type LeaseQueue []*LeaseWithTime
```

### 2.2 LeaseExpiredNotifier

```go
  // LeaseExpiredNotifier is a queue used to notify lessor to revoke expired lease.
  // Only save one item for a lease, `Register` will update time of the corresponding lease.
  // LeaseExpiredNotifier 里面有一个队列，主要用于计算最近过期的 lease，然后通知 lessor 删除过期的 lease
  // 里面只存储一个 lease 的 item，`Register` 接口用于更新对应 lease 的过期时间
  type LeaseExpiredNotifier struct {
    // 用于 check LeaseID 对应的 LeaseWithTime 在 queue 中是否存在，无其他用处
	m     map[LeaseID]*LeaseWithTime 
	queue LeaseQueue // 时间堆
  }
 
  // 注册或者更新 @item
  func (mq *LeaseExpiredNotifier) RegisterOrUpdate(item *LeaseWithTime)
  // 删除最近过期的 LeaseWithItem
  func (mq *LeaseExpiredNotifier) Unregister() *LeaseWithTime
  // 获取堆头，离过期时间最近的 LeaseWithItem
  func (mq *LeaseExpiredNotifier) Poll() *LeaseWithTime
```

## 3 Lessor

翻译：lessor 出租方，lessee 承租方。
lessor 就是 lease 的管理方，lessee 则是各个 kv。 

lessor 每次处理顶多处理 500 个过期 lease，防止瞬间过多的 lease 过期，导致 server 忙于处理过期 kv 而无法响应外部响应。 

```go
  // Lessor 拥有 lease【租约】，可以用于授权、销毁、重新续租和修改 [grant/revoke/renew/modify] 承租方 [lessee] 的 lease。
  type Lessor interface {
    // 给 lessor 创建 TxnDeletes，以用于 store。
    // Lessor 通过创建一个新的 TxnDeletes 删除被废除的后者过期的 lease。
	SetRangeDeleter(rd RangeDeleter)

	SetCheckpointer(cp Checkpointer)
    // 给承租方 @id 创建一个时长为 @ttl 的 Lease
	Grant(id LeaseID, ttl int64) (*Lease, error)
    // Remove 根据一个租约 ID @id 删除对应的租约，与租约相关 kv 都会被删除。如果 @id 不存在，则会返回相应的错误。
	Revoke(id LeaseID) error

	// Checkpoint 设定 lease @id 的 ttl。 @remainingTTL 会在 Promote 中被使用，用于缩短 @id 的 TTL。
	Checkpoint(id LeaseID, remainingTTL int64) error

    // Attach 用于给 lease @id 绑定其相关的 kv 组 @items。
	Attach(id LeaseID, items []LeaseItem) error

    // 获取 @item 对应的 LeaseID 
	// If no lease found, NoLease value will be returned.
	GetLease(item LeaseItem) LeaseID

    // 把 @items 从 @id 解绑
	// If the lease does not exist, an error will be returned.
	Detach(id LeaseID, items []LeaseItem) error

	// Promote 把 lessor 提升为 主 lessor。主 lessor 用于管理 lease 的过期和续租。
	// Newly promoted lessor renew the TTL of all lease to extend + previous TTL.
	Promote(extend time.Duration)

    // 与 Promote 相反，对 lessor 进行降级。 
	Demote()

	// Renew  用于给 lease @id 重新续租。
	// It returns the renewed TTL. If the ID does not exist,
	// an error will be returned.
	Renew(id LeaseID) (int64, error)

	// Lookup gives the lease at a given lease id, if any
	Lookup(id LeaseID) *Lease

	// Leases lists all leases.
	Leases() []*Lease

	// ExpiredLeasesC 返回一个 channel，用于使用者接收过期的 lease
	ExpiredLeasesC() <-chan []*Lease

	// Recover 根据 backend 的存储回复一个 lessor，并使用 RangeDelete @rd 删除过期的时间节点
	Recover(b backend.Backend, rd RangeDeleter)

	// Stop stops the lessor for managing leases. The behavior of calling Stop multiple
	// times is undefined.
	Stop()
  }
```

### 3.1 lessor

lessor 实现了接口 Lessor, 管理所有的 lease，实现过期 kv 检查与 checkpoint 检查。

```go
  type lessor struct {
    //  如果 lessor 是主 lessor，则创建该 channel。否则关闭它。 
	demotec chan struct{}
    // map: lease id -> lease  
	leaseMap             map[LeaseID]*Lease
    // 时钟堆 
	leaseExpiredNotifier *LeaseExpiredNotifier
    // lease 队列
	leaseCheckpointHeap  LeaseQueue
    // kv 所使用的 lease 的 ID
	itemMap              map[LeaseItem]LeaseID

    // 如果租约 lease 过时，则 lessor 会通过下面这个接口删除其过期的 key 或者一个 range
	rd RangeDeleter

    // 当发生 leader 选举和重启时，一个 lease 应当被固化到 bolt 中，lessor 会使用下面的 @cp 对 lease 进行检测。  
    // leaseCheckpointHeap 中的数据会执行 Checkpointer 方法
	cp Checkpointer

    // lease 存储点。这里面只存储 lease ID 及其从当前时间开始的过期时间。
    // 当重启时，从存储内容中恢复时，根据 lease 检测 kv 的有效性。
	b backend.Backend

    // 最小 lease ttl。
	minLeaseTTL int64

    // 过期的 lease 写入此 channel，等待被 EtcdServer 调用处理。
	expiredC chan []*Lease
    // stopC 用于标记 lessor 将要停止。
	stopC chan struct{}
    // doneC 用于标记 lessor 已经停止。
	doneC chan struct{}

	lg *zap.Logger

    // checkpoint 时间间隔 
	checkpointInterval time.Duration
	// the interval to check if the expired lease is revoked
	expiredLeaseRetryInterval time.Duration
  }

  // 如果当前 lessor 是 主 lessor，则调用 findExpiredLeases 寻找过期的 lease 数组，
  // 然后放入 lessor.expiredC。
  // 需要注意的是，lessor 会确保 lease 数组不会大于 500 [= leaseRevokeRate/2]
  func (le *lessor) revokeExpiredLeases() 
  // 调用 expireExists，返回固定数目以内的的 Lease 集合  
  func (le *lessor) findExpiredLeases(limit int) []*Lease 

  // lessor 创建时就会启动一个 runLoop 异步 goroutine，每个 500ms 检测超时的 lease，然后把超时的 lease 返回。
  // 检查过期的 lease 和 checkpoint lease
  func (le *lessor) runLoop() {
	defer close(le.doneC)

	for {
        // 删除过期的 lease 
		le.revokeExpiredLeases()
        // 检查调度的租约
		le.checkpointScheduledLeases()

		select {
		case <-time.After(500 * time.Millisecond):
		case <-le.stopC:
			return
		}
	}
  }
```

> 除过期的 lease  

```go
  // 如果当前 lessor 是 主 lessor，则调用 findExpiredLeases 寻找过期的 lease 数组，
  // 然后放入 lessor.expiredC。
  // 需要注意的是，lessor 会确保 lease 数组不会大于 500 [= leaseRevokeRate/2]
  func (le *lessor) revokeExpiredLeases() 
  // 调用 expireExists，返回固定数目以内的的 Lease 集合  
  func (le *lessor) findExpiredLeases(limit int) []*Lease 

  // findExpiredLeases loops leases in the leaseMap until reaching expired limit
  // and returns the expired leases that needed to be revoked.
  func (le *lessor) findExpiredLeases(limit int) []*Lease {
	leases := make([]*Lease, 0, 16)

	for {
		l, ok, next := le.expireExists()
		if !ok && !next {
			break
		}
        // 确认 lease 确实超时
		if l.expired() {
			leases = append(leases, l)
			// reach expired limit
			if len(leases) == limit {
				break
			}
		}
	}

	return leases
  }

  // ok 为 true，说明获取到了过期的 Lease 
  // 如果再尝试一次，也许还能获取到新的超时 Lease，则 "next" 为 true
  func (le *lessor) expireExists() (l *Lease, ok bool, next bool) {
    // 读取堆顶，但是不从堆所在的队列中删除它
	item := le.leaseExpiredNotifier.Poll()
	l = le.leaseMap[item.id]
	if l == nil {
        // 时间堆中 leaseWithTime 对应的 lease 已经不存在，
		// 把 leaseWithTime 从时钟轮 le.leaseExpiredNotifier 中删除
		le.leaseExpiredNotifier.Unregister() // O(log N)
		return nil, false, true
	}
	now := time.Now()
	if now.UnixNano() < item.time /* expiration time */ {
        // 没有超时
		return l, false, false
	}

    // 这里重新更新时间是为了防止机器时钟错乱
    // 重新放入时钟堆，让这个元素不要在顶部，以便下一次收集过期元素时因为找不到对应的 lease 而被删除 
    // 即会在上面第一个 if 逻辑段内执行删除操作
	// recheck if revoke is complete after retry interval
	item.time = now.Add(le.expiredLeaseRetryInterval).UnixNano()
	le.leaseExpiredNotifier.RegisterOrUpdate(item)
	return l, true, false
  }
```

## 4 server

EtcdServer 通过 lessor.ExpiredLeasesC() 函数获取超时的 lease，然后处理。

```go 
  // etcdserver/v3_server.go
  func (s *EtcdServer) run() {
	var expiredLeaseC <-chan []*lease.Lease
	if s.lessor != nil {
		expiredLeaseC = s.lessor.ExpiredLeasesC()
	}
			
	for {
		select {
			case leases := <-expiredLeaseC: // 过期的 lease
        		s.goAttach(func() {
        			c := make(chan struct{}, maxPendingRevokes)
        			for _, lease := range leases {
         				lid := lease.ID
        				s.goAttach(func() { // 撤销租约
        					ctx := s.authStore.WithRoot(s.ctx)
        					_, lerr := s.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: int64(lid)})

        					<-c
        				})
        			}
        		})
		}
	}
  }

  func (s *EtcdServer) LeaseRevoke(ctx context.Context, r *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	resp, err := s.raftRequestOnce(ctx, pb.InternalRaftRequest{LeaseRevoke: r})
	return resp.(*pb.LeaseRevokeResponse), nil
  }
  func (s *EtcdServer) raftRequestOnce(ctx context.Context, r pb.InternalRaftRequest) (proto.Message, error) {
	result, err := s.processInternalRaftRequestOnce(ctx, r)
  }

  func (s *EtcdServer) processInternalRaftRequestOnce(ctx context.Context, r pb.InternalRaftRequest) (*applyResult, error) {
	data, err := r.Marshal()
	err = s.r.Propose(cctx, data)
	select {
	case x := <-ch:
		return x.(*applyResult), nil
	}
  }
```

在 s.LeaseRevoke 中，走完整个提案流程后，才会转换为 apply 消息，被节点应用。
server 处理过期 kv 的过程是一个完整的提案过程。

```go 
  // applyEntryNormal apples an EntryNormal type raftpb request to the EtcdServer
  func (s *EtcdServer) applyEntryNormal(e *raftpb.Entry) {
	var ar *applyResult
	needResult := s.w.IsRegistered(id)
	if needResult || !noSideEffect(&raftReq) {
	  ar = s.applyV3.Apply(&raftReq)
	}
  }

  // etcdserver/apply.go
  func (a *applierV3backend) Apply(r *pb.InternalRaftRequest) *applyResult {
	ar := &applyResult{}
	switch {
	case r.LeaseGrant != nil:
		ar.resp, ar.err = a.s.applyV3.LeaseGrant(r.LeaseGrant)
	case r.LeaseRevoke != nil:
		ar.resp, ar.err = a.s.applyV3.LeaseRevoke(r.LeaseRevoke)
	case r.LeaseCheckpoint != nil:
		ar.resp, ar.err = a.s.applyV3.LeaseCheckpoint(r.LeaseCheckpoint)
	}
	return ar
  }

  func (a *applierV3backend) LeaseGrant(lc *pb.LeaseGrantRequest) (*pb.LeaseGrantResponse, error) {
	l, err := a.s.lessor.Grant(lease.LeaseID(lc.ID), lc.TTL)
	return resp, err
  }

  func (a *applierV3backend) LeaseRevoke(lc *pb.LeaseRevokeRequest) (*pb.LeaseRevokeResponse, error) {
	err := a.s.lessor.Revoke(lease.LeaseID(lc.ID))
	return &pb.LeaseRevokeResponse{Header: newHeader(a.s)}, err
  }
``` 
