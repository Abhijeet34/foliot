// Package claudecode is the Claude Code adapter for the benchmark: how `claude -p` is
// launched headless, the run profile its sandbox reads, and the stream it writes. Every
// flag, variable and event shape here was measured on claude 2.1.270 on 2026-09-14.
package claudecode

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Abhijeet34/foliot/src/core/bench"
)

// Adapter launches the binary at Binary.
type Adapter struct{ Binary string }

var _ bench.Harness = Adapter{}

func (Adapter) Name() string { return "claude-code" }

// Billing is subscription: the credential is a `claude setup-token` OAuth token (p8 R4).
func (Adapter) Billing() string { return "subscription" }

func (Adapter) CredentialEnv() string { return "CLAUDE_CODE_OAUTH_TOKEN" }

// tools are what a bare arm gets: enough to read, edit and run the repository's suite. The
// web tools and subagents are left out, and the web tools are also denied in the profile.
const tools = "Bash,Read,Edit,Write,Grep,Glob"

// Launch writes the run profile into <home>/.claude/settings.json and returns the command.
// Only user settings load, so a repository's own .claude directory cannot widen the profile.
func (a Adapter) Launch(l bench.Launch) (bench.Command, error) {
	mode := "acceptEdits"
	if l.Isolation != nil {
		b, err := settings(l.Isolation, l.Home, os.Getuid())
		if err != nil {
			return bench.Command{}, err
		}
		dir := filepath.Join(l.Home, ".claude")
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return bench.Command{}, err
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"), b, 0o600); err != nil {
			return bench.Command{}, err
		}
	} else {
		// The state the critique measured: no sandbox, and a shell that runs unprompted,
		// which a bare arm needs to run its suite. With no profile and acceptEdits, headless
		// Bash refuses `npm test` as well as the check, so that mode is not a runnable arm.
		mode = "bypassPermissions"
	}
	return bench.Command{
		Argv: []string{a.Binary, "-p", l.Prompt,
			"--output-format", "stream-json", "--verbose",
			"--model", l.Model,
			"--max-budget-usd", fmt.Sprintf("%.2f", l.CapUSD),
			"--no-session-persistence", "--strict-mcp-config",
			"--setting-sources", "user",
			"--tools", tools,
			"--permission-mode", mode,
		},
		Env: []string{
			// The harness's session temp directory, whose default is under the shared /tmp.
			"CLAUDE_CODE_TMPDIR=" + l.Tmp,
			// a5 section 2.13: no self-update under a running worker, no telemetry.
			"DISABLE_AUTOUPDATER=1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"DISABLE_TELEMETRY=1",
			// Corpus v1's visible suite ran 306 s against the Bash tool's 120 s default, which
			// would push every arm's suite to the background.
			"BASH_DEFAULT_TIMEOUT_MS=600000",
			Adapter{}.CredentialEnv() + "=" + l.Credential,
		},
	}, nil
}

// settings renders the isolation as Claude Code's sandbox and permission settings. The
// sandbox binds Bash and its subprocesses; permission deny rules bind the Read, Grep and
// Glob tools, which the sandbox does not reach. Both, because each covers what the other
// does not (docs: sandboxing, permissions "Read and Edit").
func settings(iso *bench.Isolation, home string, uid int) ([]byte, error) {
	profileDir := filepath.Join(home, ".claude")
	tmp := tmpEntries("/private/tmp", uid)
	var rules []string
	for _, p := range append(append([]string{}, iso.ToolDeny...), tmp...) {
		rules = append(rules, "Read(/"+p+"/**)", "Edit(/"+p+"/**)")
	}
	rules = append(rules, "Edit(/"+profileDir+"/**)",
		// WebFetch and WebSearch run in-process, outside the sandbox's network proxy, and
		// would fetch the public landed patch (Fable critique k3 section 1.2 path 1).
		"WebFetch", "WebSearch")
	s := map[string]any{
		"sandbox": map[string]any{
			"enabled":                  true,
			"failIfUnavailable":        true,
			"allowUnsandboxedCommands": false, // no dangerouslyDisableSandbox retry
			"autoAllowBashIfSandboxed": true,
			"excludedCommands":         []string{},
			"filesystem": map[string]any{
				// /private/var/folders holds every macOS per-user temp directory.
				"denyRead":   append(append(append([]string{}, iso.DenyRead...), "/private/var/folders"), tmp...),
				"allowRead":  iso.AllowRead,
				"allowWrite": []string{iso.Run},
				"denyWrite":  []string{profileDir},
			},
			// The visible suite runs offline once setup has run, so a sandboxed command needs
			// no host; the harness's own API traffic is in-process and not proxied.
			"network": map[string]any{"allowedDomains": []string{}, "strictAllowlist": true},
			// The token reaches the harness by design (a5 section 2.13); its shell does not.
			"credentials": map[string]any{
				"envVars": []map[string]string{{"name": Adapter{}.CredentialEnv(), "mode": "deny"}},
			},
		},
		"permissions": map[string]any{"deny": rules, "defaultMode": "acceptEdits"},
	}
	return json.MarshalIndent(s, "", "  ")
}

// tmpEntries is every entry of the shared temporary directory except the harness's own
// per-user directory, whose session subdirectories (named from a project path, so starting
// with "-") are denied by pattern. Denying the directory whole makes every sandboxed
// command exit 1, because the harness writes its cwd file under /tmp/claude-<uid>.
func tmpEntries(tmp string, uid int) []string {
	own := fmt.Sprintf("claude-%d", uid)
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return nil
	}
	out := []string{filepath.Join(tmp, own, "-*")}
	for _, e := range entries {
		if e.Name() != own {
			out = append(out, filepath.Join(tmp, e.Name()))
		}
	}
	return out
}
