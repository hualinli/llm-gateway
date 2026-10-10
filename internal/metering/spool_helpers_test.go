package metering

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// readSpool decodes a spool file for assertions. A missing file is empty. Any
// malformed record is reported as an error, after the valid records are returned.
func readSpool(path string) ([]Event, error) {
	reader, err := openSpoolReader(path, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer reader.Close()

	var events []Event
	corrupt := 0
	for {
		line, err := reader.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		event, kind := classifySpoolRecord(line)
		switch kind {
		case spoolValid:
			events = append(events, event)
		case spoolCorrupt:
			corrupt++
		}
	}
	if corrupt > 0 {
		return events, fmt.Errorf("spool contains %d malformed records", corrupt)
	}
	return events, nil
}

// rewriteSpool replaces a spool file with the given events. It is a test
// helper: production repairs files in place and never re-encodes records.
func rewriteSpool(path string, events []Event) error {
	if len(events) == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncSpoolDirectory(path)
	}
	return writeSpoolAtomically(path, func(w io.Writer) error {
		enc := json.NewEncoder(w)
		for _, event := range events {
			if err := enc.Encode(event); err != nil {
				return err
			}
		}
		return nil
	})
}
