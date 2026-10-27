package auth

import (
	"context"
	"net/http"
	"strconv"
	"testing"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func claudeQuotaAuth(id string, reset7d time.Time, util7d, util5h float64) *Auth {
	return &Auth{
		ID:       id,
		Provider: "claude",
		Quota: QuotaState{
			ObservedAt: time.Now(),
			Signals: map[string]string{
				"Anthropic-Ratelimit-Unified-7d-Reset":       strconv.FormatInt(reset7d.Unix(), 10),
				"Anthropic-Ratelimit-Unified-7d-Utilization": strconv.FormatFloat(util7d, 'f', -1, 64),
				"Anthropic-Ratelimit-Unified-5h-Reset":       strconv.FormatInt(time.Now().Add(3*time.Hour).Unix(), 10),
				"Anthropic-Ratelimit-Unified-5h-Utilization": strconv.FormatFloat(util5h, 'f', -1, 64),
			},
		},
	}
}

func codexQuotaAuth(id string, signals map[string]string, observedAt time.Time) *Auth {
	return &Auth{ID: id, Provider: "codex", Quota: QuotaState{ObservedAt: observedAt, Signals: signals}}
}

func pickID(t *testing.T, selector Selector, ctx context.Context, provider string, opts cliproxyexecutor.Options, auths []*Auth) string {
	t.Helper()
	got, err := selector.Pick(ctx, provider, "", opts, auths)
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if got == nil {
		t.Fatal("Pick() auth = nil")
	}
	return got.ID
}

func TestSoonestResetPicksEarliestLongWindowReset(t *testing.T) {
	t.Parallel()
	now := time.Now()
	auths := []*Auth{
		claudeQuotaAuth("a", now.Add(72*time.Hour), 0.1, 0.1),
		claudeQuotaAuth("b", now.Add(24*time.Hour), 0.1, 0.1),
		claudeQuotaAuth("c", now.Add(120*time.Hour), 0.1, 0.1),
	}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("picked %q, want b (earliest weekly reset)", got)
	}
}

func TestSoonestResetShortWindowGuard(t *testing.T) {
	t.Parallel()
	now := time.Now()
	for _, tc := range []struct {
		util5h float64
		want   string
	}{{0.9, "later"}, {0.95, "later"}, {0.89, "soonest"}} {
		auths := []*Auth{
			claudeQuotaAuth("soonest", now.Add(24*time.Hour), 0.5, tc.util5h),
			claudeQuotaAuth("later", now.Add(96*time.Hour), 0.1, 0.0),
		}
		if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != tc.want {
			t.Fatalf("5h utilization %.2f: picked %q, want %q", tc.util5h, got, tc.want)
		}
	}
}

func TestSoonestResetIgnoresExpiredShortWindow(t *testing.T) {
	t.Parallel()
	now := time.Now()
	stale := claudeQuotaAuth("soonest", now.Add(24*time.Hour), 0.5, 0.99)
	stale.Quota.Signals["Anthropic-Ratelimit-Unified-5h-Reset"] = strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)
	auths := []*Auth{stale, claudeQuotaAuth("later", now.Add(96*time.Hour), 0.1, 0.0)}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "soonest" {
		t.Fatalf("picked %q, want soonest (its 5h window already reset)", got)
	}
}

func TestSoonestResetUnknownAndExpiredSortLast(t *testing.T) {
	t.Parallel()
	now := time.Now()
	auths := []*Auth{
		{ID: "a-no-signals", Provider: "claude"},
		claudeQuotaAuth("b-expired", now.Add(-time.Hour), 0.99, 0.0),
		claudeQuotaAuth("c-known", now.Add(100*time.Hour), 0.0, 0.0),
	}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "c-known" {
		t.Fatalf("picked %q, want c-known", got)
	}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths[:2]); got != "a-no-signals" {
		t.Fatalf("picked %q among unknowns, want a-no-signals (ID order)", got)
	}
}

func TestSoonestResetTieBreaks(t *testing.T) {
	t.Parallel()
	reset := time.Now().Add(48 * time.Hour)
	auths := []*Auth{
		claudeQuotaAuth("a", reset, 0.5, 0.0),
		claudeQuotaAuth("b", reset, 0.8, 0.0),
	}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("picked %q, want b (higher weekly utilization)", got)
	}
	auths[1] = claudeQuotaAuth("b", reset, 0.5, 0.0)
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("picked %q, want a (ID order)", got)
	}
}

func TestSoonestResetCodexWindowsByLength(t *testing.T) {
	t.Parallel()
	now := time.Now()
	epoch := func(d time.Duration) string { return strconv.FormatInt(now.Add(d).Unix(), 10) }
	weeklyPrimary := codexQuotaAuth("a", map[string]string{
		"X-Codex-Primary-Window-Minutes":   "10080",
		"X-Codex-Primary-Reset-At":         epoch(144 * time.Hour),
		"X-Codex-Primary-Used-Percent":     "10",
		"X-Codex-Secondary-Window-Minutes": "300",
		"X-Codex-Secondary-Reset-At":       epoch(time.Hour),
		"X-Codex-Secondary-Used-Percent":   "10",
	}, now)
	weeklySecondary := codexQuotaAuth("b", map[string]string{
		"X-Codex-Primary-Window-Minutes":   "300",
		"X-Codex-Primary-Reset-At":         epoch(30 * time.Minute),
		"X-Codex-Primary-Used-Percent":     "10",
		"X-Codex-Secondary-Window-Minutes": "10080",
		"X-Codex-Secondary-Reset-At":       epoch(48 * time.Hour),
		"X-Codex-Secondary-Used-Percent":   "10",
	}, now)
	auths := []*Auth{weeklyPrimary, weeklySecondary}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "codex", cliproxyexecutor.Options{}, auths); got != "b" {
		t.Fatalf("picked %q, want b (weekly window resets in 48h, not 144h)", got)
	}
	weeklySecondary.Quota.Signals["X-Codex-Primary-Used-Percent"] = "90"
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "codex", cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("picked %q, want a (b's short window is at 90%%)", got)
	}
}

func TestSoonestResetCodexResetAfterUsesObservedAt(t *testing.T) {
	t.Parallel()
	now := time.Now()
	relative := codexQuotaAuth("a", map[string]string{
		"X-Codex-Primary-Window-Minutes":      "10080",
		"X-Codex-Primary-Reset-After-Seconds": strconv.Itoa(int((2 * time.Hour).Seconds())),
	}, now.Add(-time.Hour))
	absolute := codexQuotaAuth("b", map[string]string{
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(90*time.Minute).Unix(), 10),
	}, now)
	auths := []*Auth{relative, absolute}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "codex", cliproxyexecutor.Options{}, auths); got != "a" {
		t.Fatalf("picked %q, want a (resets 1h from now, observed 1h ago with 2h left)", got)
	}
}

func TestSoonestResetSkipsUnavailableAndKeepsPriorityTiers(t *testing.T) {
	t.Parallel()
	now := time.Now()
	disabled := claudeQuotaAuth("a-disabled", now.Add(time.Hour), 0.1, 0.0)
	disabled.Disabled = true
	cooling := claudeQuotaAuth("b-cooling", now.Add(2*time.Hour), 0.1, 0.0)
	cooling.Unavailable = true
	cooling.NextRetryAfter = now.Add(10 * time.Minute)
	auths := []*Auth{disabled, cooling, claudeQuotaAuth("c", now.Add(72*time.Hour), 0.1, 0.0)}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "c" {
		t.Fatalf("picked %q, want c (others unavailable)", got)
	}

	preferred := claudeQuotaAuth("d-priority", now.Add(144*time.Hour), 0.0, 0.0)
	preferred.Attributes = map[string]string{"priority": "10"}
	auths = []*Auth{claudeQuotaAuth("e", now.Add(time.Hour), 0.1, 0.0), preferred}
	if got := pickID(t, &SoonestResetSelector{}, context.Background(), "claude", cliproxyexecutor.Options{}, auths); got != "d-priority" {
		t.Fatalf("picked %q, want d-priority (higher priority tier wins)", got)
	}
}

func TestSoonestResetPrefersCodexWebsocketAuths(t *testing.T) {
	t.Parallel()
	now := time.Now()
	soonest := codexQuotaAuth("a", map[string]string{
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(time.Hour).Unix(), 10),
	}, now)
	websocket := codexQuotaAuth("b", map[string]string{
		"X-Codex-Primary-Window-Minutes": "10080",
		"X-Codex-Primary-Reset-At":       strconv.FormatInt(now.Add(100*time.Hour).Unix(), 10),
	}, now)
	websocket.Attributes = map[string]string{"websockets": "true"}
	ctx := cliproxyexecutor.WithDownstreamWebsocket(context.Background())
	if got := pickID(t, &SoonestResetSelector{}, ctx, "codex", cliproxyexecutor.Options{}, []*Auth{soonest, websocket}); got != "b" {
		t.Fatalf("picked %q, want b (websocket request prefers websocket auth)", got)
	}
}

func sessionOpts(sessionID string, agentID string) cliproxyexecutor.Options {
	headers := http.Header{"X-Claude-Code-Session-Id": []string{sessionID}}
	if agentID != "" {
		headers.Set("X-Claude-Code-Agent-Id", agentID)
	}
	return cliproxyexecutor.Options{
		Headers:         headers,
		OriginalRequest: []byte(`{"messages":[{"role":"user","content":"task"}]}`),
		Metadata:        map[string]any{},
	}
}

func TestSoonestResetWithSessionAffinity(t *testing.T) {
	t.Parallel()
	now := time.Now()
	selector := NewSessionAffinitySelectorWithConfig(SessionAffinityConfig{
		Fallback: &SoonestResetSelector{},
		TTL:      time.Minute,
	})
	defer selector.Stop()

	x := claudeQuotaAuth("x", now.Add(24*time.Hour), 0.5, 0.1)
	y := claudeQuotaAuth("y", now.Add(48*time.Hour), 0.1, 0.1)
	z := claudeQuotaAuth("z", now.Add(96*time.Hour), 0.1, 0.1)
	auths := []*Auth{x, y, z}
	pick := func(opts cliproxyexecutor.Options, candidates []*Auth) string {
		got, err := selector.Pick(context.Background(), "claude", "claude-haiku-5-5", opts, candidates)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		return got.ID
	}

	if got := pick(sessionOpts("session-a", ""), auths); got != "x" {
		t.Fatalf("session-a cold binding = %q, want x (soonest reset)", got)
	}
	if got := pick(sessionOpts("session-a", "worker-1"), auths); got != "x" {
		t.Fatalf("session-a subagent = %q, want parent's x", got)
	}

	x.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(200*time.Hour).Unix(), 10)
	for i := 0; i < 3; i++ {
		if got := pick(sessionOpts("session-a", ""), auths); got != "x" {
			t.Fatalf("session-a moved to %q after signals changed, want it to stay on x", got)
		}
	}
	if got := pick(sessionOpts("session-b", ""), auths); got != "y" {
		t.Fatalf("session-b cold binding = %q, want y (now soonest)", got)
	}

	x.Quota.Signals["Anthropic-Ratelimit-Unified-7d-Reset"] = strconv.FormatInt(now.Add(12*time.Hour).Unix(), 10)
	x.Unavailable = true
	x.NextRetryAfter = now.Add(30 * time.Minute)
	if got := pick(sessionOpts("session-a", ""), auths); got != "y" {
		t.Fatalf("session-a failover = %q, want y (next soonest available)", got)
	}
	if got := pick(sessionOpts("session-a", ""), auths); got != "y" {
		t.Fatalf("session-a after failover = %q, want to stay on y", got)
	}
}
