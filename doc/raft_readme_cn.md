## Etcd Raft 代码笔记

## 1 Message

Raft 集群各个 node 之间的通信是通过 Message 进行的，是各种通信信息的载体。例如 leader 向 follower 发送日志时，为了提高吞吐率，一次发送一条 message 中包含了多个 log entry。

Message 实体定义如下：

```protobuf
  // raft/raftpb/raft.proto
  message Message {
  	optional MessageType type        = 1  [(gogoproto.nullable) = false];
  	optional uint64      to          = 2  [(gogoproto.nullable) = false];
  	optional uint64      from        = 3  [(gogoproto.nullable) = false];
  	optional uint64      term        = 4  [(gogoproto.nullable) = false];
  	optional uint64      logTerm     = 5  [(gogoproto.nullable) = false];
  	optional uint64      index       = 6  [(gogoproto.nullable) = false];
  	repeated Entry       entries     = 7  [(gogoproto.nullable) = false];
  	optional uint64      commit      = 8  [(gogoproto.nullable) = false];
  	optional Snapshot    snapshot    = 9  [(gogoproto.nullable) = false];
  	optional bool        reject      = 10 [(gogoproto.nullable) = false];
  	optional uint64      rejectHint  = 11 [(gogoproto.nullable) = false];
  	optional bytes       context     = 12;
  }
```

主要字段定义如下： 
  * Type：消息类型
    消息类型有很多，大致可以分成两类：
    + Raft 协议相关的，包括心跳 MsgHeartbeat、日志 MsgApp、投票消息 MsgVote 等。
    + 上层应用触发的消息，比如应用对数据更改的消息 MsgProp。
  * To, From分别代表了这个消息的接受者和发送者。
  * Term：这个消息发出时整个集群所处的任期
    + 如果是应用层向本地 raft 发送的消息，其 term 为 0；
    + 如果是网络其他 raft node 转发来的 message，其 term 不为 0。
  * LogTerm：消息发出者所保存的日志中最后一条的任期号，一般MsgVote会用到这个字段。
  * Index：日志索引号。如果当前消息是MsgVote的话，代表这个candidate最后一条日志的索引号，它跟上面的LogTerm一起代表这个candidate所拥有的最新日志信息，这样别人就可以比较自己的日志是不是比candidata的日志要新，从而决定是否投票。
  * Entries：需要存储的日志。
  * Commit：已经提交的日志的索引值，用来向别人同步日志的提交信息。
  * Snapshot：一般跟MsgSnap合用，用来放置具体的Snapshot值。
  * Reject，RejectHint：代表对方节点拒绝了当前节点的请求(MsgVote/MsgApp/MsgSnap…)

### 1.1 Message Type 

Raft 包用 pb 协议描述收到的消息。Node 角色的不同决定其使用不同的消息处理方法，如
stepFollower/stepCandidate/stepLeader 等。每个 step 方法处理消息的第一步都是
检验 message 中每个 log entry term 是否与当前 node term 一致，防止收到过时消息。

	'MsgHup' 类型消息用于选举。如果一个 node 是 follower/candidate，则 raft 结构体
	中的 'tick' 会被设置为 'tickElection'。如果 follower/candidate 在选举超时前
	没有收到任何心跳消息，则它会调用 step 函数发送 MsgHup 消息，然后转换角色为 follower
	以发起新一轮选举。

	'MsgBeat' 用于标识心跳消息。leader node 会调用 'tickHeartbeat' 函数, 周期性
	地向 follower 发送心跳消息。
	
	'MsgProp' 应用层发起的数据固化类型消息。发送者会重写消息的 term 值为 HardState.term。
	当 leader 的 step 方法收到这个类型的消息后，首先调用 'appendEntry' 方法把 entry 写入
	自己的 log entry 集合，然后调用 'bcastAppend' 方法把消息发送给各个 follower。
	candidate 收到这个类型的消息后，丢弃掉不处理。如果 follower 收到这个类型的消息，则
	会 redirect 发往 leader。

	'MsgApp' 类型消息是用于复制的消息。leader 通过 bcastAppend -> sendAppend 把消息发送
	给各个 followers。如果 candidate 收到这种类型的消息，则转换角色为 follower。
	follower/candidate 收到消息后，调用 'handleAppendEntries' 处理相关消息。

	'MsgAppResp' 是对 'MsgApp' 类型消息的回复。

	'MsgVote' 选举消息。如果 follower/candidate 收到 'MsgHup' 类型消息，则会调用
	'campaign' 发送 'MsgVote' 类型消息以竞选 leader。leader/candidate 收到消息后，
	如果消息的 term 比本地 term 小，则给发送者回一个 'MsgVoteResp' 消息，消息中的 reject
	为 true。如果 term 比本地大，则 leader/candidate 会转换为 follower。

	'MsgVoteResp' 选举响应消息。

	'MsgPreVote' 和 'MsgPreVoteResp' 用于两阶段竞选协议。如果 Config.PreVote 为 true，
	一个 node 发起正式 veote 之前会先发起 preVote，先不增加自身的 term，除非预选
	阶段的响应消息预示其能赢得选举。这种消息用于减少 网络分区后的网络恢复的情况下，减少选举。

	'MsgSnap' snapshot 类型消息。leader 收到 'MsgProp' 类型消息后，通过
	'bcastAppend' -> 'sendAppend' 把消息广告给各个 follower，如果 leader 获取 term 失败，
	则 leader 会发送 'MsgSnap' 类型消息给 follower。

	'MsgSnapStatus' snapshot 响应消息。如果因为网络层原因发送消息失败，leader 获取到失败消息后
	把 follower 置为 probe。否则 leader 会知道 follower 已经成功获取 snapshot，重启其状态并
	回复数据复制。

	'MsgHeartbeat' 心跳消息。发送者为 leader。如果接受者为 candidate，则降级为 follower，
	然后把 committed index 转换为心跳消息中的 index，并把 leader 置位消息中的 leaderID。
	
	'MsgHeartbeatResp' 心跳响应消息。leader 收到 follower 发来的响应消息后，会根据
	消息中的 committed index 更新 ProgressTracker 中 follower id 对应的 match 字段。

	'MsgUnreachable' 不可达消息。一般是对 'MsgApp' 类型的消息回复，leader 收到这种
	类型的消息后，会把 follower 状态调整为 probe 状态。
	
其他消息类型定义如下：

```go
  // raft/raftpb/raft.proto
  enum MessageType {
    MsgHup            = 0   // 本地消息：选举，可能会触发 pre-vote 或者 vote
    MsgBeat           = 1   // 本地消息：心跳，触发放给 peers 的 Msgheartbeat
    MsgProp           = 2   // 本地消息：Propose，触发 MsgApp
    MsgApp            = 3   // 非本地：Op log 复制/配置变更 request
    MsgAppResp        = 4   // 非本地：Op log 复制 response
    MsgVote           = 5   // 非本地：vote request
    MsgVoteResp       = 6   // 非本地：vote response
    MsgSnap           = 7   // 非本地：Leader 向 Follower 拷贝 Snapshot，response Message 就是 MsgAppResp，通过这个值告诉 Leader 继续复制后面的日志
    MsgHeartbeat      = 8   // 非本地：心跳 request
    MsgHeartbeatResp  = 9   // 非本地：心跳 response
    MsgUnreachable    = 10  // 本地消息：EtcdServer 通过这个消息告诉 raft 状态某个 Follower 不可达，让其发送 message方式由 pipeline 切成 ping-pong 模式
    MsgSnapStatus     = 11  // 本地消息：EtcdServer 通过这个消息告诉 raft 状态机 snapshot 发送成功还是失败
    MsgCheckQuorum    = 12  // 本地消息：CheckQuorum，用于 Lease read，Leader lease
    MsgTransferLeader = 13  // 本地消息：可能会触发一个空的 MsgApp 尽快完成日志复制，也有可能是 MsgTimeoutNow 出 Transferee 立即进入选举
    MsgTimeoutNow     = 14  // 非本地：触发 Transferee 立即进行选举
    MsgReadIndex      = 15  // 非本地：Read only ReadIndex
    MsgReadIndexResp  = 16  // 非本地：Read only ReadIndex response
    MsgPreVote        = 17  // 非本地：pre vote request
    MsgPreVoteResp    = 18  // 非本地：pre vote response
  }
```

所谓本地消息，是应用层发给 raft node 的 message。非本地消息，则是各个 raft node 之间的通信 message。

### 1.2 心跳消息 

```Go
  // raft/raft.go
  // follower 收到的心跳消息中的 commit 值不能大于 follower 上报的 match 值
  func (r *raft) sendHeartbeat(to uint64, ctx []byte) 
```

preVote 发起预选，此时并不增加 term，主要用于检测网络是否恢复、其数据是否是最新的。

### 1.3 Ready 消息

应用层向 raft 提交一阶段 MsgPro 消息，如果经过二阶段的投票通过，则 raft 会给应用层返回集群消息 Ready：

```go
  // raft/node.go
  
  // Ready encapsulates the entries and messages that are ready to read,
  // be saved to stable storage, committed or sent to other peers.
  // All fields in Ready are read-only.
  type Ready struct {
  	// 包括自身的 role、leader ID
  	// nil代表软状态没有变化
  	*SoftState
  	// 包括了 term、commit、vote 等值
  	// 这些值会被持久化到磁盘上，所以称之为 hard state
  	pb.HardState
  	// Node.ReadIndex() 的回调，是某一个时刻的集群最大提交索引。
  	ReadStates []ReadState
  	// 待被存入可靠存储 WAL 的 message，来自 log.unstable
  	Entries []pb.Entry
  	// 待被存储的快照，来自 log.unstable
  	Snapshot pb.Snapshot
  	// 已经提交有待被 applied 的日志，
  	CommittedEntries []pb.Entry
  	// 已经被存入可靠存储，待被同步给其他 node 的 message。
  	// 如果这其中有 MsgSnqp 消息，当接收
    // snapshot 完毕 或者 接收失败，都要通过 ReportSnapshot 向 leader 汇报接收结果。 
  	Messages []pb.Message
  	// 指示了 hardstate、unstable entry 是否必须同步写入磁盘，如果为否则异步写入即可
  	// 如果为 true，则必须同步成功调用 Advance()
  	MustSync bool
  }
```

Ready 中记录了可存入 WAL 的 Entries，持久快照 Snapshot、写入状态机处理的 CommittedEntries、待广播给其他 peer 的 Messages。

## 2 Raft Node 

Raft Node 可以认为是 raft 状态机的更进一步封装，提供了更高级的接口。

```go
  // raft/node.go

  // 这里面存储了如下系列的函数：
  // 竞选： Campaign
  // 向集群提交：Propose() + ProposeConfChange()
  // raft：
  //   * 向 raft 提交日志 Step()
  //   * 从 raft 获取已经提交的数据 Ready()
  //   * 告诉 raft 已经处理完毕一批数据 Advance()
  type Node interface {
  	// raft 算法自身没有计时器，其计时是由这个接口驱动的，每调用一次内部计数一次。raft 内部计时有 心跳 和 选举 两种。
  	Tick()
  	// 把 node 状态转换为 candidate 状态，并开始发起选举
  	Campaign(ctx context.Context) error
  	// 把 data 广播给其他 follower，如果当前 node 不是 leader，则会把数据数据发送到 leader。
    //
  	// 需要注意：该函数虽然返回错误代码，但是返回nil不代表data已经被超过半数节点接收了。因为提议
  	// 是一个异步的过程，该接口的返回值只能表示提议这个操作是否被允许，比如在没有选出Leader的情况下
  	// 是无法提议的。而提议的数据被超过半数的节点接受是通过下面的Ready()获取的。
  	Propose(ctx context.Context, data []byte) error
  	// 提交 raft 集群配置数据
  	ProposeConfChange(ctx context.Context, cc pb.ConfChangeI) error
  	// 把数据传输给 raft
  	Step(ctx context.Context, msg pb.Message) error
  	// node 从这个接口获取 commited 数据，把一些数据应用起来，从 commited 转换为 applied。相当于数据会先被
  	// Propose 提交，然后再通过这个函数返回的 chan 通道获取响应值。
  	Ready() <-chan Ready
  	// 通过这个接口告诉 raft，其已经处理完毕 Ready() <-chan Ready 返回的数据，raft 收到 Advance() 接口里面的
  	// 发来的通知后，才会通过 <- chan Ready 发来下一批数据，否则 Node 会阻塞在 <- chan Ready 这个通道上。
  	Advance()
  	// ProposeConfChange() 应用最新的 raft 集群配置。
  	ApplyConfChange(cc pb.ConfChangeI) *pb.ConfState
  	// 把 leader 转移到 transferee
  	TransferLeadership(ctx context.Context, lead, transferee uint64)
  	// 获取当前集群最大的提交索引。
  	ReadIndex(ctx context.Context, rctx []byte) error
 	// 获取 raft 状态。
  	Status() Status
  	// ReportUnreachable reports the given node is not reachable for the last send.
  	ReportUnreachable(id uint64)
 	// 向 raft 报告向某个 follower 发送 快照成功与否。只有快照发送成功，leader raft 才会向 follower 发送
  	// 后续 log entry，否则就会啥也不做。
  	ReportSnapshot(id uint64, status SnapshotStatus)
  	// Stop performs any necessary termination of the Node.
  	// 停止 raft 运转。
  	Stop()
  }
```

Node 接口的实际实现者 node 定义如下：

```go
  // raft/node.go
 
  // node is the canonical implementation of the Node interface
  type node struct {
  	// Propose()，应用层通过这个 chan 向 raft StateMachine 提交各种本地消息
  	propc      chan msgWithResult
  	// Step()，向 raft 提交各种非本地消息，如 leader 发来的日志消息或者 follower 给 leader 回复的 response
  	recvc      chan pb.Message
  	// ApplyConfChange()
  	confc      chan pb.ConfChangeV2
  	confstatec chan pb.ConfState
  	// Ready()，raft 给 node 输出 raft 的各种数据或者状态 
  	readyc     chan Ready
  	// Advance(), node 给 raft 反馈 ready 数据的处理结果，通知其处理完毕准备下一个 
  	advancec   chan struct{}
  	// Tick()，node 给 raft 通知时钟推进
  	tickc      chan struct{}
  	// Stop()
  	done       chan struct{}
  	// Stop()
  	stop       chan struct{}
  	// Status(), raft 向上层应用反馈 raft 的状态 
  	status     chan chan Status
  
  	rn *RawNode
  }
```

### 2.1 Run

raft StateMachine【下面简称 raft】自身仅仅提供了各个接口，node 通过一个 run goroutine 驱动 raft 运转起来。node 和 raft 之间还有一层 raftNode，它也有一个 run goroutine。

node 的 run goroutine 函数主体如下，负责监听其各个 chan 的输入给 raft，并处理 raft 的响应输出。

```go
  // raft/node.go
  func StartNode(c *Config, peers []Peer) Node {
  	rn, err := NewRawNode(c)
  	rn.Bootstrap(peers)
  	n := newNode(rn)
  	go n.run()
  	return &n
  }

  func (n *node) run() {
	r := n.rn.raft
	for {
		if advancec != nil {
			readyc = nil
		} else if n.rn.HasReady() {
			rd = n.rn.readyWithoutAccept()
			readyc = n.readyc
		}

		select {
        // 从 propc 获取本地消息，交给 raft 处理
		case pm := <-propc:
			err := r.Step(m)
		case m := <-n.recvc:
            // 从 raft peer 收到的消息，如果是 follower 发来的，必然是 response 消息；
            // 如果是 leader 发来的，则 progress 必然不是 nil
			if pr := r.prs.Progress[m.From]; pr != nil || !IsResponseMsg(m.Type) {
				r.Step(m)
			}
		case <-n.tickc:
			n.rn.Tick()
		case readyc <- rd:
            // 处理 ready 消息，并给 advancec 赋值 
			n.rn.acceptReady(rd)
			advancec = n.advancec
		case <-advancec:
            // advance 后，给 advancec 赋 nil 
			n.rn.Advance(rd)
			rd = Ready{}
			advancec = nil
		}
	}
  }
```


```go
  func (r *raftNode) start(rh *raftReadyHandler) {
	go func() {
		defer r.onStop()
		islead := false

		for {
			select {
			case <-r.ticker.C:
                // 监听 ticker 事件，并通知 raft Statemachine
				r.tick()
			case rd := <-r.Ready():
	
				r.Advance()
			case <-r.stopped:
				return
			}
		}
	}()
  }
```

rafftNode 模块的 cortoutine 核心就是处理 raft StateMachine 的 Ready，下面将用文字单独描述，这里仅考虑Leader 分支，Follower 分支省略：

1. 取出 Ready.SoftState 更新 EtcdServer 的当前节点身份信息（leader、follower....）等
2. 取出 Ready.ReadStates（保存了 commit index），通过 raftNode.readStateC 通道传递给 EtcdServer 处理 read 的 coroutine
3. 取出 Ready.CommittedEntires 封装成 apply 结构，通过 raftnode.applyc 通道传递给 EtcdServer 异步 Apply 的 coroutine，并更新 EtcdServer 的 commit index
4. 取出 Ready.Messages，通过网络模块 raftNode.transport 发送给 Peers
5. 取出 Ready.HardState 和 Entries，通过 raftNode.storage 持久化到 WAL 中
（Follower分支）取出 Ready.snapshot（Leader 发送过来的），（1）通过 raftNode.storage 持久化 Snapshot 到盘中的 Snapshot，（2）通知异步 Apply coroutine apply snapshot 到 KV 存储中，（3）Apply snapshot 到 raftNode.raftStorage 中（all raftLog in memory）
6. 取出 Ready.entries，append 到 raftLog 中
7. 调用 raftNode.Advance 通知 raft StateMachine coroutine，当前 Ready 已经处理完，可以投递下一个准备好的 Ready 给 raftNode cortouine 处理了（raft StateMachine 中会删除 raftLog 中 unstable 中 log entries 拷贝到 raftLog 的 Memory storage 中）

### 2.2 线程模型

![](./pic/raft_put.jpg)

其中红色虚线框起来的代表一个 coroutine，下面将对各个协程的作用基本的描述：

* Ticker：golang 的 Ticker struct 会定期触发 Tick 滴答时钟，etcd raft 的都是通过滴答时钟往前推进，从而触发相应的 heartbeat timeout 和 election timeout，从而触发发送心跳和选举。
* ReadLoop：这个 coroutine 主要负责处理 Read request，负责将 Read 请求通过 node 模块的 Propose 提交给 raft StateMachine，然后监听 raft StateMachine，一旦 raft StateMachine 完成 read 请求的处理，会通过 readStateC 通知 ReadLoop coroutine 此 read 的commit index，然后 ReadLoop coroutine 就可以根据当前 applied index 的推进情况，一旦 applied index >= commit index，ReadLoop coroutine 就会 Read 数据并通过网络返回 client read response
* raftNode：raftNode 模块会有一个 coroutine 负责处理 raft StateMachine 的输出 Ready，上文已经描述了，这里不在赘述
* node：node 模块也会有一个 coroutine 负责接收输入，运行状态机和准备输出，上文已经描述，这里不在赘述
* apply：raftNode 模块在 raft StateMachine 输出 Ready 中已经 committed entries 的时候，会将 apply 逻辑放在单独的 coroutine 处理，这就是 Async apply。
* GC：WAL 和 snapshot 的 GC 回收也都是分别在两个单独的 coroutine 中完成的。etcd 会在配置文中分别设置 WAL 和 snapshot 文件最大数量，然后两个 GC 后台异步 GC

etcd-raft 在性能上做了如下的优化：

* Batch：batch 的发送和持久化 Op log entries，raftNode 处理 Ready 和 node 模块处理 request 分别在两个单独的 coroutine 处理，这样 raftNode 在处理一个 Ready 的时候，node 模块的就会积累用户输入产生的输出，从而形成 batch。
* Pipeline： 一个完整的 raft 流程被拆，本身就是一种 pipeline
   + Leader 向 Follower 发送 Message 是 pipeline 发送的
   + Append Log Parallelly：Leader 发送 Op log entries message 给 Follower和 Leader 持久化 Op log entries 是并行的
* Asynchronous Apply：由单独的 coroutine 负责异步的 Apply
* Asynchronous Gc：WAL 和 snapshot 文件会分别开启单独的 coroutine 进行 GC

put 示例：

  * 1. client 通过 grpc 发送一个 Put kv request，etcd server 的 rpc server 收到这个请求，通过 node 模块的 Propose 接口提交，node 模块将这个 Put kv request 转换成 raft StateMachine 认识的 MsgProp Msg 并通过 propc Channel 传递给 node 模块的 coroutine；
  * 2. node 模块 coroutine 监听在 propc Channel 中，收到 MsgProp Msg 之后，通过 raft.Step(Msg) 接口将其提交给 raft StateMachine 处理；
  * 3. raft StateMachine 处理完这个 MsgProp Msg 会产生 1 个 Op log entry 和 2 个发送给另外两个副本的 Append entries 的 MsgApp messages，node 模块会将这两个输出打包成 Ready，然后通过 readyc Channel 传递给 raftNode 模块的 coroutine；
  * 4. raftNode 模块的 coroutine 通过 readyc 读取到 Ready，首先通过网络层将 2 个 append entries 的 messages 发送给两个副本(PS:这里是异步发送的)；
  * 5. raftNode 模块的 coroutine 自己将 Op log entry 通过持久化层的 WAL 接口同步的写入 WAL 文件中
  * 6. raftNode 模块的 coroutine 通过 advancec Channel 通知当前 Ready 已经处理完，请给我准备下一个 带出的 raft StateMachine 输出Ready；
  * 7. 其他副本的返回 Append entries 的 response： MsgAppResp message，会通过 node 模块的接口经过 recevc Channel 提交给 node 模块的 coroutine；
  * 8. node 模块 coroutine 从 recev Channel 读取到 MsgAppResp，然后提交给 raft StateMachine 处理。node 模块 coroutine 会驱动 raft StateMachine 得到关于这个 committedEntires，也就是一旦大多数副本返回了就可以 commit 了，node 模块 new 一个新的 Ready其包含了 committedEntries，通过 readyc Channel 传递给 raftNode 模块 coroutine 处理；
  * 9. raftNode 模块 coroutine 从 readyc Channel 中读取 Ready结构，然后取出已经 commit 的 committedEntries 通过 applyc 传递给另外一个 etcd server coroutine 处理，其会将每个 apply 任务提交给 FIFOScheduler 调度异步处理，这个调度器可以保证 apply 任务按照顺序被执行，因为 apply 的执行是不能乱的；
  * 10. raftNode 模块的 coroutine 通过 advancec Channel 通知当前 Ready 已经处理完，请给我准备下一个待处理的 raft StateMachine 输出Ready；
  * 11. FIFOScheduler 调度执行 apply 已经提交的 committedEntries
  * 12. AppliedIndex 推进，通知 ReadLoop coroutine，满足 applied index>= commit index 的 read request 可以返回；
  * 13. 调用网络层接口返回 client 成功。
  
 ## 3 Raft
 
features:

* Pre-vote：在发起 election vote 之前，先进行 pre-vote，可以避免在网络分区的情况避免反复的 election 打断当前 leader，触发新的选举造成可用性降低的问题
* ReadIndex：优化 raft read 走 Op log 性能问题，每次 read Op，仅记录 commit index，然后发送所有 peers heartbeat 确认 leader 身份，如果 leader 身份确认成功，等到 applied index >= commit index，就可以返回 client read 了
* Lease read：通过 lease 保证 leader 的身份，从而省去了 ReadIndex 每次 heartbeat 确认 leader 身份，性能更好，但是通过时钟维护 lease 本身并不是绝对的安全

etcd server:

![](./pic/etcd_server.jpg)

  * 网络层
    负责收发 etcd client、raft peer 的各种 messages。
  * 持久化层
    负责对 raft 各种数据的持久化存储。
    + WAL 持久化 raft op logs entries；
    + Snapshot 持久化 raft snapshot；
    + KV 应用层数据写入 kv 数据库中。

  * Raft 层
    etcd raft 剥离了存储层和网络层，专注于 StateMachine 实现。

### 3.1 Raft 关键类

> raft StateMachine

以状态机形式存在，提供了 raft 算法的核心逻辑算法封装。输入是 Message，输出是 Ready。

> node

第一职责是接受客户端的请求，封装为本地消息后调用 raft StateMachine 处理。
其次是提供 run goroutine，把 raft StateMachine 运转起来，并接收 raft 返回的 Ready。

> raftNode

启动一个单独的 goroutine，处理从 node 传来的 Ready 处理，譬如：
* 写 WAL
* 持久化 snapshot
* 向 follower 同步 log entries
* apply committied logs

> 总结

其实从整体架构图可见：

* node 处理 client 和 peer 请求，把请求作为输入交给 raft 处理后，raft 会返回初步加工的结果 Ready，然后交给 raftNode；
* raftNode 则处理输入的 Ready 消息，转发给 peers、WAL、Snapshot 和 KV;
* raft 则只管 log entry 处理和 raft 算法封装。
    
### 3.2 Raft 写请求处理流程

以 raft 收到一个流程为例，说明整个写请求的处理过程：

* 1：etcd server 收到请求后，生成一个 Propse Message 的本地消息发给 raft，raft 处理后返回 Ready 消息，包含了写到 WAL 的 op log 和 发给 follower 的 Append entries Msg。
     leader 先把 op log 写入 WAL，然后向 follower 发送 Append entries Msg。
* 2：follower 收到 leader 发来的 Append entries Msg 后，先写入 op log，给 leader 回复的 同时 发起自己的 raft StateMachine 日志处理流程。
     leader 收到 follower 返回的 Append entries Response Msg 后交给 raft StateMachine 处理。
     leader raft 如果收到了超过一半的 follower 的回复，则更新 match 和 committed index， 给 etcd server 返回 committed entries。
* 3: leader etcd server 将 raft 输出的 committed entries 进行 apply 后，给 client 返回 put success response。 

### 3.3 raft election

PreCandidate：etcd raft为了防止在网络分区的情况Candidate不断增加term id发起投票导致term id爆增。当网络恢复之后，分区节点将 term 信息传播给其他节点，导致这些节点变成 follower，触发重新选主，但是分区节点的日志不是最新的，不能成为leader，这样便会对整个集群造成扰动。

Raft 为了解决这种因网络分区 term id 爆增问题引入了 PreVote，在投票之前状态变成PreCandidate，发起预选举征求其它节点的同意。若要得到节点的同意需要同时满足下面两个条件：

* 1 没有收到有效的leader心跳，至少有一次选举超时；
* 2 Candidate 的日志足够新（Term更大，或者Term相同raft index更大）。

PreCandidate得到大多数节点同意之后将进入Candidate进行真正的投票，否则将以Follower的角色加入集群。

选举相关代码流程:

> follower 发现 leader 超时，变化为 candidate，发起选举

follower 的 tick 函数指向 tickElection，与 leader 之间心跳超时，则向自己发送本地消息 MsgHup，其 Step 函数看到后会进入竞选状态。

预选时本地 term 并不增长，分区恢复时，不会扰乱系统。

```go
  // raft/raft.go
  // tickElection is run by followers and candidates after r.electionTimeout.
  func (r *raft) tickElection() {
	r.electionElapsed++

	if r.promotable() && r.pastElectionTimeout() {
		r.electionElapsed = 0
		r.Step(pb.Message{From: r.id, Type: pb.MsgHup})
	}
  }

  func (r *raft) Step(m pb.Message) error {
    switch m.Type {
	  case pb.MsgHup:
		if r.state != StateLeader {
    		if r.preVote {
				r.campaign(campaignPreElection)
			} else {
				r.campaign(campaignElection)
			}
	  } 
	}
  }

  // campaign transitions the raft instance to candidate state. This must only be
  // called after verifying that this is a legitimate transition.
  func (r *raft) campaign(t CampaignType) {
	var term uint64
	var voteMsg pb.MessageType
	if t == campaignPreElection {
		r.becomePreCandidate()
		voteMsg = pb.MsgPreVote
        // 预选时发出去消息的 term 是当前 term + 1，本地的 term 并不增长
		term = r.Term + 1
	} else {
		r.becomeCandidate()
		voteMsg = pb.MsgVote
		term = r.Term
	}
	for _, id := range r.prs.Voters {
		r.send(pb.Message{Term: term, To: id, Type: voteMsg, Index: r.raftLog.lastIndex(), LogTerm: r.raftLog.lastTerm(), Context: ctx})
	}
  }
```

>  其他 follower 发起投票

```go
  func (r *raft) Step(m pb.Message) error {
    // 走到这里，说明 @m.term 与当前 raft term 一致
	switch m.Type {
	case pb.MsgVote, pb.MsgPreVote:
        // 已经投过票
		canVote := r.Vote == m.From ||
            // 没有投过票且当前无有效 leader
			(r.Vote == None && r.lead == None) ||
            // 预投票，且 term 足够大
			(m.Type == pb.MsgPreVote && m.Term > r.Term)
		// preCandidate/candidate 足够新：当前 follower 无有效投票记录，候选人的数据比本地更新或者一样新
		if canVote && r.raftLog.isUpToDate(m.Index, m.LogTerm) {	
            // 发出赞成投票，并记录下投票记录 
			r.send(pb.Message{To: m.From, Term: m.Term, Type: voteRespMsgType(m.Type)})
        	if m.Type == pb.MsgVote {
        		// Only record real votes.
        		r.electionElapsed = 0
        		r.Vote = m.From
        	} 
        }  else {
            // 发出反对投票
            r.send(pb.Message{To: m.From, Term: r.Term, Type: voteRespMsgType(m.Type), Reject: true})
        }
	}
  }
```

> candidate 统计投票结果

```go
  // stepCandidate is shared by StateCandidate and StatePreCandidate; the difference is
  // whether they respond to MsgVoteResp or MsgPreVoteResp.
  func stepCandidate(r *raft, m pb.Message) error {
	// Only handle vote responses corresponding to our candidacy (while in
	// StateCandidate, we may get stale MsgPreVoteResp messages in this term from
	// our pre-candidate state).
	var myVoteRespType pb.MessageType
	if r.state == StatePreCandidate {
		myVoteRespType = pb.MsgPreVoteResp
	} else {
		myVoteRespType = pb.MsgVoteResp
	}
	switch m.Type {
		case myVoteRespType:
    		gr, rj, res := r.poll(m.From, m.Type, !m.Reject)
    		r.logger.Infof("%x has received %d %s votes and %d vote rejections", r.id, gr, m.Type, rj)
    		switch res {
    		case quorum.VoteWon:
    			if r.state == StatePreCandidate {
    				r.campaign(campaignElection)
    			} else {
    				r.becomeLeader()
    				r.bcastAppend()
    			}
    		case quorum.VoteLost:
    			// pb.MsgPreVoteResp contains future term of pre-candidate
    			// m.Term > r.Term; reuse r.Term
    			r.becomeFollower(r.Term, None)
    	}
	}
  }
```

## 4 log

> unstable

```go
  // raft/log_unstable.go
  type unstable struct {
      // the incoming unstable snapshot, if any.
      snapshot *pb.Snapshot
      // all entries that have not yet been written to storage.
      entries []pb.Entry
      offset  uint64
      logger Logger
  }
```

unstable 存储还未被持久化的数据以及其起始 index @offset。

当一个新 follower 加入集群时，从 leader 获取到的 snapshot 会被存入 unstable.snapshot。其他情况下 unstable.snapshot 为 nil，这点也可以从下面这块代码验证之。

```go
// 代码源自go.etcd.io/etcd/raft/log.go
// raftLog的构造函数
func newLogWithSize(storage Storage, logger Logger, maxNextEntsSize uint64) *raftLog {
    if storage == nil {
        log.Panic("storage must not be nil")
    }
    // 创建raftLog对象
    log := &raftLog{
        storage:         storage,
        logger:          logger,
        maxNextEntsSize: maxNextEntsSize,
    }
    // 使用者启动需要把持久化的快照以及日志存储在storage中，前面已经提到了，这个
    // storage类似于使用者持久化存储的cache。
    firstIndex, err := storage.FirstIndex()
    if err != nil {
        panic(err) // TODO(bdarnell)
    }
    lastIndex, err := storage.LastIndex()
    if err != nil {
        panic(err) // TODO(bdarnell)
    }
    // 这个代码印证了前面提到了，当unstable没有不可靠日志的时候，unstable.offset的值就是
    // 未来的第一个不可靠日志的索引。
    log.unstable.offset = lastIndex + 1
    log.unstable.logger = logger
    // 初始化提交索引和应用索引,切记只是初始化，raft在构造完raftLog后还会设置这两个值，所以下面
    // 赋值感觉奇怪的可以忽略它。
    log.committed = firstIndex - 1
    log.applied = firstIndex - 1
 
    return log
}
```

![](./pic/log_unstable.png)

unstable.offset 等于 snapshot 最后 index + 1。unstable.entries 第 i 条日志的索引是 unstable.offset + i。

> Storage

raft 算法不负责数据的持久化，所以只提供了一个接口 Storage，以及一个内存版本的实现 MemoryStorage。

MemoryStorage 和 unstable 相似，有 snapshot 和 entries 两部分构成，存储稳定可靠的日志。

> raftLog

raftLog由以下成员组成：

* storage Storage：前面提到的存放已经持久化数据的Storage接口。
* unstable unstable：前面分析过的unstable结构体，用于保存应用层还没有持久化的数据。
* committed uint64：保存当前提交的日志数据索引。
* applied uint64：保存当前传入状态机的数据最高索引。

![](./pic/raft_log.png)   

raftLog 几个数据成员之间的关系如上图所示，用如下代码可以佐证。

```go
  func newRaft(c *Config) *raft {
	raftlog := newLogWithSize(c.Storage, c.Logger, c.MaxCommittedSizePerReady)
  }

  // newLogWithSize returns a log using the given storage and max
  // message size.
  func newLogWithSize(storage Storage, logger Logger, maxNextEntsSize uint64) *raftLog {
	log := &raftLog{
		storage:         storage,
		logger:          logger,
		maxNextEntsSize: maxNextEntsSize,
	}
	firstIndex, err := storage.FirstIndex()
	lastIndex, err := storage.LastIndex()
	log.unstable.offset = lastIndex + 1
	log.unstable.logger = logger
	// Initialize our committed and applied pointers to the time of the last compaction.
	log.committed = firstIndex - 1
	log.applied = firstIndex - 1

	return log
  }
```

从上面代码可见：

* 初始状态的 unstable 内确定了 offset 值，但 unstable.snapshot 为 nil。
* unstable 和 storage 之间间隔是 lastIndex

每一条日志 Entry 都会经过 unstable、stable、committed、applied、compacted 五个阶段，接下来总结一下日志的状态转换过程：

* 刚刚收到一条日志会被存储在unstable中，日志在没有被持久化之前如果遇到了换届选举，这个日志可能会被相同索引值的新日志覆盖，这个一点可以在raftLog.maybeAppend()和unstable.truncateAndAppend()找到相关的处理逻辑。
* unstable中存储的日志会被使用者写入持久存储（文件）中，这些持久化的日志就会从unstable转移到MemoryStorage中。读者可能会问MemoryStorage并不是持久存储啊，其实日志是被双写了，文件和MemoryStorage各存储了一份，而raft包只能访问MemoryStorage中的内容。这样设计的目的是用内存缓冲文件中的日志，在频繁操作日志的时候性能会更高。此处需要注意的是，MemoryStorage中的日志仅仅代表日志是可靠的，与提交和应用没有任何关系。
* leader 会搜集所有 peer 的接收日志状态，只要日志被超过半数以上的 peer 接收，那么就会提交该日志，peer 接收到 leader 的数据包更新自己的已提交的最大索引值，这样小于等于该索引值的日志就是可以被提交的日志。
* 已经被提交的日志会被使用者获得，并逐条应用，进而影响使用者的数据状态。
* 已经被应用的日志意味着使用者已经把状态持久化在自己的存储中了，这条日志就可以删除了，避免日志一直追加造成存储无限增大的问题。不要忘了所有的日志都存储在MemoryStorage中，不删除已应用的日志对于内存是一种浪费，这也就是日志的compacted。

## 5 Progress & Tracker

Leader 通过跟踪 follower 的数据同步进度进行数据发送和 committed 确认，以及选举。

### 5.1 Progress 

Progress 用于跟踪 follower 的 log entry 同步进度。

```go
  // 用于跟踪 follower 的 log entry 同步进度。
  // 探测状态通过 ProbeSent 控制探测消息的发送频率，复制状态下通过 Inflights 控制发送流量。
  // 如果 follower 的回复消息中的 index 大于 Match，说明 follower 开始接收日志了，它会进入复制状态。
  //
  // leader 向 follower 发送的 probe 消息，可能是心跳消息，也可能是日志复制消息，没有专门的探测消息。
  type Progress struct {
	// 为了提高数据同步效率，Leader 异步批量地把数据同步给 Follower。leader 已经同步给 follower 的
	// 数据的最大 index 为 Next - 1，follower 给 leader 确认已经收到的日志的最大索引是 Match。
	// [match+1, Next-1] 之间的日志就是处于 inflights 状态的日志：leader 已经发出，但 follower
	// 还未确认。
	Match, Next uint64
	// 记录 follower 的状态，决定了数据复制的方式
	// 如果是 StateProbe，则 leader 在一个心跳周期内最对复制一个 message 给 follower，
	// 一般地，一个新的 leader 刚选举出来时，其 follower 都处于 probe 状态，leader 先发出 probe 消息，
	// 第一用于让 follower 确认 leader 的权威，第二用于让 leader 确认 follower 还活着。
	//
	// 如果是 StateReplicate，则 leader 会批量地把 message 同步给 follower
	//
	// 如果是 StateSnapshot，则 leader 只发送 snapshot 给 follower，不会发送 message 给 follower
	State StateType
	// 如果 follower 处于 StateSnapshot 状态，则这个值用于记录 snapshot 的 index
	PendingSnapshot uint64
	// 用于标识 follower 处于 active 状态
	RecentActive bool
	// 如果 follower 处于 StateSnapshot 状态，则这个值用于标识 Probe 消息是否已经发送
	ProbeSent bool
	// 起到滑动窗口限流的作用。限定处于飞行状态的 log entry 的数目。
	Inflights *Inflights
	// IsLearner is true if this progress is tracked for a learner.
	IsLearner bool
  }
```

Progress.State

* ProgressStateProbe
  探测状态，当 follower 拒绝了最近的 append 消息时，那么就会进入探测状态，此时 leader 会试图继续往前追溯
该 follower 的日志从哪里开始丢失的。在 probe 状态时，leader 每次最多 append 一条日志，如果收到的回应中带
有 RejectHint 信息，则回退 Next 索引，以便下次重试。在初始时，leader 会把所有 follower 的状态设为 probe，
因为它并不知道各个 follower 的同步状态，所以需要慢慢试探。

* ProgressStateSnapshot
  接收快照状态。当 leader 向某个 follower 发送 append 消息，试图让该 follower 状态跟上 leader 时，发现
此时 leader上保存的索引数据已经对不上了，比如 leader 在 index 为 10 之前的数据都已经写入快照中了，但是该
follower 需要的是 10 之前的数据，此时就会切换到该状态下，发送快照给该 follower。当快照数据同步追上之后并
不是直接切换到 Replicate 状态，而是首先切换到 Probe 状态。

* ProgressStateReplicate
  当leader确认某个 follower 的同步状态后，它就会把这个 follower 的 state 切换到这个状态，并且用 pipeline
的方式快速复制日志。leader 在发送复制消息之后，就修改该节点的 Next 索引为发送消息的 最大索引 + 1。

Progress 本质是个状态机，状态转移图如下：

![](./pic/progress-state-machine.png)  

### 5.2 ProgressTracker

```go
type ProgressTracker struct {
	Config
    // 所有的 peer 的 Progress
	Progress ProgressMap

	Votes map[uint64]bool
	// Inflights 的容量
	MaxInflight int
}

// 根据各个 follower 返回的 match 结果计算集群的 committed 值。
// 这里只计算了 follower 作为 voter 的投票结果，不计算 learner[looker] 的结果。
//
// 另外，p.Progress 类型和 matchAckIndexer 类型一致，所以可以强行转换
func (p *ProgressTracker) Committed() uint64 {
	return uint64(p.Voters.CommittedIndex(matchAckIndexer(p.Progress)))
}
```

### 5.3 JointConfig

用于选举和计算 committed 值。

```go
// 其函数接口和 MajorityConfig 一致，其结果是对两组投票结果的比较，取值较小者。
// 之所以是两组投票结果，考虑的是添加或者删除的 membership change 的情况
type JointConfig [2]MajorityConfig

// CommittedIndex returns the largest committed index for the given joint
// quorum. An index is jointly committed if it is committed in both constituent
// majorities.
func (c JointConfig) CommittedIndex(l AckedIndexer) Index {
	idx0 := c[0].CommittedIndex(l)
	idx1 := c[1].CommittedIndex(l)
	if idx0 < idx1 {
		return idx0
	}
	return idx1
}

// VoteResult takes a mapping of voters to yes/no (true/false) votes and returns
// a result indicating whether the vote is pending, lost, or won. A joint quorum
// requires both majority quorums to vote in favor.
func (c JointConfig) VoteResult(votes map[uint64]bool) VoteResult {
	r1 := c[0].VoteResult(votes)
	r2 := c[1].VoteResult(votes)

	if r1 == r2 {
		// If they agree, return the agreed state.
		return r1
	}
	if r1 == VoteLost || r2 == VoteLost {
		// If either config has lost, loss is the only possible outcome.
		return VoteLost
	}
	// One side won, the other one is pending, so the whole outcome is.
	return VotePending
}
```
