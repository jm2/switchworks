// SPDX-License-Identifier: GPL-3.0-or-later

package attach

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/jm2/switchworks/internal/model"
)

// File attachments: text files (patches, logs, notes) sent with a message.
// The receiving node writes them to disk and names their paths in the
// message (see docs/messaging.md). Base64 makes a message's files about
// 2.7 MiB on the wire.
const (
	MaxFileBytes      = 1 << 20 // one file
	MaxFiles          = 4       // files per message
	MaxFileTotalBytes = 2 << 20 // all files of one message
)

// AllowedFiles lists the file types Switchworks carries.
var AllowedFiles = []string{"text/plain", "text/x-patch", "text/x-diff", "text/markdown", "application/json"}

// SniffText returns the type of a file from its bytes and name: "" unless
// the bytes are text (valid UTF-8 without NUL bytes); otherwise a type the
// name's extension suggests (.patch, .diff, .md, .json, the last only when
// the bytes are JSON), or text/plain.
func SniffText(name string, b []byte) string {
	if bytes.IndexByte(b, 0) >= 0 || !utf8.Valid(b) {
		return ""
	}
	switch strings.ToLower(filepath.Ext(name)) {
	case ".patch":
		return "text/x-patch"
	case ".diff":
		return "text/x-diff"
	case ".md", ".markdown":
		return "text/markdown"
	case ".json":
		if json.Valid(b) {
			return "application/json"
		}
	}
	return "text/plain"
}

// NormalizeFiles is Normalize for file attachments: the type is sniffed
// (a declared type that disagrees is refused), the size and digest are
// computed, and the name is made safe.
func NormalizeFiles(atts []model.Attachment) ([]model.Attachment, error) {
	if len(atts) == 0 {
		return nil, nil
	}
	if len(atts) > MaxFiles {
		return nil, fmt.Errorf("%d files; at most %d per message", len(atts), MaxFiles)
	}
	out := make([]model.Attachment, len(atts))
	total := 0
	for i, a := range atts {
		name := SafeName(a.Name, i, "text/plain")
		if len(a.Data) == 0 {
			return nil, fmt.Errorf("file %d (%s) is empty", i+1, name)
		}
		if len(a.Data) > MaxFileBytes {
			return nil, fmt.Errorf("file %s is %d bytes; the limit is %d (1 MiB)", name, len(a.Data), MaxFileBytes)
		}
		total += len(a.Data)
		mime := SniffText(name, a.Data)
		if mime == "" {
			return nil, fmt.Errorf("file %s is not text (UTF-8 without NUL bytes)", name)
		}
		if a.MIME != "" && !strings.EqualFold(a.MIME, mime) {
			return nil, fmt.Errorf("file %s is declared %s but reads as %s", name, a.MIME, mime)
		}
		sum := SHA256(a.Data)
		if a.SHA256 != "" && a.SHA256 != sum {
			return nil, fmt.Errorf("file %s does not match its sha256", name)
		}
		out[i] = model.Attachment{Name: name, MIME: mime, Size: int64(len(a.Data)), SHA256: sum, Data: a.Data}
	}
	if total > MaxFileTotalBytes {
		return nil, fmt.Errorf("files total %d bytes; the limit per message is %d (2 MiB)", total, MaxFileTotalBytes)
	}
	return out, nil
}

// ReadTextFile reads a file a person names on the command line: a regular
// file, size-capped, that is text.
func ReadTextFile(path string) (model.Attachment, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return model.Attachment{}, err
	}
	if !fi.Mode().IsRegular() {
		return model.Attachment{}, fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Size() > MaxFileBytes {
		return model.Attachment{}, fmt.Errorf("%s is %d bytes; the limit is %d (1 MiB)", path, fi.Size(), MaxFileBytes)
	}
	f, err := os.Open(path)
	if err != nil {
		return model.Attachment{}, err
	}
	defer f.Close()
	return readText(f, filepath.Base(path))
}

func readText(r io.Reader, name string) (model.Attachment, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxFileBytes+1))
	if err != nil {
		return model.Attachment{}, err
	}
	if len(data) > MaxFileBytes {
		return model.Attachment{}, fmt.Errorf("%s is over the 1 MiB limit", name)
	}
	if len(data) == 0 {
		return model.Attachment{}, fmt.Errorf("%s is empty", name)
	}
	safe := SafeName(name, 0, "text/plain")
	mime := SniffText(safe, data)
	if mime == "" {
		return model.Attachment{}, fmt.Errorf("%s is not a text file (UTF-8 without NUL bytes)", name)
	}
	return model.Attachment{Name: safe, MIME: mime, Size: int64(len(data)), SHA256: SHA256(data), Data: data}, nil
}

// DiskNames turns the safe names of a message's files into names to write
// them under in one directory: unique, and none Windows reserves (CON,
// NUL, COM1...) or would change (a trailing dot or space).
func DiskNames(atts []model.Attachment) []string {
	out := make([]string, len(atts))
	used := map[string]bool{}
	for i, a := range atts {
		mime := a.MIME
		if mime == "" {
			mime = "text/plain"
		}
		name := strings.TrimRight(SafeName(a.Name, i, mime), ". ")
		if name == "" {
			name = fmt.Sprintf("file-%d%s", i+1, Ext(mime))
		}
		stem, _, _ := strings.Cut(name, ".")
		if reservedName(strings.TrimSpace(stem)) {
			name = "_" + name
		}
		for base, n := name, 2; used[strings.ToLower(name)]; n++ {
			ext := filepath.Ext(base)
			name = fmt.Sprintf("%s-%d%s", strings.TrimSuffix(base, ext), n, ext)
		}
		used[strings.ToLower(name)] = true
		out[i] = name
	}
	return out
}

func reservedName(stem string) bool {
	switch s := strings.ToUpper(stem); s {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	default:
		return len(s) == 4 && (strings.HasPrefix(s, "COM") || strings.HasPrefix(s, "LPT")) && s[3] >= '0' && s[3] <= '9'
	}
}
