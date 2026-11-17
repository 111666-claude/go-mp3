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
	"io"
	"io/ioutil"
	"math/rand"
	"testing"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frameheader"
)

// countingReadSeeker wraps an io.ReadSeeker and counts the bytes read from it.
type countingReadSeeker struct {
	r io.ReadSeeker
	n int64
}

func (c *countingReadSeeker) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReadSeeker) Seek(offset int64, whence int) (int64, error) {
	return c.r.Seek(offset, whence)
}

// sameFrameFormat reports whether two frame headers describe the same stream
// format. It mirrors the decoder's checkFrameConsistency.
func sameFrameFormat(a, b frameheader.FrameHeader) bool {
	return a.ID() == b.ID() &&
		a.Layer() == b.Layer() &&
		a.SamplingFrequency() == b.SamplingFrequency() &&
		a.NumberOfChannels() == b.NumberOfChannels()
}

// scanTestFrames splits src into the leading tags and the sequence of MP3
// frames with a consistent format.
func scanTestFrames(t *testing.T, src []byte) (prefix []byte, frames [][]byte) {
	t.Helper()
	s := &source{reader: bytes.NewReader(src)}
	if err := s.skipTags(); err != nil {
		t.Fatal(err)
	}
	start := s.pos
	data := src[start:]
	var first frameheader.FrameHeader
	pos := 0
	for pos+4 <= len(data) {
		h := frameheader.FrameHeader(binary.BigEndian.Uint32(data[pos : pos+4]))
		if !h.IsValid() || h.BitrateIndex() == 0 {
			break
		}
		if h.ID() == consts.Version2_5 || h.Layer() != consts.Layer3 {
			break
		}
		if len(frames) == 0 {
			first = h
		} else if !sameFrameFormat(first, h) {
			break
		}
		size, err := h.FrameSize()
		if err != nil {
			break
		}
		if pos+size > len(data) {
			break
		}
		frames = append(frames, data[pos:pos+size])
		pos += size
	}
	if len(frames) < 200 {
		t.Fatalf("too few frames found: %d", len(frames))
	}
	return src[:start], frames
}

// buildXingFile inserts a synthetic Xing/Info frame at the head of the stream
// in src and returns the new stream. The TOC is built so that every bucket
// maps exactly to a frame boundary: bucket boundary frames are aligned to a
// multiple of the stream's 1/256 byte unit, which makes random seeks land
// exactly on the estimated frame.
func buildXingFile(t *testing.T, src []byte, magic string) []byte {
	t.Helper()
	prefix, frames := scanTestFrames(t, src)

	h := frameheader.FrameHeader(binary.BigEndian.Uint32(frames[0][:4]))
	xingSize, err := h.FrameSize()
	if err != nil {
		t.Fatal(err)
	}
	xoff := 4 + h.SideInfoSize()
	if h.ProtectionBit() == 0 {
		xoff += 2
	}
	if xoff+4+4+4+4+100 > xingSize {
		t.Fatalf("frame size %d too small for a Xing/Info header", xingSize)
	}

	m := len(frames) + 1 // Including the Xing/Info frame itself.
	base := xingSize
	for _, f := range frames {
		base += len(f)
	}

	// The stream is measured in units of 1/256 of the nominal byte count.
	// Choose the unit so that 256 units cover the stream plus the alignment
	// padding (less than one unit per bucket).
	q := base/156 + 1

	// Lay out the frames. Frame 0 is the Xing/Info frame. Bucket boundary
	// frames are aligned to a multiple of q so that the TOC is exact.
	offsets := make([]int, m)
	var toc [100]int
	boundary := map[int]int{}
	for i := 0; i < 100; i++ {
		boundary[i*m/100] = i
	}
	pos := 0
	for j := 0; j < m; j++ {
		if bi, ok := boundary[j]; ok {
			pos = (pos + q - 1) / q * q
			toc[bi] = pos / q
			if toc[bi] > 255 {
				t.Fatalf("TOC entry out of range: %d", toc[bi])
			}
		}
		offsets[j] = pos
		if j == 0 {
			pos += xingSize
		} else {
			pos += len(frames[j-1])
		}
	}
	if pos > 256*q {
		t.Fatalf("stream does not fit in 256 units: %d > %d", pos, 256*q)
	}
	streamBytes := 256 * q

	// Build the Xing/Info frame.
	xing := make([]byte, xingSize)
	copy(xing, frames[0][:4])
	p := xing[xoff:]
	copy(p, magic)
	binary.BigEndian.PutUint32(p[4:], xingFlagFrames|xingFlagBytes|xingFlagTOC)
	binary.BigEndian.PutUint32(p[8:], uint32(m))
	binary.BigEndian.PutUint32(p[12:], uint32(streamBytes))
	for i := 0; i < 100; i++ {
		p[16+i] = byte(toc[i])
	}

	// Assemble the stream.
	var out bytes.Buffer
	out.Write(prefix)
	written := 0
	writeFrame := func(idx int, data []byte) {
		out.Write(make([]byte, offsets[idx]-written))
		out.Write(data)
		written = offsets[idx] + len(data)
	}
	writeFrame(0, xing)
	for j, f := range frames {
		writeFrame(j+1, f)
	}
	return out.Bytes()
}

// readFull reads exactly len(buf) bytes or whatever is available before EOF.
func readFull(t *testing.T, r io.Reader, buf []byte) []byte {
	t.Helper()
	n, err := io.ReadFull(r, buf)
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		t.Fatal(err)
	}
	return buf[:n]
}

// checkSeekWaveform seeks both decoders to pos and verifies that they report
// the same position and produce the same PCM bytes afterwards.
func checkSeekWaveform(t *testing.T, ref, fast *Decoder, pos int64, whence, n int) {
	t.Helper()
	p1, err := ref.Seek(pos, whence)
	if err != nil {
		t.Fatalf("ref.Seek(%d, %d): %v", pos, whence, err)
	}
	p2, err := fast.Seek(pos, whence)
	if err != nil {
		t.Fatalf("fast.Seek(%d, %d): %v", pos, whence, err)
	}
	if p1 != p2 {
		t.Fatalf("seek positions differ: ref=%d fast=%d", p1, p2)
	}
	b1 := readFull(t, ref, make([]byte, n))
	b2 := readFull(t, fast, make([]byte, n))
	if !bytes.Equal(b1, b2) {
		t.Fatalf("waveforms differ after seeking to %d (whence %d)", pos, whence)
	}
}

func TestFastSeekRandomAccess(t *testing.T) {
	for _, tc := range []struct {
		file  string
		magic string
		seeks int
	}{
		{"example/mpeg2.mp3", "Xing", 24},
		{"example/mpeg2.mp3", "Info", 24},
		{"example/classic.mp3", "Xing", 8},
	} {
		t.Run(tc.file+"/"+tc.magic, func(t *testing.T) {
			src, err := ioutil.ReadFile(tc.file)
			if err != nil {
				t.Fatal(err)
			}
			xingFile := buildXingFile(t, src, tc.magic)

			ref, err := NewDecoder(bytes.NewReader(xingFile))
			if err != nil {
				t.Fatal(err)
			}
			cr := &countingReadSeeker{r: bytes.NewReader(xingFile)}
			fast, err := NewFastSeekDecoder(cr)
			if err != nil {
				t.Fatal(err)
			}
			if fast.xing == nil {
				t.Fatal("NewFastSeekDecoder did not use the Xing/Info header")
			}
			if got, want := fast.xing.frames, int64(len(ref.frameStarts)); got != want {
				t.Fatalf("xing frames: got %d, want %d", got, want)
			}
			if got, want := fast.Length(), ref.Length(); got != want {
				t.Fatalf("Length: got %d, want %d", got, want)
			}
			// The constructor must not scan the whole file.
			if cr.n > 64*1024 {
				t.Fatalf("constructor read %d bytes, want a small prefix", cr.n)
			}

			rng := rand.New(rand.NewSource(42))
			length := ref.Length()
			for i := 0; i < tc.seeks; i++ {
				pos := rng.Int63n(length)
				before := cr.n
				checkSeekWaveform(t, ref, fast, pos, io.SeekStart, 16*1024)
				// A single seek must not read a significant part of the file.
				if got, limit := cr.n-before, int64(len(xingFile))/16; got > limit {
					t.Fatalf("seek to %d read %d bytes, limit %d", pos, got, limit)
				}
			}
			// Seeking relative to the end and to the current position.
			checkSeekWaveform(t, ref, fast, -4096, io.SeekEnd, 8*1024)
			checkSeekWaveform(t, ref, fast, 0, io.SeekStart, 4*1024)
			checkSeekWaveform(t, ref, fast, 8192, io.SeekCurrent, 8*1024)

			// The whole session must not have read the whole file.
			if cr.n >= int64(len(xingFile)) {
				t.Fatalf("fast seeks read %d bytes in total, file size %d", cr.n, len(xingFile))
			}
		})
	}
}

func TestFastSeekFallbackNoXing(t *testing.T) {
	src, err := ioutil.ReadFile("example/mpeg2.mp3")
	if err != nil {
		t.Fatal(err)
	}
	ref, err := NewDecoder(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := NewFastSeekDecoder(bytes.NewReader(src))
	if err != nil {
		t.Fatal(err)
	}
	if fast.xing != nil {
		t.Fatal("xing should be nil for a stream without a Xing/Info header")
	}
	if got, want := fast.Length(), ref.Length(); got != want {
		t.Fatalf("Length: got %d, want %d", got, want)
	}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 8; i++ {
		checkSeekWaveform(t, ref, fast, rng.Int63n(ref.Length()), io.SeekStart, 8*1024)
	}
}

func TestFastSeekFallbackBadTOC(t *testing.T) {
	src, err := ioutil.ReadFile("example/mpeg2.mp3")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		corrupt func(toc []byte)
	}{
		{"non-monotonic", func(toc []byte) { toc[50], toc[51] = 200, 10 }},
		{"all-zero", func(toc []byte) {
			for i := range toc {
				toc[i] = 0
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			xingFile := buildXingFile(t, src, "Xing")
			// Find the TOC: it is the 100 bytes right after the Xing magic,
			// the flags and the frames/bytes fields.
			idx := bytes.Index(xingFile, []byte("Xing"))
			if idx < 0 {
				t.Fatal("Xing header not found")
			}
			tc.corrupt(xingFile[idx+16 : idx+116])

			ref, err := NewDecoder(bytes.NewReader(xingFile))
			if err != nil {
				t.Fatal(err)
			}
			fast, err := NewFastSeekDecoder(bytes.NewReader(xingFile))
			if err != nil {
				t.Fatal(err)
			}
			if fast.xing != nil {
				t.Fatal("a corrupt TOC must be rejected")
			}
			if got, want := fast.Length(), ref.Length(); got != want {
				t.Fatalf("Length: got %d, want %d", got, want)
			}
			rng := rand.New(rand.NewSource(2))
			for i := 0; i < 8; i++ {
				checkSeekWaveform(t, ref, fast, rng.Int63n(ref.Length()), io.SeekStart, 8*1024)
			}
		})
	}
}

func TestSeekNonSeeker(t *testing.T) {
	src, err := ioutil.ReadFile("example/mpeg2.mp3")
	if err != nil {
		t.Fatal(err)
	}
	// Hide the Seek method from the reader.
	d, err := NewFastSeekDecoder(struct{ io.Reader }{bytes.NewReader(src)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Seek(1024, io.SeekStart); err == nil {
		t.Fatal("seeking a non-io.Seeker source should fail")
	}
}

func TestFastSeekTruncated(t *testing.T) {
	src, err := ioutil.ReadFile("example/mpeg2.mp3")
	if err != nil {
		t.Fatal(err)
	}
	xingFile := buildXingFile(t, src, "Xing")
	truncated := xingFile[:len(xingFile)*6/10]

	ref, err := NewDecoder(bytes.NewReader(truncated))
	if err != nil {
		t.Fatal(err)
	}
	fast, err := NewFastSeekDecoder(bytes.NewReader(truncated))
	if err != nil {
		t.Fatal(err)
	}
	if fast.xing == nil {
		t.Fatal("xing should be used for the truncated stream")
	}
	// The claimed length is based on the Xing header and exceeds the actual
	// stream.
	claimed := fast.Length()
	if claimed <= ref.Length() {
		t.Fatalf("claimed length %d should exceed the actual %d", claimed, ref.Length())
	}

	// A seek within the actual stream works and matches the exact seek.
	checkSeekWaveform(t, ref, fast, claimed/3, io.SeekStart, 8*1024)

	// A seek beyond the actual stream deterministically falls back to the
	// exact seek, which reports an out-of-range error.
	if _, err := fast.Seek(claimed*9/10, io.SeekStart); err == nil {
		t.Fatal("seek beyond the truncated stream should fail")
	}
	if fast.xing != nil {
		t.Fatal("the fast path should have been disabled after the fallback")
	}
}

func TestSeekOutOfRange(t *testing.T) {
	src, err := ioutil.ReadFile("example/mpeg2.mp3")
	if err != nil {
		t.Fatal(err)
	}
	xingFile := buildXingFile(t, src, "Xing")

	fast, err := NewFastSeekDecoder(bytes.NewReader(xingFile))
	if err != nil {
		t.Fatal(err)
	}
	if fast.xing == nil {
		t.Fatal("xing should be used")
	}
	reg, err := NewDecoder(bytes.NewReader(xingFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*Decoder{fast, reg} {
		if _, err := d.Seek(-1, io.SeekStart); err == nil {
			t.Error("seeking to a negative position should fail")
		}
		if _, err := d.Seek(d.Length()+1, io.SeekStart); err == nil {
			t.Error("seeking beyond the end should fail")
		}
		if _, err := d.Seek(1, io.SeekEnd); err == nil {
			t.Error("seeking beyond the end should fail")
		}
		// Seeking to the very end is valid; the next Read reports io.EOF.
		if _, err := d.Seek(0, io.SeekEnd); err != nil {
			t.Errorf("seeking to the end: %v", err)
		}
		if n, err := d.Read(make([]byte, 16)); n != 0 || err != io.EOF {
			t.Errorf("Read at the end: got (%d, %v), want (0, io.EOF)", n, err)
		}
	}
}

func TestFastSeekFallbackInconsistentFrame(t *testing.T) {
	src, err := ioutil.ReadFile("example/mpeg2.mp3")
	if err != nil {
		t.Fatal(err)
	}
	xingFile := buildXingFile(t, src, "Xing")

	// Corrupt the channel mode of the frame at TOC bucket 50. The frame
	// stays a valid MP3 frame of the same size, but its channel count
	// differs from the first frame's.
	fast, err := NewFastSeekDecoder(bytes.NewReader(xingFile))
	if err != nil {
		t.Fatal(err)
	}
	if fast.xing == nil {
		t.Fatal("xing should be used")
	}
	m := fast.xing.frames
	g := 50 * m / 100
	// Locate the frame at bucket 50 via the TOC.
	off := fast.firstFrameStart + int64(fast.xing.toc[50])*fast.xing.bytes/256
	xingFile[off+3] &^= 0xc0 // Set the channel mode to stereo.

	ref, err := NewDecoder(bytes.NewReader(xingFile))
	if err != nil {
		t.Fatal(err)
	}
	// Seek to a position whose TOC bucket starts at the corrupt frame. The
	// target frame itself is past the corrupt frame, so the exact seek does
	// not decode it.
	pos := (g + 5) * fast.bytesPerFrame
	checkSeekWaveform(t, ref, fast, pos, io.SeekStart, 8*1024)
	if fast.xing != nil {
		t.Fatal("the fast path should have been disabled after the fallback")
	}
}
