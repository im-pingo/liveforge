VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TAGS    ?=

.PHONY: build build-static test clean deps deps-static check-deps

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

# Default: auto-install deps, then build.
#   macOS  — vendored static libs (checked/built automatically)
#   Linux  — system pkg-config shared libs (auto-installed)
build: deps
	CGO_ENABLED=1 go build -trimpath -tags '$(TAGS)' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

# Linux with vendored static FFmpeg libs (built from source automatically).
build-static: deps-static
	CGO_ENABLED=1 go build -trimpath -tags 'ffmpeg_static' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

test: deps
	CGO_ENABLED=1 go test -race -tags '$(TAGS)' -cover ./...

clean:
	rm -rf bin/

# ---------------------------------------------------------------------------
# Dependency management (auto-detect platform, install if needed)
# ---------------------------------------------------------------------------

# Install/verify system FFmpeg dev packages (shared libs on Linux, vendored on macOS).
deps:
	@./scripts/install-deps.sh

# Build vendored static libs from FFmpeg source.
deps-static:
	@./scripts/install-deps.sh --static

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
