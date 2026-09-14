// Package bench holds the benchmark corpus and the command that certifies it (a5 §2.16, p8 R10).
package bench

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Classes are pre-registered (a5 §2.16); a corpus declares how many of each it holds.
var Classes = []string{"defect", "feature", "refactor"}

// maxRequestChars is criterion 6's bound on the request as filed.
const maxRequestChars = 4000

// Task is one corpus entry, read from <root>/bench/corpus/<corpus>/<id>/task.json.
// The arm prompt is rendered from Request, Repository, BaseSHA, VisibleCheck and Scope;
// the other fields exist so verify can check the six criteria.
type Task struct {
	ID            string `json:"id"`
	Class         string `json:"class"`
	Request       string `json:"request"`
	RequestSHA256 string `json:"request_sha256"`
	Repository    string `json:"repository"`
	BaseSHA       string `json:"base_sha"`
	LandedSHA     string `json:"landed_sha"`
	PullRequest   string `json:"pull_request"`
	// Setup installs dependencies and is the only step allowed the package registry.
	Setup        string `json:"setup"`
	VisibleCheck string `json:"visible_check"`
	Scope        Scope  `json:"scope"`
	// History is the archived item the task was drawn from, verbatim, which criteria 2
	// and 5 are read against.
	History History `json:"history"`
}

// Scope is the files the item named, or discover when it named none.
type Scope struct {
	Files    []string `json:"files,omitempty"`
	Discover bool     `json:"discover,omitempty"`
}

// History names the archived item and carries its text.
type History struct {
	Source string `json:"source"`
	Item   string `json:"item"`
	Text   string `json:"text"`
}

// Manifest is <root>/bench/corpus/<corpus>/corpus.json: the pre-registered class counts
// and the history markers criterion 5 refuses, which are data because their words are
// the previous tool's vocabulary.
type Manifest struct {
	Classes  map[string]int `json:"classes"`
	Refusals []Marker       `json:"history_refusals"`
}

// Marker is one pattern that makes a task not self-contained when its history matches.
type Marker struct {
	What    string `json:"what"`
	Pattern string `json:"pattern"`
	re      *regexp.Regexp
}

var (
	shaRe     = regexp.MustCompile(`^[0-9a-f]{40}$`)
	idRe      = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,99}$`)
	prRe      = regexp.MustCompile(`^https://github\.com/[\w.-]+/[\w.-]+/pull/([0-9]+)$`)
	diffRe    = regexp.MustCompile(`(?m)^(diff --git |@@ -[0-9]|\+\+\+ [ab/]|--- a/)`)
	networkRe = regexp.MustCompile(strings.Join([]string{
		`\b(curl|wget|ncat|nc|ssh|scp|sftp|rsync|telnet|ftp)\s`,
		`\bgit\s+(clone|fetch|pull|push|ls-remote|remote|submodule)\b`,
		`\b(npm|pnpm|yarn|bun)\s+(install|i|ci|add|dlx|update|up)\b`,
		`\b(npx|bunx)\b`,
		`\bhttps?://`,
		`\bfetch\s*\(`,
		`\bnode:(http|https|http2|net|dgram|tls|dns)\b`,
		`\brequire\(\s*['"](http|https|http2|net|dgram|tls|dns)['"]\s*\)`,
		`/dev/(tcp|udp)/`,
	}, "|"))
)

// LoadManifest reads and validates a corpus manifest.
func LoadManifest(path string) (*Manifest, error) {
	var m Manifest
	if err := readJSON(path, &m); err != nil {
		return nil, err
	}
	for name, n := range m.Classes {
		if !knownClass(name) || n < 0 {
			return nil, fmt.Errorf("%s: class %q with count %d is not a pre-registered class", path, name, n)
		}
	}
	if len(m.Refusals) == 0 {
		return nil, fmt.Errorf("%s: history_refusals is empty, so criterion 5 would examine nothing", path)
	}
	for i := range m.Refusals {
		re, err := regexp.Compile(m.Refusals[i].Pattern)
		if err != nil || m.Refusals[i].Pattern == "" {
			return nil, fmt.Errorf("%s: history_refusals[%d] pattern %q does not compile", path, i, m.Refusals[i].Pattern)
		}
		m.Refusals[i].re = re
	}
	return &m, nil
}

// LoadTask reads a task and checks everything that needs no checkout: the record's shape,
// criterion 5 against its history and criterion 6 against its request.
func LoadTask(path string, m *Manifest) (*Task, []string) {
	var t Task
	if err := readJSON(path, &t); err != nil {
		return nil, []string{err.Error()}
	}
	var refusals []string
	refuse := func(format string, a ...any) { refusals = append(refusals, fmt.Sprintf(format, a...)) }

	if dir := filepath.Base(filepath.Dir(path)); t.ID != dir {
		refuse("record: id %q does not match its directory %q", t.ID, dir)
	}
	if !idRe.MatchString(t.ID) {
		refuse("record: id %q is not a lower-case slug", t.ID)
	}
	if !knownClass(t.Class) {
		refuse("record: class %q is not one of %s", t.Class, strings.Join(Classes, ", "))
	}
	if !shaRe.MatchString(t.BaseSHA) || !shaRe.MatchString(t.LandedSHA) {
		refuse("record: base_sha and landed_sha must be full 40-hex shas")
	} else if t.BaseSHA == t.LandedSHA {
		refuse("record: base_sha equals landed_sha")
	}
	for name, v := range map[string]string{"repository": t.Repository, "visible_check": t.VisibleCheck, "history.source": t.History.Source, "history.item": t.History.Item, "history.text": t.History.Text} {
		if strings.TrimSpace(v) == "" {
			refuse("record: %s is empty", name)
		}
	}
	if len(t.Scope.Files) == 0 && !t.Scope.Discover {
		refuse("record: scope names no files and is not discover")
	}
	if sum := sha256.Sum256([]byte(t.Request)); hex.EncodeToString(sum[:]) != t.RequestSHA256 {
		refuse("record: request_sha256 does not match the request bytes")
	}

	// Criterion 2, the part readable from the record: the pull request is named by the item.
	if !prRe.MatchString(t.PullRequest) {
		refuse("criterion 2: pull_request %q is not a GitHub pull request URL", t.PullRequest)
	} else if !strings.Contains(t.History.Text, t.PullRequest) {
		refuse("criterion 2: the archived item does not name %s", t.PullRequest)
	}

	// Criterion 5: no decision above the implementer, no credential, in the item's history.
	if m != nil {
		for _, mk := range m.Refusals {
			if loc := mk.re.FindStringIndex(t.History.Text + "\n" + t.Request); loc != nil {
				refuse("criterion 5: history carries %s: %q", mk.What, excerpt(t.History.Text+"\n"+t.Request, loc))
			}
		}
	}

	// Criterion 6, the part readable without the diff.
	if n := utf8.RuneCountInString(t.Request); n > maxRequestChars {
		refuse("criterion 6: request is %d characters, over %d", n, maxRequestChars)
	}
	if strings.TrimSpace(t.Request) == "" {
		refuse("criterion 6: request is empty")
	}
	if loc := diffRe.FindStringIndex(t.Request); loc != nil {
		refuse("criterion 6: request carries a diff: %q", excerpt(t.Request, loc))
	}
	if t.PullRequest != "" && strings.Contains(t.Request, t.PullRequest) {
		refuse("criterion 6: request links the landed pull request")
	}
	if len(t.LandedSHA) >= 7 && strings.Contains(t.Request, t.LandedSHA[:7]) {
		refuse("criterion 6: request names the landed sha")
	}
	return &t, refusals
}

// checkFiles lists the hidden check's files and refuses a check that is missing, holds a
// network call, or runs the visible suite (criteria 3 to 5).
func checkFiles(dir, visibleCheck string) ([]string, []string) {
	var files, refusals []string
	if fi, err := os.Stat(filepath.Join(dir, "check.sh")); err != nil || !fi.Mode().IsRegular() {
		return nil, []string{fmt.Sprintf("criterion 3: no hidden check at %s", filepath.Join(dir, "check.sh"))}
	}
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			refusals = append(refusals, fmt.Sprintf("criterion 4: %s is a symlink, which can point into the repository", p))
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		files = append(files, p)
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if loc := networkRe.FindIndex(b); loc != nil {
			refusals = append(refusals, fmt.Sprintf("criterion 5: %s reaches the network: %q", rel(dir, p), excerpt(string(b), loc)))
		}
		if v := strings.TrimSpace(visibleCheck); v != "" && strings.Contains(string(b), v) {
			refusals = append(refusals, fmt.Sprintf("criterion 4: %s runs the visible check %q", rel(dir, p), v))
		}
		return nil
	})
	if err != nil {
		refusals = append(refusals, fmt.Sprintf("criterion 3: reading %s: %v", dir, err))
	}
	sort.Strings(files)
	return files, refusals
}

func knownClass(name string) bool {
	for _, c := range Classes {
		if c == name {
			return true
		}
	}
	return false
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}

func excerpt(s string, loc []int) string {
	from, to := max(0, loc[0]-20), min(len(s), loc[1]+20)
	return strings.ReplaceAll(s[from:to], "\n", " ")
}

func rel(base, p string) string {
	if r, err := filepath.Rel(base, p); err == nil {
		return r
	}
	return p
}
