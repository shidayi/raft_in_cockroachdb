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
	"errors"
	"sync"

	pb "go.etcd.io/etcd/raft/raftpb"
)

// 提供存储接口，应用层可以按照自己的需求实现该接口

// 这个文件定义了一个Storage接口，因为etcd中的raft实现并不负责数据的持久化，所以它希望上面的应用层能实现这个接口，以便提供给它查询log的能力。
// 另外，这个文件也提供了Storage接口的一个内存版本的实现MemoryStorage，这个实现同样也维护了snapshot和entries这两部分，他们的排列跟
// unstable中的类似，也是snapshot在前，entries在后。从代码中看来etcdserver和raftexample都是直接用的这个实现来提供log的查询功能的。




// ErrCompacted is returned by Storage.Entries/Compact when a requested
// index is unavailable because it predates the last snapshot.
// 当返回ErrCompacted表示传入的索引数据已经找不到，说明已经被压缩成快照数据了；
var ErrCompacted = errors.New("requested index is unavailable due to compaction")

// ErrSnapOutOfDate is returned by Storage.CreateSnapshot when a requested
// index is older than the existing snapshot.
var ErrSnapOutOfDate = errors.New("requested index is older than the existing snapshot")

// ErrUnavailable is returned by Storage interface when the requested log entries
// are unavailable.
// 返回ErrUnavailable：表示传入的索引值不可用。
var ErrUnavailable = errors.New("requested entry at index is unavailable")

// ErrSnapshotTemporarilyUnavailable is returned by the Storage interface when the required
// snapshot is temporarily unavailable.

// 请求的snapshot 临时不可用
var ErrSnapshotTemporarilyUnavailable = errors.New("snapshot is temporarily unavailable")

// Storage is an interface that may be implemented by the application
// to retrieve log entries from storage.
// 提供了存储持久化日志相关的接口操作。

// If any Storage method returns an error, the raft instance will
// become inoperable and refuse to participate in elections; the
// application is responsible for cleanup and recovery in this case.

// 如果Storage方法返回一个错误,raft实例将出现问题,并拒绝增加选举,应用程序需要清理和恢复

type Storage interface {
	// InitialState returns the saved HardState and ConfState information.
	// InitialState 返回保存的HardState和ConfState
	// 返回当前的初始状态，其中包括硬状态（HardState）以及配置（里面存储了集群中有哪些节点）。
	InitialState() (pb.HardState, pb.ConfState, error)

	// Entries returns a slice of log entries in the range [lo,hi).
	// MaxSize limits the total size of the log entries returned, but
	// Entries returns at least one entry if any.
	// 传入起始和结束索引值，以及最大的尺寸
	// 返回索引范围在这个传入范围以内并且不超过大小的日志条目数组
	Entries(lo, hi, maxSize uint64) ([]pb.Entry, error)

	// Term returns the term of entry i, which must be in the range
	// [FirstIndex()-1, LastIndex()]. The term of the entry before
	// FirstIndex is retained for matching purposes even though the
	// rest of that entry may not be available.
	// 传入日志索引i
	// 返回这条日志对应的任期号。找不到的情况下error返回值不为空，
	// 其中当返回ErrCompacted表示传入的索引数据已经找不到，说明已经被压缩成快照数据了；
	// 返回ErrUnavailable：表示传入的索引值大于当前的最大索引。
	Term(i uint64) (uint64, error)

	// LastIndex returns the index of the last entry in the log.
	// 返回最后一条数据的索引
	LastIndex() (uint64, error)

	// FirstIndex returns the index of the first log entry that is
	// possibly available via Entries (older entries have been incorporated
	// into the latest Snapshot; if storage only contains the dummy entry the
	// first log entry is not available).
	// 返回第一条数据的索引
	FirstIndex() (uint64, error)

	// Snapshot returns the most recent snapshot.
	// If snapshot is temporarily unavailable, it should return ErrSnapshotTemporarilyUnavailable,
	// so raft state machine could know that Storage needs some time to prepare
	// snapshot and call Snapshot later.
	// 返回最近的快照数据
	Snapshot() (pb.Snapshot, error)
}

// 我对这个接口提供出来的接口函数比较有疑问，因为搜索了etcd的代码，该接口只有MemoryStorage一个实现，
// 而实际上MemoryStorage这个结构体还有其他的函数，比如添加日志数据的操作，但是这个操作并没有在Storage接口中声明。
// 接下来看看实现了Storage接口的MemoryStorage结构体的实现


// MemoryStorage implements the Storage interface backed by an
// in-memory array.
// MemoryStorage 是通过内存数组来实现的内存接口
type MemoryStorage struct {
	// Protects access to all fields. Most methods of MemoryStorage are
	// run on the raft goroutine, but Append() is run on an application
	// goroutine.
	sync.Mutex     //  继承了Mutex的所有方法，如调用MemoryStorage.Lock(), 就是调用MemoryStorage.(sync.Mutex).Lock()

	// 存储硬状态
	hardState pb.HardState
	// 存储快照数据
	snapshot  pb.Snapshot
	// ents[i] has raft log position i+snapshot.Metadata.Index
	// 存储紧跟着快照数据的日志条目数组，即ents[i]保存的日志数据索引位置为i + snapshot.Metadata.Index
	ents []pb.Entry
}

// NewMemoryStorage creates an empty MemoryStorage.
// NewXXX 构造函数
func NewMemoryStorage() *MemoryStorage {
	return &MemoryStorage{
		// When starting from scratch populate the list with a dummy entry at term zero.
		// 开始填充list前在任期0有一个哨兵entry
		// 表示只初始化ents值，其他值都未显示初始化，使用各种类型的零值
		// entries 第一个日志是哨兵
		ents: make([]pb.Entry, 1),
	}
}

// InitialState implements the Storage interface.
// 返回当前的初始状态，其中包括硬状态（HardState）以及配置（里面存储了集群中有哪些节点）。
func (ms *MemoryStorage) InitialState() (pb.HardState, pb.ConfState, error) {
	return ms.hardState, ms.snapshot.Metadata.ConfState, nil
}

// SetHardState saves the current HardState.
func (ms *MemoryStorage) SetHardState(st pb.HardState) error {
	ms.Lock()    //  使用互斥锁进行保护ß
	defer ms.Unlock()
	ms.hardState = st
	return nil
}

// Entries implements the Storage interface.
// maxSize表示取出日志的总长度
// 传入起始和结束索引值，以及最大的尺寸（字节数）
// 返回索引范围在这个传入范围以内并且不超过大小的日志条目数组。
func (ms *MemoryStorage) Entries(lo, hi, maxSize uint64) ([]pb.Entry, error) {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index

	// 提前访问了,可能是过早的进行了snapshot
	if lo <= offset {
		return nil, ErrCompacted   // 当返回ErrCompacted表示传入的索引数据已经找不到，说明已经被压缩成快照数据了；
	}
	if hi > ms.lastIndex()+1 {
		raftLogger.Panicf("entries' hi(%d) is out of bound lastindex(%d)", hi, ms.lastIndex())
	}
	// only contains dummy entries.   保护空的entries
	// 仅仅剩余最后的哨兵
	//  返回ErrUnavailable：表示传入的索引值不可用。
	if len(ms.ents) == 1 {
		return nil, ErrUnavailable
	}

	ents := ms.ents[lo-offset : hi-offset]
	// 使用limitSize来进行截断
	return limitSize(ents, maxSize), nil
}

// Term implements the Storage interface.
// i是日志的索引
// 传入日志索引i，返回这条日志对应的任期号。找不到的情况下error返回值不为空，其中当返回ErrCompacted表示传入的索引数据
// 已经找不到，说明已经被压缩成快照数据了；返回ErrUnavailable：表示传入的索引值大于当前的最大索引。
func (ms *MemoryStorage) Term(i uint64) (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index
	// 表示传入的索引数据已经找不到，说明已经被压缩成快照数据了
	if i < offset {
		return 0, ErrCompacted
	}
	// 表示传入的索引值大于当前的最大索引。
	if int(i-offset) >= len(ms.ents) {
		return 0, ErrUnavailable
	}
	return ms.ents[i-offset].Term, nil
}

// LastIndex implements the Storage interface.
func (ms *MemoryStorage) LastIndex() (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	return ms.lastIndex(), nil
}

// 存储的日志中最后的索引编号
func (ms *MemoryStorage) lastIndex() uint64 {
	return ms.ents[0].Index + uint64(len(ms.ents)) - 1
}

// FirstIndex implements the Storage interface.
func (ms *MemoryStorage) FirstIndex() (uint64, error) {
	ms.Lock()
	defer ms.Unlock()
	return ms.firstIndex(), nil
}

// 返回第一条数据的索引,   这里为什么+1
// 如果是应用快照的时候，ms.ents[0].Index 存储的是快照的最大索引，所以加一
func (ms *MemoryStorage) firstIndex() uint64 {
	return ms.ents[0].Index + 1
}

// Snapshot implements the Storage interface.
// 返回最近的快照数据s
func (ms *MemoryStorage) Snapshot() (pb.Snapshot, error) {
	ms.Lock()
	defer ms.Unlock()
	return ms.snapshot, nil
}

// ApplySnapshot overwrites the contents of this Storage object with
// those of the given snapshot.
func (ms *MemoryStorage) ApplySnapshot(snap pb.Snapshot) error {
	ms.Lock()
	defer ms.Unlock()

	//handle check for old snapshot being applied
	msIndex := ms.snapshot.Metadata.Index
	snapIndex := snap.Metadata.Index

	// 请求的比已经存在的陈旧,  快照过期
	if msIndex >= snapIndex {
		return ErrSnapOutOfDate
	}

	ms.snapshot = snap

	//  重新初始化ents, 哨兵改为已经存入到snap的任期和索引
	ms.ents = []pb.Entry{{Term: snap.Metadata.Term, Index: snap.Metadata.Index}}
	return nil
}

// CreateSnapshot makes a snapshot which can be retrieved with Snapshot() and
// can be used to reconstruct the state at that point.
// If any configuration changes have been made since the last compaction,
// the result of the last ApplyConfChange must be passed in.

// CreateSnapshot创建一个snapshot,能被Snapshot()函数检索,能够在某个时间点重建状态
// 如果自上次压缩以来已进行了任何配置更改，则必须传递上一次ApplyConfChange的结果。
func (ms *MemoryStorage) CreateSnapshot(i uint64, cs *pb.ConfState, data []byte) (pb.Snapshot, error) {
	ms.Lock()
	defer ms.Unlock()

	// 请求的索引号 小于已经存在的快照中最大的索引号
	if i <= ms.snapshot.Metadata.Index {
		return pb.Snapshot{}, ErrSnapOutOfDate
	}

	offset := ms.ents[0].Index
	if i > ms.lastIndex() {
		raftLogger.Panicf("snapshot %d is out of bound lastindex(%d)", i, ms.lastIndex())
	}


	ms.snapshot.Metadata.Index = i
	// snapshot的任期就是ms.ents[i-offset].Term,和Term函数殊途同归
	ms.snapshot.Metadata.Term = ms.ents[i-offset].Term
	if cs != nil {
		ms.snapshot.Metadata.ConfState = *cs
	}
	ms.snapshot.Data = data
	return ms.snapshot, nil
}

// Compact discards all log entries prior to compactIndex.
// It is the application's responsibility to not attempt to compact an index
// greater than raftLog.applied.
// 压缩日志到compactIndex,
// 应用程序应该控制不要压缩index大于raftLog.applied
func (ms *MemoryStorage) Compact(compactIndex uint64) error {
	ms.Lock()
	defer ms.Unlock()
	offset := ms.ents[0].Index
	if compactIndex <= offset {
		return ErrCompacted   // 表示传入的索引数据已经找不到，说明已经被压缩成快照数据了；
	}
	if compactIndex > ms.lastIndex() {
		raftLogger.Panicf("compact %d is out of bound lastindex(%d)", compactIndex, ms.lastIndex())
	}

	// i  变成了新的offset
	i := compactIndex - offset

	// 第一个元素为哨兵
	ents := make([]pb.Entry, 1, 1+uint64(len(ms.ents))-i)
	ents[0].Index = ms.ents[i].Index
	ents[0].Term = ms.ents[i].Term
	ents = append(ents, ms.ents[i+1:]...)
	ms.ents = ents
	return nil
}

// Append the new entries to storage.
// TODO (xiangli): ensure the entries are continuous and
// entries[0].Index > ms.entries[0].Index
// 往storage中添加日志数组
func (ms *MemoryStorage) Append(entries []pb.Entry) error {
	if len(entries) == 0 {
		return nil
	}

	ms.Lock()
	defer ms.Unlock()

	// 要追加的最小的日志索引值
	first := ms.firstIndex()
	last := entries[0].Index + uint64(len(entries)) - 1

	// shortcut if there is no new entry.
	//  新追加的日志比原来的最小的日志都小，丢弃
	if last < first {
		return nil
	}
	// truncate compacted entries
	// 当前要追加的最小的日志索引值比entries的第一个索引值小,需要对要追加的日志进行截断，取出有效部分
	if first > entries[0].Index {
		entries = entries[first-entries[0].Index:]
	}

	// 获取偏差，找到新插入的最小索引和原来的ms中的日志的最小索引之间的差值
	offset := entries[0].Index - ms.ents[0].Index
	switch {
	case uint64(len(ms.ents)) > offset:
		// 有重合的部分日志,首先截断,然后再添加新的
		ms.ents = append([]pb.Entry{}, ms.ents[:offset]...)
		ms.ents = append(ms.ents, entries...)
	case uint64(len(ms.ents)) == offset:
		// 刚好， 附加到原来的日志后面
		ms.ents = append(ms.ents, entries...)
	default:
		// 缺少日志,中间的一部分日志缺失
		raftLogger.Panicf("missing log entry [last: %d, append at: %d]",
			ms.lastIndex(), entries[0].Index)
	}
	return nil
}
