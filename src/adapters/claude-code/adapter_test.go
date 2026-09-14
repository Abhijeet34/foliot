package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Abhijeet34/foliot/internal/testenv"
	"github.com/Abhijeet34/foliot/src/core/bench"
)

func TestMain(m *testing.M) { os.Exit(testenv.Main(m)) }

// transcript is shaped after a measured 2.1.270 stream: init, a warning line, a rate-limit
// reading, a denied Bash call, a Read call the permission layer refused, and the result.
func transcript(total string) string {
	return strings.Join([]string{
		`{"type":"system","subtype":"init","model":"claude-opus-5","apiKeySource":"none","claude_code_version":"2.1.270"}`,
		`Warning: no stdin data received in 3s`,
		`{"type":"rate_limit_event","rate_limit_info":{"unifiedWindows":{"seven_day":{"utilization":0.27}}}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"a","name":"Read","input":{"file_path":"/root/bench/checks/t/check.sh"}},{"type":"tool_use","id":"b","name":"Bash","input":{"command":"cat /root/bench/checks/t/check.sh"}}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"b","is_error":true,"content":"cat: /root/bench/checks/t/check.sh: Operation not permitted"},{"type":"tool_result","tool_use_id":"a","is_error":true,"content":"<tool_use_error>File is in a directory that is denied by your permission settings.</tool_use_error>"}]}}`,
		`{"type":"result","subtype":"error_max_budget_usd","is_error":true,"total_cost_usd":` + total + `,"duration_ms":900,"permission_denials":[{"tool_use_id":"a","tool_input":{}}],"modelUsage":{"claude-opus-5":{"inputTokens":1,"outputTokens":2,"cacheReadInputTokens":3,"cacheCreationInputTokens":4,"costUSD":1.0},"claude-haiku-4-5-20251001":{"inputTokens":10,"outputTokens":20,"cacheReadInputTokens":30,"cacheCreationInputTokens":40,"costUSD":0.5}}}`,
	}, "\n")
}

func TestReadTakesEveryFigureFromTheStream(t *testing.T) {
	testenv.Isolate(t)
	r, err := Adapter{}.Read(strings.NewReader(transcript("1.5")))
	if err != nil {
		t.Fatal(err)
	}
	if r.Model != "claude-opus-5" || r.Billing != "subscription" || r.Version != "2.1.270" || *r.WeekUsed != 0.27 ||
		r.ToolUses != 2 || r.Denials != 2 || !r.Ended || r.EndedBy != "budget" || r.ClaimedDone ||
		*r.InputTokens != 11 || *r.OutputTokens != 22 || *r.CacheRead != 33 || *r.CacheWrite != 44 || *r.CostUSD != 1.5 || *r.DurationMS != 900 {
		t.Fatalf("reading %+v", r)
	}
}

func TestReadRefusesACostTheStreamContradicts(t *testing.T) {
	testenv.Isolate(t)
	// total_cost_usd says 0.01 while the per-model costs sum to 1.5.
	r, _ := Adapter{}.Read(strings.NewReader(transcript("0.01")))
	if r.CostUSD != nil {
		t.Fatalf("cost %v, want unknown when total_cost_usd disagrees with modelUsage", *r.CostUSD)
	}
	// A transcript cut before its result leaves the run unended and every figure unknown.
	cut, _ := Adapter{}.Read(strings.NewReader(strings.SplitN(transcript("1.5"), "\n", 2)[0]))
	if cut.Ended || cut.CostUSD != nil || cut.InputTokens != nil {
		t.Fatalf("cut transcript %+v", cut)
	}
	// An API key in place of the token is not the subscription.
	api, _ := Adapter{}.Read(strings.NewReader(`{"type":"system","subtype":"init","apiKeySource":"ANTHROPIC_API_KEY"}`))
	if api.Billing != "api" {
		t.Fatalf("billing %q", api.Billing)
	}
}

func TestLaunchWritesTheRunProfileOnlyWhenIsolated(t *testing.T) {
	testenv.Isolate(t)
	run := t.TempDir()
	home := filepath.Join(run, "home")
	iso := &bench.Isolation{Run: run, DenyRead: []string{"/root", "/home/u"}, AllowRead: []string{run}, ToolDeny: []string{"/root/log", "/home/u/records"}}
	l := bench.Launch{Home: home, Tmp: filepath.Join(run, "tmp"), Model: "claude-opus-5", CapUSD: 1, Prompt: "p", Credential: "tok", Isolation: iso}
	cmd, err := Adapter{Binary: "/bin/claude"}.Launch(l)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN=tok") || !slices.Contains(cmd.Env, "CLAUDE_CODE_TMPDIR="+l.Tmp) ||
		!strings.Contains(strings.Join(cmd.Argv, " "), "--permission-mode acceptEdits") || !strings.Contains(strings.Join(cmd.Argv, " "), "--setting-sources user") {
		t.Fatalf("command %q env %q", cmd.Argv, cmd.Env)
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "tok") {
		t.Fatal("the credential reached the run profile")
	}
	var s struct {
		Sandbox struct {
			Enabled, FailIfUnavailable, AllowUnsandboxedCommands bool
			Filesystem                                           struct{ DenyRead, AllowRead, AllowWrite, DenyWrite []string }
			Network                                              struct {
				AllowedDomains  []string
				StrictAllowlist bool
			}
			Credentials struct{ EnvVars []struct{ Name, Mode string } }
		}
		Permissions struct{ Deny []string }
	}
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	sb := s.Sandbox
	if !sb.Enabled || !sb.FailIfUnavailable || sb.AllowUnsandboxedCommands || !sb.Network.StrictAllowlist || len(sb.Network.AllowedDomains) != 0 ||
		!slices.Contains(sb.Filesystem.DenyRead, "/root") || !slices.Contains(sb.Filesystem.DenyRead, "/private/var/folders") ||
		!slices.Contains(sb.Filesystem.AllowRead, run) || !slices.Contains(sb.Filesystem.DenyWrite, filepath.Join(home, ".claude")) ||
		len(sb.Credentials.EnvVars) != 1 || sb.Credentials.EnvVars[0].Name != "CLAUDE_CODE_OAUTH_TOKEN" {
		t.Fatalf("sandbox %+v", sb)
	}
	for _, want := range []string{"Read(//root/log/**)", "Read(//home/u/records/**)", "Edit(/" + filepath.Join(home, ".claude") + "/**)", "WebFetch", "WebSearch"} {
		if !slices.Contains(s.Permissions.Deny, want) {
			t.Errorf("permissions.deny lacks %s", want)
		}
	}

	bare := t.TempDir()
	l.Home, l.Isolation = filepath.Join(bare, "home"), nil
	cmd, _ = Adapter{Binary: "/bin/claude"}.Launch(l)
	if _, err := os.Stat(filepath.Join(l.Home, ".claude", "settings.json")); err == nil || !strings.Contains(strings.Join(cmd.Argv, " "), "--permission-mode bypassPermissions") {
		t.Fatalf("an unisolated launch wrote a profile or kept prompts: %q", cmd.Argv)
	}
}
