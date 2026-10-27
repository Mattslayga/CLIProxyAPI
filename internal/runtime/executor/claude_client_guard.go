package executor

import (
	"net/http"
	"os"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// claudeSubscriptionClientGuardEnv enables the guard when set to "1". It is
// off by default so upstream behaviour, which cloaks non-Claude-Code clients
// on OAuth credentials, is unchanged unless the operator opts in.
const claudeSubscriptionClientGuardEnv = "CLIPROXY_CLAUDE_OAUTH_NATIVE_ONLY"

const claudeSubscriptionClientGuardBody = `{"type":"error","error":{"type":"invalid_request_error","message":"Claude subscription credentials on this hub serve only native Claude Code clients"}}`

// claudeSubscriptionClientGuard refuses to spend a Claude subscription (OAuth)
// credential on a client that is not native Claude Code, such as Codex,
// OpenCode or a script calling the translated routes. Native means the
// User-Agent carries a Claude Code entrypoint the detector trusts (cli,
// sdk-cli, sdk-ts, claude-vscode). The refusal is a 400 request fault, so the
// credential is neither cooled down nor rotated away from.
func claudeSubscriptionClientGuard(fp claudeFingerprintPolicy, detection helps.ClaudeCodeRequestDetection) error {
	if os.Getenv(claudeSubscriptionClientGuardEnv) != "1" || !fp.AuthIsOAuthToken || detection.NativeClient {
		return nil
	}
	return statusErr{code: http.StatusBadRequest, msg: claudeSubscriptionClientGuardBody}
}
