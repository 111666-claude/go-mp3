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

	"github.com/hajimehoshi/go-mp3/internal/id3"
)

func readChapterFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "chapters", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// dominantFrequency estimates the strongest frequency of 16-bit stereo PCM
// samples by counting zero crossings. Both channels are identical in the
// fixtures, so only the left one is examined.
func dominantFrequency(pcm []byte) float64 {
	if len(pcm) < 4 {
		return 0
	}
	crossings := 0
	prev := int16(binary.LittleEndian.Uint16(pcm[0:2]))
	for i := 4; i+1 < len(pcm); i += 4 {
		v := int16(binary.LittleEndian.Uint16(pcm[i : i+2]))
		if (prev < 0) != (v < 0) {
			crossings++
		}
		prev = v
	}
	samples := float64(len(pcm) / 4)
	return float64(crossings) / 2 * 44100 / samples
}

// peakAmplitude returns the largest absolute sample value in 16-bit stereo
// PCM.
func peakAmplitude(pcm []byte) int {
	peak := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(binary.LittleEndian.Uint16(pcm[i : i+2])))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

func TestChaptersFromFixture(t *testing.T) {
	raw := readChapterFixture(t, "two_chapters.mp3")
	d, err := NewDecoder(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if d.SampleRate() != 44100 {
		t.Fatalf("sample rate %d", d.SampleRate())
	}
	chapters, err := d.Chapters()
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 {
		t.Fatalf("got %d chapters, want 2", len(chapters))
	}
	want := []Chapter{
		{ID: "ch1", Title: "Chapter One", StartMS: 0, EndMS: 1200},
		{ID: "ch2", Title: "Chapter Two", StartMS: 1200, EndMS: 2400},
	}
	if chapters[0].ID != want[0].ID || chapters[0].Title != want[0].Title ||
		chapters[0].StartMS != want[0].StartMS || chapters[0].EndMS != want[0].EndMS {
		t.Errorf("chapter 0 = %+v, want %+v", chapters[0], want[0])
	}
	if chapters[1].ID != want[1].ID || chapters[1].Title != want[1].Title ||
		chapters[1].StartMS != want[1].StartMS || chapters[1].EndMS != want[1].EndMS {
		t.Errorf("chapter 1 = %+v, want %+v", chapters[1], want[1])
	}
	// 44.1 kHz means one millisecond is 44.1 samples, rounded down to whole
	// samples (176.4 bytes per ms); chapter positions are always on a sample
	// boundary.
	if chapters[0].StartPCM != 0 {
		t.Errorf("chapter 0 start = %d, want 0", chapters[0].StartPCM)
	}
	if chapters[0].EndPCM != 1200*44100/1000*4 {
		t.Errorf("chapter 0 end = %d", chapters[0].EndPCM)
	}
	if chapters[1].StartPCM != 1200*44100/1000*4 {
		t.Errorf("chapter 1 start = %d", chapters[1].StartPCM)
	}
	if chapters[1].EndPCM != 2400*44100/1000*4 {
		t.Errorf("chapter 1 end = %d", chapters[1].EndPCM)
	}
	for i, c := range chapters {
		if c.StartPCM%4 != 0 || c.EndPCM%4 != 0 {
			t.Errorf("chapter %d is not on a sample boundary: %+v", i, c)
		}
	}
	if chapters[0].EndPCM != chapters[1].StartPCM {
		t.Error("chapters are not contiguous")
	}
	if chapters[1].EndPCM > d.Length() {
		t.Errorf("last chapter end %d beyond length %d", chapters[1].EndPCM, d.Length())
	}
}

func TestChaptersSeekWaveform(t *testing.T) {
	raw := readChapterFixture(t, "two_chapters.mp3")
	whole, err := io.ReadAll(func() io.Reader {
		d, err := NewDecoder(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}())
	if err != nil {
		t.Fatal(err)
	}

	// Each chapter carries a different tone. Measure well inside each chapter
	// so the codec's filter-bank warm-up does not skew the estimate.
	const warmup = 8000 // bytes, 2000 stereo samples
	for i, wantHz := range []float64{440, 880} {
		d, err := NewDecoder(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		chapters, err := d.Chapters()
		if err != nil {
			t.Fatal(err)
		}
		pos, err := d.SeekChapter(i)
		if err != nil {
			t.Fatalf("SeekChapter(%d): %v", i, err)
		}
		if pos != chapters[i].StartPCM {
			t.Fatalf("SeekChapter(%d) = %d, want %d", i, pos, chapters[i].StartPCM)
		}
		buf := make([]byte, warmup+44100*4/10) // warm-up plus 100 ms
		if _, err := io.ReadFull(d, buf); err != nil {
			t.Fatalf("read after SeekChapter(%d): %v", i, err)
		}
		window := buf[warmup:]
		if got := dominantFrequency(window); got < wantHz*0.9 || got > wantHz*1.1 {
			t.Errorf("chapter %d tone: got %.0f Hz, want %.0f Hz", i, got, wantHz)
		}
		if peakAmplitude(window) < 1000 {
			t.Errorf("chapter %d window is silent", i)
		}
		// Seeking must not change the samples relative to a full decode.
		if !bytes.Equal(buf, whole[pos:pos+int64(len(buf))]) {
			t.Errorf("chapter %d: data after SeekChapter differs from sequential decode", i)
		}
	}
}

func TestChaptersSequentialDecodeUnchanged(t *testing.T) {
	// Chapter metadata must not alter the decoded audio: the same bytes with
	// and without an ID3 tag decode identically.
	tagged := readChapterFixture(t, "two_chapters.mp3")
	plain := readChapterFixture(t, "two_chapters_noid3.mp3")

	dTagged, err := NewDecoder(bytes.NewReader(tagged))
	if err != nil {
		t.Fatal(err)
	}
	dPlain, err := NewDecoder(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if dTagged.Length() != dPlain.Length() {
		t.Fatalf("lengths differ: tagged %d plain %d", dTagged.Length(), dPlain.Length())
	}
	taggedPCM, err := io.ReadAll(dTagged)
	if err != nil {
		t.Fatal(err)
	}
	plainPCM, err := io.ReadAll(dPlain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(taggedPCM, plainPCM) {
		t.Fatal("ID3 tag changed the decoded audio")
	}
	chapters, err := dPlain.Chapters()
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 0 {
		t.Fatalf("untagged file reports %d chapters", len(chapters))
	}
}

func TestSeekChapterBounds(t *testing.T) {
	raw := readChapterFixture(t, "two_chapters.mp3")
	d, err := NewDecoder(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []int{-1, 2, 100} {
		if _, err := d.SeekChapter(i); !errors.Is(err, ErrNoSuchChapter) {
			t.Errorf("SeekChapter(%d) error = %v, want ErrNoSuchChapter", i, err)
		}
	}
}

func TestChaptersNilWithoutTag(t *testing.T) {
	for _, name := range []string{
		filepath.Join("example", "classic.mp3"),
		filepath.Join("example", "mpeg2.mp3"),
		filepath.Join("testdata", "chapters", "two_chapters_noid3.mp3"),
	} {
		t.Run(name, func(t *testing.T) {
			raw, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			d, err := NewDecoder(bytes.NewReader(raw))
			if err != nil {
				t.Fatal(err)
			}
			chapters, err := d.Chapters()
			if err != nil {
				t.Fatalf("Chapters: %v", err)
			}
			if len(chapters) != 0 {
				t.Fatalf("got %d chapters, want none", len(chapters))
			}
			if _, err := d.SeekChapter(0); !errors.Is(err, ErrNoSuchChapter) {
				t.Fatalf("SeekChapter(0) = %v, want ErrNoSuchChapter", err)
			}
			if _, err := io.ReadAll(d); err != nil {
				t.Fatalf("decode: %v", err)
			}
		})
	}
}

// tagWithPatch returns the two-chapter fixture with its ID3 tag replaced by
// the result of patch, which receives a copy of the tag.
func tagWithPatch(t *testing.T, patch func(tag []byte) []byte) []byte {
	t.Helper()
	raw := readChapterFixture(t, "two_chapters.mp3")
	size, _ := id3.Synchsafe(raw[6:10])
	tagLen := 10 + int(size)
	newTag := patch(append([]byte{}, raw[:tagLen]...))
	return append(newTag, raw[tagLen:]...)
}

func TestChaptersBrokenTagStillDecodes(t *testing.T) {
	plain := readChapterFixture(t, "two_chapters_noid3.mp3")
	want, err := io.ReadAll(func() io.Reader {
		d, err := NewDecoder(bytes.NewReader(plain))
		if err != nil {
			t.Fatal(err)
		}
		return d
	}())
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string]struct {
		patch func(tag []byte) []byte
		want  error
	}{
		"inverted time range": {
			patch: func(tag []byte) []byte {
				// The first CHAP's start time is stored after the CHAP frame ID,
				// its 4-byte size and 2 flag bytes, the 4-byte Element ID and a
				// NUL terminator.
				i := bytes.Index(tag, []byte("CHAP"))
				binary.BigEndian.PutUint32(tag[i+10+len("ch2")+1:], 3000)
				return tag
			},
			want: id3.ErrInvalidTimeRange,
		},
		"duplicate element id": {
			patch: func(tag []byte) []byte {
				i := bytes.Index(tag, []byte("CHAP"))
				j := bytes.Index(tag[i+4:], []byte("CHAP")) + i + 4
				copy(tag[j+10:], []byte("ch2"))
				return tag
			},
			want: id3.ErrDuplicateID,
		},
		"truncated frame": {
			patch: func(tag []byte) []byte {
				i := bytes.Index(tag, []byte("CHAP"))
				// Claim the frame body is 1 MiB.
				copy(tag[i+4:i+8], []byte{0x3f, 0x7f, 0x7f, 0x00})
				return tag
			},
			want: id3.ErrTruncatedFrame,
		},
		"cyclic ctoc": {
			patch: func(tag []byte) []byte {
				// Point the TOC's first entry at itself.
				i := bytes.Index(tag, []byte("CTOC"))
				// Element ID "toc\0", flags, count, then "ch1\0".
				entry := i + 10 + len("toc") + 1 + 2
				copy(tag[entry:], []byte("toc"))
				return tag
			},
			want: id3.ErrChapterCycle,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			raw := tagWithPatch(t, tc.patch)
			d, err := NewDecoder(bytes.NewReader(raw))
			if err != nil {
				t.Fatalf("broken chapter metadata must not stop decoding: %v", err)
			}
			chapters, err := d.Chapters()
			if !errors.Is(err, tc.want) {
				t.Fatalf("Chapters error = %v, want %v", err, tc.want)
			}
			if chapters != nil {
				t.Fatalf("Chapters returned %d chapters alongside an error", len(chapters))
			}
			if _, err := d.SeekChapter(0); !errors.Is(err, tc.want) {
				t.Fatalf("SeekChapter error = %v, want %v", err, tc.want)
			}
			got, err := io.ReadAll(d)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !bytes.Equal(got, want) {
				t.Fatal("broken chapter tag changed the decoded audio")
			}
		})
	}
}

func TestChaptersBeyondAudioLength(t *testing.T) {
	// Stretch the last chapter far beyond the audio. ch2 is the first CHAP in
	// the fixture, so its start and end are the first pair of times.
	patched := tagWithPatch(t, func(tag []byte) []byte {
		i := bytes.Index(tag, []byte("CHAP"))
		binary.BigEndian.PutUint32(tag[i+10+len("ch2")+1:], 100)
		binary.BigEndian.PutUint32(tag[i+10+len("ch2")+1+4:], 600000)
		return tag
	})
	d, err := NewDecoder(bytes.NewReader(patched))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Chapters(); !errors.Is(err, id3.ErrChapterTimeOutOfRange) {
		t.Fatalf("Chapters error = %v, want ErrChapterTimeOutOfRange", err)
	}
	if _, err := d.SeekChapter(0); !errors.Is(err, id3.ErrChapterTimeOutOfRange) {
		t.Fatalf("SeekChapter error = %v, want ErrChapterTimeOutOfRange", err)
	}
	if _, err := io.ReadAll(d); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

func TestChaptersDoNotBreakRegularSeek(t *testing.T) {
	// Ordinary Seek must behave the same whether or not the stream carries a
	// chapter tag: the tag is skipped before the frame scan, so positions and
	// returned bytes must match the tagless file exactly.
	tagged := readChapterFixture(t, "two_chapters.mp3")
	plain := readChapterFixture(t, "two_chapters_noid3.mp3")
	dTagged, err := NewDecoder(bytes.NewReader(tagged))
	if err != nil {
		t.Fatal(err)
	}
	dPlain, err := NewDecoder(bytes.NewReader(plain))
	if err != nil {
		t.Fatal(err)
	}
	if dTagged.Length() != dPlain.Length() {
		t.Fatalf("lengths differ: tagged %d plain %d", dTagged.Length(), dPlain.Length())
	}
	for _, off := range []int64{0, 4, 4608, 9216, 100000, 211680, dPlain.Length() - 4} {
		pTagged, err := dTagged.Seek(off, io.SeekStart)
		if err != nil {
			t.Fatalf("Seek(%d) on tagged file: %v", off, err)
		}
		pPlain, err := dPlain.Seek(off, io.SeekStart)
		if err != nil {
			t.Fatalf("Seek(%d) on plain file: %v", off, err)
		}
		if pTagged != pPlain || pTagged != off {
			t.Fatalf("Seek(%d) returned %d (tagged) / %d (plain)", off, pTagged, pPlain)
		}
		// The special-case SeekCurrent query must report the same position.
		if cur, err := dTagged.Seek(0, io.SeekCurrent); err != nil || cur != off {
			t.Fatalf("SeekCurrent after Seek(%d) = %d, %v", off, cur, err)
		}
		bufTagged := make([]byte, 64)
		nTagged, err := io.ReadFull(dTagged, bufTagged)
		if err != nil && err != io.ErrUnexpectedEOF {
			t.Fatalf("read at %d: %v", off, err)
		}
		bufPlain := make([]byte, 64)
		nPlain, err := io.ReadFull(dPlain, bufPlain)
		if err != nil && err != io.ErrUnexpectedEOF {
			t.Fatalf("read at %d: %v", off, err)
		}
		if nTagged != nPlain || !bytes.Equal(bufTagged[:nTagged], bufPlain[:nPlain]) {
			t.Fatalf("data at %d differs between tagged and tagless files", off)
		}
	}
}
