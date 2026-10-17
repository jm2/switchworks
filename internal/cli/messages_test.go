// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/jm2/switchworks/internal/model"
)

func TestPrintMessagesAttachments(t *testing.T) {
	var b bytes.Buffer
	printMessages(&b, []model.Message{{ID: "m1", Kind: model.MsgNotify, From: "user:cli", To: "h/a", Text: "what does this show?",
		Attachments: []model.Attachment{{Name: "test.png", Size: 589 << 10}, {Size: 3 << 20}}}})
	if want := "[test.png 589 KiB, image 3.0 MiB] what does"; !strings.Contains(b.String(), want) {
		t.Fatalf("table lacks %q:\n%s", want, b.String())
	}
}
