package bench

import (
	"io"
	"strings"
)

// Harness is what the benchmark needs from an adapter (a5 section 1.1 `adapter`): how to
// launch one headless run and how to read its stream. The harness's flags, environment,
// run-profile format and event shapes live under src/adapters/<name>, never here (K8).
type Harness interface {
	Name() string
	// Billing is the economics the injected credential bills to (p8 R4).
	Billing() string
	// CredentialEnv names the variable the credential is injected as, so the probe can ask
	// whether a shell inherited it without printing it.
	CredentialEnv() string
	// Launch writes whatever run profile the isolation needs into the run's home and
	// returns the command. A nil isolation is the unisolated state a red proof runs in.
	Launch(l Launch) (Command, error)
	Read(r io.Reader) (*Reading, error)
}

// Launch is one run's launch request.
type Launch struct {
	Home, Tmp, Checkout string
	Model               string
	CapUSD              float64
	Prompt              string
	Credential          string
	Isolation           *Isolation
}

// Command is the argv and the harness's own environment, added to the runner's base.
type Command struct {
	Argv []string
	Env  []string
}

// Reading is a run's stream as the harness's own events state it. A nil pointer is a
// figure the stream did not carry or whose control inside the adapter failed; it is never
// read as zero.
type Reading struct {
	Model   string // the model the harness reports running
	Billing string // the credential kind the harness reports using; "" when unread
	Version string

	WeekUsed *float64 // the subscription's seven-day utilization, 0 to 1

	ToolUses int
	Denials  int
	// DeniedInputs holds each denied call's input, so the scope of a refusal is judged from
	// what was asked rather than from the refusal's wording.
	DeniedInputs []string
	Texts        []string // every string in the transcript, decoded

	Ended       bool
	EndedBy     string // exit, budget, cap or error (a5 section 2.7's rules for a bare run)
	ClaimedDone bool   // the harness ended the run as a success

	CostUSD                                          *float64
	InputTokens, OutputTokens, CacheRead, CacheWrite *int64
	DurationMS                                       *int64
}

// Observed is what the runner measured itself about the process, beside the stream.
type Observed struct {
	WallMS      int64
	CPUMS       int64
	LoadAtStart *float64
}

// columns is a5 section 2.16's per-run columns for a bare arm (p8 R13). A nil value is a
// reading whose control failed or whose event never arrived; the report refuses it as a
// value and never counts it as zero.
func (r *Reading) columns(o Observed, injected, run, root, realHome string) map[string]any {
	c := map[string]any{
		"input_tokens": ptr(r.InputTokens), "output_tokens": ptr(r.OutputTokens),
		"cache_read": ptr(r.CacheRead), "cache_write": ptr(r.CacheWrite),
		"cost_usd": nil, "wall_ms": nil, "cpu_ms": o.CPUMS, "load_at_start": nil,
		"ended_by": "unknown", "denials_in_scope": r.inScopeDenials(run, root, realHome),
		// Zero by construction in a bare run: no router routes a refusal, nothing asks after
		// dispatch, and no intake call exists (a5 section 2.16).
		"refusals_routed": 0, "questions_after": 0, "orchestrator_tokens": 0,
		"billing": nil,
	}
	if r.CostUSD != nil {
		c["cost_usd"] = *r.CostUSD
	}
	if o.LoadAtStart != nil {
		c["load_at_start"] = *o.LoadAtStart
	}
	if r.Ended {
		c["ended_by"] = r.EndedBy
	}
	// Control: the harness cannot have run longer than the runner watched it run.
	if r.DurationMS != nil && *r.DurationMS >= 0 && *r.DurationMS <= o.WallMS {
		c["wall_ms"] = *r.DurationMS
	}
	// Control: the credential the stream reports must be the one the runner injected.
	switch {
	case r.Billing == injected:
		c["billing"] = injected
	case r.Billing != "":
		c["billing"] = "mismatch:" + r.Billing
	}
	return c
}

func ptr(p *int64) any {
	if p == nil {
		return nil
	}
	return *p
}

// inScopeDenials counts refusals of work inside the run: a denied call whose input names
// no path outside the run's directory under <root>, the real home or the shared temp.
func (r *Reading) inScopeDenials(run, root, realHome string) int {
	n := 0
	for _, in := range r.DeniedInputs {
		rest := strings.ReplaceAll(in, run, "")
		if strings.Contains(rest, root) || strings.Contains(rest, realHome) || strings.Contains(rest, "/tmp") {
			continue
		}
		n++
	}
	return n
}
