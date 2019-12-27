## Progress

Progress represents a follower’s progress in the view of the leader. Leader maintains progresses of all followers, and sends `replication message` to the follower based on its progress. 
Progress代表从领导者角度来看，follower的进度。 领导者维护所有follower的进度，并根据其进度向follower发送“复制消息”。

`replication message` is a `msgApp` with log entries.

A progress has two attribute: `match` and `next`. `match` is the index of the highest known matched entry. If leader knows nothing about follower’s replication status, `match` is set to zero. `next` is the index of the first entry that will be replicated to the follower. Leader puts entries from `next` to its latest one in next `replication message`.


A progress is in one of the three state: `probe`, `replicate`, `snapshot`. 

```
                            +--------------------------------------------------------+          
                            |                  send snapshot                         |          
                            |                                                        |          
                  +---------+----------+                                  +----------v---------+
              +--->       probe        |                                  |      snapshot      |
              |   |  max inflight = 1  <----------------------------------+  max inflight = 0  |
              |   +---------+----------+                                  +--------------------+
              |             |            1. snapshot success                                    
              |             |               (next=snapshot.index + 1)                           
              |             |            2. snapshot failure                                    
              |             |               (no change)                                         
              |             |            3. receives msgAppResp(rej=false&&index>lastsnap.index)
              |             |               (match=m.index,next=match+1)                        
receives msgAppResp(rej=true)                                                                   
(next=match+1)|             |                                                                   
              |             |                                                                   
              |             |                                                                   
              |             |   receives msgAppResp(rej=false&&index>match)                     
              |             |   (match=m.index,next=match+1)                                    
              |             |                                                                   
              |             |                                                                   
              |             |                                                                   
              |   +---------v----------+                                                        
              |   |     replicate      |                                                        
              +---+  max inflight = n  |                                                        
                  +--------------------+                                                        
```

When the progress of a follower is in `probe` state, leader sends at most one `replication message` per heartbeat interval. The leader sends `replication message` slowly and probing the actual progress of the follower. A `msgHeartbeatResp` or a `msgAppResp` with reject might trigger the sending of the next `replication message`.
当follower的进度处于“探测”状态时，领导者每心跳间隔最多发送一个“复制消息”。 
领导者缓慢地发送“复制消息”并探查跟随者的实际进度。 带有拒绝的`msgHeartbeatResp`或`msgAppResp`可能触发下一个`复制消息`的发送。


When the progress of a follower is in `replicate` state, leader sends `replication message`, then optimistically increases `next` to the latest entry sent. This is an optimized state for fast replicating log entries to the follower.
当follower的进度处于“复制”状态时，领导者发送“复制消息”，然后乐观地将“next”增加到将要发送的最新条目。
 这是用于将日志条目快速复制到follower的优化状态。

When the progress of a follower is in `snapshot` state, leader stops sending any `replication message`.
当follower的进度处于“快照”状态时，领导者将停止发送任何“复制消息”。

A newly elected leader sets the progresses of all the followers to `probe` state with `match` = 0 and `next` = last index. The leader slowly (at most once per heartbeat) sends `replication message` to the follower and probes its progress.
新当选的领导者将所有follower的进度设置为“ probe”状态，其中“ match” = 0，“ next” =最后索引。
领导者（每个心跳最多一次）缓慢地向跟follower发送“复制消息”并探测其进度。

A progress changes to `replicate` when the follower replies with a non-rejection `msgAppResp`, which implies that it has matched the index sent. At this point, leader starts to stream log entries to the follower fast. The progress will fall back to `probe` when the follower replies a rejection `msgAppResp` or the link layer reports the follower is unreachable. We aggressively reset `next` to `match`+1 since if we receive any `msgAppResp` soon, both `match` and `next` will increase directly to the `index` in `msgAppResp`. (We might end up with sending some duplicate entries when aggressively reset `next` too low.  see open question)
当follower以不拒绝的“ msgAppResp”答复时，进度将变为“复制”，这意味着它已与发送的索引匹配。
此时，领导者开始快速将日志条目流式传输到follower。 当follower回复拒绝“ msgAppResp”或
链接层报告follower无法访问时，进度将退回到“探测”。 我们会主动将“next”重置为“match” +1，因为如果我们很快收到任何“ msgAppResp”，则“match”和“next”都将直接增加到“ msgAppResp
”中的“索引”。 （当将“ next”设置得太低时，我们可能最终会发送一些重复的条目。请参阅未解决的问题）


A progress changes from `probe` to `snapshot` when the follower falls very far behind and requires a snapshot. After sending `msgSnap`, the leader waits until the success, failure or abortion of the previous snapshot sent. The progress will go back to `probe` after the sending result is applied.
当follower落后很远并且需要快照时，进度将从“探测”变为“快照”。 
发送“ msgSnap”后，领导者会一直等待，直到成功，失败或中止上次发送的快照。 发送结果被应用后，进度将返回到“探测”。

### Flow Control

1. limit the max size of message sent per message. Max should be configurable.
Lower the cost at probing state as we limit the size per message; lower the penalty when aggressively decreased to a too low `next`
限制每条消息发送的最大消息大小。 Max应该是可配置的。
降低探测状态的成本，因为我们限制了每条消息的大小； 当积极降低到太低的“next”时，降低处罚

2. limit the # of in flight messages < N when in `replicate` state. N should be configurable. Most implementation will have a sending buffer on top of its actual network transport layer (not blocking raft node). We want to make sure raft does not overflow that buffer, which can cause message dropping and triggering a bunch of unnecessary resending repeatedly. 
在处于“复制”状态时，限制飞行中消息的数量<N。 
N应该是可配置的。 大多数实现将在其实际网络传输层（不阻塞raft节点）的顶部具有一个发送缓冲区。 
我们要确保raft不溢出该缓冲区，这可能导致消息丢失并触发大量不必要的重发

![(RaftLog Layout)](https://blog.betacat.io/image/raft-in-etcd/raftlog-layout.png)



Cockroach的Raft实现在CoreOS的基础上，增加额外的优化层，因为考虑到一个节点可能有几百万的一致性组（每个range一个）。少部分优化主要是合并心跳（与数量巨大的range相反，节点数量决定了心跳的数量）和 请求批处理。将来的优化还包括二阶段选举和静态range.
[cockroach设计文档](https://lihuanghe.github.io/2016/05/06/cockroachdb-design.html)

[知乎：CockroachDB和TiDB中的Multi Raft Group是如何实现的?](https://www.zhihu.com/question/54095779)

[TiKV 功能介绍 - Raft 的优化](https://pingcap.com/blog-cn/optimizing-raft-in-tikv/)

1）OB使用的选举算法，选举开始点靠timer对齐，保证网络中的参与者都是“同时”发起选举的；而Raft是一个非同步发起的选举，往往是先开始选举的candidate赢得选举；
2）OB选举算法有一个预投票阶段，可以保证根据特定业务逻辑选主；Raft无法实现特定选主；
3）OB每个选举周期内的投票不持久化，通过实例启动后第一个lease周期内不投票的方式，保证任何一个实例在一个lease周期内都不会重复投票；而Raft每轮的投票是持久化的；
4）OB由于选举起始点需要靠timer对齐，因此对机房的时钟误差有要求；基本假设是最大偏差不超过100ms；Raft论文中明确提出其对timing无依赖；
5）OB允许有主状态下根据指令进行改选，便于运维；
 [oceanbase选举协议](https://www.cnblogs.com/liuhao/p/3860742.html)


[raft算法的局限性](https://mp.weixin.qq.com/s/a3yvatXyfxP5iIzcF_2fdw?)