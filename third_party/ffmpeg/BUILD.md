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

## Build Modes

### Linux — System Libraries (default)

The default `make build` on Linux uses `pkg-config` to find system-installed FFmpeg
shared libraries. Install the dev packages for your distro:

```bash
# Debian / Ubuntu
sudo apt install pkg-config libavcodec-dev libswresample-dev libavutil-dev

# RHEL / Fedora
sudo dnf install pkgconf-pkg-config libavcodec-free-devel libswresample-free-devel libavutil-free-devel

# Alpine
sudo apk add ffmpeg-dev pkgconf
```

Then build normally:

```bash
make build
```

Run `make check-deps` to verify all required libraries are found.

### Linux — Vendored Static Libraries

To use vendored static `.a` files instead of system libraries:

```bash
make build-static
```

This requires static libraries in `third_party/ffmpeg/lib/linux_amd64/` (or
`linux_arm64/`). Build them from source:

```bash
# Install build dependencies
# Debian/Ubuntu:
sudo apt install build-essential nasm libopus-dev libmp3lame-dev libspeex-dev

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
  --enable-pic

make -j$(nproc)

# Determine target directory
ARCH=$(uname -m)
if [ "$ARCH" = "x86_64" ]; then DIR="linux_amd64"; else DIR="linux_arm64"; fi

# Copy built libs (adjust path to your repo checkout)
mkdir -p third_party/ffmpeg/lib/$DIR
cp libavcodec/libavcodec.a   third_party/ffmpeg/lib/$DIR/
cp libavutil/libavutil.a     third_party/ffmpeg/lib/$DIR/
cp libswresample/libswresample.a third_party/ffmpeg/lib/$DIR/

# Copy codec static libs (from system)
cp /usr/lib/$(uname -m)-linux-gnu/libopus.a   third_party/ffmpeg/lib/$DIR/ 2>/dev/null || \
cp /usr/lib64/libopus.a                        third_party/ffmpeg/lib/$DIR/
cp /usr/lib/$(uname -m)-linux-gnu/libmp3lame.a third_party/ffmpeg/lib/$DIR/ 2>/dev/null || \
cp /usr/lib64/libmp3lame.a                     third_party/ffmpeg/lib/$DIR/
cp /usr/lib/$(uname -m)-linux-gnu/libspeex.a   third_party/ffmpeg/lib/$DIR/ 2>/dev/null || \
cp /usr/lib64/libspeex.a                       third_party/ffmpeg/lib/$DIR/
```

### macOS — Vendored Static Libraries (default)

macOS always uses vendored static libs from `third_party/ffmpeg/lib/darwin_{amd64,arm64}/`.

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
