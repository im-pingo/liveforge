[English](wiki.md) | **中文**

# LiveForge 百科

> LiveForge 完整文档 — 一个使用 Go 编写的高性能多协议直播流媒体服务器。

---

## 目录

- [概述](#概述)
- [快速开始](#快速开始)
- [配置参考](#配置参考)
  - [服务器](#服务器)
  - [TLS](#tls)
  - [限制](#限制)
  - [流](#流)
- [协议模块](#协议模块)
  - [RTMP](#rtmp)
  - [RTSP](#rtsp)
  - [HTTP 流媒体 (HLS/DASH/HTTP-FLV/FMP4/TS)](#http-流媒体)
  - [WebSocket](#websocket)
  - [WebRTC (WHIP/WHEP)](#webrtc-whipwhep)
- [业务模块](#业务模块)
  - [认证](#认证)
  - [录制](#录制)
  - [Webhook 通知](#webhook-通知)
- [管理](#管理)
  - [REST API](#rest-api)
  - [Web 控制台](#web-控制台)
- [架构](#架构)
  - [模块系统](#模块系统)
  - [事件总线](#事件总线)
  - [流生命周期](#流生命周期)
  - [GOP 缓存](#gop-缓存)
- [使用场景](#使用场景)
- [故障排除](#故障排除)

---

## 概述

LiveForge 是一个模块化的直播流媒体服务器，支持实时采集、转封装和分发音视频。它支持 RTMP、RTSP、WebRTC (WHIP/WHEP)、HLS、DASH、HTTP-FLV、FMP4 和 WebSocket 流媒体传输 — 全部集成在单一二进制文件中，无外部依赖。

**核心特性：**

- 单一二进制文件，单一配置文件
- 任意采集协议可通过任意输出协议播放（协议桥接）
- 模块化架构 — 仅启用所需的协议
- 配置文件支持环境变量展开（`${ENV_VAR}`）
- 优雅关闭并支持排空超时

---

## 快速开始

### 构建

```bash
go build -o liveforge ./cmd/liveforge
```

### 运行

```bash
./liveforge -c configs/liveforge.yaml
```

### 使用 FFmpeg 推流

```bash
# RTMP
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://localhost:1935/live/stream1

# RTSP
ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://localhost:8554/live/stream1

# SRT
ffmpeg -re -i input.mp4 -c copy -f mpegts "srt://localhost:6000?streamid=publish:/live/stream1"
```

### 播放

```bash
# RTMP
ffplay rtmp://localhost:1935/live/stream1

# HLS
ffplay http://localhost:8080/live/stream1.m3u8

# DASH
ffplay http://localhost:8080/live/stream1.mpd

# HTTP-FLV
ffplay http://localhost:8080/live/stream1.flv

# HTTP-TS
ffplay http://localhost:8080/live/stream1.ts

# HTTP-FMP4
ffplay http://localhost:8080/live/stream1.mp4

# RTSP
ffplay rtsp://localhost:8554/live/stream1

# SRT
ffplay "srt://localhost:6000?streamid=subscribe:/live/stream1"
```

### Web 控制台

在浏览器中打开 `http://localhost:8090/console`（默认登录凭据：admin/admin）。

---

## 配置参考

配置文件使用 YAML 格式。所有字段支持通过 `${VAR_NAME}` 语法进行环境变量展开。默认配置文件位于 `configs/liveforge.yaml`。

### 服务器

```yaml
server:
  name: "streamserver-01"
  log_level: info
  drain_timeout: 30s
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `name` | `liveforge` | 实例标识符，在集群部署中用于区分节点 |
| `log_level` | `info` | 日志详细级别 |
| `drain_timeout` | `0s` | 关闭时等待活跃连接关闭的时间 |

### TLS

全局 TLS 配置。当 `cert_file` 和 `key_file` 同时设置时，所有已启用的模块会自动使用 TLS（RTMPS、RTSPS、HTTPS）。

```yaml
tls:
  cert_file: "/path/to/cert.pem"
  key_file: "/path/to/key.pem"
```

每个协议模块可以通过模块级别的 `tls` 字段覆盖全局设置：

```yaml
rtmp:
  enabled: true
  listen: ":1935"
  tls: false          # Force plain TCP even if global TLS is configured

http_stream:
  enabled: true
  listen: ":8080"
  tls: true           # Force TLS (error if global cert/key not set)
```

**覆盖规则：**

| 全局 TLS | 模块 `tls` | 结果 |
|----------|-----------|------|
| 已配置 | _(省略)_ | 启用 TLS |
| 已配置 | `false` | 明文 TCP |
| 已配置 | `true` | 启用 TLS |
| 未配置 | _(省略)_ | 明文 TCP |
| 未配置 | `true` | **错误** — 需要 cert/key |
| 未配置 | `false` | 明文 TCP |

**使用场景：** 在负载均衡器上终止 HTTP 流量的 TLS，但对 RTMP 推流直接使用 RTMPS：

```yaml
tls:
  cert_file: "/etc/ssl/server.crt"
  key_file: "/etc/ssl/server.key"

rtmp:
  enabled: true
  listen: ":1935"
  # tls: omitted -> follows global -> RTMPS

http_stream:
  enabled: true
  listen: ":8080"
  tls: false           # Behind reverse proxy, no TLS needed

api:
  enabled: true
  listen: ":8090"
  tls: false           # Internal network only
```

### 限制

```yaml
limits:
  max_streams: 0                    # 0 = unlimited
  max_subscribers_per_stream: 0     # 0 = unlimited
  max_connections: 0                # 0 = unlimited
  max_bitrate_per_stream: 0         # 0 = unlimited (reserved)
```

| 字段 | 说明 |
|------|------|
| `max_streams` | 服务器接受的最大并发流数量 |
| `max_subscribers_per_stream` | 每个流的订阅者上限 |
| `max_connections` | 所有模块共享的全局连接限制 |
| `max_bitrate_per_stream` | 预留字段，暂未使用 |

### 流

控制服务器级别的流行为。

```yaml
stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  max_skip_count: 3
  max_skip_window: 60s
  idle_timeout: 30s
  no_publisher_timeout: 15s
  simulcast:
    enabled: false
  audio_on_demand:
    enabled: false
  feedback:
    enabled: false
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `gop_cache` | `true` | 新订阅者立即接收最新的关键帧组，实现快速起播 |
| `gop_cache_num` | `1` | 保留的 GOP 数量。较高的值会增加内存使用，但为订阅者提供更多追赶数据 |
| `audio_cache_ms` | `1000` | 音频缓存时长（毫秒），与视频 GOP 缓存配合使用 |
| `ring_buffer_size` | `1024` | 无锁环形缓冲区的槽位数。对于高码率或高帧率的流，建议增大此值 |
| `max_skip_count` | `3` | 订阅者在窗口期内最大允许跳帧次数 |
| `max_skip_window` | `60s` | 跳帧统计的滑动窗口时长 |
| `idle_timeout` | `30s` | 如果在此时间内没有推流者和订阅者，流将被销毁 |
| `no_publisher_timeout` | `15s` | 如果流已创建但在此时间内没有推流者连接，流将被关闭 |
| `simulcast.enabled` | `false` | 启用 simulcast 多质量流支持 |
| `audio_on_demand.enabled` | `false` | 启用音频按需模式。仅在订阅者请求时转发音频。 |
| `feedback.enabled` | `false` | 启用订阅者反馈（如 NACK、PLI 请求） |

---

## 协议模块

### RTMP

RTMP 模块处理使用 RTMP/RTMPS 协议的采集（推流）和播放（订阅）。

```yaml
rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `false` | 启用 RTMP 模块 |
| `listen` | `:1935` | 绑定的 TCP 地址 |
| `chunk_size` | `4096` | RTMP 块大小（字节） |
| `tls` | _(null)_ | 模块级 TLS 覆盖（启用 RTMPS） |

**支持的 RTMP 命令：** connect、createStream、publish、play、deleteStream、releaseStream、FCPublish、FCUnpublish、closeStream、@setDataFrame（metadata）。

**推流（采集）：**

```bash
# OBS: Set Server to rtmp://host:1935/live, Stream Key to stream1

# FFmpeg:
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://host:1935/live/stream1

# With auth token:
ffmpeg -re -i input.mp4 -c copy -f flv "rtmp://host:1935/live/stream1?token=YOUR_JWT"
```

**播放（订阅）：**

```bash
ffplay rtmp://host:1935/live/stream1
vlc rtmp://host:1935/live/stream1
```

**使用场景：**
- OBS Studio 直播推流
- FFmpeg 转推/转码流水线
- 旧版播放器兼容
- 硬件编码器低延迟采集

---

### RTSP

RTSP 模块支持 TCP 交织和 UDP 传输模式，用于采集和播放。

```yaml
rtsp:
  enabled: true
  listen: ":8554"
  rtp_port_range: [10000, 20000]
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `false` | 启用 RTSP 模块 |
| `listen` | `:8554` | 绑定的 TCP 地址 |
| `rtp_port_range` | `[10000, 20000]` | RTP/RTCP 媒体传输的 UDP 端口范围 |
| `tls` | _(null)_ | 模块级 TLS 覆盖（启用 RTSPS） |

**支持的 RTSP 方法：** OPTIONS、DESCRIBE、SETUP、PLAY、PAUSE、ANNOUNCE、RECORD、TEARDOWN。

**传输模式：**
- **TCP 交织** — RTP 数据嵌入 RTSP TCP 连接中（防火墙友好）
- **UDP** — 使用独立的 UDP 端口传输 RTP/RTCP（更低延迟）

**通过 RTSP 推流（ANNOUNCE/RECORD）：**

```bash
# TCP transport
ffmpeg -re -i input.mp4 -c copy -f rtsp -rtsp_transport tcp rtsp://host:8554/live/stream1

# UDP transport
ffmpeg -re -i input.mp4 -c copy -f rtsp rtsp://host:8554/live/stream1
```

**通过 RTSP 播放（DESCRIBE/PLAY）：**

```bash
ffplay -rtsp_transport tcp rtsp://host:8554/live/stream1
ffplay rtsp://host:8554/live/stream1
vlc rtsp://host:8554/live/stream1
```

**使用场景：**
- IP 摄像头采集（大多数摄像头原生支持 RTSP）
- 监控系统
- 跨协议桥接：RTSP 采集 -> HLS/DASH 分发

---

### HTTP 流媒体

单个 HTTP 模块根据 URL 扩展名提供多种流媒体格式。

```yaml
http_stream:
  enabled: true
  listen: ":8080"
  cors: true
  # tls: null
  hls:
    segment_duration: 6
    playlist_size: 5
  dash:
    segment_duration: 6
    playlist_size: 30
  llhls:
    enabled: false
    part_duration: 0.2
    segment_count: 4
    container: fmp4
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `true` | 是否启用 HTTP 流媒体模块 |
| `listen` | `:8080` | 监听地址和端口 |
| `cors` | `true` | 是否启用 CORS 跨域头 |
| `tls` | _(null)_ | 模块级 TLS 覆盖 |

**URL 格式：** `http://host:8080/{app}/{stream_key}.{format}`

| 格式 | URL | Content-Type | 说明 |
|------|-----|-------------|------|
| **HLS** | `/live/stream1.m3u8` | `application/vnd.apple.mpegurl` | Apple HLS — 设备兼容性最广 |
| **HLS 分片** | `/live/stream1/{N}.ts` | `video/mp2t` | 单个 TS 分片 |
| **LL-HLS** | `/live/stream1.m3u8` | `application/vnd.apple.mpegurl` | 低延迟 HLS（需开启 `llhls.enabled`） |
| **LL-HLS 部分分片** | `/live/stream1/{MSN}.{part}.m4s` | `video/mp4` | 部分分片（fMP4） |
| **LL-HLS 初始化** | `/live/stream1/init.mp4` | `video/mp4` | 初始化分片（fMP4） |
| **DASH** | `/live/stream1.mpd` | `application/dash+xml` | MPEG-DASH，使用 SegmentTimeline |
| **DASH 视频** | `/live/stream1/v{N}.m4s` | `video/mp4` | fMP4 视频分片 |
| **DASH 音频** | `/live/stream1/a{N}.m4s` | `video/mp4` | fMP4 音频分片 |
| **DASH 初始化** | `/live/stream1/init.mp4` | `video/mp4` | 视频初始化分片（ftyp+moov） |
| **DASH 音频初始化** | `/live/stream1/audio_init.mp4` | `video/mp4` | 音频初始化分片 |
| **HTTP-FLV** | `/live/stream1.flv` | `video/x-flv` | 分块传输 FLV 流 |
| **HTTP-TS** | `/live/stream1.ts` | `video/mp2t` | 分块传输 MPEG-TS 流 |
| **HTTP-FMP4** | `/live/stream1.mp4` | `video/mp4` | 分块传输 fMP4 流 |

#### HLS

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `segment_duration` | `6` | 目标分片时长（秒）。实际时长取决于关键帧间隔。 |
| `playlist_size` | `5` | m3u8 播放列表中列出的分片数量。 |

**使用场景：**
- iOS Safari 浏览器播放（原生 HLS 支持）
- 使用 hls.js 的任意设备（Chrome、Firefox、Edge）
- CDN 友好分发（可缓存的分片）

```bash
ffplay http://localhost:8080/live/stream1.m3u8
```

#### LL-HLS（低延迟 HLS）

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `false` | 启用 LL-HLS 模式（替代普通 HLS 的 `.m3u8` 请求） |
| `part_duration` | `0.2` | 目标部分分片时长（秒），对应 PART-TARGET |
| `segment_count` | `4` | 滑动窗口中保留的完整分片数量 |
| `container` | `fmp4` | 容器格式：`fmp4`（推荐）或 `ts`/`mpegts` |

启用后，`.m3u8` 请求将返回 LL-HLS 播放列表（HLS 版本 9），包含部分分片、阻塞式播放列表重载、增量更新和预加载提示。普通 HLS 和 LL-HLS 互斥 — 设置 `llhls.enabled: true` 使用 LL-HLS，设置为 `false` 使用普通 HLS。

> **容器别名：** `mpegts` 和 `mpeg-ts` 会自动规范化为 `ts`。

**LL-HLS 特性：**
- `EXT-X-PART` — 部分分片（约 200ms），实现亚秒级延迟
- `CAN-BLOCK-RELOAD` — 服务端等待新内容可用后再返回响应
- `EXT-X-PRELOAD-HINT` — 客户端预加载尚未生成的下一个部分分片
- `EXT-X-SKIP` — 增量播放列表更新，减少带宽
- 目标延迟：默认设置下约 2 秒

**支持的播放器：** Safari（原生支持）、hls.js 1.0+（需启用 `lowLatencyMode: true`）

```bash
# 在配置中启用 LL-HLS 后：
ffplay http://localhost:8080/live/stream1.m3u8
```

#### DASH

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `segment_duration` | `6` | 目标分片时长（秒）。 |
| `playlist_size` | `30` | 滑动窗口中保留的最大分片数。 |

MPD 使用带有逐分片精确时间信息的 `SegmentTimeline`，在 ffplay 和 dash.js 中提供准确的播放体验。视频和音频作为独立的 AdaptationSet 分开提供。

**使用场景：**
- 通过 dash.js 进行浏览器播放
- Android ExoPlayer
- CDN 分发，支持自适应码率

```bash
ffplay http://localhost:8080/live/stream1.mpd
mpv http://localhost:8080/live/stream1.mpd
```

#### HTTP-FLV / HTTP-TS / HTTP-FMP4

分块传输编码流 — 服务器持续推送数据，直到客户端断开连接。

**使用场景：**
- HTTP-FLV：flv.js 浏览器播放，低延迟 Web 推流
- HTTP-TS：播放器兼容性，转码器输入
- HTTP-FMP4：基于 MSE 的浏览器播放器

---

### WebSocket

WebSocket 流媒体通过 WebSocket 连接复用相同的 FLV/TS/FMP4 格式。

**URL 格式：** `ws://host:8080/ws/live/stream1.flv`

WebSocket 处理程序与 HTTP 监听器共享。支持的格式：`flv`、`ts`、`mp4`。

**使用场景：**
- 偏好 WebSocket 而非 HTTP 分块传输的浏览器播放器
- HTTP 分块传输被代理阻断的环境

---

### WebRTC (WHIP/WHEP)

使用 WHIP（推流）和 WHEP（订阅）信令标准的 WebRTC 支持。

```yaml
webrtc:
  enabled: true
  listen: ":8443"
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 30000]
  candidates: []
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `false` | 启用 WebRTC 模块 |
| `listen` | `:8443` | HTTP 信令端点地址 |
| `ice_servers` | Google STUN | 用于 ICE 连接的 STUN/TURN 服务器 |
| `udp_port_range` | `[20000, 30000]` | RTP 媒体的 UDP 端口范围 |
| `candidates` | `[]` | 额外的 ICE 候选地址（用于 NAT 穿透的公网 IP） |
| `tls` | _(null)_ | 模块级 TLS 覆盖 |

**WHIP（推流）：**

```bash
# HTTP endpoint: POST /webrtc/whip/{app}/{stream_key}
# The browser console has a built-in WHIP publisher with camera/mic selection.

curl -X POST http://host:8443/webrtc/whip/live/stream1 \
  -H "Content-Type: application/sdp" \
  -d "$SDP_OFFER"
```

**WHEP（订阅）：**

```bash
# HTTP endpoint: POST /webrtc/whep/{app}/{stream_key}
# The browser console has built-in WHEP playback.

# Two modes:
# ?mode=realtime  — minimum latency, may skip frames
# ?mode=live      — slightly higher latency, smoother playback
```

**会话管理：**
- `DELETE /webrtc/session/{id}` — 终止会话
- `PATCH /webrtc/session/{id}` — ICE trickle 候选

**使用场景：**
- 亚秒级浏览器到浏览器推流
- 无需插件的浏览器采集（摄像头/屏幕捕获）
- 超低延迟监控面板
- 跨协议：WebRTC 采集 -> HLS/DASH 分发给大量观众

---

### SRT（安全可靠传输）

SRT 模块使用纯 Go 库 `datarhei/gosrt` 提供基于 UDP 的低延迟、可靠流媒体传输。SRT 承载 MPEG-TS 数据，支持 AES 加密以确保在不可靠网络上的安全传输。

```yaml
srt:
  enabled: true
  listen: ":6000"
  latency: 120
  # passphrase: "your_secret"
  # pbkeylen: 16
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `false` | 启用 SRT 模块 |
| `listen` | `:6000` | UDP 监听地址 |
| `latency` | `120` | 接收端延迟（毫秒） |
| `passphrase` | _(空)_ | AES 加密密码（空 = 不加密） |
| `pbkeylen` | `0` | 加密密钥长度：0、16、24 或 32 字节 |

**Stream ID 格式：**

SRT Stream ID 决定连接是推流还是拉流，以及使用哪个流密钥。支持以下格式：

| 格式 | 示例 | 说明 |
|------|------|------|
| `publish:` 前缀 | `publish:/live/stream1` | 推流 |
| `subscribe:` 前缀 | `subscribe:/live/stream1` | 拉流 |
| SRT 访问控制 | `#!::r=/live/stream1,m=publish` | 标准 SRT ACL 语法 |
| 裸路径 | `/live/stream1` | 默认为拉流 |

**推流：**

```bash
# FFmpeg SRT 推流：
ffmpeg -re -i input.mp4 -c copy -f mpegts \
  "srt://host:6000?streamid=publish:/live/stream1"

# 加密推流：
ffmpeg -re -i input.mp4 -c copy -f mpegts \
  "srt://host:6000?streamid=publish:/live/stream1&passphrase=your_secret"

# OBS：输出设置为自定义，服务器填 srt://host:6000?streamid=publish:/live/stream1
```

**拉流：**

```bash
ffplay "srt://host:6000?streamid=subscribe:/live/stream1"

# VLC：打开网络串流 → srt://host:6000?streamid=subscribe:/live/stream1
```

**使用场景：**
- 公网低延迟贡献链路
- 在丢包或高抖动网络上（蜂窝网络、卫星链路）实现可靠传输
- 加密点对点媒体传输
- 跨协议：SRT 采集 → HLS/DASH 分发给大量观众

---

## 业务模块

### 认证

认证模块为推流和订阅事件提供 JWT 令牌和 HTTP 回调认证。

```yaml
auth:
  enabled: true
  publish:
    mode: "token"
    token:
      secret: "${AUTH_JWT_SECRET}"
      algorithm: HS256
    callback:
      url: "http://auth-service/verify"
      timeout: 3s
  subscribe:
    mode: "callback"
    token:
      secret: "${AUTH_JWT_SECRET}"
      algorithm: HS256
    callback:
      url: "http://auth-service/verify"
      timeout: 3s
  api:
    bearer_token: "${API_TOKEN}"
```

> **注意：** 目前仅支持 `HS256` 作为 JWT 签名算法。

**认证模式：**

| 模式 | 说明 |
|------|------|
| `none` | 无认证（默认） |
| `token` | 通过 `?token=` 查询参数进行 JWT 令牌验证 |
| `callback` | 向外部认证服务发送 HTTP POST 请求 |
| `token+callback` | 先尝试 JWT，JWT 失败后回退到回调 |

#### JWT 令牌模式

令牌通过 URL 查询参数传递：`?token=YOUR_JWT`。

**JWT 载荷字段：**

```json
{
  "sub": "live/stream1",
  "action": "publish",
  "exp": 1711500000
}
```

JWT 使用配置的 `secret` 进行 HMAC-SHA256 签名。

**示例 — 生成推流令牌：**

```python
import jwt, time
token = jwt.encode(
    {"sub": "live/stream1", "action": "publish", "exp": int(time.time()) + 3600},
    "my-secret", algorithm="HS256"
)
```

```bash
ffmpeg -re -i input.mp4 -c copy -f flv "rtmp://host:1935/live/stream1?token=$TOKEN"
```

#### 回调模式

LiveForge 向配置的 URL 发送 HTTP POST 请求，包含以下内容：

```json
{
  "stream_key": "live/stream1",
  "protocol": "rtmp",
  "remote_addr": "192.168.1.100:54321",
  "token": "optional-token-from-query",
  "action": "publish"
}
```

- **HTTP 200** -> 允许
- **其他状态码** -> 拒绝

**使用场景：**
- 与现有用户系统集成
- 动态流密钥验证
- 按用户进行速率限制

---

### 录制

录制模块将直播流捕获为 FLV 文件，支持基于时长的分段。

```yaml
record:
  enabled: true
  stream_pattern: "live/*"
  format: flv
  path: "/data/record/{stream_key}/{date}_{time}.flv"
  segment:
    mode: duration
    duration: 30m
    max_size: 512MB
  on_file_complete:
    url: "http://my-service/on-record-complete"
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `stream_pattern` | `*` | 流密钥的 Glob 模式。`live/*` 仅录制 `live` 应用下的流。 |
| `format` | `flv` | 输出格式（目前仅支持 FLV） |
| `path` | `./recordings/{stream_key}/{date}_{time}.flv` | 文件路径模板 |
| `segment.duration` | `30m` | 分段时长。设置为 `0` 表示单个连续文件。 |
| `on_file_complete.url` | _(空)_ | 录制文件完成时的 HTTP POST 回调 |

**路径模板变量：**

| 变量 | 示例 | 说明 |
|------|------|------|
| `{stream_key}` | `live/stream1` | 完整流密钥 |
| `{date}` | `2026-03-26` | 当前日期 |
| `{time}` | `143052` | 当前时间（HHMMSS） |

**文件完成回调载荷：**

```json
{
  "stream_key": "live/stream1",
  "file_path": "/data/record/live/stream1/2026-03-26_143052.flv",
  "bytes": 52428800,
  "duration": 1800.5
}
```

**使用场景：**
- 直播流的点播归档
- 合规性录制
- 从直播素材进行后期制作编辑
- 通过回调 Webhook 自动上传到 S3/云存储

---

### Webhook 通知

通知模块为流生命周期事件发送 HTTP POST Webhook。

```yaml
notify:
  http:
    enabled: true
    endpoints:
      - url: "https://my-service/webhook"
        events: ["on_publish", "on_publish_stop"]
        secret: "webhook-signing-secret"
        retry: 3
        timeout: 5s
      - url: "https://analytics/events"
        events: []
        secret: ""
        retry: 1
        timeout: 3s
  alive_interval: 10s
  websocket:
    enabled: false
    path: "/ws/notify"
```

**事件：**

| 事件 | 触发条件 |
|------|----------|
| `on_publish` | 推流者开始发送媒体数据 |
| `on_publish_stop` | 推流者断开连接 |
| `on_subscribe` | 订阅者连接 |
| `on_subscribe_stop` | 订阅者断开连接 |
| `on_stream_create` | 新流在 Hub 中创建 |
| `on_stream_destroy` | 流从 Hub 中移除 |
| `on_publish_alive` | 推流者活跃期间的周期性心跳 |
| `on_subscribe_alive` | 存在订阅者期间的周期性心跳 |
| `on_stream_alive` | 流存在期间的周期性心跳 |

**Webhook 载荷：**

```json
{
  "event": "on_publish",
  "stream_key": "live/stream1",
  "protocol": "rtmp",
  "remote_addr": "192.168.1.100:54321",
  "timestamp": 1711500000,
  "extra": {
    "bytes_in": 1048576,
    "video_frames": 900,
    "audio_frames": 1500,
    "bitrate_kbps": 2500,
    "fps": 30.0,
    "uptime_sec": 60
  }
}
```

**Alive 事件额外字段：**

`on_publish_alive`、`on_subscribe_alive` 和 `on_stream_alive` 事件在 `extra` 对象中包含额外的统计信息：

| 字段 | 类型 | 说明 |
|------|------|------|
| `bytes_in` | int64 | 已接收的总字节数 |
| `video_frames` | int64 | 已接收的总视频帧数 |
| `audio_frames` | int64 | 已接收的总音频帧数 |
| `bitrate_kbps` | float64 | 当前码率（kbps） |
| `fps` | float64 | 当前视频帧率 |
| `uptime_sec` | float64 | 流运行时长（秒） |

**WebSocket 通知：**

当 `websocket.enabled` 为 `true` 时，客户端可以连接到配置的 `path` WebSocket 端点，实时接收事件通知。事件以 JSON 消息格式发送，与 HTTP Webhook 载荷格式相同。

**签名验证（HMAC-SHA256）：**

当配置了 `secret` 时，Webhook 将包含 `X-Signature` 头：

```
X-Signature: hex(HMAC-SHA256(request_body, secret))
```

在接收端进行验证：

```python
import hmac, hashlib
expected = hmac.new(b"webhook-signing-secret", request.body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(request.headers["X-Signature"], expected)
```

**重试行为：** 指数退避（1s、2s、4s、...），上限 30s，按配置的重试次数执行。

**使用场景：**
- 流状态仪表盘
- 直播开始时触发聊天叠加层
- 基于流时长的数据分析和计费
- 根据流数量自动扩缩容工作节点

---

## 管理

### REST API

API 模块运行在独立端口上（默认 `:8090`），提供管理接口。

```yaml
api:
  enabled: true
  listen: ":8090"
  # tls: null
  auth:
    bearer_token: "${API_TOKEN}"
  console:
    username: "admin"
    password: "admin"
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `enabled` | `true` | 是否启用 API 模块 |
| `listen` | `:8090` | 监听地址和端口 |
| `tls` | _(null)_ | 模块级 TLS 覆盖 |
| `auth.bearer_token` | `""` | API Bearer Token（支持环境变量） |
| `console.username` | `"admin"` | Web 控制台登录用户名 |
| `console.password` | `"admin"` | Web 控制台登录密码 |

**认证方式：**
- **API 接口**（`/api/*`）：通过 Bearer 令牌或有效的控制台会话 Cookie 保护
- **控制台**（`/console`）：通过会话 Cookie 登录保护（24 小时过期，HMAC-SHA256 签名）

**接口列表：**

| 方法 | 路径 | 说明 |
|------|------|------|
| `GET` | `/api/v1/streams` | 列出所有活跃流及其统计信息 |
| `GET` | `/api/v1/streams/{app}/{key}` | 获取单个流的详细信息 |
| `DELETE` | `/api/v1/streams/{app}/{key}` | 删除一个流 |
| `POST` | `/api/v1/streams/{app}/{key}/kick` | 踢掉推流者 |
| `GET` | `/api/v1/server/info` | 服务器信息（版本、运行时间、模块、端点） |
| `GET` | `/api/v1/server/stats` | 服务器统计（流数量、连接数量） |
| `GET` | `/api/v1/server/health` | 健康检查 |
| `GET` | `/console` | Web 控制台仪表盘 |
| `GET/POST` | `/console/login` | 登录页面 |

**示例 — 列出流：**

```bash
curl -H "Authorization: Bearer $API_TOKEN" http://localhost:8090/api/v1/streams
```

**响应：**

```json
{
  "code": 0,
  "message": "ok",
  "data": {
    "streams": [
      {
        "key": "live/stream1",
        "state": "publishing",
        "publisher": "rtmp://192.168.1.100:54321",
        "video_codec": "H264",
        "audio_codec": "AAC",
        "subscribers": { "flv": 1, "rtmp": 2 },
        "stats": {
          "bytes_in": 5242880,
          "video_frames": 9000,
          "audio_frames": 15000,
          "uptime_sec": 300,
          "bitrate_kbps": 2500,
          "fps": 30.0
        }
      }
    ]
  }
}
```

**示例 — 踢掉推流者：**

```bash
curl -X POST -H "Authorization: Bearer $API_TOKEN" \
  http://localhost:8090/api/v1/streams/live/stream1/kick
```

### Web 控制台

Web 控制台是内置的单页仪表盘，访问地址为 `/console`。

**功能：**
- 实时流列表，自动刷新
- 每个流的详细信息：视频/音频编解码器、码率、帧率、GOP 缓存信息、订阅者数量
- 预览播放器，支持 HLS、DASH、HTTP-FLV、WebRTC (WHEP) 播放
- 通过浏览器直接进行 WebRTC WHIP 推流（摄像头/麦克风选择）
- 流管理：踢掉推流者、删除流
- 服务器信息和健康状态

**访问控制台：**

1. 在浏览器中打开 `http://host:8090/console`
2. 使用配置的 `username` / `password` 登录
3. 开始推流后，流会自动出现

---

## 架构

### 模块系统

LiveForge 采用模块化架构。每个协议或功能是一个实现以下接口的 `Module`：

```go
type Module interface {
    Name() string
    Init(s *Server) error
    Hooks() []HookRegistration
    Close() error
}
```

模块在 `main.go` 中注册并按顺序初始化。关闭时按反向顺序（后进先出）关闭，以确保依赖模块先停止。

```
注册：auth -> rtmp -> rtsp -> http -> webrtc -> record -> notify -> api
关闭：api -> notify -> record -> webrtc -> http -> rtsp -> rtmp -> auth
```

### 事件总线

事件总线提供跨模块通信的钩子系统。事件按优先级分发给已注册的处理程序（数字越小优先级越高）。

**钩子类型：**
- **同步钩子** — 按优先级顺序执行。如果任何一个返回错误，后续钩子将被跳过（用于认证拒绝）。
- **异步钩子** — 在所有同步钩子成功后在 goroutine 中触发（用于通知、录制）。

**事件生命周期：**

```
Publisher connects
  -> EventStreamCreate (if new stream)
  -> EventPublish (auth hooks can reject here)
  -> [streaming...]
  -> EventPublishAlive (periodic)
  -> EventPublishStop
  -> EventStreamDestroy (after idle timeout)
```

### 流生命周期

流具有状态机：

```
         创建
          |
          v
  +---------------+
  |     Idle      |  <--- 流已创建，等待推流者
  +-------+-------+
          |  SetPublisher
          v
  +---------------+
  |  Publishing   |  <--- 推流中，接收并分发媒体帧
  +-------+-------+
          |  RemovePublisher
          v
  +---------------+
  | NoPublisher   |  <--- 推流者断开，等待重连
  +-------+-------+
          |
    +-----+-----+
    |           |
重新推流     超时(no_publisher_timeout)
    |           |
    v           v
Publishing  +---------------+
            |  Destroying   |  <--- 销毁中，释放所有资源
            +---------------+
```

| 状态 | 说明 |
|------|------|
| `idle` | 流已创建，等待推流者连接 |
| `waiting_pull` | 回源拉流等待中 |
| `publishing` | 推流中，正常接收和分发媒体数据 |
| `no_publisher` | 推流者已断开，在 `no_publisher_timeout` 内等待重连 |
| `destroying` | 超时或主动关闭，释放所有资源后销毁流 |

### GOP 缓存

当 `gop_cache` 启用时，服务器会缓冲最新的图像组（关键帧 + 后续帧）。当新订阅者加入时，会立即接收缓存的 GOP，从而无需等待下一个关键帧即可立即显示视频。

---

## 使用场景

### 场景一：直播活动推流

**架构：** OBS -> RTMP -> LiveForge -> HLS/DASH -> CDN -> 观众

```yaml
rtmp:
  enabled: true
http_stream:
  enabled: true
  cors: true
  hls:
    segment_duration: 4
    playlist_size: 6
```

1. 配置 OBS 将 RTMP 推送到 `rtmp://server:1935/live/event1`
2. 将 `https://cdn.example.com/live/event1.m3u8` 分发给观众
3. 通过 Web 控制台 `http://server:8090/console` 监控

### 场景二：安防摄像头 NVR

**架构：** IP 摄像头 -> RTSP -> LiveForge -> 录制 + WebRTC 预览

```yaml
rtsp:
  enabled: true
  listen: ":8554"
webrtc:
  enabled: true
record:
  enabled: true
  stream_pattern: "camera/*"
  path: "/data/nvr/{stream_key}/{date}_{time}.flv"
  segment:
    duration: 1h
```

1. 摄像头将 RTSP 推送到 `rtsp://server:8554/camera/front-door`
2. 安保人员通过浏览器控制台中的 WebRTC WHEP 进行预览
3. 录制文件存储为 1 小时的 FLV 文件

### 场景三：浏览器到浏览器超低延迟

**架构：** 浏览器 -> WHIP -> LiveForge -> WHEP -> 浏览器

```yaml
webrtc:
  enabled: true
  listen: ":8443"
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
```

1. 推流者打开控制台，点击"Publish"，选择摄像头/麦克风
2. 观看者打开控制台，选择一个流，点击"WebRTC"预览
3. 端到端亚秒级延迟

### 场景四：多协议分发

**架构：** 单一采集，多协议输出。

```yaml
rtmp:
  enabled: true
http_stream:
  enabled: true
webrtc:
  enabled: true
rtsp:
  enabled: true
```

通过 RTMP 推流一次，同时提供：
- HLS 给 iOS/Safari 观众
- DASH 给 Android/Chrome 观众
- WebRTC 给超低延迟观众
- RTSP 给旧版媒体播放器
- HTTP-FLV 给使用 flv.js 的 Web 播放器

### 场景五：认证付费推流

**架构：** JWT 认证推流者，回调认证订阅者。

```yaml
auth:
  enabled: true
  publish:
    mode: "token"
    token:
      secret: "my-super-secret-key"
  subscribe:
    mode: "callback"
    callback:
      url: "http://billing-service/check-subscription"
      timeout: 3s
notify:
  http:
    enabled: true
    endpoints:
      - url: "http://analytics/events"
        events: ["on_publish", "on_publish_stop", "on_subscribe"]
```

1. 推流者从认证服务获取 JWT
2. 在 URL 中使用 `?token=JWT` 进行推流
3. 观看者请求流时，LiveForge 调用计费服务验证订阅状态
4. 分析服务接收所有事件的 Webhook

---

## 故障排除

### 流无法播放

1. 检查 `/console` 控制台 — 流是否列出？
2. 验证推流者是否已连接：`curl http://host:8090/api/v1/streams`
3. 检查服务器日志中的认证拒绝消息
4. 确保格式扩展名匹配：`.m3u8` 对应 HLS、`.mpd` 对应 DASH、`.flv` 对应 FLV

### HLS/DASH 延迟过高

- 减小 `segment_duration`（最小值 = 源的关键帧间隔）
- 确保编码器每 2-4 秒发送一个关键帧
- 如需最低延迟，使用 WebRTC (WHEP) 或 HTTP-FLV

### ffplay LL-HLS 首次播放卡顿

FFmpeg 的 HLS 解复用器不支持 LL-HLS 扩展（`EXT-X-PART`、阻塞式重载）。它将播放列表视为标准 HLS，需要至少 3 个完整分片才能流畅播放（因为 `live_start_index=-3`）。LiveForge 会自动检测旧版客户端（无 `_HLS_msn` 查询参数），并在首次请求播放列表时等待 3 个分片准备就绪后再返回。这会为 ffplay 增加约 4-6 秒的初始延迟，但确保无卡顿播放。支持 LL-HLS 的客户端（hls.js、Safari）不受影响，仍可通过阻塞式重载获得亚秒级延迟。

如果仍然卡顿，请确保 `stream` 配置中启用了 `gop_cache`（默认启用）— 分段器使用 GOP 缓存预填充首个分片。

### ffplay DASH 卡顿

- LiveForge 在 MPD 中使用 `SegmentTimeline`。请确保使用较新版本的 ffplay/ffmpeg。
- 如果出现播放间隙，可能是源编码器的关键帧间隔不规则。

### 浏览器 DASH 音频错误

如果 Chrome/Edge 显示 `CHUNK_DEMUXER_ERROR_APPEND_FAILED: audio object type 0x40 does not match what is specified in the mimetype`，原因是 LL-HLS 和 DASH 初始化段的 URL 冲突。当 LL-HLS（fmp4 模式）和 DASH 同时活跃时，`init.mp4` 被两个协议共用。dash.js 为音频和视频创建独立的 SourceBuffer，但 `init.mp4` 返回的是 LL-HLS 的合并（视频+音频）初始化段。Chrome 的 MSE 发现了意外的音频轨道并拒绝了它。

修复方式：

- DASH 现在使用 `vinit.mp4` 作为视频初始化段 URL，避免与 LL-HLS 冲突。
- ESDS 使用 ISO 14496-1 4 字节可扩展描述符长度编码（兼容 Chrome MSE）。
- DASH 编解码器字符串现在从 AudioSpecificConfig 解析实际的 `audioObjectType`。

如遇此错误，请升级到最新版本。

### WebRTC 连接失败

- 检查防火墙是否开放了 `udp_port_range` 端口
- 对于 NAT 穿透，使用服务器的公网 IP 配置 `candidates`：
  ```yaml
  webrtc:
    candidates: ["203.0.113.10"]
  ```
- 验证 STUN 服务器是否可达

### TLS 错误

- 验证证书和密钥文件是否存在且可读
- 检查证书是否对使用的主机名有效
- 使用以下命令测试：`openssl s_client -connect host:port`

### 连接数达到上限

- 检查配置中的 `limits.max_connections`
- 通过 API 监控：`GET /api/v1/server/stats` -> `connections` 字段
- 增大限制或添加更多服务器实例
