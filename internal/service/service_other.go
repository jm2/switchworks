// SPDX-License-Identifier: GPL-3.0-or-later

//go:build !linux && !darwin && !windows

package service

import (
	"errors"
	"io"
	"os/exec"
)

const managerName = "none"

var errUnsupported = errors.New("no supported service manager on this platform; run `swyd node run` from your own init system, as your user")

func unitName(role string) string                   { return "swyd-" + role }
func command(name string, args ...string) *exec.Cmd { return exec.Command(name, args...) }
func query(string) (state, error)                   { return state{}, errUnsupported }
func install(string, string, bool, io.Writer) error { return errUnsupported }
func uninstall(string, io.Writer) error             { return errUnsupported }
func start(string) error                            { return errUnsupported }
func stop(string) error                             { return errUnsupported }
func restart(string) error                          { return errUnsupported }
