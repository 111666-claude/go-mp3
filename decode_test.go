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
	"path/filepath"
	"testing"
)

func openFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "gapless", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decodeAll(t *testing.T, r io.Reader, opts DecoderOptions) (*Decoder, []byte) {
	t.Helper()
	d, err := NewDecoderWithOptions(r, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(d)
	if err != nil {
		t.Fatalf("ReadAll: %v", err)
	}
	return d, got
}

func TestGaplessLengths(t *testing.T) {
	// wantSamples is the encoder-declared music sample count per channel,
	// computed as XingFrames*samplesPerFrame - delay - padding. This is the
	// authoritative gapless length and can be smaller than a naive
	// "physical frames minus padding" subtraction when the encoder appends a
	// padding-only final frame.
	cases := []struct {
		name         string
		delay        int
		padding      int
		xingFrames   int
		samplesPerFr int
		hasGapless   bool
		sampleRate   int
	}{
		{"lavc_cbr.mp3", 576, 1404, 40, 1152, true, 44100},
		{"lavc_vbr.mp3", 576, 1404, 40, 1152, true, 44100},
		{"lame_patched.mp3", 1445, 107, 40, 1152, true, 44100},
		{"lavc_mpeg2_mono.mp3", 576, 990, 41, 576, true, 22050},
		// Usable metadata absent: trimmed length must equal raw length.
		{"lame_truncated.mp3", 0, 0, 0, 1152, false, 44100},
		{"lame_badmagic.mp3", 0, 0, 0, 1152, false, 44100},
		// Large but in-range values are honored.
		{"lame_overtrim.mp3", 4095, 4095, 40, 1152, true, 44100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := openFixture(t, tc.name)
			plain, plainPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{})
			if plain.SampleRate() != tc.sampleRate {
				t.Fatalf("sample rate: got %d want %d", plain.SampleRate(), tc.sampleRate)
			}
			if int64(len(plainPCM)) != plain.Length() {
				t.Fatalf("raw: read %d bytes but Length()=%d", len(plainPCM), plain.Length())
			}

			gd, gPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})
			want := plain.Length()
			if tc.hasGapless {
				musicSamples := int64(tc.xingFrames*tc.samplesPerFr - tc.delay - tc.padding)
				want = musicSamples * bytesPerSample
			}
			if gd.Length() != want {
				t.Fatalf("trimmed Length(): got %d want %d (raw %d)",
					gd.Length(), want, plain.Length())
			}
			if int64(len(gPCM)) != want {
				t.Fatalf("trimmed ReadAll: got %d bytes want %d", len(gPCM), want)
			}
			if len(gPCM)%bytesPerSample != 0 {
				t.Fatalf("trimmed length not aligned to stereo samples: %d", len(gPCM))
			}
		})
	}
}

// TestGaplessIsExactSubstream proves no samples are dropped or duplicated:
// the trimmed output must appear byte-for-byte inside the untrimmed output,
// starting right after the encoder delay. The end is derived from the Xing
// frame count minus the end padding, exactly as the decoder computes it.
func TestGaplessIsExactSubstream(t *testing.T) {
	cases := []struct {
		name         string
		delay        int
		padding      int
		xingFrames   int
		samplesPerFr int
	}{
		{"lavc_cbr.mp3", 576, 1404, 40, 1152},
		{"lavc_vbr.mp3", 576, 1404, 40, 1152},
		{"lame_patched.mp3", 1445, 107, 40, 1152},
		{"lavc_mpeg2_mono.mp3", 576, 990, 41, 576},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw := openFixture(t, tc.name)
			_, plainPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{})
			_, gapPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})

			start := tc.delay * bytesPerSample
			validEnd := tc.xingFrames * tc.samplesPerFr * bytesPerSample
			end := validEnd - tc.padding*bytesPerSample
			if end-start != len(gapPCM) {
				t.Fatalf("length mismatch: window %d, gapless %d", end-start, len(gapPCM))
			}
			if !bytes.Equal(plainPCM[start:end], gapPCM) {
				t.Fatalf("trimmed stream is not the exact middle of the raw stream")
			}
		})
	}
}

// TestGaplessSmallReadsAndFrameCrossings reads through tiny buffers whose
// sizes deliberately do not align with MP3 frames (4608 bytes) or samples,
// then reassembles the stream.
func TestGaplessSmallReadsAndFrameCrossings(t *testing.T) {
	raw := openFixture(t, "lame_patched.mp3")
	_, whole := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})

	for _, bufSize := range []int{1, 3, 4, 7, 100, 4095, 4096, 4097, 4607, 4608, 4609} {
		t.Run("", func(t *testing.T) {
			d, err := NewDecoderWithOptions(bytes.NewReader(raw), DecoderOptions{Gapless: true})
			if err != nil {
				t.Fatal(err)
			}
			var got bytes.Buffer
			buf := make([]byte, bufSize)
			for {
				n, err := d.Read(buf)
				got.Write(buf[:n])
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatalf("Read: %v", err)
				}
			}
			if !bytes.Equal(got.Bytes(), whole) {
				t.Fatalf("bufSize=%d reassembled %d bytes, want %d",
					bufSize, got.Len(), len(whole))
			}
		})
	}
}

// TestGaplessHeadAndTailWaveform verifies the head and tail carry the
// expected signal. MP3's filter bank adds its own delay on top of the LAME
// delay and smears the first frame, so sample-zero alignment with the source
// WAV is not achievable for any MP3 decoder. The strong guarantees here are:
//
//  1. the trimmed stream begins exactly the encoder-delay samples after the
//     untrimmed decode (exact middle, exhaustively checked in
//     TestGaplessIsExactSubstream);
//  2. the trimmed head/tail contain the same loud, structured signal as the
//     source WAV (matched by energy), rather than encoder padding;
//  3. a well-formed LAME tag reconstructs the exact source sample count.
func TestGaplessHeadAndTailWaveform(t *testing.T) {
	raw := openFixture(t, "lavc_cbr.mp3")
	wavBytes, err := os.ReadFile(filepath.Join("testdata", "gapless", "tone_stereo_ref.wav"))
	if err != nil {
		t.Fatal(err)
	}
	const wavHeader = 44
	ref := wavBytes[wavHeader:]

	d, gapPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})
	_, rawPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{})

	// (1) Head positioned exactly after the 576-sample encoder delay.
	const delay = 576
	const padding = 1404
	head := delay * bytesPerSample
	if !bytes.Equal(gapPCM[:1024], rawPCM[head:head+1024]) {
		t.Fatal("trimmed head is not positioned after the encoder delay")
	}

	// (2) Energy of head/tail windows must be comparable to the source WAV:
	// a factor-of-four bound catches both dead silence and gross corruption
	// while tolerating codec filtering and gain quantization. The head
	// window starts past MP3's first-frame filter-bank zero run so it
	// contains actual signal rather than the codec's internal ramp-up.
	const (
		winSamples = 1024
		headStart  = 2000
	)
	headWin := gapPCM[headStart*bytesPerSample : (headStart+winSamples)*bytesPerSample]
	refHead := ref[headStart*bytesPerSample : (headStart+winSamples)*bytesPerSample]
	if e := energy(headWin); e < energy(refHead)/4 {
		t.Fatalf("trimmed head energy %.0f far below reference %.0f", e, energy(refHead))
	}
	tail := winSamples * bytesPerSample
	if e := energy(gapPCM[len(gapPCM)-tail:]); e < energy(ref[len(ref)-tail:])/4 {
		t.Fatalf("trimmed tail energy far below reference")
	}
	// The encoder-delay region of the raw stream is silence, while later
	// trimmed samples are loud: trimming removed real encoder padding.
	if peakAmplitude(rawPCM[:delay*bytesPerSample]) != 0 {
		t.Errorf("encoder-delay region not silent in this fixture: peak %d",
			peakAmplitude(rawPCM[:delay*bytesPerSample]))
	}
	if peakAmplitude(headWin) < 1000 {
		t.Fatalf("head signal window is essentially silent, peak %d", peakAmplitude(headWin))
	}

	// (3) Exact source sample count reconstructed.
	if d.Length() != int64(len(ref)) {
		t.Fatalf("trimmed length %d != reference WAV PCM %d", d.Length(), len(ref))
	}
	if padding != 1404 {
		t.Fatal("test constant drift")
	}
}

func energy(b []byte) float64 {
	var sum float64
	for i := 0; i+1 < len(b); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(b[i : i+2])))
		sum += v * v
	}
	return sum
}

func peakAmplitude(b []byte) int {
	peak := 0
	for i := 0; i+1 < len(b); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(b[i : i+2])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

// TestGaplessSeekCoordinates exercises SeekStart/Current/End and verifies
// every seek lands on the same bytes a full sequential decode would produce.
func TestGaplessSeekCoordinates(t *testing.T) {
	raw := openFixture(t, "lame_patched.mp3")
	d, err := NewDecoderWithOptions(bytes.NewReader(raw), DecoderOptions{Gapless: true})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := io.ReadAll(d)
	if err != nil {
		t.Fatal(err)
	}
	length := d.Length()

	// Offsets chosen to cut across frames and the 1445-sample delay region.
	offsets := []int64{
		0, 4, 8, 100, 1445 * bytesPerSample, 1445*bytesPerSample + 4,
		4608, 4608 + 12, length - bytesPerSample, length - 8, length,
	}
	for _, off := range offsets {
		t.Run("", func(t *testing.T) {
			pos, err := d.Seek(off, io.SeekStart)
			if err != nil {
				t.Fatalf("SeekStart(%d): %v", off, err)
			}
			if pos != off {
				t.Fatalf("SeekStart returned %d want %d", pos, off)
			}
			cur, err := d.Seek(0, io.SeekCurrent)
			if err != nil || cur != off {
				t.Fatalf("SeekCurrent: pos=%d err=%v want %d", cur, err, off)
			}
			want := length - off
			buf := make([]byte, 4096)
			n, err := io.ReadFull(d, buf)
			if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				t.Fatalf("ReadFull after seek: %v", err)
			}
			if int64(n) > want {
				t.Fatalf("read %d bytes past end (available %d)", n, want)
			}
			if !bytes.Equal(buf[:n], whole[off:off+int64(n)]) {
				t.Fatalf("data after SeekStart(%d) does not match sequential decode", off)
			}
		})
	}

	// SeekEnd with negative and zero offsets.
	for _, rel := range []int64{0, -bytesPerSample, -4608, -10000} {
		t.Run("", func(t *testing.T) {
			want := length + rel
			pos, err := d.Seek(rel, io.SeekEnd)
			if err != nil {
				t.Fatalf("SeekEnd(%d): %v", rel, err)
			}
			if pos != want {
				t.Fatalf("SeekEnd returned %d want %d", pos, want)
			}
			buf := make([]byte, 256)
			n, err := io.ReadFull(d, buf)
			if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
				t.Fatalf("ReadFull after SeekEnd: %v", err)
			}
			if !bytes.Equal(buf[:n], whole[want:want+int64(n)]) {
				t.Fatalf("data after SeekEnd(%d) mismatches sequential decode", rel)
			}
		})
	}

	// SeekCurrent with relative movement. Each read advances the decoder's
	// position, so the local cursor must track both seeks and reads.
	if _, err := d.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	moves := []int64{12, 4600, -8, 4096, -100}
	cursor := int64(0)
	for _, m := range moves {
		cursor += m
		pos, err := d.Seek(m, io.SeekCurrent)
		if err != nil {
			t.Fatalf("SeekCurrent(%d): %v", m, err)
		}
		if pos != cursor {
			t.Fatalf("after move %d: pos=%d want %d", m, pos, cursor)
		}
		buf := make([]byte, 16)
		n, _ := io.ReadFull(d, buf)
		if !bytes.Equal(buf[:n], whole[cursor:cursor+int64(n)]) {
			t.Fatalf("data at cursor %d mismatches sequential decode", cursor)
		}
		cursor += int64(n)
	}
}

func TestGaplessSeekOutOfRange(t *testing.T) {
	raw := openFixture(t, "lame_patched.mp3")
	d, err := NewDecoderWithOptions(bytes.NewReader(raw), DecoderOptions{Gapless: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Seek(-1, io.SeekStart); err == nil {
		t.Fatal("negative SeekStart must fail")
	}
	if _, err := d.Seek(1, io.SeekEnd); err == nil {
		t.Fatal("SeekEnd past the end must fail")
	}
	if _, err := d.Seek(d.Length()+1, io.SeekStart); err == nil {
		t.Fatal("SeekStart past Length must fail")
	}
}

// forwardOnlyReader wraps a Reader so it does not expose io.Seeker even when
// the underlying value has one.
type forwardOnlyReader struct {
	r io.Reader
}

func (f *forwardOnlyReader) Read(p []byte) (int, error) { return f.r.Read(p) }

// TestGaplessNonSeekable makes the failure mode explicit: a forward-only
// source cannot support gapless trimming.
func TestGaplessNonSeekable(t *testing.T) {
	raw := openFixture(t, "lavc_cbr.mp3")
	if _, err := NewDecoderWithOptions(bytes.NewReader(raw), DecoderOptions{Gapless: true}); err != nil {
		t.Fatalf("bytes.Reader is seekable and should work: %v", err)
	}
	_, err := NewDecoderWithOptions(&forwardOnlyReader{bytes.NewReader(raw)}, DecoderOptions{Gapless: true})
	if !errors.Is(err, ErrGaplessNotSeekable) {
		t.Fatalf("got %v, want ErrGaplessNotSeekable", err)
	}

	// Without the option, non-seekable sources keep working as before.
	d, err := NewDecoder(&forwardOnlyReader{bytes.NewReader(raw)})
	if err != nil {
		t.Fatal(err)
	}
	if d.Length() != -1 {
		t.Fatalf("non-seekable Length = %d, want -1", d.Length())
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("non-seekable decode: %v", err)
	}
}

// TestGaplessOverTrimIsRejected covers metadata whose delay would consume the
// whole stream. The decoder must ignore such values and expose the untrimmed
// stream instead of returning an empty or inverted one.
func TestGaplessOverTrimIsRejected(t *testing.T) {
	raw := openFixture(t, "lame_overtrim_short.mp3")
	plain, plainPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{})
	gap, gapPCM := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})
	if gap.Length() != plain.Length() {
		t.Fatalf("over-trim must fall back: gap Length=%d plain=%d",
			gap.Length(), plain.Length())
	}
	if !bytes.Equal(gapPCM, plainPCM) {
		t.Fatal("over-trim fallback produced different PCM than plain decode")
	}
	if gap.Length() <= 0 || len(gapPCM) == 0 {
		t.Fatal("fallback stream is empty")
	}
}

// TestNoGaplessKeepsLegacyBehavior pins the untrimmed behavior for both the
// legacy constructor and an explicitly disabled option, including the two
// shipped example files that have no Xing/LAME tag.
func TestNoGaplessKeepsLegacyBehavior(t *testing.T) {
	names := []string{
		"lavc_cbr.mp3",
		"lame_patched.mp3",
		"lame_truncated.mp3",
		"lame_badmagic.mp3",
		"lame_overtrim.mp3",
	}
	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			raw := openFixture(t, name)
			d1, pcm1 := decodeAll(t, bytes.NewReader(raw), DecoderOptions{})
			d2, pcm2 := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: false})
			if d1.Length() != d2.Length() || !bytes.Equal(pcm1, pcm2) {
				t.Fatal("NewDecoder and Gapless:false diverged")
			}
			// Raw length is full-frame aligned.
			if d1.Length()%4608 != 0 && d1.SampleRate() == 44100 {
				t.Fatalf("MPEG1 raw length not frame aligned: %d", d1.Length())
			}
		})
	}

	for _, name := range []string{
		filepath.Join("example", "classic.mp3"),
		filepath.Join("example", "mpeg2.mp3"),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			plain, p1 := decodeAll(t, bytes.NewReader(raw), DecoderOptions{})
			gap, p2 := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})
			if plain.Length() != gap.Length() || !bytes.Equal(p1, p2) {
				t.Fatal("untagged file must decode identically with gapless on/off")
			}
		})
	}
}

// TestGaplessRepeatedSeeksReadEverything confirms seeks and sequential reads
// never lose samples across many random-looking repositionings.
func TestGaplessRepeatedSeeksReadEverything(t *testing.T) {
	raw := openFixture(t, "lavc_vbr.mp3")
	d, whole := decodeAll(t, bytes.NewReader(raw), DecoderOptions{Gapless: true})

	checkpoints := []int64{0, d.Length() / 3, d.Length() / 2, 2 * d.Length() / 3}
	seen := make(map[int64]bool)
	for _, cp := range checkpoints {
		// Round to a sample boundary.
		cp -= cp % bytesPerSample
		if _, err := d.Seek(cp, io.SeekStart); err != nil {
			t.Fatalf("seek to %d: %v", cp, err)
		}
		buf := make([]byte, 512)
		n, err := io.ReadFull(d, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			t.Fatalf("read at %d: %v", cp, err)
		}
		if !bytes.Equal(buf[:n], whole[cp:cp+int64(n)]) {
			t.Fatalf("mismatch at checkpoint %d", cp)
		}
		seen[cp] = true
	}
	if len(seen) != len(checkpoints) {
		t.Fatal("checkpoints collapsed")
	}
}
