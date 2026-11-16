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
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/hajimehoshi/go-mp3/internal/id3"
)

func TestSourceRejectsBrokenTagSizes(t *testing.T) {
	plain := readChapterFixture(t, "two_chapters_noid3.mp3")
	cases := map[string]struct {
		header []byte
		want   error
	}{
		"non-synchsafe": {
			header: []byte{'I', 'D', '3', 4, 0, 0, 0x80, 0, 0, 0},
			want:   id3.ErrInvalidTagSize,
		},
		"absurd size": {
			header: []byte{'I', 'D', '3', 4, 0, 0, 0x7F, 0x7F, 0x7F, 0x7F},
			want:   id3.ErrInvalidTagSize,
		},
		"truncated body": {
			// Declares 1 MiB but the stream holds a few hundred bytes.
			header: []byte{'I', 'D', '3', 4, 0, 0, 0x00, 0x40, 0x00, 0x00},
			want:   id3.ErrTruncatedTag,
		},
		"footer flag on v2.3": {
			header: []byte{'I', 'D', '3', 3, 0, 0x10, 0, 0, 0, 0},
			want:   id3.ErrInvalidTag,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stream := append(append([]byte{}, tc.header...), plain...)
			if _, err := NewDecoder(bytes.NewReader(stream)); !errors.Is(err, tc.want) {
				t.Fatalf("NewDecoder error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestSourceTruncatedTagAtEnd(t *testing.T) {
	// A tag header claiming more data than the stream holds must not make the
	// decoder allocate the claimed size or read on into nothing.
	header := []byte{'I', 'D', '3', 4, 0, 0, 0x00, 0x00, 0x01, 0x00} // 128 bytes
	stream := append(header, make([]byte, 10)...)
	_, err := NewDecoder(bytes.NewReader(stream))
	if !errors.Is(err, id3.ErrTruncatedTag) {
		t.Fatalf("got %v, want ErrTruncatedTag", err)
	}
}

func TestSourceTagAllocationBoundedByStream(t *testing.T) {
	// A short stream declaring a multi-megabyte tag must fail without
	// reserving the claimed size.
	const declared = 4 << 20
	header := []byte{
		'I', 'D', '3', 4, 0, 0,
		byte((declared >> 21) & 0x7F), byte((declared >> 14) & 0x7F),
		byte((declared >> 7) & 0x7F), byte(declared & 0x7F),
	}
	stream := append(header, make([]byte, 1024)...)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := NewDecoder(bytes.NewReader(stream))
	runtime.ReadMemStats(&after)

	if !errors.Is(err, id3.ErrTruncatedTag) {
		t.Fatalf("got %v, want ErrTruncatedTag", err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 1<<20 {
		t.Fatalf("allocated %d bytes for a 1 KiB input claiming a %d-byte tag", allocated, declared)
	}
}

func TestSourceNonSeekableWithChapters(t *testing.T) {
	// Chapter parsing must work for forward-only streams too; only Seek needs
	// an io.Seeker.
	raw := readChapterFixture(t, "two_chapters.mp3")
	d, err := NewDecoder(&forwardOnlyReader{bytes.NewReader(raw)})
	if err != nil {
		t.Fatal(err)
	}
	chapters, err := d.Chapters()
	if err != nil {
		t.Fatal(err)
	}
	if len(chapters) != 2 {
		t.Fatalf("got %d chapters, want 2", len(chapters))
	}
	if d.Length() != invalidLength {
		t.Fatalf("non-seekable Length = %d, want %d", d.Length(), invalidLength)
	}
	if _, err := d.SeekChapter(0); err == nil {
		t.Fatal("SeekChapter on a non-seekable source must fail")
	}
	if _, err := io.Copy(io.Discard, d); err != nil {
		t.Fatalf("decode: %v", err)
	}
}

type forwardOnlyReader struct {
	r io.Reader
}

func (f *forwardOnlyReader) Read(p []byte) (int, error) { return f.r.Read(p) }
