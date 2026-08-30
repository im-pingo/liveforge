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
- **第二个断点（已修复）**：`whep_feed.go` 中 `video.WriteSample`/`audio.WriteSample` 错误现在进入 WHEP feed 状态和结构化日志；每次真实迁移包含 generation、cursor、mode、前后状态和有界错误，同状态逐帧更新不重复记录；`GET /webrtc/session/{sessionId}/status` 可读取首媒体时间、固定等待毫秒数和有界诊断。
- **测试证据**：当前 Pion WHEP H.264 RTP、GCC、AAC->Opus、H.264+PCMA，以及 VP8 headless Chrome 测试通过。默认 Console/lab 路径已切到 `mode=live`，显式 `mode=realtime` 仍会在后续 IDR 到来前处于 `waiting_keyframe`；状态 endpoint 已覆盖 generation/cursor/error 读取。新增真实 H.264 Annex-B fixture -> AVCC -> WHEP -> Chromium 回归，已验证 320x180 解码尺寸、ICE connected、无 media error 且 currentTime 连续推进。2026-08-28 独立端口验收进一步验证 SIP 与 GB28181 假设备生成的 H.264 均在 Console WHEP 中得到 160x90、`readyState=4`、无 media error 且 `currentTime` 连续推进。
- **剩余验证**：继续保留显式 `mode=realtime`、稀疏关键帧和无 GOP cache 的状态区分回归；统一矩阵的长期 soak 和并发容量仍需独立运行，不能由短时正确性矩阵替代。
- **验收标准**：默认 Console WHEP 必须在 8 秒内收到可解码视频帧并推进 `currentTime`；首帧前允许等待关键帧，但不能因正常的 GOP 间隔先显示误导性的失败状态，也不能静默丢包或永久等待；显式 realtime 模式若无法及时获得关键帧，必须展示可区分的等待/无关键帧状态；失败时服务端日志必须指出是无关键帧、编码不匹配还是样本写入错误。

### WEBRTC-002：真实 H.264 浏览器覆盖

- **等级**：P1，状态为 `SIP/GB28181/WHIP 统一自动化矩阵已实现并通过短时 soak`。
- 统一 Chromium 矩阵覆盖 SIP publish -> GB28181 receive + WHEP、GB28181 publish -> SIP receive + WHEP、WHIP H.264/Opus publish -> SIP receive + GB28181 receive + WHEP。它校验真实解码尺寸、媒体时钟、音视频 RTP/解码帧、RTCP、ICE 和服务端非 stalled 状态。
- `LIVEFORGE_PROTOCOL_MATRIX_SOAK` 可逐秒扩展推进检查；2026-08-29 已通过 15 秒/场景的自动化运行。Chrome 缺失时测试会 skip，默认 soak 为零，因此 CI 必须具备 Chromium 并显式启用 soak 才能把它当作发布门禁。该矩阵证明协议正确性，不证明并发会话、长时背压或部署容量。

## 性能风险处置状态

以下问题不会因为删除 `audioCache` 自动消失，需要单独处理和基准验证。

| ID | 状态 | 当前结论 | 剩余影响 |
| --- | --- | --- | --- |
| PERF-001 | 部分缓解 | 协议热路径使用稳定 publisher ID，统计写入改成 atomic；Apple M1 Pro、Go 1.26.0 的真实 `WriteFrameForPublisher` + ring + 交错 GOP fixture 为 65.86-67.28 ns/op、29 B/op、0 alloc/op；它使用共享只读 payload 的预分配 64 秒单调时间戳帧池，零 subscriber，关闭 bitrate limit，保留 2 个 GOP，单 GOP 上限 300 帧，ring 为 4096 项；该路径覆盖 publisher 身份、ring 和 GOP，不与旧的直接 `BenchmarkStreamWriteFrame` 数字比较 | 媒体信息、GOP 和 ring 写入仍由 stream 单写者锁保证顺序；启用 `max_bitrate_per_stream` 的额外成本见 PERF-003；多 publisher 争用不是正常单流拓扑，真实多流/多订阅者容量仍需负载测试 |
| PERF-002 | 已关闭 | 正常协议 publisher 的每帧 identity 校验不再 reflection；仅空 ID 的 legacy/test publisher 回退到反射比较 | 不应让生产 adapter 使用空 publisher ID |
| PERF-003 | 部分缓解 | 每帧 stats 更新不再等待窗口锁 | 启用 `max_bitrate_per_stream` 时仍会在每帧读取完整 snapshot 和时钟，后续可改成周期更新的原子 bitrate |
| PERF-004 | 未关闭 | 共享 transcode track 已做引用计数，但 reader/goroutine 数仍随独立消费者增长 | 大量不同输出/订阅者仍需内存和 goroutine 容量测试 |
| PERF-005 | 部分缓解 | SIP/GB28181 RTP 改用 session-owned marshal buffer，分别降到 264 B/3 alloc 和 1880 B/6 alloc 每测试帧 | packetizer fragment 分配与每 packet UDP syscall 仍在，批量发送需按平台验证 |
| PERF-006 | 部分缓解 | source I/O 保持串行；相同 source version 或相同 hash 会跳过 diff/application，snapshot 读取为原子且约 0.54ns、0 alloc | 后端仍返回完整变化文档时必须解析/hash，大文档高频刷新仍可能排队 |
| PERF-007 | 部分缓解 | per-stream Prometheus series 默认关闭；无 allowlist 时 Collector 按创建顺序进行生命周期接纳，容量满后不驱逐标量 key，重复 gather 与并发 race 回归证明 churn 不会产生超过 limit 的新 `stream_key`；exact allowlist 仍只允许配置键并在 Collector 创建时去重排序；Apple M1 Pro、128 个活跃流、limit 32 的 Gather-only 微基准中，首次接纳为 126892-127899 ns/op、169326 B/op、2683 allocs/op，稳定 Gather 为 133365-136777 ns/op、164580-164581 B/op、2667 allocs/op | 较大 limit 或较大 exact allowlist 的 cardinality 与采集成本仍由部署方承担；这些数字只描述单机固定 fixture 的 Collector Gather 路径，不能作为 stream、scrape、并发或部署容量结论 |
| PERF-008 | 未关闭 | 同机生产 egress fixture 覆盖完整 RTMP FLV/chunk framing、RTSP H.264 packetizer/RTP/interleaved framing 和 relay accounting：RTMP H.264 155.1-155.6 ns/op、24 B/op、3 allocs/op，RTMP AAC 73.60-73.76 ns/op、21 B/op、3 allocs/op，RTSP 单 NAL H.264 1.825-1.833 us/op、4044 B/op、9 allocs/op，三包 FU-A H.264 4.593-4.605 us/op、9892 B/op、23 allocs/op；relay first/batch/threshold/terminal 分别为 7.700-7.751、6.256-6.289、31.50-31.52、7.765-7.792 ns/op 且均为 0 alloc | 两种 egress 都使用固定时间戳媒体帧并终止于有界内存 writer，不含 socket write、deadline、TCP writev 和内核/网络 syscall；RTMP 按 payload、RTSP 按 framed bytes 统计，独立 accounting 数字排除 context lookup，主要用于 allocation 回归；RTSP packetization/marshal 分配仍明显，仍需真实连接、并发订阅和背压负载测试 |

## 架构与可靠性风险处置状态

| ID | 状态 | 处置 |
| --- | --- | --- |
| ARCH-001 | 已关闭 | GOP cache 增加单 GOP 帧数、持续时间和 payload 字节上限，保留关键帧与可播放交错前缀 |
| ARCH-002 | 已关闭 | RTP receiver 在进入重排队列前取得 payload 所有权，并有 buffer alias 回归测试 |
| ARCH-003 | 已关闭 | idle/no-publisher timeout 通过带 instance/generation 的 callback 从 StreamHub 删除匹配对象 |
| ARCH-004 | 已关闭 | HTTP 注册表改为 stream key/instance/generation 元数据，不保留历史 Stream 指针 |
| ARCH-005 | 已关闭 | `AcquireConn` 使用 CAS 严格接纳，并通过并发测试验证不超限 |
| ARCH-006 | 已关闭 | DVR handler 在 session 前申请全局连接配额，成功、错误、客户端取消和写超时路径均 release-once；有限 playlist/segment 响应使用 10 秒 server `WriteTimeout`，stalled peer 不能无限持有槽位 |
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
| ARCH-017 | 已关闭 | WHEP 对非零且接收方向的每条请求源轨 fail closed：媒体级方向优先并继承会话级方向，codec 只精确匹配该 m-line 列出的 payload `rtpmap`；codec 不兼容返回 415，内部 track/AddTrack 失败返回 500，并释放 generation lease、连接槽、PeerConnection 和 session；禁用/非接收 m-line 可有意省略且不增加 dropped。状态区分 waiting/playing/no-input/media-stalled/codec/write/generation/closed，公开 expected 音视频、首个成功样本时间及固定等待毫秒数、分轨最后推进时间、实际 RTP 包/字节和收到的 RTCP 包；真实状态迁移写一次带上下文的结构化日志，同状态逐帧更新不写；Close 在终态记录前捕获一次最终单调 transport snapshot；混合流全部期望轨推进后才 playing，视频首个 IDR 前即使音频推进也保持 waiting-keyframe，启动后任一轨 8 秒不推进才 stalled，全部恢复才 playing，Console 仅显示实际过期轨；原子终态拒绝普通迟到媒体/watchdog/transport 更新，feed 终止自动释放全部资源，最多 64 条终态保留两分钟 |
| ARCH-018 | 已关闭 | 录像轮转保留 publisher 声明轨道和深拷贝的最新音视频序列头，按轨道归零文件内时间轴；TS 首媒体前写 PAT/PMT，经典 MP4 独立计算音视频 duration、将 `mvhd/tkhd` 归一到 movie timescale、保留 `mdhd` 轨道 timescale、边界值饱和而不回绕、负 PTS-DTS 使用 `ctts` version 1、非负保持 version 0，并使用可扩展 AAC ESDS 长度；逐格式解析回归覆盖，超长单文件仍需保留轮转 |
| ARCH-019 | 已关闭 | Server info 公开当前进程真实音频转码能力；Console fMP4 根据有效输出 codec 而非 G.711 源 codec 创建 MSE SourceBuffer，避免含 AAC 初始化段被视频-only MIME 拒绝 |
| ARCH-020 | 已关闭 | SIP receive 将所选 PCMA/PCMU 作为真实协商目标；源 codec 不同时使用 generation 绑定的独立目标音频 reader，H.264 保持原始 live cursor，并在无可用转换时信令前失败 |
| ARCH-021 | 已关闭 | GB28181 live/playback 成功 INVITE 将托管 dialog 交给 MediaSession；停止、receiver failure 和回滚汇聚到一次 BYE/Close，重复停止幂等 |
| ARCH-022 | 已关闭 | publisher identity 匹配要求流仍处于 publishing 且当前 publisher 非空；旧 `lastPublisherID` 不能重复解绑 generation 或重置 no-publisher timer |
| ARCH-023 | 已关闭 | SIP Gateway RTP/RTCP pair 在 allocator 锁内完成双 socket 绑定，跳过外部占用，并从 SDP 协商前持有到 session cleanup；本地 Lab 假端点避开配置范围，消除编号分配到实际 bind 之间的 TOCTOU |
| ARCH-024 | 已关闭 | GB28181 入站 INVITE 在 2xx 前完成异步 publish-start admission，backpressure 回滚 publisher/session/socket/stream/port 且不发未配对 stop；GB28181 receive Lab 原子分配并绑定 RTP/RTCP，SIP/GB 一键自测也实际绑定配置 pair 并检测外部端口耗尽 |
| ARCH-025 | 已关闭 | HLS/DASH/LL-HLS publish-stop 仅退休匹配 generation 的请求查找，manager 排空捕获的 generation end cursor 后一次完成；替代 generation 使用独立 manager 且无帧串入。LL-HLS 条件等待同时响应 request/hold 取消和 manager stop，HTTP 模块关闭仍强制停止并等待 active/draining worker |
| ARCH-026 | 已关闭 | DVR publish admission 将一次校验通过的 publisher-generation snapshot 贯穿索引/存储恢复和 session 构造，安装前再次校验相同 stream generation；设置期间替代 publisher 会丢弃候选并只关闭候选取得的资源，不能组合旧 identity 与新 media，也不能覆盖新 session |
| ARCH-027 | 已关闭 | 非空 publisher ID 在单个 Stream 生命周期内只能使用一次；A -> B -> A 在修改 timer/state/generation/media/GOP/ring 前拒绝，旧 A 的延迟写入、活动和清理不能命中新 owner；身份集合不跨 Stream 保存 |
| ARCH-028 | 已关闭 | GOP 帧数、时长和字节上限热更新会从保留前缀清除并重新计算当前 seal；收紧保持关键帧开头的可播放前缀，放宽只接纳后续交错帧，不恢复已省略或裁剪历史，下一个关键帧开始完整新 GOP |
| ARCH-029 | 已关闭 | `Destroying` 是 Stream 的不可逆终态；显式关闭、idle/no-publisher timeout 和策略触发销毁后的延迟 publisher 清理不能恢复 `NoPublisher`，不能挂接非空或空 ID publisher，也不会重复销毁通知或重开 ring |
| ARCH-030 | 已关闭 | 共享转码输出使用携带 source-ring `SourceSpan` 的内部 envelope；snapshot reader 以 reader-local `SourceCursor` floor 按 `Begin >= floor` 过滤同 epoch 历史，跨 floor packet 也丢弃；track 按值保留最近 8 个 epoch 的目标序列头，lagging bridge 在首个可接受 payload 前只补发相同 epoch 的头，匹配头已超出边界时丢弃 AAC payload 而不把 miss 视为满足；直接帧保持原指针/载荷，decode/resample/PCM 聚合/encode/drain 保留有效保守归因 |
| ARCH-031 | P1，未关闭 | 共享转码 producer 已通过原子 read result 在 source overwrite 时丢弃事件所带保留帧、禁止 clean tail 并以精确 skip count 终止该 generation track，内部 output bridge overwrite 也只关闭该 reader；连续 HTTP-FLV/HTTP-TS/fMP4 与 HTTP/WebSocket 输出均 fail closed。HLS/LL-HLS 已丢弃 partial/retained media、推进 live cursor、刷新同 generation 容器状态；刷新后的音频计划若在直接源与共享转码源间变化，会一次关闭/释放旧 reader 并从 live cursor 打开新 reader，LL-HLS 也会按刷新后的 topology 重置视频关键帧 gate。视频等待关键帧、纯音频直接恢复，并只对首个恢复 segment/part 标记 discontinuity；LL-HLS 还会一次性放弃 MSN、清除已公布 part 并唤醒 blocking reload，已公告的 fMP4 media 各自保留匹配且不可变的 init epoch，旧版本 URL 保留到对应 media 被窗口淘汰。DASH 保留已完成的单 Period init/timeline，丢弃当前 batch 后退休 manager；active-generation transformed EOF 不再 clean flush。SIP 出站现在独立保留 source 与 target-audio 的原子 overwrite 事件，丢弃保留帧并只推进受影响 reader；source overwrite 继续有效转码音频并让直接 H.264 等待同 generation 最新序列头加 IDR，target-audio overwrite 则保持直接视频连续并从 live 恢复音频。最终 RTP 准入在终态 send gate 下复查取消状态和当前 generation；终态先关闭准入和自有 socket，不持有 lifecycle/admission 锁等待已准入发送退出，之后才发布状态与回调。active-generation target-audio EOF 以 `network_lost` 终止呼叫，双 reader 父循环会取消并等待两个 pump 后才返回。GB28181 出站也保留 source/target-audio reader identity 和精确 overwrite 数，丢弃 retained 值及受影响的 20ms holdback，只推进发生覆盖的 reader；wait-only pump 仅排队 reader readiness，merge 串行执行原子 read 并在待发媒体前优先排空 control，因此已经观察到的 source overwrite、target-audio overwrite 或 active target EOF 不能再被 gap 前 RTP 越过。source overwrite 清除待发视频并创建新 PS muxer，同时保留 SSRC/RTP sequence、继续有效目标音频，直到同 generation gap 后最新 sequence header 加 IDR 才恢复视频。target-audio overwrite 仅清除待发目标音频，保持干净的源视频与 PS 状态，并保留该视频原有的 holdback deadline；active-generation target-audio EOF 会在待发 RTP 前失败，所有终态都会取消并等待两个 pump。WHEP 为 source/target-audio 各自保留独立的 condition-backed pump，由同一个 pump 串行执行 readiness、原子读取和 live 推进；source overwrite 保留原 generation 与已建立音频，重置视频 pacing/DTS/PTS 并通过 TrackSender gate 等待同 generation 最新参数集加关键帧，target-audio overwrite 不扰动干净视频，active target-audio EOF 立即进入 `target_audio_failed`，关闭会取消并等待两个 pump 后再 release-once。RTMP、RTSP、SRT、cluster、Record、DVR 仍需各自处理，因此本项保持未关闭 |
| ARCH-032 | 已关闭 | GOP duration admission/热更新使用溢出安全的无序 min/max DTS span，保留插入顺序并在越界前封存；启用 GOP 时至少要求正的帧数或字节硬上限，直接构造缺失硬上限时使用 300 帧防御默认，避免等 DTS 帧导致无界增长 |
| ARCH-033 | 已关闭 | API、WebRTC 和 metrics HTTP server 统一配置 `ReadHeaderTimeout=5s`、`IdleTimeout=2m`，慢 header 在限流前有界、空闲 keep-alive 连接不会长期占用资源；现有 handler/media write deadline 和行为保持不变，三模块 focused tests 已验证 |
| ARCH-034 | P2，未关闭 | EventBus 非 lifecycle 异步事件按 hook 启动无界 goroutine；Alive 与通知 WebSocket 缺少统一 queue/admission/pressure 指标 |
| ARCH-035 | P2，未关闭 | file 和 Redis 配置源没有统一 document/materialization 字节上限；`os.ReadFile`、Redis hash/prefix 全量读取可能造成无界内存和命令批量 |
| ARCH-036 | P2，未关闭 | SIP egress 将 packetize 错误、空输出和部分 marshal 错误当作成功，调用保持 active 且没有 `last_error`/drop 观测 |
| ARCH-037 | P3，未关闭 | SIP/GB28181 Lab manager 的 active session 没有显式 admission ceiling；底层端口/呼叫限制只能间接约束资源 |

## Config、Storage 与 Record 待关闭项

| ID | 等级/状态 | 当前问题 |
| --- | --- | --- |
| CONFIG-001 | 已关闭 | viewer Validate 不展开服务端进程环境变量；受信任的 runtime source 加载保留环境变量展开 |
| CONFIG-002 | 已关闭 | viewer Validate 只接受单个 YAML/JSON 文档并拒绝未知 root/nested typed fields；Apply/source loading 对未映射 source 字段保持宽松 |
| CONFIG-003 | 已关闭 | schema secrets、`api_key`、`tls.key_file` 和 URL path-token 已不透明脱敏并稳定恢复；原始 source 中即使路径符合 digest marker 语法也会再次 hash，只有 candidate 将该语法视为占位符且必须匹配当前原始路径计算出的 digest；未映射字段中的有效 absolute hierarchical URL scalar 也按 value 识别，reorder 按 stable digest/public identity 恢复且 edit/ambiguity fail closed；普通 string、duration、ID、bare host/address 保持不变；Consul GET/PUT 拒绝 redirect 且不转发 `X-Consul-Token` |
| CONFIG-004 | 已关闭 | Redis document 与可选 version increment 在一个 `MULTI/EXEC` 事务中排队；事务/EXEC 错误由 `RedisSource.Write` 返回，Apply 只有写入成功才返回 202 |
| CONFIG-005 | 已关闭 | FileSource 新目标明确使用 `0600`，原有文件替换时保留既有 permission bits；`TestFileSourceWriteUsesPrivateModeForNewDocument` 覆盖新文件路径 |
| CONFIG-006 | 已关闭 | Consul/Redis flattened dotted/slashed key 先规范化、排序，再拒绝重复路径和 scalar/container 前缀冲突；错误顺序由测试固定 |
| CONFIG-007 | 已关闭 | Console Apply 捕获提交文本和单调 editor revision，过期 desired refresh 只有在 revision 未变化时才能回填；browser race regression 覆盖新编辑优先 |
| CONFIG-008 | 已关闭 | OpenAPI Apply 202 使用 `ConfigApplyResponse` 的 `written_and_refresh_scheduled`，独立 refresh 仍使用 `scheduled`；contract test 校验引用和 schema |
| STORAGE-001 | 已关闭 | Record 与 DVR 都在等待 admission/setup 锁前捕获唯一绝对 drain deadline；调用方在该边界返回 timeout，已经启动的清理继续后台完成 |
| STORAGE-002 | 已关闭 | recording play/download 在打开媒体前申请全局连接槽，每条成功/错误路径 release-once，并在 `ServeContent` 前设置 10 秒写期限；metadata/list/status/delete 不额外占用媒体槽 |
| STORAGE-003 | 已关闭 | 自动轮转在阈值后立即停止时不创建空后继；视频在阈值后首个有效关键帧前轮转，纯音频在阈值后首个音频帧前轮转；无 `{time}` 的固定模板也会为后继生成唯一且排他创建的路径 |
| STORAGE-004 | 已关闭 | audio-only DVR 按音频媒体时间达到 segment duration 时轮转并立即发布分片，publisher 持续在线不依赖视频关键帧；带媒体的分片经过在线 demux 验证 |
| STORAGE-005 | 已关闭 | DVR route 严格拒绝非法/编码分隔符；嵌套 stream key 保持层级，playlist 对每个 key segment 单独 `PathEscape`，保留 `?`、`#`、`%` 不改变资源边界 |
| STORAGE-006 | 已关闭 | Record 与 FileWriter 共用格式和字节大小校验；支持 `flv`、`fmp4`、`mp4`、`ts`，并将 `hls` 作为 TS 存储 alias；空值/零值关闭 max-size，其他值只接受非负十进制 B/KB/MB/GB |
| STORAGE-007 | 已关闭 | `/api/v1/server/info` 从已初始化 DVR 的绑定 listener 发现非零端口，并通过 `endpoint_schemes.dvr` 报告实际 `http`/`https` scheme；未初始化时才回退配置值 |
| STORAGE-008 | 已关闭 | recording download 只服务 `completed`；active 或 failed recording 返回 JSON `409` 且不返回媒体，OpenAPI 与 inline play 保持同一语义 |
| STORAGE-009 | 已关闭 | `!audiocodec` 自动化覆盖 H.264 + G.711 DVR fallback：TS 分片可 demux 为视频、没有音频帧；带 FFmpeg 的构建仍由 tagged transcode 测试覆盖 |

## 功能边界和未完成项

| ID | 当前边界 | 处理方式 |
| --- | --- | --- |
| FUNC-001 | WebRTC simulcast layer selection 和 automatic layer pausing 未实现 | `stream.simulcast.*` 明确标记 deferred/unsupported，不得宣传为已支持 |
| FUNC-002 | 未使用 `audiocodec`/FFmpeg 时，非 AAC 录制和部分输出可能过滤音频并保留纯视频 | 保持可播放视频输出，并在 UI/文档标明构建前提 |
| FUNC-003 | SIP 主要覆盖 H.264 + PCMA/PCMU，GB28181 主要覆盖 H.264 + G.711A | 协议实验室和 API 应对不支持 codec fail closed，并展示原因 |
| FUNC-004 | SIP/GB28181/WHIP 已形成统一 Chromium 正确性矩阵，但 Chrome 可缺席且默认不 soak | 发布门禁必须提供 Chromium 并显式运行长时 soak；不能把短时矩阵当作容量证明 |
| FUNC-005 | G.711A 源已实测 HTTP-FLV/WS-FLV/HTTP-TS/fMP4/HLS/DASH/WHEP；矩阵覆盖 SIP/GB28181/WHIP H.264 和 PCMA/PCMU/G.711A/Opus 的关键转换，其他 codec 组合仍未穷举 | 继续扩展 capability matrix，尤其是 AAC/H.265 和无 FFmpeg fallback |

## `audioCache` 删除后的设计记录

当前没有独立的 `audioCache`。视频流的 GOP cache 以视频关键帧开始，并保存该 GOP 内交错到达的音频；纯音频流不等待关键帧，也不依赖 GOP cache，订阅者从 ring buffer 的 live cursor 读取最新音频。

这个改动本身不会消除上面的锁竞争、ring buffer 边界、转码 reader 生命周期、协议 RTP 分配、配置刷新、指标高基数或 WebRTC 首帧门控问题。它只移除了音频单独缓存带来的重复状态和错误的音频关键帧假设，相关风险必须继续单独验证。

## 后续验证顺序

1. 先关闭剩余的 ARCH-031 媒体正确性 P1，再处理 Config/Storage/Record 的 P1。
2. 关闭 ARCH-032..ARCH-037 和 Config/Storage 的 P2/P3，并补齐 source、OpenAPI/schema 和 Console 状态。
3. 显式运行 60 秒统一协议 soak，再对 PERF-001/PERF-003/PERF-004/PERF-005/PERF-006 做多 publisher/多 subscriber 长时容量测试；微基准和短时矩阵都不能替代容量测试。

## 当前验证记录

- `go test ./module/webrtc -run 'WHEP|whep|Browser' -count=1 -v`：通过。
- `go test -tags audiocodec ./module/webrtc -run 'TestWHEPPayloadTypeCorrectness|TestWHEPWithGCC|TestWHEPAudioTranscoding|TestValidVideoRTPDelta' -count=1 -v`：通过。
- `go test -tags audiocodec ./module/webrtc -run TestWHEPBrowserJitterDiagnostic -count=1 -v`：通过；VP8 视频和 VP8+AAC->Opus 场景均有推进帧、无丢包、无冻结。
- `go test ./module/record -count=1`、`go test -race ./module/record -count=1`、`CGO_ENABLED=1 go test -tags audiocodec -race ./module/record -count=1`：通过；覆盖 fMP4/FLV/MP4/TS 轮转后的完整轨道初始化。
- `go test ./module/sipgateway -count=1`、`go test -race ./module/sipgateway -count=1`、`CGO_ENABLED=1 go test -tags audiocodec -race ./module/sipgateway -count=1`：通过；包含 PCMA 源到请求 PCMU 的真实 RTP/RTCP Lab 转码回归。
- 2026-08-28 独立端口 Console 验收：GB28181 G.711A 源的 HTTP-FLV、WS-FLV、HTTP-TS、fMP4、HLS、DASH、WHEP 均解码为 160x90 且媒体时钟推进；SIP WHEP 同样为 160x90、`readyState=4` 并推进。GB28181->SIP PCMU receive 音频/视频/RTCP 计数增长，SIP PCMA->GB28181 receive 的 RTP/RTCP/PS 发送和接收计数一致。SIP 11 项与 GB28181 13 项一键自测全部通过。
- 当前运行实例的 H.264 对照：`lf-test` WHEP `mode=realtime` 与 `mode=live` 的 5 秒帧数分别为 240 和 1395；浏览器 `mode=live` 已观察到 640x360、`readyState=4`、`paused=false` 且 `currentTime` 递增，浏览器 `mode=realtime` 在首个后续关键帧前保持等待。新增 fixture 浏览器回归通过，关闭了“所有 H.264 RTP 都不可解码”的假设。
- 2026-08-29 统一 Chromium 矩阵以 `LIVEFORGE_PROTOCOL_MATRIX_SOAK=15s` 通过三种场景，每个场景约 19 秒；SIP/GB28181 为 160x90，WHIP 为 640x360，全部校验音视频 RTP、decoded frames、媒体时钟、RTCP、ICE、浏览器错误和服务端 stall。随后 `CGO_ENABLED=1 go test -tags audiocodec -race ./module/gb28181 ./tools/testkit/push ./tools/testkit/testutil ./test/integration -count=1` 通过。该结果是短时正确性证据，不是并发/容量结论。
- WHEP 音频样本写入失败从缓存、直读和转码 reader 三条路径立即终止 feed；连接后 8 秒完全没有输入进入可恢复的 `no_media_input`，首帧后任一期望轨 8 秒不推进进入 `media_stalled`，全部期望轨重新推进后恢复；无效 H.264/H.265 参数集和空访问单元进入 `codec_mismatch`。状态公开首个成功媒体时间和不会被 watchdog/后续帧改写的 `first_media_wait_ms`；媒体热路径使用原子计数/时间戳，基准命令为 `go test ./module/webrtc -run '^$' -bench '^BenchmarkWHEPFeedStatus' -benchmem -count=3`，结果只用于同机回归。
- WHEP 源 reader 与目标音频 reader 独立消费原子覆盖结果：覆盖后的保留帧不会进入 RTP，只有发生覆盖的 reader 推进到 live。源覆盖会保留原 publisher generation，重置视频 pacing/DTS/PTS 状态，复用 TrackSender 的关键帧门并刷新同 generation 最新参数集；已经建立的直通或转码音频继续推进，纯音频从下一帧 live 音频恢复。目标音频覆盖不扰动干净视频；active generation 中期望目标音频 EOF 会立即进入 `target_audio_failed`，不会等待 8 秒 watchdog 或静默降级。终止路径取消并 join reader waiter，且目标 reader ownership 只释放一次；每次覆盖 warning 只记录 reader 身份、精确覆盖数和恢复动作。
- `go test ./pkg/muxer/mp4 ./module/gb28181 ./module/httpstream ./pkg/ratelimit ./core ./module/metrics -count=1` 的对应包级回归均通过；覆盖负 CTS、GB28181 回放 BYE、延迟 HLS/DASH 写 deadline、XFF 前缀绕过、稳定有界指标迭代和重复 publisher 清理。
- `go test ./module/sipgateway -count=5 -timeout=120s`：通过；覆盖外部占用 pair 跳过、SDP 前 socket 绑定、Lab 范围避让和失败清理顺序。
- 2026-08-28 Apple M1 Pro 微基准：Stream write 54.8-55.0ns/0 alloc；Ring TryRead 37.6-37.7ns/0 alloc；Ring immediate context read 40.1-40.5ns/0 alloc；GB28181 outbound 6.43-6.62us/1880 B/6 alloc；SIP outbound 4.49-4.57us/264 B/3 alloc。结果仅用于同机相对回归。
- 2026-08-29 Apple M1 Pro Prometheus Collector fixture（128 个活跃流，detail limit 32，3 次运行）：首次接纳 gather 为 157.5-158.1us、约 179.6KB/2822 alloc；接纳满后的 steady gather 为 134.9-136.9us、约 164.6KB/2667 alloc。结果只描述该真实 Collector gather/admission fixture 的分配与延迟，不是容量结论。
