package audiocodec

/*
#cgo CFLAGS: -I${SRCDIR}/../../third_party/ffmpeg/include

#cgo darwin,amd64  LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/darwin_amd64
#cgo darwin,arm64  LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/darwin_arm64
#cgo linux,amd64   LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/linux_amd64
#cgo linux,arm64   LDFLAGS: -L${SRCDIR}/../../third_party/ffmpeg/lib/linux_arm64

#cgo LDFLAGS: -lavcodec -lswresample -lavutil -lopus -lmp3lame -lspeex -lm -lpthread
#cgo darwin LDFLAGS: -framework CoreFoundation -framework AudioToolbox -framework VideoToolbox -framework CoreMedia -framework CoreVideo -framework CoreServices -framework Security -liconv -lz -lbz2 -llzma
#cgo linux LDFLAGS: -ldl
*/
import "C"
