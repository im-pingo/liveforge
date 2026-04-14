# syntax=docker/dockerfile:1

# ---- Build stage ----
FROM golang:1.26-bookworm AS builder

ARG VERSION=dev
ARG TARGETOS=linux
ARG TARGETARCH

RUN apt-get update && apt-get install -y --no-install-recommends \
    pkg-config \
    libavcodec-dev \
    libswresample-dev \
    libavutil-dev \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=1 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /liveforge ./cmd/liveforge

# ---- Runtime stage ----
FROM debian:bookworm-slim

RUN apt-get update && apt-get install -y --no-install-recommends \
    libavcodec59 \
    libswresample4 \
    libavutil57 \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /etc/liveforge /data

COPY --from=builder /liveforge /usr/local/bin/liveforge
COPY configs/liveforge.yaml /etc/liveforge/liveforge.yaml

# RTMP, RTSP, HTTP(HLS/DASH/FLV), WebRTC, SRT, SIP(UDP), API+Console, Metrics
EXPOSE 1935 8554 8080 8443 6000 5060/udp 8090 9090

VOLUME ["/data"]

ENTRYPOINT ["/usr/local/bin/liveforge"]
CMD ["-c", "/etc/liveforge/liveforge.yaml"]
