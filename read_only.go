// Copyright 2016 The etcd Authors
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

import pb "go.etcd.io/etcd/raft/raftpb"

// 线性一致性读相关
/*
线性一致性（Linearizable Read）通俗来讲，就是读请求需要读到最新的已经commit的数据，不会读到老数据。
由于所有的leader和follower都能处理客户端的读请求，所以存在可能造成返回读出的旧数据的情况：
leader和follower之间存在状态差，因为follower总是由leader同步过去的，可能会返回同步之前的数据。
如果发生了网络分区，某个leader实际上已经被隔离出了集群之外，但是该leader并不知道，如果还继续响应客户端的读请求，也可能会返回旧的数据。
因此，在接收到客户端的读请求时，需要保证返回的数据都是当前最新的。


ReadOnlySafe方式
leader在接收到读请求时，需要向集群中的超半数server确认自己仍然是当前的leader，这样它返回的就是最新的数据。
 */
// ReadState provides state for read only query.
// It's caller's responsibility to call ReadIndex first before getting
// this state from ready, it's also caller's duty to differentiate if this
// state is what it requests through RequestCtx, eg. given a unique id as
// RequestCtx

// ReadState提供只读查询的状态。
// 在此状态准备就绪之前，调用方有责任首先调用ReadIndex，调用方有责任区分是否是它通过RequestCtx请求的状态。
// 例如，给定一个唯一的ID作为RequestCtx

// Index：接收到该读请求时，当前节点的commit索引。
// RequestCtx：客户端读请求的唯一标识。
//ReadState结构体用于保存读请求到来时的节点状态。
type ReadState struct {
	Index      uint64
	RequestCtx []byte
}

// readIndexStatus数据结构用于追踪leader向follower发送的心跳信息，其中：
//
// req：保存原始的readIndex请求。
// index：leader当前的commit日志索引。
// acks：存放该readIndex请求有哪些节点进行了应答，当超过半数应答时，leader就可以确认自己还是当前集群的leader。
type readIndexStatus struct {
	req   pb.Message
	index uint64
	acks  map[uint64]struct{}
}

// readOnly用于管理全局的readIndex数据，其中：
//
// option：readOnly选项。
// pendingReadIndex：当前所有待处理的readIndex请求，其中key为客户端读请求的唯一标识。
// readIndexQueue：保存所有readIndex请求的请求唯一标识数组。
type readOnly struct {
	option           ReadOnlyOption
	pendingReadIndex map[string]*readIndexStatus
	readIndexQueue   []string
}

func newReadOnly(option ReadOnlyOption) *readOnly {
	return &readOnly{
		option:           option,
		pendingReadIndex: make(map[string]*readIndexStatus),
	}
}

// addRequest adds a read only request into readonly struct.
// `index` is the commit index of the raft state machine when it received
// the read only request.

// addRequest 增加一个只读请求到只读结构体
// index 是raft状态机接收到只读请求时的commit index
// `m` is the original read only request message from the local or remote node.
// m 是来自本地或者远程节点的原始制度请求信息
func (ro *readOnly) addRequest(index uint64, m pb.Message) {
	ctx := string(m.Entries[0].Data)
	if _, ok := ro.pendingReadIndex[ctx]; ok {
		return
	}
	ro.pendingReadIndex[ctx] = &readIndexStatus{index: index, req: m, acks: make(map[uint64]struct{})}
	ro.readIndexQueue = append(ro.readIndexQueue, ctx)
}

// recvAck notifies the readonly struct that the raft state machine received
// an acknowledgment of the heartbeat that attached with the read only request
// context.
// recvAck通知只读结构，raft状态机收到了附带只读请求上下文的心跳确认。

// 根据消息中的ctx字段，到全局的pendingReadIndex中查找是否有保存该ctx的带处理的readIndex请求，
// 如果有就在acks map中记录下该follower已经进行了应答。
func (ro *readOnly) recvAck(m pb.Message) int {
	rs, ok := ro.pendingReadIndex[string(m.Context)]
	if !ok {
		return 0
	}

	rs.acks[m.From] = struct{}{}
	// add one to include an ack from local node
	return len(rs.acks) + 1
}

// advance advances the read only request queue kept by the readonly struct.
// It dequeues the requests until it finds the read only request that has
// the same context as the given `m`.

// advance 推进由readonly结构保留的只读请求队列。
// 它使请求出队，直到找到与给定的m具有相同上下文的只读请求。

// 将该readIndex之前的所有readIndex请求都认为是已经成功进行确认的了，
// 所有成功确认的readIndex请求，将会加入到readStates数组中
func (ro *readOnly) advance(m pb.Message) []*readIndexStatus {
	var (
		i     int
		found bool
	)

	ctx := string(m.Context)
	rss := []*readIndexStatus{}

	for _, okctx := range ro.readIndexQueue {
		i++
		rs, ok := ro.pendingReadIndex[okctx]
		if !ok {
			panic("cannot find corresponding read state from pending map")
		}
		rss = append(rss, rs)
		if okctx == ctx {
			found = true
			break
		}
	}

	if found {
		ro.readIndexQueue = ro.readIndexQueue[i:]
		for _, rs := range rss {
			// 删除指定的元素
			delete(ro.pendingReadIndex, string(rs.req.Entries[0].Data))
		}
		return rss
	}

	return nil
}

// lastPendingRequestCtx returns the context of the last pending read only
// request in readonly struct.
// 返回最后悬挂只读请求的上下文
func (ro *readOnly) lastPendingRequestCtx() string {
	if len(ro.readIndexQueue) == 0 {
		return ""
	}
	return ro.readIndexQueue[len(ro.readIndexQueue)-1]
}
