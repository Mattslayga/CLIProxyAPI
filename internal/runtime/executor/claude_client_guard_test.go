package executor

import (
	"errors"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/clienterror"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

func TestClaudeSubscriptionClientGuardOffByDefault(t *testing.T) {
	t.Setenv(claudeSubscriptionClientGuardEnv, "")
	oauth := resolveClaudeFingerprintPolicy(nil, nil, "sk-ant-oat01-guard-test")
	headers := http.Header{"User-Agent": []string{"opencode/1.4.2"}}
	if err := claudeSubscriptionClientGuard(oauth, helps.DetectClaudeCodeRequest(headers, nil, false)); err != nil {
		t.Fatalf("guard refused with %s unset: %v", claudeSubscriptionClientGuardEnv, err)
	}
}

func TestClaudeSubscriptionClientGuard(t *testing.T) {
	t.Setenv(claudeSubscriptionClientGuardEnv, "1")
	oauth := resolveClaudeFingerprintPolicy(nil, nil, "sk-ant-oat01-guard-test")
	apiKey := resolveClaudeFingerprintPolicy(nil, nil, "sk-ant-api03-guard-test")
	payload := []byte(`{"model":"claude-opus-5-5","messages":[{"role":"user","content":"hi"}]}`)
	for _, tc := range []struct {
		name      string
		userAgent string
		fp        claudeFingerprintPolicy
		allowed   bool
	}{
		{"t3 agent sdk", "claude-cli/2.1.310 (external, sdk-ts, agent-sdk/0.3.10)", oauth, true},
		{"claude -p", "claude-cli/2.1.310 (external, sdk-cli)", oauth, true},
		{"interactive cli", "claude-cli/2.1.310 (external, cli)", oauth, true},
		{"vscode", "claude-cli/2.1.310 (external, claude-vscode)", oauth, true},
		{"codex", "codex_exec/0.164.0 (Ubuntu 24.4.0; x86_64)", oauth, false},
		{"opencode", "opencode/1.4.2", oauth, false},
		{"python agent sdk", "claude-cli/2.1.310 (external, sdk-py)", oauth, false},
		{"no user agent", "", oauth, false},
		{"api key credential", "opencode/1.4.2", apiKey, true},
	} {
		headers := http.Header{}
		if tc.userAgent != "" {
			headers.Set("User-Agent", tc.userAgent)
		}
		err := claudeSubscriptionClientGuard(tc.fp, helps.DetectClaudeCodeRequest(headers, payload, false))
		if tc.allowed {
			if err != nil {
				t.Fatalf("%s: guard refused: %v", tc.name, err)
			}
			continue
		}
		if err == nil {
			t.Fatalf("%s: guard allowed a non-Claude-Code client", tc.name)
		}
		var se statusErr
		if !errors.As(err, &se) || se.StatusCode() != http.StatusBadRequest {
			t.Fatalf("%s: err = %v, want 400 statusErr", tc.name, err)
		}
		if !clienterror.IsRequestFault(http.StatusBadRequest, err) {
			t.Fatalf("%s: refusal is not a request fault, so it would cool the credential down", tc.name)
		}
	}
}
