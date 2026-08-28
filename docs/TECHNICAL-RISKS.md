# 技术风险、性能瓶颈与问题记录

> 记录日期：2026-08-29
>
> 本文是源码审查和当前复现结果的工作记录。`已确认` 表示已经从源码、测试或稳定复现得到证据；`待复现` 表示代码路径明确但还需要真实控制台/协议输入确认；`功能边界` 表示当前没有实现或受构建条件限制，不能当作已支持能力。

## 当前最高优先级回归

### WEBRTC-001：控制台 WHEP 播放报 `No advancing media received`

- **等级**：P0，用户可见，状态为 `默认 Console 与 SIP/GB28181 协议实验室路径已修复并完成真实浏览器验收`。
- **现象**：控制台在 8 秒后显示 `No advancing media received (check codec support and keyframes)`，用户看不到视频。
- **已确认的数据流**：控制台和协议 lab 的默认 WHEP 请求现在使用 `mode=live`；显式 `mode=realtime` 仍从 `startup.LiveCursor` 创建 reader，并在 `gotKeyframe` 变为 true 前丢弃所有视频非关键帧。
- **已确认断点**：如果 `LiveCursor` 位于最近一个关键帧之后，而输入源下一个 IDR 间隔较长、没有继续发送 IDR，或输入源不响应 PLI，则 feed loop 会持续读取并丢弃视频，浏览器在 watchdog 窗口内收不到可解码的首个视频访问单元。`mode=live` 会先发送快照中的 GOP，因此可作为对照组。
- **第二个断点（已修复）**：`whep_feed.go` 中 `video.WriteSample`/`audio.WriteSample` 错误现在进入 WHEP feed 状态和结构化日志；`GET /webrtc/session/{sessionId}/status` 可读取有界诊断。
- **测试证据**：当前 Pion WHEP H.264 RTP、GCC、AAC->Opus、H.264+PCMA，以及 VP8 headless Chrome 测试通过。默认 Console/lab 路径已切到 `mode=live`，显式 `mode=realtime` 仍会在后续 IDR 到来前处于 `waiting_keyframe`；状态 endpoint 已覆盖 generation/cursor/error 读取。新增真实 H.264 Annex-B fixture -> AVCC -> WHEP -> Chromium 回归，已验证 320x180 解码尺寸、ICE connected、无 media error 且 currentTime 连续推进。2026-08-28 独立端口验收进一步验证 SIP 与 GB28181 假设备生成的 H.264 均在 Console WHEP 中得到 160x90、`readyState=4`、无 media error 且 `currentTime` 连续推进。
- **剩余验证**：继续保留显式 `mode=realtime`、稀疏关键帧和无 GOP cache 的状态区分回归；WHIP 真设备输入仍需与协议 Lab 相同的长期浏览器矩阵。
- **验收标准**：默认 Console WHEP 必须在 8 秒内收到可解码视频帧并推进 `currentTime`；首帧前允许等待关键帧，但不能因正常的 GOP 间隔先显示误导性的失败状态，也不能静默丢包或永久等待；显式 realtime 模式若无法及时获得关键帧，必须展示可区分的等待/无关键帧状态；失败时服务端日志必须指出是无关键帧、编码不匹配还是样本写入错误。

### WEBRTC-002：真实 H.264 浏览器覆盖

- **等级**：P1，状态为 `SIP/GB28181 运行时验收已关闭，统一自动化矩阵仍待扩展`。
- 当前 browser jitter 长时测试主要使用 VP8；短路径已验证 Chrome/浏览器 H.264 实际解码、profile-level-id、SPS/PPS 和访问单元结构，独立端口验收已覆盖 SIP RTP 解包与 GB28181 PS 解复用后的 WHEP 浏览器解码。
- WHIP 真设备输入和三种输入的统一长时自动化矩阵仍是测试覆盖改进项，不能用一次运行时验收替代长期回归。

## 性能风险处置状态

以下问题不会因为删除 `audioCache` 自动消失，需要单独处理和基准验证。

| ID | 状态 | 当前结论 | 剩余影响 |
| --- | --- | --- | --- |
| PERF-001 | 部分缓解 | 协议热路径使用稳定 publisher ID，统计写入改成 atomic；`BenchmarkStreamWriteFrame` 为约 55ns、0 alloc | 媒体信息、GOP 和 ring 写入仍由 stream 单写者锁保证顺序，高竞争容量必须用负载测试评估 |
| PERF-002 | 已关闭 | 正常协议 publisher 的每帧 identity 校验不再 reflection；仅空 ID 的 legacy/test publisher 回退到反射比较 | 不应让生产 adapter 使用空 publisher ID |
| PERF-003 | 部分缓解 | 每帧 stats 更新不再等待窗口锁 | 启用 `max_bitrate_per_stream` 时仍会在每帧读取完整 snapshot 和时钟，后续可改成周期更新的原子 bitrate |
| PERF-004 | 未关闭 | 共享 transcode track 已做引用计数，但 reader/goroutine 数仍随独立消费者增长 | 大量不同输出/订阅者仍需内存和 goroutine 容量测试 |
| PERF-005 | 部分缓解 | SIP/GB28181 RTP 改用 session-owned marshal buffer，分别降到 264 B/3 alloc 和 1880 B/6 alloc 每测试帧 | packetizer fragment 分配与每 packet UDP syscall 仍在，批量发送需按平台验证 |
| PERF-006 | 部分缓解 | source I/O 保持串行；相同 source version 或相同 hash 会跳过 diff/application，snapshot 读取为原子且约 0.54ns、0 alloc | 后端仍返回完整变化文档时必须解析/hash，大文档高频刷新仍可能排队 |
| PERF-007 | 已关闭 | per-stream Prometheus series 默认关闭；显式开启后受 `stream_detail_limit` 和可选 exact allowlist 限制；StreamHub 用 O(1) 创建顺序链表让每次抓取最多复制 limit 个流，allowlist 只在 Collector 创建时去重排序一次 | 开启较大 limit 仍由部署方承担 Prometheus cardinality 成本 |

## 架构与可靠性风险处置状态

| ID | 状态 | 处置 |
| --- | --- | --- |
| ARCH-001 | 已关闭 | GOP cache 增加单 GOP 帧数、持续时间和 payload 字节上限，保留关键帧与可播放交错前缀 |
| ARCH-002 | 已关闭 | RTP receiver 在进入重排队列前取得 payload 所有权，并有 buffer alias 回归测试 |
| ARCH-003 | 已关闭 | idle/no-publisher timeout 通过带 instance/generation 的 callback 从 StreamHub 删除匹配对象 |
| ARCH-004 | 已关闭 | HTTP 注册表改为 stream key/instance/generation 元数据，不保留历史 Stream 指针 |
| ARCH-005 | 已关闭 | `AcquireConn` 使用 CAS 严格接纳，并通过并发测试验证不超限 |
| ARCH-006 | 已关闭 | DVR handler 在 session 前申请全局连接配额，并在所有返回路径 release-once |
| ARCH-007 | 已关闭 | HTTP-FLV/TS/fMP4/WebSocket、RTMP、RTSP、SRT subscriber 均绑定一个 startup generation lease |
| ARCH-008 | 已关闭 | typed config 拒绝非正 ring size，工具层构造函数对非法容量使用一槽 fallback |
| ARCH-009 | 已关闭 | DeviceRegistry 对外返回深拷贝 snapshot，内部 channel map 不再逃逸 |
| ARCH-010 | 已关闭 | HTTP server 配置 header/idle timeout；请求入口不提前设置 write deadline，HLS/DASH 等待完成后才在 manifest/init/segment 实际写入前刷新 10 秒期限；HTTP-FLV/TS/fMP4 每次 write/flush 与 WebSocket 每次 write 同样使用逐次期限，stream loop 响应 request cancellation |
| ARCH-011 | 已关闭 | HLS/DASH/LL-HLS 等待使用 context-aware timer/condition，客户端取消立即退出 |
| ARCH-012 | 已关闭 | manager/muxer cleanup 校验 stream instance 和 publisher generation，旧事件不能删除替代 generation |
| ARCH-013 | 已关闭 | lifecycle start 在 EventBus admission 成功后才标记 started；失败回滚资源；stop lane 按 consumer 的全部 terminal hooks 预留，shutdown 有界 drain |
| ARCH-014 | 已关闭 | Config 文档、source details 和 runtime last error 脱敏 URL userinfo/query/fragment；secret map/sequence 保留结构，token/ICE/endpoint 集合按稳定身份恢复，增删不能错配密文，身份歧义拒绝 Apply；编辑 URL 只恢复旧 secret 组件 |
| ARCH-015 | 已关闭 | 默认忽略 forwarded headers；仅可信代理 IP/CIDR 可提供 client IP；XFF 从右向左剥离可信跳点并选择首个不可信来源，攻击者左前缀不能切换限流桶；非法配置启动期拒绝 |
| ARCH-016 | 已关闭 | DeviceRegistry、Limiter 和 Server shutdown 使用 once/幂等关闭语义 |
| ARCH-017 | 已关闭 | WHEP session 状态区分 waiting/playing/no-input/codec/write/generation/closed，公开实际 RTP 包/字节和收到的 RTCP 包；feed 终止自动释放 session 全部资源，最多 64 条终态保留两分钟 |
| ARCH-018 | 已关闭 | 录像轮转保留 publisher 声明轨道和深拷贝的最新音视频序列头，按轨道归零文件内时间轴；TS 首媒体前写 PAT/PMT，经典 MP4 独立计算音视频 duration、将 `mvhd/tkhd` 归一到 movie timescale、保留 `mdhd` 轨道 timescale、边界值饱和而不回绕、负 PTS-DTS 使用 `ctts` version 1、非负保持 version 0，并使用可扩展 AAC ESDS 长度；逐格式解析回归覆盖，超长单文件仍需保留轮转 |
| ARCH-019 | 已关闭 | Server info 公开当前进程真实音频转码能力；Console fMP4 根据有效输出 codec 而非 G.711 源 codec 创建 MSE SourceBuffer，避免含 AAC 初始化段被视频-only MIME 拒绝 |
| ARCH-020 | 已关闭 | SIP receive 将所选 PCMA/PCMU 作为真实协商目标；源 codec 不同时使用 generation 绑定的独立目标音频 reader，H.264 保持原始 live cursor，并在无可用转换时信令前失败 |
| ARCH-021 | 已关闭 | GB28181 live/playback 成功 INVITE 将托管 dialog 交给 MediaSession；停止、receiver failure 和回滚汇聚到一次 BYE/Close，重复停止幂等 |
| ARCH-022 | 已关闭 | publisher identity 匹配要求流仍处于 publishing 且当前 publisher 非空；旧 `lastPublisherID` 不能重复解绑 generation 或重置 no-publisher timer |
| ARCH-023 | 已关闭 | SIP Gateway RTP/RTCP pair 在 allocator 锁内完成双 socket 绑定，跳过外部占用，并从 SDP 协商前持有到 session cleanup；本地 Lab 假端点避开配置范围，消除编号分配到实际 bind 之间的 TOCTOU |
| ARCH-024 | 已关闭 | GB28181 入站 INVITE 在 2xx 前完成异步 publish-start admission，backpressure 回滚 publisher/session/socket/stream/port 且不发未配对 stop；GB28181 receive Lab 原子分配并绑定 RTP/RTCP，SIP/GB 一键自测也实际绑定配置 pair 并检测外部端口耗尽 |

## 功能边界和未完成项

| ID | 当前边界 | 处理方式 |
| --- | --- | --- |
| FUNC-001 | WebRTC simulcast layer selection 和 automatic layer pausing 未实现 | `stream.simulcast.*` 明确标记 deferred/unsupported，不得宣传为已支持 |
| FUNC-002 | 未使用 `audiocodec`/FFmpeg 时，非 AAC 录制和部分输出可能过滤音频并保留纯视频 | 保持可播放视频输出，并在 UI/文档标明构建前提 |
| FUNC-003 | SIP 主要覆盖 H.264 + PCMA/PCMU，GB28181 主要覆盖 H.264 + G.711A | 协议实验室和 API 应对不支持 codec fail closed，并展示原因 |
| FUNC-004 | SIP/GB28181 H.264 已有真实 Console WHEP 验收，但尚未形成统一长时自动化矩阵 | 保留协议输入到浏览器的自动化扩展项；不能把一次运行时验收当作长期容量证明 |
| FUNC-005 | G.711A 源已实测 HTTP-FLV/WS-FLV/HTTP-TS/fMP4/HLS/DASH/WHEP，SIP PCMA->PCMU 和 SIP->GB28181/GB28181->SIP 双向 receive 已实测；其他 codec 组合仍未穷举 | 继续建立 capability matrix 和跨协议自动化测试，尤其是 Opus/AAC/H.265 组合 |

## `audioCache` 删除后的设计记录

当前没有独立的 `audioCache`。视频流的 GOP cache 以视频关键帧开始，并保存该 GOP 内交错到达的音频；纯音频流不等待关键帧，也不依赖 GOP cache，订阅者从 ring buffer 的 live cursor 读取最新音频。

这个改动本身不会消除上面的锁竞争、ring buffer 边界、转码 reader 生命周期、协议 RTP 分配、配置刷新、指标高基数或 WebRTC 首帧门控问题。它只移除了音频单独缓存带来的重复状态和错误的音频关键帧假设，相关风险必须继续单独验证。

## 后续验证顺序

1. 把已完成的真实 SIP/GB28181 Lab 到 Chromium 验收固化为统一长时自动化矩阵，并加入 WHIP 真设备输入。
2. 对 PERF-001/PERF-003/PERF-004/PERF-005/PERF-006 做多 publisher/多 subscriber 长时容量测试；微基准不能替代该测试。
3. 关闭功能边界前补齐 source、OpenAPI/schema、Console 状态和跨协议 codec matrix。

## 当前验证记录

- `go test ./module/webrtc -run 'WHEP|whep|Browser' -count=1 -v`：通过。
- `go test -tags audiocodec ./module/webrtc -run 'TestWHEPPayloadTypeCorrectness|TestWHEPWithGCC|TestWHEPAudioTranscoding|TestValidVideoRTPDelta' -count=1 -v`：通过。
- `go test -tags audiocodec ./module/webrtc -run TestWHEPBrowserJitterDiagnostic -count=1 -v`：通过；VP8 视频和 VP8+AAC->Opus 场景均有推进帧、无丢包、无冻结。
- `go test ./module/record -count=1`、`go test -race ./module/record -count=1`、`CGO_ENABLED=1 go test -tags audiocodec -race ./module/record -count=1`：通过；覆盖 fMP4/FLV/MP4/TS 轮转后的完整轨道初始化。
- `go test ./module/sipgateway -count=1`、`go test -race ./module/sipgateway -count=1`、`CGO_ENABLED=1 go test -tags audiocodec -race ./module/sipgateway -count=1`：通过；包含 PCMA 源到请求 PCMU 的真实 RTP/RTCP Lab 转码回归。
- 2026-08-28 独立端口 Console 验收：GB28181 G.711A 源的 HTTP-FLV、WS-FLV、HTTP-TS、fMP4、HLS、DASH、WHEP 均解码为 160x90 且媒体时钟推进；SIP WHEP 同样为 160x90、`readyState=4` 并推进。GB28181->SIP PCMU receive 音频/视频/RTCP 计数增长，SIP PCMA->GB28181 receive 的 RTP/RTCP/PS 发送和接收计数一致。SIP 11 项与 GB28181 13 项一键自测全部通过。
- 当前运行实例的 H.264 对照：`lf-test` WHEP `mode=realtime` 与 `mode=live` 的 5 秒帧数分别为 240 和 1395；浏览器 `mode=live` 已观察到 640x360、`readyState=4`、`paused=false` 且 `currentTime` 递增，浏览器 `mode=realtime` 在首个后续关键帧前保持等待。新增 fixture 浏览器回归通过，关闭了“所有 H.264 RTP 都不可解码”的假设；默认 Console 入口以及真实 SIP/GB28181 Lab H.264 浏览器路径已关闭，剩余缺口是 WHIP 真设备和统一长时自动化矩阵。
- WHEP 音频样本写入失败现在从缓存、直读和转码 reader 三条路径立即终止 feed；连接后 8 秒完全没有输入会进入可恢复的 `no_media_input`，后续媒体恢复为 `playing`；无效 H.264/H.265 参数集和空访问单元进入 `codec_mismatch`。
- `go test ./pkg/muxer/mp4 ./module/gb28181 ./module/httpstream ./pkg/ratelimit ./core ./module/metrics -count=1` 的对应包级回归均通过；覆盖负 CTS、GB28181 回放 BYE、延迟 HLS/DASH 写 deadline、XFF 前缀绕过、稳定有界指标迭代和重复 publisher 清理。
- `go test ./module/sipgateway -count=5 -timeout=120s`：通过；覆盖外部占用 pair 跳过、SDP 前 socket 绑定、Lab 范围避让和失败清理顺序。
- 2026-08-28 Apple M1 Pro 微基准：Stream write 54.8-55.0ns/0 alloc；Ring TryRead 37.6-37.7ns/0 alloc；Ring immediate context read 40.1-40.5ns/0 alloc；GB28181 outbound 6.43-6.62us/1880 B/6 alloc；SIP outbound 4.49-4.57us/264 B/3 alloc。结果仅用于同机相对回归。
