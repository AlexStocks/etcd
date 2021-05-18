# etcd

## etcd dir

```

  ├── Documentation  # 文档
  ├── auth  # 认证
  ├── bin  # 编译出来的二进制文件
  ├── client  # 应该是v2版本的客户端代码
  ├── clientv3  # 应该是v3版本的客户端代码
  ├── contrib  # 今天我们要看的raftexample就在这里面
  ├── default.etcd  # 运行编译好的etcd产生的，忽略之
  ├── docs  # 文档
  ├── embed  # 封装了etcd的函数，以便别的程序封装
  ├── etcdctl  # etcdctl命令，也就是客户端
  ├── etcdmain  # main.go 调用了这里
  ├── etcdserver  # 服务端代码
  ├── functional  # 不知道是干啥的，看起来是用来验证功能的测试套件
  ├── hack  # 开发者用的
  ├── integration
  ├── lease  # 实现etcd的租约
  ├── logos
  ├── mvcc # MVCC存储的实现
  ├── pkg  # 通用库
  ├── proxy  # 代理
  ├── raft  # raft一致性协议的实现
  ├── scripts  # 各种脚本
  ├── tests
  ├── tools  # 一些工具
  ├── vendor  # go的vendor，忽略
  ├── version  # 版本信息
  └── wal  # Write-Ahead-Log的实现

```

## 对外接口

一个etcd节点运行以后，有3个通道接收外界消息，以kv数据的增删改查请求处理为例，介绍这3个通道的工作机制。

1. client的http调用：会通过注册到http模块的keysHandler的ServeHTTP方法处理。解析好的消息调用EtcdServer的Do()方法处理。(图中2)
2. client的grpc调用：启动时会向grpc server注册quotaKVServer对象，quotaKVServer是以组合的方式增强了kvServer这个数据结构。grpc消息解析完以后会调用kvServer的Range、Put、DeleteRange、Txn、Compact等方法。kvServer中包含有一个RaftKV的接口，由EtcdServer这个结构实现。所以最后就是调用到EtcdServer的Range、Put、DeleteRange、Txn、Compact等方法。(图中1)
3. 节点之间的grpc消息：每个EtcdServer中包含有Transport结构，Transport中会有一个peers的map，每个peer封装了节点到其他某个节点的通信方式。包括streamReader、streamWriter等，用于消息的发送和接收。streamReader中有recvc和propc队列，streamReader处理完接收到的消息会将消息推到这连个队列中。由peer去处理，peer调用raftNode的Process方法处理消息。(图中3、4)

## 内部源码

### EtcdServer

```go
type EtcdServer struct {
    // 当前正在发送的snapshot数量
    inflightSnapshots int64 
    //已经apply到状态机的日志index
    appliedIndex      uint64 
    //已经提交的日志index，也就是leader确认多数成员已经同步了的日志index
    committedIndex    uint64 
    //已经持久化到kvstore的index
    consistIndex consistentIndex 
    //配置项
    Cfg          *ServerConfig
    //启动成功并注册了自己到cluster，关闭这个通道。
    readych chan struct{}
    //重要的数据结果，存储了raft的状态机信息。
    r       raftNode
    //满多少条日志需要进行snapshot
    snapCount uint64
    //为了同步调用情况下让调用者阻塞等待调用结果的。
    w wait.Wait
    //下面3个结果都是为了实现linearizable 读使用的
    readMu sync.RWMutex
    readwaitc chan struct{}
    readNotifier *notifier
    //停止通道
    stop chan struct{}
    //停止时关闭这个通道
    stopping chan struct{}
    //etcd的start函数中的循环退出，会关闭这个通道
    done chan struct{}
    //错误通道，用以传入不可恢复的错误，关闭raft状态机。
    errorc     chan error
    //etcd实例id
    id         types.ID
    //etcd实例属性
    attributes membership.Attributes
    //集群信息
    cluster *membership.RaftCluster
    //v2的kv存储
    store       store.Store
    //用以snapshot
    snapshotter *snap.Snapshotter
    //v2的applier，用于将commited index apply到raft状态机
    applyV2 ApplierV2
    //v3的applier，用于将commited index apply到raft状态机
    applyV3 applierV3
    //剥去了鉴权和配额功能的applyV3
    applyV3Base applierV3
    //apply的等待队列，等待某个index的日志apply完成
    applyWait   wait.WaitTime
    //v3用的kv存储
    kv         mvcc.ConsistentWatchableKV
    //v3用，作用是实现过期时间
    lessor     lease.Lessor
    //守护后端存储的锁，改变后端存储和获取后端存储是使用
    bemu       sync.Mutex
    //后端存储
    be         backend.Backend
    //存储鉴权数据
    authStore  auth.AuthStore
    //存储告警数据
    alarmStore *alarm.AlarmStore
    //当前节点状态
    stats  *stats.ServerStats
    //leader状态
    lstats *stats.LeaderStats
    //v2用，实现ttl数据过期的
    SyncTicker *time.Ticker
    //压缩数据的周期任务
    compactor *compactor.Periodic
    //用于发送远程请求
    peerRt   http.RoundTripper
    //用于生成请求id
    reqIDGen *idutil.Generator
    // forceVersionC is used to force the version monitor loop
    // to detect the cluster version immediately.
    forceVersionC chan struct{}
    // wgMu blocks concurrent waitgroup mutation while server stopping
    wgMu sync.RWMutex
    // wg is used to wait for the go routines that depends on the server state
    // to exit when stopping the server.
    wg sync.WaitGroup
    // ctx is used for etcd-initiated requests that may need to be canceled
    // on etcd server shutdown.
    ctx    context.Context
    cancel context.CancelFunc
    leadTimeMu      sync.RWMutex
    leadElectedTime time.Time
}
```

## Main

### read config

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

// embed/etcd.go
func StartEtcd(inCfg *Config) (e *Etcd, err error) {
	srvcfg := etcdserver.ServerConfig{}
    e.Server = etcdserver.NewServer(srvcfg)
    e.Server.Start()
}

// startEtcd runs StartEtcd in addition to hooks needed for standalone etcd.
func startEtcd(cfg *embed.Config) (<-chan struct{}, <-chan error, error) {
	embed.StartEtcd(cfg)
}

// etcdmain/etcd.go
func startEtcdOrProxyV2() {
    // 读取配置
    cfg := newConfig()
	cfg.parse(os.Args[1:])
    // 启动 etcd
    startEtcd(&cfg.ec)
	select {
	case <-stopped:
	}

	osutil.Exit(0)
}

// etcdmain/main.go
func Main() {
	startEtcdOrProxyV2()
}
```

### main procedure

```go
// pkg/transport/listener.go
func newListener(addr string, scheme string) (net.Listener, error) {
	if scheme == "unix" || scheme == "unixs" {
		// unix sockets via unix://laddr
		return NewUnixListener(addr)
	}
	return net.Listen("tcp", addr)
}

// 创建 etcd peer 之间的监听端口 
// embed/etcd.go
func configurePeerListeners(cfg *Config) (peers []*peerListener, err error) {
	peers = make([]*peerListener, len(cfg.LPUrls))
	for i, u := range cfg.LPUrls {
	    peers[i].Listener, err = rafthttp.NewListener(u, &cfg.PeerTLSInfo)
		// once serve, overwrite with 'http.Server.Shutdown'
		peers[i].close = func(context.Context) error {
			return peers[i].Listener.Close()
		}
	}
	return peers, nil
}

// 创建对 client 的监听端口
// embed/etcd.go
func configureClientListeners(cfg *Config) (sctxs map[string]*serveCtx, err error) {
	sctxs = make(map[string]*serveCtx)
	for _, u := range cfg.LCUrls {
	    sctx.l = net.Listen(network, addr)
	    sctxs[addr] = sctx
	}
	return sctxs, nil
}

// 构建 server 对象：
// 1 创建 backend；
// 2 创建 EtcdServer 对象，包含了 RaftNode； 
// 3 创建 lessor；
// 4 创建 kv 对象；
// 5 创建 compactor 对象；
// 6 创建 raftHTTP transport 通信对象；
// etcdserver/server.go
func NewServer(cfg ServerConfig) (srv *EtcdServer, err error) {
    haveWAL := wal.Exist(cfg.WALDir())	
    ss := snap.New(cfg.Logger, cfg.SnapDir())
    be := openBackend(cfg)
	switch {
	case haveWAL:
		cl.SetStore(st)
		cl.SetBackend(be)
	}
	srv = &EtcdServer{
		r: *newRaftNode(
			raftNodeConfig{
				isIDRemoved: func(id uint64) bool { return cl.IsIDRemoved(types.ID(id)) },
				Node:        n,
				raftStorage: s,
				storage:     NewStorage(w, ss),
			},
		),		
	}
	srv.lessor = lease.NewLessor(srv.be)
	srv.kv = mvcc.New(srv.getLogger(), srv.be, srv.lessor, srv.authStore, &srv.consistIndex, mvcc.StoreConfig{CompactionBatchLimit: cfg.CompactionBatchLimit})

	srv.consistIndex.setConsistentIndex(srv.kv.ConsistentIndex())
	if num := cfg.AutoCompactionRetention; num != 0 {
		srv.compactor, err = v3compactor.New(cfg.Logger, cfg.AutoCompactionMode, num, srv.kv, srv)
		if err != nil {
			return nil, err
		}
		srv.compactor.Run()
	}

	srv.applyV3Base = srv.newApplierV3Backend()
	if err = srv.restoreAlarms(); err != nil {
		return nil, err
	}

	if srv.Cfg.EnableLeaseCheckpoint {
		// setting checkpointer enables lease checkpoint feature.
		srv.lessor.SetCheckpointer(func(ctx context.Context, cp *pb.LeaseCheckpointRequest) {
			srv.raftRequestOnce(ctx, pb.InternalRaftRequest{LeaseCheckpoint: cp})
		})
	}

	// TODO: move transport initialization near the definition of remote
	tr := &rafthttp.Transport{
		Raft:        srv,
	}
	tr.Start()
	// add all remotes into transport
	for _, m := range remotes {
		if m.ID != id {
			tr.AddRemote(m.ID, m.PeerURLs)
		}
	}
	for _, m := range cl.Members() {
		if m.ID != id {
			tr.AddPeer(m.ID, m.PeerURLs)
		}
	}
	srv.r.transport = tr

	return srv, nil	
}

// embed/etcd.go
func StartEtcd(inCfg *Config) (e *Etcd, err error) {
	// 启动客户端监听
	e = &Etcd{cfg: *inCfg, stopc: make(chan struct{})}
	e.Peers = configurePeerListeners(cfg)
	e.sctxs = configureClientListeners(cfg)
	for _, sctx := range e.sctxs {
		 e.Clients = append(e.Clients, sctx.l)
	}
    e.Server = etcdserver.NewServer(srvcfg)
    e.Server.Start()
    e.servePeers()
    e.serveClients()
   	return e, nil
}

// startEtcd runs StartEtcd in addition to hooks needed for standalone etcd.
func startEtcd(cfg *embed.Config) (<-chan struct{}, <-chan error, error) {
	e = embed.StartEtcd(cfg)
	osutil.RegisterInterruptHandler(e.Close)
	select {
	case <-e.Server.ReadyNotify(): // wait for e.Server to join the cluster
	case <-e.Server.StopNotify(): // publish aborted from 'ErrStopped'
	}
	return e.Server.StopNotify(), e.Err(), nil
}

// etcdmain/etcd.go
func startEtcdOrProxyV2() {
    // 读取配置
    cfg := newConfig()
	cfg.parse(os.Args[1:])
    // 启动 etcd
    startEtcd(&cfg.ec)
	select {
	case <-stopped:
	}

	osutil.Exit(0)
}

// etcdmain/main.go
func Main() {
	startEtcdOrProxyV2()
}
```

### 发起请求

```go
type RequestHeader struct {
    // 全局请求 ID
	ID uint64
	// username is a username that is associated with an auth token of gRPC connection
	Username string
	// auth_revision is a revision number of auth.authStore. It is not related to mvcc
	AuthRevision uint64
}

// An InternalRaftRequest is the union of all requests which can be
// sent via raft.
type InternalRaftRequest struct {
	Header   *RequestHeader
	ID       uint64
}

// etcdserver/v3_server.go
func (s *EtcdServer) processInternalRaftRequestOnce(ctx context.Context, r pb.InternalRaftRequest) (*applyResult, error) {
    // 如果 ci - ai > 5000，则拒绝发起请求
	ai := s.getAppliedIndex()
	ci := s.getCommittedIndex()
	if ci > ai+maxGapBetweenApplyAndCommitIndex {
		return nil, ErrTooManyRequests
	}

    // 构建全局请求唯一 ID
	r.Header = &pb.RequestHeader{
		ID: s.reqIDGen.Next(),
	}
	data := r.Marshal()
	id := r.ID
	if id == 0 {
		id = r.Header.ID
	}
	
    // 注册请求上下文，并发起请求
	ch := s.w.Register(id)
	err = s.r.Propose(cctx, data)
	if err != nil {
        // 发起请求失败，删除注册中心内 @id 相关的请求上下文
		s.w.Trigger(id, nil) // GC wait
		return nil, err
	}
 
    // 阻塞等待响应
	select {
	case x := <-ch:
		return nil, ErrStopped
	}
}

// 发起本地消息 MsgProp 
func (n *node) Propose(ctx context.Context, data []byte) error {
	return n.stepWait(ctx, pb.Message{Type: pb.MsgProp, Entries: []pb.Entry{{Data: data}}})
}

```














