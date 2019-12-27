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
	"bytes"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"strings"
	"sync"
	"time"

	pb "go.etcd.io/etcd/raft/raftpb"
)


// raft协议的具体实现就在这个文件里。这其中，大部分的逻辑是由Step函数驱动的

/*
Term	     选举任期，每次选举之后递增1
Vote	     选举投票(的ID)
Entry	     Raft算法的日志数据条目
candidate	 候选人
leader	     领导者
follower	 跟随者
commit	     提交
propose	     提议
 */


/*
每个raft的节点，分为以下三种状态：

candidate：候选人状态，节点切换到这个状态时，意味着将进行一次新的选举。
follower：跟随者状态，节点切换到这个状态时，意味着选举结束。
leader：领导者状态，所有数据提交都必须先提交到leader上。
每一个状态都有其对应的状态机，每次收到一条提交的数据时，都会根据其不同的状态将消息输入到不同状态的状态机中。
同时，在进行tick操作时，每种状态对应的处理函数也是不一样的。

所以raft结构体中将不同的状态，及其不同的处理函数独立出来几个成员变量：

成员	作用
state	保存当前节点状态
tick函数	tick函数，每个状态对应的tick函数不同
step函数	状态机函数，同样每个状态对应的状态机也不相同
raft库中提供几个成员函数becomeCandidate、becomeFollower、becomeLeader分别进入这几种状态的，这些函数中做的事情，概况起来就是：

切换raft.state成员到对应状态。
切换raft.tick函数到对应状态的处理函数。
切换raft.step函数到对应状态的状态机。
 */
// None is a placeholder node ID used when there is no leader.
// None 是当没有leader的时候。node Id 占位符，
const None uint64 = 0
const noLimit = math.MaxUint64

// Possible values for StateType.
const (
	StateFollower StateType = iota   // iota 被重设为0
	StateCandidate
	StateLeader
	StatePreCandidate
	numStates
)

type ReadOnlyOption int

const (
	// ReadOnlySafe guarantees the linearizability of the read only request by
	// communicating with the quorum. It is the default and suggested option.
	// ReadOnlySafe保证线性化访问
	// ReadOnlySafe通过与法定人数通信   来保证只读请求的线性化。 这是默认的建议选项。
	ReadOnlySafe ReadOnlyOption = iota

	// ReadOnlyLeaseBased ensures linearizability of the read only request by
	// relying on the leader lease. It can be affected by clock drift.
	// If the clock drift is unbounded, leader might keep the lease longer than it
	// should (clock can move backward/pause without any bound). ReadIndex is not safe
	// in that case.

	// ReadOnlyLeaseBased 通过依赖于领导者租约 来确保只读请求的线性化。 它可能会受到时钟漂移的影响。
	// 如果时钟漂移不受限制，则领导者可能会持有租约的时间比它应该持有的时间要长（时钟可以向后移动/暂停，没有任何限制）。
	// 在这种情况下，ReadIndex是不安全的。
	ReadOnlyLeaseBased
)

// Possible values for CampaignType
// 选举类型的取值
const (
	// campaignPreElection represents the first phase of a normal election when
	// Config.PreVote is true.
	// Config.PreVote 是true时， campaignPreElection表示正常选举的第一个阶段
	// 对应PreVote的场景
	campaignPreElection CampaignType = "CampaignPreElection"

	// campaignElection represents a normal (time-based) election (the second phase
	// of the election when Config.PreVote is true).
	// campaignElection表示基于时间的正常选举
	// 当Config.PreVote 是true时， campaignElection表示正常选举的第二个阶段
	// 正常的选举场景
	campaignElection CampaignType = "CampaignElection"

	// campaignTransfer represents the type of leader transfer
	// 由于leader转让发起的竞选类型
	// 由于leader迁移发生的选举。如果是这种类型的选举，那么msg.Context字段保存的是“CampaignTransfer”`字符串，
	// 这种情况下会强制进行leader的迁移。
	campaignTransfer CampaignType = "CampaignTransfer"
)

// ErrProposalDropped is returned when the proposal is ignored by some cases,
// so that the proposer can be notified and fail fast.
// 在某些case下，建议被忽略
var ErrProposalDropped = errors.New("raft proposal dropped")

// lockedRand is a small wrapper around rand.Rand to provide
// synchronization among multiple raft groups. Only the methods needed
// by the code are exposed (e.g. Intn).
// lockedRand 是 rand.Rand 的一个小的封装，用于在多个raft组中提供同步
type lockedRand struct {
	mu   sync.Mutex
	rand *rand.Rand
}

// Intn返回一个 非负伪随机数，[0,n)
func (r *lockedRand) Intn(n int) int {
	r.mu.Lock()

	// Intn返回一个 非负伪随机数，[0,n)
	v := r.rand.Intn(n)
	r.mu.Unlock()
	return v
}

// 随机数生成器
var globalRand = &lockedRand{
	rand: rand.New(rand.NewSource(time.Now().UnixNano())),
}

// CampaignType represents the type of campaigning
// the reason we use the type of string instead of uint64
// is because it's simpler to compare and fill in raft entries
// CampaignType代表竞选类型
// 使用string而不是uint64的原因：string类型更容易比较和在raft实体中填充
type CampaignType string

// StateType represents the role of a node in a cluster.
// StateType代表在集群中node的角色
type StateType uint64

var stmap = [...]string{
	"StateFollower",
	"StateCandidate",
	"StateLeader",
	"StatePreCandidate",
}

//  通过int值获取对应的string,  打印对应的名字
func (st StateType) String() string {
	return stmap[uint64(st)]
}

// Config contains the parameters to start a raft.
// 封装raft算法相关配置参数
// 与raft算法相关的配置参数都包装在该结构体中。从这个结构体的命名是大写字母开头，就可以知道是提供给外部调用的。
type Config struct {
	// ID is the identity of the local raft. ID cannot be 0.
	// ID是本地raft的标识      ID不能是0
	ID uint64

	// peers contains the IDs of all nodes (including self) in the raft cluster. It
	// should only be set when starting a new raft cluster. Restarting raft from
	// previous configuration will panic if peers is set. peer is private and only
	// used for testing right now.

	// peers包含raft群集中所有节点（包括自身）的ID。 仅在启动新的raft群集时才应设置它。
	// 如果peers设置了。从先前的配置重新启动raft会出现故障。 peer是私有的，并且目前仅用于测试。
	// 保存集群所有节点ID的数组（包括自身）
	peers []uint64

	// learners contains the IDs of all learner nodes (including self if the
	// local node is a learner) in the raft cluster. learners only receives
	// entries from the leader node. It does not vote or promote itself.

    // learners 包含raft集群中所有learner节点的ID（如果本地节点是learner，则包括自身）。
    // learners 仅从领导者节点接收条目。 它不会投票或自我提升。
	learners []uint64

	// ElectionTick is the number of Node.Tick invocations that must pass between
	// elections. That is, if a follower does not receive any message from the
	// leader of current term before ElectionTick has elapsed, it will become
	// candidate and start an election. ElectionTick must be greater than
	// HeartbeatTick. We suggest ElectionTick = 10 * HeartbeatTick to avoid
	// unnecessary leader switching.

	// ElectionTick是选举之间Node.Tick调用的数量。 也就是说，在ElectionTick消逝之前，
	// 如果follower没有从当前任职的领导者那里收到任何消息，它将成为候选人并开始选举。 ElectionTick必须大于HeartbeatTick。
	// 我们建议ElectionTick = 10 * HeartbeatTick以避免不必要的领导者切换。

	// 选举超时tick
	ElectionTick int


	// HeartbeatTick is the number of Node.Tick invocations that must pass between
	// heartbeats. That is, a leader sends heartbeat messages to maintain its
	// leadership every HeartbeatTick ticks.
	// HeartbeatTick是心跳之间Node.Tick调用的数量。 也就是说，领导者通过HeartbeatTick，发送心跳消息，来保持其领导地位。

	// 心跳超时tick
	HeartbeatTick int

	// Storage is the storage for raft. raft generates entries and states to be
	// stored in storage. raft reads the persisted entries and states out of
	// Storage when it needs. raft reads out the previous state and configuration
	// out of storage when restarting.

	// 存储是raft的存储。 raft生成条目和状态以存储在存储器中。 raft当需要的时候，从存储中读取一致的条目和状态。
	// 重新启动时，raft会从存储中读取以前的状态和配置。
	Storage Storage

	// Applied is the last applied index. It should only be set when restarting
	// raft. raft will not return entries to the application smaller or equal to
	// Applied. If Applied is unset when restarting, raft might return previous
	// applied entries. This is a very application dependent configuration.

	// Applied是是最后应用的索引。 仅应在重新启动raft时设置。
	// raft不会返回小于或等于Applied的条目给应用程序。 如果重新启动时Applied未设置，则raft可能会返回以前的applied条目。
	// 这是一个非常依赖于应用程序的配置。
	Applied uint64

	// MaxSizePerMsg limits the max byte size of each append message. Smaller
	// value lowers the raft recovery cost(initial probing and message lost
	// during normal operation). On the other side, it might affect the
	// throughput during normal replication. Note: math.MaxUint64 for unlimited,
	// 0 for at most one entry per message.

	// MaxSizePerMsg限制每个附加消息的最大字节大小。 较小的值可降低raft恢复成本（正常操作期间的初始探测和消息丢失）。
	// 另一方面，它可能会影响正常复制期间的吞吐量。
	// 注意：math.MaxUint64表示无限制，0表示每个消息最多一个条目。
	MaxSizePerMsg uint64

	// MaxCommittedSizePerReady limits the size of the committed entries which
	// can be applied.
	// MaxCommittedSizePerReady 限制committed 条目的大小， 该条目可以被应用
	MaxCommittedSizePerReady uint64

	// MaxUncommittedEntriesSize limits the aggregate byte size of the
	// uncommitted entries that may be appended to a leader's log. Once this
	// limit is exceeded, proposals will begin to return ErrProposalDropped
	// errors. Note: 0 for no limit.

	// MaxUncommittedEntriesSize 限制可以附加到领导者日志的未提交条目的总字节大小。
	// 一旦超过此限制，提议将开始返回ErrProposalDropped错误。 注意：0为无限制。
	MaxUncommittedEntriesSize uint64

	// MaxInflightMsgs limits the max number of in-flight append messages during
	// optimistic replication phase. The application transportation layer usually
	// has its own sending buffer over TCP/UDP. Setting MaxInflightMsgs to avoid
	// overflowing that sending buffer. TODO (xiangli): feedback to application to
	// limit the proposal rate?

	// MaxInflightMsgs限制了乐观复制阶段的正在运行中附加消息的最大数量。 应用程序传输层通常在TCP/UDP上具有自己的发送缓冲区。
	// 设置MaxInflightMsgs可以避免发送缓冲区溢出。

	MaxInflightMsgs int

	// CheckQuorum specifies if the leader should check quorum activity. Leader
	// steps down when quorum is not active for an electionTimeout.
	// 标记leader是否需要检查集群中超过半数节点的活跃性，如果在选举超时内没有满足该条件，leader切换到follower状态
	CheckQuorum bool

	// PreVote enables the Pre-Vote algorithm described in raft thesis section
	// 9.6. This prevents disruption when a node that has been partitioned away
	// rejoins the cluster.

	// PreVote启用了raft论文9.6节中描述的Pre-Vote算法。 这样可以防止当在已分区的节点重新加入群集时的中断。
	PreVote bool

	// ReadOnlyOption specifies how the read only request is processed.
	//
	// ReadOnlySafe guarantees the linearizability of the read only request by
	// communicating with the quorum. It is the default and suggested option.
	//
	// ReadOnlyLeaseBased ensures linearizability of the read only request by
	// relying on the leader lease. It can be affected by clock drift.
	// If the clock drift is unbounded, leader might keep the lease longer than it
	// should (clock can move backward/pause without any bound). ReadIndex is not safe
	// in that case.
	// CheckQuorum MUST be enabled if ReadOnlyOption is ReadOnlyLeaseBased.

	// ReadOnlyOption指定如何处理只读请求。
	// ReadOnlySafe 通过与法定人数的通信来保证只读请求的线性化。 这是默认的建议选项.
	//
	// ReadOnlyLeaseBased通过依赖于领导者租约来确保只读请求的线性化。 它可能会受到时钟漂移的影响。
	// 如果时钟漂移不受限制，则领导者可能会将租约保留的时间延长（时钟可以向后移动/暂停，没有任何限制）。 在这种情况下，ReadIndex是不安全的。

	//  如果ReadOnlyOption为ReadOnlyLeaseBased，则必须启用CheckQuorum。
	ReadOnlyOption ReadOnlyOption

	// Logger is the logger used for raft log. For multinode which can host
	// multiple raft group, each raft group can have its own logger

	// 记录器是用于raft记录的记录器。 对于拥有多个raft组的多节点，每个raft组可以有自己的记录器
	Logger Logger

	// DisableProposalForwarding set to true means that followers will drop
	// proposals, rather than forwarding them to the leader. One use case for
	// this feature would be in a situation where the Raft leader is used to
	// compute the data of a proposal, for example, adding a timestamp from a
	// hybrid logical clock to data in a monotonically increasing way. Forwarding
	// should be disabled to prevent a follower with an inaccurate hybrid
	// logical clock from assigning the timestamp and then forwarding the data
	// to the leader.

	// DisableProposalForwarding设置为true意味着followers将丢弃建议，而不是将其转发给领导者。
	// 此功能的一个用例是在使用Raft 领导者计算提议数据的情况下
	// 例如，以单调递增的方式将混合逻辑时钟的时间戳添加到数据。
	// 应该禁用转发，以防止具有不正确的混合逻辑时钟的跟随者分配时间戳，然后将数据转发给领导者。
	DisableProposalForwarding bool
}

// 配置参数的有效性
func (c *Config) validate() error {
	if c.ID == None {
		// raft id  不能为0
		return errors.New("cannot use none as id")
	}

	if c.HeartbeatTick <= 0 {
		return errors.New("heartbeat tick must be greater than 0")
	}

	if c.ElectionTick <= c.HeartbeatTick {
		return errors.New("election tick must be greater than heartbeat tick")
	}

	if c.Storage == nil {
		return errors.New("storage cannot be nil")
	}

	if c.MaxUncommittedEntriesSize == 0 {
		c.MaxUncommittedEntriesSize = noLimit
	}

	// default MaxCommittedSizePerReady to MaxSizePerMsg because they were
	// previously the same parameter.
	if c.MaxCommittedSizePerReady == 0 {
		c.MaxCommittedSizePerReady = c.MaxSizePerMsg
	}

	if c.MaxInflightMsgs <= 0 {
		return errors.New("max inflight messages must be greater than 0")
	}

	if c.Logger == nil {
		c.Logger = raftLogger
	}

	if c.ReadOnlyOption == ReadOnlyLeaseBased && !c.CheckQuorum {
		return errors.New("CheckQuorum must be enabled when ReadOnlyOption is ReadOnlyLeaseBased")
	}

	return nil
}

type raft struct {
	// 本节点raft Id
	id uint64

	// 任期号  选举任期，每次选举之后递增1
	Term uint64

	// 投票leader给哪个节点ID， 也就是选举投票(的ID)
	Vote uint64

	// readStates数组的信息，将做为ready结构体的信息更新给上层的raft协议库的使用者。
	readStates []ReadState

	// the log
	// raft 日志
	raftLog *raftLog

	maxMsgSize         uint64
	maxUncommittedSize uint64
	maxInflight        int

	// peer个数，也就是raft组里面副本的个数
	prs                map[uint64]*Progress
	learnerPrs         map[uint64]*Progress
	matchBuf           uint64Slice

	// 保存当前节点状态
	state StateType

	// isLearner is true if the local raft node is a learner.
	// 判断本地的raft node是否是learner
	isLearner bool

	// 该map存放哪些节点投票给了本节点
	votes map[uint64]bool

	msgs []pb.Message

	// the leader id
	lead uint64

	// leadTransferee is id of the leader transfer target when its value is not zero.
	// Follow the procedure defined in raft thesis 3.10.
	// leadTransferee的值不为零时，是领导者转移目标的ID。 遵循raft论文3.10中定义的过程。
	// leader转让的目标节点id
	leadTransferee uint64

	// Only one conf change may be pending (in the log, but not yet
	// applied) at a time. This is enforced via pendingConfIndex, which
	// is set to a value >= the log index of the latest pending
	// configuration change (if any). Config changes are only allowed to
	// be proposed if the leader's applied index is greater than this
	// value.

	// 一次只能进行一次配置更改（在日志中，但尚未应用）。 这是通过pendingConfIndex强制执行的，
	// 该值设置为 >= 最新的未决配置更改的日志索引（如果有）。 仅当领导者的applied索引大于此值时，才可以建议更改配置。

	// 标识当前还有没有applied的配置数据
	pendingConfIndex uint64


	// an estimate of the size of the uncommitted tail of the Raft log. Used to
	// prevent unbounded log growth. Only maintained by the leader. Reset on
	// term changes.

	// raft日志未提交尾部大小的估计值。 用于防止无限制的日志增长。 仅由领导者维护。 term改变的时候重置。
	uncommittedSize uint64

	readOnly *readOnly

	// number of ticks since it reached last electionTimeout when it is leader
	// or candidate.
	// number of ticks since it reached last electionTimeout or received a
	// valid message from current leader when it is a follower.

	// 当它是领导者或候选人时，自达到上次选举超时以来的滴答数。
	// 当它是follower，自从其达到上次选举超时或从当前领导者收到的有效消息以来的滴答数。
	electionElapsed int

	// number of ticks since it reached last heartbeatTimeout.
	// only leader keeps heartbeatElapsed.

	// 自达到上一个heartbeatTimeout以来的滴答数。
	// 只有领导者保持heartbeatElapsed。
	heartbeatElapsed int

	// 是否检查法定人数
	checkQuorum bool

	// 使用使用两阶段选举
	preVote     bool

	// 心跳超时时间
	heartbeatTimeout int

	// The election timeout is the amount of time a follower waits until becoming a candidate.
	// 选举超时时间是follower等待直到变成一个候选人的时间
	electionTimeout  int

	// randomizedElectionTimeout is a random number between
	// [electiontimeout, 2 * electiontimeout - 1]. It gets reset
	// when raft changes its state to follower or candidate.

	// randomizedElectionTimeout是介于[electiontimeout，2 *electiontimeout-1]之间的随机数。
	// 当raft更改自己的状态为follower或者候选者时，，它将重置。
	randomizedElectionTimeout int

	//  DisableProposalForwarding设置为true意味着followers将丢弃建议，而不是将其转发给领导者。
	disableProposalForwarding bool

	// tick函数，在到期的时候调用，不同的角色该函数不同
	// tick函数，每个状态对应的tick函数不同
	tick func()

	// 状态机函数，同样每个状态对应的状态机也不相同/**/
	step stepFunc

	logger Logger
}

// 新建raft
func newRaft(c *Config) *raft {
	if err := c.validate(); err != nil {
		panic(err.Error())
	}
	raftlog := newLogWithSize(c.Storage, c.Logger, c.MaxCommittedSizePerReady)
	hs, cs, err := c.Storage.InitialState()
	if err != nil {
		panic(err) // TODO(bdarnell)
	}
	peers := c.peers
	learners := c.learners
	if len(cs.Nodes) > 0 || len(cs.Learners) > 0 {
		if len(peers) > 0 || len(learners) > 0 {
			// TODO(bdarnell): the peers argument is always nil except in
			// tests; the argument should be removed and these tests should be
			// updated to specify their nodes through a snapshot.

			// 除非在测试中，否则peers参数始终为nil。 应删除该参数，并应更新这些测试以通过快照指定其节点。
			panic("cannot specify both newRaft(peers, learners) and ConfState.(Nodes, Learners)")
		}
		peers = cs.Nodes
		learners = cs.Learners
	}
	r := &raft{
		id:                        c.ID,       // raft id
		lead:                      None,       // leader id
		isLearner:                 false,      // 判断本地的raft node是否是learner
		raftLog:                   raftlog,
		maxMsgSize:                c.MaxSizePerMsg,
		maxInflight:               c.MaxInflightMsgs,
		maxUncommittedSize:        c.MaxUncommittedEntriesSize,
		prs:                       make(map[uint64]*Progress),
		learnerPrs:                make(map[uint64]*Progress),
		electionTimeout:           c.ElectionTick,
		heartbeatTimeout:          c.HeartbeatTick,
		logger:                    c.Logger,
		checkQuorum:               c.CheckQuorum,
		preVote:                   c.PreVote,
		readOnly:                  newReadOnly(c.ReadOnlyOption),
		disableProposalForwarding: c.DisableProposalForwarding,
	}

	for _, p := range peers {
		r.prs[p] = &Progress{Next: 1, ins: newInflights(r.maxInflight)}
	}
	for _, p := range learners {
		if _, ok := r.prs[p]; ok {
			panic(fmt.Sprintf("node %x is in both learner and peer list", p))
		}
		r.learnerPrs[p] = &Progress{Next: 1, ins: newInflights(r.maxInflight), IsLearner: true}
		if r.id == p {
			r.isLearner = true
		}
	}

	// 如果不是第一次启动而是从之前的数据进行恢复
	if !isHardStateEqual(hs, emptyState) {
		r.loadState(hs)
	}
	if c.Applied > 0 {
		raftlog.appliedTo(c.Applied)
	}
	// 启动都是follower状态
	r.becomeFollower(r.Term, None)

	var nodesStrs []string
	for _, n := range r.nodes() {
		nodesStrs = append(nodesStrs, fmt.Sprintf("%x", n))
	}

	r.logger.Infof("newRaft %x [peers: [%s], term: %d, commit: %d, applied: %d, lastindex: %d, lastterm: %d]",
		r.id, strings.Join(nodesStrs, ","), r.Term, r.raftLog.committed, r.raftLog.applied, r.raftLog.lastIndex(), r.raftLog.lastTerm())
	return r
}

// 判断是否有leader
func (r *raft) hasLeader() bool { return r.lead != None }


func (r *raft) softState() *SoftState { return &SoftState{Lead: r.lead, RaftState: r.state} }

func (r *raft) hardState() pb.HardState {
	return pb.HardState{
		Term:   r.Term,
		Vote:   r.Vote,
		Commit: r.raftLog.committed,
	}
}

// 计算法定人数,需要超过法定人数才可以当选
func (r *raft) quorum() int { return len(r.prs)/2 + 1 }

// 返回排序之后的节点ID数组
func (r *raft) nodes() []uint64 {
	nodes := make([]uint64, 0, len(r.prs))
	for id := range r.prs {
		nodes = append(nodes, id)
	}

	// 返回排序之后的节点ID数组
	sort.Sort(uint64Slice(nodes))
	return nodes
}

// 排序之后的learner Id数组
func (r *raft) learnerNodes() []uint64 {
	nodes := make([]uint64, 0, len(r.learnerPrs))
	for id := range r.learnerPrs {
		nodes = append(nodes, id)
	}
	sort.Sort(uint64Slice(nodes))
	return nodes
}

// send persists state to stable storage and then sends to its mailbox.
// 将持久状态发送到稳定的存储，然后发送到其邮箱。
func (r *raft) send(m pb.Message) {
	m.From = r.id
	if m.Type == pb.MsgVote || m.Type == pb.MsgVoteResp || m.Type == pb.MsgPreVote || m.Type == pb.MsgPreVoteResp {
		// 投票时term不能为空
		if m.Term == 0 {
			// All {pre-,}campaign messages need to have the term set when
			// sending.
			// - MsgVote: m.Term is the term the node is campaigning for,
			//   non-zero as we increment the term when campaigning.
			// - MsgVoteResp: m.Term is the new r.Term if the MsgVote was
			//   granted, non-zero for the same reason MsgVote is
			// - MsgPreVote: m.Term is the term the node will campaign,
			//   non-zero as we use m.Term to indicate the next term we'll be
			//   campaigning for
			// - MsgPreVoteResp: m.Term is the term received in the original
			//   MsgPreVote if the pre-vote was granted, non-zero for the
			//   same reasons MsgPreVote is
			panic(fmt.Sprintf("term should be set when sending %s", m.Type))
		}
	} else {
		// 其他的消息类型，term必须为空
		if m.Term != 0 {
			panic(fmt.Sprintf("term should not be set when sending %s (was %d)", m.Type, m.Term))
		}
		// do not attach term to MsgProp, MsgReadIndex
		// proposals are a way to forward to the leader and
		// should be treated as local message.
		// MsgReadIndex is also forwarded to leader.

		// 不要附加term 到MsgProp，MsgReadIndex
		// 建议是转发给领导者的一种方法，应被视为本地消息。
		// MsgReadIndex也转发给领导者。
		if m.Type != pb.MsgProp && m.Type != pb.MsgReadIndex {
			m.Term = r.Term
		}
	}
	// 注意这里只是添加到msgs中
	r.msgs = append(r.msgs, m)
}


// 返回id对应的 peer进程或者learner进程
func (r *raft) getProgress(id uint64) *Progress {
	if pr, ok := r.prs[id]; ok {
		return pr
	}

	return r.learnerPrs[id]
}

// sendAppend sends an append RPC with new entries (if any) and the
// current commit index to the given peer.

// sendAppend将带有新条目（如果有）和当前提交索引的附加RPC发送到给定的对等方。
// 向to节点发送append消息
func (r *raft) sendAppend(to uint64) {
	r.maybeSendAppend(to, true)
}

// maybeSendAppend sends an append RPC with new entries to the given peer,
// if necessary. Returns true if a message was sent. The sendIfEmpty
// argument controls whether messages with no entries will be sent
// ("empty" messages are useful to convey updated Commit indexes, but
// are undesirable when we're sending multiple messages in a batch).

// 必要时，maybeSendAppend将带有新条目的附加RPC发送到给定的对等方。
// 如果发送了消息，则返回true。

// sendIfEmpty参数控制是否发送不带任何条目的消息（“空”消息对于传达更新的Commit索引很有用，
// 但是当我们批量发送多条消息时是不希望的）。
func (r *raft) maybeSendAppend(to uint64, sendIfEmpty bool) bool {

	// 获取消息接收者的进程
	pr := r.getProgress(to)
	if pr.IsPaused() {
		return false
	}
	m := pb.Message{}
	m.To = to

	// 从接收节点的Next的上一条数据获取term
	term, errt := r.raftLog.term(pr.Next - 1)

	// 获取接收节点的Next之后的entries，总和不超过maxMsgSize
	ents, erre := r.raftLog.entries(pr.Next, r.maxMsgSize)
	if len(ents) == 0 && !sendIfEmpty {
		return false
	}

	// 使用快照发送
	if errt != nil || erre != nil { // send snapshot if we failed to get term or entries
		// 如果前面过程中有错，那说明之前的数据都写到快照里了，尝试发送快照数据过去
		if !pr.RecentActive {
			// 如果该节点当前不可用
			r.logger.Debugf("ignore sending snapshot to %x since it is not recently active", to)
			return false
		}

		// 尝试发送快照
		m.Type = pb.MsgSnap

		// 返回最新的snapshot
		snapshot, err := r.raftLog.snapshot()
		if err != nil {
			if err == ErrSnapshotTemporarilyUnavailable {
				r.logger.Debugf("%x failed to send snapshot to %x because snapshot is temporarily unavailable", r.id, to)
				return false
			}
			panic(err) // TODO(bdarnell)
		}

		// 不能发送空快照
		if IsEmptySnap(snapshot) {
			panic("need non-empty snapshot")
		}
		m.Snapshot = snapshot

		// 可以连续赋值
		sindex, sterm := snapshot.Metadata.Index, snapshot.Metadata.Term
		r.logger.Debugf("%x [firstindex: %d, commit: %d] sent snapshot[index: %d, term: %d] to %x [%s]",
			r.id, r.raftLog.firstIndex(), r.raftLog.committed, sindex, sterm, to, pr)

		// 该节点进入接收快照的状态
		pr.becomeSnapshot(sindex)
		r.logger.Debugf("%x paused sending replication messages to %x [%s]", r.id, to, pr)

		// 否则就是简单的发送append消息
	} else {
		m.Type = pb.MsgApp
		m.Index = pr.Next - 1
		m.LogTerm = term
		m.Entries = ents
		// append消息需要告知当前leader的commit索引
		m.Commit = r.raftLog.committed

		// 如果发送过去的entries不为空
		if n := len(m.Entries); n != 0 {
			switch pr.State {
			// optimistically increase the next when in ProgressStateReplicate
			// 正常接收副本日志的状态
			case ProgressStateReplicate:
				// 如果该节点在接受副本的状态
				// 得到待发送数据的最后一条索引
				last := m.Entries[n-1].Index
				pr.optimisticUpdate(last)
				pr.ins.add(last)
			case ProgressStateProbe:
				// 在probe状态时，每次只能发送一条app消息
				pr.pause()
			default:
				r.logger.Panicf("%x is sending append in unhandled state %s", r.id, pr.State)
			}
		}
	}

	// 将持久状态发送到稳定的存储，
	r.send(m)
	return true
}

// sendHeartbeat sends a heartbeat RPC to the given peer.
// 往特定的peer发送心跳RPC
func (r *raft) sendHeartbeat(to uint64, ctx []byte) {
	// Attach the commit as min(to.matched, r.committed).
	// When the leader sends out heartbeat message,
	// the receiver(follower) might not be matched with the leader
	// or it might not have all the committed entries.
	// The leader MUST NOT forward the follower's commit to
	// an unmatched index.


	// commit index 是 需要发送过去的节点的match，当前leader的commited中的较小值
	commit := min(r.getProgress(to).Match, r.raftLog.committed)
	m := pb.Message{
		To:      to,
		Type:    pb.MsgHeartbeat,
		Commit:  commit,
		Context: ctx,
	}

	// 将持久状态发送到稳定的存储，
	r.send(m)
}


// 针对每个进程
func (r *raft) forEachProgress(f func(id uint64, pr *Progress)) {
	for id, pr := range r.prs {
		f(id, pr)
	}

	for id, pr := range r.learnerPrs {
		f(id, pr)
	}
}

// bcastAppend sends RPC, with entries to all peers that are not up-to-date
// according to the progress recorded in r.prs.

// 根据r.prs中记录的进程向所有未更新的follower发送带有entry的RPC
// 向所有follower发送append消息
func (r *raft) bcastAppend() {
	r.forEachProgress(func(id uint64, _ *Progress) {
		if id == r.id {
			return
		}

		r.sendAppend(id)
	})
}

// bcastHeartbeat sends RPC, without entries to all the peers.
//  向所有的follower发送心跳信息
func (r *raft) bcastHeartbeat() {
	lastCtx := r.readOnly.lastPendingRequestCtx()
	if len(lastCtx) == 0 {
		r.bcastHeartbeatWithCtx(nil)
	} else {
		r.bcastHeartbeatWithCtx([]byte(lastCtx))
	}
}

// 向所有的follower发送带有上下文的心跳信息
func (r *raft) bcastHeartbeatWithCtx(ctx []byte) {
	r.forEachProgress(func(id uint64, _ *Progress) {
		if id == r.id {
			return
		}
		r.sendHeartbeat(id, ctx)
	})
}

// maybeCommit attempts to advance the commit index. Returns true if
// the commit index changed (in which case the caller should call
// r.bcastAppend).

// maybeCommit尝试提高commit index。 如果提交索引已更改，则返回true（在这种情况下，调用方应调用r.bcastAppend）。
// 尝试commit当前的日志，如果commit日志索引发生变化了就返回true
func (r *raft) maybeCommit() bool {
	// Preserving matchBuf across calls is an optimization
	// used to avoid allocating a new slice on each call.

	// 跨调用保留matchBuf是一种优化，用于避免在每个调用上分配新的分片。
	if cap(r.matchBuf) < len(r.prs) {
		r.matchBuf = make(uint64Slice, len(r.prs))
	}
	mis := r.matchBuf[:len(r.prs)]
	idx := 0

	// 拿到当前所有节点的Match到数组中
	for _, p := range r.prs {
		mis[idx] = p.Match
		idx++
	}
	sort.Sort(mis)

	// 排列之后拿到中位数的Match，因为如果这个位置的Match对应的Term也等于当前的Term
	// 说明有过半的节点至少commit了mci这个索引的数据，这样leader就可以以这个索引进行commit了
	mci := mis[len(mis)-r.quorum()]

	// raft日志尝试commit
	return r.raftLog.maybeCommit(mci, r.Term)
}

// 重置raft的一些状态
func (r *raft) reset(term uint64) {
	if r.Term != term {
		//  如果是新的任期，那么保存任期号，同时将投票节点置空
		r.Term = term
		r.Vote = None
	}

	// 刚开始时设置为非lead节点
	r.lead = None

	r.electionElapsed = 0
	r.heartbeatElapsed = 0

	// 重置随机选举超时
	r.resetRandomizedElectionTimeout()

	// 中断lead转让流程
	r.abortLeaderTransfer()

	r.votes = make(map[uint64]bool)

	// 似乎对于非leader节点来说，重置progress数组状态没有太多的意义？
	r.forEachProgress(func(id uint64, pr *Progress) {
		*pr = Progress{Next: r.raftLog.lastIndex() + 1, ins: newInflights(r.maxInflight), IsLearner: pr.IsLearner}
		if id == r.id {
			pr.Match = r.raftLog.lastIndex()
		}
	})

	r.pendingConfIndex = 0
	r.uncommittedSize = 0
	r.readOnly = newReadOnly(r.readOnly.option)
}

// 批量append一堆entries
func (r *raft) appendEntry(es ...pb.Entry) (accepted bool) {
	li := r.raftLog.lastIndex()
	for i := range es {
		// 设置这些entries的Term以及index
		es[i].Term = r.Term

		// 当前这个entry在整个raft日志中的位置索引
		es[i].Index = li + 1 + uint64(i)
	}
	// Track the size of this uncommitted proposal.
	if !r.increaseUncommittedSize(es) {
		r.logger.Debugf(
			"%x appending new entries to log would exceed uncommitted entry size limit; dropping proposal",
			r.id,
		)
		// Drop the proposal.
		return false
	}

	// use latest "last" index after truncate/append
	// 批量追加entry，返回最后索引
	li = r.raftLog.append(es...)

	// 更新本节点的Next以及Match索引
	r.getProgress(r.id).maybeUpdate(li)


	// Regardless of maybeCommit's return, our caller will call bcastAppend.

	// append之后，尝试一下是否可以进行commit
	r.maybeCommit()
	return true
}

// tickElection is run by followers and candidates after r.electionTimeout.
// follower以及candidate的tick函数，在r.electionTimeout之后被调用

// tickElection的处理逻辑是给自己发送一个MsgHup的内部消息，Step函数看到这个消息后会调用campaign函数，进入竞选状态。

/*
只有在candidate或者follower状态下的节点，才有可能发起一个选举流程，而这两种状态的节点，其对应的tick函数都是raft.tickElection函数，
这个函数的主要流程是：

1) 将选举超时递增1。
2) 当选举超时到期，同时该节点又在集群中时，说明此时可以进行一轮新的选举。此时会向本节点发送HUP消息，这个消息最终会走到状态机函数raft.Step中进行处理。
 */
func (r *raft) tickElection() {
	r.electionElapsed++

	//  如果等待时间过长并且可以提升为领导人就发送MsgHup,开始下一个任期的选举,投票给自己
	if r.promotable() && r.pastElectionTimeout() {
		// 如果可以被提升为leader，同时选举时间也到了
		r.electionElapsed = 0

		// 发送HUP消息是为了重新开始选举
		r.Step(pb.Message{From: r.id, Type: pb.MsgHup})
	}
}

// tickHeartbeat is run by leaders to send a MsgBeat after r.heartbeatTimeout.
// tickHeartbeat是leader的tick函数
// leader在r.heartbeatTimeout后执行该函数，去发送MsgBeat
func (r *raft) tickHeartbeat() {
	r.heartbeatElapsed++
	r.electionElapsed++


	// 选举超时了，检查leader
	if r.electionElapsed >= r.electionTimeout {
		// 如果超过了选举时间
		r.electionElapsed = 0
		if r.checkQuorum {
			r.Step(pb.Message{From: r.id, Type: pb.MsgCheckQuorum})
		}
		// If current leader cannot transfer leadership in electionTimeout, it becomes leader again.
		if r.state == StateLeader && r.leadTransferee != None {
			// 当前在迁移leader的流程，但是过了选举超时新的leader还没有产生，那么旧的leader重新成为leader
			r.abortLeaderTransfer()
		}
	}

	if r.state != StateLeader {
		// 不是leader的就不用往下走了
		return
	}


	//  心跳超时
	if r.heartbeatElapsed >= r.heartbeatTimeout {
		// 向集群中其他节点发送广播消息
		r.heartbeatElapsed = 0
		// 尝试发送MsgBeat消息
		r.Step(pb.Message{From: r.id, Type: pb.MsgBeat})
	}
}


// 函数中的step和tick两个函数构建了raft 角色转换的状态机
func (r *raft) becomeFollower(term uint64, lead uint64) {
	r.step = stepFollower
	r.reset(term)
	r.tick = r.tickElection
	r.lead = lead
	r.state = StateFollower
	r.logger.Infof("%x became follower at term %d", r.id, r.Term)
}

// 不能从leader往candidate转换
func (r *raft) becomeCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> candidate]")
	}
	r.step = stepCandidate
	// 因为进入candidate状态，意味着需要重新进行选举了，所以reset的时候传入的是Term+1
	r.reset(r.Term + 1)
	r.tick = r.tickElection

	// 给自己投票
	r.Vote = r.id
	r.state = StateCandidate
	r.logger.Infof("%x became candidate at term %d", r.id, r.Term)
}

func (r *raft) becomePreCandidate() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateLeader {
		panic("invalid transition [leader -> pre-candidate]")
	}
	// Becoming a pre-candidate changes our step functions and state,
	// but doesn't change anything else. In particular it does not increase
	// r.Term or change r.Vote.

	// prevote不会递增term，也不会先进行投票，而是等prevote结果出来再进行决定
	r.step = stepCandidate
	r.votes = make(map[uint64]bool)
	r.tick = r.tickElection
	r.lead = None
	r.state = StatePreCandidate
	r.logger.Infof("%x became pre-candidate at term %d", r.id, r.Term)
}

/*
    而当一个节点刚成为leader的时候，如果没有提交过任何数据，那么在它所在的这个任期（term）内的commit索引当时是并不知道的，
    因此在成为leader之后，需要马上提交一个no-op的空日志，这样拿到该任期的第一个commit索引。
 */
func (r *raft) becomeLeader() {
	// TODO(xiangli) remove the panic when the raft implementation is stable
	if r.state == StateFollower {
		panic("invalid transition [follower -> leader]")
	}
	r.step = stepLeader
	r.reset(r.Term)
	r.tick = r.tickHeartbeat
	r.lead = r.id
	r.state = StateLeader
	// Followers enter replicate mode when they've been successfully probed
	// (perhaps after having received a snapshot as a result). The leader is
	// trivially in this state. Note that r.reset() has initialized this
	// progress with the last index already.

	// followers成功探测后（可能是在收到快照后）进入复制模式。
	// 领导者处于这种状态。 请注意，r.reset（）已使用最后一个索引初始化了此进度。
	r.prs[r.id].becomeReplicate()

	// Conservatively set the pendingConfIndex to the last index in the
	// log. There may or may not be a pending config change, but it's
	// safe to delay any future proposals until we commit all our
	// pending log entries, and scanning the entire tail of the log
	// could be expensive.

	// 保守地设置pendingConfIndex为日志中的最后一个索引。 可能有也可能没有挂起的配置更改，
	// 但是可以安全的推迟任何以后的提议，直到我们提交所有挂起的日志条目为止，并且扫描整个日志尾部可能会很昂贵。
	r.pendingConfIndex = r.raftLog.lastIndex()

	// 为什么成为leader之后需要传入一个空数据？

	// 称为leader之后，要发一条空数据，entry的data部分是空， 但是term是当前的term，已经index是 当前这个entry在整个raft日志中的位置索引
	emptyEnt := pb.Entry{Data: nil}
	if !r.appendEntry(emptyEnt) {
		// This won't happen because we just called reset() above.
		r.logger.Panic("empty entry was dropped")
	}


	// As a special case, don't count the initial empty entry towards the
	// uncommitted log quota. This is because we want to preserve the
	// behavior of allowing one entry larger than quota if the current
	// usage is zero.

	// 作为特殊情况，请勿将初始空条目计入未提交的日志配额。 这是因为我们要保留这样的行为：如果当前使用量为零，则允许一个条目大于配额。
	r.reduceUncommittedSize([]pb.Entry{emptyEnt})
	r.logger.Infof("%x became leader at term %d", r.id, r.Term)
}

// campaign则会调用becomeCandidate把自己切换到candidate模式，并递增Term值。然后再将自己的Term及日志信息发送给其他的节点，请求投票。
/*
否则进入campaign函数中进行选举：首先将任期号+1，然后广播给其他节点选举消息，带上的其它字段包括：
节点当前的最后一条日志索引（Index字段），
最后一条日志对应的任期号（LogTerm字段），
选举任期号（Term字段，即前面已经进行+1之后的任期号），
Context字段（目的是为了告知这一次是否是leader转让类需要强制进行选举的消息）
 */
func (r *raft) campaign(t CampaignType) {
	var term uint64
	var voteMsg pb.MessageType
	if t == campaignPreElection {
		r.becomePreCandidate()
		voteMsg = pb.MsgPreVote
		// PreVote RPCs are sent for the next term before we've incremented r.Term.
		term = r.Term + 1
	} else {
		r.becomeCandidate()
		voteMsg = pb.MsgVote
		term = r.Term
	}

	// 调用poll函数给自己投票，同时返回当前投票给本节点的节点数量
	if r.quorum() == r.poll(r.id, voteRespMsgType(voteMsg), true) {
		// We won the election after voting for ourselves (which must mean that
		// this is a single-node cluster). Advance to the next state.
		// 有半数投票，说明通过，切换到下一个状态
		if t == campaignPreElection {
			r.campaign(campaignElection)
		} else {

			// 如果给自己投票之后，刚好超过半数的通过，那么就成为新的leader
			r.becomeLeader()
		}
		return
	}

	// 向集群里的其他节点发送投票消息
	for id := range r.prs {
		if id == r.id {
			// 过滤掉自己
			continue
		}
		r.logger.Infof("%x [logterm: %d, index: %d] sent %s request to %x at term %d",
			r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), voteMsg, id, r.Term)

		var ctx []byte
		if t == campaignTransfer {
			ctx = []byte(t)
		}
		r.send(pb.Message{Term: term, To: id, Type: voteMsg, Index: r.raftLog.lastIndex(), LogTerm: r.raftLog.lastTerm(), Context: ctx})
	}
}

// 轮询集群中所有节点，返回一共有多少节点已经进行了投票
func (r *raft) poll(id uint64, t pb.MessageType, v bool) (granted int) {
	if v {
		r.logger.Infof("%x received %s from %x at term %d", r.id, t, id, r.Term)
	} else {
		r.logger.Infof("%x received %s rejection from %x at term %d", r.id, t, id, r.Term)
	}

	// 如果id没有投票过，那么更新id的投票情况
	if _, ok := r.votes[id]; !ok {
		r.votes[id] = v
	}

	// 计算下都有多少节点已经投票给自己了
	for _, vv := range r.votes {
		if vv {
			granted++
		}
	}
	return granted
}

// raft 的状态机
// 状态机函数，同样每个状态对应的状态机也不相同

/*
   Step的主要作用是处理不同的消息，所以以后当我们想知道raft对某种消息的处理逻辑时，到这里找就对了。在函数的最后，有个default语句，
   即所有上面不能处理的消息都落入这里，由一个小写的step函数处理，这个设计的原因是什么呢？
   其实是因为这里的raft也被实现为一个状态机，它的step属性是一个函数指针，根据当前节点的不同角色，指向不同的消息处理函数：
   stepLeader/stepFollower/stepCandidate。与它类似的还有一个tick
   函数指针，根据角色的不同，也会在tickHeartbeat和tickElection之间来回切换，分别用来触发定时心跳和选举检测。这里的函数指针感觉像实现了OOP里的多态。
 */

// 处理消息的入口
func (r *raft) Step(m pb.Message) error {
	// Handle the message term, which may result in our stepping down to a follower.
	// 处理信息term，可能导致回退到follower状态
	switch {
	case m.Term == 0:
		// local message
		// 来自本地的消息

	// 消息的Term大于节点当前的Term
	/*
	   首先该函数会判断msg.Term是否大于本节点的Term，如果消息的任期号更大则说明是一次新的选举。这种情况下将根据msg.
	   Context是否等于“CampaignTransfer”字符串来确定是不是一次由于leader迁移导致的强制选举过程。同时也会根据当前的
	   electionElapsed是否小于electionTimeout来确定是否还在租约期以内。如果既不是强制leader选举又在租约期以内，
	   那么节点将忽略该消息的处理，在论文4.2.3部分论述这样做的原因，是为了避免已经离开集群的节点在不知道自己已经不在集群内的情况下，
	   仍然频繁的向集群内节点发起选举导致耗时在这种无效的选举流程中。
	   如果以上检查流程通过了，说明可以进行选举了，
	   如果消息类型还不是MsgPreVote类型，那么此时节点会切换到follower状态且认为发送消息过来的节点msg.From是新的leader。
	 */
	case m.Term > r.Term:
		if m.Type == pb.MsgVote || m.Type == pb.MsgPreVote {

			// 如果收到的是投票类消息
			// 当context为campaignTransfer时表示强制要求进行竞选
			force := bytes.Equal(m.Context, []byte(campaignTransfer))

			// 是否在租约期以内
			inLease := r.checkQuorum && r.lead != None && r.electionElapsed < r.electionTimeout
			if !force && inLease {
				// If a server receives a RequestVote request within the minimum election timeout
				// of hearing from a current leader, it does not update its term or grant its vote

				// 如果非强制，而且又在租约期以内，就不做任何处理
				// 非强制又在租约期内可以忽略选举消息，见论文的4.2.3，这是为了阻止已经离开集群的节点再次发起投票请求
				r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] ignored %s from %x [logterm: %d, index: %d] at term %d: lease is not expired (remaining ticks: %d)",
					r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term, r.electionTimeout-r.electionElapsed)
				return nil
			}
		}
		switch {
		case m.Type == pb.MsgPreVote:
			// Never change our term in response to a PreVote
			// 在应答一个prevote消息时不对任期term做修改
		case m.Type == pb.MsgPreVoteResp && !m.Reject:
			// We send pre-vote requests with a term in our future. If the
			// pre-vote is granted, we will increment our term when we get a
			// quorum. If it is not, the term comes from the node that
			// rejected our vote so we should become a follower at the new
			// term.
		default:
			// 变成follower状态
			r.logger.Infof("%x [term: %d] received a %s message with higher term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
			if m.Type == pb.MsgApp || m.Type == pb.MsgHeartbeat || m.Type == pb.MsgSnap {
				r.becomeFollower(m.Term, m.From)
			} else {
				r.becomeFollower(m.Term, None)   // 收到消息的Term 大于自身的，那么转为follower
			}
		}

	case m.Term < r.Term:
		// 消息的Term小于节点自身的Term，同时消息类型是心跳消息或者是append消息
		if (r.checkQuorum || r.preVote) && (m.Type == pb.MsgHeartbeat || m.Type == pb.MsgApp) {
			// We have received messages from a leader at a lower term. It is possible
			// that these messages were simply delayed in the network, but this could
			// also mean that this node has advanced its term number during a network
			// partition, and it is now unable to either win an election or to rejoin
			// the majority on the old term. If checkQuorum is false, this will be
			// handled by incrementing term numbers in response to MsgVote with a
			// higher term, but if checkQuorum is true we may not advance the term on
			// MsgVote and must generate other messages to advance the term. The net
			// result of these two features is to minimize the disruption caused by
			// nodes that have been removed from the cluster's configuration: a
			// removed node will send MsgVotes (or MsgPreVotes) which will be ignored,
			// but it will not receive MsgApp or MsgHeartbeat, so it will not create
			// disruptive term increases, by notifying leader of this node's activeness.
			// The above comments also true for Pre-Vote
			//
			// When follower gets isolated, it soon starts an election ending
			// up with a higher term than leader, although it won't receive enough
			// votes to win the election. When it regains connectivity, this response
			// with "pb.MsgAppResp" of higher term would force leader to step down.
			// However, this disruption is inevitable to free this stuck node with
			// fresh election. This can be prevented with Pre-Vote phase.

			/*
			  我们收到一位领导者的更低term的信息。这些消息可能只是在网络中被延迟了，但这也可能意味着该节点在网络分区期间已经提高了其任期编号，
			   现在它无法赢得选举或重新加入旧任期的多数席位。如果checkQuorum为false，这将通过增加具有较高term的MsgVote的term编号来解决，
			   但是如果checkQuorum为true，我们可能不会在MsgVote上推进该term，并且必须生成其他消息以使term超前。这两个功能的最终结果是最
			   大程度地减少了从群集配置中删除的节点所造成的破坏：删除的节点将发送MsgVotes（或MsgPreVotes），该消息将被忽略，
			   但不会接收MsgApp或MsgHeartbeat，因此通过通知负责人该节点的活动状态，不会造成破坏性的期限增加。以上评论也适用于预投票

			    当追随者被孤立时，它很快就会开始选举，其任期要比领导者的任期长，尽管它不会获得足够的选票来赢得选举。
			   当恢复连接时，带有较高期限的“ pb.MsgAppResp”的响应将迫使领导者下台。但是，这种中断是不可避免的，以便通过重新选举来释放此卡住的节点。
			    这可以通过预投票阶段来防止。
			 */

			// 收到了一个节点发送过来的更小的term消息。这种情况可能是因为消息的网络延时导致，
			// 但是也可能因为该节点由于网络分区导致了它递增了term到一个新的任期。
			// 这种情况下该节点不能赢得一次选举，也不能使用旧的任期号重新再加入集群中。
			// 如果checkQurom为false，这种情况可以使用递增任期号应答来处理。
			// 但是如果checkQurom为True
			// 此时收到了一个更小的term的节点发出的HB或者APP消息，于是应答一个appresp消息，试图纠正它的状态


			// 比自身Term小的很可能是delay的消息
			r.send(pb.Message{To: m.From, Type: pb.MsgAppResp})
		} else if m.Type == pb.MsgPreVote {
			// Before Pre-Vote enable, there may have candidate with higher term,
			// but less log. After update to Pre-Vote, the cluster may deadlock if
			// we drop messages with a lower term.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: pb.MsgPreVoteResp, Reject: true})
		} else {
			// ignore other cases
			// 除了上面的情况以外，忽略任何term小于当前节点所在任期号的消息
			r.logger.Infof("%x [term: %d] ignored a %s message with lower term from %x [term: %d]",
				r.id, r.Term, m.Type, m.From, m.Term)
		}
		// 在消息的term小于当前节点的term时，不往下处理直接返回了
		return nil
	}

	switch m.Type {
	// Step函数看到这个消息后会调用campaign函数，进入竞选状态。
	case pb.MsgHup:   // Hup 消息用于触发选举
		// 收到HUP消息，说明准备进行选举
		if r.state != StateLeader {
			// 当前不是leader
			// 取出[applied+1,committed+1]之间的消息，即得到还未进行applied的日志列表
			ents, err := r.raftLog.slice(r.raftLog.applied+1, r.raftLog.committed+1, noLimit)
			if err != nil {
				r.logger.Panicf("unexpected error getting unapplied entries (%v)", err)
			}
			// 如果其中有config消息，并且commited > applied，说明当前还有没有apply的config消息，这种情况下不能开始投票
			if n := numOfPendingConf(ents); n != 0 && r.raftLog.committed > r.raftLog.applied {
				r.logger.Warningf("%x cannot campaign at term %d since there are still %d pending configuration changes to apply", r.id, r.Term, n)
				return nil
			}

			r.logger.Infof("%x is starting a new election at term %d", r.id, r.Term)
			// 进行选举
			if r.preVote {
				r.campaign(campaignPreElection)
			} else {
				r.campaign(campaignElection)
			}
		} else {
			r.logger.Debugf("%x ignoring MsgHup because already leader", r.id)
		}

	// 	其他节点在接受到这个请求后，会首先比较接收到的Term是不是比自己的大，以及接受到的日志信息是不是比自己的要新，从而决定是否投票。
	// 	这个逻辑我们还是可以从Step函数中找到
	/*
	   在raft.Step函数的后面，会判断消息类型是MsgVote或者MsgPreVote来进一步进行处理。其判断条件是以下两个条件同时成立：
	   当前没有给任何节点进行过投票（r.Vote == None ），或者消息的任期号更大（m.Term > r.Term ），
	   或者是之前已经投过票的节点（r.Vote == m.From)）。这个条件是检查是否可以还能给该节点投票。

	   同时该节点的日志数据是最新的（r.raftLog.isUpToDate(m.Index,m.LogTerm) ）
	   这个条件是检查这个节点上的日志数据是否足够的新。

	   只有在满足以上两个条件的情况下，节点才投票给这个消息节点，将修改raft.Vote为消息发送者ID。
	   如果不满足条件，将应答msg.Reject=true，拒绝该节点的投票消息。
	 */
	case pb.MsgVote, pb.MsgPreVote:
		if r.isLearner {
			// TODO: learner may need to vote, in case of node down when confchange.
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] ignored %s from %x [logterm: %d, index: %d] at term %d: learner can not vote",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			return nil
		}
		// We can vote if this is a repeat of a vote we've already cast...
		// 重复投票
		canVote := r.Vote == m.From ||
			// ...we haven't voted and we don't think there's a leader yet in this term...
			// 没有投过票，不认为存在leader
			(r.Vote == None && r.lead == None) ||
			// ...or this is a PreVote for a future term...
			// 存在一个PreVote
			(m.Type == pb.MsgPreVote && m.Term > r.Term)
		// ...and we believe the candidate is up to date.
		// 候选者是最新的
		if canVote && r.raftLog.isUpToDate(m.Index, m.LogTerm) {

			// 如果当前没有给任何节点投票（r.Vote == None）或者投票的节点term大于本节点的（m.Term > r.Term）
			// 或者是之前已经投票的节点（r.Vote == m.From）
			// 同时还满足该节点的消息是最新的（r.raftLog.isUpToDate(m.Index, m.LogTerm)），那么就接收这个节点的投票
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] cast %s for %x [logterm: %d, index: %d] at term %d",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			// When responding to Msg{Pre,}Vote messages we include the term
			// from the message, not the local term. To see why consider the
			// case where a single node was previously partitioned away and
			// it's local term is now of date. If we include the local term
			// (recall that for pre-votes we don't update the local term), the
			// (pre-)campaigning node on the other end will proceed to ignore
			// the message (it ignores all out of date messages).
			// The term in the original message and current local term are the
			// same in the case of regular votes, but different for pre-votes.

			// 在回应Msg {Pre} Vote消息时，我们包括消息中的term，而不是本地term
			// 要了解为什么要考虑以前将单个节点分割开并且其本地term 为最新的情况。
			// 如果我们包含本地term （重新调用pre-vote 我们不会更新本地term），（pre)竞选节点将继续忽略该消息（它将忽略所有过期消息）
			// 在常规投票中，原始消息中的term和当前的本地term相同，但预投票中的term不同

			// 投票
			r.send(pb.Message{To: m.From, Term: m.Term, Type: voteRespMsgType(m.Type)})

			if m.Type == pb.MsgVote {
				// Only record real votes.
				// 保存下来给哪个节点投票了
				r.electionElapsed = 0

				// 保存给哪个节点投过票
				// 只有在满足以上两个条件的情况下，节点才投票给这个消息节点，将修改raft.Vote为消息发送者ID
				r.Vote = m.From
			}
		} else {
			// 否则拒绝投票
			r.logger.Infof("%x [logterm: %d, index: %d, vote: %x] rejected %s from %x [logterm: %d, index: %d] at term %d",
				r.id, r.raftLog.lastTerm(), r.raftLog.lastIndex(), r.Vote, m.Type, m.From, m.LogTerm, m.Index, r.Term)
			r.send(pb.Message{To: m.From, Term: r.Term, Type: voteRespMsgType(m.Type), Reject: true})
		}

	default:
		// 其他情况下进入各种状态下自己定制的状态机函数
		// 消息处理函数Step无法直接处理这个消息，它会调用那个小写的step函数，来根据当前的状态进行处理
		err := r.step(r, m)
		if err != nil {
			return err
		}
	}
	return nil
}

type stepFunc func(r *raft, m pb.Message) error

// leader的状态机
func stepLeader(r *raft, m pb.Message) error {
	// These message types do not require any progress for m.From.

	// 信息类型不要求m.From的进程
	switch m.Type {
	case pb.MsgBeat:
		// 广播HB消息
		r.bcastHeartbeat()
		return nil


	case pb.MsgCheckQuorum:
	    // 检查集群可用性
		if !r.checkQuorumActive() {
			//  如果超过半数的服务器没有活着, 变成follower状态
			r.logger.Warningf("%x stepped down to follower since quorum is not active", r.id)
			r.becomeFollower(r.Term, None)
		}
		return nil

	// Leader收到这个消息后（不管是follower转发过来的还是自己内部产生的）会有两步操作：
	// 将这个消息添加到自己的log里
	// 向其他follower广播这个消息
	case pb.MsgProp:
		// 不能提交空数据
		if len(m.Entries) == 0 {
			r.logger.Panicf("%x stepped empty MsgProp", r.id)
		}
		// 检查是否在集群中
		if _, ok := r.prs[r.id]; !ok {
			// If we are not currently a member of the range (i.e. this node
			// was removed from the configuration while serving as leader),
			// drop any new proposals.

			// 这里检查本节点是否还在集群以内，如果已经不在集群中了，不处理该消息直接返回。
			// 这种情况出现在本节点已经通过配置变化被移除出了集群的场景。
			return ErrProposalDropped
		}

		// 当前正在转换leader过程中，不能提交
		if r.leadTransferee != None {
			r.logger.Debugf("%x [term %d] transfer leadership to %x is in progress; dropping proposal", r.id, r.Term, r.leadTransferee)
			return ErrProposalDropped
		}

		for i, e := range m.Entries {
			if e.Type == pb.EntryConfChange {
				if r.pendingConfIndex > r.raftLog.applied {
					// 如果当前为还有没有处理的配置变化请求，则其他配置变化的数据暂时忽略
					r.logger.Infof("propose conf %s ignored since pending unapplied configuration [index %d, applied %d]",
						e.String(), r.pendingConfIndex, r.raftLog.applied)
					// 将这个数据置空
					m.Entries[i] = pb.Entry{Type: pb.EntryNormal}
				} else {
					// 置位有还没有处理的配置变化数据
					r.pendingConfIndex = r.raftLog.lastIndex() + uint64(i) + 1
				}
			}
		}

		// 添加消息数据到自己的log中
		if !r.appendEntry(m.Entries...) {
			return ErrProposalDropped
		}

		// 向集群其他follower广播这个消息
		r.bcastAppend()
		return nil

    /*
      而leader收到不论是本节点还是由其他follower发来的MsgReadIndex消息，其处理都是：

	  a. 首先如果该leader在成为新的leader之后没有提交过任何值，那么会直接返回不做处理。
	  b. 调用r.readOnly.addRequest(r.raftLog.committed, m)保存该MsgreadIndex请求到来时的commit索引。
	  c. r.bcastHeartbeatWithCtx(m.Entries[0].Data)，向集群中所有其他节点广播一个心跳消息MsgHeartbeat，并且在其中带上该读请求的唯一标识。
	  d. follower在收到leader发送过来的MsgHeartbeat，将应答MsgHeartbeatResp消息，并且如果MsgHeartbeat消息中有ctx数据，
         MsgHeartbeatResp消息将原样返回这个ctx数据。
	  e. leader在接收到MsgHeartbeatResp消息后，如果其中有ctx字段，说明该MsgHeartbeatResp消息对应的MsgHeartbeat消息，是收到
         ReadIndex时leader消息为了确认自己还是集群leader发送的心跳消息。首先会调用r.readOnly.recvAck(m)函数，根据消息中的ctx字段，
         到全局的pendingReadIndex中查找是否有保存该ctx的带处理的readIndex请求，如果有就在acks map中记录下该follower已经进行了应答。

	  f. 当ack数量超过了集群半数时，意味着该leader仍然还是集群的leader，此时调用r.readOnly.advance(m)函数，
         将该readIndex之前的所有readIndex请求都认为是已经成功进行确认的了，所有成功确认的readIndex请求，将会加入到readStates数组中，
         同时leader也会向follower发送MsgReadIndexResp。
      g. follower收到MsgReadIndexResp消息时，同样也会更新自己的readStates数组信息。
      h. readStates数组的信息，将做为ready结构体的信息更新给上层的raft协议库的使用者
     */
	case pb.MsgReadIndex:
		if r.quorum() > 1 {
			// 这个表达式用于判断commttied log索引对应的term是否与当前term不相等，
			// 如果不相等说明这是一个新的leader，而在它当选之后还没有提交任何数据
			if r.raftLog.zeroTermOnErrCompacted(r.raftLog.term(r.raftLog.committed)) != r.Term {
				// Reject read only request when this leader has not committed any log entry at its term.
				return nil
			}

			// thinking: use an interally defined context instead of the user given context.
			// We can express this in terms of the term and index instead of a user-supplied value.
			// This would allow multiple reads to piggyback on the same message.
			switch r.readOnly.option {
			case ReadOnlySafe:
				// 把读请求到来时的committed索引保存下来
				r.readOnly.addRequest(r.raftLog.committed, m)
				// 广播消息出去，其中消息的CTX是该读请求的唯一标识
				// 在应答是Context要原样返回，将使用这个ctx操作readOnly相关数据
				r.bcastHeartbeatWithCtx(m.Entries[0].Data)
			case ReadOnlyLeaseBased:
				ri := r.raftLog.committed
				if m.From == None || m.From == r.id { // from local member
					r.readStates = append(r.readStates, ReadState{Index: r.raftLog.committed, RequestCtx: m.Entries[0].Data})
				} else {
					r.send(pb.Message{To: m.From, Type: pb.MsgReadIndexResp, Index: ri, Entries: m.Entries})
				}
			}
		} else {
			r.readStates = append(r.readStates, ReadState{Index: r.raftLog.committed, RequestCtx: m.Entries[0].Data})
		}

		return nil
	}

	// All other message types require a progress for m.From (pr).
	// 检查消息发送者当前是否在集群中
	pr := r.getProgress(m.From)
	if pr == nil {
		r.logger.Debugf("%x no progress available for %x", r.id, m.From)
		return nil
	}


	switch m.Type {
	// 对append消息的应答， 当leader接收到该消息，说明了follower已经将数据追加到log中了
	case pb.MsgAppResp:

		// 置位该节点当前是活跃的
		pr.RecentActive = true

		// 如果拒绝了append消息，说明term、index不匹配
		if m.Reject {
			r.logger.Debugf("%x received msgApp rejection(lastindex: %d) from %x for index %d",
				r.id, m.RejectHint, m.From, m.Index)

			// reject hint带来的是拒绝该app请求的节点其最大日志的索引
			// 尝试回退关于该节点的Match、Next索引
			if pr.maybeDecrTo(m.Index, m.RejectHint) {
				r.logger.Debugf("%x decreased progress of %x to [%s]", r.id, m.From, pr)
				if pr.State == ProgressStateReplicate {
					pr.becomeProbe()
				}

				// 再次发送append消息
				r.sendAppend(m.From)
			}
		} else {
			// 通过该append请求
			oldPaused := pr.IsPaused()
			// 如果该节点的索引发生了更新
			if pr.maybeUpdate(m.Index) {
				switch {
				case pr.State == ProgressStateProbe:
					// 如果当前该节点在探测状态，切换到可以接收副本状态
					pr.becomeReplicate()
				case pr.State == ProgressStateSnapshot && pr.needSnapshotAbort():
					r.logger.Debugf("%x snapshot aborted, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
					// Transition back to replicating state via probing state
					// (which takes the snapshot into account). If we didn't
					// move to replicating state, that would only happen with
					// the next round of appends (but there may not be a next
					// round for a while, exposing an inconsistent RaftStatus).

					// 如果当前该接在在接受快照状态，而且已经快照数据同步完成了
					// 切换到探测状态
					pr.becomeProbe()
					pr.becomeReplicate()
				case pr.State == ProgressStateReplicate:
					// 如果当前该节点在接收副本状态，因为通过了该请求，所以滑动窗口可以释放在这之前的索引了
					pr.ins.freeTo(m.Index)
				}

				// 当leader确认已经有足够多的follower接受了这个log后，它首先会commit这个log，
				// 然后再广播一次，告诉别人它的commit状态。这里的实现就有点像两阶段提交了
				if r.maybeCommit() {
					// 如果可以commit日志，那么广播append消息
					r.bcastAppend()

				} else if oldPaused {
					// If we were paused before, this node may be missing the
					// latest commit index, so send it.
					// 如果该节点之前状态是暂停，继续发送append消息给它
					r.sendAppend(m.From)
				}

				// We've updated flow control information above, which may
				// allow us to send multiple (size-limited) in-flight messages
				// at once (such as when transitioning from probe to
				// replicate, or when freeTo() covers multiple messages). If
				// we have more entries to send, send as many messages as we
				// can (without sending empty messages for the commit index)
				// 我们已经更新了上面的流控制信息，这可能使我们可以立即发送多个（大小受限的）运行中消息（例如，从探针过渡到复制时，或者当freeTo（）覆盖多个消息时）。
				// 如果我们有更多条目要发送，请发送尽可能多的消息（不发送提交索引的空消息）
				for r.maybeSendAppend(m.From, false) {
				}
				// Transfer leadership is in progress.
				if m.From == r.leadTransferee && pr.Match == r.raftLog.lastIndex() {
					// 要迁移过去的新leader，其日志已经追上了旧的leader
					r.logger.Infof("%x sent MsgTimeoutNow to %x after received MsgAppResp", r.id, m.From)

					// 发送超时消息
					r.sendTimeoutNow(m.From)
				}
			}
		}

	// 	leader在接收到MsgHeartbeatResp消息后，如果其中有ctx字段，说明该MsgHeartbeatResp消息对应的MsgHeartbeat消息，
	// 	是收到ReadIndex时leader消息为了确认自己还是集群leader发送的心跳消息。
	// 	首先会调用r.readOnly.recvAck(m)函数，根据消息中的ctx字段，到全局的pendingReadIndex中查找是否有保存该ctx的待
	// 	处理的readIndex请求，如果有就在acks map中记录下该follower已经进行了应答。

	case pb.MsgHeartbeatResp:
		// 该节点当前处于活跃状态
		pr.RecentActive = true
		// 这里调用resume是因为当前可能处于probe状态，而这个状态在两个heartbeat消息的间隔期只能收一条同步日志消息，因此在收到HB消息时就停止pause标记
		pr.resume()

		// free one slot for the full inflights window to allow progress.
		// 释放一个插槽
		if pr.State == ProgressStateReplicate && pr.ins.full() {
			pr.ins.freeFirstOne()
		}

		// 该节点的match节点小于当前最大日志索引，可能已经过期了，尝试添加日志
		if pr.Match < r.raftLog.lastIndex() {
			r.sendAppend(m.From)
		}

		// 只有readonly safe方案，才会继续往下走
		if r.readOnly.option != ReadOnlySafe || len(m.Context) == 0 {
			return nil
		}

		// 收到应答调用recvAck函数返回当前针对该消息已经应答的节点数量
		ackCount := r.readOnly.recvAck(m)
		if ackCount < r.quorum() {
			// 小于集群半数以上就返回不往下走了
			return nil
		}

		// 调用advance函数尝试丢弃已经被确认的read index状态
		/*
		   当ack数量超过了集群半数时，意味着该leader仍然还是集群的leader，此时调用r.readOnly.advance(m)函数，
		   将该readIndex之前的所有readIndex请求都认为是已经成功进行确认的了，所有成功确认的readIndex请求，将
		   会加入到readStates数组中，同时leader也会向follower发送MsgReadIndexResp。
		 */
		rss := r.readOnly.advance(m)

		// 遍历准备被丢弃的readindex状态
		for _, rs := range rss {
			req := rs.req
			if req.From == None || req.From == r.id { // from local member
			    // 如果来自本地
			        // r.readStates = append(r.readStates, ReadState{Index: rs.index, RequestCtx: req.Entries[0].Data})
			} else {
				// 否则就是来自外部，需要应答
				r.send(pb.Message{To: req.From, Type: pb.MsgReadIndexResp, Index: rs.index, Entries: req.Entries})
			}
		}

	case pb.MsgSnapStatus:
		// 当前该节点状态已经不是在接受快照的状态了，直接返回
		if pr.State != ProgressStateSnapshot {
			return nil
		}
		if !m.Reject {
			// 接收快照成功
			pr.becomeProbe()
			r.logger.Debugf("%x snapshot succeeded, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		} else {
			// 接收快照失败
			pr.snapshotFailure()
			pr.becomeProbe()
			r.logger.Debugf("%x snapshot failed, resumed sending replication messages to %x [%s]", r.id, m.From, pr)
		}
		// If snapshot finish, wait for the msgAppResp from the remote node before sending
		// out the next msgApp.
		// If snapshot failure, wait for a heartbeat interval before next try
		// 先暂停等待下一次被唤醒
		pr.pause()

	case pb.MsgUnreachable:
		// During optimistic replication, if the remote becomes unreachable,
		// there is huge probability that a MsgApp is lost.
		if pr.State == ProgressStateReplicate {
			pr.becomeProbe()
		}
		r.logger.Debugf("%x failed to send message to %x because it is unreachable [%s]", r.id, m.From, pr)

	case pb.MsgTransferLeader:
		if pr.IsLearner {
			r.logger.Debugf("%x is learner. Ignored transferring leadership", r.id)
			return nil
		}

		// 之前的leader转让流程
		//leader在收到这类消息时，是以下的处理流程。
		//1)  如果当前的raft.leadTransferee成员不为空，说明有正在进行的leader迁移流程。此时会判断是否与这次迁移是同样的新leader ID
		//    如果是则忽略该消息直接返回；否则将终止前面还没有完毕的迁移流程。

		//2)  如果这次迁移过去的新节点，就是当前的leader ID，也直接返回不进行处理。

		//3)  到了这一步就是正式开始这一次的迁移leader流程了，一个节点能成为一个集群的leader，其必要条件是上面的日志与当前leader
		//    的一样多，所以这里会判断是否满足这个条件，如果满足那么发送MsgTimeoutNow消息给新的leader通知该节点进行leader迁移，
		//    否则就先进行日志同步操作让新的leader追上旧leader的日志数据。
		leadTransferee := m.From
		lastLeadTransferee := r.leadTransferee
		if lastLeadTransferee != None {
			// 判断是否已经有相同节点的leader转让流程在进行中
			if lastLeadTransferee == leadTransferee {
				r.logger.Infof("%x [term %d] transfer leadership to %x is in progress, ignores request to same node %x",
					r.id, r.Term, leadTransferee, leadTransferee)
				// 如果是，直接返回
				return nil
			}

			// 否则中断之前的转让流程
			r.abortLeaderTransfer()
			r.logger.Infof("%x [term %d] abort previous transferring leadership to %x", r.id, r.Term, lastLeadTransferee)
		}

		// 判断是否转让过来的leader是否本节点，如果是也直接返回，因为本节点已经是leader了
		if leadTransferee == r.id {
			r.logger.Debugf("%x is already leader. Ignored transferring leadership to self", r.id)
			return nil
		}

		// Transfer leadership to third party.
		r.logger.Infof("%x [term %d] starts to transfer leadership to %x", r.id, r.Term, leadTransferee)

		// Transfer leadership should be finished in one electionTimeout, so reset r.electionElapsed.
		// 转让leader应该在一次electionTimeout时间内完成
		r.electionElapsed = 0
		r.leadTransferee = leadTransferee
		if pr.Match == r.raftLog.lastIndex() {
			// 如果日志已经匹配了，那么就发送timeoutnow协议过去
			r.sendTimeoutNow(leadTransferee)
			r.logger.Infof("%x sends MsgTimeoutNow to %x immediately as %x already has up-to-date log", r.id, leadTransferee, leadTransferee)
		} else {
			// 否则继续追加日志
			r.sendAppend(leadTransferee)
		}
	}
	return nil
}

// stepCandidate is shared by StateCandidate and StatePreCandidate; the difference is
// whether they respond to MsgVoteResp or MsgPreVoteResp.

// stepCandidate由StateCandidate和StatePreCandidate共享； 区别在于它们是响应MsgVoteResp还是MsgPreVoteResp。


// 候选者的step函数
func stepCandidate(r *raft, m pb.Message) error {
	// Only handle vote responses corresponding to our candidacy (while in
	// StateCandidate, we may get stale MsgPreVoteResp messages in this term from
	// our pre-candidate state).

	// 仅处理与我们的候选资格相对应的投票响应（在StateCandidate中, 我们从pre-candidate状态获的term中可能得到陈旧的MsgPreVoteResp消息）。
	var myVoteRespType pb.MessageType
	if r.state == StatePreCandidate {
		myVoteRespType = pb.MsgPreVoteResp
	} else {
		myVoteRespType = pb.MsgVoteResp
	}


	// 以下转换成follower状态时，为什么不判断消息的term是否至少大于当前节点的term？？？
	switch m.Type {
	case pb.MsgProp:
		// 当前没有leader，所以忽略掉提交的消息
		r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
		return ErrProposalDropped

	case pb.MsgApp:
		// 收到append消息，说明集群中已经有leader，转换为follower
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		// 添加日志
		r.handleAppendEntries(m)

	case pb.MsgHeartbeat:
		// 收到HB消息，说明集群已经有leader，转换为follower
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleHeartbeat(m)


	case pb.MsgSnap:
		// 收到快照消息，说明集群已经有leader，转换为follower
		r.becomeFollower(m.Term, m.From) // always m.Term == r.Term
		r.handleSnapshot(m)

	// 最后当candidate节点收到投票回复后，就会计算收到的选票数目是否大于所有节点数的一半，
	// 如果大于则自己成为leader，并昭告天下，否则将自己置为follower

	/*
	  来看节点收到投票应答数据之后的处理。
	  1) 节点调用raft.poll函数，其中传入msg.Reject参数表示发送者是否同意这次选举，根据这些来计算当前集群中有多少节点给这次选举投了同意票。
	  2) 如果有半数的节点同意了，如果选举类型是PreVote，那么进行Vote状态正式进行一轮选举；
	     否则该节点就成为了新的leader，调用raft.becomeLeader函数切换状态，然后开始同步日志数据给集群中其他节点了。
	  3) 而如果半数以上的节点没有同意，那么重新切换到follower状态。
	 */
	case myVoteRespType:

		// 计算当前集群中有多少节点给自己投了票
		gr := r.poll(m.From, m.Type, !m.Reject)
		r.logger.Infof("%x [quorum:%d] has received %d %s votes and %d vote rejections", r.id, r.quorum(), gr, m.Type, len(r.votes)-gr)
		switch r.quorum() {
		// 如果进行投票的节点数量正好是半数以上节点数量
		case gr:
			if r.state == StatePreCandidate {
				r.campaign(campaignElection)
			} else {
				// 变成leader
				r.becomeLeader()
				r.bcastAppend()
			}

		case len(r.votes) - gr:  // 如果是半数以上节点拒绝了投票
			// pb.MsgPreVoteResp contains future term of pre-candidate
			// m.Term > r.Term; reuse r.Term
			// 变成follower
			r.becomeFollower(r.Term, None)
		}

	case pb.MsgTimeoutNow:
		// candidate收到timeout指令忽略不处理
		r.logger.Debugf("%x [term %d state %v] ignored MsgTimeoutNow from %x", r.id, r.Term, r.state, m.From)
	}
	return nil
}


// follower的状态机函数
func stepFollower(r *raft, m pb.Message) error {
	switch m.Type {

	// 如果当前是follower，那它会把这个消息转发给leader。
	case pb.MsgProp:
		// 本节点提交的值
		if r.lead == None {
			// 没有leader则提交失败，忽略
			r.logger.Infof("%x no leader at term %d; dropping proposal", r.id, r.Term)
			return ErrProposalDropped

		// DisableProposalForwarding设置为true意味着followers将丢弃建议，而不是将其转发给领导者。
		} else if r.disableProposalForwarding {
			r.logger.Infof("%x not forwarding to leader %x at term %d; dropping proposal", r.id, r.lead, r.Term)
			return ErrProposalDropped
		}

		// 向leader进行redirect,  也就是将消息转发给leader
		m.To = r.lead
		r.send(m)

	// 从leader中收到追加数据到log中
	case pb.MsgApp:
		// append消息
		// 收到leader的app消息，重置选举tick计时器，因为这样证明leader还存活
		r.electionElapsed = 0
		r.lead = m.From
		r.handleAppendEntries(m)

	// follower在收到leader发送过来的MsgHeartbeat，将应答MsgHeartbeatResp消息，并且如果MsgHeartbeat消息中有ctx数据，
	// MsgHeartbeatResp消息将原样返回这个ctx数据。
	case pb.MsgHeartbeat:
		// HB消息
		// 收到leader的HB消息，重置选举tick计时器，因为这样证明leader还存活
		r.electionElapsed = 0
		r.lead = m.From
		r.handleHeartbeat(m)

	case pb.MsgSnap:
		// 快照消息
		r.electionElapsed = 0
		r.lead = m.From
		r.handleSnapshot(m)

	case pb.MsgTransferLeader:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping leader transfer msg", r.id, r.Term)
			return nil
		}
		// 向leader转发leader转让消息
		m.To = r.lead
		r.send(m)

	case pb.MsgTimeoutNow:  // 如果本节点可以提升为leader，那么就发起新一轮的竞选
		if r.promotable() {
			r.logger.Infof("%x [term %d] received MsgTimeoutNow from %x and starts an election to get leadership.", r.id, r.Term, m.From)
			// Leadership transfers never use pre-vote even if r.preVote is true; we
			// know we are not recovering from a partition so there is no need for the
			// extra round trip.
			// timeout消息用在leader转让中，所以不需要prevote即使开了这个选项
			r.campaign(campaignTransfer)
		} else {
			r.logger.Infof("%x received MsgTimeoutNow from %x but is not promotable", r.id, m.From)
		}

	// follower将向leader直接转发MsgReadIndex消息
	case pb.MsgReadIndex:
		if r.lead == None {
			r.logger.Infof("%x no leader at term %d; dropping index reading msg", r.id, r.Term)
			return nil
		}
		// 向leader转发此类型消息
		m.To = r.lead
		r.send(m)


	//  follower收到MsgReadIndexResp消息时，同样也会更新自己的readStates数组信息。
	case pb.MsgReadIndexResp:
		if len(m.Entries) != 1 {
			r.logger.Errorf("%x invalid format of MsgReadIndexResp from %x, entries count: %d", r.id, m.From, len(m.Entries))
			return nil
		}
		// 更新readstates数组
		r.readStates = append(r.readStates, ReadState{Index: m.Index, RequestCtx: m.Entries[0].Data})
	}
	return nil
}

// 添加日志
func (r *raft) handleAppendEntries(m pb.Message) {
	// 先检查消息消息的合法性
	if m.Index < r.raftLog.committed {
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
		return
	}

	// 尝试添加到日志模块中
	if mlastIndex, ok := r.raftLog.maybeAppend(m.Index, m.LogTerm, m.Commit, m.Entries...); ok {
		// 添加成功，返回的index是添加成功之后的最大index

		//  在follower接受完这个log后，会返回一个MsgAppResp消息
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: mlastIndex})
	} else {
		// 添加失败
		r.logger.Debugf("%x [logterm: %d, index: %d] rejected msgApp [logterm: %d, index: %d] from %x",
			r.id, r.raftLog.zeroTermOnErrCompacted(r.raftLog.term(m.Index)), m.Index, m.LogTerm, m.Index, m.From)
		// 添加失败的时候，返回的Index是传入过来的Index，RejectHint是该节点当前日志的最后索引
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: m.Index, Reject: true, RejectHint: r.raftLog.lastIndex()})
	}
}

// 处理HB消息
func (r *raft) handleHeartbeat(m pb.Message) {
	r.raftLog.commitTo(m.Commit)
	// 要把HB消息带过来的context原样返回
	r.send(pb.Message{To: m.From, Type: pb.MsgHeartbeatResp, Context: m.Context})
}

// 处理snapshot
func (r *raft) handleSnapshot(m pb.Message) {
	sindex, sterm := m.Snapshot.Metadata.Index, m.Snapshot.Metadata.Term
	// 注意这里成功与失败，只是返回的Index参数不同
	if r.restore(m.Snapshot) {
		r.logger.Infof("%x [commit: %d] restored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, sindex, sterm)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.lastIndex()})
	} else {
		r.logger.Infof("%x [commit: %d] ignored snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, sindex, sterm)
		r.send(pb.Message{To: m.From, Type: pb.MsgAppResp, Index: r.raftLog.committed})
	}
}

// restore recovers the state machine from a snapshot. It restores the log and the
// configuration of state machine.
// 使用快照数据进行恢复
// 恢复log和状态机的配置
func (r *raft) restore(s pb.Snapshot) bool {
	// 首先判断快照索引的合法性
	if s.Metadata.Index <= r.raftLog.committed {
		return false
	}

	// matchTerm返回true，说明本节点的日志中已经有对应的日志了
	if r.raftLog.matchTerm(s.Metadata.Index, s.Metadata.Term) {
		r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] fast-forwarded commit to snapshot [index: %d, term: %d]",
			r.id, r.raftLog.committed, r.raftLog.lastIndex(), r.raftLog.lastTerm(), s.Metadata.Index, s.Metadata.Term)
		// 提交到快照所在的索引
		r.raftLog.commitTo(s.Metadata.Index)

		// 为什么这里返回false？？？？
		return false
	}

	// The normal peer can't become learner.
	if !r.isLearner {
		for _, id := range s.Metadata.ConfState.Learners {
			if id == r.id {
				r.logger.Errorf("%x can't become learner when restores snapshot [index: %d, term: %d]", r.id, s.Metadata.Index, s.Metadata.Term)
				return false
			}
		}
	}

	r.logger.Infof("%x [commit: %d, lastindex: %d, lastterm: %d] starts to restore snapshot [index: %d, term: %d]",
		r.id, r.raftLog.committed, r.raftLog.lastIndex(), r.raftLog.lastTerm(), s.Metadata.Index, s.Metadata.Term)

	// 使用快照数据进行日志的恢复
	r.raftLog.restore(s)
	r.prs = make(map[uint64]*Progress)
	r.learnerPrs = make(map[uint64]*Progress)


	r.restoreNode(s.Metadata.ConfState.Nodes, false)
	r.restoreNode(s.Metadata.ConfState.Learners, true)
	return true
}

// 包括集群中其他节点的状态也使用快照中的状态数据进行恢复
func (r *raft) restoreNode(nodes []uint64, isLearner bool) {
	for _, n := range nodes {
		match, next := uint64(0), r.raftLog.lastIndex()+1
		if n == r.id {
			match = next - 1
			r.isLearner = isLearner
		}
		r.setProgress(n, match, next, isLearner)
		r.logger.Infof("%x restored progress of %x [%s]", r.id, n, r.getProgress(n))
	}
}

// promotable indicates whether state machine can be promoted to leader,
// which is true when its own id is in progress list.
// 检测是否能够提升到领导人
// 返回是否可以被提升为leader
func (r *raft) promotable() bool {
	// 在prs数组中找到本节点，说明该节点在集群中，这就具备了可以被选为leader的条件
	_, ok := r.prs[r.id]
	return ok
}

// 增加一个节点
func (r *raft) addNode(id uint64) {
	r.addNodeOrLearnerNode(id, false)
}

func (r *raft) addLearner(id uint64) {
	r.addNodeOrLearnerNode(id, true)
}


 // 增加一个node节点或者learner节点
func (r *raft) addNodeOrLearnerNode(id uint64, isLearner bool) {
	pr := r.getProgress(id)
	if pr == nil {
		// 这里才真的添加进来
		r.setProgress(id, 0, r.raftLog.lastIndex()+1, isLearner)
	} else {
		if isLearner && !pr.IsLearner {
			// can only change Learner to Voter
			r.logger.Infof("%x ignored addLearner: do not support changing %x from raft peer to learner.", r.id, id)
			return
		}

		if isLearner == pr.IsLearner {
			// Ignore any redundant addNode calls (which can happen because the
			// initial bootstrapping entries are applied twice).
			// 忽略任何多余的addNode调用（之所以会发生，是因为初始引导条目将被应用两次）。
			return
		}

		// change Learner to Voter, use origin Learner progress
		delete(r.learnerPrs, id)
		pr.IsLearner = false
		r.prs[id] = pr
	}

	if r.id == id {
		r.isLearner = isLearner
	}

	// When a node is first added, we should mark it as recently active.
	// Otherwise, CheckQuorum may cause us to step down if it is invoked
	// before the added node has a chance to communicate with us.

	// 首次添加节点时，我们应将其标记为最近处于活动状态。
	// 否则，CheckQuorum可能会导致我们退出, 如果CheckQuorum在add node 跟我们通信之前有可能被调用
	pr = r.getProgress(id)
	pr.RecentActive = true
}

// 删除一个节点
func (r *raft) removeNode(id uint64) {
	r.delProgress(id)

	// do not try to commit or abort transferring if there is no nodes in the cluster.
	// 在集群中没有node节点，不要尝试去commit或者abort转移
	if len(r.prs) == 0 && len(r.learnerPrs) == 0 {
		return
	}

	// The quorum size is now smaller, so see if any pending entries can
	// be committed.
	// 由于删除了节点，所以半数节点的数量变少了，于是去查看是否有可以认为提交成功的数据
	// 如果提交成功了， 向所有follower发送append消息
	if r.maybeCommit() {
		r.bcastAppend()
	}
	// If the removed node is the leadTransferee, then abort the leadership transferring.
	if r.state == StateLeader && r.leadTransferee == id {
		// 如果在leader迁移过程中发生了删除节点的操作，那么中断迁移leader流程
		r.abortLeaderTransfer()
	}
}

func (r *raft) setProgress(id, match, next uint64, isLearner bool) {
	if !isLearner {
		delete(r.learnerPrs, id)
		r.prs[id] = &Progress{Next: next, Match: match, ins: newInflights(r.maxInflight)}
		return
	}

	if _, ok := r.prs[id]; ok {
		panic(fmt.Sprintf("%x unexpected changing from voter to learner for %x", r.id, id))
	}
	r.learnerPrs[id] = &Progress{Next: next, Match: match, ins: newInflights(r.maxInflight), IsLearner: true}
}

// 删除指定的进程元素
func (r *raft) delProgress(id uint64) {
	delete(r.prs, id)
	delete(r.learnerPrs, id)
}

func (r *raft) loadState(state pb.HardState) {
	if state.Commit < r.raftLog.committed || state.Commit > r.raftLog.lastIndex() {
		r.logger.Panicf("%x state.commit %d is out of range [%d, %d]", r.id, state.Commit, r.raftLog.committed, r.raftLog.lastIndex())
	}
	r.raftLog.committed = state.Commit
	r.Term = state.Term
	r.Vote = state.Vote
}

// pastElectionTimeout returns true iff r.electionElapsed is greater
// than or equal to the randomized election timeout in
// [electiontimeout, 2 * electiontimeout - 1].
// 选举超时, 时间已经超过随机超时时间
func (r *raft) pastElectionTimeout() bool {
	return r.electionElapsed >= r.randomizedElectionTimeout
}

// 设置随机选举超时时间 [electionTimeout, 2 * electiontimeout - 1]
func (r *raft) resetRandomizedElectionTimeout() {
	r.randomizedElectionTimeout = r.electionTimeout + globalRand.Intn(r.electionTimeout)
}

// checkQuorumActive returns true if the quorum is active from
// the view of the local raft state machine. Otherwise, it returns false.
// checkQuorumActive also resets all RecentActive to false.

// 检查节点有多少是存活的
// 检查集群的可用于，当小于半数的节点不可用时返回false
func (r *raft) checkQuorumActive() bool {
	var act int

	r.forEachProgress(func(id uint64, pr *Progress) {
		if id == r.id { // self is always active
			act++
			return
		}

		if pr.RecentActive && !pr.IsLearner {
			act++
		}

		pr.RecentActive = false
	})

	return act >= r.quorum()
}

// 发送超时信息
func (r *raft) sendTimeoutNow(to uint64) {
	r.send(pb.Message{To: to, Type: pb.MsgTimeoutNow})
}

// 中断leader转让流程
func (r *raft) abortLeaderTransfer() {
	r.leadTransferee = None
}

// increaseUncommittedSize computes the size of the proposed entries and
// determines whether they would push leader over its maxUncommittedSize limit.
// If the new entries would exceed the limit, the method returns false. If not,
// the increase in uncommitted entry size is recorded and the method returns
// true.

// increaseUncommittedSize 可以计算建议条目的大小，并确定它们是否将领导者超过其maxUncommittedSize限制。
// 如果新条目将超过限制，则该方法返回false
// 如果不是，则记录未提交条目大小的增加，并且该方法返回true。
func (r *raft) increaseUncommittedSize(ents []pb.Entry) bool {
	var s uint64
	for _, e := range ents {
		s += uint64(PayloadSize(e))
	}

	if r.uncommittedSize > 0 && r.uncommittedSize+s > r.maxUncommittedSize {
		// If the uncommitted tail of the Raft log is empty, allow any size
		// proposal. Otherwise, limit the size of the uncommitted tail of the
		// log and drop any proposal that would push the size over the limit.
		// 如果raft日志的未提交尾部为空，则允许任何大小的建议。
		// 否则，请限制日志未提交尾部的大小，并删除任何会超出该限制的建议。
		return false
	}
	r.uncommittedSize += s
	return true
}

// reduceUncommittedSize accounts for the newly committed entries by decreasing
// the uncommitted entry size limit.
// reduceUncommittedSize通过减小未提交的条目大小限制来解决新提交的条目。

func (r *raft) reduceUncommittedSize(ents []pb.Entry) {
	if r.uncommittedSize == 0 {
		// Fast-path for followers, who do not track or enforce the limit.
		// 追随者的快速路径，这些追随者不跟踪或执行该限制。
		return
	}

	var s uint64
	for _, e := range ents {
		s += uint64(PayloadSize(e))
	}
	if s > r.uncommittedSize {
		// uncommittedSize may underestimate the size of the uncommitted Raft
		// log tail but will never overestimate it. Saturate at 0 instead of
		// allowing overflow.

		// uncommittedSize可能会低估未提交的Raft日志尾部的大小，但是永远不会高估它。
		// 饱和为0，而不允许溢出。
		r.uncommittedSize = 0
	} else {
		r.uncommittedSize -= s
	}
}

// 统计多少个PendingConf
// 返回消息数组中配置变化的消息数量
func numOfPendingConf(ents []pb.Entry) int {
	n := 0
	for i := range ents {
		if ents[i].Type == pb.EntryConfChange {
			n++
		}
	}
	return n
}
