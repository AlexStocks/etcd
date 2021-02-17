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
  * Term：这个消息发出时整个集群所处的任期。
  * LogTerm：消息发出者所保存的日志中最后一条的任期号，一般MsgVote会用到这个字段。
  * Index：日志索引号。如果当前消息是MsgVote的话，代表这个candidate最后一条日志的索引号，它跟上面的LogTerm一起代表这个candidate所拥有的最新日志信息，这样别人就可以比较自己的日志是不是比candidata的日志要新，从而决定是否投票。
  * Entries：需要存储的日志。
  * Commit：已经提交的日志的索引值，用来向别人同步日志的提交信息。
  * Snapshot：一般跟MsgSnap合用，用来放置具体的Snapshot值。
  * Reject，RejectHint：代表对方节点拒绝了当前节点的请求(MsgVote/MsgApp/MsgSnap…)

### 1.2 Message Type 

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

### 1.3 心跳消息 

```Go
  // raft/raft.go
  // follower 收到的心跳消息中的 commit 值不能大于 follower 上报的 match 值
  func (r *raft) sendHeartbeat(to uint64, ctx []byte) 
```

preVote 发起预选，此时并不增加 term，主要用于检测网络是否恢复、其数据是否是最新的。