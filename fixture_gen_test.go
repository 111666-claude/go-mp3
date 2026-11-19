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
	"os"
	"testing"
)

// TestGenerateProtectedFixture regenerates the CRC-protected test fixtures
// under testdata/ from a raw MP3 file. It only runs when GO_MP3_GEN_FIXTURE
// is set:
//
//	GO_MP3_GEN_FIXTURE=1 go test -run TestGenerateProtectedFixture .
//
// The raw file was produced with:
//
//	ffmpeg -f lavfi -i "sine=frequency=440:duration=2:sample_rate=44100" \
//	    -ac 2 -c:a libmp3lame -b:a 128k -reservoir 0 /tmp/mp3probe/cbr_nores.mp3
//
// The bit reservoir is disabled (main_data_begin is always 0) so that
// growing the frames by raising their bitrate index never invalidates
// back-references into previous frames. The VBR fixture is synthesized by
// raising the bitrate index of each frame by a varying amount. The test
// verifies that each rewritten stream decodes to the exact same PCM as
// the raw stream before writing the fixture.
func TestGenerateProtectedFixture(t *testing.T) {
	if os.Getenv("GO_MP3_GEN_FIXTURE") == "" {
		t.Skip("GO_MP3_GEN_FIXTURE is not set")
	}
	raw, err := os.ReadFile("/tmp/mp3probe/cbr_nores.mp3")
	if err != nil {
		t.Fatal(err)
	}
	rawPCM, err := decodeAll(t, bytes.NewReader(raw), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		out  string
		bump func(i int) int
		vbr  bool
	}{
		{out: "testdata/crc_cbr.mp3", bump: func(i int) int { return 1 }},
		{out: "testdata/crc_vbr.mp3", bump: func(i int) int { return 1 + i%3 }, vbr: true},
	} {
		protected, frames := addCRC(t, raw, tc.bump)
		if tc.vbr {
			sizes := map[int]bool{}
			for _, f := range frames {
				sizes[f.size] = true
			}
			if len(sizes) < 2 {
				t.Fatalf("%s: not a VBR stream, frame sizes: %v", tc.out, sizes)
			}
		}
		protectedPCM, err := decodeAll(t, bytes.NewReader(protected), &Options{VerifyCRC: true})
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(rawPCM, protectedPCM) {
			t.Fatalf("%s: the CRC-protected rewrite does not decode to the original PCM", tc.out)
		}
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(tc.out, protected, 0o644); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s (%d frames)", tc.out, len(frames))
	}
}
