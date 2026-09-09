# RBAC And Audit Operations

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

## Prerequisites And Configuration

Keep the API on loopback while creating tokens. Supply secrets through deployment secret injection, not committed YAML.

```yaml
api:
  listen: "127.0.0.1:8090"
  auth:
    bearer_token: ""
    tokens:
      - {name: monitor, token: "${VIEWER_TOKEN}", role: viewer}
      - {name: operator, token: "${OPERATOR_TOKEN}", role: operator}
      - {name: administrator, token: "${ADMIN_TOKEN}", role: admin}
  console:
    username: "${CONSOLE_USERNAME}"
    password: "${CONSOLE_PASSWORD}"
    role: admin
  audit:
    max_entries: 1000
    path: ""              # Optional private NDJSON path; empty disables disk storage.
    max_bytes: 8388608     # Per data file; one rotated .1 file is retained.
```

Named tokens and console credentials are hot-reloadable. `api.audit.max_entries` is allocated at bootstrap and requires restart. `api.auth.bearer_token` is a compatibility admin token; prefer named tokens for attributable operations.

All audit settings require restart. On Linux and macOS, setting `api.audit.path` enables
synchronous NDJSON persistence. Trusted environment references and `~/` expand.
The store keeps the active file, one rotated `.1` file, and an empty `.lock`
file. Each must be a regular, single-link file with mode0600; symlinks, unsafe
permissions, and a second writer are rejected. `max_bytes` defaults to8MiB per
data file (zero selects the default); valid positive values are64KiB..1GiB.
Tightening below an existing file's size fails startup. Configured persistence
on unsupported operating systems fails explicitly; empty-path memory mode has
no filesystem dependency.

Startup streams bounded64KiB NDJSON lines from the two files and restores the
last `max_entries` entries in append order. It discards/truncates an incomplete
final crash line but rejects malformed complete or oversized entries. Restored
events do not increment this process's audit event counter. Entries are bounded
and sanitized before memory, logs, and disk; request bodies and supplied
credentials are never audit metadata. Writes are synchronous; rotation and
Close sync the file, but there is no per-event fsync durability guarantee.
The API drains active HTTP requests up to `server.drain_timeout` (default10s)
before closing and syncing audit storage. A runtime persistence failure stops
further disk appends until restart while memory and structured logging remain
available. `GET /api/v1/security/status` exposes `audit_persistence` with
`enabled`, `healthy`, bounded `error`, and `write_failures_total`, without a path.

The deprecated `auth.api.bearer_token` migrates only when `api.auth.bearer_token` is empty. If both exist, the current `api.auth.bearer_token` wins; they are not two active tokens. Migration is one move:

```text
auth.api.bearer_token -> api.auth.bearer_token
```

## Role Matrix

| Permission | Viewer | Operator | Admin |
| --- | --- | --- | --- |
| `server:read`, `streams:read`, `cluster:read`, `sip:read` | yes | yes | yes |
| `recordings:read`, `audit:read`, `gb28181:read` | yes | yes | yes |
| `streams:kick`, `sip:calls`, `config:reload`, `gb28181:control` | no | yes | yes |
| `streams:delete`, `recordings:delete`, `gb28181:delete` | no | no | yes |
| `gb28181:manage`, `server:mutate`, `debug:read` | no | no | yes |

Registered module handlers use the permission of the handler actually matched
by the HTTP mux. Modules can declare it with `core.WithAPIPermission`;
unannotated custom writes require `server:mutate`, even below a built-in read
or operator namespace. Native GB28181 operations declare their read, control,
manage, and delete permissions explicitly. Cluster relay registrations are
POST-only and require `server:mutate`. Public health/login/logout exemptions
belong to the built-in handlers; a custom handler using the same path does not
inherit them. Authentication, authorization, and mutation audit use this same
matched-handler policy, including rate-limit denials.

## Verify Authentication And Authorization

```bash
export LIVEFORGE_API=http://127.0.0.1:8090
curl -fsS "$LIVEFORGE_API/api/v1/server/health"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/security/status"
curl -fsS -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/audit"
curl -sS -o /dev/null -w '%{http_code}\n' -X POST \
  -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/config/refresh"
curl -fsS -X POST -H "Authorization: Bearer $OPERATOR_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/config/refresh"
```

Health returns 200 without credentials. Valid reads return 200. The viewer mutation returns 403. The operator refresh returns 202. Missing/invalid credentials return 401 and rate limiting can return 429. Destructive stream/recording/device operations require explicit admin action.

The console login issues the `lf_session` cookie; the session has the configured console role and can authenticate management requests. Static console assets are public, while `/console` and `/console/cert.pem` require a valid session when console credentials exist. Debug/pprof endpoints require admin `debug:read`.

## Audit Semantics

`GET /api/v1/audit` supports exact `principal`, `action`, `result`, inclusive
RFC3339 `since`/`until`, and `order=asc|desc`. Optional `limit=1..500` and
`offset` paginate the matching retained entries while preserving the original
data array. `format=ndjson` returns an authenticated download of the matching
retained history. The Console provides50-row pages, filters, and export.
Pagination and export operate on the bounded in-memory trail, including
startup-restored entries, rather than searching an unlimited disk archive.

The bounded trail records authentication failures, authorization denials, failed console logins, mutation outcomes, mutation rate-limit denials, and accepted runtime applications (`config:apply`). Entries contain time, request ID, principal, role, action, resource, result, remote address, and optional metadata. Metadata keys containing token, secret, password, or authorization are removed. Entries are also emitted as structured logs.

Security diagnostics and Prometheus counters are:

```text
GET /api/v1/security/status
GET /api/v1/audit
liveforge_api_authentication_failures_total
liveforge_api_authorization_failures_total
liveforge_api_rate_limit_denials_total
liveforge_api_audit_events_total
```

## Rollback And Recovery

1. Keep one separately stored admin credential valid throughout a rotation.
2. Add and verify a replacement token before removing the old token.
3. If all credentials are rejected, restore the last valid configuration through the protected source/deployment channel and restart only if the source cannot be refreshed.
4. Treat an unexpected rise in authentication/authorization failures as an incident; review bounded audit entries and upstream access logs without logging supplied tokens.
5. Do not recover by disabling auth on a publicly reachable listener. Bind to loopback or a private interface first.
