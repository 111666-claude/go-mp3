// Copyright 2017 Hajime Hoshi
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
	"errors"
	"fmt"
	"io"

	"github.com/hajimehoshi/go-mp3/internal/id3"
)

type source struct {
	reader io.Reader
	buf    []byte
	pos    int64
	// id3Tag is the complete ID3v2 tag (header, frames and footer, if any)
	// found at the beginning of the stream, or nil when there is none.
	id3Tag []byte
}

func (s *source) Seek(position int64, whence int) (int64, error) {
	seeker, ok := s.reader.(io.Seeker)
	if !ok {
		return 0, errors.New("mp3: source must be io.Seeker")
	}
	s.buf = nil
	n, err := seeker.Seek(position, whence)
	if err != nil {
		return 0, err
	}
	s.pos = n
	return n, nil
}

func (s *source) skipTags() error {
	buf := make([]byte, 3)
	if _, err := s.ReadFull(buf); err != nil {
		return err
	}
	switch string(buf) {
	case "TAG":
		buf := make([]byte, 125)
		if _, err := s.ReadFull(buf); err != nil {
			return err
		}

	case "ID3":
		// version (2 bytes) and flags (1 byte)
		header := make([]byte, 10)
		copy(header, buf)
		if _, err := s.ReadFull(header[3:]); err != nil {
			return err
		}
		size, ok := id3.Synchsafe(header[6:10])
		if !ok {
			return fmt.Errorf("%w: header size is not synchsafe", id3.ErrInvalidTagSize)
		}
		if size > id3.MaxTagSize {
			return fmt.Errorf("%w: %d bytes", id3.ErrInvalidTagSize, size)
		}
		// A v2.4 footer is not covered by the tag size. Reject a footer flag
		// on an older version: ID3v2.2/2.3 define no footer and treating the
		// bit as one would desynchronise the stream.
		total := int64(10 + size)
		if header[3] == 4 && header[5]&0x10 != 0 {
			total += 10
		} else if header[3] < 4 && header[5]&0x10 != 0 {
			return fmt.Errorf("%w: footer flag on ID3v2.%d", id3.ErrInvalidTag, header[3])
		}
		tag := make([]byte, 10)
		copy(tag, header)
		// Read the body in chunks and keep only the bytes that actually
		// arrive. Trusting the declared size for a single allocation would
		// let a short stream make the decoder reserve its claimed tag size;
		// growing by received data bounds memory by the real stream length.
		const chunk = 64 << 10
		buf := make([]byte, chunk)
		for int64(len(tag)) < total {
			n := int64(chunk)
			if remaining := total - int64(len(tag)); remaining < n {
				n = remaining
			}
			got, err := s.ReadFull(buf[:n])
			tag = append(tag, buf[:got]...)
			if err != nil {
				// A truncated tag has no audio worth decoding. Reporting it
				// is better than silently continuing at an arbitrary byte
				// offset.
				if err == io.EOF {
					return fmt.Errorf("%w: declared %d bytes", id3.ErrTruncatedTag, size)
				}
				return err
			}
		}
		s.id3Tag = tag

	default:
		s.Unread(buf)
	}

	return nil
}

func (s *source) rewind() error {
	if _, err := s.Seek(0, io.SeekStart); err != nil {
		return err
	}
	s.pos = 0
	s.buf = nil
	return nil
}

func (s *source) Unread(buf []byte) {
	s.buf = append(s.buf, buf...)
	s.pos -= int64(len(buf))
}

func (s *source) ReadFull(buf []byte) (int, error) {
	read := 0
	if s.buf != nil {
		read = copy(buf, s.buf)
		if len(s.buf) > read {
			s.buf = s.buf[read:]
		} else {
			s.buf = nil
		}
		if len(buf) == read {
			return read, nil
		}
	}

	n, err := io.ReadFull(s.reader, buf[read:])
	if err != nil {
		// Allow if all data can't be read. This is common.
		if err == io.ErrUnexpectedEOF {
			err = io.EOF
		}
	}
	s.pos += int64(n)
	return n + read, err
}
