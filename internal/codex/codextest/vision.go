// SPDX-License-Identifier: GPL-3.0-or-later

package codextest

import (
	"encoding/base64"
	"fmt"
	"strings"

	"github.com/jm2/switchworks/internal/attach"
)

// How the fakes and the mock provider "see" images. None of them can read
// pixels, so:
//   - a prompt asking for "the code in this image" (the vision probe) is
//     answered with the code the probe also stores in the PNG's tEXt chunk
//     (attach.ProbeKey);
//   - otherwise the reply lists each image's type, size and SHA-256, so
//     tests can check that the exact bytes arrived.

// SeeImages returns the reply to text with images, and whether there were
// any images to see.
func SeeImages(text string, images [][]byte) (string, bool) {
	if len(images) == 0 {
		return "", false
	}
	if strings.Contains(strings.ToLower(text), "code in this image") {
		for _, im := range images {
			if code := attach.PNGText(im, attach.ProbeKey); code != "" {
				return code, true
			}
		}
		return "I cannot read a code in this image.", true
	}
	var b strings.Builder
	for _, im := range images {
		fmt.Fprintf(&b, "\n[image %s %d bytes sha256=%s]", attach.Sniff(im), len(im), attach.SHA256(im))
	}
	return b.String(), true
}

// DecodeDataURL returns the bytes of a base64 data: URL, or nil.
func DecodeDataURL(u string) []byte {
	if !strings.HasPrefix(u, "data:") {
		return nil
	}
	_, data, ok := strings.Cut(u, ";base64,")
	if !ok {
		return nil
	}
	b, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil
	}
	return b
}
