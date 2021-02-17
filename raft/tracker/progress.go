// Copyright 2019 The etcd Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package tracker

import (
	"fmt"
	"sort"
	"strings"
)

// Progress represents a follower’s progress in the view of the leader. Leader
// maintains progresses of all followers, and sends entries to the follower
// based on its progress.
//
// NB(tbg): Progress is basically a state machine whose transitions are mostly
// strewn around `*raft.raft`. Additionally, some fields are only used when in a
// certain State. All of this isn't ideal.
//
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
	// State defines how the leader should interact with the follower.
	//
	// When in StateProbe, leader sends at most one replication message
	// per heartbeat interval. It also probes actual progress of the follower.
	//
	// When in StateReplicate, leader optimistically increases next
	// to the latest entry sent after sending replication message. This is
	// an optimized state for fast replicating log entries to the follower.
	//
	// When in StateSnapshot, leader should have sent out snapshot
	// before and stops sending any replication message.
	//
	// 记录 follower 的状态，决定了数据复制的方式
	// 如果是 StateProbe，则 leader 在一个心跳周期内最对复制一个 message 给 follower，
	// 一般地，一个新的 leader 刚选举出来时，其 follower 都处于 probe 状态，leader 先发出 probe 消息，
	// 第一用于让 follower 确认 leader 的权威，第二用于让 leader 确认 follower 还活着。
	//
	// 如果是 StateReplicate，则 leader 会批量地把 message 同步给 follower
	//
	// 如果是 StateSnapshot，则 leader 只发送 snapshot 给 follower，不会发送 message 给 follower
	State StateType

	// PendingSnapshot is used in StateSnapshot.
	// If there is a pending snapshot, the pendingSnapshot will be set to the
	// index of the snapshot. If pendingSnapshot is set, the replication process of
	// this Progress will be paused. raft will not resend snapshot until the pending one
	// is reported to be failed.
	//
	// 如果 follower 处于 StateSnapshot 状态，则这个值用于记录 snapshot 的 index
	PendingSnapshot uint64

	// RecentActive is true if the progress is recently active. Receiving any messages
	// from the corresponding follower indicates the progress is active.
	// RecentActive can be reset to false after an election timeout.
	//
	// TODO(tbg): the leader should always have this set to true.
	//
	// 用于标识 follower 处于 active 状态
	RecentActive bool

	// ProbeSent is used while this follower is in StateProbe. When ProbeSent is
	// true, raft should pause sending replication message to this peer until
	// ProbeSent is reset. See ProbeAcked() and IsPaused().
	//
	// 如果 follower 处于 StateSnapshot 状态，则这个值用于标识 Probe 消息是否已经发送
	ProbeSent bool

	// Inflights is a sliding window for the inflight messages.
	// Each inflight message contains one or more log entries.
	// The max number of entries per message is defined in raft config as MaxSizePerMsg.
	// Thus inflight effectively limits both the number of inflight messages
	// and the bandwidth each Progress can use.
	// When inflights is Full, no more message should be sent.
	// When a leader sends out a message, the index of the last
	// entry should be added to inflights. The index MUST be added
	// into inflights in order.
	// When a leader receives a reply, the previous inflights should
	// be freed by calling inflights.FreeLE with the index of the last
	// received entry.
	//
	// 起到滑动窗口限流的作用。限定处于飞行状态的 log entry 的数目。
	Inflights *Inflights

	// IsLearner is true if this progress is tracked for a learner.
	IsLearner bool
}

// ResetState moves the Progress into the specified State, resetting ProbeSent,
// PendingSnapshot, and Inflights.
func (pr *Progress) ResetState(state StateType) {
	pr.ProbeSent = false
	pr.PendingSnapshot = 0
	pr.State = state
	pr.Inflights.reset()
}

func max(a, b uint64) uint64 {
	if a > b {
		return a
	}
	return b
}

func min(a, b uint64) uint64 {
	if a > b {
		return b
	}
	return a
}

// ProbeAcked is called when this peer has accepted an append. It resets
// ProbeSent to signal that additional append messages should be sent without
// further delay.
//
// 当 leader 收到 follower 对 probe 消息的回复时，这个函数会被调用。
func (pr *Progress) ProbeAcked() {
	pr.ProbeSent = false
}

// BecomeProbe transitions into StateProbe. Next is reset to Match+1 or,
// optionally and if larger, the index of the pending snapshot.
func (pr *Progress) BecomeProbe() {
	// If the original state is StateSnapshot, progress knows that
	// the pending snapshot has been sent to this peer successfully, then
	// probes from pendingSnapshot + 1.
	if pr.State == StateSnapshot {
		// ResetState 会把 pr.PendingSnapshot 置为空，所以此处提前 copy
		pendingSnapshot := pr.PendingSnapshot
		pr.ResetState(StateProbe)
		pr.Next = max(pr.Match+1, pendingSnapshot+1)
	} else {
		pr.ResetState(StateProbe)
		pr.Next = pr.Match + 1
	}
}

// BecomeReplicate transitions into StateReplicate, resetting Next to Match+1.
// leader 收到 follower 对探测消息的回复，则把状态置位复制状态。
func (pr *Progress) BecomeReplicate() {
	pr.ResetState(StateReplicate)
	pr.Next = pr.Match + 1
}

// BecomeSnapshot moves the Progress to StateSnapshot with the specified pending
// snapshot index.
func (pr *Progress) BecomeSnapshot(snapshoti uint64) {
	pr.ResetState(StateSnapshot)
	pr.PendingSnapshot = snapshoti
}

// MaybeUpdate is called when an MsgAppResp arrives from the follower, with the
// index acked by it. The method returns false if the given n index comes from
// an outdated message. Otherwise it updates the progress and returns true.
//
// 更新 Progress 的状态，为什么有个 maybe 呢？因为不确定是否更新，raft 代码中有很多maybeXxx系列函数，
// 大多是尝试性操作，毕竟是分布式系统，消息重发、网络分区可能会让某些操作失败。这个函数是在节点回复
// 追加日志消息时被调用的，在反馈消息中节点告知 Leader 日志消息中有一条日志的索引已经被接收，就是
// 下面的参数 n，Leader 尝试更新节点的 Progress 的状态。
//
// 当收到 follower 发来的 MsgAppResp 消息时，这个函数会被调用
func (pr *Progress) MaybeUpdate(n uint64) bool {
	var updated bool
	if pr.Match < n {
		pr.Match = n
		updated = true
		pr.ProbeAcked() // 日志消息也是一种探测消息
	}
	if pr.Next < n+1 {
		pr.Next = n + 1
	}
	return updated
}

// OptimisticUpdate signals that appends all the way up to and including index n
// are in-flight. As a result, Next is increased to n+1.
func (pr *Progress) OptimisticUpdate(n uint64) { pr.Next = n + 1 }

// MaybeDecrTo adjusts the Progress to the receipt of a MsgApp rejection. The
// arguments are the index the follower rejected to append to its log, and its
// last index.
//
// Rejections can happen spuriously as messages are sent out of order or
// duplicated. In such cases, the rejection pertains to an index that the
// Progress already knows were previously acknowledged, and false is returned
// without changing the Progress.
//
// If the rejection is genuine, Next is lowered sensibly, and the Progress is
// cleared for sending log entries.
//
// 当 leader 收到 follower 的拒绝消息时，调用这个函数。
// @rejected follower 拒绝 leader 的日志的 index。
// @last follower 最后一次确认的 leader 的日志的 index。
func (pr *Progress) MaybeDecrTo(rejected, last uint64) bool {
	if pr.State == StateReplicate {
		// The rejection must be stale if the progress has matched and "rejected"
		// is smaller than "match".
		if rejected <= pr.Match { // 发生了日志重复发送，follower 拒绝掉对 leader 的重复日志的接受。leader 忽略即可。
			return false
		}
		// Directly decrease next to match + 1.
		//
		// TODO(tbg): why not use last if it's larger?
		// 发生了日志消息丢失现象，为了安全起见，next 直接被设置为 match + 1
		pr.Next = pr.Match + 1
		return true
	}

	// The rejection must be stale if "rejected" does not match next - 1. This
	// is because non-replicating followers are probed one entry at a time.
	//
	// 非复制状态下，如果 rejected 不等于 next - 1，说明 follower 回复的是陈旧消息，不用暂停。
	// 在 probe 状态下，leader 给 follower 一次最多只发送一次 probe message，所以正常情况下应该相等。
	if pr.Next-1 != rejected {
		return false
	}

	// 根据 follower 的反馈，调整 next
	if pr.Next = min(rejected, last+1); pr.Next < 1 {
		pr.Next = 1
	}
	// 当前状态不是复制状态，且 follower 回复了拒绝，需要再发送一次探测消息，置 ProbeSent 为 true。
	pr.ProbeSent = false
	return true
}

// IsPaused returns whether sending log entries to this node has been throttled.
// This is done when a node has rejected recent MsgApps, is currently waiting
// for a snapshot, or has reached the MaxInflightMsgs limit. In normal
// operation, this is false. A throttled node will be contacted less frequently
// until it has reached a state in which it's able to accept a steady stream of
// log entries again.
//
// 是否限流起作用导致 follower 处于暂停状态。
func (pr *Progress) IsPaused() bool {
	switch pr.State {
	case StateProbe:
		// 探测状态下，发送一条即可，再多无用。
		return pr.ProbeSent
	case StateReplicate:
		// 复制状态下是否发送了太多的消息。
		return pr.Inflights.Full()
	case StateSnapshot:
		return true
	default:
		panic("unexpected state")
	}
}

func (pr *Progress) String() string {
	var buf strings.Builder
	fmt.Fprintf(&buf, "%s match=%d next=%d", pr.State, pr.Match, pr.Next)
	if pr.IsLearner {
		fmt.Fprint(&buf, " learner")
	}
	if pr.IsPaused() {
		fmt.Fprint(&buf, " paused")
	}
	if pr.PendingSnapshot > 0 {
		fmt.Fprintf(&buf, " pendingSnap=%d", pr.PendingSnapshot)
	}
	if !pr.RecentActive {
		fmt.Fprintf(&buf, " inactive")
	}
	if n := pr.Inflights.Count(); n > 0 {
		fmt.Fprintf(&buf, " inflight=%d", n)
		if pr.Inflights.Full() {
			fmt.Fprint(&buf, "[full]")
		}
	}
	return buf.String()
}

// ProgressMap is a map of *Progress.
type ProgressMap map[uint64]*Progress

// String prints the ProgressMap in sorted key order, one Progress per line.
func (m ProgressMap) String() string {
	ids := make([]uint64, 0, len(m))
	for k := range m {
		ids = append(ids, k)
	}
	sort.Slice(ids, func(i, j int) bool {
		return ids[i] < ids[j]
	})
	var buf strings.Builder
	for _, id := range ids {
		fmt.Fprintf(&buf, "%d: %s\n", id, m[id])
	}
	return buf.String()
}
