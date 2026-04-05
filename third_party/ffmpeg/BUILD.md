# FFmpeg Static Libraries

## Provenance

- **Source**: Homebrew (`brew install ffmpeg`)
- **Version**: 8.1
- **Platform**: darwin/arm64 (Apple Silicon)
- **Extraction date**: 2026-04-05

## Included Libraries

| Library | Source Path | Purpose |
|---------|-----------|---------|
| libavcodec.a | /opt/homebrew/opt/ffmpeg/lib/ | Audio/video codec encoding and decoding |
| libavutil.a | /opt/homebrew/opt/ffmpeg/lib/ | FFmpeg core utility functions |
| libswresample.a | /opt/homebrew/opt/ffmpeg/lib/ | Audio resampling and format conversion |
| libopus.a | /opt/homebrew/lib/ | Opus codec (used by WebRTC) |
| libmp3lame.a | /opt/homebrew/lib/ | MP3 encoding (used by RTMP/FLV) |
| libspeex.a | /opt/homebrew/lib/ | Speex codec support |

## Extraction Commands

```bash
# FFmpeg libraries
cp /opt/homebrew/opt/ffmpeg/lib/libavcodec.a third_party/ffmpeg/lib/darwin_arm64/
cp /opt/homebrew/opt/ffmpeg/lib/libavutil.a third_party/ffmpeg/lib/darwin_arm64/
cp /opt/homebrew/opt/ffmpeg/lib/libswresample.a third_party/ffmpeg/lib/darwin_arm64/

# Codec libraries
cp /opt/homebrew/lib/libopus.a third_party/ffmpeg/lib/darwin_arm64/
cp /opt/homebrew/lib/libmp3lame.a third_party/ffmpeg/lib/darwin_arm64/
cp /opt/homebrew/lib/libspeex.a third_party/ffmpeg/lib/darwin_arm64/

# Headers
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
