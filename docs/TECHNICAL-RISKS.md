# 技术风险、性能瓶颈与问题记录

> 记录日期：2026-08-28
>
> 本文是源码审查和当前复现结果的工作记录。`已确认` 表示已经从源码、测试或稳定复现得到证据；`待复现` 表示代码路径明确但还需要真实控制台/协议输入确认；`功能边界` 表示当前没有实现或受构建条件限制，不能当作已支持能力。

## 当前最高优先级回归

### WEBRTC-001：控制台 WHEP 播放报 `No advancing media received`

- **等级**：P0，用户可见，状态为 `根因已确认，修复未关闭`。
- **现象**：控制台在 8 秒后显示 `No advancing media received (check codec support and keyframes)`，用户看不到视频。
- **已确认的数据流**：控制台 `module/api/console.html` 的 `playWHEP` 默认请求 `mode=realtime`；`module/webrtc/whep_feed.go` 从 `startup.LiveCursor` 创建 reader，并在 `gotKeyframe` 变为 true 前丢弃所有视频非关键帧。
- **已确认断点**：如果 `LiveCursor` 位于最近一个关键帧之后，而输入源下一个 IDR 间隔较长、没有继续发送 IDR，或输入源不响应 PLI，则 feed loop 会持续读取并丢弃视频，浏览器在 watchdog 窗口内收不到可解码的首个视频访问单元。`mode=live` 会先发送快照中的 GOP，因此可作为对照组。
- **第二个断点**：`whep_feed.go` 中 `video.WriteSample` 的错误被转换成 `false` 后由调用方忽略，track 关闭、协商 payload type 不匹配、编码器拒绝样本等情况不会进入 session 状态或日志，最终只表现为前端 watchdog 超时。
- **测试证据**：当前 Pion WHEP H.264 RTP、GCC、AAC->Opus、H.264+PCMA，以及 VP8 headless Chrome 测试通过。对当前运行实例的 `live/h264-test`（H.264 + AAC，观测到 GOP 约 3.8-6.1 秒）实测，`mode=realtime` 在 5 秒窗口只有 240 个视频帧，而 `mode=live` 有 1395 个视频帧；Console 浏览器实测 `mode=live` 在约 3.5 秒内得到 640x360、`currentTime` 递增的视频，`mode=realtime` 在后续 IDR 到来前停留在等待状态，8 秒 watchdog 可能先报错，关键帧到达后才恢复 `Playing`。这确认了首帧门控/超时问题，但仍未覆盖真实 GB28181/SIP H.264 的浏览器解码器路径。
- **必须补齐的验证**：记录 WHEP offer/answer 中实际 video codec、publisher codec、startup generation、`LiveCursor`、首个关键帧时间、丢弃帧数量、`WriteSample` 错误和每个 sender 的 RTP 计数；分别验证 `mode=realtime`、`mode=live`、稀疏关键帧、无 GOP cache、GB28181 H.264 和 SIP H.264。
- **验收标准**：默认 Console WHEP 必须在 8 秒内收到可解码视频帧并推进 `currentTime`；首帧前允许等待关键帧，但不能因正常的 GOP 间隔先显示误导性的失败状态，也不能静默丢包或永久等待；显式 realtime 模式若无法及时获得关键帧，必须展示可区分的等待/无关键帧状态；失败时服务端日志必须指出是无关键帧、编码不匹配还是样本写入错误。

### WEBRTC-002：真实 H.264 浏览器覆盖不足

- **等级**：P1，状态为 `测试缺口`。
- 当前 browser jitter 测试主要使用 VP8，H.264 相关端到端测试主要验证 Pion 对端的 RTP，不验证 Chrome/浏览器 H.264 实际解码、profile-level-id、SPS/PPS 和访问单元结构。
- GB28181 输入的 PS 解封装、SIP 输入的 RTP 解包和 WHIP 输入的 RTP 解包可能生成不同的 H.264 payload/关键帧形态；没有一条真实输入到浏览器解码的统一回归路径。

## 已确认的性能瓶颈

以下问题不会因为删除 `audioCache` 自动消失，需要单独处理和基准验证。

| ID | 风险 | 证据位置 | 影响 |
| --- | --- | --- | --- |
| PERF-001 | `Stream.WriteFrame` 在 publisher 校验、反射比较、媒体信息、GOP、统计和 ring 写入期间持有 stream 锁 | `core/stream.go` 的 `WriteFrame`/`writeFrameLocked` | 所有协议推流共享串行临界区，帧率和并发 publisher 增加时锁竞争放大 |
| PERF-002 | `samePublisher` 在帧热路径使用 reflection | `core/stream.go` 的 `samePublisher` | 每帧产生额外类型/可比性判断，削弱高帧率输入吞吐 |
| PERF-003 | 码率限制每帧调用 stats snapshot，包含窗口锁和 `time.Now` | `core/stream.go`、`core/stream_stats.go` | 码率限制打开时 CPU、锁竞争和时间调用开销按帧增长 |
| PERF-004 | 音频转码可能为每个 subscriber 创建 reader-local RingBuffer 和 goroutine | `core/transcode_manager.go`、`module/httpstream/muxer_worker.go`、`module/rtmp/subscriber.go` | 订阅者数量增加时内存、goroutine 和重复搬运增长 |
| PERF-005 | SIP/GB28181 出站 RTP 按 fragment 分配、marshal 和 UDP syscall | `module/gb28181/outbound_media.go`、`module/sipgateway/call_session.go` | 监控流/呼叫数增加时系统调用和 GC 压力高 |
| PERF-006 | Consul/Redis refresh 会完整读取、解析、hash、diff，并在一个 worker 中串行应用 | `config/runtime/manager.go`、`source_consul.go`、`source_redis.go` | 大配置或高刷新频率下阻塞后续 refresh/callback，造成配置延迟 |
| PERF-007 | Prometheus 使用任意 `stream_key` 作为 label | `module/metrics/collector.go` | 高基数流键导致时间序列 churn、内存增长和查询退化 |

## 已确认的架构与可靠性风险

| ID | 风险 | 证据位置 | 影响 |
| --- | --- | --- | --- |
| ARCH-001 | GOP cache 只有 GOP 数量上限，没有单 GOP 的帧数、持续时间和字节上限 | `core/stream.go`、`config/config.go` | 异常稀疏关键帧或超大帧会导致单个 GOP 占用过多内存；`gop_cache_num=1` 不能保证内存有界 |
| ARCH-002 | GB28181 RTP receiver 复用 UDP buffer，重排队列保留 Payload slice | `module/gb28181/rtp_receiver.go` | 后续 ReadFrom 会覆盖已排队 payload，造成偶发 PS/RTP 损坏和难以复现的解码失败 |
| ARCH-003 | 无 publisher 超时只将 stream 标为 `Destroying`，未完整从 StreamHub 移除和释放资源 | `core/stream.go`、`core/stream_hub.go` | 空流对象和关联资源可能长期保留，流键复用时状态边界复杂 |
| ARCH-004 | HTTP module 的 `registered map[*core.Stream]bool` 保留历史 Stream 指针 | `module/httpstream/module.go` | 长时间运行和大量动态流键下内存泄漏式增长 |
| ARCH-005 | `AcquireConn` 使用 Load-then-Add，存在并发超限竞态 | `core/server.go` | 峰值并发可能超过 `max_connections` |
| ARCH-006 | `max_connections` 未覆盖所有会产生连接的路径，DVR 没有 `AcquireConn` | `module/dvr/module.go`、`README.md` | 限流语义不一致，DVR 可绕过全局容量保护 |
| ARCH-007 | HTTP-FLV/TS/fMP4/WebSocket 播放未统一使用 generation-aware subscriber admission | `module/httpstream/handler.go`、`ws_handler.go` | publisher 替换期间可能跨 generation 计数或绕过 per-stream subscriber limit |
| ARCH-008 | `ring_buffer_size=0` 通过 Go validation，但 RingBuffer 取模时可除零/panic | `config/validate.go`、`pkg/util/ringbuffer.go` | 错误配置导致进程崩溃而不是启动期拒绝 |
| ARCH-009 | GB28181 DeviceRegistry 对外暴露可变 `*Device` 和 `Channels` map | `module/gb28181/device_registry.go`、`api.go` | Keepalive/Catalog 更新与 API 读取可能 data race 或观察到半更新状态 |
| ARCH-010 | HTTP streaming 没有清晰的写超时、读 header 超时和慢消费者断开策略 | `module/httpstream/module.go`、`handler.go` | 客户端不读或网络异常时 goroutine、连接和 buffer 可能长时间占用 |
| ARCH-011 | HLS/LL-HLS 阻塞等待使用 `time.Sleep`，没有绑定 request cancellation | `module/httpstream/handler_hls.go` | 客户端断开后请求仍可能等待到超时，浪费 goroutine 和调度时间 |
| ARCH-012 | HLS/DASH/LL-HLS manager cleanup 只按 stream key，不按 publisher generation | `module/httpstream/module.go` | 旧异步 destroy 事件可能删除新 generation 的 manager |
| ARCH-013 | 多处异步 lifecycle event 错误被忽略，背压时 stop/cleanup 事件可能丢失 | 各协议模块 lifecycle 调用点 | 录制、DVR、审计和监控可能与实际 session 状态不一致 |
| ARCH-014 | 配置 URL 中的账号密码可能绕过仅按字段名的脱敏逻辑 | `module/api/config.go` | desired/effective 文档或错误响应可能泄漏 source credentials |
| ARCH-015 | 限流器信任可伪造的 `X-Forwarded-For`/`X-Real-IP` | `pkg/ratelimit/ratelimit.go` | 未配置可信代理时攻击者可绕过 IP 限流 |
| ARCH-016 | `DeviceRegistry.Stop` 和 `ratelimit.Limiter.Close` 非幂等 | 对应模块的 `Stop`/`Close` | 重复 shutdown 或失败回滚可能 panic/重复 close |
| ARCH-017 | WHEP feed loop 的媒体错误和首帧门控状态没有统一的可观测状态模型 | `module/webrtc/whep_feed.go`、`track_sender.go` | 浏览器只能看到笼统的 watchdog 错误，诊断依赖猜测 |

## 功能边界和未完成项

| ID | 当前边界 | 处理方式 |
| --- | --- | --- |
| FUNC-001 | WebRTC simulcast layer selection 和 automatic layer pausing 未实现 | `stream.simulcast.*` 明确标记 deferred/unsupported，不得宣传为已支持 |
| FUNC-002 | 未使用 `audiocodec`/FFmpeg 时，非 AAC 录制和部分输出可能过滤音频并保留纯视频 | 保持可播放视频输出，并在 UI/文档标明构建前提 |
| FUNC-003 | SIP 主要覆盖 H.264 + PCMA/PCMU，GB28181 主要覆盖 H.264 + G.711A | 协议实验室和 API 应对不支持 codec fail closed，并展示原因 |
| FUNC-004 | 当前 WebRTC 浏览器回归没有覆盖真实 GB28181/SIP H.264 输入 | 在 WEBRTC-002 关闭前不能把“Pion RTP 测试通过”当作浏览器播放完整证明 |
| FUNC-005 | 各输出协议对同一 stream 的 codec 能力和音频转码前提仍不完全一致 | 需要建立 capability matrix 和跨协议自动化测试，尤其是 G.711/Opus/AAC |

## `audioCache` 删除后的设计记录

当前没有独立的 `audioCache`。视频流的 GOP cache 以视频关键帧开始，并保存该 GOP 内交错到达的音频；纯音频流不等待关键帧，也不依赖 GOP cache，订阅者从 ring buffer 的 live cursor 读取最新音频。

这个改动本身不会消除上面的锁竞争、ring buffer 边界、转码 reader 生命周期、协议 RTP 分配、配置刷新、指标高基数或 WebRTC 首帧门控问题。它只移除了音频单独缓存带来的重复状态和错误的音频关键帧假设，相关风险必须继续单独验证。

## 后续验证顺序

1. 完成 WEBRTC-001 Phase 1：用控制台真实请求采集 SDP、generation、游标、关键帧、丢弃帧、WriteSample 错误和 RTP 计数。
2. 为已确认的断点添加最小失败测试，优先覆盖 realtime 模式在快照后等待关键帧、稀疏/无关键帧、以及 H.264 真实 payload。
3. 修复并验证 WHEP 后，再按 PERF-001/PERF-003/PERF-007 和 ARCH-001/ARCH-002/ARCH-005/ARCH-008 的风险顺序做基准、race 和故障注入。
4. 关闭功能边界前补齐文档、OpenAPI/schema（若契约变化）、控制台状态和跨协议验收矩阵。

## 当前验证记录

- `go test ./module/webrtc -run 'WHEP|whep|Browser' -count=1 -v`：通过。
- `go test -tags audiocodec ./module/webrtc -run 'TestWHEPPayloadTypeCorrectness|TestWHEPWithGCC|TestWHEPAudioTranscoding|TestValidVideoRTPDelta' -count=1 -v`：通过。
- `go test -tags audiocodec ./module/webrtc -run TestWHEPBrowserJitterDiagnostic -count=1 -v`：通过；VP8 视频和 VP8+AAC->Opus 场景均有推进帧、无丢包、无冻结。
- 当前运行实例的 H.264 对照：`lf-test` WHEP `mode=realtime` 与 `mode=live` 的 5 秒帧数分别为 240 和 1395；浏览器 `mode=live` 已观察到 640x360、`readyState=4`、`paused=false` 且 `currentTime` 递增，浏览器 `mode=realtime` 在首个后续关键帧前保持等待。这些结果关闭了“所有 H.264 RTP 都不可解码”的假设，但 WEBRTC-001 仍未关闭，因为默认 Console 行为和真实 GB28181/SIP H.264 浏览器路径仍需修复与覆盖。
