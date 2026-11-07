package main

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const claudeHealthy = `{"five_hour":{"utilization":10},"seven_day":{"utilization":42}}`
const claudeLow = `{"five_hour":{"utilization":20},"seven_day":{"utilization":92}}`
const claudeCritical = `{"five_hour":{"utilization":99},"seven_day":{"utilization":99}}`

func resetRuntime(t *testing.T) {
	t.Helper()
	runtime.mu.Lock()
	runtime.cache = map[string]observation{}
	runtime.catalog = nil
	runtime.inflight = map[string]bool{}
	runtime.cfg = defaultSettings
	runtime.cacheTT = 5 * 60 * 1e9
	runtime.timeout = 8 * 1e9
	runtime.mu.Unlock()
}

func candidate(id, provider string, priority int) pluginapi.SchedulerAuthCandidate {
	return pluginapi.SchedulerAuthCandidate{ID: id, Provider: provider, Priority: priority}
}

// fakeHost answers host.auth.list, host.auth.get and host.http.do.
// probes is written by concurrent probe goroutines, so it is guarded.
type fakeHost struct {
	mu          sync.Mutex
	quotaBodies map[string]string
	tokens      map[string]string
	probes      map[string]int
	httpStatus  int
}

func (h *fakeHost) probeCount(token string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.probes[token]
}

func (h *fakeHost) call(_ context.Context, method string, payload any) (json.RawMessage, error) {
	switch method {
	case "host.auth.list":
		files := make([]pluginapi.HostAuthFileEntry, 0, len(h.quotaBodies))
		for id := range h.quotaBodies {
			files = append(files, pluginapi.HostAuthFileEntry{ID: id, AuthIndex: id, Provider: "claude"})
		}
		return json.Marshal(map[string]any{"files": files})
	case "host.auth.get":
		request := payload.(pluginapi.HostAuthGetRequest)
		token := h.tokens[request.AuthIndex]
		return json.Marshal(pluginapi.HostAuthGetResponse{
			AuthIndex: request.AuthIndex,
			JSON:      json.RawMessage(`{"access_token":"` + token + `"}`),
		})
	case "host.http.do":
		if h.httpStatus != 0 {
			return json.Marshal(pluginapi.HTTPResponse{StatusCode: h.httpStatus})
		}
		request := payload.(pluginapi.HTTPRequest)
		token := ""
		if values := request.Headers.Values("Authorization"); len(values) > 0 {
			token = values[0]
		}
		h.mu.Lock()
		h.probes[token]++
		body := h.quotaBodies[token]
		h.mu.Unlock()
		return json.Marshal(pluginapi.HTTPResponse{StatusCode: 200, Body: []byte(body)})
	}
	return nil, nil
}

func newFakeHost(quotas map[string]string) *fakeHost {
	host := &fakeHost{quotaBodies: map[string]string{}, tokens: map[string]string{}, probes: map[string]int{}}
	for id, body := range quotas {
		token := "Bearer " + id
		host.quotaBodies[token] = body
		host.tokens[id] = id
	}
	return host
}

func pickOnce(t *testing.T, host *fakeHost, candidates ...pluginapi.SchedulerAuthCandidate) pluginapi.SchedulerPickResponse {
	t.Helper()
	response, err := pick(pluginapi.SchedulerPickRequest{Provider: "claude", Candidates: candidates}, host.call)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if !response.Handled {
		t.Fatalf("pick() Handled = false, want a decision")
	}
	return response
}

func TestTierThresholds(t *testing.T) {
	cfg := defaultSettings
	cases := map[float64]int{
		1.0:   0,
		0.5:   0,
		0.15:  0,
		0.14:  2,
		0.05:  2,
		0.049: 3,
		0:     3,
		-1:    1,
	}
	for score, want := range cases {
		if got := tier(score, cfg); got != want {
			t.Fatalf("tier(%v) = %d, want %d", score, got, want)
		}
	}
}

func TestPickPrefersHighestRemainingQuota(t *testing.T) {
	resetRuntime(t)
	// a is configured with the best priority but the least headroom; quota must win.
	host := newFakeHost(map[string]string{
		"a": `{"seven_day":{"utilization":70}}`,
		"b": `{"seven_day":{"utilization":10}}`,
		"c": `{"seven_day":{"utilization":40}}`,
	})
	response := pickOnce(t, host, candidate("a", "claude", 1), candidate("b", "claude", 20), candidate("c", "claude", 10))
	if response.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b", response.AuthID)
	}
}

func TestPickUsesBottleneckWindow(t *testing.T) {
	resetRuntime(t)
	host := newFakeHost(map[string]string{
		"split": claudeLow,
		"flat":  `{"seven_day":{"utilization":50}}`,
	})
	response := pickOnce(t, host, candidate("split", "claude", 10), candidate("flat", "claude", 10))
	if response.AuthID != "flat" {
		t.Fatalf("AuthID = %q, want flat", response.AuthID)
	}
}

func TestPickDemotesCriticalBehindLow(t *testing.T) {
	resetRuntime(t)
	host := newFakeHost(map[string]string{
		"critical": claudeCritical,
		"low":      claudeLow,
	})
	response := pickOnce(t, host, candidate("critical", "claude", 1), candidate("low", "claude", 9))
	if response.AuthID != "low" {
		t.Fatalf("AuthID = %q, want low", response.AuthID)
	}
}

func TestPickKeepsUnknownAheadOfExhausted(t *testing.T) {
	resetRuntime(t)
	host := newFakeHost(map[string]string{
		"exhausted": `{"seven_day":{"utilization":100}}`,
		"silent":    `{}`,
	})
	response := pickOnce(t, host, candidate("exhausted", "claude", 1), candidate("silent", "claude", 20))
	if response.AuthID != "silent" {
		t.Fatalf("AuthID = %q, want silent", response.AuthID)
	}
}

func TestPickCachesWithinTTL(t *testing.T) {
	resetRuntime(t)
	host := newFakeHost(map[string]string{"a": claudeHealthy})
	pickOnce(t, host, candidate("a", "claude", 10))
	pickOnce(t, host, candidate("a", "claude", 10))
	if host.probeCount("Bearer a") != 1 {
		t.Fatalf("probes = %d, want 1 within the cache TTL", host.probeCount("Bearer a"))
	}
}

func TestPickReprobesAfterTTL(t *testing.T) {
	resetRuntime(t)
	runtime.mu.Lock()
	runtime.cacheTT = 1
	runtime.mu.Unlock()
	host := newFakeHost(map[string]string{"a": claudeHealthy})
	pickOnce(t, host, candidate("a", "claude", 10))
	// Force the cached observation to look stale instead of sleeping in a unit test.
	runtime.mu.Lock()
	entry := runtime.cache["a"]
	entry.fetchedAt = entry.fetchedAt.Add(-time.Hour)
	runtime.cache["a"] = entry
	runtime.mu.Unlock()
	pickOnce(t, host, candidate("a", "claude", 10))
	if host.probeCount("Bearer a") != 2 {
		t.Fatalf("probes = %d, want 2 after TTL expiry", host.probeCount("Bearer a"))
	}
}

func TestPickIgnoresProvidersOutsideQuotaProviders(t *testing.T) {
	resetRuntime(t)
	cfg := defaultSettings
	cfg.Providers = []string{"codex"}
	applySettings(cfg)
	host := newFakeHost(map[string]string{"a": claudeHealthy})
	response, err := pick(pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Candidates: []pluginapi.SchedulerAuthCandidate{candidate("a", "claude", 10)},
	}, host.call)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if response.AuthID != "a" || !response.Handled {
		t.Fatalf("response = %#v, want the single candidate still selected", response)
	}
	if host.probeCount("Bearer a") != 0 {
		t.Fatalf("probes = %d, want 0 for an unprobed provider", host.probeCount("Bearer a"))
	}
}

func TestPickRespectsIgnoreProviders(t *testing.T) {
	resetRuntime(t)
	cfg := defaultSettings
	cfg.IgnoreList = []string{"claude"}
	applySettings(cfg)
	host := newFakeHost(map[string]string{"a": claudeHealthy})
	response, err := pick(pluginapi.SchedulerPickRequest{
		Provider:   "claude",
		Candidates: []pluginapi.SchedulerAuthCandidate{candidate("a", "claude", 10)},
	}, host.call)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if response.Handled {
		t.Fatalf("Handled = true, want delegation to the built-in scheduler")
	}
}

func TestPickWithoutCandidatesDelegates(t *testing.T) {
	resetRuntime(t)
	response, err := pick(pluginapi.SchedulerPickRequest{Provider: "claude"}, newFakeHost(nil).call)
	if err != nil {
		t.Fatalf("pick() error = %v", err)
	}
	if response.Handled || response.AuthID != "" {
		t.Fatalf("response = %#v, want delegation", response)
	}
}

func TestPickFallsBackToPriorityWhenProbeFails(t *testing.T) {
	resetRuntime(t)
	host := newFakeHost(map[string]string{"a": claudeHealthy, "b": claudeHealthy})
	host.httpStatus = http.StatusUnauthorized
	response := pickOnce(t, host, candidate("a", "claude", 5), candidate("b", "claude", 1))
	if response.AuthID != "b" {
		t.Fatalf("AuthID = %q, want b (lower priority number wins on unknown quota)", response.AuthID)
	}
}

func TestScoreClaudePayload(t *testing.T) {
	if score, ok := scoreClaudePayload([]byte(claudeLow)); !ok || score > 0.09 || score < 0.07 {
		t.Fatalf("score = %v ok = %v, want ~0.08", score, ok)
	}
	if _, ok := scoreClaudePayload([]byte(`{}`)); ok {
		t.Fatal("empty payload scored")
	}
}

func TestScoreCodexPayloadHonorsLimitReached(t *testing.T) {
	body := `{"rate_limit":{"primary_window":{"used_percent":12,"limit_reached":true},"secondary_window":{"used_percent":5}}}`
	if score, ok := scoreCodexPayload([]byte(body)); !ok || score != 0 {
		t.Fatalf("score = %v ok = %v, want 0/true", score, ok)
	}
	body = `{"rate_limit":{"primary_window":{"used_percent":10},"secondary_window":{"used_percent":60}}}`
	if score, ok := scoreCodexPayload([]byte(body)); !ok || score != 0.4 {
		t.Fatalf("score = %v ok = %v, want 0.4", score, ok)
	}
}

func TestCredentialTokensReadsBothProviders(t *testing.T) {
	if token, account := credentialTokens(json.RawMessage(`{"access_token":"t","account_id":"a"}`)); token != "t" || account != "a" {
		t.Fatalf("claude/codex tokens = %q/%q", token, account)
	}
	if token, _ := credentialTokens(json.RawMessage(`{}`)); token != "" {
		t.Fatalf("empty token = %q", token)
	}
}

func TestConfigRejectsUnknownKeysAndBadThresholds(t *testing.T) {
	if _, err := parseSettings([]byte("enabled: true\npriority: 0\nnope: 1\n")); err == nil {
		t.Fatal("unknown key accepted")
	}
	if _, err := parseSettings([]byte("enabled: true\npriority: 0\nlow_threshold: 0.2\ncritical_threshold: 0.5\n")); err == nil {
		t.Fatal("critical_threshold above low_threshold accepted")
	}
	if _, err := parseSettings([]byte("enabled: true\npriority: 0\n---\nsecond: doc\n")); err == nil {
		t.Fatal("two YAML documents accepted")
	}
}

func TestRegisterDeclaresCapabilities(t *testing.T) {
	resetRuntime(t)
	result := registerResult(10)
	if result.Capabilities["scheduler"] != true || result.Capabilities["management_api"] != true {
		t.Fatalf("capabilities = %#v", result.Capabilities)
	}
	if result.Metadata.Name == "" || result.Metadata.GitHubRepository == "" {
		t.Fatalf("metadata = %#v", result.Metadata)
	}
}

func TestManagementStatusReportsObservations(t *testing.T) {
	resetRuntime(t)
	host := newFakeHost(map[string]string{"a": claudeHealthy})
	pickOnce(t, host, candidate("a", "claude", 10))
	value, err := managementHandle(nil, host.call)
	if err != nil {
		t.Fatalf("managementHandle error = %v", err)
	}
	response := value.(pluginapi.ManagementResponse)
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	var summary map[string]any
	if errDecode := json.Unmarshal(response.Body, &summary); errDecode != nil {
		t.Fatalf("decode summary: %v", errDecode)
	}
	rows := summary["credentials"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["auth_id"] != "a" {
		t.Fatalf("rows = %#v", rows)
	}
}

func TestShutdownClearsState(t *testing.T) {
	resetRuntime(t)
	runtime.remember("a", 0.5, "")
	if _, err := dispatch("plugin.shutdown", []byte("{}"), nil); err != nil {
		t.Fatalf("shutdown error = %v", err)
	}
	if _, ok := runtime.observe("a"); ok {
		t.Fatal("cache survived shutdown")
	}
}
