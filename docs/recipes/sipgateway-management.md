# SIP Gateway Management

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

## Prerequisites

- Ensure SIP signaling and the configured RTP/RTCP range are reachable only by intended peers.
- Keep the API listener on loopback while validating the setup.
- Publish the stream referenced by outbound calls before dialing.
- Export credentials without placing them in shell history or source control:

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
export VIEWER_TOKEN='replace-me'
export OPERATOR_TOKEN='replace-me'
```

Example configuration:

```yaml
sip:
  enabled: true
  listen: "127.0.0.1:5060"
  transport: [udp, tcp]
  server_id: "34020000002000000001"
  domain: "3402000000"
  auth:
    enabled: true
    password: "${SIP_PASSWORD}"
  gateway:
    enabled: true
    stream_prefix: sip
    rtp_port_range: [30000, 30100]
    codecs: [opus, PCMA, PCMU]
    max_calls: 100
api:
  auth:
    tokens:
      - {name: sip-reader, token: "${VIEWER_TOKEN}", role: viewer}
      - {name: sip-operator, token: "${OPERATOR_TOKEN}", role: operator}
```

SIP module and gateway configuration is restart-required.

For a local protocol check that does not require a remote SIP peer, run the
[SIP and GB28181 Protocol Test Lab](protocol-test-lab.md) or use the Test Lab
section on the Console SIP page. The SIP self-test validates SDP/codec selection,
RTP/RTCP allocation, and localhost UDP loopback before any dial attempt.

## List And Dial

```bash
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/sipgateway/calls"
curl -fsS -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"target_uri":"sip:1001@127.0.0.1:5062","stream_key":"live/camera"}' \
  "$LIVEFORGE_API/api/v1/sipgateway/calls"
```

The list returns 200. A successful dial returns 201 and a `call_id`. Dial can return 400 for malformed JSON or URI, 404 for a missing stream, 422 for a missing target or codec mismatch, 429 for gateway capacity/RTP port exhaustion or API rate limiting, 502 for redacted setup failure, and 503 when the gateway is disabled, closed, or unavailable. Authentication and authorization failures are 401 and 403.

## Inspect And Hang Up

```bash
export CALL_ID='value-from-dial-response'
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/sipgateway/calls/$CALL_ID"
curl -fsS -X DELETE -H "Authorization: Bearer $OPERATOR_TOKEN" \
  "$LIVEFORGE_API/api/v1/sipgateway/calls/$CALL_ID"
```

Success is 200. A missing call is 404 and an unavailable module is 503. Viewer can inspect but receives 403 on dial or hangup because those operations require `sip:calls`.

## Metrics And Diagnostics

The list response includes call snapshots and counters. Prometheus exports:

```text
liveforge_sipgateway_active_calls{direction="inbound|outbound"}
liveforge_sipgateway_calls_started_total
liveforge_sipgateway_calls_ended_total
liveforge_sipgateway_setup_failures_total
liveforge_sipgateway_codec_failures_total
liveforge_sipgateway_duplicate_call_ids_total
liveforge_sipgateway_port_exhaustions_total
liveforge_sipgateway_capacity_rejections_total
liveforge_sipgateway_network_failures_total
liveforge_sipgateway_rtp_packets_total{direction="sent|received"}
liveforge_sipgateway_rtp_bytes_total{direction="sent|received"}
```

Check call `state`, `last_error`, `last_rtp_at`, packet counts, advertised codecs, firewall rules, and port-range exhaustion together. Call IDs and peer addresses are intentionally not Prometheus labels.

## Rollback And Recovery

1. Hang up test calls before reverting configuration.
2. Restore the previous SIP/gateway block and restart; these settings are not hot-reloaded.
3. For 422, align the offered gateway codec list with the active stream and remote endpoint.
4. For 429 capacity or port exhaustion, end stale calls before expanding ranges; a port-range change requires restart.
5. For 502, inspect structured logs and the remote SIP response. The API deliberately does not return raw upstream bodies.
