# 集群部署指南

LiveForge 支持多节点集群转发，提供两种模式：

- **Forward（主动转推）** — 源站在收到推流后，主动将流转推到一个或多个下游节点。
- **Origin Pull（按需回源）** — 边缘节点在收到订阅请求但本地不存在流时，按需从上游源站拉取。

两种模式均支持多种传输协议：**RTMP**、**SRT**、**RTSP**、**RTP**。协议由配置中的 URL scheme 决定（`rtmp://`、`srt://`、`rtsp://`、`rtp://`）。

## 协议选择

| 协议 | 适用场景 | 延迟 | 说明 |
|------|---------|------|------|
| **SRT** | 低延迟转发 | ~120ms | FEC + ARQ 纠错，AES 加密，适合不稳定网络 |
| **RTMP** | 通用转发 | ~1-3s | 广泛支持，适合 CDN 级联和兼容性场景 |
| **RTSP** | IP 摄像头集成 | ~1-2s | 支持 TCP/UDP 传输，适合监控场景 |
| **RTP** | 直接 RTP 转发 | ~100ms | SDP-over-HTTP 信令，最低开销 |

## 拓扑示例

### 1. SRT 低延迟三节点集群

```
推流端 (FFmpeg/OBS)
    │
    ▼ RTMP 推流
┌─────────┐   SRT 转推       ┌─────────┐   SRT 回源拉取     ┌─────────┐
│ 节点 A  │ ──────────────▶  │ 节点 B  │ ◀────────────────  │ 节点 C  │
│  (源站) │  :6000 → :6001  │ (中继站)│  :6001 ← :6002    │ (边缘站)│
└─────────┘                   └─────────┘                    └─────────┘
    │                              │                              │
    ▼                              ▼                              ▼
  播放端                         播放端                          播放端
```

- **节点 A**：接收推流，通过 SRT 转发到节点 B
- **节点 B**：接收来自 A 的 SRT 转发，为本地订阅者服务
- **节点 C**：当有订阅者请求时，按需从节点 B 拉取

#### 节点 A 配置（源站 + SRT 转推）

```yaml
# configs/srt-node-a.yaml
server:
  name: "srt-node-a"
  log_level: info
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096

srt:
  enabled: true
  listen: ":6000"
  latency: 120

http_stream:
  enabled: true
  listen: ":8080"
  cors: true

webrtc:
  enabled: true
  listen: ":8443"
  ice_lite: true
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [30000, 30500]
  candidates: []

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  idle_timeout: 30s
  no_publisher_timeout: 15s

cluster:
  forward:
    enabled: true
    targets:
      - "srt://127.0.0.1:6001/live"   # SRT 转推到节点 B
    retry_max: 3
    retry_interval: 2s
  origin:
    enabled: false
  srt:
    latency: 120ms

api:
  enabled: true
  listen: ":8090"
  console:
    username: "admin"
    password: "admin"
```

#### 节点 B 配置（SRT 中继节点）

```yaml
# configs/srt-node-b.yaml
server:
  name: "srt-node-b"
  log_level: info
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1936"
  chunk_size: 4096

srt:
  enabled: true
  listen: ":6001"
  latency: 120

http_stream:
  enabled: true
  listen: ":8081"
  cors: true

webrtc:
  enabled: true
  listen: ":8444"
  ice_lite: true
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [30500, 31000]
  candidates: []

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  idle_timeout: 30s
  no_publisher_timeout: 15s

cluster:
  forward:
    enabled: false
  origin:
    enabled: false

api:
  enabled: true
  listen: ":8091"
  console:
    username: "admin"
    password: "admin"
```

#### 节点 C 配置（SRT 边缘站 / 回源拉取）

```yaml
# configs/srt-node-c.yaml
server:
  name: "srt-node-c"
  log_level: info
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1937"
  chunk_size: 4096

srt:
  enabled: true
  listen: ":6002"
  latency: 120

http_stream:
  enabled: true
  listen: ":8082"
  cors: true

webrtc:
  enabled: true
  listen: ":8445"
  ice_lite: true
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [31000, 31500]
  candidates: []

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  idle_timeout: 30s
  no_publisher_timeout: 15s

cluster:
  forward:
    enabled: false
  origin:
    enabled: true
    servers:
      - "srt://127.0.0.1:6001"   # 从节点 B 通过 SRT 拉取
    idle_timeout: 30s
    retry_max: 3
    retry_delay: 2s
  srt:
    latency: 120ms

api:
  enabled: true
  listen: ":8092"
  console:
    username: "admin"
    password: "admin"
```

#### 测试 SRT 集群

```bash
# 启动三个节点
./liveforge -c configs/srt-node-a.yaml &
./liveforge -c configs/srt-node-b.yaml &
./liveforge -c configs/srt-node-c.yaml &

# 推流到节点 A
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://127.0.0.1:1935/live/test

# 节点 A 自动通过 SRT 转发到节点 B
# 在节点 B 验证：
ffplay rtmp://127.0.0.1:1936/live/test

# 节点 C 在有订阅者时按需从节点 B 拉取：
ffplay rtmp://127.0.0.1:1937/live/test

# 任意节点 WebRTC 播放：
# http://127.0.0.1:8090/console  （节点 A）
# http://127.0.0.1:8091/console  （节点 B）
# http://127.0.0.1:8092/console  （节点 C）
```

---

### 2. RTMP 通用双节点集群

```
推流端 (FFmpeg/OBS)
    │
    ▼ RTMP 推流
┌─────────┐   RTMP 转推      ┌─────────┐
│ 节点 A  │ ──────────────▶  │ 节点 B  │
│  (源站) │  :1935 → :1935  │ (边缘站)│
└─────────┘                   └─────────┘
    │                              │
    ▼                              ▼
  播放端                         播放端
```

#### 节点 A 配置（RTMP 源站 + 转推）

```yaml
# configs/rtmp-node-a.yaml
server:
  name: "rtmp-node-a"
  log_level: info
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096

http_stream:
  enabled: true
  listen: ":8080"
  cors: true

webrtc:
  enabled: true
  listen: ":8443"
  ice_lite: true
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 25000]
  candidates: []

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  idle_timeout: 30s
  no_publisher_timeout: 15s

cluster:
  forward:
    enabled: true
    targets:
      - "rtmp://192.168.1.100:1935/live"   # RTMP 转推到节点 B
    retry_max: 5
    retry_interval: 3s
  origin:
    enabled: false

api:
  enabled: true
  listen: ":8090"
  console:
    username: "admin"
    password: "admin"
```

#### 节点 B 配置（RTMP 边缘站）

```yaml
# configs/rtmp-node-b.yaml
server:
  name: "rtmp-node-b"
  log_level: info
  drain_timeout: 10s

rtmp:
  enabled: true
  listen: ":1935"
  chunk_size: 4096

http_stream:
  enabled: true
  listen: ":8080"
  cors: true

webrtc:
  enabled: true
  listen: ":8443"
  ice_lite: true
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 25000]
  candidates: []

stream:
  gop_cache: true
  gop_cache_num: 1
  audio_cache_ms: 1000
  ring_buffer_size: 1024
  idle_timeout: 30s
  no_publisher_timeout: 15s

cluster:
  forward:
    enabled: false
  origin:
    enabled: false

api:
  enabled: true
  listen: ":8090"
  console:
    username: "admin"
    password: "admin"
```

#### 测试 RTMP 集群

```bash
# 在不同主机上启动（或同一主机调整端口）
./liveforge -c configs/rtmp-node-a.yaml &   # 主机 A
./liveforge -c configs/rtmp-node-b.yaml &   # 主机 B (192.168.1.100)

# 推流到节点 A
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://host-a:1935/live/test

# 节点 A 自动通过 RTMP 转推到节点 B
# 从任意节点播放：
ffplay rtmp://host-a:1935/live/test    # 从源站
ffplay rtmp://host-b:1935/live/test    # 从边缘站
```

---

### 3. RTMP 按需回源（边缘 → 源站）

不需要持续转推，只在有订阅者时才拉取：

```
推流端 (FFmpeg/OBS)
    │
    ▼ RTMP 推流
┌─────────┐                   ┌─────────┐
│  源站   │ ◀── RTMP 拉取 ── │ 边缘站  │ ◀── 订阅者
│         │    (按需触发)     │         │
└─────────┘                   └─────────┘
```

#### 边缘站配置（回源拉取）

```yaml
# configs/rtmp-edge.yaml
server:
  name: "rtmp-edge"
  log_level: info

rtmp:
  enabled: true
  listen: ":1935"

http_stream:
  enabled: true
  listen: ":8080"
  cors: true

stream:
  gop_cache: true
  gop_cache_num: 1
  idle_timeout: 30s
  no_publisher_timeout: 15s

cluster:
  forward:
    enabled: false
  origin:
    enabled: true
    servers:
      - "rtmp://origin-host:1935"    # 通过 RTMP 从源站拉取
    idle_timeout: 30s                 # 无订阅者 30s 后断开回源连接
    retry_max: 3
    retry_delay: 2s

api:
  enabled: true
  listen: ":8090"
  console:
    username: "admin"
    password: "admin"
```

当订阅者请求边缘站上的 `live/test` 且本地不存在该流时，边缘站自动连接 `rtmp://origin-host:1935/live/test` 并开始转发。最后一个订阅者离开后，经过 `idle_timeout` 超时后自动断开。

---

## 配置参考

### Forward 转推配置

```yaml
cluster:
  forward:
    enabled: true
    targets:
      - "rtmp://host:1935/live"       # RTMP 转发
      - "srt://host:6000/live"        # SRT 转发（低延迟）
      - "rtsp://host:554/live"        # RTSP 转发
      - "rtp://host:8090/live"        # RTP 直接转发
    schedule_url: ""                   # HTTP 调度器，动态解析转发目标
    schedule_priority: schedule_first  # schedule_first | config_first
    schedule_timeout: 3s
    retry_max: 5
    retry_interval: 3s
```

**工作原理**：当流在本节点发布时，自动转推到所有配置的 `targets`。发布 URL 中的流名称会自动附加到每个目标 URL 后。

**多目标**：可以列出多个不同协议的目标，所有目标同时接收流。

**HTTP 调度器**：设置 `schedule_url` 可通过 HTTP 回调动态解析转发目标。调度器接收流名称，返回目标 URL 列表。

### Origin Pull 回源配置

```yaml
cluster:
  origin:
    enabled: true
    servers:
      - "rtmp://origin1:1935"         # RTMP 源站
      - "srt://origin2:6000"          # SRT 源站
    schedule_url: ""                   # HTTP 调度器，动态解析源站地址
    schedule_priority: schedule_first
    schedule_timeout: 3s
    idle_timeout: 30s                  # 无订阅者后拆除回源连接
    retry_max: 3
    retry_delay: 2s
```

**工作原理**：当订阅者请求本地不存在的流时，节点按顺序尝试每个源站直到成功。流被转发到本地供所有订阅者使用。最后一个订阅者离开后，经过 `idle_timeout` 后拆除回源连接。

### 协议专属设置

```yaml
cluster:
  # SRT 转发传输设置
  srt:
    latency: 120ms        # SRT 延迟（接收缓冲区）。越低延迟越小，越高抗丢包能力越强
    # passphrase: ""      # AES 加密密码（两端必须一致）
    # pbkeylen: 16        # 密钥长度：16 (AES-128)、24 (AES-192)、32 (AES-256)

  # RTSP 转发传输设置
  rtsp:
    transport: tcp        # tcp 或 udp

  # RTP 直接转发设置（SDP-over-HTTP 信令）
  rtp:
    port_range: "20000-20100"
    signaling_path: "/api/relay"
    rtcp_interval: 5s
    timeout: 15s
```

## WebRTC 配置

在任意集群节点上启用 WebRTC 播放，建议开启 ICE Lite 加速连接：

```yaml
webrtc:
  enabled: true
  listen: ":8443"
  ice_lite: true                       # 跳过 ICE 收集 — 瞬间完成协商
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 30000]       # 不能与集群 RTP 端口重叠
  candidates: []                        # 外部 IP 地址（用于 NAT 穿越）
  gcc:
    enabled: true
    initial_bitrate: 2000000           # 兜底初始码率（会根据流实际码率自动调整）
    min_bitrate: 100000
    max_bitrate: 10000000
```

**ICE Lite**：推荐公网服务器或局域网可直接访问的服务器开启。服务端跳过 STUN 查询和连通性检测，WebRTC 协商时间从约 5 秒降至 200 毫秒以内。

**GCC 拥塞控制**：当 WHEP 订阅者连接时，初始码率会自动设置为流的实际码率（+ 20% 余量）。配置中的 `initial_bitrate` 仅在流码率尚未测量到时作为兜底值使用。

**端口范围**：确保 WebRTC UDP 端口（`udp_port_range`）与集群 RTP 端口（`cluster.rtp.port_range`）在同一节点上不重叠。

## 最佳实践

1. **SRT 用于低延迟转发**：节点间需要亚 200ms 延迟时使用 SRT。局域网设置 `latency: 120ms`，广域网有丢包时增加到 `200-500ms`。

2. **RTMP 用于兼容性**：与现有 CDN 基础设施或仅支持 RTMP 的第三方服务集成时使用 RTMP 转发。

3. **Forward 与 Origin Pull 选择**：
   - **Forward（转推）**：源站需要始终向已知下游节点分发时使用（如主备冗余）。
   - **Origin Pull（回源）**：边缘节点仅在有订阅者时才拉取流（节省带宽）。

4. **端口规划**：在同一主机上运行多个节点进行测试时，确保端口不冲突：
   - RTMP: 1935, 1936, 1937, ...
   - SRT: 6000, 6001, 6002, ...
   - HTTP: 8080, 8081, 8082, ...
   - WebRTC: 不重叠的 UDP 范围（如 30000-30500、30500-31000、31000-31500）
   - API: 8090, 8091, 8092, ...

5. **SRT 加密**：通过不可信网络进行转发时，启用 SRT 加密：
   ```yaml
   cluster:
     srt:
       latency: 200ms
       passphrase: "your-secret-key"
       pbkeylen: 32    # AES-256
   ```
