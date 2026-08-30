# Cluster Relay Operations

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

## Prerequisites

- Use versioned binaries or a commit SHA on every node.
- Bind the management listener to a private interface or loopback during validation.
- Allow only required media and relay port ranges between known peers.
- Configure the same management credential policy on signaling peers.

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
export CLUSTER_ADMIN_TOKEN='replace-me'
```

Example node configuration:

```yaml
api:
  listen: "127.0.0.1:8090"
  auth:
    tokens:
      - name: cluster-admin
        token: "${CLUSTER_ADMIN_TOKEN}"
        role: admin
cluster:
  forward:
    enabled: true
    targets: ["rtp://127.0.0.1:8091/live/camera"]
    retry_max: 5
    retry_interval: 3s
  origin:
    enabled: false
    servers: []
    idle_timeout: 30s
  health_check:
    enabled: true
    interval: 10s
    timeout: 2s
    evict_threshold: 3
  relay_pool:
    max_per_host: 8
  rtp:
    port_range: "20000-20100"
    signaling_path: "/api/relay"
    rtcp_interval: 5s
    timeout: 15s
  gb28181:
    port_range: [41000, 41100]
    signaling_path: "/api/relay/gb"
    rtcp_interval: 5s
    timeout: 15s
```

Forward/origin targets, scheduling, retry policy, origin idle timeout, and enabled health-check timing are hot-reloadable. Enabling those managers, transport settings, pool size, signaling paths, and port ranges require restart.

## Status And Internal Signaling

```bash
curl -fsS -H "Authorization: Bearer $CLUSTER_ADMIN_TOKEN" \
  "$LIVEFORGE_API/api/v1/cluster/status"
```

Status returns 200, including active forward/origin counts, bounded relay snapshots, peer failures/eviction, and `truncated`. An absent cluster module returns a disabled-compatible empty 200 response. Authentication/authorization/rate failures are 401/403/429.

The default internal endpoints are `POST /api/relay/push`, `POST /api/relay/pull`, `POST /api/relay/gb/push`, and `POST /api/relay/gb/pull`. They require `server:mutate` and are node-to-node contracts, not operator workflows. RTP signaling uses SDP; GB signaling exchanges stream/port query values. Expected failures include 400 for invalid input, 404 for a missing pull stream, and 503 for allocation or setup failure.

RTP relay media admission fails closed: packetizer errors, empty packetizer output, nil packets, RTP marshal errors, UDP write errors, and short writes terminate the affected relay instead of being counted as successful media. Push cancellation remains a normal shutdown path; non-cancellation send failures are reported in bounded logs and relay status.

## Credential Selection And Hot Rotation

For every RTP/GB peer request, the node loads the current atomic configuration and selects credentials in this order:

1. Non-empty `api.auth.bearer_token`.
2. The first `api.auth.tokens` entry whose role is `admin`.

This lookup is per request and does not perform source I/O or block on refresh. Rotation is therefore hot:

1. Add the new admin token to receiving peers while retaining the old token.
2. Refresh and confirm `config_changes_accepted` and a new active hash.
3. Update sending nodes so their first selected admin credential is the new token; refresh and exercise a relay.
4. Remove the old token from receivers and refresh again.

If management auth is configured but no usable admin token exists, signaling fails locally before contacting the peer. This is deliberate: an operator/viewer token cannot satisfy `server:mutate`. If no management auth is configured, compatibility mode sends no credential and the peer can accept anonymous admin requests; do not use that mode on an exposed network.

Peer HTTP error bodies are bounded and redacted. Tokens, credentials, query secrets, arbitrary upstream bodies, and unbounded error content must not appear in status, API errors, or logs.

## Metrics And Diagnostics

```text
cluster_relay_active{direction="forward|origin",protocol="rtmp|srt|rtsp|rtp|gb28181"}
cluster_relay_errors_total{direction,protocol,error_type="connection"}
cluster_relay_bytes_total{direction,protocol}
cluster_relay_latency_seconds{protocol}
cluster_rtp_packet_loss_ratio{direction}
```

The label set is intentionally finite. Do not add stream keys, URLs, peer hosts, raw errors, or call IDs as labels. Correlate status peer failures, relay counters, bounded logs, port availability, and reachability.

## Rollback And Recovery

1. Restore the last known target/origin list and refresh; current relays remain visible in status during convergence.
2. During credential failure, restore the previous admin token on receivers first, then refresh senders.
3. For a pending signaling-path or port-range change, revert it or restart all affected peers in a coordinated window.
4. For evicted peers, repair connectivity/auth first; health checks will record later success.
5. Do not bypass RBAC with a viewer/operator token and do not expose internal signaling through an untrusted proxy.
