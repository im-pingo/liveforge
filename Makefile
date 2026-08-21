VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
TAGS    ?= audiocodec

.PHONY: build build-static test clean deps deps-static check-deps check-deps-strict check-deps-static

# ---------------------------------------------------------------------------
# Build targets
# ---------------------------------------------------------------------------

# Default: verify dependencies, then build.
#   macOS  — vendored static libs under third_party/ffmpeg
#   Linux  — system pkg-config shared libs
build: deps
	CGO_ENABLED=1 go build -trimpath -tags '$(TAGS)' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

# Linux with vendored static FFmpeg libs.
build-static: deps-static
	CGO_ENABLED=1 go build -trimpath -tags 'ffmpeg_static audiocodec' \
		-ldflags "-s -w -X main.version=$(VERSION)" \
		-o bin/liveforge ./cmd/liveforge

test: deps
	CGO_ENABLED=1 go test -race -tags '$(TAGS)' -cover ./...

clean:
	rm -rf bin/

# ---------------------------------------------------------------------------
# Dependency management (verify only; installation is platform-specific)
# ---------------------------------------------------------------------------

# Verify FFmpeg development dependencies. Installation is platform-specific and
# intentionally not performed by Makefile to avoid mutating a developer host.
deps:
	@$(MAKE) --no-print-directory check-deps-strict

# Build with vendored static FFmpeg libs. The libraries must already be present.
deps-static:
	@$(MAKE) --no-print-directory check-deps-static

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

check-deps-strict:
	@set -e; \
	if [ "$$(uname)" = "Darwin" ]; then \
		ARCH="$$(uname -m)"; \
		[ "$$ARCH" = "x86_64" ] && DIR="darwin_amd64" || DIR="darwin_arm64"; \
		for lib in libavcodec.a libswresample.a libavutil.a; do \
			test -f "third_party/ffmpeg/lib/$$DIR/$$lib" || { echo "MISSING: third_party/ffmpeg/lib/$$DIR/$$lib (run make check-deps)" >&2; exit 1; }; \
		done; \
	else \
		command -v pkg-config >/dev/null 2>&1 || { echo "MISSING: pkg-config" >&2; exit 1; }; \
		for lib in libavcodec libswresample libavutil; do \
			pkg-config --exists "$$lib" || { echo "MISSING: $$lib development package" >&2; exit 1; }; \
		done; \
	fi

check-deps-static:
	@set -e; \
	OS="$$(uname | tr '[:upper:]' '[:lower:]')"; \
	ARCH="$$(uname -m)"; \
	[ "$$ARCH" = "x86_64" ] && ARCH="amd64" || ARCH="arm64"; \
	DIR="$$OS"_"$$ARCH"; \
	for lib in libavcodec.a libswresample.a libavutil.a; do \
		test -f "third_party/ffmpeg/lib/$$DIR/$$lib" || { echo "MISSING: third_party/ffmpeg/lib/$$DIR/$$lib" >&2; exit 1; }; \
	done
