// Copyright 2017 Hajime Hoshi
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package mp3

import (
	"errors"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frame"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
	"github.com/hajimehoshi/go-mp3/internal/id3"
)

// A Decoder is a MP3-decoded stream.
//
// Decoder decodes its underlying source on the fly.
type Decoder struct {
	source        *source
	sampleRate    int
	length        int64
	frameStarts   []int64
	buf           []byte
	frame         *frame.Frame
	pos           int64
	bytesPerFrame int64
	chapters      []Chapter
	chapterErr    error
}

// A Chapter is an audio book chapter declared in the ID3v2 tag.
//
// StartMS and EndMS are the chapter boundaries in milliseconds, as stored in
// the CHAP frame. StartPCM and EndPCM are the same boundaries converted to
// offsets in the decoded PCM stream (16-bit little endian stereo, so a sample
// is 4 bytes), at the decoder's sample rate. StartPCM is always a multiple of
// 4 and can be passed to Seek.
type Chapter struct {
	// ID is the chapter's opaque Element ID. Titles are not required to be
	// unique, but IDs are.
	ID string
	// Title is the text of the chapter's TIT2 sub-frame, or "" if absent.
	Title string
	// StartMS and EndMS are milliseconds from the beginning of the audio.
	StartMS uint32
	EndMS   uint32
	// StartPCM and EndPCM are byte offsets in the decoded PCM stream. They
	// are derived from StartMS and EndMS at the decoder's sample rate, so they
	// can be passed directly to Seek. EndPCM never exceeds Length.
	StartPCM int64
	EndPCM   int64
}

// ErrNoSuchChapter is returned by SeekChapter when the chapter index is out
// of range.
var ErrNoSuchChapter = errors.New("mp3: no such chapter")

// ErrChapterOutOfRange is returned when a chapter's declared start or end
// time lies beyond the end of the decoded audio.
var ErrChapterOutOfRange = id3.ErrChapterTimeOutOfRange

func (d *Decoder) readFrame() error {
	var err error
	d.frame, _, err = frame.Read(d.source, d.source.pos, d.frame)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		if _, ok := err.(*consts.UnexpectedEOF); ok {
			// TODO: Log here?
			return io.EOF
		}
		return err
	}
	d.buf = append(d.buf, d.frame.Decode()...)
	return nil
}

// Read is io.Reader's Read.
func (d *Decoder) Read(buf []byte) (int, error) {
	for len(d.buf) == 0 {
		if err := d.readFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(buf, d.buf)
	d.buf = d.buf[n:]
	d.pos += int64(n)
	return n, nil
}

// Seek is io.Seeker's Seek.
//
// Seek returns an error when the underlying source is not io.Seeker.
//
// Note that seek uses a byte offset but samples are aligned to 4 bytes (2
// channels, 2 bytes each). Be careful to seek to an offset that is divisible by
// 4 if you want to read at full sample boundaries.
func (d *Decoder) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekCurrent {
		// Handle the special case of asking for the current position specially.
		return d.pos, nil
	}
	if _, ok := d.source.reader.(io.Seeker); !ok {
		return 0, errors.New("mp3: source must be io.Seeker")
	}

	npos := int64(0)
	switch whence {
	case io.SeekStart:
		npos = offset
	case io.SeekCurrent:
		npos = d.pos + offset
	case io.SeekEnd:
		npos = d.Length() + offset
	default:
		return 0, errors.New("mp3: invalid whence")
	}
	d.pos = npos
	d.buf = nil
	d.frame = nil
	f := d.pos / d.bytesPerFrame
	// If the frame is not first, read the previous ahead of reading that
	// because the previous frame can affect the targeted frame.
	if f > 0 {
		f--
		if _, err := d.source.Seek(d.frameStarts[f], 0); err != nil {
			return 0, err
		}
		if err := d.readFrame(); err != nil {
			return 0, err
		}
		if err := d.readFrame(); err != nil {
			return 0, err
		}
		d.buf = d.buf[d.bytesPerFrame+(d.pos%d.bytesPerFrame):]
	} else {
		if _, err := d.source.Seek(d.frameStarts[f], 0); err != nil {
			return 0, err
		}
		if err := d.readFrame(); err != nil {
			return 0, err
		}
		d.buf = d.buf[d.pos:]
	}
	return npos, nil
}

// SampleRate returns the sample rate like 44100.
//
// Note that the sample rate is retrieved from the first frame.
func (d *Decoder) SampleRate() int {
	return d.sampleRate
}

func (d *Decoder) ensureFrameStartsAndLength() error {
	if d.length != invalidLength {
		return nil
	}

	if _, ok := d.source.reader.(io.Seeker); !ok {
		return nil
	}

	// Keep the current position.
	pos, err := d.source.Seek(0, io.SeekCurrent)
	if err != nil {
		return err
	}
	if err := d.source.rewind(); err != nil {
		return err
	}

	if err := d.source.skipTags(); err != nil {
		return err
	}
	l := int64(0)
	for {
		h, pos, err := frameheader.Read(d.source, d.source.pos)
		if err != nil {
			if err == io.EOF {
				break
			}
			if _, ok := err.(*consts.UnexpectedEOF); ok {
				// TODO: Log here?
				break
			}
			return err
		}
		d.frameStarts = append(d.frameStarts, pos)
		d.bytesPerFrame = int64(h.BytesPerFrame())
		l += d.bytesPerFrame

		framesize, err := h.FrameSize()
		if err != nil {
			return err
		}
		buf := make([]byte, framesize-4)
		if _, err := d.source.ReadFull(buf); err != nil {
			if err == io.EOF {
				break
			}
			return err
		}
	}
	d.length = l

	if _, err := d.source.Seek(pos, io.SeekStart); err != nil {
		return err
	}
	return nil
}

const invalidLength = -1

// Length returns the total size in bytes.
//
// Length returns -1 when the total size is not available
// e.g. when the given source is not io.Seeker.
func (d *Decoder) Length() int64 {
	return d.length
}

// Chapters returns the chapters declared in the ID3v2 tag, in playback order.
//
// Chapters are ordered by the tag's CTOC frame when it has one; otherwise they
// are ordered by ascending start time. The returned slice is nil when the
// stream has no chapter information.
//
// Chapters returns an error when the chapter metadata is structurally invalid
// (truncated frames, duplicate Element IDs, a cyclic or dangling CTOC,
// inverted chapter times) or when a chapter starts beyond the decoded audio.
// Such tags do not stop the decoder from being constructed and the audio still
// decodes normally; the error is reported here so that callers that rely on
// chapter data never silently see a partial or invented list.
func (d *Decoder) Chapters() ([]Chapter, error) {
	if d.chapterErr != nil {
		return nil, d.chapterErr
	}
	if len(d.chapters) == 0 {
		return nil, nil
	}
	out := make([]Chapter, len(d.chapters))
	copy(out, d.chapters)
	return out, nil
}

// SeekChapter positions the decoder at the beginning of the given chapter.
//
// The chapters are numbered in the order Chapters returns them. SeekChapter
// accepts an index from 0 to len(Chapters())-1 and returns the resulting PCM
// position, which is also where the next Read starts.
func (d *Decoder) SeekChapter(i int) (int64, error) {
	chapters, err := d.Chapters()
	if err != nil {
		return 0, err
	}
	if i < 0 || i >= len(chapters) {
		return 0, fmt.Errorf("%w: %d (have %d)", ErrNoSuchChapter, i, len(chapters))
	}
	return d.Seek(chapters[i].StartPCM, io.SeekStart)
}

// NewDecoder decodes the given io.Reader and returns a decoded stream.
//
// The stream is always formatted as 16bit (little endian) 2 channels
// even if the source is single channel MP3.
// Thus, a sample always consists of 4 bytes.
func NewDecoder(r io.Reader) (*Decoder, error) {
	s := &source{
		reader: r,
	}
	d := &Decoder{
		source: s,
		length: invalidLength,
	}

	if err := s.skipTags(); err != nil {
		return nil, err
	}
	// TODO: Is readFrame here really needed?
	if err := d.readFrame(); err != nil {
		return nil, err
	}
	freq, err := d.frame.SamplingFrequency()
	if err != nil {
		return nil, err
	}
	d.sampleRate = freq

	if err := d.ensureFrameStartsAndLength(); err != nil {
		return nil, err
	}

	if err := d.readChapters(); err != nil {
		// Broken chapter metadata must not make otherwise playable audio
		// undecodable. Keep the error for Chapters and continue.
		d.chapterErr = err
	}

	return d, nil
}

// readChapters parses the ID3v2 tag captured by skipTags and converts the
// declared chapter times to PCM coordinates.
func (d *Decoder) readChapters() error {
	if len(d.source.id3Tag) == 0 {
		return nil
	}
	parsed, err := id3.ParseChapters(d.source.id3Tag)
	if err != nil {
		return err
	}
	if len(parsed) == 0 {
		return nil
	}
	// Times are in milliseconds at the decoder's sample rate. One stereo
	// sample is 4 bytes. Convert to whole samples so that StartPCM is always a
	// multiple of 4 (a sample boundary); sub-sample precision is meaningless
	// for a chapter boundary and would make Seek land mid-sample.
	pcm := func(ms uint32) int64 {
		return int64(ms) * int64(d.sampleRate) / 1000 * 4
	}
	chapters := make([]Chapter, 0, len(parsed))
	for _, c := range parsed {
		ch := Chapter{
			ID:       c.ID,
			Title:    c.Title,
			StartMS:  c.StartMS,
			EndMS:    c.EndMS,
			StartPCM: pcm(c.StartMS),
			EndPCM:   pcm(c.EndMS),
		}
		if d.length != invalidLength {
			audioMS := d.length / 4 * 1000 / int64(d.sampleRate)
			if ch.StartPCM > d.length {
				return fmt.Errorf("%w: chapter %q starts at %d ms, audio is %d ms",
					id3.ErrChapterTimeOutOfRange, c.ID, c.StartMS, audioMS)
			}
			// The last chapter commonly ends at, or a hair past, the duration
			// the encoder measured. Clamp such an end to the audio actually
			// present; a start beyond the audio cannot be clamped.
			if ch.EndPCM > d.length {
				if ch.EndPCM-d.length > int64(d.bytesPerFrame) {
					return fmt.Errorf("%w: chapter %q ends at %d ms, audio is %d ms",
						id3.ErrChapterTimeOutOfRange, c.ID, c.EndMS, audioMS)
				}
				ch.EndPCM = d.length
			}
		}
		chapters = append(chapters, ch)
	}
	d.chapters = chapters
	return nil
}
