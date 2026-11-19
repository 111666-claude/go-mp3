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
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/hajimehoshi/go-mp3/internal/crc16"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
)

// frameInfo describes one MPEG audio frame inside an MP3 stream.
type frameInfo struct {
	offset int
	size   int
	header frameheader.FrameHeader
}

// skipID3v2 returns the offset of the first byte after a leading ID3v2 tag.
func skipID3v2(data []byte) int {
	if len(data) >= 10 && string(data[:3]) == "ID3" {
		size := int(data[6])<<21 | int(data[7])<<14 | int(data[8])<<7 | int(data[9])
		return 10 + size
	}
	return 0
}

// scanFrames splits data into a leading tag prefix, the MPEG frames and a
// trailing remainder that does not form a complete frame.
func scanFrames(t *testing.T, data []byte) (prefix int, frames []frameInfo, trailer []byte) {
	t.Helper()
	prefix = skipID3v2(data)
	off := prefix
	for off+4 <= len(data) {
		h := frameheader.FrameHeader(binary.BigEndian.Uint32(data[off:]))
		if !h.IsValid() {
			break
		}
		size, err := h.FrameSize()
		if err != nil || size < 4 || off+size > len(data) {
			break
		}
		frames = append(frames, frameInfo{offset: off, size: size, header: h})
		off += size
	}
	return prefix, frames, data[off:]
}

// addCRC rewrites every frame of an MP3 stream so that it carries CRC
// protection: the protection bit is cleared, the bitrate index is raised
// (by bump(i) table steps) so that the frame grows, a CRC-16 over the
// header's last two bytes and the side information is inserted after the
// header, and the frame is padded with zero (ancillary) bytes up to its
// new size. The original main data of every frame is preserved verbatim,
// so the rewritten stream decodes to the exact same PCM. bump(i) must be
// at least 1 so that there is room for the two CRC bytes.
func addCRC(t *testing.T, data []byte, bump func(i int) int) ([]byte, []frameInfo) {
	t.Helper()
	prefix, frames, trailer := scanFrames(t, data)
	var out []byte
	out = append(out, data[:prefix]...)
	for i, f := range frames {
		sis := f.header.SideInfoSize()
		hdr := append([]byte(nil), data[f.offset:f.offset+4]...)
		hdr[1] &^= 0x01 // clear the protection bit
		brIdx := int(hdr[2] >> 4)
		if brIdx+bump(i) > 14 {
			t.Fatalf("frame %d: bitrate index %d cannot be raised by %d", i, brIdx, bump(i))
		}
		hdr[2] = hdr[2]&0x0f | byte(brIdx+bump(i))<<4
		nh := frameheader.FrameHeader(binary.BigEndian.Uint32(hdr))
		nsize, err := nh.FrameSize()
		if err != nil {
			t.Fatal(err)
		}
		side := data[f.offset+4 : f.offset+4+sis]
		main := data[f.offset+4+sis : f.offset+f.size]
		pad := nsize - 4 - 2 - sis - len(main)
		if pad < 0 {
			t.Fatalf("frame %d: no room for the CRC (frame size %d, need %d)", i, nsize, 4+2+sis+len(main))
		}
		crc := crc16.New()
		crc.Write(hdr[2:4])
		crc.Write(side)
		out = append(out, hdr...)
		var crcb [2]byte
		binary.BigEndian.PutUint16(crcb[:], crc.Sum())
		out = append(out, crcb[:]...)
		out = append(out, side...)
		out = append(out, main...)
		out = append(out, make([]byte, pad)...)
	}
	out = append(out, trailer...)
	_, newFrames, _ := scanFrames(t, out)
	if len(newFrames) != len(frames) {
		t.Fatalf("frame count changed: got %d, want %d", len(newFrames), len(frames))
	}
	return out, newFrames
}

// flipBit returns a copy of data with one bit flipped.
func flipBit(data []byte, offset int, mask byte) []byte {
	c := append([]byte(nil), data...)
	c[offset] ^= mask
	return c
}

// frameCRC computes the CRC-16 a protected frame at offset off should carry.
func frameCRC(data []byte, f frameInfo) uint16 {
	crc := crc16.New()
	crc.Write(data[f.offset+2 : f.offset+4])
	// Skip the header (4 bytes) and the stored CRC (2 bytes).
	side := f.offset + 4 + 2
	crc.Write(data[side : side+f.header.SideInfoSize()])
	return crc.Sum()
}

type nonSeeker struct {
	r io.Reader
}

func (n nonSeeker) Read(p []byte) (int, error) {
	return n.r.Read(p)
}

func decodeAll(t *testing.T, r io.Reader, opts *Options) ([]byte, error) {
	t.Helper()
	d, err := NewDecoderWithOptions(r, opts)
	if err != nil {
		return nil, err
	}
	return io.ReadAll(d)
}

// fixtures holds the shared, lazily built test fixtures.
var fixtures struct {
	once sync.Once

	classicData   []byte
	classicPCM    []byte
	protectedData []byte
	protectedPCM  []byte
	frames        []frameInfo
	bytesPerFrame int
}

func setupFixtures(t *testing.T) {
	t.Helper()
	fixtures.once.Do(func() {
		var err error
		fixtures.classicData, err = os.ReadFile("example/classic.mp3")
		if err != nil {
			t.Fatal(err)
		}
		fixtures.classicPCM, err = decodeAll(t, bytes.NewReader(fixtures.classicData), nil)
		if err != nil {
			t.Fatal(err)
		}
		// testdata/crc_cbr.mp3 is a CRC-protected MP3 generated from a
		// raw stream; see TestGenerateProtectedFixture in
		// fixture_gen_test.go for how it was built. Its PCM is identical
		// to the raw stream's PCM.
		fixtures.protectedData, err = os.ReadFile("testdata/crc_cbr.mp3")
		if err != nil {
			t.Fatal(err)
		}
		_, fixtures.frames, _ = scanFrames(t, fixtures.protectedData)
		if len(fixtures.frames) == 0 {
			t.Fatal("no frames found in testdata/crc_cbr.mp3")
		}
		for i, f := range fixtures.frames {
			if f.header.ProtectionBit() != 0 {
				t.Fatalf("frame %d of the protected fixture is not CRC-protected", i)
			}
		}
		// Decoding with verification enabled must succeed and must not
		// change the output.
		pcm, err := decodeAll(t, bytes.NewReader(fixtures.protectedData), &Options{VerifyCRC: true})
		if err != nil {
			t.Fatal(err)
		}
		fixtures.protectedPCM, err = decodeAll(t, bytes.NewReader(fixtures.protectedData), nil)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(pcm, fixtures.protectedPCM) {
			t.Fatal("PCM differs with CRC verification enabled")
		}
		fixtures.bytesPerFrame = fixtures.frames[0].header.BytesPerFrame()
		if got, want := len(fixtures.protectedPCM), len(fixtures.frames)*fixtures.bytesPerFrame; got != want {
			t.Fatalf("unexpected PCM size: got %d, want %d", got, want)
		}
	})
}

// corruptFrameSideInfo returns a copy of the protected fixture with one bit
// flipped inside the side information of the idx-th frame.
func corruptFrameSideInfo(t *testing.T, idx int) []byte {
	t.Helper()
	setupFixtures(t)
	f := fixtures.frames[idx]
	// Skip the header (4 bytes) and the CRC (2 bytes), flip a bit well
	// inside the side information.
	return flipBit(fixtures.protectedData, f.offset+4+2+3, 0x10)
}

func TestProtectedFixtureDecodesIdentically(t *testing.T) {
	setupFixtures(t)
	// The fixture must be real audio, not digital silence, so that the
	// PCM comparisons in the other tests are meaningful.
	allZero := true
	for _, b := range fixtures.protectedPCM {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("the protected fixture decodes to silence")
	}
}

func TestNoFalsePositivesOnUnprotectedFrames(t *testing.T) {
	setupFixtures(t)
	// classic.mp3 has no CRC-protected frames. Verification must not
	// report anything and must not change the output.
	pcm, err := decodeAll(t, bytes.NewReader(fixtures.classicData), &Options{VerifyCRC: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pcm, fixtures.classicPCM) {
		t.Fatal("PCM differs with CRC verification enabled")
	}
}

func TestValidCRCsAccepted(t *testing.T) {
	setupFixtures(t)
	pcm, err := decodeAll(t, bytes.NewReader(fixtures.protectedData), &Options{VerifyCRC: true})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pcm, fixtures.protectedPCM) {
		t.Fatal("PCM differs with CRC verification enabled")
	}
}

func TestCRCErrorStrict(t *testing.T) {
	setupFixtures(t)
	bad := len(fixtures.frames) / 2
	data := corruptFrameSideInfo(t, bad)
	f := fixtures.frames[bad]

	d, err := NewDecoderWithOptions(bytes.NewReader(data), &Options{VerifyCRC: true})
	if err != nil {
		t.Fatal(err)
	}
	var out []byte
	buf := make([]byte, 8192)
	var crcErr *CRCError
	for {
		n, err := d.Read(buf)
		out = append(out, buf[:n]...)
		if err == io.EOF {
			break
		}
		if err == nil {
			continue
		}
		var e *CRCError
		if !errors.As(err, &e) {
			t.Fatalf("unexpected error: %v", err)
		}
		if crcErr != nil {
			t.Fatalf("more than one CRC error: %v and %v", crcErr, e)
		}
		crcErr = e
	}
	if crcErr == nil {
		t.Fatal("no CRC error reported")
	}
	if got, want := crcErr.Position, int64(f.offset); got != want {
		t.Errorf("Position: got %d, want %d", got, want)
	}
	if got, want := crcErr.Expected, binary.BigEndian.Uint16(fixtures.protectedData[f.offset+4:]); got != want {
		t.Errorf("Expected: got 0x%04x, want 0x%04x", got, want)
	}
	if got, want := crcErr.Actual, frameCRC(data, f); got != want {
		t.Errorf("Actual: got 0x%04x, want 0x%04x", got, want)
	}
	if got, want := crcErr.BytesPerFrame, fixtures.bytesPerFrame; got != want {
		t.Errorf("BytesPerFrame: got %d, want %d", got, want)
	}
	// In strict mode the corrupt frame is dropped: the stream continues
	// at the next frame and exactly one frame of PCM is missing.
	if got, want := len(out), len(fixtures.protectedPCM)-fixtures.bytesPerFrame; got != want {
		t.Errorf("PCM size after the error: got %d, want %d", got, want)
	}
}

func TestCRCErrorStrictAtFirstFrame(t *testing.T) {
	setupFixtures(t)
	data := corruptFrameSideInfo(t, 0)
	_, err := NewDecoderWithOptions(bytes.NewReader(data), &Options{VerifyCRC: true})
	var crcErr *CRCError
	if !errors.As(err, &crcErr) {
		t.Fatalf("got %v, want a *CRCError", err)
	}
	if got, want := crcErr.Position, int64(fixtures.frames[0].offset); got != want {
		t.Errorf("Position: got %d, want %d", got, want)
	}
}

func TestCRCErrorSkip(t *testing.T) {
	setupFixtures(t)
	bad := len(fixtures.frames) / 2
	data := corruptFrameSideInfo(t, bad)

	pcm, err := decodeAll(t, bytes.NewReader(data), &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	bpf := fixtures.bytesPerFrame
	// The PCM time positions are preserved: the corrupt frame is replaced
	// by exactly one frame of silence.
	if got, want := len(pcm), len(fixtures.protectedPCM); got != want {
		t.Fatalf("PCM size: got %d, want %d", got, want)
	}
	if !bytes.Equal(pcm[:bad*bpf], fixtures.protectedPCM[:bad*bpf]) {
		t.Error("PCM before the corrupt frame differs")
	}
	for i, b := range pcm[bad*bpf : (bad+1)*bpf] {
		if b != 0 {
			t.Fatalf("skipped frame is not silent at byte %d", bad*bpf+i)
		}
	}
}

func TestCRCErrorSkipFirstFrame(t *testing.T) {
	setupFixtures(t)
	data := corruptFrameSideInfo(t, 0)
	pcm, err := decodeAll(t, bytes.NewReader(data), &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(pcm), len(fixtures.protectedPCM); got != want {
		t.Fatalf("PCM size: got %d, want %d", got, want)
	}
}

func TestCRCErrorSkipConsecutiveLimit(t *testing.T) {
	setupFixtures(t)
	// Corrupt more consecutive frames than the default limit allows.
	bad := len(fixtures.frames)/2 - defaultMaxConsecutiveBadFrames
	data := fixtures.protectedData
	for i := bad; i < bad+defaultMaxConsecutiveBadFrames+2; i++ {
		f := fixtures.frames[i]
		data = flipBit(data, f.offset+4+2+3, 0x10)
	}
	_, err := decodeAll(t, bytes.NewReader(data), &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err == nil || !strings.Contains(err.Error(), "consecutive corrupt frames") {
		t.Fatalf("got %v, want a consecutive corrupt frames error", err)
	}

	// With a raised limit the same stream decodes to the end.
	pcm, err := decodeAll(t, bytes.NewReader(data), &Options{
		VerifyCRC:               true,
		SkipBadFrames:           true,
		MaxConsecutiveBadFrames: 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(pcm), len(fixtures.protectedPCM); got != want {
		t.Fatalf("PCM size: got %d, want %d", got, want)
	}
}

func TestTruncatedCRC(t *testing.T) {
	setupFixtures(t)
	// Truncate the stream in the middle of the CRC of frame 5.
	f := fixtures.frames[5]
	data := fixtures.protectedData[:f.offset+4+1]
	pcm, err := decodeAll(t, bytes.NewReader(data), &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(pcm), 5*fixtures.bytesPerFrame; got != want {
		t.Fatalf("PCM size: got %d, want %d", got, want)
	}
}

func TestFalseSyncRecovery(t *testing.T) {
	setupFixtures(t)
	// Destroy the sync word of one frame. The decoder must resynchronize
	// and finish the stream without hanging or panicking.
	bad := len(fixtures.frames) / 2
	f := fixtures.frames[bad]
	data := flipBit(fixtures.protectedData, f.offset, 0x01)
	pcm, err := decodeAll(t, bytes.NewReader(data), &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The output must stay bounded: at most a few frames are gained or
	// lost while resynchronizing.
	bpf := fixtures.bytesPerFrame
	if diff := len(pcm) - len(fixtures.protectedPCM); diff < -2*bpf || diff > 2*bpf {
		t.Fatalf("PCM size drifted by %d bytes", diff)
	}
}

func TestNonSeekableSource(t *testing.T) {
	setupFixtures(t)
	bad := len(fixtures.frames) / 2
	data := corruptFrameSideInfo(t, bad)

	d, err := NewDecoderWithOptions(nonSeeker{bytes.NewReader(data)}, &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.Length(); got != -1 {
		t.Fatalf("Length: got %d, want -1", got)
	}
	pcm, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := len(pcm), len(fixtures.protectedPCM); got != want {
		t.Fatalf("PCM size: got %d, want %d", got, want)
	}
	if _, err := d.Seek(0, io.SeekStart); err == nil {
		t.Fatal("Seek on a non-seekable source must return an error")
	}
}

func TestSeekWithBadFrames(t *testing.T) {
	setupFixtures(t)
	bad := len(fixtures.frames) / 2
	data := corruptFrameSideInfo(t, bad)
	bpf := int64(fixtures.bytesPerFrame)

	// Strict mode: seeking onto the corrupt frame reports the CRC error.
	d, err := NewDecoderWithOptions(bytes.NewReader(data), &Options{VerifyCRC: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Seek(int64(bad)*bpf, io.SeekStart); err != nil {
		var crcErr *CRCError
		if !errors.As(err, &crcErr) {
			t.Fatalf("got %v, want a *CRCError", err)
		}
	} else {
		t.Fatal("no error seeking onto a corrupt frame in strict mode")
	}

	// Skip mode: seeking near the corrupt frame works and reading
	// continues with silence at the corrupt position.
	d, err = NewDecoderWithOptions(bytes.NewReader(data), &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Seek(int64(bad-1)*bpf, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	pcm := make([]byte, 3*bpf)
	if _, err := io.ReadFull(d, pcm); err != nil {
		t.Fatal(err)
	}
	for i, b := range pcm[bpf : 2*bpf] {
		if b != 0 {
			t.Fatalf("skipped frame is not silent at byte %d", int(bpf)+i)
		}
	}
}

func TestVBRWithBadFrames(t *testing.T) {
	// testdata/crc_vbr.mp3 is a CRC-protected VBR stream; see
	// TestGenerateProtectedFixture in fixture_gen_test.go.
	data, err := os.ReadFile("testdata/crc_vbr.mp3")
	if err != nil {
		t.Fatal(err)
	}
	_, frames, _ := scanFrames(t, data)
	sizes := map[int]bool{}
	for _, f := range frames {
		sizes[f.size] = true
	}
	if len(sizes) < 2 {
		t.Fatalf("not a VBR stream, frame sizes: %v", sizes)
	}
	bpf := frames[0].header.BytesPerFrame()

	want, err := decodeAll(t, bytes.NewReader(data), &Options{VerifyCRC: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(want); got != len(frames)*bpf {
		t.Fatalf("unexpected PCM size: got %d, want %d", got, len(frames)*bpf)
	}

	// Corrupt one frame in the middle and decode with recovery from a
	// non-seekable source.
	bad := len(frames) / 2
	f := frames[bad]
	data = flipBit(data, f.offset+4+2+3, 0x10)
	pcm, err := decodeAll(t, nonSeeker{bytes.NewReader(data)}, &Options{
		VerifyCRC:     true,
		SkipBadFrames: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The PCM time positions are preserved even for VBR streams.
	if got := len(pcm); got != len(want) {
		t.Fatalf("PCM size: got %d, want %d", got, len(want))
	}
	if !bytes.Equal(pcm[:bad*bpf], want[:bad*bpf]) {
		t.Error("PCM before the corrupt frame differs")
	}
	for i, b := range pcm[bad*bpf : (bad+1)*bpf] {
		if b != 0 {
			t.Fatalf("skipped frame is not silent at byte %d", bad*bpf+i)
		}
	}
}
