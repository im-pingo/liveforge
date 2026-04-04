# Cluster Deployment Guide

LiveForge supports multi-node cluster relay with two modes:

- **Forward (push)** — The origin server actively pushes streams to one or more downstream nodes when a publisher connects.
- **Origin pull** — Edge nodes pull streams on-demand from upstream origin servers when a subscriber requests a stream that doesn't exist locally.

Both modes support multiple transport protocols: **RTMP**, **SRT**, **RTSP**, and **RTP**. The protocol is determined by the URL scheme in the configuration (`rtmp://`, `srt://`, `rtsp://`, `rtp://`).

## Protocol Selection

| Protocol | Use Case | Latency | Notes |
|----------|----------|---------|-------|
| **SRT** | Low-latency relay | ~120ms | FEC + ARQ error recovery, AES encryption, ideal for unreliable networks |
| **RTMP** | General-purpose relay | ~1-3s | Widely supported, good for CDN cascading and compatibility |
| **RTSP** | IP camera integration | ~1-2s | TCP or UDP transport, good for surveillance scenarios |
| **RTP** | Direct RTP relay | ~100ms | SDP-over-HTTP signaling, lowest overhead |

## Topology Examples

### 1. SRT Low-Latency 3-Node Cluster

```
Publisher (FFmpeg/OBS)
    │
    ▼ RTMP push
┌─────────┐   SRT forward    ┌─────────┐   SRT origin pull   ┌─────────┐
│ Node A  │ ───────────────▶  │ Node B  │ ◀─────────────────  │ Node C  │
│ (origin)│   :6000 → :6001  │ (relay) │   :6001 ← :6002    │  (edge) │
└─────────┘                   └─────────┘                     └─────────┘
    │                              │                               │
    ▼                              ▼                               ▼
  Players                       Players                         Players
```

- **Node A**: Receives the publish, forwards via SRT to Node B
- **Node B**: Receives the SRT relay from A, serves subscribers
- **Node C**: Pulls from Node B on-demand when a subscriber connects

#### Node A Configuration (Origin + SRT Forward)

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
      - "srt://127.0.0.1:6001/live"   # SRT forward to Node B
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

#### Node B Configuration (SRT Relay Hub)

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

#### Node C Configuration (SRT Edge / Origin Pull)

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
      - "srt://127.0.0.1:6001"   # Pull from Node B via SRT
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

#### Testing the SRT Cluster

```bash
# Start all three nodes
./liveforge -c configs/srt-node-a.yaml &
./liveforge -c configs/srt-node-b.yaml &
./liveforge -c configs/srt-node-c.yaml &

# Publish to Node A
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://127.0.0.1:1935/live/test

# Node A forwards to Node B automatically via SRT.
# Verify on Node B:
ffplay rtmp://127.0.0.1:1936/live/test

# Node C pulls from Node B on-demand when a subscriber connects:
ffplay rtmp://127.0.0.1:1937/live/test

# WebRTC playback on any node:
# http://127.0.0.1:8090/console  (Node A)
# http://127.0.0.1:8091/console  (Node B)
# http://127.0.0.1:8092/console  (Node C)
```

---

### 2. RTMP General-Purpose 2-Node Cluster

```
Publisher (FFmpeg/OBS)
    │
    ▼ RTMP push
┌─────────┐  RTMP forward   ┌─────────┐
│ Node A  │ ──────────────▶  │ Node B  │
│ (origin)│  :1935 → :1936  │  (edge) │
└─────────┘                  └─────────┘
    │                             │
    ▼                             ▼
  Players                      Players
```

#### Node A Configuration (RTMP Origin + Forward)

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
      - "rtmp://192.168.1.100:1935/live"   # RTMP forward to Node B
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

#### Node B Configuration (RTMP Edge)

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

#### Testing the RTMP Cluster

```bash
# Start both nodes (on different hosts, or adjust ports for same host)
./liveforge -c configs/rtmp-node-a.yaml &   # Host A
./liveforge -c configs/rtmp-node-b.yaml &   # Host B (192.168.1.100)

# Publish to Node A
ffmpeg -re -i input.mp4 -c copy -f flv rtmp://host-a:1935/live/test

# Node A forwards to Node B via RTMP automatically.
# Play from either node:
ffplay rtmp://host-a:1935/live/test    # From origin
ffplay rtmp://host-b:1935/live/test    # From edge
```

---

### 3. RTMP Origin Pull (Edge → Origin)

For on-demand pulling instead of always-on forwarding:

```
Publisher (FFmpeg/OBS)
    │
    ▼ RTMP push
┌─────────┐                  ┌─────────┐
│ Origin  │ ◀─── RTMP pull ──│  Edge   │ ◀── Subscriber
│  Node   │     (on-demand)  │  Node   │
└─────────┘                  └─────────┘
```

#### Edge Node Configuration (Origin Pull)

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
      - "rtmp://origin-host:1935"    # Pull from origin via RTMP
    idle_timeout: 30s                 # Stop pulling after no subscribers for 30s
    retry_max: 3
    retry_delay: 2s

api:
  enabled: true
  listen: ":8090"
  console:
    username: "admin"
    password: "admin"
```

When a subscriber requests `live/test` on the edge and the stream doesn't exist locally, the edge automatically connects to `rtmp://origin-host:1935/live/test` and starts relaying. When the last subscriber leaves, the relay is torn down after `idle_timeout`.

---

## Configuration Reference

### Forward Push

```yaml
cluster:
  forward:
    enabled: true
    targets:
      - "rtmp://host:1935/live"       # RTMP relay
      - "srt://host:6000/live"        # SRT relay (low-latency)
      - "rtsp://host:554/live"        # RTSP relay
      - "rtp://host:8090/live"        # RTP direct relay
    schedule_url: ""                   # HTTP scheduler for dynamic targets
    schedule_priority: schedule_first  # schedule_first | config_first
    schedule_timeout: 3s
    retry_max: 5
    retry_interval: 3s
```

**How it works**: When a stream is published on this node, it is automatically pushed to all configured `targets`. The stream name from the publish URL is appended to each target URL.

**Multiple targets**: You can list multiple targets with different protocols. All targets receive the stream simultaneously.

**HTTP scheduler**: Set `schedule_url` to dynamically resolve forward targets via an HTTP callback. The scheduler receives the stream key and returns a list of target URLs.

### Origin Pull

```yaml
cluster:
  origin:
    enabled: true
    servers:
      - "rtmp://origin1:1935"         # RTMP origin
      - "srt://origin2:6000"          # SRT origin
    schedule_url: ""                   # HTTP scheduler for dynamic origin resolution
    schedule_priority: schedule_first
    schedule_timeout: 3s
    idle_timeout: 30s                  # Tear down after no subscribers
    retry_max: 3
    retry_delay: 2s
```

**How it works**: When a subscriber requests a stream that doesn't exist locally, the node tries each origin server in order until one succeeds. The stream is relayed locally for all subscribers. When the last subscriber leaves, the relay is torn down after `idle_timeout`.

### Protocol-Specific Settings

```yaml
cluster:
  # SRT relay transport settings
  srt:
    latency: 120ms        # SRT latency (receiver buffer). Lower = less delay, higher = more resilience
    # passphrase: ""      # AES encryption passphrase (must match on both ends)
    # pbkeylen: 16        # Key length: 16 (AES-128), 24 (AES-192), or 32 (AES-256)

  # RTSP relay transport settings
  rtsp:
    transport: tcp        # tcp or udp

  # RTP direct relay settings (SDP-over-HTTP signaling)
  rtp:
    port_range: "20000-20100"
    signaling_path: "/api/relay"
    rtcp_interval: 5s
    timeout: 15s
```

## WebRTC Configuration

For WebRTC playback from any cluster node, enable WebRTC with ICE Lite for fast connection:

```yaml
webrtc:
  enabled: true
  listen: ":8443"
  ice_lite: true                       # Skip ICE gathering — instant negotiation
  ice_servers:
    - urls: ["stun:stun.l.google.com:19302"]
  udp_port_range: [20000, 30000]       # Must not overlap with cluster RTP ports
  candidates: []                        # External IP addresses (for NAT traversal)
  gcc:
    enabled: true
    initial_bitrate: 2000000           # Fallback initial rate (auto-tuned from stream)
    min_bitrate: 100000
    max_bitrate: 10000000
```

**ICE Lite**: Recommended for servers with a public IP or directly accessible on LAN. The server skips STUN queries and connectivity checks, reducing WebRTC negotiation from ~5s to <200ms.

**GCC congestion control**: The initial bitrate is automatically set to the stream's actual bitrate (+ 20% headroom) when a WHEP subscriber connects. The `initial_bitrate` config value is only used as a fallback when the stream's bitrate hasn't been measured yet.

**Port ranges**: Ensure WebRTC UDP ports (`udp_port_range`) do not overlap with cluster RTP ports (`cluster.rtp.port_range`) when both are enabled on the same node.

## Best Practices

1. **SRT for low-latency relay**: Use SRT between nodes when you need sub-200ms inter-node latency. Set `latency: 120ms` for LAN, increase to `200-500ms` for WAN with packet loss.

2. **RTMP for compatibility**: Use RTMP relay when integrating with existing CDN infrastructure or third-party services that only support RTMP.

3. **Forward vs Origin Pull**:
   - Use **forward** when the origin always needs to distribute to known downstream nodes (e.g., active-active redundancy).
   - Use **origin pull** when edge nodes should only fetch streams when subscribers are present (saves bandwidth).

4. **Port planning**: When running multiple nodes on the same host for testing, ensure no port conflicts:
   - RTMP: 1935, 1936, 1937, ...
   - SRT: 6000, 6001, 6002, ...
   - HTTP: 8080, 8081, 8082, ...
   - WebRTC: non-overlapping UDP ranges (e.g., 30000-30500, 30500-31000, 31000-31500)
   - API: 8090, 8091, 8092, ...

5. **SRT encryption**: For relay over untrusted networks, enable SRT encryption:
   ```yaml
   cluster:
     srt:
       latency: 200ms
       passphrase: "your-secret-key"
       pbkeylen: 32    # AES-256
   ```
