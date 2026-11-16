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

// Package gapless parses the encoder delay and end padding stored in the
// Xing/Info and LAME headers of the first MP3 audio frame.
//
// The values describe how many samples per channel the encoder added at the
// beginning (delay) and at the end (padding) of the stream. Decoders can skip
// them to reproduce the original PCM length, which makes looped playback
// seamless. See the LAME header documentation:
// https://wiki.hydrogenaud.io/index.php?title=LAME#VBRI.2C_Xing.2FInfo_and_LAME_tag
package gapless

import (
	"errors"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
)

// Info is the gapless metadata found in the first audio frame.
//
// Delay and Padding are numbers of PCM samples per channel. The decoder
// always outputs stereo frames (4 bytes per sample), so the byte offsets are
// Delay*4 and Padding*4.
//
// XingFrames is the number of audio frames reported by the Xing/Info header
// (including the header frame itself). The encoder's declared total sample
// count is XingFrames times the per-frame sample count; this can be smaller
// than the number of frames physically present because some encoders append
// a padding-only final frame. A value of 0 means the header did not carry a
// frame count.
type Info struct {
	Delay      int
	Padding    int
	XingFrames int
}

var (
	// errTruncated means the frame claims to hold a Xing/LAME tag but does
	// not contain enough bytes for one. Callers treat this as "no gapless
	// information" rather than a fatal decode error.
	errTruncated = errors.New("mp3: truncated Xing/LAME header")
	errNoTag     = errors.New("mp3: no Xing/LAME gapless header")
)

// sideInfoSize returns the size of the side information block that sits
// between the frame header and the Xing/Info tag.
func sideInfoSize(h frameheader.FrameHeader) int {
	mono := h.Mode() == consts.ModeSingleChannel
	if h.LowSamplingFrequency() == 1 {
		if mono {
			return 9
		}
		return 17
	}
	if mono {
		return 17
	}
	return 32
}

// Parse looks for a Xing/Info header followed by a LAME tag in frame, which
// must be the complete bytes of the first audio frame as found at frameStart.
//
// Parse never turns ordinary audio into a gapless result: a valid Xing/Info
// magic and a recognized LAME-compatible encoder string are both required.
// A missing header, an unrecognized encoder, a truncated tag, or zero
// gapless values make Parse report ok == false with a nil error.
func Parse(frame []byte) (info Info, ok bool, err error) {
	if len(frame) < 4 {
		return Info{}, false, errTruncated
	}
	h := frameheader.FrameHeader(uint32(frame[0])<<24 | uint32(frame[1])<<16 |
		uint32(frame[2])<<8 | uint32(frame[3]))
	if !h.IsValid() {
		return Info{}, false, errNoTag
	}
	frameSize, err := h.FrameSize()
	if err != nil {
		return Info{}, false, err
	}
	if len(frame) < frameSize {
		return Info{}, false, errTruncated
	}

	xingOff := 4
	if h.ProtectionBit() == 0 {
		// A 2-byte CRC word follows the header when the protection bit is 0.
		xingOff += 2
	}
	xingOff += sideInfoSize(h)

	// The Xing/Info header begins with a 4-byte magic.
	if frameSize < xingOff+4 ||
		(string(frame[xingOff:xingOff+4]) != "Xing" &&
			string(frame[xingOff:xingOff+4]) != "Info") {
		return Info{}, false, errNoTag
	}
	if frameSize < xingOff+8 {
		return Info{}, false, errTruncated
	}
	flags := uint32(frame[xingOff+4])<<24 | uint32(frame[xingOff+5])<<16 |
		uint32(frame[xingOff+6])<<8 | uint32(frame[xingOff+7])
	p := xingOff + 8
	// Optional fields, each present when its flag bit is set:
	//   frames (4), bytes (4), TOC (100), VBR quality (4)
	var xingFrames int
	if flags&0x0001 != 0 {
		xingFrames = int(uint32(frame[p])<<24 | uint32(frame[p+1])<<16 |
			uint32(frame[p+2])<<8 | uint32(frame[p+3]))
		p += 4
	}
	if flags&0x0002 != 0 {
		p += 4
	}
	if flags&0x0004 != 0 {
		p += 100
	}
	if flags&0x0008 != 0 {
		p += 4
	}

	// The LAME tag starts here. Its first 9 bytes are the encoder string.
	// Only well-known LAME-compatible encoders are accepted so that plain
	// audio frames carrying an unrelated Xing magic cannot be mistaken for
	// gapless data.
	if frameSize < p+9 {
		return Info{}, false, errTruncated
	}
	enc := string(frame[p : p+9])
	if !hasLAMEEncoderPrefix(enc) {
		return Info{}, false, errNoTag
	}

	// Encoder delay (12 bits) and padding (12 bits) are packed into three
	// bytes at offset 21..23 of the LAME tag.
	if frameSize < p+24 {
		return Info{}, false, errTruncated
	}
	b0 := frame[p+21]
	b1 := frame[p+22]
	b2 := frame[p+23]
	delay := int(b0)<<4 | int(b1>>4)
	padding := int(b1&0x0f)<<8 | int(b2)
	if delay == 0 && padding == 0 {
		// The encoder did not write usable gapless information.
		return Info{}, false, nil
	}
	return Info{
		Delay:      delay,
		Padding:    padding,
		XingFrames: xingFrames,
	}, true, nil
}

// hasLAMEEncoderPrefix reports whether the 9-byte encoder string belongs to a
// LAME-compatible encoder known to fill the gapless fields:
//   - "LAME"     : the LAME encoder itself;
//   - "Lavc"     : FFmpeg's libmp3lame wrapper, which keeps the LAME layout.
func hasLAMEEncoderPrefix(enc string) bool {
	switch {
	case len(enc) >= 4 && enc[:4] == "LAME":
		return true
	case len(enc) >= 4 && enc[:4] == "Lavc":
		return true
	default:
		return false
	}
}
