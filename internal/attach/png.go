// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
)

// PNG text chunks let the vision probe carry its code in the image's
// metadata as well as in its pixels: only the mock model provider and the
// fake harnesses read it there, so tests can check the image path end to
// end without a real model.

var pngSig = []byte("\x89PNG\r\n\x1a\n")

// PNGText returns the value of the first tEXt chunk with keyword key, or "".
func PNGText(b []byte, key string) string {
	if !bytes.HasPrefix(b, pngSig) {
		return ""
	}
	b = b[len(pngSig):]
	for len(b) >= 12 {
		n := binary.BigEndian.Uint32(b[:4])
		if uint64(n)+12 > uint64(len(b)) {
			return ""
		}
		typ, data := string(b[4:8]), b[8:8+n]
		if typ == "tEXt" {
			if k, v, ok := bytes.Cut(data, []byte{0}); ok && string(k) == key {
				return string(v)
			}
		}
		if typ == "IEND" {
			return ""
		}
		b = b[12+n:]
	}
	return ""
}

// AddPNGText inserts a tEXt chunk after a PNG's IHDR chunk.
func AddPNGText(png []byte, key, value string) []byte {
	if !bytes.HasPrefix(png, pngSig) || len(png) < len(pngSig)+12 {
		return png
	}
	ihdrLen := binary.BigEndian.Uint32(png[len(pngSig):])
	cut := len(pngSig) + 12 + int(ihdrLen)
	if cut > len(png) {
		return png
	}
	data := append(append([]byte(key), 0), value...)
	chunk := make([]byte, 0, len(data)+12)
	chunk = binary.BigEndian.AppendUint32(chunk, uint32(len(data)))
	chunk = append(chunk, "tEXt"...)
	chunk = append(chunk, data...)
	chunk = binary.BigEndian.AppendUint32(chunk, crc32.ChecksumIEEE(chunk[4:]))
	out := make([]byte, 0, len(png)+len(chunk))
	out = append(out, png[:cut]...)
	out = append(out, chunk...)
	return append(out, png[cut:]...)
}

// ProbeKey is the tEXt keyword of the vision probe's code.
const ProbeKey = "swk-probe"
