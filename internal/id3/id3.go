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

// Package id3 parses the chapter information stored in ID3v2 tags.
//
// Two frame types carry chapter metadata: CHAP describes a single chapter and
// CTOC describes the order in which chapters and nested tables of contents
// appear. See https://id3.org/id3v2-chapters-1.0.
package id3

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"strings"
	"unicode/utf16"
)

var (
	// ErrInvalidTag means the tag or one of its frame headers is structurally
	// invalid.
	ErrInvalidTag = errors.New("mp3: invalid ID3v2 tag")
	// ErrUnsupportedVersion means the tag uses an ID3 version this parser
	// does not understand.
	ErrUnsupportedVersion = errors.New("mp3: unsupported ID3v2 version")
	// ErrInvalidTagSize means the declared tag size is not a valid synchsafe
	// integer or is unreasonably large.
	ErrInvalidTagSize = errors.New("mp3: invalid ID3v2 tag size")
	// ErrTruncatedTag means the tag ends before the declared size.
	ErrTruncatedTag = errors.New("mp3: truncated ID3v2 tag")
	// ErrTruncatedFrame means a frame body extends past the end of the tag or
	// of its enclosing frame.
	ErrTruncatedFrame = errors.New("mp3: truncated ID3v2 frame")
	// ErrInvalidChapter means a CHAP or CTOC frame is malformed (for example
	// a missing Element ID or an invalid child list).
	ErrInvalidChapter = errors.New("mp3: invalid chapter frame")
	// ErrDuplicateID means two CHAP or CTOC frames share an Element ID.
	ErrDuplicateID = errors.New("mp3: duplicate chapter element ID")
	// ErrChapterCycle means the tables of contents reference each other in a
	// cycle, so no playing order can be derived.
	ErrChapterCycle = errors.New("mp3: cyclic chapter table of contents")
	// ErrInvalidTimeRange means a chapter's start time is after its end time.
	ErrInvalidTimeRange = errors.New("mp3: chapter start time is after its end time")
	// ErrChapterTimeOutOfRange means a chapter time points past the end of the
	// audio stream.
	ErrChapterTimeOutOfRange = errors.New("mp3: chapter time is beyond the audio length")
)

// Chapter is a single CHAP entry.
//
// Times are milliseconds from the beginning of the audio as declared in the
// tag. The caller is responsible for validating them against the decoded
// stream and converting them to PCM coordinates.
type Chapter struct {
	ID      string
	Title   string
	StartMS uint32
	EndMS   uint32
}

// ParseChapters extracts the chapters from a complete ID3v2 tag (including
// the 10-byte tag header). It returns the chapters in playback order.
//
// If the tag contains no CTOC frame, CHAP frames are returned in ascending
// start-time order (stable for equal times). If it contains a table of
// contents, the order and nesting of its child list is used. A tag without
// chapter frames yields a nil slice and a nil error.
//
// ParseChapters is strict about structural damage: truncated frames, invalid
// sizes, duplicate Element IDs, dangling or cyclic CTOC references and
// inverted chapter time ranges are reported as errors.
func ParseChapters(tag []byte) ([]Chapter, error) {
	if len(tag) == 0 {
		return nil, nil
	}
	if len(tag) < 10 || string(tag[:3]) != "ID3" {
		return nil, fmt.Errorf("%w: missing ID3 header", ErrInvalidTag)
	}
	major := tag[3]
	if major < 2 || major > 4 {
		return nil, fmt.Errorf("%w: v2.%d", ErrUnsupportedVersion, major)
	}
	size, ok := synchsafe(tag[6:10])
	if !ok {
		return nil, fmt.Errorf("%w: size is not synchsafe", ErrInvalidTagSize)
	}
	// The caller should have checked this before allocating; repeat the check
	// here so ParseChapters is safe when used on its own.
	if uint64(size) > MaxTagSize {
		return nil, fmt.Errorf("%w: %d bytes", ErrInvalidTagSize, size)
	}
	if len(tag) < 10+int(size) {
		return nil, fmt.Errorf("%w: declared %d bytes, have %d", ErrTruncatedTag, size, len(tag)-10)
	}
	body := tag[10 : 10+int(size)]
	flags := tag[5]

	// ID3v2.2 uses 3-byte frame IDs, so it cannot contain CHAP or CTOC
	// frames. The tag is still valid audio metadata; just report no chapters.
	if major == 2 {
		return nil, nil
	}

	if major == 4 {
		// In ID3v2.4 unsynchronisation may cover the extended header too, so
		// remove it before any body parsing.
		if flags&0x80 != 0 {
			body = deunsynchronise(body)
		}
		if flags&0x10 != 0 {
			// With a footer the size field covers the frames and padding but
			// not the 10-byte footer that follows them.
			if len(tag) < 10+int(size)+10 {
				return nil, fmt.Errorf("%w: missing ID3 footer", ErrTruncatedTag)
			}
			if string(tag[10+int(size):10+int(size)+3]) != "3DI" {
				return nil, fmt.Errorf("%w: missing ID3 footer", ErrInvalidTag)
			}
		}
	} else {
		// ID3v2.3 unsynchronises the whole tag body.
		if flags&0x80 != 0 {
			body = deunsynchronise(body)
		}
	}

	if flags&0x40 != 0 {
		var err error
		body, err = skipExtendedHeader(body, major)
		if err != nil {
			return nil, err
		}
	}

	var (
		chaps = make(map[string]*Chapter)
		ctocs = make(map[string]*ctoc)
		order []string // CHAP IDs in tag order, for linear fallback
	)
	err := eachFrame(body, major, func(id string, data []byte) error {
		switch id {
		case "CHAP":
			c, err := parseCHAP(data, major)
			if err != nil {
				return err
			}
			if _, ok := chaps[c.ID]; ok {
				return fmt.Errorf("%w: %q", ErrDuplicateID, c.ID)
			}
			if _, ok := ctocs[c.ID]; ok {
				return fmt.Errorf("%w: %q", ErrDuplicateID, c.ID)
			}
			chaps[c.ID] = c
			order = append(order, c.ID)
		case "CTOC":
			c, err := parseCTOC(data, major)
			if err != nil {
				return err
			}
			if _, ok := chaps[c.ID]; ok {
				return fmt.Errorf("%w: %q", ErrDuplicateID, c.ID)
			}
			if _, ok := ctocs[c.ID]; ok {
				return fmt.Errorf("%w: %q", ErrDuplicateID, c.ID)
			}
			ctocs[c.ID] = c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	if len(chaps) == 0 {
		if len(ctocs) > 0 {
			return nil, fmt.Errorf("%w: %d CTOC frames but no CHAP frames", ErrInvalidChapter, len(ctocs))
		}
		return nil, nil
	}
	if len(ctocs) == 0 {
		return linearChapters(chaps, order), nil
	}
	return orderedChapters(chaps, ctocs)
}

// MaxTagSize is the largest ID3v2 tag body the parser accepts. Real tags are
// normally a few kilobytes; the ID3 format allows 256 MiB, which is far more
// memory than a tag should be able to make a decoder allocate.
const MaxTagSize = 32 << 20

// Synchsafe decodes a four-byte synchsafe integer, as used for tag and frame
// sizes in ID3v2.4. It reports false when any of the four high bits is set,
// which would mean the value is not a valid synchsafe integer.
func Synchsafe(b []byte) (uint32, bool) {
	if len(b) != 4 || b[0]&0x80 != 0 || b[1]&0x80 != 0 || b[2]&0x80 != 0 || b[3]&0x80 != 0 {
		return 0, false
	}
	return uint32(b[0])<<21 | uint32(b[1])<<14 | uint32(b[2])<<7 | uint32(b[3]), true
}

func synchsafe(b []byte) (uint32, bool) {
	return Synchsafe(b)
}

func skipExtendedHeader(body []byte, major byte) ([]byte, error) {
	if len(body) < 4 {
		return nil, fmt.Errorf("%w: truncated extended header", ErrTruncatedTag)
	}
	var size int
	if major == 4 {
		s, ok := synchsafe(body[:4])
		if !ok {
			return nil, fmt.Errorf("%w: extended header size is not synchsafe", ErrInvalidTagSize)
		}
		// In v2.4 the size includes the four size bytes themselves.
		size = int(s)
		if size < 6 {
			return nil, fmt.Errorf("%w: extended header size %d", ErrInvalidTag, size)
		}
	} else {
		// In v2.3 the size excludes the four size bytes themselves. Comparing
		// in uint64 avoids sign overflow on 32-bit platforms.
		size = int(binary.BigEndian.Uint32(body[:4]))
		if uint64(size) > uint64(len(body)-4) {
			return nil, fmt.Errorf("%w: extended header size %d, have %d", ErrTruncatedTag, size, len(body)-4)
		}
		size += 4
	}
	if size > len(body) {
		return nil, fmt.Errorf("%w: extended header size %d, have %d", ErrTruncatedTag, size, len(body))
	}
	return body[size:], nil
}

// eachFrame iterates over the frames in a tag body. It stops at padding and
// skips frames that are not of interest, but rejects structurally broken
// headers and frame bodies.
func eachFrame(body []byte, major byte, fn func(id string, data []byte) error) error {
	for len(body) > 0 {
		if len(body) < 10 {
			if isZero(body) {
				return nil
			}
			return fmt.Errorf("%w: %d stray bytes at end of tag", ErrTruncatedFrame, len(body))
		}
		if body[0] == 0 {
			if isZero(body) {
				return nil
			}
			return fmt.Errorf("%w: non-zero data after padding", ErrInvalidTag)
		}
		id := string(body[:4])
		if !validFrameID(id) {
			return fmt.Errorf("%w: frame ID %q", ErrInvalidTag, id)
		}

		var size int
		if major == 4 {
			s, ok := synchsafe(body[4:8])
			if !ok {
				return fmt.Errorf("%w: frame %q size is not synchsafe", ErrInvalidTagSize, id)
			}
			size = int(s)
		} else {
			s := binary.BigEndian.Uint32(body[4:8])
			// Compare as uint64: on 32-bit platforms int(s) could wrap.
			if uint64(s) > uint64(len(body)-10) {
				return fmt.Errorf("%w: frame %q declares %d bytes, have %d", ErrTruncatedFrame, id, s, len(body)-10)
			}
			size = int(s)
		}
		if size > len(body)-10 {
			return fmt.Errorf("%w: frame %q declares %d bytes, have %d", ErrTruncatedFrame, id, size, len(body)-10)
		}
		flags := body[8:10]
		data := body[10 : 10+size]
		next := body[10+size:]

		// Remove the version-specific extra flag fields and, for v2.4,
		// reverse frame-level unsynchronisation.
		var err error
		data, err = frameData(data, id, major, flags)
		if err != nil {
			return err
		}
		if err := fn(id, data); err != nil {
			return err
		}
		body = next
	}
	return nil
}

func frameData(data []byte, id string, major byte, flags []byte) ([]byte, error) {
	if major < 4 {
		// v2.3 format byte: compression (0x80), encryption (0x40),
		// grouping identity (0x20). A data length indicator precedes
		// compressed data and the group byte precedes the frame data.
		if flags[1]&0xC0 != 0 {
			if id == "CHAP" || id == "CTOC" {
				return nil, fmt.Errorf("%w: %s is compressed or encrypted", ErrInvalidChapter, id)
			}
			return data, nil
		}
		if flags[1]&0x20 != 0 {
			if len(data) < 1 {
				return nil, fmt.Errorf("%w: frame %q is missing a group byte", ErrTruncatedFrame, id)
			}
			data = data[1:]
		}
		return data, nil
	}

	// v2.4 format byte: grouping (0x40), compression (0x08), encryption
	// (0x04), unsynchronisation (0x02), data length indicator (0x01).
	if flags[1]&0x40 != 0 {
		if len(data) < 1 {
			return nil, fmt.Errorf("%w: frame %q is missing a group byte", ErrTruncatedFrame, id)
		}
		data = data[1:]
	}
	if flags[1]&0x08 != 0 || flags[1]&0x04 != 0 {
		if id == "CHAP" || id == "CTOC" {
			return nil, fmt.Errorf("%w: %s is compressed or encrypted", ErrInvalidChapter, id)
		}
		return data, nil
	}
	if flags[1]&0x01 != 0 {
		if len(data) < 4 {
			return nil, fmt.Errorf("%w: frame %q is missing a data length indicator", ErrTruncatedFrame, id)
		}
		data = data[4:]
	}
	if flags[1]&0x02 != 0 {
		data = deunsynchronise(data)
	}
	return data, nil
}

func validFrameID(id string) bool {
	if len(id) != 4 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'A' && c <= 'Z') && !(c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}

func isZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func deunsynchronise(b []byte) []byte {
	if !bytes.Contains(b, []byte{0xFF, 0x00}) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

type ctoc struct {
	ID       string
	TopLevel bool
	Ordered  bool
	Entries  []string
	Title    string
}

func parseCHAP(data []byte, major byte) (*Chapter, error) {
	id, rest, err := takeElementID(data)
	if err != nil {
		return nil, err
	}
	// Start time, end time, start offset and end offset are each 4 bytes.
	if len(rest) < 16 {
		return nil, fmt.Errorf("%w: CHAP %q has %d bytes after its ID, need 16", ErrTruncatedFrame, id, len(rest))
	}
	// Times are in milliseconds from the start of the audio. Offsets are
	// unreliable in practice (encoders frequently cannot fill them in) and are
	// not needed: the decoder derives PCM positions from the times.
	start := binary.BigEndian.Uint32(rest[0:4])
	end := binary.BigEndian.Uint32(rest[4:8])
	title, err := embeddedTitle(rest[16:], major)
	if err != nil {
		return nil, err
	}
	if start > end {
		return nil, fmt.Errorf("%w: CHAP %q is %d..%d ms", ErrInvalidTimeRange, id, start, end)
	}
	return &Chapter{
		ID:      id,
		Title:   title,
		StartMS: start,
		EndMS:   end,
	}, nil
}

func parseCTOC(data []byte, major byte) (*ctoc, error) {
	id, rest, err := takeElementID(data)
	if err != nil {
		return nil, err
	}
	if len(rest) < 2 {
		return nil, fmt.Errorf("%w: CTOC %q has no flags or entry count", ErrTruncatedFrame, id)
	}
	flags := rest[0]
	count := int(rest[1])
	rest = rest[2:]
	if count == 0 {
		return nil, fmt.Errorf("%w: CTOC %q has no entries", ErrInvalidChapter, id)
	}
	entries := make([]string, 0, count)
	for i := 0; i < count; i++ {
		n := bytes.IndexByte(rest, 0)
		if n < 0 {
			return nil, fmt.Errorf("%w: CTOC %q child %d is not terminated", ErrTruncatedFrame, id, i)
		}
		if n == 0 {
			return nil, fmt.Errorf("%w: CTOC %q has an empty child ID", ErrInvalidChapter, id)
		}
		entries = append(entries, string(rest[:n]))
		rest = rest[n+1:]
	}
	title, err := embeddedTitle(rest, major)
	if err != nil {
		return nil, err
	}
	return &ctoc{
		ID:       id,
		TopLevel: flags&0x02 != 0,
		Ordered:  flags&0x01 != 0,
		Entries:  entries,
		Title:    title,
	}, nil
}

func takeElementID(data []byte) (string, []byte, error) {
	n := bytes.IndexByte(data, 0)
	if n < 0 {
		return "", nil, fmt.Errorf("%w: Element ID is not terminated", ErrInvalidChapter)
	}
	if n == 0 {
		return "", nil, fmt.Errorf("%w: empty Element ID", ErrInvalidChapter)
	}
	return string(data[:n]), data[n+1:], nil
}

// embeddedTitle reads the TIT2 sub-frame of a CHAP or CTOC frame. Both frame
// types may carry optional sub-frames after their fixed fields.
func embeddedTitle(data []byte, major byte) (string, error) {
	title := ""
	for len(data) > 0 {
		if isZero(data) {
			return title, nil
		}
		if len(data) < 10 {
			return "", fmt.Errorf("%w: %d stray bytes in chapter frame", ErrTruncatedFrame, len(data))
		}
		id := string(data[:4])
		if !validFrameID(id) {
			return "", fmt.Errorf("%w: embedded frame ID %q", ErrInvalidTag, id)
		}
		var size int
		if major == 4 {
			s, ok := synchsafe(data[4:8])
			if !ok {
				return "", fmt.Errorf("%w: embedded frame %q size is not synchsafe", ErrInvalidTagSize, id)
			}
			size = int(s)
		} else {
			s := binary.BigEndian.Uint32(data[4:8])
			if uint64(s) > uint64(len(data)-10) {
				return "", fmt.Errorf("%w: embedded frame %q declares %d bytes, have %d", ErrTruncatedFrame, id, s, len(data)-10)
			}
			size = int(s)
		}
		if size > len(data)-10 {
			return "", fmt.Errorf("%w: embedded frame %q declares %d bytes, have %d", ErrTruncatedFrame, id, size, len(data)-10)
		}
		if id == "TIT2" {
			title = decodeText(data[10 : 10+size])
		}
		data = data[10+size:]
	}
	return title, nil
}

// decodeText decodes an ID3 text frame body according to its encoding byte.
func decodeText(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	enc := b[0]
	b = b[1:]
	var s string
	switch enc {
	case 0:
		// ISO-8859-1.
		var sb strings.Builder
		sb.Grow(len(b))
		for _, c := range b {
			sb.WriteRune(rune(c))
		}
		s = sb.String()
	case 1:
		// UTF-16 with a byte order mark.
		var order binary.ByteOrder = binary.BigEndian
		if len(b) >= 2 {
			switch {
			case b[0] == 0xFF && b[1] == 0xFE:
				order = binary.LittleEndian
				b = b[2:]
			case b[0] == 0xFE && b[1] == 0xFF:
				b = b[2:]
			}
		}
		s = decodeUTF16(b, order)
	case 2:
		// UTF-16 big endian without a byte order mark.
		s = decodeUTF16(b, binary.BigEndian)
	case 3:
		s = string(b)
	default:
		// Unknown encodings are unusable as text. Keep the chapter but leave
		// its title empty rather than failing the whole tag.
		return ""
	}
	// Text frames may contain several NUL-separated values. A chapter title
	// is the first one.
	if i := strings.IndexByte(s, 0); i >= 0 {
		s = s[:i]
	}
	return s
}

func decodeUTF16(b []byte, order binary.ByteOrder) string {
	if len(b)%2 != 0 {
		b = b[:len(b)-1]
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = order.Uint16(b[i*2:])
	}
	return string(utf16.Decode(u))
}

func linearChapters(chaps map[string]*Chapter, order []string) []Chapter {
	out := make([]Chapter, 0, len(chaps))
	for _, id := range order {
		if c, ok := chaps[id]; ok {
			out = append(out, *c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StartMS != out[j].StartMS {
			return out[i].StartMS < out[j].StartMS
		}
		return out[i].EndMS < out[j].EndMS
	})
	return out
}

func orderedChapters(chaps map[string]*Chapter, ctocs map[string]*ctoc) ([]Chapter, error) {
	var roots []*ctoc
	for _, c := range ctocs {
		if c.TopLevel {
			roots = append(roots, c)
		}
	}
	if len(roots) > 1 {
		return nil, fmt.Errorf("%w: %d top-level CTOC frames", ErrInvalidChapter, len(roots))
	}
	var root *ctoc
	if len(roots) == 1 {
		root = roots[0]
	} else if len(ctocs) == 1 {
		// Tolerate a single CTOC without the top-level flag, which several
		// encoders omit in practice.
		for _, c := range ctocs {
			root = c
		}
	} else {
		return nil, fmt.Errorf("%w: no top-level CTOC frame", ErrInvalidChapter)
	}

	// walk emits the chapters reachable from id in declaration order. When
	// collect is false it only validates the tree.
	state := make(map[string]int) // 0 unvisited, 1 visiting, 2 done
	var out []Chapter
	var walk func(id string, collect bool) error
	walk = func(id string, collect bool) error {
		switch state[id] {
		case 1:
			return fmt.Errorf("%w: %q", ErrChapterCycle, id)
		case 2:
			// A DAG may reference the same element more than once; it has
			// already been emitted.
			return nil
		}
		if c, ok := chaps[id]; ok {
			state[id] = 2
			if collect {
				out = append(out, *c)
			}
			return nil
		}
		ct, ok := ctocs[id]
		if !ok {
			return fmt.Errorf("%w: CTOC references unknown element %q", ErrInvalidChapter, id)
		}
		state[id] = 1
		for _, child := range ct.Entries {
			if err := walk(child, collect); err != nil {
				return err
			}
		}
		state[id] = 2
		return nil
	}
	if err := walk(root.ID, true); err != nil {
		return nil, err
	}
	// Validate every other CTOC as well, so a dangling reference or a cycle
	// hidden in an orphaned table is reported rather than silently ignored.
	for id := range ctocs {
		if err := walk(id, false); err != nil {
			return nil, err
		}
	}
	// Any chapter the root's table does not mention is still part of the
	// audio; append it in start-time order.
	var ordered []Chapter
	seen := make(map[string]bool)
	for _, c := range out {
		seen[c.ID] = true
		ordered = append(ordered, c)
	}
	var rest []Chapter
	for id, c := range chaps {
		if !seen[id] {
			rest = append(rest, *c)
		}
	}
	sort.SliceStable(rest, func(i, j int) bool {
		if rest[i].StartMS != rest[j].StartMS {
			return rest[i].StartMS < rest[j].StartMS
		}
		return rest[i].EndMS < rest[j].EndMS
	})
	ordered = append(ordered, rest...)
	if len(ordered) == 0 {
		return nil, nil
	}
	return ordered, nil
}
