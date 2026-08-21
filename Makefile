VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TAGS    ?=

.PHONY: build build-static build-core test test-core clean deps deps-static check-deps

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

# Full build with audio transcoding. Dependencies are checked by the compiler
# (system FFmpeg on Linux, vendored libraries on macOS); no missing helper
# script is required for a normal build.
build:
	CGO_ENABLED=1 go build -trimpath -tags 'audiocodec $(TAGS)' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

# Linux with vendored static FFmpeg libraries.
build-static:
	CGO_ENABLED=1 go build -trimpath -tags 'audiocodec ffmpeg_static $(TAGS)' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

# Dependency-free core build for development and protocol/lifecycle smoke
# tests. Audio transcoding remains explicitly disabled in this artifact.
build-core:
	CGO_ENABLED=0 go build -trimpath -tags '$(TAGS)' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge-core ./cmd/liveforge

test:
	CGO_ENABLED=1 go test -race -tags 'audiocodec $(TAGS)' -cover ./...

test-core:
	CGO_ENABLED=0 go test -race -tags '$(TAGS)' ./...

clean:
	rm -rf bin/

# ---------------------------------------------------------------------------
# Dependency management (auto-detect platform, install if needed)
# ---------------------------------------------------------------------------

# Install/verify system FFmpeg dev packages (shared libs on Linux, vendored on macOS).
deps:
	@echo "Dependencies are resolved by the selected build target; run make check-deps for status."

# Build vendored static libs from FFmpeg source.
deps-static:
	@echo "Static FFmpeg libraries must be present under third_party/ffmpeg/lib/<os>_<arch>/."

# Print dependency status without installing.
check-deps:
	@echo "=== FFmpeg dependency check ==="
	@if [ "$$(uname)" = "Darwin" ]; then \
		echo "Platform: macOS (vendored static libs)"; \
		ARCH=$$(uname -m); \
		if [ "$$ARCH" = "x86_64" ]; then DIR="darwin_amd64"; else DIR="darwin_arm64"; fi; \
		if [ -d "third_party/ffmpeg/lib/$$DIR" ]; then \
			echo "OK: third_party/ffmpeg/lib/$$DIR/"; \
			ls third_party/ffmpeg/lib/$$DIR/*.a 2>/dev/null || echo "WARNING: no .a files"; \
		else \
			echo "MISSING: third_party/ffmpeg/lib/$$DIR/"; \
		fi; \
	elif [ "$$(uname)" = "Linux" ]; then \
		echo "Platform: Linux (system pkg-config)"; \
		if command -v pkg-config >/dev/null 2>&1; then \
			for lib in libavcodec libswresample libavutil; do \
				if pkg-config --exists $$lib 2>/dev/null; then \
					echo "  OK: $$lib $$(pkg-config --modversion $$lib)"; \
				else \
					echo "  MISSING: $$lib"; \
				fi; \
			done; \
		else \
			echo "MISSING: pkg-config"; \
		fi; \
	fi
