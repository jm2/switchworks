// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !windows

package service

import (
	"errors"
	"io"
)

const (
	systemManagerName = managerName
	systemUnitName    = ""
)

var errSystemUnsupported = errors.New("the coordinator's system service is available on Linux and Windows; here, run it as a user of its own (see the README)")

func systemHome() string                          { return "" }
func querySystem() (state, error)                 { return state{}, errSystemUnsupported }
func installSystem(string, bool, io.Writer) error { return errSystemUnsupported }
func uninstallSystem(io.Writer) error             { return errSystemUnsupported }
func startSystem() error                          { return errSystemUnsupported }
func stopSystem() error                           { return errSystemUnsupported }
func restartSystem() error                        { return errSystemUnsupported }
func enterSystem([]string, io.Writer) (int, bool) { return 1, true }
