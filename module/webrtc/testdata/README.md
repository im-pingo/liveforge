# WebRTC Media Fixtures

`simulcast_80x46.h264` and `simulcast_160x90.h264` are synthetic FFmpeg
`testsrc2` frames, not captured camera media. They provide distinct H.264
sequence headers and pictures for the three-RID isolation and resume tests;
the existing `test_320x180.h264` supplies the highest layer.

Generate either small fixture by replacing `SIZE` and `OUTPUT`:

```sh
ffmpeg -hide_banner -loglevel error -f lavfi -i testsrc2=size=SIZE:rate=25 \
  -frames:v 1 -c:v libx264 -profile:v baseline -level:v 3.1 \
  -preset veryfast -tune zerolatency -x264-params keyint=25:repeat-headers=1 \
  -f h264 OUTPUT
```

The Simulcast tests use real Pion RTP transport, selected WHEP H.264 media,
and canonical fMP4 demuxing. When the `ffmpeg` executable is available,
the canonical fMP4 is additionally decoded through FFmpeg. That optional
external decoding assertion does not change the server's build dependencies.
