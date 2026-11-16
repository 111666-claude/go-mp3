# This project is no longer maintained.

# go-mp3

[![Go Reference](https://pkg.go.dev/badge/github.com/hajimehoshi/go-mp3.svg)](https://pkg.go.dev/github.com/hajimehoshi/go-mp3) 

An MP3 decoder in pure Go based on [PDMP3](https://github.com/technosaurus/PDMP3).

## Chapters

Audio books can describe their chapters in the ID3v2 tag with `CHAP` and
`CTOC` frames. `Decoder.Chapters` returns them in playback order, and
`Decoder.SeekChapter` positions the decoder at a chapter:

```go
d, err := mp3.NewDecoder(f)
if err != nil {
    return err
}
chapters, err := d.Chapters()
if err != nil {
    return err
}
for i, c := range chapters {
    fmt.Printf("%d: %s (%d-%d ms)\n", i, c.Title, c.StartMS, c.EndMS)
}
if _, err := d.SeekChapter(2); err != nil {
    return err
}
// Reading now starts at chapter 3's first sample.
```

A stream without chapter metadata reports no chapters. Broken chapter
metadata (truncated frames, duplicate IDs, cyclic tables of contents, inverted
times) is reported by `Chapters` as an error; the audio itself still decodes
normally.

[Slide at golang.tokyo #11](https://docs.google.com/presentation/d/e/2PACX-1vTTXf-LWNRvMVGQ7GI4Wh8EKohot_9CMtlF4dswpYGpuYKOek5NeNP-_QZnNcRFZp9Cwm0pCcykjqDN/pub?start=false&loop=false&delayms=3000)
