# LiveForge 流媒体可靠性与播放设计

**日期：** 2026-08-28  
**状态：** 已批准执行  
**范围：** WebRTC 首帧与真实 H.264、核心缓存与生命周期、连接容量与协议出站、运行时配置与指标、Console 与跨协议验收

## 目标

1. 控制台和 API 创建的 WHEP 播放在正常 GOP 周期内稳定收到可解码首帧，不再把“等待关键帧”误报为“无媒体”。
2. SIP/GB28181/WHIP/RTMP 等输入经过服务器后，能够通过 WebRTC 和其他已启用输出进行可重复的跨协议验证。
3. 缓存、连接限制、publisher generation、RTP 接收/发送和模块 shutdown 在异常、并发和重启场景下有界且无 data race。
4. 热路径在保持媒体时序和协议语义不变的前提下减少不必要的锁、反射、分配和系统调用，并提供可重复基准。
5. Config 页面、协议实验室和 Storage 页面只展示已实现且可验证的能力，配置源、权限、敏感信息和重启语义保持一致。

## 设计决策

### 1. WebRTC 启动模型

WHEP 使用一次性的 `StreamStartupSnapshot`，其中包含 publisher identity、generation、MediaInfo、sequence headers、交错 GOP replay frames、LiveCursor 和 GenerationDone。`mode=live` 发送该 snapshot 中的完整可用 GOP，然后从 LiveCursor 读取新帧；`mode=realtime` 不发送 replay frames，但等待后续 video keyframe。纯音频两种模式都从 LiveCursor 直接开始，不等待视频关键帧，也不引入独立 audio cache。

Console 的默认 WHEP 链接使用 `mode=live`，显式 `mode=realtime` 仍然可用并显示“等待关键帧”状态。首帧门控状态定义为 `waiting_keyframe`、`playing`、`no_media_input`、`codec_mismatch`、`sample_write_failed`、`generation_ended` 和 `closed`。feed loop 每次状态变化写结构化日志，并把首帧时间、等待时长、generation、cursor、丢弃帧、发送音视频帧、RTP 计数和最后错误绑定到 session 诊断；`WriteSample` 错误不再静默丢弃。

H.264/H.265 输入统一走 AVCC/HVCC 到 Annex-B 的访问单元转换。关键帧携带缓存的 SPS/PPS/VPS；空访问单元、缺失参数集、协商 codec 不匹配和发送错误分别失败关闭。浏览器回归使用真实 Chromium H.264 解码结果判定：`readyState`、视频尺寸、`currentTime` 推进、音频帧计数和 media error 必须同时满足，SDP 成功或 `ontrack` 不能单独算通过。

### 2. 核心缓存与 generation

GOP cache 仍然是以视频关键帧开始、包含该 GOP 内交错音频的单一 replay cache；纯音频只使用 ring live cursor。GOP cache 在 `GOPCacheNum` 之外增加单 GOP 帧数、持续时间和字节上限，三者任一达到上限时保留从当前关键帧开始的可播放内容并停止继续增长；配置值经过 schema 和运行时校验，默认值保持当前行为的有界版本。

`Stream.WriteFrame` 在 generation/publisher 校验和 ring 写入之间保持单写者顺序，但把不需要保护的统计更新移出大锁；publisher identity 使用稳定的接口 identity 或显式 token，不在每帧使用 reflection。所有 subscriber admission、replay reader 和异步 manager cleanup 必须带 generation；旧 generation 的 stop 事件不得清理新 generation 的资源。

### 3. 资源与连接边界

`AcquireConn` 使用 CAS 循环实现严格的 max connection 上限，所有 HTTP、WebRTC、DVR 和协议 session 路径使用同一个 release-once 约定。DVR 请求在创建 session 前占用连接配额，取消、错误和正常结束均释放。RingBuffer 对非法容量进行构造期保护，避免直接调用工具包时除零或越界。

GB28181 RTP receiver 对从 UDP buffer 交给重排队列的 payload 做拥有式复制，DeviceRegistry 对外只返回 immutable snapshot，Stop/Close 均幂等。HTTP streaming 设置 header/write deadline 和 request cancellation 响应；HLS/DASH/LL-HLS 等等待路径使用 context-aware condition，不再用无法取消的固定 sleep。

### 4. 性能与观测

协议出站优先复用 packet/fragment buffer，在不改变所有权的地方使用 `net.Buffers` 或批量写；配置 refresh 保持 source I/O 串行，但解析/hash/diff/application 不阻塞下一次调度，重复版本快速返回。Prometheus 的 stream labels 使用受控、可配置的采集策略，默认不把任意高基数 stream key 扩散到无限时间序列；必要的流明细通过管理 API 获取。

所有优化必须有行为测试和 benchmark，benchmark 只用来比较相对变化，不能替代容量验收。错误日志不得包含 source URL credentials、bearer token 或 SIP 密码；限流器只有在明确配置可信代理时才读取 forwarded headers，否则使用 RemoteAddr。

### 5. Console 和配置 UX

Console 顶层分为 Workspace、Operations、System；Config 和 Security 只属于 System，Streams、GB28181、SIP Calls、Storage 属于 Workspace，Cluster 属于 Operations。Config 页面分别显示完整 redacted effective document、desired source document、schema、source details、pending restart、校验结果和 apply 状态，不把不可写 source 伪装成可编辑。

SIP/GB28181 lab 的 publish 与 receive 都展示 source/target stream、RTP/RTCP、音视频帧、generation、codec、错误和跨协议 playback links。lab 只使用 loopback fake device，不依赖外部平台；H.264/G.711/Opus/AAC 的输出能力通过 capability matrix 明确显示“可用、需 FFmpeg、视频-only 或不支持”。录像默认 fMP4/MP4，Storage 在 record module 缺失时显示 disabled 状态而不是模块错误。

## 阶段与验收

### 阶段 A：WHEP 首帧与 H.264

- 添加 realtime/live/纯音频/稀疏关键帧/无 cache/样本写入失败测试。
- 添加真实 GB28181、SIP 和 WHIP H.264 输入到 WHEP 的浏览器回归。
- 修正默认 Console 模式和状态展示。
- 验收：默认 WHEP 在 8 秒内推进 `currentTime`；失败时日志和 UI 能区分四类根因。

### 阶段 B：核心可靠性

- 增加 GOP 三类上限、ring 非法容量保护、publisher identity 热路径优化。
- 修复 generation-aware cleanup、publisher timeout、HTTP 注册表历史指针和 subscriber admission。
- 验收：race 测试、替换 publisher 压力测试、缓存内存上限测试全部通过。

### 阶段 C：连接、RTP 和 shutdown

- 修复严格连接上限、DVR 配额、GB28181 packet ownership、DeviceRegistry snapshot、幂等 close、HTTP cancellation。
- 验收：并发超限永不超过配置值，所有路径释放资源，`go test -race` 无数据竞争和 close panic。

### 阶段 D：性能与配置

- 低锁热路径、出站 buffer、配置刷新调度、指标高基数策略和基准。
- 修复 config source 脱敏和 trusted proxy 语义。
- 验收：基准报告保存在 PR/变更说明中，功能测试与配置源 contract 测试通过。

### 阶段 E：Console 与发布验收

- 完成页面分组、Config 全量字段/源适配、协议 lab 能力矩阵和 Storage disabled/回放状态。
- 更新 manifest、llms、README、schema、OpenAPI 和 recipes。
- 验收：本地无外部平台完成 SIP/GB28181 publish/receive、跨协议播放、录像回放，并通过完整构建、race、文档检查和 CI。

## 非目标

- 本轮不实现 WebRTC simulcast layer selection；配置继续标记为 deferred/unsupported。
- 不把没有 FFmpeg 的构建描述成支持非 AAC 音频转码；无依赖构建只保证其声明的 codec 和视频-only fallback。
- 不改变已公开的 stream-key escaping、权限和 bearer token 语义，除非测试证明当前行为违反安全契约。

