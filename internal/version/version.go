// SPDX-License-Identifier: GPL-3.0-or-later

// Package version holds the project's name and version in one place, so the
// working name ("Switchworks") can be changed without touching the rest of the
// code. The Go module path and the binary name are the only other places the
// name appears.
package version

const (
	// Name is the human-facing project name (working name).
	Name = "Switchworks"
	// Slug is the lowercase identifier used for client names, file names and
	// directory names.
	Slug = "switchworks"
	// Bin is the binary / CLI name.
	Bin = "swk"
)

// Version is the release version. Overridden at build time with
// -ldflags "-X github.com/jm2/switchworks/internal/version.Version=...".
// It must stay a variable: the linker's -X flag cannot set a constant and
// silently ignores one.
var Version = "0.1.2-dev"
