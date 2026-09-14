package bench

import (
	"fmt"
	"strings"
)

// Arm is one benchmark row that runs a model bare: no orchestrator, no router, no gate.
// Cutoff is the vendor's training data cutoff, read from the models overview page
// (platform.claude.com/docs/en/about-claude/models/overview, 2026-09-14), so a
// run can be read against when its task became public (Fable critique k3 section 1.2).
type Arm struct {
	Name    string
	Model   string
	Cutoff  string  // YYYY-MM
	CapUSD  float64 // a5 section 2.16: $3 per large run, $1 per small run
	Repeats int     // 0 means the sweep's --repeats; the ceiling runs once (p8 R16)
}

// Arms are P1's bare arms (a5 section 2.16); orchestrated arms arrive with P2.
var Arms = []Arm{
	{Name: "bare-large", Model: "claude-opus-5", Cutoff: "2026-05", CapUSD: 3},
	{Name: "bare-small", Model: "claude-sonnet-5", Cutoff: "2026-01", CapUSD: 1},
	{Name: "bare-small-haiku", Model: "claude-haiku-4-5-20251001", Cutoff: "2025-07", CapUSD: 1},
	// The ceiling's cap is the large cap: a5 names none for it.
	{Name: "ceiling", Model: "claude-fable-5-1", Cutoff: "2026-06", CapUSD: 3, Repeats: 1},
}

// Every arm here runs one harness on the subscription (p8 R4; a5 section 2.13).
const (
	adapterName = "claude-code"
	gateName    = "none"
	providerID  = "anthropic"
	billingKind = "subscription"
)

// ParseArms resolves a comma-separated arm list, refusing an unknown or repeated name.
func ParseArms(list string) ([]Arm, error) {
	var out []Arm
	seen := map[string]bool{}
	for _, name := range strings.Split(list, ",") {
		name = strings.TrimSpace(name)
		arm, ok := armByName(name)
		switch {
		case !ok:
			return nil, fmt.Errorf("unknown arm %q; the arms are %s", name, armNames())
		case seen[name]:
			return nil, fmt.Errorf("arm %q is listed twice", name)
		}
		seen[name] = true
		out = append(out, arm)
	}
	return out, nil
}

func armByName(name string) (Arm, bool) {
	for _, a := range Arms {
		if a.Name == name {
			return a, true
		}
	}
	return Arm{}, false
}

func armNames() string {
	var names []string
	for _, a := range Arms {
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
