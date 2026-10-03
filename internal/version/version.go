// SPDX-License-Identifier: GPL-3.0-or-later

// Package version holds the project's name and version in one place, so the
// working name ("Switchyard") can be changed without touching the rest of the
// code. The Go module path and the binary name are the only other places the
// name appears.
package version

const (
	// Name is the human-facing project name (working name).
	Name = "Switchyard"
	// Slug is the lowercase identifier used for client names, file names and
	// directory names.
	Slug = "switchyard"
	// Bin is the binary / CLI name.
	Bin = "swyd"
	// Version is the release version. Overridden at build time with
	// -ldflags "-X github.com/jm2/switchyard/internal/version.Version=..."
	Version = "0.1.0-dev"
)
