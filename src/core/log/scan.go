package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
)

// VerifyError is verify's failure: the seq it concerns and why (a5 section 1.1).
type VerifyError struct {
	Seq    int64
	Reason string
}

func (e *VerifyError) Error() string { return fmt.Sprintf("log seq %d: %s", e.Seq, e.Reason) }

// badLine is a complete line the fold cannot use: unparseable, or a catalogued
// type that fails its checks. Its seq is attributed by position when unreadable.
type badLine struct {
	seq    int64
	reason string
}

type scanned struct {
	events      []Event // lines that passed check, in seq order
	bad         []badLine
	quarantined map[int64]bool // seqs already named by a log.quarantined event
	lastSeq     int64
	end         int64  // offset just past the last newline
	torn        []byte // bytes after end: a write that never completed
}

// scan reads a whole log. It returns an error only for what no repair may touch
// without editing history: a seq out of order, a gap no unreadable line explains,
// or an envelope newer than this binary (a5 section 3.4: refused, never guessed at).
func scan(data []byte) (*scanned, error) {
	s := &scanned{quarantined: map[int64]bool{}}
	end := bytes.LastIndexByte(data, '\n') + 1
	s.end, s.torn = int64(end), data[end:]

	type line struct {
		e      Event
		reason string // non-empty when the line carries no usable seq
	}
	var lines []line
	for raw := range bytes.Lines(data[:end]) {
		var l line
		if err := json.Unmarshal(raw, &l.e); err != nil {
			l.reason = "unparseable: " + err.Error()
		} else if l.e.Seq < 1 {
			l.reason = "no positive seq"
		}
		lines = append(lines, l)
	}

	var unreadable []string // reasons of unusable lines since the last parsed one
	for i, l := range lines {
		e := l.e
		if l.reason != "" {
			unreadable = append(unreadable, l.reason)
			continue
		}
		want := s.lastSeq + 1 + int64(len(unreadable))
		if e.V > Version {
			return nil, &VerifyError{e.Seq, fmt.Sprintf("envelope version %d is newer than this binary reads (%d)", e.V, Version)}
		}
		if e.Seq != want {
			// A corrupted seq is told from a deleted or reordered line by the next
			// usable line: only when it sits exactly where position says is this
			// line unreadable rather than the log out of order.
			confirmed := false
			for d, n := range lines[i+1:] {
				if n.reason == "" {
					confirmed = n.e.Seq == want+int64(d+1)
					break
				}
			}
			if confirmed {
				unreadable = append(unreadable, fmt.Sprintf("seq %d where seq %d belongs", e.Seq, want))
				continue
			}
			return nil, &VerifyError{want, fmt.Sprintf("expected seq %d, found %d after seq %d and %d unreadable lines", want, e.Seq, s.lastSeq, len(unreadable))}
		}
		s.flush(unreadable)
		unreadable = nil
		s.lastSeq = e.Seq
		switch err := check(&e); {
		case errors.Is(err, errUnknownType):
			// a5 section 2.2: a reader skips an unknown type.
		case err != nil:
			s.bad = append(s.bad, badLine{e.Seq, err.Error()})
		default:
			s.events = append(s.events, e)
			if e.Type == "log.quarantined" {
				var q struct{ Seq int64 }
				if json.Unmarshal(e.Data, &q) == nil {
					s.quarantined[q.Seq] = true
				}
			}
		}
	}
	s.flush(unreadable)
	return s, nil
}

func (s *scanned) flush(reasons []string) {
	for _, r := range reasons {
		s.lastSeq++
		s.bad = append(s.bad, badLine{s.lastSeq, r})
	}
}

// unrecorded lists bad lines no log.quarantined event names yet.
func (s *scanned) unrecorded() []badLine {
	var out []badLine
	for _, b := range s.bad {
		if !s.quarantined[b.seq] {
			out = append(out, b)
		}
	}
	return out
}

// Read returns every readable event with seq >= from. It takes no lock and repairs
// nothing: an incomplete last line is not yet an event, and an unreadable line is
// skipped exactly as the fold skips it.
func Read(path string, from int64) ([]Event, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := scan(data)
	if err != nil {
		return nil, err
	}
	for i, e := range s.events {
		if e.Seq >= from {
			return s.events[i:], nil
		}
	}
	return nil, nil
}

// Verify reports the first problem in a log, or nil when it is gapless, strictly
// increasing, complete to its last line, and every unreadable line is quarantined.
func Verify(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	s, err := scan(data)
	if err != nil {
		return err
	}
	if bad := s.unrecorded(); len(bad) > 0 {
		return &VerifyError{bad[0].seq, "not quarantined: " + bad[0].reason}
	}
	if len(s.torn) > 0 {
		return &VerifyError{s.lastSeq + 1, fmt.Sprintf("torn last line, %d bytes with no newline", len(s.torn))}
	}
	return nil
}
