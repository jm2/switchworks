package jsonrpc

import (
	"bufio"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReadLineCap(t *testing.T) {
	in := "short\n" + strings.Repeat("x", 300) + "\nafter\nend"
	br := bufio.NewReaderSize(strings.NewReader(in), 16)
	want := []struct {
		line string
		err  error
	}{{"short\n", nil}, {"", errLineTooLong}, {"after\n", nil}, {"end", io.EOF}}
	for i, w := range want {
		got, err := readLine(br, 100)
		if string(got) != w.line || !errors.Is(err, w.err) && !(w.err == nil && err == nil) {
			t.Fatalf("read %d: got %q, %v; want %q, %v", i, got, err, w.line, w.err)
		}
	}
}
