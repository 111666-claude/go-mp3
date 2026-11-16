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

package gapless

import (
	"os"
	"testing"
)

// buildMPEG1Frame wraps a Xing/Info + LAME payload in a valid MPEG1 Layer III
// frame header (128 kbps, 44.1 kHz, stereo: 417 compressed bytes).
func buildMPEG1Frame(xingAt int, payload []byte) []byte {
	frame := make([]byte, 417)
	// 0xFFFB9000: MPEG1, Layer III, no CRC, 128kbps index 9, 44.1kHz, stereo.
	frame[0] = 0xFF
	frame[1] = 0xFB
	frame[2] = 0x90
	frame[3] = 0x00
	copy(frame[xingAt:], payload)
	return frame
}

// xingPayload builds the Xing/Info + LAME region with controllable fields.
// xingAt is the offset where the payload starts inside the frame.
func xingPayload(magic string, flags uint32, enc string, delay, padding int) []byte {
	buf := make([]byte, 417-36) // caller slices to frame size anyway
	p := 0
	copy(buf[p:], magic)
	p += 4
	buf[p] = byte(flags >> 24)
	buf[p+1] = byte(flags >> 16)
	buf[p+2] = byte(flags >> 8)
	buf[p+3] = byte(flags)
	p += 4
	if flags&0x0001 != 0 { // frames
		p += 4
	}
	if flags&0x0002 != 0 { // bytes
		p += 4
	}
	if flags&0x0004 != 0 { // TOC
		p += 100
	}
	if flags&0x0008 != 0 { // quality
		p += 4
	}
	// LAME tag: 9-byte encoder string, then 12 bytes to the gapless field.
	encBytes := make([]byte, 9)
	copy(encBytes, enc)
	copy(buf[p:], encBytes)
	// Gapless info sits at LAME tag offset 21..23.
	buf[p+21] = byte(delay >> 4)
	buf[p+22] = byte((delay&0x0F)<<4) | byte((padding>>8)&0x0F)
	buf[p+23] = byte(padding)
	return buf
}

const mpeg1XingOffset = 4 + 32 // no CRC + 32 stereo side-info bytes

func TestParseKnownValues(t *testing.T) {
	cases := []struct {
		name    string
		magic   string
		enc     string
		delay   int
		padding int
	}{
		{"lame info", "Info", "LAME3.99", 576, 1404},
		{"lame xing", "Xing", "LAME3.100", 1445, 107},
		{"ffmpeg lavc", "Info", "Lavc62.11", 1152, 576},
		{"max 12-bit", "Xing", "LAME3.99", 4095, 4095},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := xingPayload(tc.magic, 0x000F, tc.enc, tc.delay, tc.padding)
			frame := buildMPEG1Frame(mpeg1XingOffset, payload)
			info, ok, err := Parse(frame)
			if err != nil || !ok {
				t.Fatalf("Parse: ok=%v err=%v", ok, err)
			}
			if info.Delay != tc.delay || info.Padding != tc.padding {
				t.Fatalf("got delay=%d padding=%d, want %d/%d",
					info.Delay, info.Padding, tc.delay, tc.padding)
			}
		})
	}
}

func TestParseXingFrameCount(t *testing.T) {
	// A Xing header with only the frames flag set; the count must round-trip.
	var frames uint32 = 0x01020304
	frame := buildMPEG1Frame(mpeg1XingOffset,
		payloadWithFrames("LAME3.99", 576, 1404, frames))
	info, ok, err := Parse(frame)
	if err != nil || !ok {
		t.Fatalf("Parse: ok=%v err=%v", ok, err)
	}
	if uint32(info.XingFrames) != frames {
		t.Fatalf("XingFrames = %d, want %d", info.XingFrames, frames)
	}
	if info.Delay != 576 || info.Padding != 1404 {
		t.Fatalf("gapless fields drift: %+v", info)
	}
}

// payloadWithFrames is like xingPayload but with an explicit frame-count field
// and only the Xing frames flag set.
func payloadWithFrames(enc string, delay, padding int, frames uint32) []byte {
	buf := make([]byte, 417-36)
	p := 0
	copy(buf[p:], "Xing")
	p += 4
	buf[p] = 0
	buf[p+1] = 0
	buf[p+2] = 0
	buf[p+3] = 1 // frames flag only
	p += 4
	buf[p] = byte(frames >> 24)
	buf[p+1] = byte(frames >> 16)
	buf[p+2] = byte(frames >> 8)
	buf[p+3] = byte(frames)
	p += 4
	encBytes := make([]byte, 9)
	copy(encBytes, enc)
	copy(buf[p:], encBytes)
	buf[p+21] = byte(delay >> 4)
	buf[p+22] = byte((delay&0x0F)<<4) | byte((padding>>8)&0x0F)
	buf[p+23] = byte(padding)
	return buf
}

func TestParseRejectsNonLAME(t *testing.T) {
	payload := xingPayload("Info", 0x000F, "NotLAME\x00", 576, 1404)
	frame := buildMPEG1Frame(mpeg1XingOffset, payload)
	if _, ok, err := Parse(frame); err != errNoTag || ok {
		t.Fatalf("unrecognized encoder must not be accepted: ok=%v err=%v", ok, err)
	}
}

func TestParseNoXingMagic(t *testing.T) {
	// A frame whose side-info/main-data area does not start with Xing/Info:
	// ordinary audio must never be reported as gapless.
	frame := buildMPEG1Frame(mpeg1XingOffset, make([]byte, 100))
	if _, ok, err := Parse(frame); err != errNoTag || ok {
		t.Fatalf("plain audio frame: ok=%v err=%v", ok, err)
	}
}

func TestParseZeroGapless(t *testing.T) {
	payload := xingPayload("Xing", 0x000F, "LAME3.99", 0, 0)
	frame := buildMPEG1Frame(mpeg1XingOffset, payload)
	if _, ok, err := Parse(frame); err != nil || ok {
		t.Fatalf("zero delay/padding: ok=%v err=%v", ok, err)
	}
}

func TestParseTruncatedTag(t *testing.T) {
	payload := xingPayload("Info", 0x000F, "LAME3.99", 576, 1404)
	// Cut the frame before the LAME gapless bytes (LAME tag starts at
	// payload offset 8; its gapless field needs 24 bytes).
	frame := buildMPEG1Frame(mpeg1XingOffset, payload)
	if _, ok, err := Parse(frame[:mpeg1XingOffset+8+20]); err == nil || ok {
		t.Fatalf("truncated LAME tag: ok=%v err=%v", ok, err)
	}
	// A frame truncated even before the Xing magic.
	if _, ok, err := Parse(frame[:mpeg1XingOffset+2]); err == nil || ok {
		t.Fatalf("truncated Xing tag: ok=%v err=%v", ok, err)
	}
}

func TestParseMPEG2MonoLayout(t *testing.T) {
	// MPEG2 mono: 9-byte side info, 576 samples/frame. The synthetic header
	// uses 48kbps at 16kHz for a 216-byte frame, large enough to hold the
	// full Xing/LAME tag like real MPEG2 mono files.
	frame := make([]byte, 216)
	// 0xFFF368C0: MPEG2 (ID=2), Layer III, 48kbps index 5, 16kHz, mono.
	frame[0] = 0xFF
	frame[1] = 0xF3
	frame[2] = 0x68
	frame[3] = 0xC0
	xingAt := 4 + 9
	payload := xingPayload("Info", 0x000F, "Lavc62.11", 576, 990)
	copy(frame[xingAt:], payload)
	info, ok, err := Parse(frame)
	if err != nil || !ok {
		t.Fatalf("MPEG2 mono parse: ok=%v err=%v", ok, err)
	}
	if info.Delay != 576 || info.Padding != 990 {
		t.Fatalf("got %+v", info)
	}
}

func TestParseRealFixtures(t *testing.T) {
	cases := []struct {
		path          string
		wantDelay     int
		wantPadding   int
		wantFrames    int
		wantGaplessOK bool
	}{
		{"../../testdata/gapless/lame_patched.mp3", 1445, 107, 40, true},
		{"../../testdata/gapless/lavc_cbr.mp3", 576, 1404, 40, true},
		{"../../testdata/gapless/lavc_mpeg2_mono.mp3", 576, 990, 41, true},
		{"../../testdata/gapless/lame_overtrim_short.mp3", 4095, 0, 6, true},
		// Truncated tag and foreign encoder string: usable metadata absent.
		{"../../testdata/gapless/lame_truncated.mp3", 0, 0, 0, false},
		{"../../testdata/gapless/lame_badmagic.mp3", 0, 0, 0, false},
		// Files without any Xing header at all.
		{"../../example/classic.mp3", 0, 0, 0, false},
		{"../../example/mpeg2.mp3", 0, 0, 0, false},
	}
	for _, tc := range cases {
		t.Run(tc.path, func(t *testing.T) {
			raw, err := os.ReadFile(tc.path)
			if err != nil {
				t.Fatal(err)
			}
			// Locate the first audio frame: skip an ID3v2 tag if present.
			off := 0
			if string(raw[:3]) == "ID3" {
				size := 0
				for _, b := range raw[6:10] {
					size = (size << 7) | int(b&0x7F)
				}
				off = 10 + size
			}
			info, ok, err := Parse(raw[off:])
			if err != nil && err != errNoTag && err != errTruncated {
				t.Fatalf("unexpected Parse error: %v", err)
			}
			if ok != tc.wantGaplessOK {
				t.Fatalf("ok=%v want %v (info=%+v, err=%v)", ok, tc.wantGaplessOK, info, err)
			}
			if ok {
				if info.Delay != tc.wantDelay || info.Padding != tc.wantPadding {
					t.Fatalf("got delay=%d padding=%d, want %d/%d",
						info.Delay, info.Padding, tc.wantDelay, tc.wantPadding)
				}
				if info.XingFrames != tc.wantFrames {
					t.Fatalf("XingFrames=%d want %d", info.XingFrames, tc.wantFrames)
				}
			}
		})
	}
}
