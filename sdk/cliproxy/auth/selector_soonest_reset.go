package auth

import (
	"context"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// soonestResetShortWindowCeiling demotes credentials whose short quota window
// (Claude 5h, Codex short window) is at or above this utilization. Binding a
// new session there would soon hit the short limit and force a rebind, which
// rewrites the session's whole prompt cache.
const soonestResetShortWindowCeiling = 0.9

// SoonestResetSelector picks the available credential whose longest quota
// window resets first, so quota that would otherwise expire at a reset is used
// before quota that lasts longer. Ordering, most preferred first:
//  1. short window below soonestResetShortWindowCeiling
//  2. no known future long-window reset
//  3. earliest long-window reset
//  4. highest long-window utilization
//  5. credential ID
//
// It reads only the passive quota snapshot in Auth.Quota.Signals, which is
// held in memory and empty after a restart. A credential with no snapshot, or
// whose window has rolled over, is tried first: one response records its real
// reset, and ranking by reset then applies. Ranking it last would starve it,
// because only a request it serves can fill the snapshot. Wrapped in
// SessionAffinitySelector it decides cold bindings and failover rebinds only;
// established bindings stay where they are.
type SoonestResetSelector struct{}

type quotaWindow struct {
	minutes     int
	resetAt     time.Time
	utilization float64
	hasUtil     bool
}

type soonestResetKey struct {
	guarded         bool
	knownReset      bool
	resetAt         time.Time
	longUtilization float64
}

// Pick selects the credential with the soonest long-window reset.
func (s *SoonestResetSelector) Pick(ctx context.Context, provider, model string, opts cliproxyexecutor.Options, auths []*Auth) (*Auth, error) {
	_ = opts
	now := time.Now()
	available, err := getSelectorAvailableAuths(ctx, auths, provider, model, now)
	if err != nil {
		return nil, err
	}
	available = preferCodexWebsocketAuths(ctx, provider, available)
	ranked := make([]*Auth, len(available))
	copy(ranked, available)
	keys := make(map[string]soonestResetKey, len(ranked))
	for _, auth := range ranked {
		keys[auth.ID] = soonestResetKeyFor(auth, now)
	}
	sort.SliceStable(ranked, func(i, j int) bool {
		return soonestResetLess(keys[ranked[i].ID], keys[ranked[j].ID], ranked[i].ID, ranked[j].ID)
	})
	return ranked[0], nil
}

func soonestResetLess(a, b soonestResetKey, idA, idB string) bool {
	if a.guarded != b.guarded {
		return !a.guarded
	}
	if a.knownReset != b.knownReset {
		return !a.knownReset
	}
	if a.knownReset && !a.resetAt.Equal(b.resetAt) {
		return a.resetAt.Before(b.resetAt)
	}
	if a.longUtilization != b.longUtilization {
		return a.longUtilization > b.longUtilization
	}
	return idA < idB
}

func soonestResetKeyFor(auth *Auth, now time.Time) soonestResetKey {
	windows := quotaWindowsFor(auth)
	if len(windows) == 0 {
		return soonestResetKey{}
	}
	long, short := windows[0], windows[0]
	for _, w := range windows[1:] {
		if w.minutes > long.minutes {
			long = w
		}
		if w.minutes < short.minutes {
			short = w
		}
	}
	key := soonestResetKey{}
	if !long.resetAt.IsZero() && long.resetAt.After(now) {
		key.knownReset = true
		key.resetAt = long.resetAt
		if long.hasUtil {
			key.longUtilization = long.utilization
		}
	}
	shortLive := short.resetAt.IsZero() || short.resetAt.After(now)
	if short.minutes < long.minutes && short.hasUtil && shortLive {
		key.guarded = short.utilization >= soonestResetShortWindowCeiling
	}
	return key
}

// quotaWindowsFor reads the windows a provider reported in its last response.
// Claude reports fixed 5h and 7d windows with utilization fractions and epoch
// reset times. Codex reports primary and secondary windows with their length
// in minutes and used percent, so the long window is found by length rather
// than by name.
func quotaWindowsFor(auth *Auth) []quotaWindow {
	if auth == nil || len(auth.Quota.Signals) == 0 {
		return nil
	}
	signals := make(http.Header, len(auth.Quota.Signals))
	for name, value := range auth.Quota.Signals {
		signals.Set(name, value)
	}
	observedAt := auth.Quota.ObservedAt
	var windows []quotaWindow
	for _, w := range []struct {
		name    string
		minutes int
	}{{"5h", 5 * 60}, {"7d", 7 * 24 * 60}} {
		prefix := "Anthropic-Ratelimit-Unified-" + w.name + "-"
		window := quotaWindow{minutes: w.minutes, resetAt: epochSeconds(signals.Get(prefix + "Reset"))}
		window.utilization, window.hasUtil = parseFraction(signals.Get(prefix + "Utilization"))
		if !window.resetAt.IsZero() || window.hasUtil {
			windows = append(windows, window)
		}
	}
	for _, name := range []string{"Primary", "Secondary"} {
		prefix := "X-Codex-" + name + "-"
		minutes, errMinutes := strconv.Atoi(strings.TrimSpace(signals.Get(prefix + "Window-Minutes")))
		if errMinutes != nil || minutes <= 0 {
			continue
		}
		window := quotaWindow{minutes: minutes, resetAt: epochSeconds(signals.Get(prefix + "Reset-At"))}
		if window.resetAt.IsZero() && !observedAt.IsZero() {
			if after, errAfter := strconv.ParseInt(strings.TrimSpace(signals.Get(prefix+"Reset-After-Seconds")), 10, 64); errAfter == nil && after >= 0 {
				window.resetAt = observedAt.Add(time.Duration(after) * time.Second)
			}
		}
		if percent, ok := parseFraction(signals.Get(prefix + "Used-Percent")); ok {
			window.utilization, window.hasUtil = percent/100, true
		}
		windows = append(windows, window)
	}
	return windows
}

func epochSeconds(raw string) time.Time {
	seconds, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}

func parseFraction(raw string) (float64, bool) {
	value, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return 0, false
	}
	return value, true
}
