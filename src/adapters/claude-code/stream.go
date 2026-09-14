package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"math"

	"github.com/Abhijeet34/foliot/src/core/bench"
)

type result struct {
	Subtype           string                `json:"subtype"`
	IsError           bool                  `json:"is_error"`
	TotalCostUSD      *float64              `json:"total_cost_usd"`
	DurationMS        *int64                `json:"duration_ms"`
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

// denialMarkers are the harness's words for a refusal in a tool result: the OS sandbox's
// EPERM, its violation block, and the permission layer's two deny messages.
var denialMarkers = [][]byte{
	[]byte("Operation not permitted"),
	[]byte("operation not permitted"),
	[]byte("<sandbox_violations>"),
	[]byte("denied by your permission settings"),
	[]byte("has been denied"),
}

// costTolerance is how far the stream's total may sit from the sum of its per-model costs
// before the cost is unknown: float noise, never a real disagreement.
const costTolerance = 1e-6

// Read parses a `--output-format stream-json --verbose` transcript. A line that is not
// JSON is skipped: the harness can write a warning into the same stream.
func (Adapter) Read(r io.Reader) (*bench.Reading, error) {
	out := &bench.Reading{}
	inputs := map[string]string{}
	denied := map[string]bool{}
	var res *result
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		var ev struct {
			Type          string `json:"type"`
			Subtype       string `json:"subtype"`
			Model         string `json:"model"`
			APIKeySource  string `json:"apiKeySource"`
			Version       string `json:"claude_code_version"`
			RateLimitInfo *struct {
				UnifiedWindows map[string]struct {
					Utilization *float64 `json:"utilization"`
				} `json:"unifiedWindows"`
			} `json:"rate_limit_info"`
			Message *struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if len(line) == 0 || json.Unmarshal(line, &ev) != nil {
			continue
		}
		var all any
		if json.Unmarshal(line, &all) == nil {
			collectStrings(all, &out.Texts)
		}
		switch ev.Type {
		case "system":
			if ev.Subtype == "init" {
				out.Model, out.Version = ev.Model, ev.Version
				// An OAuth token shows apiKeySource none; any other source is an API key.
				switch ev.APIKeySource {
				case "none":
					out.Billing = Adapter{}.Billing()
				case "":
				default:
					out.Billing = "api"
				}
			}
		case "rate_limit_event":
			if ev.RateLimitInfo != nil {
				if w, ok := ev.RateLimitInfo.UnifiedWindows["seven_day"]; ok && w.Utilization != nil {
					out.WeekUsed = w.Utilization
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
				switch {
				case b.Type == "tool_use":
					out.ToolUses++
					inputs[b.ID] = string(b.Input)
				case b.Type == "tool_result" && b.IsError && isDenial(b.Content):
					denied[b.ToolUseID] = true
				}
			}
		case "result":
			res = &result{}
			if json.Unmarshal(line, res) != nil {
				res = nil
				continue
			}
			for _, d := range res.PermissionDenials {
				denied[d.ToolUseID] = true
				if _, ok := inputs[d.ToolUseID]; !ok {
					inputs[d.ToolUseID] = string(d.ToolInput)
				}
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	for id := range denied {
		out.Denials++
		out.DeniedInputs = append(out.DeniedInputs, inputs[id])
	}
	if res != nil {
		readResult(res, out)
	}
	return out, nil
}

func readResult(res *result, out *bench.Reading) {
	out.Ended = true
	out.ClaimedDone = res.Subtype == "success" && !res.IsError
	switch res.Subtype {
	case "success":
		out.EndedBy = "exit"
	case "error_max_budget_usd":
		out.EndedBy = "budget"
	case "error_max_turns":
		out.EndedBy = "cap"
	default:
		out.EndedBy = "error"
	}
	out.DurationMS = res.DurationMS
	// Tokens are summed over every model the harness billed; a sum is unknown when any
	// model's entry lacks the field.
	sum := func(get func(modelUsage) *int64) *int64 {
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
		return &n
	}
	out.InputTokens = sum(func(u modelUsage) *int64 { return u.InputTokens })
	out.OutputTokens = sum(func(u modelUsage) *int64 { return u.OutputTokens })
	out.CacheRead = sum(func(u modelUsage) *int64 { return u.CacheReadInputTokens })
	out.CacheWrite = sum(func(u modelUsage) *int64 { return u.CacheCreationInputTokens })
	// Control: the stream's total must equal the sum of its per-model costs.
	if res.TotalCostUSD == nil || len(res.ModelUsage) == 0 {
		return
	}
	var parts float64
	for _, u := range res.ModelUsage {
		if u.CostUSD == nil {
			return
		}
		parts += *u.CostUSD
	}
	if math.Abs(parts-*res.TotalCostUSD) <= costTolerance {
		out.CostUSD = res.TotalCostUSD
	}
}

func isDenial(text []byte) bool {
	for _, m := range denialMarkers {
		if bytes.Contains(text, m) {
			return true
		}
	}
	return false
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
