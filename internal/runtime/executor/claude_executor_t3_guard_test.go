package executor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestClaudeExecutorT3ClientGuard(t *testing.T) {
	t.Setenv(claudeSubscriptionClientGuardEnv, "1")
	for _, operation := range []string{"message", "stream", "count_tokens"} {
		for _, client := range []struct {
			name      string
			userAgent string
			allowed   bool
		}{
			{"t3-code", "claude-cli/2.1.316 (external, t3-code, agent-sdk/0.3.276)", true},
			{"opencode", "opencode/1.4.2", false},
		} {
			t.Run(operation+"/"+client.name, func(t *testing.T) {
				var upstreamCalls int
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					upstreamCalls++
					if got := r.Header.Get("User-Agent"); got != client.userAgent {
						t.Errorf("upstream User-Agent = %q, want caller identity %q", got, client.userAgent)
					}
					switch operation {
					case "count_tokens":
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"input_tokens":1}`)
					case "stream":
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_t3\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-5\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
					default:
						w.Header().Set("Content-Type", "application/json")
						fmt.Fprint(w, `{"id":"msg_t3","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`)
					}
				}))
				defer server.Close()

				payload := []byte(`{"model":"claude-sonnet-5","max_tokens":8,"messages":[{"role":"user","content":"Reply ok"}],"metadata":{"user_id":"` + strings.ReplaceAll(claudeNativeHelperUserID, `"`, `\"`) + `"}}`)
				headers := claudeNativeHelperHeaders("claude-code-20250219,oauth-2025-04-20", "gzip, deflate, br, zstd")
				headers.Set("User-Agent", client.userAgent)
				executor := NewClaudeExecutor(&config.Config{DisableClaudeCloakMode: true})
				auth := claudeNativeHelperOAuthAuth(server.URL)
				request := cliproxyexecutor.Request{Model: "claude-sonnet-5", Payload: payload}
				options := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatClaude, OriginalRequest: payload, Headers: headers}
				var err error
				switch operation {
				case "message":
					_, err = executor.Execute(context.Background(), auth, request, options)
				case "count_tokens":
					_, err = executor.countTokensUpstream(context.Background(), auth, request, options)
				case "stream":
					var stream *cliproxyexecutor.StreamResult
					stream, err = executor.ExecuteStream(context.Background(), auth, request, options)
					if err == nil {
						for chunk := range stream.Chunks {
							if chunk.Err != nil && chunk.Err != io.EOF {
								err = chunk.Err
							}
						}
					}
				}
				if client.allowed {
					if err != nil || upstreamCalls != 1 {
						t.Fatalf("%s: err = %v, upstream calls = %d, want success and one call", operation, err, upstreamCalls)
					}
				} else if err == nil || !strings.Contains(err.Error(), "serve only native Claude Code clients") || upstreamCalls != 0 {
					t.Fatalf("%s: err = %v, upstream calls = %d, want native-client refusal before upstream", operation, err, upstreamCalls)
				}
			})
		}
	}
}
