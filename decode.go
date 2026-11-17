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
)

// A Decoder is a MP3-decoded stream.
//
// Decoder decodes its underlying source on the fly.
type Decoder struct {
	source          *source
	sampleRate      int
	length          int64
	frameStarts     []int64
	buf             []byte
	frame           *frame.Frame
	frameStart      int64
	pos             int64
	bytesPerFrame   int64
	firstFrameStart int64
	firstHeader     frameheader.FrameHeader
	xing            *xingHeader
}

func (d *Decoder) readFrame() error {
	var err error
	d.frame, d.frameStart, err = frame.Read(d.source, d.source.pos, d.frame)
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
// Seek returns an error when the resulting position is negative or, when the
// total length is known, beyond the end of the stream.
//
// When the decoder was created with NewFastSeekDecoder and the stream has a
// usable Xing/Info TOC, Seek approximates the target frame with the TOC and
// decodes forward from there instead of scanning the whole file. If the TOC
// turns out to be unusable, Seek falls back to the exact (scanning) behavior.
//
// Note that seek uses a byte offset but samples are aligned to 4 bytes (2
// channels, 2 bytes each). Be careful to seek to an offset that is divisible by
// 4 if you want to read at full sample boundaries.
func (d *Decoder) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekCurrent {
		// Handle the special case of asking for the current position specially.
		return d.pos, nil
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
	if npos < 0 {
		return 0, fmt.Errorf("mp3: negative position: %d", npos)
	}

	if d.xing != nil {
		if npos > d.length {
			return 0, fmt.Errorf("mp3: position %d exceeds the stream length %d", npos, d.length)
		}
		if err := d.seekWithXing(npos); err == nil {
			return npos, nil
		}
		// The Xing/Info TOC was not usable. Disable the fast path and fall
		// back to the exact seek by scanning the whole stream once.
		d.xing = nil
		d.length = invalidLength
		d.frameStarts = nil
		if err := d.ensureFrameStartsAndLength(); err != nil {
			return 0, err
		}
	}
	if d.length != invalidLength && npos > d.length {
		return 0, fmt.Errorf("mp3: position %d exceeds the stream length %d", npos, d.length)
	}
	return d.seekExact(npos)
}

// seekExact positions the decoder at the given PCM byte position by using the
// frame positions found by scanning the whole stream.
func (d *Decoder) seekExact(npos int64) (int64, error) {
	d.pos = npos
	d.buf = nil
	d.frame = nil
	if len(d.frameStarts) == 0 {
		// The frame positions are unknown e.g. because the source is
		// not an io.Seeker.
		if _, err := d.source.Seek(0, io.SeekStart); err != nil {
			return 0, err
		}
		return 0, errors.New("mp3: cannot seek without frame positions")
	}
	f := d.pos / d.bytesPerFrame
	if f >= int64(len(d.frameStarts)) {
		// The position is at the end of the stream. Position the source so
		// that the next Read reports io.EOF.
		if _, err := d.source.Seek(d.frameStarts[len(d.frameStarts)-1], 0); err != nil {
			return 0, err
		}
		if err := d.readFrame(); err != nil {
			return 0, err
		}
		d.buf = nil
		return npos, nil
	}
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
//
// When the decoder was created with NewFastSeekDecoder and the stream has a
// Xing/Info header, the length is derived from the header's frame count
// without scanning the whole source.
func (d *Decoder) Length() int64 {
	return d.length
}

// NewDecoder decodes the given io.Reader and returns a decoded stream.
//
// The stream is always formatted as 16bit (little endian) 2 channels
// even if the source is single channel MP3.
// Thus, a sample always consists of 4 bytes.
//
// When the source is an io.Seeker, NewDecoder scans the whole source once to
// determine the frame positions for Seek and Length. Use NewFastSeekDecoder
// to avoid this scan for streams with a Xing/Info header.
func NewDecoder(r io.Reader) (*Decoder, error) {
	return newDecoder(r, false)
}

// NewFastSeekDecoder is like NewDecoder, but when the source is an io.Seeker
// and the stream's first frame carries a Xing/Info header with a valid TOC,
// it does not scan the whole source: Length is derived from the header and
// Seek approximates the target frame via the TOC and then decodes forward to
// the exact requested position. If the header is missing or unusable, the
// decoder deterministically falls back to NewDecoder's behavior.
func NewFastSeekDecoder(r io.Reader) (*Decoder, error) {
	return newDecoder(r, true)
}

func newDecoder(r io.Reader, fastSeek bool) (*Decoder, error) {
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

	if fastSeek {
		d.firstFrameStart = d.frameStart
		d.firstHeader = d.frame.Header()
		if _, ok := r.(io.Seeker); ok {
			if x := d.readXingHeader(); x != nil {
				d.xing = x
				d.bytesPerFrame = int64(d.firstHeader.BytesPerFrame())
				d.length = x.frames * d.bytesPerFrame
				return d, nil
			}
		}
	}

	if err := d.ensureFrameStartsAndLength(); err != nil {
		return nil, err
	}

	return d, nil
}
