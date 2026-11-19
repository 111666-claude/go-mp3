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

// Package crc16 implements the CRC-16 checksum used by MPEG audio frames
// (polynomial 0x8005, initial value 0xFFFF, no reflection, no final XOR).
package crc16

const (
	poly       = 0x8005
	initialCRC = 0xFFFF
)

var table [256]uint16

func init() {
	for i := 0; i < 256; i++ {
		crc := uint16(i) << 8
		for j := 0; j < 8; j++ {
			if crc&0x8000 != 0 {
				crc = (crc << 1) ^ poly
			} else {
				crc <<= 1
			}
		}
		table[i] = crc
	}
}

// A CRC16 is a running CRC-16/MPEG checksum.
type CRC16 struct {
	crc uint16
}

// New returns a new CRC16 with the MPEG initial value 0xFFFF.
func New() *CRC16 {
	return &CRC16{crc: initialCRC}
}

// Write adds p to the running checksum.
func (c *CRC16) Write(p []byte) {
	for _, b := range p {
		c.crc = c.crc<<8 ^ table[byte(c.crc>>8)^b]
	}
}

// Sum returns the current checksum.
func (c *CRC16) Sum() uint16 {
	return c.crc
}
