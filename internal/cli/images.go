// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"flag"
	"fmt"
	"strings"

	attachpkg "github.com/jm2/switchworks/internal/attach"
	"github.com/jm2/switchworks/internal/model"
)

// imageFlag collects repeated --image FILE flags.
type imageFlag []string

func (f *imageFlag) String() string { return strings.Join(*f, ",") }

func (f *imageFlag) Set(v string) error {
	*f = append(*f, v)
	return nil
}

// addImageFlag registers --image on fs.
func addImageFlag(fs *flag.FlagSet) *imageFlag {
	var f imageFlag
	fs.Var(&f, "image", "attach an image file (PNG, JPEG, WebP or GIF; repeat for up to 4, 5 MiB each, 10 MiB in all)")
	return &f
}

// readImages reads the named files (on this machine) as attachments,
// checking type and limits before anything is sent.
func readImages(files []string) ([]model.Attachment, error) {
	if len(files) == 0 {
		return nil, nil
	}
	atts := make([]model.Attachment, 0, len(files))
	for _, p := range files {
		a, err := attachpkg.ReadFile(p)
		if err != nil {
			return nil, fmt.Errorf("--image %s: %w", p, err)
		}
		atts = append(atts, a)
	}
	return attachpkg.Normalize(atts)
}

// addFileFlag registers --file on fs.
func addFileFlag(fs *flag.FlagSet) *imageFlag {
	var f imageFlag
	fs.Var(&f, "file", "attach a text file, such as a patch or a log (UTF-8; repeat for up to 4, 1 MiB each, 2 MiB in all); agent sessions only")
	return &f
}

// readFiles reads the named text files (on this machine) as file
// attachments, checking type and limits before anything is sent.
func readFiles(files []string) ([]model.Attachment, error) {
	if len(files) == 0 {
		return nil, nil
	}
	atts := make([]model.Attachment, 0, len(files))
	for _, p := range files {
		a, err := attachpkg.ReadTextFile(p)
		if err != nil {
			return nil, fmt.Errorf("--file %s: %w", p, err)
		}
		atts = append(atts, a)
	}
	return attachpkg.NormalizeFiles(atts)
}
