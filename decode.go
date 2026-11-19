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

// A CRCError is reported when a CRC-protected frame fails CRC-16
// verification. Use errors.As to inspect an error returned by Read or
// NewDecoderWithOptions.
type CRCError = consts.CRCError

// Options configures optional behaviors of a Decoder.
type Options struct {
	// VerifyCRC enables CRC-16 verification of frames that carry CRC
	// protection (i.e. frames with the protection bit cleared in their
	// headers). Frames without CRC protection are decoded as usual and
	// never reported.
	VerifyCRC bool

	// SkipBadFrames controls what happens when a frame fails the CRC
	// verification. If false, Read returns a *CRCError. If true, the
	// corrupt frame is replaced by one frame of silence so that the PCM
	// time positions of the following frames are preserved, and decoding
	// continues.
	SkipBadFrames bool

	// MaxConsecutiveBadFrames bounds how many consecutive corrupt frames
	// may be skipped before Read returns an error. This keeps recovery
	// bounded for streams that are garbage rather than MP3. A value <= 0
	// means the default of 8. It has no effect when SkipBadFrames is
	// false.
	MaxConsecutiveBadFrames int
}

// defaultMaxConsecutiveBadFrames is the default bound of consecutive
// corrupt frames skipped in SkipBadFrames mode.
const defaultMaxConsecutiveBadFrames = 8

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

	opts                 Options
	consecutiveBadFrames int
}

func (d *Decoder) readFrame() error {
	var err error
	d.frame, _, err = frame.Read(d.source, d.source.pos, d.frame, d.opts.VerifyCRC)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		if _, ok := err.(*consts.UnexpectedEOF); ok {
			// TODO: Log here?
			return io.EOF
		}
		var crcErr *consts.CRCError
		if errors.As(err, &crcErr) {
			return d.handleBadFrame(crcErr)
		}
		return err
	}
	d.consecutiveBadFrames = 0
	d.buf = append(d.buf, d.frame.Decode()...)
	return nil
}

// handleBadFrame handles a frame that failed the CRC verification. The
// stream is already positioned at the next frame.
func (d *Decoder) handleBadFrame(crcErr *consts.CRCError) error {
	if !d.opts.SkipBadFrames {
		return crcErr
	}
	d.consecutiveBadFrames++
	if d.consecutiveBadFrames > d.maxConsecutiveBadFrames() {
		return fmt.Errorf("mp3: more than %d consecutive corrupt frames, last error: %w",
			d.maxConsecutiveBadFrames(), crcErr)
	}
	// d.frame is nil here, so the corrupt frame's main data is not reused
	// through the bit reservoir by the next frame.
	//
	// Replace the frame with silence so that the PCM time positions of
	// the following frames stay unchanged.
	d.buf = append(d.buf, make([]byte, crcErr.BytesPerFrame)...)
	return nil
}

func (d *Decoder) maxConsecutiveBadFrames() int {
	if d.opts.MaxConsecutiveBadFrames > 0 {
		return d.opts.MaxConsecutiveBadFrames
	}
	return defaultMaxConsecutiveBadFrames
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
	d.consecutiveBadFrames = 0
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
		// In SkipBadFrames mode a skipped frame might have contributed a
		// different number of bytes than bytesPerFrame (e.g. for VBR
		// streams), so clamp the cut to the buffer size.
		cut := d.bytesPerFrame + (d.pos % d.bytesPerFrame)
		if cut > int64(len(d.buf)) {
			cut = int64(len(d.buf))
		}
		d.buf = d.buf[cut:]
	} else {
		if _, err := d.source.Seek(d.frameStarts[f], 0); err != nil {
			return 0, err
		}
		if err := d.readFrame(); err != nil {
			return 0, err
		}
		cut := d.pos
		if cut > int64(len(d.buf)) {
			cut = int64(len(d.buf))
		}
		d.buf = d.buf[cut:]
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

// NewDecoder decodes the given io.Reader and returns a decoded stream.
//
// The stream is always formatted as 16bit (little endian) 2 channels
// even if the source is single channel MP3.
// Thus, a sample always consists of 4 bytes.
func NewDecoder(r io.Reader) (*Decoder, error) {
	return NewDecoderWithOptions(r, nil)
}

// NewDecoderWithOptions is like NewDecoder but accepts optional behaviors
// such as CRC verification of protected frames. A nil o is equivalent to
// the zero Options, which keeps the historical behavior: CRCs are not
// verified.
func NewDecoderWithOptions(r io.Reader, o *Options) (*Decoder, error) {
	s := &source{
		reader: r,
	}
	d := &Decoder{
		source: s,
		length: invalidLength,
	}
	if o != nil {
		d.opts = *o
	}

	if err := s.skipTags(); err != nil {
		return nil, err
	}
	// Read frames until the first valid one. In SkipBadFrames mode the
	// first frames can be corrupt; the number of frames skipped here is
	// bounded by MaxConsecutiveBadFrames.
	for d.frame == nil {
		if err := d.readFrame(); err != nil {
			return nil, err
		}
	}
	freq, err := d.frame.SamplingFrequency()
	if err != nil {
		return nil, err
	}
	d.sampleRate = freq

	if err := d.ensureFrameStartsAndLength(); err != nil {
		return nil, err
	}

	return d, nil
}
