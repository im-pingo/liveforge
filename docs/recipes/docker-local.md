# Local Docker Compose

The Compose file builds from the checked-out source by default, so it does not depend on a Docker Hub image being published.

```bash
docker compose build
docker compose up -d
curl http://127.0.0.1:8090/api/v1/server/health
```

To use a released image after a `v*` GitHub Release completes:

```bash
export LIVEFORGE_IMAGE=ghcr.io/im-pingo/liveforge:vX.Y.Z
docker pull "$LIVEFORGE_IMAGE"
docker compose up -d --no-build
```

The GHCR package must be Public for anonymous pulls. If it is private, authenticate first with `docker login ghcr.io` using a token that can read packages.

The sample configuration is for local development only. Change console credentials and enable TLS and authentication before exposing the service.
