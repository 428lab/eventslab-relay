# testdata

- `vt_bad_idr_head.bin`: the first 64 bytes of a real IDR NAL unit (nal_unit_type 5) from Chrome's
  VideoToolbox encoder whose slice is a **P slice** (slice_type 0). Captured from a prototype recording
  (2026-11-23) taken before the relay dropped these. Decoders reject it ("A non-intra slice in an IDR NAL unit").
- `vt_good_idr_head.bin`: the first 64 bytes of a normal IDR from the same recording (I slice).
- `pattern.h264`: 2 s of a 320x180 test pattern, H.264 Main, no B-frames, Annex-B (ffmpeg `testsrc2` + `h264_videotoolbox`).
- `pattern.ogg`: 2 s of a 440 Hz tone, Opus in Ogg (ffmpeg `libopus`).
