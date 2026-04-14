// ff_cgo_linux_static.go — vendored static FFmpeg libs for Linux.
//
// Activated with: go build -tags ffmpeg_static
//
// Requires static .a files in third_party/ffmpeg/lib/linux_{amd64,arm64}/.
// See third_party/ffmpeg/BUILD.md for build instructions.
//
//go:build linux && ffmpeg_static

package audiocodec

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/ffmpeg/include

#cgo linux,amd64 LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/linux_amd64
#cgo linux,arm64 LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/linux_arm64

#cgo LDFLAGS: -lavcodec -lswresample -lavutil -lopus -lmp3lame -lspeex -lm -lpthread -ldl
*/
import "C"
