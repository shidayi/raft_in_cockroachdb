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

import (
	"context"
	"errors"

	pb "go.etcd.io/etcd/raft/raftpb"
)

// 提供出去的是Node接口及其实现node结构体，这是外界与raft库打交道的唯一接口，除此之外该路径下的其他文件并不直接与外界打交道
/*
Raft库却相对而言更复杂一些，因为还有以下的问题存在：
1) 写入的数据，可能是集群状态变更的数据，Raft库在执行写入这类数据之后，需要返回新的状态给应用层。
2) Raft库中的数据不可能一直以日志的形式存在，这样会导致数据越来越大，所以有可能被压缩成快照（snapshot）的数据形式，
   这种情况下也需要返回这部分快照数据。
3) 由于etcd的Raft库不包括持久化数据存储相关的模块，而是由应用层自己来做实现，所以也需要返回在某次写入成功之后，
   哪些数据可以进行持久化保存了。
4) 同样的，etcd的Raft库也不自己实现网络传输，所以同样需要返回哪些数据需要进行网络传输给集群中的其他节点。
以上的这些，集中在raft/node.go的Ready结构体中
 */
type SnapshotStatus int

const (
	SnapshotFinish  SnapshotStatus = 1
	SnapshotFailure SnapshotStatus = 2
)

var (
	emptyState = pb.HardState{}

	// ErrStopped is returned by methods on Nodes that have been stopped.
	ErrStopped = errors.New("raft: stopped")
)

// SoftState provides state that is useful for logging and debugging.
// The state is volatile and does not need to be persisted to the WAL.

// SoftState在记录日志和调试时展示状态用的,不用持久化到wal
type SoftState struct {
	Lead      uint64     // must use atomic operations to access; keep 64-bit aligned.
	RaftState StateType
}

func (a *SoftState) equal(b *SoftState) bool {
	return a.Lead == b.Lead && a.RaftState == b.RaftState
}

// Ready encapsulates the entries and messages that are ready to read,
// be saved to stable storage, committed or sent to other peers.
// All fields in Ready are read-only.
// Ready封装了已经可以去读, 可存到稳定存储,提交,发送到其他节点的日志和信息
// 所有字段都是只读的

//  在etcd的这个实现中，node并不负责数据的持久化、网络消息的通信、以及将已经提交的log应用到状态机中，
//	所以node使用readyc这个channel对外通知有数据要处理了，并将这些需要外部处理的数据打包到一个Ready结构体中

/*
将HardState, Entries, Snapshot持久化到storage。
将Messages广播给其他节点。
将CommittedEntries（已经commit还没有apply）应用到状态机。
如果发现CommittedEntries中有成员变更类型的entry，调用node.ApplyConfChange()方法让node知道。
最后再调用node.Advance()告诉raft，这批状态更新处理完了，状态已经演进了，可以给我下一批Ready让我处理。

成员名称 	类型	             作用
SoftState	SoftState	     软状态，软状态易变且不需要保存在WAL日志中的状态数据，包括：集群leader、节点的当前状态
HardState	HardState	     硬状态，与软状态相反，需要写入持久化存储中，包括：节点当前Term、Vote、Commit
ReadStates	[]ReadStates	 用于读一致性的数据，后续会详细介绍
Entries	    []pb.Entry	     在向其他集群发送消息之前需要先写入持久化存储的日志数据
Snapshot	pb.Snapshot	     需要写入持久化存储中的快照数据
CommittedEntries []pb.Entry	 需要输入到状态机中的数据，这些数据之前已经被保存到持久化存储中了
Messages	[]pb.Message	 在entries被写入持久化存储中以后，需要发送出去的数据


根据上面的分析，应用层在写入一段数据之后，Raft库将返回这样一个Ready结构体，其中可能某些字段是空的，毕竟不是每次改动都会
导致Ready结构体中的成员都发生变化，此时使用者就需要根据情况，取出其中不为空的成员进行操作了。
*/
type Ready struct {
	// The current volatile state of a Node.
	// SoftState will be nil if there is no update.
	// It is not required to consume or store SoftState.

	// 一个节点的当前可变的状态
	*SoftState

	// The current state of a Node to be saved to stable storage BEFORE
	// Messages are sent.
	// HardState will be equal to empty state if there is no update.

	// 在发送消息之前, 当前的状态状态应该稳定存储
	// 如果没有更新, HardState为空状态
	pb.HardState

	// ReadStates can be used for node to serve linearizable read requests locally
	// when its applied index is greater than the index in ReadState.
	// Note that the readState will be returned when raft receives msgReadIndex.
	// The returned is only valid for the request that requested to read.

	// 注意当raft接收到msgReadIndex时readState将会被返回
	// 仅对请求读有效
	ReadStates []ReadState

	// Entries specifies entries to be saved to stable storage BEFORE
	// Messages are sent.

	// 在消息发送之前需要稳定存储的日志条目
	Entries []pb.Entry

	// Snapshot specifies the snapshot to be saved to stable storage.
	// 稳定存储的snapshot
	Snapshot pb.Snapshot

	// CommittedEntries specifies entries to be committed to a
	// store/state-machine. These have previously been committed to stable
	// store.

	// 指定需要committed到存储或者状态机的entry
	// 已经提交到了稳定存储
	CommittedEntries []pb.Entry

	// Messages specifies outbound messages to be sent AFTER Entries are
	// committed to stable storage.
	// If it contains a MsgSnap message, the application MUST report back to raft
	// when the snapshot has been received or has failed by calling ReportSnapshot.

	// 消息指定将条目提交到稳定存储后要发送的出站消息。
	// 如果它包含一条MsgSnap消息，则当收到快照或快照失败时，应用程序务必通过调用ReportSnapshot向raft报告。
	Messages []pb.Message

	// MustSync indicates whether the HardState and Entries must be synchronously
	// written to disk or if an asynchronous write is permissible.

	// MustSync指示是否必须将HardState和Entries同步写入磁盘，或者是否允许异步写入。
	MustSync bool
}

func isHardStateEqual(a, b pb.HardState) bool {
	return a.Term == b.Term && a.Vote == b.Vote && a.Commit == b.Commit
}

// IsEmptyHardState returns true if the given HardState is empty.
func IsEmptyHardState(st pb.HardState) bool {
	return isHardStateEqual(st, emptyState)
}

// IsEmptySnap returns true if the given Snapshot is empty.

// 根据Snapshot.Metadata.Index == 0来判断Snapshot是不是为空
func IsEmptySnap(sp pb.Snapshot) bool {
	return sp.Metadata.Index == 0
}

// 检测Ready是否已经更新过了
func (rd Ready) containsUpdates() bool {
	return rd.SoftState != nil || !IsEmptyHardState(rd.HardState) ||
		!IsEmptySnap(rd.Snapshot) || len(rd.Entries) > 0 ||
		len(rd.CommittedEntries) > 0 || len(rd.Messages) > 0 || len(rd.ReadStates) != 0
}

// appliedCursor extracts from the Ready the highest index the client has
// applied (once the Ready is confirmed via Advance). If no information is
// contained in the Ready, returns zero.

// appliedCursor从“就绪”中提取客户端已应用的最高索引（一旦通过“前进”确认了“就绪”）。 如果“就绪”中未包含任何信息，则返回零。
func (rd Ready) appliedCursor() uint64 {
	if n := len(rd.CommittedEntries); n > 0 {
		return rd.CommittedEntries[n-1].Index
	}
	if index := rd.Snapshot.Metadata.Index; index > 0 {
		return index
	}
	return 0
}

// Node represents a node in a raft cluster.
// Node代表在raft集群中的一个节点

/*
raft库对外提供一个Node的interface，其是现有raft/node.go中的node结构体实现，
这也是应用层唯一需要与这个raft库直接打交道的结构体，简单的来看看Node接口需要实现的函数：

函数	                作用
Tick	            应用层每次tick时需要调用该函数，将会由这里驱动raft的一些操作比如选举等。
                    至于tick的单位是多少由应用层自己决定，只要保证是恒定时间都会来调用一次就好了。
Campaign	        调用该函数将驱动节点进入候选人状态，进而将竞争leader。
Propose	            提议写入数据到日志中，可能会返回错误。
ProposeConfChange	提交配置变更
Step 	            将消息msg灌入状态机中
Ready	            这里是核心函数，将返回Ready的channel，应用层需要关注这个channel，当发生变更时将其中的数据进行操作
Advance	            Advance函数是当使用者已经将上一次Ready数据处理之后，调用该函数告诉raft库可以进行下一步的操作

流程
// HTTP server
HttpServer主循环:
  接收用户提交的数据：
    如果是PUT请求：
      将数据写入到proposeC中
    如果是POST请求：
      将配置变更数据写入到confChangeC中

// raft Node
raftNode结构体主循环：
  如果proposeC中有数据写入：
    调用node.Propose向raft库提交数据
  如果confChangeC中有数据写入：
    调用node.Node.ProposeConfChange向raft库提交配置变更数据
  如果tick定时器到期：
    调用node.Tick函数进行raft库的定时操作
  如果node.Ready()函数返回的Ready结构体channel有数据变更：
    依次处理Ready结构体中各成员数据
    处理完毕之后调用node.Advance函数进行收尾处理

通过node结构体实现的Node接口与raft库进行交互，涉及数据变更的核心数据结构就是Ready结构体，接下来可以进一步来分析该库的实现了。
*/

type Node interface {
	// Tick increments the internal logical clock for the Node by a single tick. Election
	// timeouts and heartbeat timeouts are in units of ticks.
	// Tick通过一个单独的tick来增加Node内部逻辑时钟,选举超时时间和心跳超时时间是以tick为单位的(多个个tick)
	// 设置一个定时器tick，每次定时器到时时，调用Node.Tick函数。
	Tick()

	// Campaign causes the Node to transition to candidate state and start campaigning to become leader.
    // Campaign引起Node角色转换,从候选人到领导人
	Campaign(ctx context.Context) error

	// Propose proposes that data be appended to the log. Note that proposals can be lost without
	// notice, therefore it is user's job to ensure proposal retries.

	// Propose提交data追加到日志，进行数据的提交
	Propose(ctx context.Context, data []byte) error

	// ProposeConfChange proposes config change.
	// At most one ConfChange can be in the process of going through consensus.
	// Application needs to call ApplyConfChange when applying EntryConfChange type entry.

	// 配置改变,最多有一个配置ConfChange达成共识,应用程序应该调用ApplyConfChange来处理EntryConfChange的日志
	// 进行配置变更
	ProposeConfChange(ctx context.Context, cc pb.ConfChange) error

	// Step advances the state machine using the given message. ctx.Err() will be returned, if any.
    // 使用给定的消息推动状态机的改变
	Step(ctx context.Context, msg pb.Message) error

	// Ready returns a channel that returns the current point-in-time state.
	// Users of the Node must call Advance after retrieving the state returned by Ready.
	//
	// NOTE: No committed entries from the next Ready may be applied until all committed entries
	// and snapshots from the previous one have finished.

	// 在接收到从Ready返回的状态后,用户必须调用Advance
	// 注意: 在所有的提交日志和snapshots从之前的返回之前, 没有提交的日志被应用
	Ready() <-chan Ready     // 单向读channel

	// Advance notifies the Node that the application has saved progress up to the last Ready.
	// It prepares the node to return the next available Ready.
	//
	// The application should generally call Advance after it applies the entries in last Ready.
	//
	// However, as an optimization, the application may call Advance while it is applying the
	// commands. For example. when the last Ready contains a snapshot, the application might take
	// a long time to apply the snapshot data. To continue receiving Ready without blocking raft
	// progress, it can call Advance before finishing applying the last ready.

	// Advance通知节点 应用程序已经根据最新的Ready保存进度,准备下次可以读的Ready
	// 在应用了日志之后,应用程序应该调用Advance
	// 进行收尾
	Advance()

	// ApplyConfChange applies config change to the local node.
	// Returns an opaque ConfState protobuf which must be recorded
	// in snapshots. Will never return nil; it returns a pointer only
	// to match MemoryStorage.Compact.

	// ApplyConfChange应用config改变到本地节点
	// 返回一个不透明的ConfState protobuf, 不许记录在snapshots中
	ApplyConfChange(cc pb.ConfChange) *pb.ConfState

	// TransferLeadership attempts to transfer leadership to the given transferee.

	// TransferLeadership尝试转化领导权到transferee
	TransferLeadership(ctx context.Context, lead, transferee uint64)

	// ReadIndex request a read state. The read state will be set in the ready.
	// Read state has a read index. Once the application advances further than the read
	// index, any linearizable read requests issued before the read request can be
	// processed safely. The read state will have the same rctx attached.


	ReadIndex(ctx context.Context, rctx []byte) error

	// Status returns the current status of the raft state machine.
	// 返回raft状态机的当前状态
	Status() Status

	// ReportUnreachable reports the given node is not reachable for the last send.
	// 最后一次send，指定的node不可达
	ReportUnreachable(id uint64)

	// ReportSnapshot reports the status of the sent snapshot. The id is the raft ID of the follower
	// who is meant to receive the snapshot, and the status is SnapshotFinish or SnapshotFailure.
	// Calling ReportSnapshot with SnapshotFinish is a no-op. But, any failure in applying a
	// snapshot (for e.g., while streaming it from leader to follower), should be reported to the
	// leader with SnapshotFailure. When leader sends a snapshot to a follower, it pauses any raft
	// log probes until the follower can apply the snapshot and advance its state. If the follower
	// can't do that, for e.g., due to a crash, it could end up in a limbo, never getting any
	// updates from the leader. Therefore, it is crucial that the application ensures that any
	// failure in snapshot sending is caught and reported back to the leader; so it can resume raft
	// log probing in the follower.

	// 报告发送的snapshot状态
	// 该ID是用于接收快照的follower的raft ID
	// status为SnapshotFinish或SnapshotFailure。
	ReportSnapshot(id uint64, status SnapshotStatus)

	// Stop performs any necessary termination of the Node.
	Stop()
}

type Peer struct {
	ID      uint64
	Context []byte
}

// StartNode returns a new Node given configuration and a list of raft peers.
// It appends a ConfChangeAddNode entry for each given peer to the initial log.

// StartNode返回给定配置的新Node和raft peers 列表。
// 它将每个给定peer的ConfChangeAddNode条目附加到初始日志。
func StartNode(c *Config, peers []Peer) Node {
	r := newRaft(c)

	// become the follower at term 1 and apply initial configuration
	// entries of term 1
	// 在term为1的时变成为follower，应用term 1 的初始配置entry
	r.becomeFollower(1, None)
	for _, peer := range peers {
		cc := pb.ConfChange{Type: pb.ConfChangeAddNode, NodeID: peer.ID, Context: peer.Context}
		d, err := cc.Marshal()
		if err != nil {
			panic("unexpected marshal error")
		}
		e := pb.Entry{Type: pb.EntryConfChange, Term: 1, Index: r.raftLog.lastIndex() + 1, Data: d}
		r.raftLog.append(e)
	}
	// Mark these initial entries as committed.
	// TODO(bdarnell): These entries are still unstable; do we need to preserve
	// the invariant that committed < unstable?
	r.raftLog.committed = r.raftLog.lastIndex()

	// Now apply them, mainly so that the application can call Campaign
	// immediately after StartNode in tests. Note that these nodes will
	// be added to raft twice: here and when the application's Ready
	// loop calls ApplyConfChange. The calls to addNode must come after
	// all calls to raftLog.append so progress.next is set after these
	// bootstrapping entries (it is an error if we try to append these
	// entries since they have already been committed).
	// We do not set raftLog.applied so the application will be able
	// to observe all conf changes via Ready.CommittedEntries.
	for _, peer := range peers {
		r.addNode(peer.ID)
	}

	n := newNode()
	n.logger = c.Logger

	// 使用协程去处理
	go n.run(r)
	return &n
}

// RestartNode is similar to StartNode but does not take a list of peers.
// The current membership of the cluster will be restored from the Storage.
// If the caller has an existing state machine, pass in the last log index that
// has been applied to it; otherwise use zero.
// RestartNode与StartNode类似，但不包含peer列表。
// 群集的当前成员将从存储中恢复。
// 如果调用方具有现有状态机，则传入已应用到该状态机的最后一个日志索引。 否则使用零。
func RestartNode(c *Config) Node {
	r := newRaft(c)

	n := newNode()
	n.logger = c.Logger
	go n.run(r)
	return &n
}

type msgWithResult struct {
	m      pb.Message
	result chan error
}

// node is the canonical implementation of the Node interface
// node是Node接口的一个典型的实现
// 大部分成员都是channel

// 实现Node接口
type node struct {
	propc      chan msgWithResult  // 是从上层应用传进来的消
	recvc      chan pb.Message     // 拿到的是从上层应用传进来的消
	confc      chan pb.ConfChange
	confstatec chan pb.ConfState
	readyc     chan Ready

	// 直接定义一个空的结构体并没有意义，但在并发编程中，channel之间的通讯，可以使用一个struct{}作为信号量。
	advancec   chan struct{}
	tickc      chan struct{}
	done       chan struct{}
	stop       chan struct{}
	status     chan chan Status
	logger Logger
}

func newNode() node {
	return node{
		propc:      make(chan msgWithResult),
		recvc:      make(chan pb.Message),
		confc:      make(chan pb.ConfChange),
		confstatec: make(chan pb.ConfState),
		readyc:     make(chan Ready),
		advancec:   make(chan struct{}),
		// make tickc a buffered chan, so raft node can buffer some ticks when the node
		// is busy processing raft messages. Raft node will resume process buffered
		// ticks when it becomes idle.

		// 将tickc设置为缓冲通道，以便raft节点在忙于处理raft消息时可以缓冲一些滴答。
		// raft节点在空闲时将恢复进程缓冲的滴答。
		tickc:  make(chan struct{}, 128),
		done:   make(chan struct{}),
		stop:   make(chan struct{}),
		status: make(chan chan Status),
	}
}

func (n *node) Stop() {
	select {
	// 直接定义一个空的结构体并没有意义，但在并发编程中，channel之间的通讯，可以使用一个struct{}作为信号量。
	case n.stop <- struct{}{}:
		// Not already stopped, so trigger it
		// 还没有停止,只是去触发停止
	case <-n.done:
		// Node has already been stopped - no need to do anything
		// 已经停止,不要做任何事情
		return
	}

	// Block until the stop has been acknowledged by run()
	// 阻塞直到stop已经被run()接受
	<-n.done
}


// node 主要逻辑是 func (n *node) run()，从各种 channel 获取处理事件。
func (n *node) run(r *raft) {
	var propc chan msgWithResult
	var readyc chan Ready
	var advancec chan struct{}
	var prevLastUnstablei, prevLastUnstablet uint64
	var havePrevLastUnstablei bool
	var prevSnapi uint64
	var applyingToI uint64
	var rd Ready

	lead := None
	prevSoftSt := r.softState()
	prevHardSt := emptyState

	for {
		// nil代表很多类型的零值
		// advancec 和readyc 配合使用，advance后再处理接下来的ready
		if advancec != nil {
			readyc = nil
		} else {
			rd = newReady(r, prevSoftSt, prevHardSt)
			if rd.containsUpdates() {
				readyc = n.readyc
			} else {
				readyc = nil
			}
		}

		if lead != r.lead {
			if r.hasLeader() {
				if lead == None {
					r.logger.Infof("raft.node: %x elected leader %x at term %d", r.id, r.lead, r.Term)
				} else {
					r.logger.Infof("raft.node: %x changed leader from %x to %x at term %d", r.id, lead, r.lead, r.Term)
				}
				propc = n.propc   //  只有leader 才处理propc
			} else {
				r.logger.Infof("raft.node: %x lost leader %x at term %d", r.id, lead, r.Term)
				propc = nil
			}
			lead = r.lead
		}

		// 使用select关键字处理异步IO， 其中一个文件句柄发生了IO动作，该select调用就会被返回
		// 每个case语句里必须是一个IO操作
		// 每个case语句必须是一个面向channel的操作
		select {
		// TODO: maybe buffer the config propose if there exists one (the way
		// described in raft dissertation)
		// Currently it is dropped in Step silently.
		// 拿到的是从上层应用传进来的消
		case pm := <-propc:
			m := pm.m
			m.From = r.id
			err := r.Step(m)        // leader 通过raft的step方法处理proposal
			if pm.result != nil {
				pm.result <- err
				close(pm.result)
			}

		// 拿到的是从上层应用传进来的消
		case m := <-n.recvc:
			// filter out response message from unknown From.
			if pr := r.getProgress(m.From); pr != nil || !IsResponseMsg(m.Type) {
				r.Step(m)   // 通过网络接收的消息
			}

		case cc := <-n.confc:    // 配置变更
			if cc.NodeID == None {
				select {
				case n.confstatec <- pb.ConfState{
					Nodes:    r.nodes(),
					Learners: r.learnerNodes()}:
				case <-n.done:
				}
				break
			}
			switch cc.Type {
			case pb.ConfChangeAddNode:
				// 增加节点
				r.addNode(cc.NodeID)
			case pb.ConfChangeAddLearnerNode:
				r.addLearner(cc.NodeID)
			case pb.ConfChangeRemoveNode:
				// block incoming proposal when local node is
				// removed
				// 删除节点
				if cc.NodeID == r.id {
					propc = nil
				}
				r.removeNode(cc.NodeID)
			case pb.ConfChangeUpdateNode:
			default:
				panic("unexpected conf type")
			}
			select {
			case n.confstatec <- pb.ConfState{
				Nodes:    r.nodes(),
				Learners: r.learnerNodes()}:
			case <-n.done:
			}

        // 首先，在node的大循环里，有一个会定时输出的tick channel，它来触发raft.tick()函数，
        // 根据上面的介绍可知，如果当前节点是follower，那它的tick函数会指向tickElection。
		case <-n.tickc:
			r.tick()    // 时钟tick

		// 在etcd的这个实现中，node并不负责数据的持久化、网络消息的通信、以及将已经提交的log应用到状态机中，
		// 所以node使用readyc这个channel对外通知有数据要处理了，并将这些需要外部处理的数据打包到一个Ready结构体中：

		// 将ready 发送给用户处理
		case readyc <- rd:
			if rd.SoftState != nil {
				prevSoftSt = rd.SoftState
			}
			if len(rd.Entries) > 0 {
				prevLastUnstablei = rd.Entries[len(rd.Entries)-1].Index
				prevLastUnstablet = rd.Entries[len(rd.Entries)-1].Term
				havePrevLastUnstablei = true
			}
			if !IsEmptyHardState(rd.HardState) {
				prevHardSt = rd.HardState
			}
			if !IsEmptySnap(rd.Snapshot) {
				prevSnapi = rd.Snapshot.Metadata.Index
			}
			if index := rd.appliedCursor(); index != 0 {
				applyingToI = index
			}

			r.msgs = nil
			r.readStates = nil
			r.reduceUncommittedSize(rd.CommittedEntries)
			advancec = n.advancec

        //  用户处理完ready，通过advance 通知可以处理下一条
		case <-advancec:
			if applyingToI != 0 {
				r.raftLog.appliedTo(applyingToI)
				applyingToI = 0
			}
			if havePrevLastUnstablei {
				r.raftLog.stableTo(prevLastUnstablei, prevLastUnstablet)
				havePrevLastUnstablei = false
			}
			r.raftLog.stableSnapTo(prevSnapi)
			advancec = nil
		case c := <-n.status:
			c <- getStatus(r)
		case <-n.stop:
			close(n.done)
			return
		}
	}
}

// Tick increments the internal logical clock for this Node. Election timeouts
// and heartbeat timeouts are in units of ticks.
// 增加此节点的内部逻辑时钟。
// 选举超时和心跳超时以滴答为单位。
func (n *node) Tick() {
	select {
	case n.tickc <- struct{}{}:
	case <-n.done:
	default:
		n.logger.Warningf("A tick missed to fire. Node blocks too long!")
	}
}

func (n *node) Campaign(ctx context.Context) error { return n.step(ctx, pb.Message{Type: pb.MsgHup}) }

// Propose提交data追加到日志
// 一个写请求一般会通过调用node.Propose开始，Propose方法将这个写请求封装到一个MsgProp消息里面，发送给自己处理。
func (n *node) Propose(ctx context.Context, data []byte) error {
	return n.stepWait(ctx, pb.Message{Type: pb.MsgProp, Entries: []pb.Entry{{Data: data}}})
}

func (n *node) Step(ctx context.Context, m pb.Message) error {
	// ignore unexpected local messages receiving over network
	if IsLocalMsg(m.Type) {
		// TODO: return an error?
		return nil
	}
	return n.step(ctx, m)
}

func (n *node) ProposeConfChange(ctx context.Context, cc pb.ConfChange) error {
	data, err := cc.Marshal()
	if err != nil {
		return err
	}
	return n.Step(ctx, pb.Message{Type: pb.MsgProp, Entries: []pb.Entry{{Type: pb.EntryConfChange, Data: data}}})
}

func (n *node) step(ctx context.Context, m pb.Message) error {
	return n.stepWithWaitOption(ctx, m, false)
}

func (n *node) stepWait(ctx context.Context, m pb.Message) error {
	return n.stepWithWaitOption(ctx, m, true)
}

// Step advances the state machine using msgs. The ctx.Err() will be returned,
// if any.

// 使用msgs推进状态机。 ctx.Err（）将被返回（如果有）。
func (n *node) stepWithWaitOption(ctx context.Context, m pb.Message, wait bool) error {
	if m.Type != pb.MsgProp {
		select {
		case n.recvc <- m:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		case <-n.done:
			return ErrStopped
		}
	}
	ch := n.propc
	pm := msgWithResult{m: m}
	if wait {
		pm.result = make(chan error, 1)
	}

	select {
	case ch <- pm:
		if !wait {
			return nil
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-n.done:
		return ErrStopped
	}

	select {
	case rsp := <-pm.result:
		if rsp != nil {
			return rsp
		}
	case <-ctx.Done():
		return ctx.Err()
	case <-n.done:
		return ErrStopped
	}
	return nil
}

func (n *node) Ready() <-chan Ready { return n.readyc }

func (n *node) Advance() {
	select {
	case n.advancec <- struct{}{}:
	case <-n.done:
	}
}

func (n *node) ApplyConfChange(cc pb.ConfChange) *pb.ConfState {
	var cs pb.ConfState
	select {
	case n.confc <- cc:
	case <-n.done:
	}
	select {
	case cs = <-n.confstatec:
	case <-n.done:
	}
	return &cs
}

func (n *node) Status() Status {
	c := make(chan Status)
	select {
	case n.status <- c:
		return <-c
	case <-n.done:
		return Status{}
	}
}

func (n *node) ReportUnreachable(id uint64) {
	select {
	case n.recvc <- pb.Message{Type: pb.MsgUnreachable, From: id}:
	case <-n.done:
	}
}

func (n *node) ReportSnapshot(id uint64, status SnapshotStatus) {
	rej := status == SnapshotFailure

	select {
	case n.recvc <- pb.Message{Type: pb.MsgSnapStatus, From: id, Reject: rej}:
	case <-n.done:
	}
}

func (n *node) TransferLeadership(ctx context.Context, lead, transferee uint64) {
	select {
	// manually set 'from' and 'to', so that leader can voluntarily transfers its leadership
	case n.recvc <- pb.Message{Type: pb.MsgTransferLeader, From: transferee, To: lead}:
	case <-n.done:
	case <-ctx.Done():
	}
}

/*
   server收到客户端的读请求，此时会调用ReadIndex函数发起一个MsgReadIndex的请求，
   带上的参数是客户端读请求的唯一标识（此时可以对照前面分析的MsgReadIndex及其对应应答消息的格式）
 */
func (n *node) ReadIndex(ctx context.Context, rctx []byte) error {
	return n.step(ctx, pb.Message{Type: pb.MsgReadIndex, Entries: []pb.Entry{{Data: rctx}}})
}

func newReady(r *raft, prevSoftSt *SoftState, prevHardSt pb.HardState) Ready {
	rd := Ready{
		Entries:          r.raftLog.unstableEntries(),
		CommittedEntries: r.raftLog.nextEnts(),
		Messages:         r.msgs,
	}
	if softSt := r.softState(); !softSt.equal(prevSoftSt) {
		rd.SoftState = softSt
	}
	if hardSt := r.hardState(); !isHardStateEqual(hardSt, prevHardSt) {
		rd.HardState = hardSt
	}
	if r.raftLog.unstable.snapshot != nil {
		rd.Snapshot = *r.raftLog.unstable.snapshot
	}
	if len(r.readStates) != 0 {
		rd.ReadStates = r.readStates
	}
	rd.MustSync = MustSync(r.hardState(), prevHardSt, len(rd.Entries))
	return rd
}

// MustSync returns true if the hard state and count of Raft entries indicate
// that a synchronous write to persistent storage is required.

// 如果Raft条目的硬状态和计数表明需要对持久性存储进行同步写入，则MustSync返回true。
func MustSync(st, prevst pb.HardState, entsnum int) bool {
	// Persistent state on all servers:
	// (Updated on stable storage before responding to RPCs)
	// currentTerm
	// votedFor
	// log entries[]
	return entsnum != 0 || st.Vote != prevst.Vote || st.Term != prevst.Term
}
