// Package sse implements a bounded Server-Sent Events reader.
package sse

import (
	"bufio"
	"errors"
	"io"
	"strings"
)

var (
	ErrLineTooLong  = errors.New("sse: line too long")
	ErrEventTooLong = errors.New("sse: event too long")
)

const (
	defaultMaxLineBytes  = 1 << 20 // 1 MiB
	defaultMaxEventBytes = 4 << 20 // 4 MiB
)

// Event is one SSE event. Raw contains the original bytes, including the
// event delimiter when the upstream sent one, so adapters can parse and
// forward without re-serializing the provider's wire format.
type Event struct {
	Event string
	ID    string
	Data  []string
	Raw   []byte
}

func (e Event) DataText() string { return strings.Join(e.Data, "\n") }

// Reader parses SSE framing while keeping a bounded line/event buffer.
type Reader struct {
	br            *bufio.Reader
	maxLineBytes  int
	maxEventBytes int
	eof           bool
}

func NewReader(r io.Reader, maxEventBytes int) *Reader {
	if maxEventBytes <= 0 {
		maxEventBytes = defaultMaxEventBytes
	}
	return &Reader{
		br:            bufio.NewReaderSize(r, 32*1024),
		maxLineBytes:  defaultMaxLineBytes,
		maxEventBytes: maxEventBytes,
	}
}

// Next returns the next dispatched SSE event. A final event without a blank
// line is returned once at EOF; a comment-only block is ignored.
func (r *Reader) Next() (Event, error) {
	if r.eof {
		return Event{}, io.EOF
	}

	var event Event
	var raw []byte
	hasField := false

	for {
		line, err := r.readLine()
		if len(line) > 0 {
			if len(raw)+len(line) > r.maxEventBytes {
				return Event{}, ErrEventTooLong
			}
			raw = append(raw, line...)
		}

		content := strings.TrimSuffix(string(line), "\n")
		content = strings.TrimSuffix(content, "\r")

		if content == "" {
			if hasField {
				event.Raw = append([]byte(nil), raw...)
				return event, nil
			}
			raw = raw[:0]
			if err == io.EOF {
				r.eof = true
				return Event{}, io.EOF
			}
		} else if !strings.HasPrefix(content, ":") {
			field, value := content, ""
			if i := strings.IndexByte(content, ':'); i >= 0 {
				field, value = content[:i], content[i+1:]
				if strings.HasPrefix(value, " ") {
					value = value[1:]
				}
			}
			hasField = true
			switch field {
			case "event":
				event.Event = value
			case "id":
				event.ID = value
			case "data":
				event.Data = append(event.Data, value)
			}
		}

		if err == io.EOF {
			r.eof = true
			if hasField {
				event.Raw = append([]byte(nil), raw...)
				return event, nil
			}
			return Event{}, io.EOF
		}
		if err != nil {
			return Event{}, err
		}
	}
}

func (r *Reader) readLine() ([]byte, error) {
	var line []byte
	for {
		part, err := r.br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > r.maxLineBytes {
			return nil, ErrLineTooLong
		}
		if err == nil {
			return line, nil
		}
		if err == bufio.ErrBufferFull {
			continue
		}
		return line, err
	}
}
