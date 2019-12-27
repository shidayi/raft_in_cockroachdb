// Copyright 2015 The etcd Authors
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

package raft

import "fmt"

// Leader通过Progress这个数据结构来追踪一个follower的状态，并根据Progress里的信息来决定每次同步的日志项。

// State属性用来保存该节点当前的同步状态
const (
	// iota，在每一个const关键字出现的时候被重置为0，然后在下一个const出现之前，每出现一次iota，其所代表的数字会自动增1

	// 探测状态
	// 当follower拒绝了最近的append消息时，那么就会进入探测状态，此时leader会试图继续往前追溯该follower的日志
	// 从哪里开始丢失的，让该节点的日志能跟leader同步上。
	// 在probe状态时，leader每次最多append一条日志，如果收到的回应中带有RejectHint信息，
	// 则回退Next索引，以便下次重试。在初始时，leader会把所有follower的状态设为probe，因为它并不知道各个follower
	// 的同步状态，所以需要慢慢试探。

	// 在probe状态时，leader只能向它发送一次append消息，此后除非状态发生变化，否则就暂停向该节点发送新的append消息了。
	// 只有在以下情况才会恢复取消暂停状态（调用Progress的resume函数）：
	// 1) 收到该节点的心跳消息。
	// 2) 该节点成功应答了前面的最后一条append消息。
	// 至于Probe状态，只有在该节点成功应答了Append消息之后，在leader上保存的索引值发生了变化，才会修改其状态切换到Replicate状态。
	ProgressStateProbe ProgressStateType = iota   // 0

	// 正常接收副本日志的状态
	// 当leader确认某个follower的同步状态后，它就会把这个follower的state切换到这个状态，并且用pipeline的
	// 方式快速复制日志。leader在发送复制消息之后，就修改该节点的Next索引为发送消息的最大索引+1。
	ProgressStateReplicate

	// 接收快照状态
	// 当leader向某个follower发送append消息，试图让该follower状态跟上leader时，发现此时leader
	// 上保存的索引数据已经对不上了，比如leader在index为10之前的数据都已经写入快照中了，但是该follower需要的
	// 是10之前的数据，此时就会切换到该状态下，发送快照给该follower。当快照数据同步追上之后，并不是直接切换到
	// Replicate状态，而是首先切换到Probe状态。

	// 因为快照数据可能很多，不知道会同步多久，所以单独把这个状态抽象出来。
	ProgressStateSnapshot
)

type ProgressStateType uint64

// 如果省略号"..."出现在数组长度的位置，那么数组的长度由初始化数组的元素个数决定。
var prstmap = [...]string{
	"ProgressStateProbe",
	"ProgressStateReplicate",
	"ProgressStateSnapshot",
}

// 将变量转成对应的string
func (st ProgressStateType) String() string { return prstmap[uint64(st)] }

// Progress represents a follower’s progress in the view of the leader. Leader maintains
// progresses of all followers, and sends entries to the follower based on its progress.
// Progress表示在领导人视角的一个跟随者的进程, 领导人维护所有跟随者的Progresses,根据他们的Progress
// 来发送日志条目

// Progress结构体中有两个保存该follower节点日志索引的数据
type Progress struct {

	// Match: 保存目前为止，已复制给该follower的日志的最高索引值。
	// 如果leader对该follower上的日志情况一无所知的话，这个值被设为0。

	// Next: 保存下一次leader发送append消息给该follower的日志索引，即下一次复制日志时，leader会从Next开始发送日志。

	// 在正常情况下，Next = Match + 1，也就是下一个要同步的日志应当是对方已有日志的下一条。
	// 有两种情况除外：
	// 1) 接收快照状态：此时Next = max(pr.Match+1, pendingSnapshot+1)
	// 2) 当该follower不在Replicate状态时，说明不是正常的接收副本状态。此时当leader与follower同步leader上的日志时，
	//    可能出现覆盖的情况，即此时follower上面假设Match为3，但是索引为3的数据会被leader覆盖，此时Next指针可能会
	//    一直回溯到与leader上日志匹配的位置，再开始正常同步日志，此时也会出现Next != Match + 1的情况出现。
	Match, Next uint64
	// State defines how the leader should interact with the follower.
	//
	// When in ProgressStateProbe, leader sends at most one replication message
	// per heartbeat interval. It also probes actual progress of the follower.
	// 当为ProgressStateProbe时，leader每心跳间隔发送最多一次replication message
	// 用来探测follower的实际进程

	// When in ProgressStateReplicate, leader optimistically increases next
	// to the latest entry sent after sending replication message. This is
	// an optimized state for fast replicating log entries to the follower.
	// leader在发送复制消息之后，就修改该节点的Next索引为发送消息的最大索引+1。

	// When in ProgressStateSnapshot, leader should have sent out snapshot
	// before and stops sending any replication message.
	// leader在发送复制信息之前发送快照

	// State属性用来保存该节点当前的同步状态
	State ProgressStateType

	// Paused is used in ProgressStateProbe.
	// When Paused is true, raft should pause sending replication message to this peer.

	// 在ProgressStateProbe中使用。
	// 当值为true的时候，raft应该暂停发送复制信息到follower
	Paused bool


	// PendingSnapshot is used in ProgressStateSnapshot.
	// If there is a pending snapshot, the pendingSnapshot will be set to the
	// index of the snapshot. If pendingSnapshot is set, the replication process of
	// this Progress will be paused. raft will not resend snapshot until the pending one
	// is reported to be failed.

	// 在ProgressStateSnapshot中使用
	// 如果存在悬挂的快照，pendingSnapshot设置为快照的索引。如果pendingSnapshot被设置，Progress
	// 的复制进程被暂停。在挂起的快照被告知失败之前，raft不会重新发送快照。
	PendingSnapshot uint64

	// RecentActive is true if the progress is recently active. Receiving any messages
	// from the corresponding follower indicates the progress is active.
	// RecentActive can be reset to false after an election timeout.

	// 如果progress最近激活,RecentActive为True, 从跟随者中接收到任何消息都表示进程为激活状态
	// 在选举超时后, RecentActive被设置为false

	// 表示该进程是否活着
	RecentActive bool

	// inflights is a sliding window for the inflight messages.
	// Each inflight message contains one or more log entries.
	// The max number of entries per message is defined in raft config as MaxSizePerMsg.
	// Thus inflight effectively limits both the number of inflight messages
	// and the bandwidth each Progress can use.
	// When inflights is full, no more message should be sent.
	// When a leader sends out a message, the index of the last
	// entry should be added to inflights. The index MUST be added
	// into inflights in order.
	// When a leader receives a reply, the previous inflights should
	// be freed by calling inflights.freeTo with the index of the last
	// received entry.
	// inflights 是一个inflight 信息的滑动窗口
	// 每一个inflight信息包含了一个或多个日志。
	// 每个inflight信息中的最大日志数在raft config中的MaxSizePerMsg定义

	// ins属性用来做流量控制，因为如果同步请求非常多，再碰上网络分区时，leader可能会累积很多待发送消息，
	// 一旦网络恢复，可能会有非常大流量发送给follower，所以这里要做flow control。它的实现有点类似TCP的滑动窗口，这里不再赘述。

	// 流量控制
	// 该结构体使用一个固定大小的循环缓冲区来控制给一个节点同步数据的流量控制，
	// 每当给该follower发送同步消息时，就占用该缓冲区的一个空间；反之，当收到该follower的成功接收了该同步消息的应答之后，
	// 就释放缓冲区的空间。
	// 当该缓冲区数据饱和时，将暂停继续同步数据到该follower。

	// 在follower端
	ins *inflights

	// IsLearner is true if this progress is tracked for a learner.
	// 该progress是否是learner
	IsLearner bool
}

func (pr *Progress) resetState(state ProgressStateType) {
	pr.Paused = false
	pr.PendingSnapshot = 0
	pr.State = state
	pr.ins.reset()
}

// 变成探测状态
func (pr *Progress) becomeProbe() {
	// If the original state is ProgressStateSnapshot, progress knows that
	// the pending snapshot has been sent to this peer successfully, then
	// probes from pendingSnapshot + 1.

	// 如果初始状态是ProgressStateSnapshot， progress知道了悬挂的快照成功的发送给follower
	// 然后从pendingSnapshot + 1开始探测
	if pr.State == ProgressStateSnapshot {
		pendingSnapshot := pr.PendingSnapshot
		pr.resetState(ProgressStateProbe)
		pr.Next = max(pr.Match+1, pendingSnapshot+1)
	} else {
		pr.resetState(ProgressStateProbe)
		pr.Next = pr.Match + 1
	}
}

// 变成正常接收副本日志的状态
func (pr *Progress) becomeReplicate() {
	pr.resetState(ProgressStateReplicate)
	pr.Next = pr.Match + 1
}

// 变成接受快照的状态
func (pr *Progress) becomeSnapshot(snapshoti uint64) {
	pr.resetState(ProgressStateSnapshot)
	// 如果存在悬挂的快照，pendingSnapshot设置为快照的索引
	pr.PendingSnapshot = snapshoti
}

// maybeUpdate returns false if the given n index comes from an outdated message.
// Otherwise it updates the progress and returns true.

// 如果给定的n索引来自一个过时的信息，返回false
// 否则更新progress的Match和Next，返回true
func (pr *Progress) maybeUpdate(n uint64) bool {
	var updated bool
	if pr.Match < n {
		pr.Match = n
		updated = true
		pr.resume()
	}
	if pr.Next < n+1 {
		pr.Next = n + 1
	}
	return updated
}

// 乐观更新
func (pr *Progress) optimisticUpdate(n uint64) { pr.Next = n + 1 }

// maybeDecrTo returns false if the given to index comes from an out of order message.
// Otherwise it decreases the progress next index to min(rejected, last) and returns true.

// 如果给定的索引来自一个出故障了的信息，返回false
// 否则减少progress  next索引为 min(rejected, last)， 返回true
func (pr *Progress) maybeDecrTo(rejected, last uint64) bool {
	if pr.State == ProgressStateReplicate {
		// the rejection must be stale if the progress has matched and "rejected"
		// is smaller than "match".
		// 如果progress已匹配并且“rejected”小于“match”，则拒绝必须是过时的。
		if rejected <= pr.Match {
			return false
		}
		// directly decrease next to match + 1
		pr.Next = pr.Match + 1
		return true
	}

	// the rejection must be stale if "rejected" does not match next - 1
	if pr.Next-1 != rejected {
		return false
	}

	if pr.Next = min(rejected, last+1); pr.Next < 1 {
		pr.Next = 1
	}
	pr.resume()
	return true
}

// 在ProgressStateProbe中使用。
// 当值为true的时候，raft应该暂停发送复制信息到follower
func (pr *Progress) pause()  { pr.Paused = true }

// 重新开始
func (pr *Progress) resume() { pr.Paused = false }

// IsPaused returns whether sending log entries to this node has been
// paused. A node may be paused because it has rejected recent
// MsgApps, is currently waiting for a snapshot, or has reached the
// MaxInflightMsgs limit.

// IsPaused 返回是否发送log 日志给已经暂停的node节点
// node节点可能已经暂停因为它拒绝了最近的MsgApps，它一直等快照，或者已经到了MaxInflightMsgs的限制
func (pr *Progress) IsPaused() bool {
	switch pr.State {
	case ProgressStateProbe:
		return pr.Paused
	case ProgressStateReplicate:
		return pr.ins.full()
	case ProgressStateSnapshot:
		return true
	default:
		panic("unexpected state")
	}
}

func (pr *Progress) snapshotFailure() { pr.PendingSnapshot = 0 }

// needSnapshotAbort returns true if snapshot progress's Match
// is equal or higher than the pendingSnapshot.

// 如果snapshot progress's Match >= pendingSnapshot, 返回true
func (pr *Progress) needSnapshotAbort() bool {
	return pr.State == ProgressStateSnapshot && pr.Match >= pr.PendingSnapshot
}

func (pr *Progress) String() string {
	return fmt.Sprintf("next = %d, match = %d, state = %s, waiting = %v, pendingSnapshot = %d", pr.Next, pr.Match, pr.State, pr.IsPaused(), pr.PendingSnapshot)
}

type inflights struct {
	// the starting index in the buffer
	// buffer数组中开始存放的位置下标
	start int

	// number of inflights in the buffer
	// buffer中 inflight的数量
	count int

	// the size of the buffer
	// buffer的能存放元素的个数，类似于capacity
	size int

	// buffer contains the index of the last entry
	// inside one message.
	// 每条信息的最后日志的索引
	buffer []uint64
}

func newInflights(size int) *inflights {
	return &inflights{
		size: size,
	}
}

// add adds an inflight into inflights
// 增加一个inflight
func (in *inflights) add(inflight uint64) {
	// 当该缓冲区数据饱和时，将暂停继续同步数据到该follower。
	if in.full() {
		panic("cannot add into a full inflights")
	}

	// 增加的元素在inflights数组中的存放的下标位置，因为是一个环
	next := in.start + in.count

	size := in.size
	if next >= size {
		next -= size
	}
	if next >= len(in.buffer) {
		in.growBuf()
	}
	in.buffer[next] = inflight
	in.count++
}

// grow the inflight buffer by doubling up to inflights.size. We grow on demand
// instead of preallocating to inflights.size to handle systems which have
// thousands of Raft groups per process.

// 通过将大小增加一倍来增加inflights.size。 我们按需增长，而不是预先分配给inflights.size来处理每个进程具有数千个Raft组的系统。
// 每次增长一倍的in.buffer的大小
// in.buffer的大小最大为size的大小
func (in *inflights) growBuf() {
	newSize := len(in.buffer) * 2
	if newSize == 0 {
		newSize = 1
	} else if newSize > in.size {
		newSize = in.size
	}
	newBuffer := make([]uint64, newSize)
	// copy函数复制数组切片的内容
	copy(newBuffer, in.buffer)
	in.buffer = newBuffer
}

// freeTo frees the inflights smaller or equal to the given `to` flight.
// 释放索引值小于等于to的元素
func (in *inflights) freeTo(to uint64) {
	if in.count == 0 || to < in.buffer[in.start] {
		// out of the left side of the window
		return
	}


    // 从start开始，找到第一个索引值大于to的值，遍历过的i的值，就是要删掉的元素个数
	idx := in.start
	var i int
	for i = 0; i < in.count; i++ {
		// 从start开始，找到第一个索引值大于to的值
		if to < in.buffer[idx] { // found the first large inflight
			break
		}

		// increase index and maybe rotate
		size := in.size
		if idx++; idx >= size {
			idx -= size
		}
	}

	// free i inflights and set new start index
	// 释放i个元素，重置start
	in.count -= i
	in.start = idx
	if in.count == 0 {
		// inflights is empty, reset the start index so that we don't grow the
		// buffer unnecessarily.
		in.start = 0
	}
}

func (in *inflights) freeFirstOne() { in.freeTo(in.buffer[in.start]) }

// full returns true if the inflights is full.
func (in *inflights) full() bool {
	return in.count == in.size
}

// resets frees all inflights.
func (in *inflights) reset() {
	in.count = 0
	in.start = 0
}
