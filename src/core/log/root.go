package log

import (
	"errors"
	"path/filepath"
)

// ErrNoRoot is returned when FOLIOT_HOME does not name an absolute directory.
var ErrNoRoot = errors.New("FOLIOT_HOME must be set to an absolute path")

// Root resolves <root> from FOLIOT_HOME, the one variable the design names for a
// home (a5 section 2.20). There is no fallback to the user's home directory: a5
// section 2.13 has the orchestrator write nowhere but <root>, and no document
// names a default location.
func Root(getenv func(string) string) (string, error) {
	root := getenv("FOLIOT_HOME")
	if root == "" || !filepath.IsAbs(root) {
		return "", ErrNoRoot
	}
	return filepath.Clean(root), nil
}

// Path is the log file under a root.
func Path(root string) string { return filepath.Join(root, "log", "events.jsonl") }
