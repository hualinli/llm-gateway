package sse

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestReaderParsesMultilineCRLFAndPreservesRaw(t *testing.T) {
	input := ": keep-alive\r\n" +
		"event: message\r\n" +
		"id: 7\r\n" +
		"data: first\r\n" +
		"data: second\r\n\r\n" +
		"data: [DONE]\r\n\r\n"
	r := NewReader(strings.NewReader(input), 0)

	first, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if first.Event != "message" || first.ID != "7" {
		t.Fatalf("event metadata = (%q, %q)", first.Event, first.ID)
	}
	if got := first.DataText(); got != "first\nsecond" {
		t.Fatalf("data=%q", got)
	}
	if string(first.Raw) != input[:strings.Index(input, "\r\n\r\n")+4] {
		t.Fatalf("raw event was not preserved: %q", first.Raw)
	}

	done, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if done.DataText() != "[DONE]" {
		t.Fatalf("done data=%q", done.DataText())
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after final event: got %v, want EOF", err)
	}
}

func TestReaderReturnsFinalEventWithoutDelimiter(t *testing.T) {
	r := NewReader(strings.NewReader("data: final"), 0)
	e, err := r.Next()
	if err != nil {
		t.Fatal(err)
	}
	if e.DataText() != "final" {
		t.Fatalf("data=%q", e.DataText())
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("got %v, want EOF", err)
	}
}

func TestReaderRejectsOversizedEvent(t *testing.T) {
	r := NewReader(strings.NewReader("data: 123456\n\n"), 8)
	if _, err := r.Next(); !errors.Is(err, ErrEventTooLong) {
		t.Fatalf("got %v, want ErrEventTooLong", err)
	}
}
