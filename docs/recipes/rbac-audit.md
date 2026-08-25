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
```

Named tokens and console credentials are hot-reloadable. `api.audit.max_entries` is allocated at bootstrap and requires restart. `api.auth.bearer_token` is a compatibility admin token; prefer named tokens for attributable operations.

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
