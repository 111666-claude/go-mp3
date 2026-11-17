# This project is no longer maintained.

# go-mp3

[![Go Reference](https://pkg.go.dev/badge/github.com/hajimehoshi/go-mp3.svg)](https://pkg.go.dev/github.com/hajimehoshi/go-mp3) 

An MP3 decoder in pure Go based on [PDMP3](https://github.com/technosaurus/PDMP3).

[Slide at golang.tokyo #11](https://docs.google.com/presentation/d/e/2PACX-1vTTXf-LWNRvMVGQ7GI4Wh8EKohot_9CMtlF4dswpYGpuYKOek5NeNP-_QZnNcRFZp9Cwm0pCcykjqDN/pub?start=false&loop=false&delayms=3000)

## Fast seeking with Xing/Info headers

`NewDecoder` scans the whole source once to determine the frame positions for
`Seek` and `Length`. For long files with a Xing/Info header, use
`NewFastSeekDecoder` instead: it reads the header's frame count, byte count
and TOC from the first frame, and `Seek` approximates the target frame via
the TOC and decodes forward to the exact position, without scanning the whole
file. If the header is missing or unusable (no TOC, a corrupt TOC, a
truncated stream, or frames with an inconsistent format), the decoder
deterministically falls back to the regular scanning behavior.

```go
d, err := mp3.NewFastSeekDecoder(f) // f must be an io.ReadSeeker
if err != nil {
    return err
}
// Seek and Length work as usual, but no full-file scan was needed.
if _, err := d.Seek(0, io.SeekEnd); err != nil {
    return err
}
```

