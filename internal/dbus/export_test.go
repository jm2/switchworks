// SPDX-License-Identifier: GPL-3.0-or-later

package dbus

import "io"

// MarshalForTest and ReadForTest expose the wire codec to the external tests.
func MarshalForTest(m *Message) ([]byte, error) { return m.marshal() }
func ReadForTest(r io.Reader) (*Message, error) { return readMessage(r) }
