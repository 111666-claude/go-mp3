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
	"io/ioutil"
	"strings"
	"testing"
)

// readerOnly hides the io.Seeker (and io.ByteReader) implementation of the
// underlying reader, simulating a plain network-stream-like io.Reader.
type readerOnly struct {
	r io.Reader
}

func (r *readerOnly) Read(buf []byte) (int, error) {
	return r.r.Read(buf)
}

// chunkReader limits every Read to at most n bytes, simulating an
// underlying transport that delivers tiny chunks.
type chunkReader struct {
	r io.Reader
	n int
}

func (c *chunkReader) Read(buf []byte) (int, error) {
	if len(buf) > c.n {
		buf = buf[:c.n]
	}
	return c.r.Read(buf)
}

// zeroReader always returns (0, nil), which must not block the decoder.
type zeroReader struct{}

func (zeroReader) Read(buf []byte) (int, error) {
	return 0, nil
}

// failReader returns err once more than `after` bytes have been read.
type failReader struct {
	r     io.Reader
	after int
	read  int
	err   error
}

func (f *failReader) Read(buf []byte) (int, error) {
	if f.read >= f.after {
		return 0, f.err
	}
	n, err := f.r.Read(buf)
	f.read += n
	return n, err
}

func decodeAll(t *testing.T, r io.Reader) []byte {
	t.Helper()
	pcm, err := ioutil.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return pcm
}

func readTestFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := ioutil.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// TestStreamDecoderMatchesDecoder checks that a stream read in 1-byte
// chunks from a non-seekable reader produces exactly the same PCM as the
// existing Decoder reading the complete file.
func TestStreamDecoderMatchesDecoder(t *testing.T) {
	for _, name := range []string{"example/classic.mp3", "example/mpeg2.mp3"} {
		data := readTestFile(t, name)

		d, err := NewDecoder(bytes.NewReader(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := decodeAll(t, d)

		sd, err := NewStreamDecoder(&chunkReader{r: &readerOnly{r: bytes.NewReader(data)}, n: 1})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := decodeAll(t, sd)

		if !bytes.Equal(got, want) {
			t.Fatalf("%s: PCM mismatch: stream decoded %d bytes, decoder decoded %d bytes", name, len(got), len(want))
		}
		if sd.SampleRate() != d.SampleRate() {
			t.Fatalf("%s: sample rate mismatch: stream %d, decoder %d", name, sd.SampleRate(), d.SampleRate())
		}
		if got, want := sd.InputOffset(), int64(len(data)); got != want {
			t.Fatalf("%s: input offset after EOF: got %d, want %d", name, got, want)
		}
	}
}

// TestStreamDecoderLengthAndSeek checks the length and seek semantics of a
// stream whose total size is unknown.
func TestStreamDecoderLengthAndSeek(t *testing.T) {
	data := readTestFile(t, "example/classic.mp3")
	sd, err := NewStreamDecoder(&readerOnly{r: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	if got := sd.Length(); got != -1 {
		t.Fatalf("Length: got %d, want -1", got)
	}
	if _, err := sd.Seek(0, io.SeekStart); err == nil {
		t.Fatal("Seek(0, io.SeekStart) must fail on a StreamDecoder")
	}
	if _, err := sd.Seek(0, io.SeekEnd); err == nil {
		t.Fatal("Seek(0, io.SeekEnd) must fail on a StreamDecoder")
	}
	// The no-op position query keeps working and tracks decoded bytes.
	pos, err := sd.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("initial position: got %d, want 0", pos)
	}
	buf := make([]byte, 4096)
	n, err := sd.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	pos, err = sd.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != int64(n) {
		t.Fatalf("position after Read: got %d, want %d", pos, n)
	}
}

func makeID3v2Tag(payload []byte) []byte {
	size := len(payload)
	if size >= 1<<28 {
		panic("payload too large for a syncsafe size")
	}
	tag := []byte{'I', 'D', '3', 4, 0, 0,
		byte(size >> 21), byte(size >> 14), byte(size >> 7), byte(size)}
	return append(tag, payload...)
}

// TestStreamDecoderID3v2 checks that an ID3v2 tag is skipped transparently
// and the PCM still matches the untagged stream.
func TestStreamDecoderID3v2(t *testing.T) {
	data := readTestFile(t, "example/classic.mp3")

	want, err := NewStreamDecoder(&readerOnly{r: bytes.NewReader(data)})
	if err != nil {
		t.Fatal(err)
	}
	wantPCM := decodeAll(t, want)

	payload := bytes.Repeat([]byte("go-mp3 streaming test "), 100)
	tagged := append(makeID3v2Tag(payload), data...)
	sd, err := NewStreamDecoder(&chunkReader{r: &readerOnly{r: bytes.NewReader(tagged)}, n: 7})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeAll(t, sd)
	if !bytes.Equal(got, wantPCM) {
		t.Fatalf("PCM with ID3v2 tag differs: got %d bytes, want %d bytes", len(got), len(wantPCM))
	}
	if got, want := sd.InputOffset(), int64(len(tagged)); got != want {
		t.Fatalf("input offset after EOF: got %d, want %d", got, want)
	}
}

// TestStreamDecoderOversizedID3v2Tag checks that a tag declaring an absurd
// size is rejected immediately with a located error, instead of allocating
// or waiting for hundreds of megabytes.
func TestStreamDecoderOversizedID3v2Tag(t *testing.T) {
	tag := []byte{'I', 'D', '3', 4, 0, 0, 0x7f, 0x7f, 0x7f, 0x7f} // ~256 MiB
	_, err := NewStreamDecoder(&readerOnly{r: bytes.NewReader(tag)})
	if err == nil {
		t.Fatal("expected an error for an oversized ID3v2 tag")
	}
	if !strings.Contains(err.Error(), "exceeds the limit") {
		t.Fatalf("unexpected error: %v", err)
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error must be locatable as *StreamError: %v", err)
	}
	if streamErr.Offset != 10 {
		t.Fatalf("error offset: got %d, want 10", streamErr.Offset)
	}
}

// TestStreamDecoderTruncated checks that a stream ending in the middle of
// a frame terminates with io.EOF instead of an error or a hang, and that
// the PCM decoded so far is a prefix of the complete PCM.
func TestStreamDecoderTruncated(t *testing.T) {
	data := readTestFile(t, "example/classic.mp3")

	d, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	full := decodeAll(t, d)

	// Cut 10 bytes into the last frame. Note that the file has trailing
	// padding and an ID3v1 tag after the last frame, so cutting a fixed
	// number of bytes from the end would not necessarily truncate a frame.
	if len(d.frameStarts) == 0 {
		t.Fatal("no frames found")
	}
	lastFrameStart := d.frameStarts[len(d.frameStarts)-1]
	truncated := data[:lastFrameStart+10]

	sd, err := NewStreamDecoder(&readerOnly{r: bytes.NewReader(truncated)})
	if err != nil {
		t.Fatal(err)
	}
	got := decodeAll(t, sd) // ReadAll turns the final io.EOF into a nil error.

	if len(got) >= len(full) {
		t.Fatalf("truncated stream decoded %d bytes, complete stream %d bytes", len(got), len(full))
	}
	if !bytes.Equal(got, full[:len(got)]) {
		t.Fatal("PCM of the truncated stream is not a prefix of the complete PCM")
	}
}

// TestStreamDecoderBadFrameHeader checks that an invalid frame header is
// reported with the input offset at which decoding failed.
func TestStreamDecoderBadFrameHeader(t *testing.T) {
	// 0xff 0xfb 0x00 ...: valid sync bits but bitrate index 0 (free
	// format), which is not supported.
	data := append([]byte{0xff, 0xfb, 0x00, 0x00}, bytes.Repeat([]byte{0}, 100)...)
	_, err := NewStreamDecoder(&readerOnly{r: bytes.NewReader(data)})
	if err == nil {
		t.Fatal("expected an error for a bad frame header")
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error must be locatable as *StreamError: %v", err)
	}
	if streamErr.Offset != 0 {
		t.Fatalf("error offset: got %d, want 0", streamErr.Offset)
	}
}

// TestStreamDecoderGarbage checks that input without any MP3 frame ends
// with io.EOF rather than an error or an endless sync scan.
func TestStreamDecoderGarbage(t *testing.T) {
	data := []byte("this is not MP3 data, just some garbage bytes without a sync word")
	_, err := NewStreamDecoder(&readerOnly{r: bytes.NewReader(data)})
	if err != io.EOF {
		t.Fatalf("got %v, want io.EOF", err)
	}
}

// TestStreamDecoderReadError checks that an underlying read error surfaces
// with the input offset instead of being swallowed or retried forever.
func TestStreamDecoderReadError(t *testing.T) {
	data := readTestFile(t, "example/classic.mp3")
	errBoom := errors.New("boom")
	// Let the first frame through, then fail.
	r := &failReader{r: &readerOnly{r: bytes.NewReader(data)}, after: 1000, err: errBoom}
	sd, err := NewStreamDecoder(r)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 1<<20)
	for {
		_, err = sd.Read(buf)
		if err != nil {
			break
		}
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("got %v, want the underlying error", err)
	}
	var streamErr *StreamError
	if !errors.As(err, &streamErr) {
		t.Fatalf("error must be locatable as *StreamError: %v", err)
	}
	if streamErr.Offset <= 0 || streamErr.Offset >= int64(len(data)) {
		t.Fatalf("implausible error offset %d for %d input bytes", streamErr.Offset, len(data))
	}
}

// TestStreamDecoderNoProgressReader checks that a reader returning
// (0, nil) forever makes the decoder give up with io.ErrNoProgress instead
// of waiting forever.
func TestStreamDecoderNoProgressReader(t *testing.T) {
	_, err := NewStreamDecoder(zeroReader{})
	if err == nil {
		t.Fatal("expected an error for a reader that never makes progress")
	}
	if !errors.Is(err, io.ErrNoProgress) {
		t.Fatalf("got %v, want io.ErrNoProgress", err)
	}
}
