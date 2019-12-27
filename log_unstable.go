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

import pb "go.etcd.io/etcd/raft/raftpb"

// unstable.entries[i] has raft log position i+unstable.offset.
// Note that unstable.offset may be less than the highest log
// position in storage; this means that the next write to storage
// might need to truncate the log before persisting unstable.entries.
// unstable.entries[i] 代表raft日志的位置是: i+unstable.offset
// 请注意，unstable.offset可能小于最高log存放位置； 这意味着下一次写入存储
// 可能需要先截断日志，然后才能保持unstable.entries。

// 顾名思义，unstable数据结构用于还没有被用户层持久化的数据，它维护了两部分内容snapshot和entries：
/*
entries代表的是要进行操作的日志，但日志不可能无限增长，在特定的情况下，某些过期的日志会被清空。那这就引入一个新问题了，如果此后一个新的follower加入，
而leader只有一部分操作日志，那这个新follower不是没法跟别人同步了吗？所以这个时候snapshot就登场了 - 我无法给你之前的日志，但我给你所有之前日志应用
后的结果，之后的日志你再以这个snapshot为基础进行应用，那我们的状态就可以同步了。因此它们的结构关系可以用下图表示3：

                offset
                |
|  snapshot     |    entries   |
这里的前半部分是快照数据，而后半部分是日志条目组成的数组entries，另外unstable.offset成员保存的是entries数组中的第一条数据在raft日志中的索引，
即第i条entries在raft日志中的索引为i + unstable.offset。
*/

type unstable struct {
	// the incoming unstable snapshot, if any.  传入的不稳定快照（如果有）。
	snapshot *pb.Snapshot
	// all entries that have not yet been written to storage
	// 还没有写入到storage的日志
	entries []pb.Entry
	offset  uint64

	logger Logger
}

// maybeFirstIndex returns the index of the first possible entry in entries
// if it has a snapshot.

// 返回unstable数据的第一条数据索引。因为只有快照数据在最前面，因此这个函数只有当快照数据存在的时候才能拿到第一条数据索引，
// 其他的情况下已经拿不到了。
func (u *unstable) maybeFirstIndex() (uint64, bool) {
	// 有快照的处理行为，取出快照中最大的索引值，+1
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index + 1, true
	}
	// 没有快照的时候，拿不到unstable的第一条索引
	return 0, false
}

// maybeLastIndex returns the last index if it has at least one
// unstable entry or snapshot.

// 返回最后一条数据的索引。因为是entries数据在后，而快照数据在前，所以取最后一
// 条数据索引是从entries开始查，查不到的情况下才查快照数据
func (u *unstable) maybeLastIndex() (uint64, bool) {
	// 从entries中查找
	if l := len(u.entries); l != 0 {
		return u.offset + uint64(l) - 1, true
	}

	// 没有entries, 从快照中读取
	if u.snapshot != nil {
		return u.snapshot.Metadata.Index, true
	}
	return 0, false
}

// maybeTerm returns the term of the entry at index i, if there
// is any.
// 这个函数根据传入的日志数据索引，得到这个日志对应的任期号。
// 前面已经提过，unstable.offset是快照数据和entries数组的分界线，因为在这个函数中，会区分传入的参数与offset的大小关系，
// 小于offset的情况下在快照数据中查询，否则就在entries数组中查询了。
func (u *unstable) maybeTerm(i uint64) (uint64, bool) {
	// 查找的索引小于offset,需要去snapshot中查找
	if i < u.offset {
		if u.snapshot == nil {
			return 0, false
		}
		if u.snapshot.Metadata.Index == i {
			return u.snapshot.Metadata.Term, true
		}
		return 0, false
	}

	last, ok := u.maybeLastIndex()
	if !ok {
		return 0, false
	}
	if i > last {
		return 0, false
	}
	return u.entries[i-u.offset].Term, true
}

 // 该函数传入一个索引号i和任期号t，表示应用层已经将这个索引之前的数据进行持久化了，
 // 此时unstable要做的事情就是在自己的数据中查询，
 // 只有在满足任期号相同以及i大于等于offset的情况下，可以将entries中的数据进行缩容，将i之前的数据删除。
 //  针对entries数组
func (u *unstable) stableTo(i, t uint64) {
	gt, ok := u.maybeTerm(i)
	if !ok {
		return
	}
	// if i < offset, term is matched with the snapshot
	// only update the unstable entries if term is matched with
	// an unstable entry.
	if gt == t && i >= u.offset {
		u.entries = u.entries[i+1-u.offset:]
		u.offset = i + 1
		u.shrinkEntriesArray()
	}
}

// shrinkEntriesArray discards the underlying array used by the entries slice
// if most of it isn't being used. This avoids holding references to a bunch of
// potentially large entries that aren't needed anymore. Simply clearing the
// entries wouldn't be safe because clients might still be using them.

// shrinkEntriesArray会丢弃没有使用的entry slice。 这样可以避免保留
// 对不再需要的大量潜在条目的引用。 仅清除条目并不安全，因为客户端可能仍在使用它们。

// 当entries数组切片capacity有一半以上未使用时，shrink下，节省内存开销。类似于C++的vector，shrink函数
func (u *unstable) shrinkEntriesArray() {
	// We replace the array if we're using less than half of the space in
	// it. This number is fairly arbitrary, chosen as an attempt to balance
	// memory usage vs number of allocations. It could probably be improved
	// with some focused tuning.

	// 如果我们使用的数组少于一半的空间，则替换数组。 这个数字相当随意，被选为平衡内存使用与分配数量的一种尝试。 可以通过一些集中的调整来改进它。
	const lenMultiple = 2
	if len(u.entries) == 0 {
		u.entries = nil
	} else if len(u.entries)*lenMultiple < cap(u.entries) {

		// 创建数组切片，copy内容
		newEntries := make([]pb.Entry, len(u.entries))
		copy(newEntries, u.entries)
		u.entries = newEntries
	}
}

 // 该函数传入一个索引i，用于告诉unstable，索引i对应的快照数据已经被应用层持久化了，
 // 如果这个索引与当前快照数据对应的上，那么快照数据就可以被置空了。
func (u *unstable) stableSnapTo(i uint64) {
	if u.snapshot != nil && u.snapshot.Metadata.Index == i {
		u.snapshot = nil
	}
}

// 把Snapshot设置放入unstable
// 从快照数据中恢复，此时unstable将保存快照数据，同时将offset成员设置成这个快照数据索引的下一位。
func (u *unstable) restore(s pb.Snapshot) {
	u.offset = s.Metadata.Index + 1
	u.entries = nil
	u.snapshot = &s
}

 //  传入日志条目数组，这段数据将添加到entries数组中。但是需要注意的是，传入的数据跟现有
 //  的entries数据可能有重合的部分，所以需要根据unstable.offset与传入数据的索引大小关系进行处理，有些数据可能会被截断。

 // append 多个entry， 有可能截断entry数组
func (u *unstable) truncateAndAppend(ents []pb.Entry) {
	after := ents[0].Index
	switch {
	case after == u.offset+uint64(len(u.entries)):
		// after is the next index in the u.entries
		// directly append
		// 正好是要追加的下个日志的索引,就直接追加
		// 追加数组切片
		u.entries = append(u.entries, ents...)
	case after <= u.offset:
		u.logger.Infof("replace the unstable entries from index %d", after)
		// The log is being truncated to before our current offset
		// portion, so set the offset and replace the entries
		// 直接替换
		u.offset = after
		u.entries = ents
	default:
		// truncate to after and copy to u.entries
		// then append
		// 截断到after,然后追加
		u.logger.Infof("truncate the unstable entries before index %d", after)
		// 截断原来的数组切片
		u.entries = append([]pb.Entry{}, u.slice(u.offset, after)...)
		// 追加新的数组切片
		u.entries = append(u.entries, ents...)
	}
}

 // 截断数组切片，[lo-u.offset : hi-u.offset]
 // 返回索引范围在[lo-u.offset : hi-u.offset]之间的数据。
func (u *unstable) slice(lo uint64, hi uint64) []pb.Entry {
	u.mustCheckOutOfBounds(lo, hi)
	return u.entries[lo-u.offset : hi-u.offset]
}

// u.offset <= lo <= hi <= u.offset+len(u.entries)
// 检查是否越界, 检查传入的数据索引范围是否合理。
func (u *unstable) mustCheckOutOfBounds(lo, hi uint64) {
	if lo > hi {
		u.logger.Panicf("invalid unstable.slice %d > %d", lo, hi)
	}
	upper := u.offset + uint64(len(u.entries))
	if lo < u.offset || hi > upper {
		u.logger.Panicf("unstable.slice[%d,%d) out of bound [%d,%d]", lo, hi, u.offset, upper)
	}
}
