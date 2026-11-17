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
	"bufio"
	"errors"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3/internal/consts"
	"github.com/hajimehoshi/go-mp3/internal/frame"
)

// StreamError reports a failure while decoding a stream, together with the
// byte offset in the input stream at which the failing frame (or tag)
// started. Use errors.As to extract it and errors.Is to match the wrapped
// cause.
type StreamError struct {
	// Offset is the byte offset in the input stream where the error
	// occurred, e.g. the start of the frame that could not be decoded.
	Offset int64
	// Err is the underlying error.
	Err error
}

// Error is error's Error.
func (e *StreamError) Error() string {
	return fmt.Sprintf("mp3: stream error at input offset %d: %v", e.Offset, e.Err)
}

// Unwrap returns the underlying error.
func (e *StreamError) Unwrap() error {
	return e.Err
}

// maxNoProgressReads bounds how many consecutive (0, nil) reads from the
// underlying reader are tolerated before giving up with io.ErrNoProgress,
// so that a misbehaving reader cannot make the decoder wait forever.
const maxNoProgressReads = 100

// noProgressReader turns a reader that keeps returning (0, nil) into an
// error instead of letting io.ReadFull spin forever.
type noProgressReader struct {
	r         io.Reader
	zeroReads int
}

func (n *noProgressReader) Read(buf []byte) (int, error) {
	read, err := n.r.Read(buf)
	if read == 0 && err == nil {
		n.zeroReads++
		if n.zeroReads >= maxNoProgressReads {
			return 0, fmt.Errorf("mp3: the underlying reader returned (0, nil) %d times in a row: %w",
				maxNoProgressReads, io.ErrNoProgress)
		}
	} else {
		n.zeroReads = 0
	}
	return read, err
}

// A StreamDecoder is a MP3-decoded stream read sequentially from an
// io.Reader that does not need to be an io.Seeker.
//
// StreamDecoder is meant for inputs whose total size is unknown up front,
// such as network radio: it decodes frame by frame as bytes arrive, skips
// an optional ID3v2 tag, and never scans or buffers the whole input.
// For a complete, seekable file, NewDecoder is a better fit since it also
// provides the total length and seeking.
//
// The decoded stream is always formatted as 16bit (little endian) 2
// channels even if the source is single channel MP3.
// Thus, a sample always consists of 4 bytes.
type StreamDecoder struct {
	source     *source
	sampleRate int
	buf        []byte
	frame      *frame.Frame
	pos        int64
}

// NewStreamDecoder decodes the given io.Reader sequentially and returns a
// decoded stream.
//
// Unlike NewDecoder, r is never required to be an io.Seeker and is read
// strictly from front to back, in whatever chunk sizes r provides. An
// ID3v2 tag at the beginning is skipped (up to a bounded size).
//
// The total decoded length is unknown until the stream ends, so
// Length always returns -1 and Seek always returns an error.
func NewStreamDecoder(r io.Reader) (*StreamDecoder, error) {
	if r == nil {
		return nil, errors.New("mp3: nil reader")
	}
	// Guard against readers returning (0, nil) repeatedly, which would
	// otherwise make io.ReadFull wait forever.
	if _, ok := r.(*noProgressReader); !ok {
		r = &noProgressReader{r: r}
	}
	// Buffer the input: frame sync scanning reads a byte at a time, which is
	// expensive on unbuffered readers such as network connections. This is
	// safe because a StreamDecoder never seeks backwards.
	if _, ok := r.(io.ByteReader); !ok {
		r = bufio.NewReader(r)
	}
	s := &source{
		reader: r,
	}
	d := &StreamDecoder{
		source: s,
	}

	if err := s.skipTags(); err != nil {
		return nil, d.wrapErr(err)
	}
	if err := d.readFrame(); err != nil {
		return nil, err
	}
	freq, err := d.frame.SamplingFrequency()
	if err != nil {
		return nil, d.wrapErr(err)
	}
	d.sampleRate = freq

	return d, nil
}

// wrapErr converts err into the form reported to callers: io.EOF and
// truncated final frames stay io.EOF, anything else is located with a
// StreamError.
func (d *StreamDecoder) wrapErr(err error) error {
	if err == nil || err == io.EOF {
		return err
	}
	if _, ok := err.(*consts.UnexpectedEOF); ok {
		// The stream ended in the middle of a frame or a tag.
		return io.EOF
	}
	var streamErr *StreamError
	if errors.As(err, &streamErr) {
		return err
	}
	return &StreamError{
		Offset: d.source.pos,
		Err:    err,
	}
}

func (d *StreamDecoder) readFrame() error {
	start := d.source.pos
	var err error
	d.frame, _, err = frame.Read(d.source, start, d.frame)
	if err != nil {
		if err == io.EOF {
			return io.EOF
		}
		if _, ok := err.(*consts.UnexpectedEOF); ok {
			// The stream ended in the middle of a frame: report a clean
			// end of stream instead of blocking for the missing bytes.
			return io.EOF
		}
		return &StreamError{
			Offset: start,
			Err:    err,
		}
	}
	d.buf = append(d.buf, d.frame.Decode()...)
	return nil
}

// Read is io.Reader's Read.
//
// Read returns io.EOF at the end of the stream, including when the stream
// ends in the middle of a frame. Any other error is reported as a
// *StreamError carrying the input offset at which decoding failed.
func (d *StreamDecoder) Read(buf []byte) (int, error) {
	for len(d.buf) == 0 {
		if err := d.readFrame(); err != nil {
			return 0, err
		}
	}
	n := copy(buf, d.buf)
	d.buf = d.buf[n:]
	d.pos += int64(n)
	return n, nil
}

// Seek is io.Seeker's Seek.
//
// A StreamDecoder reads a forward-only stream whose total length is
// unknown, so seeking is not possible and Seek always returns an error,
// except for the no-op query Seek(0, io.SeekCurrent), which reports the
// current decoded position in bytes. Use NewDecoder with an io.Seeker
// source if you need to seek.
func (d *StreamDecoder) Seek(offset int64, whence int) (int64, error) {
	if offset == 0 && whence == io.SeekCurrent {
		return d.pos, nil
	}
	return 0, errors.New("mp3: StreamDecoder does not support seeking; use NewDecoder with an io.Seeker source instead")
}

// Length returns the total size in bytes of the decoded stream.
//
// A StreamDecoder cannot know the total size before the stream ends, so
// Length always returns -1.
func (d *StreamDecoder) Length() int64 {
	return invalidLength
}

// SampleRate returns the sample rate like 44100.
//
// Note that the sample rate is retrieved from the first frame.
func (d *StreamDecoder) SampleRate() int {
	return d.sampleRate
}

// InputOffset returns the number of bytes consumed so far from the input
// stream, including a skipped ID3v2 tag. It helps locate decoding errors
// in the raw input, e.g. when logging failures of a radio stream.
func (d *StreamDecoder) InputOffset() int64 {
	return d.source.pos
}
