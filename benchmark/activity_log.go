package benchmark

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

const (
	maxLogRecords  = 20000
	maxLogLineSize = 4096
)

// Log is the append-only operator activity log: the source of truth for human
// attention, independent of every aggregate derived from it. Lines are only
// ever appended; nothing here rewrites or truncates the file.
//
// ponytail: no cross-process lock. One operator runs one CLI at a time; two
// concurrent appends could both pass validation. Add flock if that changes.
type Log struct{ Path string }

// Records reads the log file. A missing file is an empty log.
func (l Log) Records() ([]Record, error) {
	f, err := os.Open(l.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open activity log: %w", err)
	}
	defer f.Close()
	return ParseRecords(f)
}

// ParseRecords reads every line, refusing malformed, unknown-field or oversized
// content.
func ParseRecords(in io.Reader) ([]Record, error) {
	var records []Record
	reader := bufio.NewReaderSize(in, maxLogLineSize)
	for line := 1; ; line++ {
		raw, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, fmt.Errorf("activity log line %d exceeds %d bytes", line, maxLogLineSize)
		}
		if len(bytes.TrimSpace(raw)) > 0 {
			if len(records) >= maxLogRecords {
				return nil, fmt.Errorf("activity log exceeds %d records", maxLogRecords)
			}
			var r Record
			dec := json.NewDecoder(bytes.NewReader(raw))
			dec.DisallowUnknownFields()
			if decodeErr := dec.Decode(&r); decodeErr != nil {
				return nil, fmt.Errorf("activity log line %d: %w", line, decodeErr)
			}
			records = append(records, r)
		}
		if errors.Is(err, io.EOF) {
			return records, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read activity log: %w", err)
		}
	}
}

// Ledger reads and validates the whole log.
func (l Log) Ledger() (Ledger, error) {
	records, err := l.Records()
	if err != nil {
		return Ledger{}, err
	}
	return Fold(records)
}

// Append validates the log WITH the new record and writes it only if the result
// is consistent, so an invalid entry never reaches the file.
func (l Log) Append(r Record) error {
	records, err := l.Records()
	if err != nil {
		return err
	}
	if _, err := Fold(append(records, r)); err != nil {
		return fmt.Errorf("refused: %w", err)
	}
	line, err := json.Marshal(r)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(l.Path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open activity log: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		f.Close()
		return fmt.Errorf("append activity log: %w", err)
	}
	return f.Close()
}

// Start opens an interval at the given time and returns its entry id.
func (l Log) Start(arm Arm, operator string, kind Kind, relay RelayKind, note string, at time.Time) (string, error) {
	at = at.UTC()
	id := EntryID(arm, operator, kind, relay, at)
	return id, l.Append(Record{SchemaVersion: ActivitySchemaVersion, Op: OpStart, EntryID: id, At: at,
		Arm: arm, Operator: operator, Kind: kind, RelayKind: relay, Note: note})
}

// Stop closes an interval. An empty id closes the single open interval; it is
// refused when none or several are open, rather than guessing.
func (l Log) Stop(id string, at time.Time) (string, error) {
	if id == "" {
		ledger, err := l.Ledger()
		if err != nil {
			return "", err
		}
		for _, in := range ledger.Intervals {
			if in.End != nil {
				continue
			}
			if id != "" {
				return "", errors.New("several intervals are open; name the entry id to stop")
			}
			id = in.ID
		}
		if id == "" {
			return "", errors.New("no open interval to stop")
		}
	}
	return id, l.Append(Record{SchemaVersion: ActivitySchemaVersion, Op: OpStop, EntryID: id, At: at.UTC()})
}

// Audit attaches independent verification to a relay entry.
func (l Log) Audit(id, auditor, evidenceSHA256 string, at time.Time) error {
	return l.Append(Record{SchemaVersion: ActivitySchemaVersion, Op: OpAudit, EntryID: id, At: at.UTC(),
		Auditor: auditor, EvidenceSHA256: evidenceSHA256})
}
