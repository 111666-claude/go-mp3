# This project is no longer maintained.

# go-mp3

[![Go Reference](https://pkg.go.dev/badge/github.com/hajimehoshi/go-mp3.svg)](https://pkg.go.dev/github.com/hajimehoshi/go-mp3) 

An MP3 decoder in pure Go based on [PDMP3](https://github.com/technosaurus/PDMP3).

[Slide at golang.tokyo #11](https://docs.google.com/presentation/d/e/2PACX-1vTTXf-LWNRvMVGQ7GI4Wh8EKohot_9CMtlF4dswpYGpuYKOek5NeNP-_QZnNcRFZp9Cwm0pCcykjqDN/pub?start=false&loop=false&delayms=3000)

## CRC verification and error recovery

Some MP3 frames carry a CRC-16 checksum (frames with the protection bit
cleared in their headers). `NewDecoderWithOptions` can verify these
checksums and lets the caller choose how to react to corrupt frames:

```go
d, err := mp3.NewDecoderWithOptions(r, &mp3.Options{
    // Verify CRC-16 of protected frames. Frames without CRC protection
    // are decoded as usual and never reported.
    VerifyCRC: true,
    // If false (default), Read returns a *mp3.CRCError (use errors.As)
    // reporting the frame offset and the expected and actual CRC values.
    // If true, a corrupt frame is replaced by one frame of silence so
    // that the PCM time positions of the following frames are preserved,
    // and decoding continues.
    SkipBadFrames: true,
    // Bound of consecutive corrupt frames to skip before giving up with
    // an error. Zero means the default of 8.
    MaxConsecutiveBadFrames: 8,
})
```

In strict mode (`SkipBadFrames: false`) the corrupt frame is dropped and
the stream is left positioned at the next frame, so the caller can keep
reading after inspecting the error. Recovery works on any `io.Reader`;
no seeking is required. `NewDecoder` keeps the historical behavior and
does not verify CRCs.
