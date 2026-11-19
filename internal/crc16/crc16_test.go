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

package crc16

import "testing"

func TestCRC16(t *testing.T) {
	// CRC-16 with polynomial 0x8005 and initial value 0xFFFF of the
	// standard check string "123456789".
	c := New()
	c.Write([]byte("123456789"))
	if got, want := c.Sum(), uint16(0xaee7); got != want {
		t.Errorf("got 0x%04x, want 0x%04x", got, want)
	}

	// An empty input keeps the initial value.
	if got, want := New().Sum(), uint16(0xffff); got != want {
		t.Errorf("got 0x%04x, want 0x%04x", got, want)
	}

	// Writing in chunks must give the same result.
	c = New()
	for _, b := range []byte("123456789") {
		c.Write([]byte{b})
	}
	if got, want := c.Sum(), uint16(0xaee7); got != want {
		t.Errorf("got 0x%04x, want 0x%04x", got, want)
	}
}
