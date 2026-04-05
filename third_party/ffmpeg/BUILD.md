# FFmpeg Static Libraries

## Provenance

- **Source**: Built from source (FFmpeg 8.1) with minimal audio-only configuration
- **Version**: 8.1
- **Platform**: darwin/arm64 (Apple Silicon)
- **Build date**: 2026-04-05

## Included Libraries

| Library | Source | Purpose |
|---------|--------|---------|
| libavcodec.a | Minimal FFmpeg build | Audio codec encoding and decoding |
| libavutil.a | Minimal FFmpeg build | FFmpeg core utility functions |
| libswresample.a | Minimal FFmpeg build | Audio resampling and format conversion |
| libopus.a | /opt/homebrew/lib/ | Opus codec (used by WebRTC) |
| libmp3lame.a | /opt/homebrew/lib/ | MP3 encoding (used by RTMP/FLV) |
| libspeex.a | /opt/homebrew/lib/ | Speex codec support |

## Build Commands

The core FFmpeg libraries (libavcodec, libavutil, libswresample) are built from
source with a minimal configuration that includes only audio codecs. This avoids
pulling in dozens of optional video/image library dependencies.

```bash
# Download and extract FFmpeg source
curl -sL https://ffmpeg.org/releases/ffmpeg-8.1.tar.xz -o /tmp/ffmpeg-8.1.tar.xz
tar -xf /tmp/ffmpeg-8.1.tar.xz -C /tmp/

# Configure with audio-only codecs
cd /tmp/ffmpeg-8.1
./configure \
  --disable-programs \
  --disable-doc \
  --disable-network \
  --disable-everything \
  --enable-decoder=pcm_mulaw,pcm_alaw,aac,libopus,mp3float,adpcm_g722,libspeex \
  --enable-encoder=pcm_mulaw,pcm_alaw,aac,libopus,libmp3lame,adpcm_g722,libspeex \
  --enable-libopus \
  --enable-libmp3lame \
  --enable-libspeex \
  --enable-swresample \
  --enable-avcodec \
  --enable-static \
  --disable-shared \
  --extra-cflags="-I/opt/homebrew/include" \
  --extra-ldflags="-L/opt/homebrew/lib"

make -j$(sysctl -n hw.ncpu)

# Copy built libs
cp libavcodec/libavcodec.a third_party/ffmpeg/lib/darwin_arm64/
cp libavutil/libavutil.a third_party/ffmpeg/lib/darwin_arm64/
cp libswresample/libswresample.a third_party/ffmpeg/lib/darwin_arm64/

# Codec libraries (from Homebrew)
cp /opt/homebrew/lib/libopus.a third_party/ffmpeg/lib/darwin_arm64/
cp /opt/homebrew/lib/libmp3lame.a third_party/ffmpeg/lib/darwin_arm64/
cp /opt/homebrew/lib/libspeex.a third_party/ffmpeg/lib/darwin_arm64/

# Headers (from Homebrew FFmpeg)
cp -r /opt/homebrew/opt/ffmpeg/include/libavcodec third_party/ffmpeg/include/
cp -r /opt/homebrew/opt/ffmpeg/include/libavutil third_party/ffmpeg/include/
cp -r /opt/homebrew/opt/ffmpeg/include/libswresample third_party/ffmpeg/include/
```

## Adding Other Platforms

To add linux/amd64 support, build or obtain FFmpeg static libraries and place them in:
```
third_party/ffmpeg/lib/linux_amd64/
```

The CGo directives in `pkg/audiocodec/ff_cgo.go` already reference all platform paths.
