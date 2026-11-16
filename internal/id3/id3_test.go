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

package id3

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"
)

func synchsafeBytes(n int) []byte {
	return []byte{byte(n>>21) & 0x7F, byte(n>>14) & 0x7F, byte(n>>7) & 0x7F, byte(n) & 0x7F}
}

type frameOpt func(body []byte) []byte

func rawFrame(id string, body []byte) []byte {
	b := make([]byte, 10, 10+len(body))
	copy(b, id)
	copy(b[4:8], synchsafeBytes(len(body)))
	return append(b, body...)
}

func v23Frame(id string, body []byte) []byte {
	b := make([]byte, 10, 10+len(body))
	copy(b, id)
	binary.BigEndian.PutUint32(b[4:8], uint32(len(body)))
	return append(b, body...)
}

func textFrame(title string) []byte {
	return rawFrame("TIT2", append([]byte{3}, []byte(title)...))
}

func v23TextFrame(title string) []byte {
	return v23Frame("TIT2", append([]byte{3}, []byte(title)...))
}

func chapBody(eid string, start, end uint32, title string) []byte {
	b := append([]byte(eid), 0)
	b = append(b, make([]byte, 4)...)
	binary.BigEndian.PutUint32(b[len(b)-4:], start)
	b = append(b, make([]byte, 4)...)
	binary.BigEndian.PutUint32(b[len(b)-4:], end)
	b = append(b, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF)
	if title != "" {
		b = append(b, textFrame(title)...)
	}
	return b
}

func ctocBody(eid string, entries []string, top, ordered bool, title string) []byte {
	b := append([]byte(eid), 0)
	flags := byte(0)
	if top {
		flags |= 0x02
	}
	if ordered {
		flags |= 0x01
	}
	b = append(b, flags, byte(len(entries)))
	for _, e := range entries {
		b = append(b, []byte(e)...)
		b = append(b, 0)
	}
	if title != "" {
		b = append(b, textFrame(title)...)
	}
	return b
}

// tag wraps frames in an ID3v2.4 header.
func tag(major byte, flags byte, frames ...[]byte) []byte {
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	h := []byte{'I', 'D', '3', major, 0, flags}
	h = append(h, synchsafeBytes(len(body))...)
	return append(h, body...)
}

func TestParseLinearChapters(t *testing.T) {
	tag := tag(4, 0,
		rawFrame("CHAP", chapBody("c2", 5000, 9000, "Second")),
		rawFrame("CHAP", chapBody("c1", 0, 5000, "First")),
		rawFrame("CHAP", chapBody("c3", 9000, 12000, "Third")),
	)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	want := []Chapter{
		{ID: "c1", Title: "First", StartMS: 0, EndMS: 5000},
		{ID: "c2", Title: "Second", StartMS: 5000, EndMS: 9000},
		{ID: "c3", Title: "Third", StartMS: 9000, EndMS: 12000},
	}
	if len(chapters) != len(want) {
		t.Fatalf("got %d chapters, want %d", len(chapters), len(want))
	}
	for i := range want {
		if chapters[i] != want[i] {
			t.Errorf("chapter %d: got %+v want %+v", i, chapters[i], want[i])
		}
	}
}

func TestParseOrderedChapters(t *testing.T) {
	tag := tag(4, 0,
		rawFrame("CHAP", chapBody("a", 0, 1000, "A")),
		rawFrame("CHAP", chapBody("b", 1000, 2000, "B")),
		rawFrame("CHAP", chapBody("c", 2000, 3000, "C")),
		rawFrame("CTOC", ctocBody("toc", []string{"c", "a", "b"}, true, true, "Contents")),
	)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range chapters {
		got = append(got, c.ID)
	}
	if strings.Join(got, ",") != "c,a,b" {
		t.Fatalf("order %v, want [c a b]", got)
	}
}

func TestParseNestedCTOC(t *testing.T) {
	tag := tag(4, 0,
		rawFrame("CHAP", chapBody("a", 0, 1000, "A")),
		rawFrame("CHAP", chapBody("b", 1000, 2000, "B")),
		rawFrame("CHAP", chapBody("c", 2000, 3000, "C")),
		rawFrame("CTOC", ctocBody("sub", []string{"b", "c"}, false, true, "Sub")),
		rawFrame("CTOC", ctocBody("root", []string{"a", "sub"}, true, true, "Root")),
	)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range chapters {
		got = append(got, c.ID)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("order %v, want [a b c]", got)
	}
}

func TestParseID3v23(t *testing.T) {
	frames := [][]byte{
		v23Frame("CHAP", chapBody("x", 100, 200, "X")),
		v23Frame("CTOC", ctocBody("t", []string{"x"}, true, true, "")),
	}
	var body []byte
	for _, f := range frames {
		body = append(body, f...)
	}
	h := []byte{'I', 'D', '3', 3, 0, 0}
	h = append(h, synchsafeBytes(len(body))...)
	chapters, err := ParseChapters(append(h, body...))
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 || chapters[0].Title != "X" || chapters[0].StartMS != 100 {
		t.Fatalf("got %+v", chapters)
	}
}

func TestParseNoChapters(t *testing.T) {
	for name, tag := range map[string][]byte{
		"no tag":    nil,
		"no frames": tag(4, 0),
		"only TIT2": tag(4, 0, textFrame("Song")),
		"padding":   tag(4, 0, textFrame("Song"), make([]byte, 100)),
		"v2.2":      tag(2, 0),
	} {
		t.Run(name, func(t *testing.T) {
			ch, err := ParseChapters(tag)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(ch) != 0 {
				t.Fatalf("got %d chapters, want none", len(ch))
			}
		})
	}
}

func TestParseTextEncodings(t *testing.T) {
	// Latin-1.
	latin1 := rawFrame("CHAP", func() []byte {
		b := chapBody("l", 0, 1, "")
		return append(b, rawFrame("TIT2", append([]byte{0}, []byte("Caf\xe9")...))...)
	}())
	// UTF-16 with a little-endian BOM.
	utf16le := rawFrame("CHAP", func() []byte {
		b := chapBody("u", 0, 1, "")
		body := []byte{1, 0xFF, 0xFE}
		for _, r := range "第二章" {
			body = append(body, byte(r), byte(r>>8))
		}
		return append(b, rawFrame("TIT2", body)...)
	}())
	chapters, err := ParseChapters(tag(4, 0, latin1, utf16le))
	if err != nil {
		t.Fatal(err)
	}
	if chapters[0].Title != "Café" {
		t.Errorf("latin-1 title %q", chapters[0].Title)
	}
	if chapters[1].Title != "第二章" {
		t.Errorf("utf-16 title %q", chapters[1].Title)
	}
}

func TestParseErrors(t *testing.T) {
	cases := map[string]struct {
		tag  []byte
		want error
	}{
		"not a tag": {
			tag:  []byte("NOPE000000000000"),
			want: ErrInvalidTag,
		},
		"unsupported version": {
			tag:  tag(5, 0),
			want: ErrUnsupportedVersion,
		},
		"non-synchsafe size": {
			tag:  []byte{'I', 'D', '3', 4, 0, 0, 0x80, 0, 0, 0},
			want: ErrInvalidTagSize,
		},
		"huge declared size": {
			tag:  []byte{'I', 'D', '3', 4, 0, 0, 0x7F, 0x7F, 0x7F, 0x7F},
			want: ErrInvalidTagSize,
		},
		"truncated tag": {
			tag:  tag(4, 0, rawFrame("CHAP", chapBody("a", 0, 1, "A")))[:20],
			want: ErrTruncatedTag,
		},
		"truncated frame": {
			tag: func() []byte {
				f := rawFrame("CHAP", chapBody("a", 0, 1, "A"))
				t := tag(4, 0, f)
				// Break the declared tag size so the frame itself is short.
				copy(t[6:10], synchsafeBytes(len(f)-3))
				return t
			}(),
			want: ErrTruncatedFrame,
		},
		"frame bigger than tag": {
			tag: func() []byte {
				f := rawFrame("CHAP", chapBody("a", 0, 1, "A"))
				// Claim far more data than the frame body holds.
				copy(f[4:8], synchsafeBytes(1<<20))
				return tag(4, 0, f)
			}(),
			want: ErrTruncatedFrame,
		},
		"missing element id terminator": {
			tag:  tag(4, 0, rawFrame("CHAP", []byte("unterminated"))),
			want: ErrInvalidChapter,
		},
		"empty element id": {
			tag:  tag(4, 0, rawFrame("CHAP", append([]byte{0}, make([]byte, 16)...))),
			want: ErrInvalidChapter,
		},
		"short chapter body": {
			tag:  tag(4, 0, rawFrame("CHAP", append([]byte("a\x00"), 1, 2, 3))),
			want: ErrTruncatedFrame,
		},
		"start after end": {
			tag:  tag(4, 0, rawFrame("CHAP", chapBody("a", 5000, 1000, "A"))),
			want: ErrInvalidTimeRange,
		},
		"duplicate chapter id": {
			tag: tag(4, 0,
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
				rawFrame("CHAP", chapBody("a", 1, 2, "B")),
			),
			want: ErrDuplicateID,
		},
		"duplicate ctoc id": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t", []string{"a"}, true, true, "")),
				rawFrame("CTOC", ctocBody("t", []string{"a"}, false, true, "")),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
			),
			want: ErrDuplicateID,
		},
		"duplicate across types": {
			tag: tag(4, 0,
				rawFrame("CHAP", chapBody("x", 0, 1, "A")),
				rawFrame("CTOC", ctocBody("x", []string{"x"}, true, true, "")),
			),
			want: ErrDuplicateID,
		},
		"ctoc with no entries": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t", nil, true, true, "")),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
			),
			want: ErrInvalidChapter,
		},
		"ctoc unterminated child": {
			tag: tag(4, 0,
				rawFrame("CTOC", append(append([]byte("t\x00"), 0x02, 0x02), []byte("a\x00")...)),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
			),
			want: ErrTruncatedFrame,
		},
		"dangling ctoc reference": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t", []string{"missing"}, true, true, "")),
			),
			want: ErrInvalidChapter,
		},
		"self-referencing ctoc": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t", []string{"t"}, true, true, "")),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
			),
			want: ErrChapterCycle,
		},
		"mutually referencing ctocs": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t1", []string{"t2"}, true, true, "")),
				rawFrame("CTOC", ctocBody("t2", []string{"t1"}, false, true, "")),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
			),
			want: ErrChapterCycle,
		},
		"two top-level ctocs": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t1", []string{"a"}, true, true, "")),
				rawFrame("CTOC", ctocBody("t2", []string{"b"}, true, true, "")),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
				rawFrame("CHAP", chapBody("b", 1, 2, "B")),
			),
			want: ErrInvalidChapter,
		},
		"no top-level ctoc": {
			tag: tag(4, 0,
				rawFrame("CTOC", ctocBody("t1", []string{"a"}, false, true, "")),
				rawFrame("CTOC", ctocBody("t2", []string{"b"}, false, true, "")),
				rawFrame("CHAP", chapBody("a", 0, 1, "A")),
				rawFrame("CHAP", chapBody("b", 1, 2, "B")),
			),
			want: ErrInvalidChapter,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ch, err := ParseChapters(tc.tag)
			if !errors.Is(err, tc.want) {
				t.Fatalf("got err %v (chapters %v), want %v", err, ch, tc.want)
			}
		})
	}
}

func TestParseChaptersNotInTOC(t *testing.T) {
	// Chapters the table of contents does not mention are still returned, in
	// start-time order after the referenced ones.
	tag := tag(4, 0,
		rawFrame("CHAP", chapBody("a", 0, 1000, "A")),
		rawFrame("CHAP", chapBody("b", 1000, 2000, "B")),
		rawFrame("CHAP", chapBody("orphan", 2000, 3000, "Orphan")),
		rawFrame("CTOC", ctocBody("toc", []string{"b", "a"}, true, true, "")),
	)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, c := range chapters {
		got = append(got, c.ID)
	}
	if strings.Join(got, ",") != "b,a,orphan" {
		t.Fatalf("order %v, want [b a orphan]", got)
	}
}

func TestParseOrphanedCTOCErrorsAreReported(t *testing.T) {
	// A cycle in a table not reachable from the root must still be reported.
	cyclic := tag(4, 0,
		rawFrame("CHAP", chapBody("a", 0, 1, "A")),
		rawFrame("CTOC", ctocBody("root", []string{"a"}, true, true, "")),
		rawFrame("CTOC", ctocBody("x", []string{"y"}, false, true, "")),
		rawFrame("CTOC", ctocBody("y", []string{"x"}, false, true, "")),
	)
	if _, err := ParseChapters(cyclic); !errors.Is(err, ErrChapterCycle) {
		t.Fatalf("got %v, want ErrChapterCycle", err)
	}

	// Likewise for a dangling reference.
	tag2 := tag(4, 0,
		rawFrame("CHAP", chapBody("a", 0, 1, "A")),
		rawFrame("CTOC", ctocBody("root", []string{"a"}, true, true, "")),
		rawFrame("CTOC", ctocBody("x", []string{"nope"}, false, true, "")),
	)
	if _, err := ParseChapters(tag2); !errors.Is(err, ErrInvalidChapter) {
		t.Fatalf("got %v, want ErrInvalidChapter", err)
	}
}

func TestParseSingleUntaggedCTOC(t *testing.T) {
	// A single CTOC without the top-level bit is tolerated.
	tag := tag(4, 0,
		rawFrame("CTOC", ctocBody("t", []string{"b", "a"}, false, true, "")),
		rawFrame("CHAP", chapBody("a", 0, 1, "A")),
		rawFrame("CHAP", chapBody("b", 1, 2, "B")),
	)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 || chapters[0].ID != "b" || chapters[1].ID != "a" {
		t.Fatalf("got %+v", chapters)
	}
}

func TestParseV24Unsynchronisation(t *testing.T) {
	// A chapter title containing 0xFF is stored unsynchronised as 0xFF 0x00.
	sub := rawFrame("TIT2", append([]byte{0}, []byte("A\xffB")...))
	f := rawFrame("CHAP", append(chapBody("a", 0, 1, ""), sub...))
	// Naively unsynchronise the frame data region.
	unsynced := append([]byte{}, f[:10]...)
	for _, c := range f[10:] {
		unsynced = append(unsynced, c)
		if c == 0xFF {
			unsynced = append(unsynced, 0x00)
		}
	}
	// Mark the frame with the v2.4 unsynchronisation flag. The frame size in
	// the header covers the unsynchronised (stored) bytes.
	unsynced[9] |= 0x02
	copy(unsynced[4:8], synchsafeBytes(len(unsynced)-10))
	tag := tag(4, 0, unsynced)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 || chapters[0].Title != "AÿB" {
		t.Fatalf("got %+v", chapters)
	}
}

func TestParseExtendedHeader(t *testing.T) {
	// v2.4 extended header: six bytes including the size field.
	ext := []byte{0, 0, 0, 6, 1, 0}
	frames := rawFrame("CHAP", chapBody("a", 0, 1, "A"))
	body := append(ext, frames...)
	h := []byte{'I', 'D', '3', 4, 0, 0x40}
	h = append(h, synchsafeBytes(len(body))...)
	chapters, err := ParseChapters(append(h, body...))
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 || chapters[0].Title != "A" {
		t.Fatalf("got %+v", chapters)
	}

	// Truncated extended header.
	body = []byte{0, 0, 0, 6, 1, 0}
	h = []byte{'I', 'D', '3', 4, 0, 0x40}
	h = append(h, synchsafeBytes(4)...)
	if _, err := ParseChapters(append(h, body[:4]...)); !errors.Is(err, ErrTruncatedTag) {
		t.Fatalf("got %v, want ErrTruncatedTag", err)
	}
}

func TestParseRejectsDuplicateChapterBytes(t *testing.T) {
	// Guards the parser against trusting a frame size that repeats data.
	a := rawFrame("CHAP", chapBody("a", 0, 1, "A"))
	b := rawFrame("CHAP", chapBody("b", 1, 2, "B"))
	tag := tag(4, 0, a, b)
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 {
		t.Fatalf("got %d chapters", len(chapters))
	}
}

func TestParsePaddingStopsIteration(t *testing.T) {
	tag := tag(4, 0, rawFrame("CHAP", chapBody("a", 0, 1, "A")), bytes.Repeat([]byte{0}, 64))
	chapters, err := ParseChapters(tag)
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 1 {
		t.Fatalf("got %d chapters", len(chapters))
	}
}
