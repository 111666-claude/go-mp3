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
	"io"
	"math"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frame"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
	"github.com/hajimehoshi/go-mp3/internal/gapless"
)

// A Decoder is a MP3-decoded stream.
//
// Decoder decodes its underlying source on the fly.
type Decoder struct {
	source      *source
	sampleRate  int
	frameStarts []int64
	buf         []byte
	frame       *frame.Frame
	// bytesPerFrame is the number of decoded PCM bytes one frame produces.
	bytesPerFrame int64

	// rawLength is the total decoded PCM size without any gapless trimming,
	// or invalidLength when the source is not seekable.
	rawLength int64
	// length is the PCM size visible to callers: rawLength minus trims.
	length int64

	// Gapless trimming, in decoded PCM bytes (4 bytes per stereo sample).
	// trimStart samples are skipped at the beginning and trimEnd samples are
	// never exposed at the end. Both stay zero when gapless decoding is off
	// or no usable Xing/LAME metadata was found.
	trimStart int64
	trimEnd   int64

	// rawPos is the read position in the untrimmed PCM stream. pos is the
	// position visible to callers: pos == rawPos - trimStart.
	rawPos int64
	pos    int64
}

// ErrGaplessNotSeekable is returned when gapless decoding is requested for a
// source that does not implement io.Seeker. The Xing/LAME metadata lives in
// the first compressed frame, which has to be re-read after the frame was
// decoded, so gapless trimming cannot work on forward-only streams.
var ErrGaplessNotSeekable = errors.New("mp3: gapless decoding requires an io.Seeker source")

// bytesPerSample is the size of one decoded stereo sample: 16-bit little
// endian, 2 channels (the decoder duplicates mono into both channels).
const bytesPerSample = 4

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
	// End of the exposed PCM stream. Without a known length (non-seekable
	// sources) the decoder simply runs until the underlying stream ends.
	limit := int64(math.MaxInt64)
	if d.rawLength != invalidLength {
		limit = d.rawLength - d.trimEnd
	}

	n := 0
	for n < len(buf) {
		if d.rawPos >= limit {
			if n > 0 {
				return n, nil
			}
			return 0, io.EOF
		}
		for len(d.buf) == 0 {
			if err := d.readFrame(); err != nil {
				if n > 0 {
					return n, nil
				}
				return 0, err
			}
		}
		c := copy(buf[n:], d.buf)
		// Never expose the end-padding samples.
		if d.rawPos+int64(c) > limit {
			c = int(limit - d.rawPos)
		}
		d.buf = d.buf[c:]
		n += c
		d.rawPos += int64(c)
		d.pos += int64(c)
	}
	return n, nil
}

// seekRaw positions the decoder at rawPos in the untrimmed PCM stream and
// refills d.buf so that its first byte corresponds to that position.
func (d *Decoder) seekRaw(rawPos int64) error {
	d.buf = nil
	d.frame = nil
	f := rawPos / d.bytesPerFrame
	// If the frame is not first, read the previous ahead of reading that
	// because the previous frame can affect the targeted frame.
	if f > 0 {
		f--
		if _, err := d.source.Seek(d.frameStarts[f], 0); err != nil {
			return err
		}
		if err := d.readFrame(); err != nil {
			return err
		}
		if err := d.readFrame(); err != nil {
			return err
		}
		d.buf = d.buf[d.bytesPerFrame+(rawPos%d.bytesPerFrame):]
	} else {
		if _, err := d.source.Seek(d.frameStarts[f], 0); err != nil {
			return err
		}
		if err := d.readFrame(); err != nil {
			return err
		}
		d.buf = d.buf[rawPos:]
	}
	d.rawPos = rawPos
	return nil
}

// Seek is io.Seeker's Seek.
//
// Seek returns an error when the underlying source is not io.Seeker.
//
// The offset always refers to the trimmed PCM coordinates that Read and
// Length use, so SeekStart(0) lands on the first sample after the encoder
// delay and SeekEnd(0) on the sample before the end padding.
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
	if npos < 0 || (d.length != invalidLength && npos > d.length) {
		return 0, errors.New("mp3: seek position out of range")
	}
	// Translate the visible (trimmed) position into the raw PCM position.
	if err := d.seekRaw(npos + d.trimStart); err != nil {
		return 0, err
	}
	d.pos = npos
	return npos, nil
}

// SampleRate returns the sample rate like 44100.
//
// Note that the sample rate is retrieved from the first frame.
func (d *Decoder) SampleRate() int {
	return d.sampleRate
}

func (d *Decoder) ensureFrameStartsAndLength() error {
	if d.rawLength != invalidLength {
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
		h, p, err := frameheader.Read(d.source, d.source.pos)
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
		d.frameStarts = append(d.frameStarts, p)
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
	d.rawLength = l
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
// With gapless decoding enabled, the encoder delay and end padding are
// subtracted and the reported length matches what Read can actually return.
func (d *Decoder) Length() int64 {
	return d.length
}

// DecoderOptions configures optional decoder behavior.
type DecoderOptions struct {
	// Gapless enables trimming of the encoder delay and end padding stored
	// in the Xing/Info and LAME headers of the first frame. When the source
	// carries no such headers, or the headers are unusable, the decoder
	// behaves exactly as if the option were off.
	//
	// Gapless requires the source to implement io.Seeker; otherwise
	// NewDecoderWithOptions returns ErrGaplessNotSeekable. Delay or padding
	// values that would trim more audio than the stream contains are
	// ignored and the untrimmed stream is exposed instead.
	Gapless bool
}

// NewDecoder decodes the given io.Reader and returns a decoded stream.
//
// The stream is always formatted as 16bit (little endian) 2 channels
// even if the source is single channel MP3.
// Thus, a sample always consists of 4 bytes.
//
// NewDecoder is equivalent to NewDecoderWithOptions with zero options.
func NewDecoder(r io.Reader) (*Decoder, error) {
	return NewDecoderWithOptions(r, DecoderOptions{})
}

// NewDecoderWithOptions decodes the given io.Reader like NewDecoder, with the
// given options applied.
func NewDecoderWithOptions(r io.Reader, opts DecoderOptions) (*Decoder, error) {
	if opts.Gapless {
		if _, ok := r.(io.Seeker); !ok {
			return nil, ErrGaplessNotSeekable
		}
	}

	s := &source{
		reader: r,
	}
	d := &Decoder{
		source:    s,
		rawLength: invalidLength,
		length:    invalidLength,
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

	if opts.Gapless {
		if err := d.applyGapless(); err != nil {
			return nil, err
		}
	}

	return d, nil
}

// applyGapless reads the Xing/LAME metadata out of the first compressed frame
// and configures the trimming offsets. Any failure to find usable metadata
// leaves the decoder in the untrimmed state instead of failing construction.
func (d *Decoder) applyGapless() error {
	if d.rawLength == invalidLength || len(d.frameStarts) == 0 {
		return ErrGaplessNotSeekable
	}
	firstStart := d.frameStarts[0]
	if _, err := d.source.Seek(firstStart, io.SeekStart); err != nil {
		return err
	}

	// Re-parse the first frame header to learn its compressed size.
	headerBuf := make([]byte, 4)
	if _, err := d.source.ReadFull(headerBuf); err != nil {
		return d.finishGapless(firstStart, 0, 0, false, err)
	}
	h := frameheader.FrameHeader(uint32(headerBuf[0])<<24 |
		uint32(headerBuf[1])<<16 | uint32(headerBuf[2])<<8 |
		uint32(headerBuf[3]))
	frameSize, err := h.FrameSize()
	if err != nil {
		return d.finishGapless(firstStart, 0, 0, false, err)
	}
	raw := make([]byte, frameSize)
	copy(raw, headerBuf)
	if _, err := d.source.ReadFull(raw[4:]); err != nil {
		// A truncated first frame cannot hold a complete LAME tag. Keep the
		// untrimmed behavior rather than rejecting the whole stream.
		return d.finishGapless(firstStart, 0, 0, false, nil)
	}

	info, ok, parseErr := gapless.Parse(raw)
	if parseErr != nil || !ok {
		// No recognizable gapless metadata: stay compatible with plain MP3s.
		return d.finishGapless(firstStart, 0, 0, false, nil)
	}

	trimStart := int64(info.Delay) * 4

	// The encoder's declared sample span is based on the Xing/Info frame
	// count, which can be smaller than the number of frames physically
	// present (some encoders append a padding-only final frame). Prefer that
	// declared span; fall back to the measured decode when the header omits a
	// frame count or reports one larger than the actual stream.
	declaredSamples := int64(-1)
	if info.XingFrames > 0 {
		declaredSamples = int64(info.XingFrames) * d.bytesPerFrame / bytesPerSample
	}
	var validEnd int64
	switch {
	case declaredSamples > 0 && declaredSamples*bytesPerSample <= d.rawLength:
		validEnd = declaredSamples * bytesPerSample
	default:
		validEnd = d.rawLength
	}
	trimEnd := d.rawLength - (validEnd - int64(info.Padding)*4)
	if trimEnd < 0 {
		trimEnd = 0
	}

	if trimStart >= validEnd-int64(info.Padding)*4 {
		// The metadata would trim away every real sample. Ignore it instead
		// of producing an empty or inverted stream.
		return d.finishGapless(firstStart, 0, 0, false, nil)
	}
	return d.finishGapless(firstStart, trimStart, trimEnd, true, nil)
}

// finishGapless puts the decoder back into a consistent streaming state after
// the first frame was re-read for metadata. When trim is true the full decode
// pipeline is re-seeded at trimStart (replaying the previous frame so the bit
// reservoir and overlap-add history are correct); otherwise the decoder is
// returned to the untrimmed state it had before metadata inspection.
func (d *Decoder) finishGapless(firstStart, trimStart, trimEnd int64, trim bool, earlier error) error {
	if earlier != nil {
		return earlier
	}
	if !trim {
		// Restore the source to where frame 1 begins (frame 0 was already
		// decoded during construction) and keep the existing decode state.
		restore := firstStart
		if len(d.frameStarts) > 1 {
			restore = d.frameStarts[1]
		}
		if _, err := d.source.Seek(restore, io.SeekStart); err != nil {
			return err
		}
		d.trimStart, d.trimEnd = 0, 0
		d.length = d.rawLength
		d.rawPos, d.pos = 0, 0
		return nil
	}

	d.trimStart = trimStart
	d.trimEnd = trimEnd
	d.length = d.rawLength - trimStart - trimEnd
	// Rebuild the decode pipeline from scratch at the first raw sample of
	// the trimmed region. seekRaw replays frame 0 (and the preceding frame
	// when needed), so reservoir and overlap state match a fresh decode.
	if err := d.seekRaw(trimStart); err != nil {
		return err
	}
	d.pos = 0
	return nil
}
