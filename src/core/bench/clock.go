package bench

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// clockPreload moves Date and the fs time surface of every node process by one offset.
//
//go:embed clock.mjs
var clockPreload []byte

const (
	// clockSlack is how far the control's reading may sit from the pin: a node start, not a day.
	clockSlack   = 120 * time.Second
	clockTimeout = time.Minute
	offsetVar    = "FOLIOT_CLOCK_OFFSET_MS"
)

// clockEnv writes the preload into dir and returns the environment that pins a run's node
// processes to at. The offset is computed here, once per run, and every process of the run
// reads the same number: a process that anchored itself at its own start would shift mtimes
// by a different amount than its siblings, which turned a warm index cold (k5 section 6.1).
func clockEnv(dir string, at time.Time) ([]string, error) {
	path := filepath.Join(dir, "clock.mjs")
	if err := os.WriteFile(path, clockPreload, 0o644); err != nil {
		return nil, err
	}
	offset := at.Sub(wallNow()).Milliseconds()
	return []string{`NODE_OPTIONS=--import "` + path + `"`, offsetVar + "=" + strconv.FormatInt(offset, 10)}, nil
}

// clockOffsetMS reads the offset clockEnv put in env back, for the records.
func clockOffsetMS(env []string) int64 {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, offsetVar+"="); ok {
			n, _ := strconv.ParseInt(v, 10, 64)
			return n
		}
	}
	return 0
}

// clockControl reads node's clock under a run's environment and refuses unless it is the pin:
// a preload that did not load, a node that ignores NODE_OPTIONS, or a wrong offset is a
// refusal of the run, never a suite read on the wall clock (R39).
func clockControl(ctx context.Context, env []string, dir string, at time.Time) error {
	ctx, cancel := context.WithTimeout(ctx, clockTimeout)
	defer cancel()
	// Through sh, so node resolves from the run's own PATH as the suite's node does.
	cmd := exec.CommandContext(ctx, "sh", "-c", "node -p 'Date.now()'")
	cmd.Env = mergeEnv(environ(), env)
	cmd.Dir = dir
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	reading := strings.TrimSpace(string(out))
	pin := at.Format(time.RFC3339)
	ms, perr := strconv.ParseInt(reading, 10, 64)
	switch {
	case err != nil:
		return fmt.Errorf("clock control: node read %q for a pin at %s (%v: %s)", reading, pin, err, strings.TrimSpace(stderr.String()))
	case perr != nil:
		return fmt.Errorf("clock control: node read %q for a pin at %s (not a count of milliseconds)", reading, pin)
	}
	read := time.UnixMilli(ms)
	if d := read.Sub(at); d > clockSlack || d < -clockSlack {
		return fmt.Errorf("clock control: node read %s for a pin at %s (%s from the pin, over %s)", read.UTC().Format("2006-01-02T15:04:05.000Z07:00"), pin, d.Round(time.Second), clockSlack)
	}
	return nil
}
