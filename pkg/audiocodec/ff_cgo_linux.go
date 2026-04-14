// ff_cgo_linux.go — use system FFmpeg via pkg-config (default on Linux).
//
// Install dev packages:
//   Debian/Ubuntu: apt install libavcodec-dev libswresample-dev libavutil-dev
//   RHEL/Fedora:   dnf install libavcodec-free-devel libswresample-free-devel libavutil-free-devel
//   Alpine:        apk add ffmpeg-dev
//
// To use vendored static libs instead, build with: go build -tags ffmpeg_static
//
//go:build linux && !ffmpeg_static

package audiocodec

/*
#cgo pkg-config: libavcodec libswresample libavutil
#cgo LDFLAGS: -lm -lpthread
*/
import "C"
