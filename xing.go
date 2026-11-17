// Copyright 2026 The go-mp3 Authors
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
	"encoding/binary"
	"errors"
	"io"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
)

// Flags in a Xing/Info header.
const (
	xingFlagFrames = 0x1
	xingFlagBytes  = 0x2
	xingFlagTOC    = 0x4
)

// A xingHeader is a parsed Xing/Info header stored in the first frame of a
// stream. The frame count covers all the frames in the stream including the
// first (Xing/Info) frame, and the byte count covers the stream from the
// first frame to the end.
type xingHeader struct {
	frames int64
	bytes  int64
	toc    [100]byte
}

// readXingHeader tries to read a Xing/Info header from the first frame of the
// stream. It returns nil when the first frame has no usable Xing/Info header.
// The source position is restored before returning.
func (d *Decoder) readXingHeader() *xingHeader {
	pos, err := d.source.Seek(0, io.SeekCurrent)
	if err != nil {
		return nil
	}
	defer func() {
		d.source.Seek(pos, io.SeekStart)
	}()

	// The Xing/Info header is stored right after the side information of
	// the first frame.
	h := d.firstHeader
	off := d.firstFrameStart + 4 + int64(h.SideInfoSize())
	if h.ProtectionBit() == 0 {
		off += 2
	}
	if _, err := d.source.Seek(off, io.SeekStart); err != nil {
		return nil
	}

	buf := make([]byte, 4)
	if _, err := d.source.ReadFull(buf); err != nil {
		return nil
	}
	if string(buf) != "Xing" && string(buf) != "Info" {
		return nil
	}

	if _, err := d.source.ReadFull(buf); err != nil {
		return nil
	}
	flags := binary.BigEndian.Uint32(buf)
	if flags&(xingFlagFrames|xingFlagBytes|xingFlagTOC) !=
		xingFlagFrames|xingFlagBytes|xingFlagTOC {
		return nil
	}

	buf = make([]byte, 4+4+100)
	if _, err := d.source.ReadFull(buf); err != nil {
		return nil
	}
	frames := binary.BigEndian.Uint32(buf[0:4])
	bytes := binary.BigEndian.Uint32(buf[4:8])
	if frames == 0 || bytes == 0 {
		return nil
	}
	x := &xingHeader{
		frames: int64(frames),
		bytes:  int64(bytes),
	}
	copy(x.toc[:], buf[8:])

	// The TOC must be non-decreasing and non-trivial. Reject it otherwise
	// so that the caller falls back to scanning the stream.
	for i := 0; i < 99; i++ {
		if x.toc[i] > x.toc[i+1] {
			return nil
		}
	}
	if x.toc[99] == 0 {
		return nil
	}
	return x
}

// errInconsistentFrame is returned when a frame's format (version, layer,
// sampling frequency or number of channels) differs from the first frame's.
// A stream whose frames change format midway cannot be sought with the
// Xing/Info TOC.
var errInconsistentFrame = errors.New("mp3: frame header is inconsistent with the first frame")

// readFrameChecked reads the next frame like readFrame, but checks the
// frame's header against the first frame's format before decoding it. This
// prevents decoding frames whose side information would be misinterpreted.
func (d *Decoder) readFrameChecked() error {
	h, startPos, err := frameheader.Read(d.source, d.source.pos)
	if err != nil {
		if _, ok := err.(*consts.UnexpectedEOF); ok {
			return io.EOF
		}
		return err
	}
	fh := d.firstHeader
	if h.ID() != fh.ID() ||
		h.Layer() != fh.Layer() ||
		h.SamplingFrequency() != fh.SamplingFrequency() ||
		h.NumberOfChannels() != fh.NumberOfChannels() {
		return errInconsistentFrame
	}
	// Rewind to the frame's start and read the frame.
	if _, err := d.source.Seek(startPos, io.SeekStart); err != nil {
		return err
	}
	return d.readFrame()
}

// seekWithXing positions the decoder at the given PCM byte position by using
// the Xing/Info TOC. The TOC gives only an approximate byte offset, so the
// decoder synchronizes to a frame near the candidate offset and decodes
// forward until the target frame. The last two frames are decoded with a cold
// bit reservoir, exactly as seekExact does, so the resulting PCM samples and
// the reported position are consistent with Seek's semantics.
//
// seekWithXing returns an error whenever the TOC turns out to be unusable
// (a truncated stream, a frame that cannot be synchronized, an inconsistent
// frame format, or a TOC that does not lead to the target). The caller then
// falls back to the exact seek.
func (d *Decoder) seekWithXing(pos int64) error {
	x := d.xing
	d.buf = nil
	d.frame = nil

	f := pos / d.bytesPerFrame
	if f >= x.frames {
		// The position is at the end of the stream. Position the source so
		// that the next Read reports io.EOF.
		if _, err := d.source.Seek(d.firstFrameStart+x.bytes, io.SeekStart); err != nil {
			return err
		}
		d.pos = pos
		return nil
	}

	// Find the TOC bucket for the target frame. Back off by one bucket if
	// needed so that the frame estimated for the bucket leaves room for the
	// two frames decoded with a cold reservoir before the target frame.
	i := f * 100 / x.frames
	if i > 99 {
		i = 99
	}
	for i > 0 && i*x.frames/100 > f-2 {
		i--
	}
	est := i * x.frames / 100

	candidate := d.firstFrameStart + int64(x.toc[i])*x.bytes/256
	if _, err := d.source.Seek(candidate, io.SeekStart); err != nil {
		return err
	}

	// Decode the frames up to the target frame. The decoded bytes of these
	// frames are discarded; only the reservoir state matters.
	startCold := f - 1
	if startCold < est {
		startCold = est
	}
	limit := 2*(x.frames/100) + 64
	first := true
	for cur := est; cur < startCold; cur++ {
		if err := d.readFrameChecked(); err != nil {
			return err
		}
		if first {
			first = false
			// The landed frame must be close to the candidate offset.
			// Allow the TOC's 1/256 resolution plus some slack.
			if d.frameStart < candidate ||
				d.frameStart > candidate+x.bytes/256+8192 {
				return errors.New("mp3: Xing/Info TOC points outside of the stream")
			}
		}
		d.buf = nil
		if cur-est > limit {
			return errors.New("mp3: Xing/Info TOC did not lead to the target frame")
		}
	}

	// Decode the previous and the target frames with a cold reservoir, as
	// seekExact does.
	d.frame = nil
	d.buf = nil
	for cur := startCold; cur <= f; cur++ {
		if err := d.readFrameChecked(); err != nil {
			return err
		}
	}
	d.buf = d.buf[(f-startCold)*d.bytesPerFrame+pos%d.bytesPerFrame:]
	d.pos = pos
	return nil
}
