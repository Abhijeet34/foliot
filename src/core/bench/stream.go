package bench

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"
	"strings"
)

// Stream is the reading of one `claude -p --output-format stream-json --verbose`
// transcript. Every figure comes from the harness's own events, never from the worker's
// prose (a5 section 1.1 `adapter`); the event shapes were measured on 2.1.270, 2026-09-14.
type Stream struct {
	Lines, Unparsed int

	Model, APIKeySource, Version string // from system/init
	Tools                        []string

	// WeekUsed is the last rate_limit_event's seven-day utilization, 0 to 1.
	WeekUsed *float64

	ToolUses int
	Denials  int
	// DeniedInputs holds each denied call's input, so the scope of a refusal is judged
	// from what was asked rather than from the refusal's wording.
	DeniedInputs []string
	Texts        []string // every string in every event, decoded

	Result *StreamResult
}

// StreamResult is the terminal `result` event.
type StreamResult struct {
	Subtype           string                `json:"subtype"`
	IsError           bool                  `json:"is_error"`
	TotalCostUSD      *float64              `json:"total_cost_usd"`
	DurationMS        *int64                `json:"duration_ms"`
	NumTurns          int                   `json:"num_turns"`
	TerminalReason    string                `json:"terminal_reason"`
	PermissionDenials []permissionDenial    `json:"permission_denials"`
	ModelUsage        map[string]modelUsage `json:"modelUsage"`
}

type permissionDenial struct {
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

type modelUsage struct {
	InputTokens              *int64   `json:"inputTokens"`
	OutputTokens             *int64   `json:"outputTokens"`
	CacheReadInputTokens     *int64   `json:"cacheReadInputTokens"`
	CacheCreationInputTokens *int64   `json:"cacheCreationInputTokens"`
	CostUSD                  *float64 `json:"costUSD"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	IsError   bool            `json:"is_error"`
	Content   json.RawMessage `json:"content"`
}

// ReadStream parses a transcript. A line that is not JSON is counted, not fatal: the
// harness writes warnings to the same file when stderr is joined to it.
func ReadStream(r io.Reader) (*Stream, error) {
	s := &Stream{}
	inputs := map[string]string{}
	denied := map[string]bool{}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		s.Lines++
		var ev struct {
			Type          string   `json:"type"`
			Subtype       string   `json:"subtype"`
			Model         string   `json:"model"`
			APIKeySource  string   `json:"apiKeySource"`
			Version       string   `json:"claude_code_version"`
			Tools         []string `json:"tools"`
			RateLimitInfo *struct {
				UnifiedWindows map[string]struct {
					Utilization *float64 `json:"utilization"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &ev); err != nil {
			s.Unparsed++
			continue
		}
		var all any
		if json.Unmarshal(line, &all) == nil {
			collectStrings(all, &s.Texts)
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				s.Model, s.APIKeySource, s.Version, s.Tools = ev.Model, ev.APIKeySource, ev.Version, ev.Tools
			}
		case "rate_limit_event":
			if ev.RateLimitInfo != nil {
				if w, ok := ev.RateLimitInfo.UnifiedWindows["seven_day"]; ok && w.Utilization != nil {
					s.WeekUsed = w.Utilization
				}
			}
		case "assistant", "user":
			if ev.Message == nil {
				continue
			}
			var blocks []contentBlock
			if json.Unmarshal(ev.Message.Content, &blocks) != nil {
				continue
			}
			for _, b := range blocks {
				switch b.Type {
				case "tool_use":
					s.ToolUses++
					inputs[b.ID] = string(b.Input)
				case "tool_result":
					if b.IsError && isDenial(b.Content) {
						denied[b.ToolUseID] = true
					}
				}
			}
		case "result":
			var res StreamResult
			if json.Unmarshal(line, &res) == nil {
				s.Result = &res
				for _, d := range res.PermissionDenials {
					denied[d.ToolUseID] = true
					if _, ok := inputs[d.ToolUseID]; !ok {
						inputs[d.ToolUseID] = string(d.ToolInput)
					}
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for id := range denied {
		s.Denials++
		s.DeniedInputs = append(s.DeniedInputs, inputs[id])
	}
	return s, nil
}

func collectStrings(v any, out *[]string) {
	switch t := v.(type) {
	case string:
		*out = append(*out, t)
	case []any:
		for _, x := range t {
			collectStrings(x, out)
		}
	case map[string]any:
		for _, x := range t {
			collectStrings(x, out)
		}
	}
}

// costTolerance is how far the stream's total may sit from the sum of its per-model
// costs before the cost reading is unknown: float noise, never a real disagreement.
const costTolerance = 1e-6

// Columns is a5 section 2.16's per-run columns for a bare arm (p8 R13). A nil value is
// a reading whose control failed or whose event never arrived; the report refuses it as
// a value and never counts it as zero.
type Columns map[string]any

// Observed is what the runner measured itself about the process, beside the stream.
type Observed struct {
	WallMS      int64
	CPUMS       int64
	LoadAtStart *float64
	Injected    string // the credential kind the runner injected
}

func (s *Stream) columns(o Observed, run, root, realHome string) Columns {
	c := Columns{
		"input_tokens": nil, "output_tokens": nil, "cache_read": nil, "cache_write": nil,
		"cost_usd": nil, "wall_ms": nil, "cpu_ms": o.CPUMS, "load_at_start": nil,
		"ended_by": "unknown", "denials_in_scope": s.inScopeDenials(run, root, realHome),
		// Zero by construction in a bare run: no router routes a refusal, nothing asks after
		// dispatch, and no intake call exists (a5 section 2.16).
		"refusals_routed": 0, "questions_after": 0, "orchestrator_tokens": 0,
		"billing": s.billing(o.Injected),
	}
	if o.LoadAtStart != nil {
		c["load_at_start"] = *o.LoadAtStart
	}
	res := s.Result
	if res == nil {
		return c
	}
	c["ended_by"] = endedBy(res.Subtype)
	// Tokens are summed over every model the harness billed; each sum is unknown when any
	// model's entry lacks the field.
	sum := func(get func(modelUsage) *int64) any {
		if len(res.ModelUsage) == 0 {
			return nil
		}
		var n int64
		for _, u := range res.ModelUsage {
			v := get(u)
			if v == nil {
				return nil
			}
			n += *v
		}
		return n
	}
	c["input_tokens"] = sum(func(u modelUsage) *int64 { return u.InputTokens })
	c["output_tokens"] = sum(func(u modelUsage) *int64 { return u.OutputTokens })
	c["cache_read"] = sum(func(u modelUsage) *int64 { return u.CacheReadInputTokens })
	c["cache_write"] = sum(func(u modelUsage) *int64 { return u.CacheCreationInputTokens })
	// Control: the stream's total must equal the sum of its per-model costs.
	if res.TotalCostUSD != nil && len(res.ModelUsage) > 0 {
		var parts float64
		ok := true
		for _, u := range res.ModelUsage {
			if u.CostUSD == nil {
				ok = false
				break
			}
			parts += *u.CostUSD
		}
		if ok && math.Abs(parts-*res.TotalCostUSD) <= costTolerance {
			c["cost_usd"] = *res.TotalCostUSD
		}
	}
	// Control: the harness cannot have run longer than the runner watched it run.
	if res.DurationMS != nil && *res.DurationMS >= 0 && *res.DurationMS <= o.WallMS {
		c["wall_ms"] = *res.DurationMS
	}
	return c
}

// endedBy maps the harness's terminal subtype onto a5 section 2.7's stop rules; a bare
// run has no verdict step of its own, so a clean end is `exit` and the check decides.
func endedBy(subtype string) string {
	switch subtype {
	case "success":
		return "exit"
	case "error_max_budget_usd":
		return "budget"
	case "error_max_turns":
		return "cap"
	case "":
		return "unknown"
	}
	return "error"
}

// billing reads the init event against the injected credential: an OAuth token shows
// apiKeySource none, so any other source means the run did not bill where it was sent.
func (s *Stream) billing(injected string) any {
	switch {
	case injected == billingKind && s.APIKeySource == "none":
		return billingKind
	case s.APIKeySource == "":
		return nil
	}
	return "mismatch:" + s.APIKeySource
}

// inScopeDenials counts refusals of work inside the run: a denied call whose input names
// no path outside the run's directory under <root>, the real home or the shared temp.
func (s *Stream) inScopeDenials(run, root, realHome string) int {
	n := 0
	for _, in := range s.DeniedInputs {
		rest := strings.ReplaceAll(in, run, "")
		if strings.Contains(rest, root) || strings.Contains(rest, realHome) || strings.Contains(rest, "/tmp") {
			continue
		}
		n++
	}
	return n
}
