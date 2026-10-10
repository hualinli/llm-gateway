package metering

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"

	"llm-gateway/internal/metrics"
)

// The durable spool keeps usage events while Redis cannot take them. It is a
// JSONL file with at most two live files:
//
//   - <spool>            the active file. Appends land here under spoolMu.
//   - <spool>.replaying  the segment being drained. Replay obtains it with one
//     rename of the active file, so appends never touch it and replay never
//     rewrites it except to repair a corrupt file.
//
// Replay delivers the segment in batches and remembers only the byte offset of
// the next undelivered record. A batch costs O(batch) work, the file is never
// rewritten per batch (the earlier design rewrote the whole remainder after
// every batch, which is O(N²) in backlog size), and spoolMu is never held across
// Redis I/O. Delivery is at-least-once: a crash after a publish but before the
// segment is removed replays that prefix, and so does a repair that runs after a
// malformed record turns up mid-replay (the repair rewrites the segment, so the
// offset restarts at zero). The ledger's unique event_id absorbs the duplicates.
// Quarantined bytes always live under <spool>.corrupt-*.

const defaultSpoolBatch = 50

// spoolReplay is the replay cursor. Only the producer's loop goroutine reads or
// writes it, so it needs no lock.
type spoolReplay struct {
	// validated is false until the segment has been repaired. Repair runs once
	// per segment, and again after a malformed record turns up mid-replay.
	validated bool
	// offset is the byte position of the next undelivered record.
	offset int64
}

func (p *Producer) replayPath() string {
	return p.spoolPath + ".replaying"
}

func (p *Producer) batchSize() int {
	if p.batch > 0 {
		return p.batch
	}
	return defaultSpoolBatch
}

func (p *Producer) appendSpool(events []Event) error {
	p.spoolMu.Lock()
	defer p.spoolMu.Unlock()
	return p.appendSpoolLocked(events)
}

func (p *Producer) appendSpoolLocked(events []Event) error {
	if len(events) == 0 {
		return nil
	}
	if p.spoolPath == "" {
		return errors.New("metering spool is not configured")
	}
	if !p.spoolRecovered {
		// Only the active file needs repair before an append: a torn tail left
		// by a crash must not be joined by the next record. A segment is
		// repaired when it is drained.
		if err := repairSpoolFile(p.spoolPath, p.spoolPath); err != nil {
			return err
		}
		p.spoolRecovered = true
	}
	if err := os.MkdirAll(filepath.Dir(p.spoolPath), 0o700); err != nil {
		return err
	}
	var payload bytes.Buffer
	enc := json.NewEncoder(&payload)
	for _, e := range events {
		if err := enc.Encode(e); err != nil {
			return err
		}
	}
	used, err := p.spoolFootprint()
	if err != nil {
		return err
	}
	if used+int64(payload.Len()) > p.maxSpoolSize {
		return fmt.Errorf("spool and quarantine size %d exceeds limit %d", used+int64(payload.Len()), p.maxSpoolSize)
	}

	f, err := os.OpenFile(p.spoolPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if n, err := f.Write(payload.Bytes()); err != nil || n != payload.Len() {
		p.spoolRecovered = false
		if err == nil {
			err = io.ErrShortWrite
		}
		return err
	}
	if err := f.Sync(); err != nil {
		p.spoolRecovered = false
		return err
	}
	if err := syncSpoolDirectory(p.spoolPath); err != nil {
		return err
	}
	p.refreshSpoolGauge()
	return nil
}

// spoolFootprint counts every byte the spool owns: both live files plus all
// quarantined bytes. Quarantine is included so corrupt data cannot slip past the
// capacity limit.
func (p *Producer) spoolFootprint() (int64, error) {
	var total int64
	for _, path := range []string{p.spoolPath, p.replayPath()} {
		size, err := fileSize(path)
		if err != nil {
			return 0, err
		}
		total += size
	}
	quarantined, err := quarantineBytes(p.spoolPath)
	if err != nil {
		return 0, err
	}
	return total + quarantined, nil
}

// refreshSpoolGauge publishes the bytes of live spool data (not quarantine).
// Callers hold spoolMu, or run before the loop starts.
func (p *Producer) refreshSpoolGauge() {
	if p.spoolPath == "" {
		return
	}
	var total int64
	for _, path := range []string{p.spoolPath, p.replayPath()} {
		if size, err := fileSize(path); err == nil {
			total += size
		}
	}
	metrics.MeteringSpoolBytes.Set(float64(total))
}

// replaySpool drains the spool oldest-first. It runs at the start of every
// flush, so a restart or a Redis recovery resumes without new traffic. Each
// call is bounded by its context: a backlog that outlasts the budget resumes at
// the saved offset on the next flush.
func (p *Producer) replaySpool(ctx context.Context) error {
	if p.spoolPath == "" {
		return nil
	}
	for {
		available, err := p.beginSegment()
		if err != nil || !available {
			return err
		}
		if err := p.drainSegment(ctx); err != nil {
			return err
		}
	}
}

// beginSegment reports whether a segment is ready to drain. If none exists and
// the active file has data, the active file is renamed to the segment name. That
// is one metadata operation under spoolMu: no copy and no network I/O.
func (p *Producer) beginSegment() (bool, error) {
	p.spoolMu.Lock()
	defer p.spoolMu.Unlock()
	if _, err := os.Stat(p.replayPath()); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if _, err := os.Stat(p.spoolPath); errors.Is(err, os.ErrNotExist) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.Rename(p.spoolPath, p.replayPath()); err != nil {
		return false, err
	}
	p.replay = spoolReplay{}
	p.refreshSpoolGauge()
	if err := syncSpoolDirectory(p.replayPath()); err != nil {
		return false, err
	}
	return true, nil
}

// drainSegment publishes the segment from the saved offset until it is empty,
// then removes it. A failed publish leaves the offset in place, so the next
// attempt retries the same batch.
func (p *Producer) drainSegment(ctx context.Context) error {
	path := p.replayPath()
	if !p.replay.validated {
		// Repair rewrites the segment, so every offset is invalid afterwards:
		// start from zero. Records already published are published again, and
		// the ledger's event_id uniqueness drops them.
		if err := repairSpoolFile(path, p.spoolPath); err != nil {
			return err
		}
		p.replay.validated = true
		p.replay.offset = 0
	}
	limit := p.batchSize()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		events, next, err := readSpoolBatch(path, p.replay.offset, limit)
		if err != nil {
			// Offsets are only trusted after a repair pass, so the next attempt
			// repairs the segment before reading it again.
			p.replay.validated = false
			return err
		}
		if len(events) == 0 {
			return p.removeSegment()
		}
		if err := p.flushBatch(ctx, events); err != nil {
			return err
		}
		metrics.MeteringSpoolReplayTotal.Inc()
		p.replay.offset = next
	}
}

// removeSegment deletes a fully delivered segment. The directory sync makes the
// removal durable.
func (p *Producer) removeSegment() error {
	p.spoolMu.Lock()
	defer p.spoolMu.Unlock()
	if err := os.Remove(p.replayPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	p.replay = spoolReplay{}
	p.refreshSpoolGauge()
	return syncSpoolDirectory(p.replayPath())
}

// readSpoolBatch returns up to limit valid events starting at byte offset, along
// with the offset just past the last line consumed. A malformed record is an
// error rather than a silent skip: the caller repairs the file before trusting
// its offsets again.
func readSpoolBatch(path string, offset int64, limit int) ([]Event, int64, error) {
	reader, err := openSpoolReader(path, offset)
	if errors.Is(err, os.ErrNotExist) {
		return nil, offset, nil
	}
	if err != nil {
		return nil, offset, err
	}
	defer reader.Close()

	events := make([]Event, 0, limit)
	next := offset
	for len(events) < limit {
		line, err := reader.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, offset, err
		}
		event, kind := classifySpoolRecord(line)
		switch kind {
		case spoolCorrupt:
			return nil, offset, fmt.Errorf("spool %s: malformed record at byte %d", path, reader.offset-int64(len(line)))
		case spoolValid:
			events = append(events, event)
		}
		next = reader.offset
	}
	return events, next, nil
}

// repairSpoolFile makes a spool file safe to append to and to replay. Corrupt
// records move to a content-addressed quarantine named after quarantineBase.
// Valid records are kept in their original order, and a final record that lacks
// its newline gets one. If the quarantine cannot be written, the file is left
// untouched.
func repairSpoolFile(path, quarantineBase string) error {
	scan, err := scanSpoolFile(path)
	if err != nil {
		return err
	}
	if scan.corrupt == 0 && !scan.missingDelimiter {
		return nil
	}
	if scan.corrupt > 0 {
		quarantinePath := fmt.Sprintf("%s.corrupt-%s", quarantineBase, scan.corruptDigest)
		if err := writeSpoolAtomically(quarantinePath, func(w io.Writer) error {
			return copySpoolRecords(path, w, spoolCorrupt)
		}); err != nil {
			return fmt.Errorf("quarantine corrupt spool: %w", err)
		}
		metrics.MeteringSpoolCorruptRecordsTotal.Add(float64(scan.corrupt))
		log.Printf("metering quarantined corrupt spool: valid_records=%d corrupt_records=%d quarantine=%s", scan.valid, scan.corrupt, quarantinePath)
	}
	if scan.valid == 0 {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return syncSpoolDirectory(path)
	}
	return writeSpoolAtomically(path, func(w io.Writer) error {
		return copySpoolRecords(path, w, spoolValid)
	})
}

// spoolScan summarises a spool file without retaining its records.
type spoolScan struct {
	valid            int
	corrupt          int
	missingDelimiter bool   // the final record has no trailing newline
	corruptDigest    string // sha256 over the corrupt lines, in file order
}

func scanSpoolFile(path string) (spoolScan, error) {
	var scan spoolScan
	reader, err := openSpoolReader(path, 0)
	if errors.Is(err, os.ErrNotExist) {
		return scan, nil
	}
	if err != nil {
		return scan, err
	}
	defer reader.Close()

	digest := sha256.New()
	for {
		line, err := reader.next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return scan, err
		}
		if !bytes.HasSuffix(line, []byte{'\n'}) {
			scan.missingDelimiter = true
		}
		_, kind := classifySpoolRecord(line)
		switch kind {
		case spoolValid:
			scan.valid++
		case spoolCorrupt:
			scan.corrupt++
			digest.Write(line)
		}
	}
	scan.corruptDigest = fmt.Sprintf("%x", digest.Sum(nil))
	return scan, nil
}

// copySpoolRecords streams the records of one kind from path into w. Valid
// records get exactly one trailing newline. Corrupt records are copied
// byte-for-byte, so the quarantine's contents match the digest in its name.
func copySpoolRecords(path string, w io.Writer, want spoolRecordKind) error {
	reader, err := openSpoolReader(path, 0)
	if err != nil {
		return err
	}
	defer reader.Close()
	for {
		line, err := reader.next()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if _, kind := classifySpoolRecord(line); kind != want {
			continue
		}
		if want == spoolValid && !bytes.HasSuffix(line, []byte{'\n'}) {
			line = append(line, '\n')
		}
		if _, err := w.Write(line); err != nil {
			return err
		}
	}
}

type spoolRecordKind int

const (
	spoolBlank spoolRecordKind = iota
	spoolValid
	spoolCorrupt
)

// classifySpoolRecord decodes one physical line. Blank lines are ignored, and
// anything that is not one Event JSON object is corrupt.
func classifySpoolRecord(line []byte) (Event, spoolRecordKind) {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return Event{}, spoolBlank
	}
	var event Event
	if err := json.Unmarshal(trimmed, &event); err != nil {
		return Event{}, spoolCorrupt
	}
	return event, spoolValid
}

// spoolReader yields the physical lines of a spool file one at a time, so
// neither replay nor repair holds the whole file in memory.
type spoolReader struct {
	file   *os.File
	reader *bufio.Reader
	offset int64 // byte offset just past the last line returned
}

func openSpoolReader(path string, offset int64) (*spoolReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return &spoolReader{file: f, reader: bufio.NewReader(f), offset: offset}, nil
}

func (s *spoolReader) Close() error {
	return s.file.Close()
}

// next returns the next physical line, including its newline when one is
// present. It returns io.EOF when nothing is left.
func (s *spoolReader) next() ([]byte, error) {
	line, err := s.reader.ReadBytes('\n')
	if len(line) == 0 {
		return nil, err
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	s.offset += int64(len(line))
	return line, nil
}

// quarantineBytes totals the quarantined bytes of every file derived from path.
func quarantineBytes(path string) (int64, error) {
	paths, err := filepath.Glob(path + ".corrupt-*")
	if err != nil {
		return 0, err
	}
	var total int64
	for _, name := range paths {
		info, err := os.Stat(name)
		if err != nil {
			return 0, err
		}
		total += info.Size()
	}
	return total, nil
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// writeSpoolAtomically writes through a temp file in the same directory, then
// renames it into place and syncs the directory, so readers never observe a
// partially written file.
func writeSpoolAtomically(path string, write func(io.Writer) error) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".events-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	defer cleanup()

	if err := write(tmp); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncSpoolDirectory(path)
}

func syncSpoolDirectory(path string) error {
	dir, err := os.Open(filepath.Dir(path))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
