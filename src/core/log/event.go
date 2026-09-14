// Package log owns the only durable truth, <root>/log/events.jsonl, and the fold
// that turns it into state (a5 sections 1.1, 2.1 to 2.4, 2.11, 2.12).
package log

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// Version is the envelope version this binary writes and the highest it reads.
const Version = 1

// MaxEventBytes bounds one encoded event line. Declared default: a5 says evidence
// is a pointer and never inline content, and names no byte bound.
const MaxEventBytes = 1 << 20

// Event is one line of the log, with its keys in the order of a5 section 2.2.
type Event struct {
	Seq      int64           `json:"seq"`
	TS       string          `json:"ts"`
	Type     string          `json:"type"`
	Task     *string         `json:"task"`
	Attempt  *int            `json:"attempt"`
	Actor    string          `json:"actor"`
	Cause    *int64          `json:"cause"`
	Evidence []Evidence      `json:"evidence"`
	Data     json.RawMessage `json:"data"`
	V        int             `json:"v"`
}

// Evidence is a pointer to an artefact, never the artefact.
type Evidence struct {
	Kind string `json:"kind"` // file, sha, url, run or event
	Ref  string `json:"ref"`
}

// Entry is what a caller appends; the writer assigns seq, ts and v.
type Entry struct {
	Type     string
	Task     *string
	Attempt  *int
	Actor    string
	Cause    *int64
	Evidence []Evidence
	Data     any // must encode to a JSON object carrying the type's required fields
}

var taskID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*-[a-z0-9]{2}$`)

var evidenceKinds = map[string]bool{"file": true, "sha": true, "url": true, "run": true, "event": true}

// check validates everything in an event except seq continuity, which only the
// log as a whole can judge. It is shared by append and by the reader, so a line
// the writer would refuse is exactly a line the reader quarantines.
func check(e *Event) error {
	if e.V != Version {
		return fmt.Errorf("envelope version %d, this binary reads %d", e.V, Version)
	}
	c, ok := catalogue[e.Type]
	if !ok {
		return errUnknownType
	}
	if e.Seq < 1 {
		return fmt.Errorf("seq %d is not positive", e.Seq)
	}
	if e.Actor != c.actor {
		return fmt.Errorf("%s is written by %q, not %q", e.Type, c.actor, e.Actor)
	}
	if e.Task != nil && !taskID.MatchString(*e.Task) {
		return fmt.Errorf("task %q is not <kebab-slug>-<2 lowercase alphanumerics>", *e.Task)
	}
	if e.Attempt != nil && *e.Attempt < 1 {
		return fmt.Errorf("attempt %d is not positive", *e.Attempt)
	}
	if e.Cause != nil && (*e.Cause < 1 || *e.Cause >= e.Seq) {
		return fmt.Errorf("cause %d does not name an earlier seq", *e.Cause)
	}
	if e.Evidence == nil {
		return fmt.Errorf("evidence is null, want an array")
	}
	for _, ev := range e.Evidence {
		if !evidenceKinds[ev.Kind] {
			return fmt.Errorf("evidence kind %q is not file, sha, url, run or event", ev.Kind)
		}
		if ev.Ref == "" || strings.ContainsAny(ev.Ref, "\r\n") {
			return fmt.Errorf("evidence ref must be one non-empty line")
		}
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(e.Data, &fields); err != nil || fields == nil {
		return fmt.Errorf("data is not a JSON object")
	}
	for _, f := range c.required {
		if _, ok := fields[f]; !ok {
			return fmt.Errorf("%s is missing required data field %q", e.Type, f)
		}
	}
	return nil
}

var errUnknownType = fmt.Errorf("type is not in the catalogue")
