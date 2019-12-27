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

/*
Package raft sends and receives messages in the Protocol Buffer format
defined in the raftpb package.

raft包用raftpb包中定义的Protocol Buffer forma来发送和接收消息

Raft is a protocol with which a cluster of nodes can maintain a replicated state machine.
The state machine is kept in sync through the use of a replicated log.
For more details on Raft, see "In Search of an Understandable Consensus Algorithm"
(https://ramcloud.stanford.edu/raft.pdf) by Diego Ongaro and John Ousterhout.

Raft是一种协议，集群节点可使用该协议维护复制的状态机。通过使用复制的日志，状态机保持同步。
有关Raft的更多详细信息，请参见“寻找可理解的共识算法”
（https://ramcloud.stanford.edu/raft.pdf），作者是Diego Ongaro和John Ousterhout。

A simple example application, _raftexample_, is also available to help illustrate
how to use this package in practice:
https://github.com/etcd-io/etcd/tree/master/contrib/raftexample
还提供了一个简单的示例应用程序_raftexample_来帮助说明如何在实践中使用此软件包：
https://github.com/etcd-io/etcd/tree/master/contrib/raftexample

Usage

The primary object in raft is a Node. You either start a Node from scratch
using raft.StartNode or start a Node from some initial state using raft.RestartNode.
raft中的主要对象是一个节点。 您可以使用raft.StartNode从头启动节点，也可以使用raft.RestartNode从某个初始状态启动节点。


To start a node from scratch:
要从头开始节点：

  storage := raft.NewMemoryStorage()
  c := &Config{
    ID:              0x01,
    ElectionTick:    10,
    HeartbeatTick:   1,
    Storage:         storage,
    MaxSizePerMsg:   4096,
    MaxInflightMsgs: 256,
  }
  n := raft.StartNode(c, []raft.Peer{{ID: 0x02}, {ID: 0x03}})


To restart a node from previous state:
要从先前的状态重启节点：

  storage := raft.NewMemoryStorage()

  // recover the in-memory storage from persistent
  // snapshot, state and entries.
  storage.ApplySnapshot(snapshot)
  storage.SetHardState(state)
  storage.Append(entries)

  c := &Config{
    ID:              0x01,
    ElectionTick:    10,
    HeartbeatTick:   1,
    Storage:         storage,
    MaxSizePerMsg:   4096,
    MaxInflightMsgs: 256,
  }

  // restart raft without peer information.
  // peer information is already included in the storage.
  n := raft.RestartNode(c)

Now that you are holding onto a Node you have a few responsibilities:
现在您已经拥有一个节点，您将承担以下几项责任：

First, you must read from the Node.Ready() channel and process the updates
it contains. These steps may be performed in parallel, except as noted in step
2.
首先，您必须从Node.Ready（）通道读取并处理它包含的更新。 这些步骤可以并行执行，除非步骤2中另有说明。

1. Write HardState, Entries, and Snapshot to persistent storage if they are
not empty. Note that when writing an Entry with Index i, any
previously-persisted entries with Index >= i must be discarded.
如果HardState，Entries和Snapshot不为空，则将它们写入持久性存储。 请注意，在编写索引为i的条目时，
必须丢弃索引 >= i的所有先前存在的条目。


2. Send all Messages to the nodes named in the To field. It is important that
no messages be sent until the latest HardState has been persisted to disk,
and all Entries written by any previous Ready batch (Messages may be sent while
entries from the same batch are being persisted). To reduce the I/O latency, an
optimization can be applied to make leader write to disk in parallel with its
followers (as explained at section 10.2.1 in Raft thesis). If any Message has type
MsgSnap, call Node.ReportSnapshot() after it has been sent (these messages may be
large).
将所有消息发送到“收件人”字段中命名的节点。 重要的是，直到将最新的HardState持久化到磁盘上，
以及任何先前的Ready批处理写入的所有条目之前，都不要发送任何消息（可以在持久化来自同一批处理的条目时发送消息）。
为了减少I / O延迟，可以应用优化以使leader与其跟随者并行地写入磁盘（如Raft论文中的10.2.1节所述）。
如果任何消息的类型为MsgSnap，请在发送消息后调用Node.ReportSnapshot（）（这些消息可能很大）。

Note: Marshalling messages is not thread-safe; it is important that you
make sure that no new entries are persisted while marshalling.
The easiest way to achieve this is to serialize the messages directly inside
your main raft loop.
注意：编组消息不是线程安全的；
重要的是，请确保在编组期间不保留任何新条目。 实现此目的的最简单方法是直接在主Raft循环内序列化消息。

3. Apply Snapshot (if any) and CommittedEntries to the state machine.
If any committed Entry has Type EntryConfChange, call Node.ApplyConfChange()
to apply it to the node. The configuration change may be cancelled at this point
by setting the NodeID field to zero before calling ApplyConfChange
(but ApplyConfChange must be called one way or the other, and the decision to cancel
must be based solely on the state machine and not external information such as
the observed health of the node).
将快照（如果有）和CommittedEntries应用于状态机。 如果任何已提交的Entry的类型为EntryConfChange，
则调用Node.ApplyConfChange（）将其应用于节点。 此时可以通过在调用ApplyConfChange之前将NodeID字段设置为零来取消配置更改
（但是ApplyConfChange必须以一种或另一种方式调用，并且取消决定必须仅基于状态机而不是外部信息，例如 观察到的节点运行状况）。


4. Call Node.Advance() to signal readiness for the next batch of updates.
This may be done at any time after step 1, although all updates must be processed
in the order they were returned by Ready.
调用Node.Advance（）唤醒readiness进行下一批更新。 尽管必须按照Ready返回的顺序处理所有更新，但是可以在步骤1之后的任何时间完成此操作。



Second, all persisted log entries must be made available via an
implementation of the Storage interface. The provided MemoryStorage
type can be used for this (if you repopulate its state upon a
restart), or you can supply your own disk-backed implementation.
其次，必须通过存储接口的实现使所有持久日志条目可用。 提供的MemoryStorage类型可以用于此目的
（如果在重新启动时重新填充其状态），或者可以提供自己的磁盘支持的实现。

Third, when you receive a message from another node, pass it to Node.Step:

	func recvRaftRPC(ctx context.Context, m raftpb.Message) {
		n.Step(ctx, m)
	}

Finally, you need to call Node.Tick() at regular intervals (probably
via a time.Ticker). Raft has two important timeouts: heartbeat and the
election timeout. However, internally to the raft package time is
represented by an abstract "tick".
最后，您需要定期（可能通过time.Ticker）调用Node.Tick（）。 raft有两个重要的超时：
心跳和选举超时。 但是，在raft包内，时间用抽象的“滴答声”表示。

The total state machine handling loop will look something like this:

  for {
    select {
    case <-s.Ticker:
      n.Tick()
    case rd := <-s.Node.Ready():
      saveToStorage(rd.State, rd.Entries, rd.Snapshot)
      send(rd.Messages)
      if !raft.IsEmptySnap(rd.Snapshot) {
        processSnapshot(rd.Snapshot)
      }
      for _, entry := range rd.CommittedEntries {
        process(entry)
        if entry.Type == raftpb.EntryConfChange {
          var cc raftpb.ConfChange
          cc.Unmarshal(entry.Data)
          s.Node.ApplyConfChange(cc)
        }
      }
      s.Node.Advance()
    case <-s.done:
      return
    }
  }

To propose changes to the state machine from your node take your application
data, serialize it into a byte slice and call:
要从您的应用程序数据节点状态机进行更改，，将其序列化为字节片并调用：

	n.Propose(ctx, data)

If the proposal is committed, data will appear in committed entries with type
raftpb.EntryNormal. There is no guarantee that a proposed command will be
committed; you may have to re-propose after a timeout.
如果提案已提交，则数据将以raftpb.EntryNormal类型出现在已提交的条目中。
无法保证将执行建议的命令； 您可能需要在超时后重新提出建议。


To add or remove a node in a cluster, build ConfChange struct 'cc' and call:
要添加或删除集群中的节点，请构建ConfChange结构'cc'并调用：
	n.ProposeConfChange(ctx, cc)

After config change is committed, some committed entry with type
raftpb.EntryConfChange will be returned. You must apply it to node through:
提交配置更改后，将返回一些类型为raftpb.EntryConfChange的已提交条目。 您必须通过以下方式将其应用于节点：

	var cc raftpb.ConfChange
	cc.Unmarshal(data)
	n.ApplyConfChange(cc)

Note: An ID represents a unique node in a cluster for all time. A
given ID MUST be used only once even if the old node has been removed.
This means that for example IP addresses make poor node IDs since they
may be reused. Node IDs must be non-zero.
注意：ID始终代表集群中的唯一节点。 即使删除了旧节点，给定的ID也必须仅使用一次。
这意味着，例如，IP地址会成为不良的节点ID，因为它们可能会被重用。 节点ID必须为非零。

Implementation notes

This implementation is up to date with the final Raft thesis
(https://ramcloud.stanford.edu/~ongaro/thesis.pdf), although our
implementation of the membership change protocol differs somewhat from
that described in chapter 4. The key invariant that membership changes
happen one node at a time is preserved, but in our implementation the
membership change takes effect when its entry is applied, not when it
is added to the log (so the entry is committed under the old
membership instead of the new). This is equivalent in terms of safety,
since the old and new configurations are guaranteed to overlap.
该实现是最新的Raft最终论文（https://ramcloud.stanford.edu/~ongaro/thesis.pdf），尽管
成员资格更改协议的实现与第4章中描述的有所不同。关键不变性是，成员资格更改一次在一个节点上发生，
但在我们的实现中，成员资格更改在应用其条目时生效，而不是在添加时生效到日志（因此该条目是在旧成员资格而不是新成员资格下提交的）。
就安全性而言，这是等效的，因为保证了新旧配置的重叠。


To ensure that we do not attempt to commit two membership changes at
once by matching log positions (which would be unsafe since they
should have different quorum requirements), we simply disallow any
proposed membership change while any uncommitted change appears in
the leader's log.
为确保我们不会尝试通过匹配日志位置来尝试一次提交两个成员资格更改（这是不安全的，
因为它们应具有不同的法定要求），我们只是禁止任何提议的成员资格更改，而任何未提交的更改都将出现在领导者的日志中。

This approach introduces a problem when you try to remove a member
from a two-member cluster: If one of the members dies before the
other one receives the commit of the confchange entry, then the member
cannot be removed any more since the cluster cannot make progress.
For this reason it is highly recommended to use three or more nodes in
every cluster.
当您尝试从两个成员的群集中删除一个成员时，这种方法会带来一个问题：如果一个成员在另
一个成员收到confchange条目的提交之前就已死亡，则该成员将无法再删除，因为该群集无法进展。
因此，强烈建议在每个群集中使用三个或更多节点。

MessageType

Package raft sends and receives message in Protocol Buffer format (defined
in raftpb package). Each state (follower, candidate, leader) implements its
own 'step' method ('stepFollower', 'stepCandidate', 'stepLeader') when
advancing with the given raftpb.Message. Each step is determined by its
raftpb.MessageType. Note that every step is checked by one common method
'Step' that safety-checks the terms of node and incoming message to prevent
stale log entries:
raft包以Protocol Buffer格式（在raftpb包中定义）发送和接收消息。 每个状态（追随者，候选人，领导者）
在会实现自己的“ step”方法（“ stepFollower”，“ stepCandidate”，“ stepLeader”）， 当与给定的raftpb.
Message一起前进时。 每个步骤均由其raftpb.MessageType确定。
请注意，每个步骤均通过一种通用方法“步骤”进行检查，该方法对节点和传入消息的条款进行安全检查，以防止日志条目过时


	'MsgHup' is used for election. If a node is a follower or candidate, the
	'tick' function in 'raft' struct is set as 'tickElection'. If a follower or
	candidate has not received any heartbeat before the election timeout, it
	passes 'MsgHup' to its Step method and becomes (or remains) a candidate to
	start a new election.

	'MsgBeat' is an internal type that signals the leader to send a heartbeat of
	the 'MsgHeartbeat' type. If a node is a leader, the 'tick' function in
	the 'raft' struct is set as 'tickHeartbeat', and triggers the leader to
	send periodic 'MsgHeartbeat' messages to its followers.

	'MsgProp' proposes to append data to its log entries. This is a special
	type to redirect proposals to leader. Therefore, send method overwrites
	raftpb.Message's term with its HardState's term to avoid attaching its
	local term to 'MsgProp'. When 'MsgProp' is passed to the leader's 'Step'
	method, the leader first calls the 'appendEntry' method to append entries
	to its log, and then calls 'bcastAppend' method to send those entries to
	its peers. When passed to candidate, 'MsgProp' is dropped. When passed to
	follower, 'MsgProp' is stored in follower's mailbox(msgs) by the send
	method. It is stored with sender's ID and later forwarded to leader by
	rafthttp package.

	'MsgApp' contains log entries to replicate. A leader calls bcastAppend,
	which calls sendAppend, which sends soon-to-be-replicated logs in 'MsgApp'
	type. When 'MsgApp' is passed to candidate's Step method, candidate reverts
	back to follower, because it indicates that there is a valid leader sending
	'MsgApp' messages. Candidate and follower respond to this message in
	'MsgAppResp' type.

	'MsgAppResp' is response to log replication request('MsgApp'). When
	'MsgApp' is passed to candidate or follower's Step method, it responds by
	calling 'handleAppendEntries' method, which sends 'MsgAppResp' to raft
	mailbox.

	'MsgVote' requests votes for election. When a node is a follower or
	candidate and 'MsgHup' is passed to its Step method, then the node calls
	'campaign' method to campaign itself to become a leader. Once 'campaign'
	method is called, the node becomes candidate and sends 'MsgVote' to peers
	in cluster to request votes. When passed to leader or candidate's Step
	method and the message's Term is lower than leader's or candidate's,
	'MsgVote' will be rejected ('MsgVoteResp' is returned with Reject true).
	If leader or candidate receives 'MsgVote' with higher term, it will revert
	back to follower. When 'MsgVote' is passed to follower, it votes for the
	sender only when sender's last term is greater than MsgVote's term or
	sender's last term is equal to MsgVote's term but sender's last committed
	index is greater than or equal to follower's.

	'MsgVoteResp' contains responses from voting request. When 'MsgVoteResp' is
	passed to candidate, the candidate calculates how many votes it has won. If
	it's more than majority (quorum), it becomes leader and calls 'bcastAppend'.
	If candidate receives majority of votes of denials, it reverts back to
	follower.

	'MsgPreVote' and 'MsgPreVoteResp' are used in an optional two-phase election
	protocol. When Config.PreVote is true, a pre-election is carried out first
	(using the same rules as a regular election), and no node increases its term
	number unless the pre-election indicates that the campaigning node would win.
	This minimizes disruption when a partitioned node rejoins the cluster.

	'MsgSnap' requests to install a snapshot message. When a node has just
	become a leader or the leader receives 'MsgProp' message, it calls
	'bcastAppend' method, which then calls 'sendAppend' method to each
	follower. In 'sendAppend', if a leader fails to get term or entries,
	the leader requests snapshot by sending 'MsgSnap' type message.

	'MsgSnapStatus' tells the result of snapshot install message. When a
	follower rejected 'MsgSnap', it indicates the snapshot request with
	'MsgSnap' had failed from network issues which causes the network layer
	to fail to send out snapshots to its followers. Then leader considers
	follower's progress as probe. When 'MsgSnap' were not rejected, it
	indicates that the snapshot succeeded and the leader sets follower's
	progress to probe and resumes its log replication.

	'MsgHeartbeat' sends heartbeat from leader. When 'MsgHeartbeat' is passed
	to candidate and message's term is higher than candidate's, the candidate
	reverts back to follower and updates its committed index from the one in
	this heartbeat. And it sends the message to its mailbox. When
	'MsgHeartbeat' is passed to follower's Step method and message's term is
	higher than follower's, the follower updates its leaderID with the ID
	from the message.

	'MsgHeartbeatResp' is a response to 'MsgHeartbeat'. When 'MsgHeartbeatResp'
	is passed to leader's Step method, the leader knows which follower
	responded. And only when the leader's last committed index is greater than
	follower's Match index, the leader runs 'sendAppend` method.

	'MsgUnreachable' tells that request(message) wasn't delivered. When
	'MsgUnreachable' is passed to leader's Step method, the leader discovers
	that the follower that sent this 'MsgUnreachable' is not reachable, often
	indicating 'MsgApp' is lost. When follower's progress state is replicate,
	the leader sets it back to probe.

*/
package raft
