package log

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Log is the single writer of one home's log. Only Open creates one, and Open
// holds <root>/home.lock for the Log's lifetime, so no other process can append.
type Log struct {
	mu       sync.Mutex
	root     string
	now      func() time.Time
	f        *os.File
	fInfo    os.FileInfo
	lock     *os.File
	lockInfo os.FileInfo
	seq      int64
	size     int64
	opened   int64  // the seq of this process's home.opened, which its home.closed names
	started  string // this process's instant, the one home.lock carries
	broken   error  // set when the file may no longer match seq and size
}

// LockedError names the live process that holds home.lock.
type LockedError struct {
	Holder string // the lock file's content, {pid, started_at}
}

func (e *LockedError) Error() string { return "home.lock is held by a live process: " + e.Holder }

type lockContent struct {
	PID       int    `json:"pid"`
	StartedAt string `json:"started_at"`
}

const tsLayout = "2006-01-02T15:04:05.000Z07:00"

// Open takes home.lock, repairs a torn last line, quarantines unreadable lines,
// and records a takeover of a lock left behind by a process that died holding it
// (a5 sections 1.1, 2.11, 2.12). now supplies every instant the writer records.
func Open(root string, now func() time.Time) (*Log, error) {
	if !filepath.IsAbs(root) {
		return nil, ErrNoRoot
	}
	if err := os.MkdirAll(filepath.Join(root, "log"), 0o700); err != nil {
		return nil, err
	}
	l := &Log{root: root, now: now}
	stale, err := l.takeLock()
	if err != nil {
		return nil, err
	}
	if err := l.load(stale); err != nil {
		l.lock.Close()
		if l.f != nil {
			l.f.Close()
		}
		return nil, err
	}
	// Last, so that a repair or a takeover this open performed is already on the log when
	// the process announces itself, and a failed open announces nothing.
	if l.opened, err = l.append(orchestrator("home.opened", map[string]any{"pid": os.Getpid(), "started_at": l.started})); err != nil {
		l.lock.Close()
		l.f.Close()
		return nil, err
	}
	return l, nil
}

// takeLock uses flock rather than a5 section 2.11's O_EXCL create plus a pid
// liveness probe: two processes that both read a dead pid would both take over
// an O_EXCL lock, while the kernel releases a flock when its holder dies. The
// file still carries {pid, started_at}, and a leftover content still means the
// last holder died holding it.
func (l *Log) takeLock() (stale []byte, err error) {
	path := filepath.Join(l.root, "home.lock")
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		holder, _ := io.ReadAll(f)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, &LockedError{Holder: string(bytes.TrimSpace(holder))}
		}
		return nil, fmt.Errorf("flock %s: %w", path, err)
	}
	stale, err = io.ReadAll(f)
	if err == nil {
		l.started = l.now().UTC().Format(tsLayout)
		content, _ := json.Marshal(lockContent{os.Getpid(), l.started})
		if err = f.Truncate(0); err == nil {
			_, err = f.WriteAt(append(content, '\n'), 0)
		}
	}
	if err == nil {
		l.lockInfo, err = f.Stat()
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	l.lock = f
	return bytes.TrimSpace(stale), nil
}

func (l *Log) load(stale []byte) error {
	path := Path(l.root)
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	l.f = f
	if l.fInfo, err = f.Stat(); err != nil {
		return err
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	s, err := scan(data)
	if err != nil {
		return err
	}
	l.seq, l.size = s.lastSeq, int64(len(data))
	// a5 section 1.1: log.repaired is the first event appended after a torn tail.
	if len(s.torn) > 0 {
		if err := l.repair(s); err != nil {
			return err
		}
	}
	for _, b := range s.unrecorded() {
		if _, err := l.append(orchestrator("log.quarantined", map[string]any{"seq": b.seq, "reason": b.reason})); err != nil {
			return err
		}
	}
	if len(stale) > 0 {
		var prev struct {
			PID       *int    `json:"pid"`
			StartedAt *string `json:"started_at"`
		}
		_ = json.Unmarshal(stale, &prev) // a torn lock file records nulls, not a guess
		data := map[string]any{"stale_pid": prev.PID, "stale_started_at": prev.StartedAt}
		if _, err := l.append(orchestrator("home.lock_taken", data)); err != nil {
			return err
		}
	}
	return nil
}

// repair moves the torn bytes to events.torn.<ts> and cuts the log at its last
// complete line. The moved copy is synced before the cut, so a crash between the
// two leaves the bytes in both places and never in neither.
func (l *Log) repair(s *scanned) error {
	name := "events.torn." + l.now().UTC().Format("20060102T150405.000Z")
	moved := filepath.Join(l.root, "log", name)
	t, err := os.OpenFile(moved, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	_, err = t.Write(s.torn)
	if err == nil {
		err = t.Sync()
	}
	if cerr := t.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = syncDir(filepath.Join(l.root, "log"))
	}
	if err == nil {
		err = l.f.Truncate(s.end)
	}
	if err == nil {
		err = l.f.Sync()
	}
	if err != nil {
		return fmt.Errorf("repair torn tail: %w", err)
	}
	l.size = s.end
	data := map[string]any{"dropped_bytes": len(s.torn), "moved_to": filepath.Join("log", name)}
	_, err = l.append(orchestrator("log.repaired", data))
	return err
}

func orchestrator(typ string, data any) Entry {
	return Entry{Type: typ, Actor: "orchestrator", Data: data}
}

// Append writes one event as one write and returns its seq. An event the
// catalogue refuses is not written and does not consume a seq.
func (l *Log) Append(in Entry) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.append(in)
}

func (l *Log) append(in Entry) (int64, error) {
	if l.broken != nil {
		return 0, l.broken
	}
	if l.f == nil {
		return 0, errors.New("log is closed")
	}
	if err := l.owned(); err != nil {
		l.broken = err
		return 0, err
	}
	data, err := json.Marshal(in.Data)
	if err != nil {
		return 0, fmt.Errorf("%s data: %w", in.Type, err)
	}
	e := Event{
		Seq: l.seq + 1, TS: l.now().UTC().Format(tsLayout), Type: in.Type, Task: in.Task,
		Attempt: in.Attempt, Actor: in.Actor, Cause: in.Cause, Evidence: in.Evidence, Data: data, V: Version,
	}
	if e.Evidence == nil {
		e.Evidence = []Evidence{}
	}
	if err := check(&e); err != nil {
		return 0, fmt.Errorf("refused %s: %w", in.Type, err)
	}
	line, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	line = append(line, '\n')
	if len(line) > MaxEventBytes {
		return 0, fmt.Errorf("refused %s: %d bytes exceeds %d", in.Type, len(line), MaxEventBytes)
	}
	if n, err := l.f.Write(line); err != nil {
		// Cut a partial write so the next event does not land on half a line; if
		// the cut fails too, the next Open repairs it as a torn tail.
		if n > 0 {
			_ = l.f.Truncate(l.size)
		}
		l.broken = fmt.Errorf("append seq %d: %w", e.Seq, err)
		return 0, l.broken
	}
	l.size += int64(len(line))
	l.seq = e.Seq
	if catalogue[e.Type].durable {
		if err := l.f.Sync(); err != nil {
			l.broken = fmt.Errorf("sync seq %d: %w", e.Seq, err)
			return e.Seq, l.broken
		}
	}
	return e.Seq, nil
}

// owned refuses to write when home.lock or events.jsonl at their paths are no
// longer the files this Log opened: a removed lock lets a second writer in, and a
// replaced log would take events nobody reads.
func (l *Log) owned() error {
	for _, c := range []struct {
		path string
		held os.FileInfo
	}{{filepath.Join(l.root, "home.lock"), l.lockInfo}, {Path(l.root), l.fInfo}} {
		now, err := os.Lstat(c.path)
		if err != nil || !os.SameFile(now, c.held) {
			return fmt.Errorf("%s was replaced or removed while this process held it", c.path)
		}
	}
	return nil
}

// Close releases home.lock and empties it, which marks a clean exit.
func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	// A log already broken cannot record its close, and the unpaired open it leaves is
	// then the true reading, so this error never stops the lock being released.
	_, err := l.append(orchestrator("home.closed", map[string]any{"pid": os.Getpid(), "opened_seq": l.opened}))
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	if terr := l.lock.Truncate(0); err == nil {
		err = terr
	}
	if cerr := l.lock.Close(); err == nil {
		err = cerr
	}
	return err
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
