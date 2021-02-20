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
      Raft 协议相关的，包括心跳 MsgHeartbeat、日志 MsgApp、投票消息 MsgVote 等。
      上层应用触发的消息，比如应用对数据更改的消息 MsgProp。
  * To, From分别代表了这个消息的接受者和发送者。
  * Term：这个消息发出时整个集群所处的任期
    + 如果是应用层想本地 raft 发送的消息，其 term 为 0；如果是网络其他 raft node 转发来的 message，其 term 不为 0。
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