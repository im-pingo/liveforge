# Runtime Configuration Sources

LiveForge reads the bootstrap YAML file once during startup. After startup, a background manager polls the selected source and publishes immutable snapshots. Reading a configuration value uses an atomic in-memory load; it never performs file or network I/O and does not wait for a refresh.

## Local file (default)

```yaml
runtime:
  source: file
  poll_interval: 30s
  load_timeout: 10s
  file:
    path: ""
```

An empty `file.path` uses the path passed with `-c`. A changed file is parsed, normalized, validated, and published only when its normalized content changes.

## HTTP distribution

```yaml
runtime:
  source: http
  poll_interval: 15s
  http:
    url: "https://config.example.internal/liveforge.yaml"
    token: "${LIVEFORGE_CONFIG_TOKEN}"
    max_bytes: 4194304
```

The client sends `If-None-Match` and `If-Modified-Since` after the first successful response. `304 Not Modified` does not replace the active snapshot. Use a private HTTPS endpoint and inject the token through a secret manager or environment variable.

## Consul KV

```yaml
runtime:
  source: consul
  poll_interval: 15s
  consul:
    address: "https://consul.example.internal"
    prefix: "liveforge"
    token: "${CONSUL_HTTP_TOKEN}"
```

The loader reads one KV prefix in one request, decodes values, and maps dotted or slash-separated keys to a complete configuration document. The Consul index is used as the source version when available.

## Redis

Hash mode reads all fields in one `HGETALL`:

```yaml
runtime:
  source: redis
  redis:
    addr: "redis.example.internal:6379"
    hash: "liveforge:config"
    version_key: "liveforge:config:version"
    password: "${REDIS_PASSWORD}"
    tls: true
```

Prefix mode uses `SCAN` and a pipelined batch of `GET` operations:

```yaml
runtime:
  source: redis
  redis:
    addr: "redis.example.internal:6379"
    prefix: "liveforge:config:"
```

Redis fields use dotted or slash-separated paths such as `server.name` and `limits.max_connections`. A field named `config`, `config.yaml`, `config.yml`, or `config.json` is treated as a complete document.

## Failure and restart behavior

- A source timeout, unavailable backend, parse error, or validation error retains the last valid snapshot and increments the source failure status.
- `SIGHUP` only schedules an asynchronous refresh; it does not perform I/O in the signal loop.
- Policy values such as limits, auth rules, log level, recording/DVR policy, and segment settings are hot-reloadable when a module supports the change.
- Listener addresses, module enablement, TLS files/mode, port ranges, and audio codec enablement are reported as `restart_required`; they are not partially applied to live listeners.
- Simulcast configuration is retained but layer selection is deferred and is not supported by the runtime.

Do not place credentials in committed configuration files or logs. Use environment expansion only as a bridge to a deployment secret store.
