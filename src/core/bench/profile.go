package bench

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Profile is <root>/profile/bench.json: the benchmark's movable half (a5 section 2.18,
// section 2.21). Model names, cutoffs, caps, credential paths and this machine's extra
// denied paths are values someone else sets differently, so none of them is in the core.
type Profile struct {
	Arms []Arm `json:"arms"`
	// DenyRead is paths a worker must not read beyond <root> and the real home, which the
	// runner always denies: an off-root backup of the corpus, the records it was mined from,
	// the credential directory (Fable critique k3 section 1.3).
	DenyRead []string `json:"deny_read"`
	// Credentials maps an adapter name to the file holding its token, mode 0600.
	Credentials map[string]string `json:"credentials"`
}

// Arm is one benchmark row that runs a model bare: no orchestrator, router or gate.
type Arm struct {
	Name    string `json:"name"`
	Adapter string `json:"adapter"`
	Model   string `json:"model"`
	// Provider is who serves the model, recorded on bench.run (a5 section 2.3).
	Provider string `json:"provider"`
	// Cutoff is the model's training data cutoff, YYYY-MM, read against when a task became
	// public (k3 section 1.2); CutoffSource says where it was read.
	Cutoff       string  `json:"cutoff"`
	CutoffSource string  `json:"cutoff_source"`
	CapUSD       float64 `json:"cap_usd"`
	// Repeats overrides a sweep's --repeats; the ceiling row runs once (p8 R16).
	Repeats int `json:"repeats,omitempty"`
}

var (
	armNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	cutoffRe  = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])$`)
)

// ProfilePath is where a root's benchmark profile lives.
func ProfilePath(root string) string { return filepath.Join(root, "profile", "bench.json") }

// LoadProfile reads and validates a root's benchmark profile against the adapters known
// to this binary.
func LoadProfile(root string, adapters map[string]Harness) (*Profile, error) {
	var p Profile
	if err := readJSON(ProfilePath(root), &p); err != nil {
		return nil, refusef("benchmark profile: %v", err)
	}
	var problems []string
	seen := map[string]bool{}
	for i, a := range p.Arms {
		switch {
		case !armNameRe.MatchString(a.Name):
			problems = append(problems, fmt.Sprintf("arms[%d].name %q is not a lower-case slug", i, a.Name))
		case seen[a.Name]:
			problems = append(problems, fmt.Sprintf("arm %q is declared twice", a.Name))
		}
		seen[a.Name] = true
		if adapters[a.Adapter] == nil {
			problems = append(problems, fmt.Sprintf("arm %s: adapter %q is not one this binary has", a.Name, a.Adapter))
		} else if _, ok := p.Credentials[a.Adapter]; !ok {
			problems = append(problems, fmt.Sprintf("arm %s: credentials has no entry for adapter %s", a.Name, a.Adapter))
		}
		if a.Model == "" || a.Provider == "" || !cutoffRe.MatchString(a.Cutoff) || a.CutoffSource == "" || a.CapUSD <= 0 || a.Repeats < 0 {
			problems = append(problems, fmt.Sprintf("arm %s needs model, provider, cutoff YYYY-MM, cutoff_source and cap_usd above 0", a.Name))
		}
	}
	if len(p.Arms) == 0 {
		problems = append(problems, "arms is empty")
	}
	for _, d := range p.DenyRead {
		if !filepath.IsAbs(d) {
			problems = append(problems, fmt.Sprintf("deny_read %q is not absolute", d))
		}
	}
	if len(problems) > 0 {
		return nil, refusef("benchmark profile %s: %s", ProfilePath(root), strings.Join(problems, "; "))
	}
	return &p, nil
}

// ParseArms resolves a comma-separated arm list against the profile.
func (p *Profile) ParseArms(list string) ([]Arm, error) {
	var out []Arm
	seen := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		arm, ok := p.arm(name)
		switch {
		case !ok:
			return nil, refusef("unknown arm %q; the profile's arms are %s", name, p.armNames())
		case seen[name]:
			return nil, refusef("arm %q is listed twice", name)
		}
		seen[name] = true
		out = append(out, arm)
	}
	return out, nil
}

func (p *Profile) arm(name string) (Arm, bool) {
	for _, a := range p.Arms {
		if a.Name == name {
			return a, true
		}
	}
	return Arm{}, false
}

func (p *Profile) armNames() string {
	var names []string
	for _, a := range p.Arms {
		names = append(names, a.Name)
	}
	return strings.Join(names, ", ")
}

func (a Arm) repeats(sweep int) int {
	if a.Repeats > 0 {
		return a.Repeats
	}
	return sweep
}

// readToken refuses a token file anyone but its owner can read, and returns the value only
// to the caller that injects it.
func readToken(path string) (string, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return "", refusef("token file: %v", err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return "", refusef("token file %s must be a regular file of mode 0600, is %v", path, fi.Mode())
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", refusef("token file: %v", err)
	}
	tok := strings.TrimSpace(string(b))
	if tok == "" || strings.ContainsAny(tok, " \n\t") {
		return "", refusef("token file %s does not hold one token", path)
	}
	return tok, nil
}
