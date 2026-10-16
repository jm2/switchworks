// SPDX-License-Identifier: GPL-3.0-or-later

// Package attach validates image attachments: the allowed types (sniffed
// from the bytes, never taken from the sender), the size limits, and safe
// display names. It also reads attachment files for the CLI and, under a
// directory an agent may use, for the node (ReadWithin).
package attach

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/model"
)

// Limits. Base64 makes a message's images about 13.4 MiB on the wire, well
// under wire.MaxLine (32 MiB); events only ever carry the metadata.
const (
	MaxImageBytes = 5 << 20  // one image
	MaxImages     = 4        // images per message or spawn prompt
	MaxTotalBytes = 10 << 20 // all images of one message
	maxNameLen    = 100
)

// Allowed lists the image types Switchworks carries.
var Allowed = []string{"image/png", "image/jpeg", "image/webp", "image/gif"}

// Sniff returns the image type of b from its magic number, or "" when b is
// not one of the allowed types.
func Sniff(b []byte) string {
	switch {
	case bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(b, []byte{0xFF, 0xD8, 0xFF}):
		return "image/jpeg"
	case bytes.HasPrefix(b, []byte("GIF87a")), bytes.HasPrefix(b, []byte("GIF89a")):
		return "image/gif"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}

// AllowedType reports whether mime is one of the allowed image types.
func AllowedType(mime string) bool {
	for _, a := range Allowed {
		if a == mime {
			return true
		}
	}
	return false
}

// Ext is a file extension for an allowed type.
func Ext(mime string) string {
	switch mime {
	case "image/png":
		return ".png"
	case "image/jpeg":
		return ".jpg"
	case "image/webp":
		return ".webp"
	case "image/gif":
		return ".gif"
	}
	return ".bin"
}

// SafeName turns a sender's file name into a display name: the base name
// only, printable characters, at most 100 bytes. It is never used as a path.
func SafeName(name string, i int, mime string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	if j := strings.LastIndex(name, "/"); j >= 0 {
		name = name[j+1:]
	}
	var b strings.Builder
	for _, r := range name {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), unicode.Is(unicode.Cf, r):
		case unicode.IsLetter(r), unicode.IsDigit(r), strings.ContainsRune("._- +()", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := strings.TrimLeft(strings.TrimSpace(b.String()), ".")
	for len(out) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size]
	}
	if out == "" {
		out = fmt.Sprintf("image-%d%s", i+1, Ext(mime))
	}
	return out
}

// SHA256 is the hex digest of b.
func SHA256(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ValidSHA256 reports whether s is a lowercase hex SHA-256.
func ValidSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Normalize checks attachments that carry data and fills in their
// metadata: the type is sniffed (a declared type that disagrees is
// refused), the size and digest are computed, and the name is made safe.
func Normalize(atts []model.Attachment) ([]model.Attachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	if len(atts) > MaxImages {
		return nil, fmt.Errorf("%d images; at most %d per message", len(atts), MaxImages)
	}
	out := make([]model.Attachment, len(atts))
	total := 0
	for i, a := range atts {
		if len(a.Data) == 0 {
			return nil, fmt.Errorf("image %d (%s) has no data", i+1, SafeName(a.Name, i, ""))
		}
		if len(a.Data) > MaxImageBytes {
			return nil, fmt.Errorf("image %s is %d bytes; the limit is %d (5 MiB)", SafeName(a.Name, i, ""), len(a.Data), MaxImageBytes)
		}
		total += len(a.Data)
		mime := Sniff(a.Data)
		if mime == "" {
			return nil, fmt.Errorf("image %s is not a PNG, JPEG, WebP or GIF image", SafeName(a.Name, i, ""))
		}
		if a.MIME != "" && !strings.EqualFold(a.MIME, mime) {
			return nil, fmt.Errorf("image %s is declared %s but its data is %s", SafeName(a.Name, i, mime), a.MIME, mime)
		}
		sum := SHA256(a.Data)
		if a.SHA256 != "" && a.SHA256 != sum {
			return nil, fmt.Errorf("image %s does not match its sha256", SafeName(a.Name, i, mime))
		}
		out[i] = model.Attachment{Name: SafeName(a.Name, i, mime), MIME: mime, Size: int64(len(a.Data)), SHA256: sum, Data: a.Data}
	}
	if total > MaxTotalBytes {
		return nil, fmt.Errorf("images total %d bytes; the limit per message is %d (10 MiB)", total, MaxTotalBytes)
	}
	return out, nil
}

// TotalBytes is the size of the images' data together.
func TotalBytes(atts []model.Attachment) int {
	n := 0
	for _, a := range atts {
		n += len(a.Data)
	}
	return n
}

// Summary describes attachments in one line, for message headers and logs.
func Summary(atts []model.Attachment) string {
	parts := make([]string, len(atts))
	for i, a := range atts {
		parts[i] = fmt.Sprintf("%s (%s, %s)", a.Name, a.MIME, HumanSize(a.Size))
	}
	return strings.Join(parts, ", ")
}

// HumanSize formats a byte count.
func HumanSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}

// ReadFile reads an image file a person names on the command line: a
// regular file, size-capped, of an allowed type.
func ReadFile(path string) (model.Attachment, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return model.Attachment{}, err
	}
	if !fi.Mode().IsRegular() {
		return model.Attachment{}, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > MaxImageBytes {
		return model.Attachment{}, fmt.Errorf("%s is %d bytes; the limit is %d (5 MiB)", path, fi.Size(), MaxImageBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return model.Attachment{}, err
	}
	defer f.Close()
	return readImage(f, filepath.Base(path))
}

func readImage(r io.Reader, name string) (model.Attachment, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxImageBytes+1))
	if err != nil {
		return model.Attachment{}, err
	}
	if len(data) > MaxImageBytes {
		return model.Attachment{}, fmt.Errorf("%s is over the 5 MiB limit", name)
	}
	mime := Sniff(data)
	if mime == "" {
		return model.Attachment{}, fmt.Errorf("%s is not a PNG, JPEG, WebP or GIF image", name)
	}
	return model.Attachment{Name: SafeName(name, 0, mime), MIME: mime, Size: int64(len(data)), SHA256: SHA256(data), Data: data}, nil
}
