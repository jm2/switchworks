// SPDX-License-Identifier: GPL-3.0-or-later

package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

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

// A long label is not cut by the text's limit: every attachment shows.
func TestPrintMessagesLongAttachmentNames(t *testing.T) {
	var b bytes.Buffer
	long := strings.Repeat("screenshot-", 6) + "1.png"
	printMessages(&b, []model.Message{{ID: "m1", Kind: model.MsgNotify, From: "user:cli", To: "h/a", Text: strings.Repeat("word ", 30),
		Attachments: []model.Attachment{{Name: long, Size: 1 << 10}, {Name: "second.png", Size: 2 << 10}}}})
	if !strings.Contains(b.String(), "second.png 2 KiB]") || !strings.Contains(b.String(), "…") {
		t.Fatalf("table:\n%s", b.String())
	}
}

// Dismissed asks and read messages say so in the table.
func TestPrintMessagesDismissedAndRead(t *testing.T) {
	var b bytes.Buffer
	now := time.Now()
	printMessages(&b, []model.Message{
		{ID: "m1", Kind: model.MsgAsk, From: "h/a", To: "user:jm", State: model.MsgDismissed, Inline: true, ReadAt: now, Text: "q?"},
		{ID: "m2", Kind: model.MsgNotify, From: "h/a", To: "user:jm", State: model.MsgDelivered, ReadAt: now, Text: "done"}})
	out := b.String()
	if !strings.Contains(out, "dismissed (inline) ") || strings.Contains(out, "dismissed (inline) (read)") || !strings.Contains(out, "delivered (read)") {
		t.Fatalf("table:\n%s", out)
	}
}
