package bench

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// historyFreeClone makes a worker's checkout holding base_sha and no other commit. The
// file:// scheme is what makes --depth bind: over a bare path git prints "--depth is
// ignored in local clones" and the landed commit is present (Fable critique k3 section 1.2).
func historyFreeClone(ctx context.Context, mirror, dir, sha string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	url := "file://" + filepath.ToSlash(mirror)
	_, err := gitOut(ctx, filepath.Dir(dir), "-c", "protocol.file.allow=always", "clone", "--quiet", "--revision="+sha, "--depth", "1", "--no-tags", url, dir)
	return err
}

// CheckoutHistory is the pre-launch reading of a worker's checkout.
type CheckoutHistory struct {
	// LandedObjectExit is `git cat-file -e <landed_sha>`: 1 means the object is absent,
	// 0 present, anything else a failed reading.
	LandedObjectExit int
	Commits          int
	Refs             int
}

// Free is true only when the landed commit is absent and exactly one commit is reachable.
func (h CheckoutHistory) Free() bool { return h.LandedObjectExit == 1 && h.Commits == 1 }

// assertHistoryFree reads a checkout with the host's git, never with anything the
// checkout configures: the runner owns this checkout until launch.
func assertHistoryFree(ctx context.Context, dir, landed string) CheckoutHistory {
	h := CheckoutHistory{LandedObjectExit: -1, Commits: -1, Refs: -1}
	cmd := exec.CommandContext(ctx, "git", "-C", dir, "cat-file", "-e", landed)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		h.LandedObjectExit = 0
	case errors.As(err, &exit):
		h.LandedObjectExit = exit.ExitCode()
	}
	if out, err := gitOut(ctx, dir, "rev-list", "--all", "--count"); err == nil {
		fmt.Sscan(out, &h.Commits)
	}
	if out, err := gitOut(ctx, dir, "for-each-ref", "--format=%(refname)"); err == nil {
		h.Refs = len(strings.Fields(out))
	}
	return h
}

// skippedDirNames are directory names skipped at any depth: git's own directory, and
// the dependency directory setup recreates in the check's checkout (including nested
// occurrences, such as an npm workspace package's own node_modules).
var skippedDirNames = map[string]bool{".git": true, "node_modules": true}

// treeChanges compares the worker's tree against a fresh export of base_sha without
// running anything in the worker's checkout: a git command there would honour the
// checkout's own configuration (core.fsmonitor, filters), which the worker controls.
// Symlinks are never carried over, and are counted.
func treeChanges(base, worker string) (changes []change, symlinks int, err error) {
	seen := map[string]bool{}
	err = filepath.WalkDir(worker, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(worker, p)
		if rel == "." {
			return nil
		}
		if d.IsDir() {
			if skippedDirNames[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Type()&fs.ModeSymlink != 0 {
			symlinks++
			seen[rel] = true
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		seen[rel] = true
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if old, err := os.ReadFile(filepath.Join(base, rel)); err == nil && bytes.Equal(old, content) {
			return nil
		}
		changes = append(changes, change{path: filepath.ToSlash(rel), content: content})
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	err = filepath.WalkDir(base, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(base, p)
		if rel == "." || d.IsDir() {
			return nil
		}
		if !seen[rel] {
			changes = append(changes, change{path: filepath.ToSlash(rel), deleted: true})
		}
		return nil
	})
	return changes, symlinks, err
}
