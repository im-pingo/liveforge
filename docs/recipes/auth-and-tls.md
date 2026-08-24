# Authentication And TLS

The checked-in sample configuration is for local development only: it disables TLS and authentication and uses the console credentials `admin/admin`. Never expose it publicly unchanged.

## Prerequisites

- Bind API and media listeners to loopback while testing.
- Obtain a trusted certificate for public browser/API use, or use `tls.auto` only for temporary local development.
- Generate independent high-entropy values for management tokens, JWT secrets, SIP credentials, webhook secrets, source tokens, and console password.
- Store secrets outside version control and inject them at runtime.

## Secure Management Configuration

```yaml
tls:
  cert_file: "/etc/liveforge/tls/fullchain.pem"
  key_file: "/etc/liveforge/tls/privkey.pem"
  auto: false
api:
  listen: "127.0.0.1:8090"
  tls: true
  auth:
    bearer_token: ""
    tokens:
      - {name: monitoring, token: "${VIEWER_TOKEN}", role: viewer}
      - {name: operations, token: "${OPERATOR_TOKEN}", role: operator}
      - {name: administration, token: "${ADMIN_TOKEN}", role: admin}
  console:
    username: "${CONSOLE_USERNAME}"
    password: "${CONSOLE_PASSWORD}"
    role: admin
  audit:
    max_entries: 1000
limits:
  rate_limit: {enabled: true, rate: 20, burst: 40}
auth:
  enabled: true
  publish:
    mode: token
    token: {secret: "${PUBLISH_JWT_SECRET}", algorithm: HS256}
  subscribe:
    mode: token
    token: {secret: "${SUBSCRIBE_JWT_SECRET}", algorithm: HS256}
```

Global TLS files/mode, `api.listen`, `api.tls`, `auth.enabled`, and audit capacity require restart. Named management tokens, the legacy management bearer, console credentials/role, and publish/subscribe rule details are hot-reloadable.

The deprecated management token path is `auth.api.bearer_token`. Move it to `api.auth.bearer_token`. Normalization uses the deprecated value only when the current path is empty; if both exist, `api.auth.bearer_token` wins. They do not create two active credentials. New deployments should prefer named `api.auth.tokens` for attribution and least privilege.

## Verify Locally

```bash
export LIVEFORGE_API=https://127.0.0.1:8090
curl --cacert /etc/liveforge/tls/fullchain.pem -fsS \
  "$LIVEFORGE_API/api/v1/server/health"
curl --cacert /etc/liveforge/tls/fullchain.pem -fsS \
  -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/server/info"
curl --cacert /etc/liveforge/tls/fullchain.pem -sS -o /dev/null -w '%{http_code}\n' \
  -X DELETE -H "Authorization: Bearer $VIEWER_TOKEN" \
  "$LIVEFORGE_API/api/v1/streams/live/test"
```

Health and authorized reads return 200. Viewer deletion returns 403. Missing or invalid credentials return 401; rate limiting can return 429. Do not use `curl -k` as a production workaround.

Management authentication accepts `Authorization: Bearer <token>` or a valid `lf_session` cookie issued by console login. Health is always public. When neither a management bearer nor named token is configured, compatibility mode grants anonymous admin access; use that only on an isolated development listener.

WebRTC publish/subscribe authorization is controlled by `auth.publish` and `auth.subscribe`, not management RBAC. Browser camera/microphone access also requires HTTPS or a browser-recognized secure context; ICE/TURN and UDP reachability are separate requirements.

## Diagnostics And Metrics

Use `GET /api/v1/security/status` and `GET /api/v1/audit` with an authenticated viewer or stronger role. Status never returns token values. Audit metadata removes keys containing token, secret, password, or authorization.

```text
liveforge_api_authentication_failures_total
liveforge_api_authorization_failures_total
liveforge_api_rate_limit_denials_total
liveforge_api_audit_events_total
```

Certificate-name/trust failures occur before HTTP status codes. Once connected, expected failures are 401 for authentication, 403 for permission, and 429 for the per-IP limiter.

## Rollback And Recovery

1. Add and verify replacement management credentials before removing old ones.
2. Keep one offline recovery admin token and the previous valid certificate/key deployment artifact.
3. If a certificate rollout fails, restore the prior matched cert/key pair and restart; do not mix files from different certificates.
4. If hot token rotation fails, restore the last valid source document and refresh from a protected channel.
5. Never recover by disabling authentication or TLS on a public listener. First bind to loopback/private networking or block ingress.
