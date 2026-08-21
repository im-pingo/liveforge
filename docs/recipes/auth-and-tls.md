# Authentication And TLS

The checked-in configuration is intentionally permissive for local development. It is not a production security baseline.

Before public exposure:

1. Replace the console username and password.
2. Set `tls.cert_file` and `tls.key_file`, or use `tls.auto` only for a temporary self-signed development certificate.
3. Enable `auth.enabled` and configure publish and subscribe rules.
4. Set `api.auth.bearer_token` through an environment variable.
5. Restrict firewall exposure to the protocols and RTP/ICE port ranges that are actually needed.
6. Enable `limits.rate_limit` and configure stream and connection limits.

The API bearer token is sent as:

```text
Authorization: Bearer $API_TOKEN
```

Never commit the token, JWT secret, webhook secret, or private key.
