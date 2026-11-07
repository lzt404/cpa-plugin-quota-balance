package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const (
	pluginID      = "quota-balance"
	pluginVersion = "1.0.0"
	pluginAuthor  = "lzt404"
	pluginRepo    = "https://github.com/lzt404/cpa-plugin-quota-balance"

	// Claude exposes subscription usage on the OAuth usage endpoint.
	claudeUsageEndpoint = "https://api.anthropic.com/api/oauth/usage"
	claudeOAuthBeta     = "oauth-2025-04-20"
	// Codex exposes rate-limit windows through the ChatGPT backend.
	codexUsageEndpoint = "https://chatgpt.com/backend-api/wham/usage"
	quotaUserAgent     = "cpa-quota-balance/1.0.0"
)

// settings is the plugin-owned configuration block under plugins.configs.quota-balance.
// The host injects enabled and priority into every plugin configuration, so the runtime
// document is decoded into rawSettings where those keys are consumed.
type settings struct {
	CacheTTL   string   `yaml:"cache_ttl"`
	Timeout    string   `yaml:"timeout"`
	Low        float64  `yaml:"low_threshold"`
	Critical   float64  `yaml:"critical_threshold"`
	IgnoreList []string `yaml:"ignore_providers"`
	Providers  []string `yaml:"quota_providers"`
	// Endpoints override the provider usage URLs. Leave empty to use the vendor
	// defaults; set them when the account traffic is routed through a mirror.
	ClaudeEndpoint string `yaml:"claude_usage_endpoint"`
	CodexEndpoint  string `yaml:"codex_usage_endpoint"`
}

var defaultSettings = settings{
	CacheTTL:  "5m",
	Timeout:   "8s",
	Low:       0.15,
	Critical:  0.05,
	Providers: []string{"claude", "codex"},
}

type rawSettings struct {
	Settings settings `yaml:",inline"`
	Enabled  any      `yaml:"enabled"`
	Priority any      `yaml:"priority"`
	Store    any      `yaml:"store"`
}

// observation is the last known quota signal for one credential.
type observation struct {
	fetchedAt time.Time
	score     float64 // bottleneck remaining fraction; -1 when nothing usable was observed
	err       string
}

// pickRecord is one scheduling decision, kept for the management status route.
type pickRecord struct {
	Seq     int       `json:"seq"`
	At      time.Time `json:"at"`
	AuthID  string    `json:"auth_id"`
	Score   float64   `json:"remaining"`
	Tier    string    `json:"tier"`
	Ordered []string  `json:"ranked"`
}

type balancer struct {
	mu      sync.Mutex
	cache   map[string]observation
	catalog []pluginapi.HostAuthFileEntry
	cfg     settings
	cacheTT time.Duration
	timeout time.Duration
	// inflight deduplicates concurrent quota probes per credential.
	inflight map[string]bool
	// picks keeps the most recent decisions for diagnosis.
	picks []pickRecord
	// pickSeq is a monotonic decision counter so clients can detect a new pick
	// even after the bounded picks buffer starts dropping old entries.
	pickSeq int
}

var runtime = &balancer{
	cache:    map[string]observation{},
	inflight: map[string]bool{},
	cfg:      defaultSettings,
	cacheTT:  5 * time.Minute,
	timeout:  8 * time.Second,
}

// hostCallbackFunc issues a plugin-to-host RPC and returns its raw result.
type hostCallbackFunc func(context.Context, string, any) (json.RawMessage, error)

func parseSettings(raw []byte) (settings, error) {
	cfg := defaultSettings
	decoder := yaml.NewDecoder(strings.NewReader(string(raw)))
	decoder.KnownFields(true)
	var document rawSettings
	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return cfg, nil
		}
		return cfg, err
	}
	var extra any
	if rest := decoder.Decode(&extra); !errors.Is(rest, io.EOF) {
		if rest != nil {
			return cfg, rest
		}
		return cfg, errors.New("configuration must contain one YAML document")
	}
	cfg = document.Settings
	if strings.TrimSpace(cfg.CacheTTL) == "" {
		cfg.CacheTTL = defaultSettings.CacheTTL
	}
	if strings.TrimSpace(cfg.Timeout) == "" {
		cfg.Timeout = defaultSettings.Timeout
	}
	if len(cfg.Providers) == 0 {
		cfg.Providers = append([]string(nil), defaultSettings.Providers...)
	}
	if endpoint := strings.TrimSpace(cfg.ClaudeEndpoint); endpoint != "" {
		if errURL := validateEndpoint(endpoint); errURL != nil {
			return cfg, fmt.Errorf("claude_usage_endpoint: %w", errURL)
		}
	}
	if endpoint := strings.TrimSpace(cfg.CodexEndpoint); endpoint != "" {
		if errURL := validateEndpoint(endpoint); errURL != nil {
			return cfg, fmt.Errorf("codex_usage_endpoint: %w", errURL)
		}
	}
	if cfg.Low <= 0 || cfg.Low > 1 {
		return cfg, fmt.Errorf("low_threshold must be in (0,1], got %v", cfg.Low)
	}
	if cfg.Critical < 0 || cfg.Critical >= cfg.Low {
		return cfg, fmt.Errorf("critical_threshold must be in [0,%v), got %v", cfg.Low, cfg.Critical)
	}
	return cfg, nil
}

func applySettings(cfg settings) {
	cacheTTL, errTTL := time.ParseDuration(cfg.CacheTTL)
	if errTTL != nil || cacheTTL <= 0 {
		cacheTTL = 5 * time.Minute
	}
	timeout, errTimeout := time.ParseDuration(cfg.Timeout)
	if errTimeout != nil || timeout <= 0 {
		timeout = 8 * time.Second
	}
	runtime.mu.Lock()
	runtime.cfg = cfg
	runtime.cacheTT = cacheTTL
	runtime.timeout = timeout
	runtime.mu.Unlock()
}

// validateEndpoint accepts only absolute http(s) URLs so a typo cannot route a
// credential token to an arbitrary scheme.
func validateEndpoint(raw string) error {
	parsed, errParse := url.Parse(strings.TrimSpace(raw))
	if errParse != nil {
		return errors.New("invalid URL")
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return errors.New("URL must use http or https")
	}
	if parsed.Host == "" {
		return errors.New("URL must include a host")
	}
	return nil
}

// tier ranks a remaining fraction: healthy first, unknown next, then low, then critical.
func tier(score float64, cfg settings) int {
	switch {
	case score < 0:
		return 1
	case score < cfg.Critical:
		return 3
	case score < cfg.Low:
		return 2
	default:
		return 0
	}
}

func tierName(rank int) string {
	switch rank {
	case 0:
		return "healthy"
	case 1:
		return "unknown"
	case 2:
		return "low"
	default:
		return "critical"
	}
}

// remainingFromUsed converts a provider "percent used" value into a remaining fraction.
// Anthropic reports percentage points where 1.0 means 1%; Codex reports the same scale.
func remainingFromUsed(used float64) (float64, bool) {
	if math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 100 {
		return 0, false
	}
	return 1 - used/100, true
}

// claudeUsagePayload is the OAuth usage document. Windows are optional per plan.
type claudeUsagePayload struct {
	FiveHour *struct {
		Utilization *float64 `json:"utilization"`
	} `json:"five_hour"`
	SevenDay *struct {
		Utilization *float64 `json:"utilization"`
	} `json:"seven_day"`
	SevenDayOAuth *struct {
		Utilization *float64 `json:"utilization"`
	} `json:"seven_day_oauth_apps"`
}

func scoreClaudePayload(body []byte) (float64, bool) {
	var payload claudeUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return -1, false
	}
	worst := math.Inf(1)
	found := false
	for _, window := range []*float64{
		utilization(payload.FiveHour),
		utilization(payload.SevenDay),
		utilization(payload.SevenDayOAuth),
	} {
		if window == nil {
			continue
		}
		remaining, ok := remainingFromUsed(*window)
		if !ok {
			continue
		}
		if remaining < worst {
			worst = remaining
		}
		found = true
	}
	if !found {
		return -1, false
	}
	return worst, true
}

func utilization(window *struct {
	Utilization *float64 `json:"utilization"`
}) *float64 {
	if window == nil {
		return nil
	}
	return window.Utilization
}

// codexUsagePayload is the wham/usage document with optional window shapes.
type codexRateWindow struct {
	UsedPercent       *float64 `json:"used_percent"`
	LimitReached      *bool    `json:"limit_reached"`
	LimitWindowSecond float64  `json:"limit_window_seconds"`
}

type codexUsagePayload struct {
	RateLimit *struct {
		Primary   *codexRateWindow `json:"primary_window"`
		Secondary *codexRateWindow `json:"secondary_window"`
	} `json:"rate_limit"`
}

func scoreCodexPayload(body []byte) (float64, bool) {
	var payload codexUsagePayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return -1, false
	}
	if payload.RateLimit == nil {
		return -1, false
	}
	worst := math.Inf(1)
	found := false
	for _, window := range []*codexRateWindow{payload.RateLimit.Primary, payload.RateLimit.Secondary} {
		if window == nil {
			continue
		}
		if window.LimitReached != nil && *window.LimitReached {
			return 0, true
		}
		if window.UsedPercent == nil {
			continue
		}
		remaining, ok := remainingFromUsed(*window.UsedPercent)
		if !ok {
			continue
		}
		if remaining < worst {
			worst = remaining
		}
		found = true
	}
	if !found {
		return -1, false
	}
	return worst, true
}

func (b *balancer) setCatalog(entries []pluginapi.HostAuthFileEntry) {
	b.mu.Lock()
	b.catalog = entries
	b.mu.Unlock()
}

func (b *balancer) catalogEntry(candidate pluginapi.SchedulerAuthCandidate) pluginapi.HostAuthFileEntry {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, entry := range b.catalog {
		if entry.ID == candidate.ID || entry.AuthIndex == candidate.ID {
			return entry
		}
	}
	return pluginapi.HostAuthFileEntry{ID: candidate.ID, AuthIndex: candidate.ID, Provider: candidate.Provider}
}

func (b *balancer) observe(authID string) (observation, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.cache[authID]
	return entry, ok
}

func (b *balancer) fresh(authID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.cache[authID]
	return ok && time.Since(entry.fetchedAt) < b.cacheTT
}

func (b *balancer) remember(authID string, score float64, errText string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cache[authID] = observation{fetchedAt: time.Now(), score: score, err: errText}
}

// claim reserves a probe slot for one credential so concurrent picks do not repeat it.
func (b *balancer) claim(authID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflight[authID] {
		return false
	}
	b.inflight[authID] = true
	return true
}

func (b *balancer) release(authID string) {
	b.mu.Lock()
	delete(b.inflight, authID)
	b.mu.Unlock()
}

// credentialTokens pulls the fields needed to query a provider usage endpoint.
func credentialTokens(raw json.RawMessage) (accessToken, accountID string) {
	if len(raw) == 0 {
		return "", ""
	}
	var document map[string]any
	if err := json.Unmarshal(raw, &document); err != nil {
		return "", ""
	}
	read := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := document[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	accessToken = read("access_token", "accessToken")
	accountID = read("account_id", "chatgpt_account_id", "chatgptAccountId")
	return accessToken, accountID
}

// probeQuota fetches and scores one credential's remaining quota.
func (b *balancer) probeQuota(callback hostCallbackFunc, entry pluginapi.HostAuthFileEntry) {
	if callback == nil || strings.TrimSpace(entry.AuthIndex) == "" {
		return
	}
	provider := strings.ToLower(strings.TrimSpace(entry.Provider))
	b.mu.Lock()
	cfg, timeout := b.cfg, b.timeout
	b.mu.Unlock()
	if !containsFold(cfg.Providers, provider) || containsFold(cfg.IgnoreList, provider) {
		return
	}
	var endpoint, beta string
	switch provider {
	case "claude":
		endpoint, beta = claudeUsageEndpoint, claudeOAuthBeta
		if override := strings.TrimSpace(cfg.ClaudeEndpoint); override != "" {
			endpoint = override
		}
	case "codex":
		endpoint = codexUsageEndpoint
		if override := strings.TrimSpace(cfg.CodexEndpoint); override != "" {
			endpoint = override
		}
	default:
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	authRaw, errAuth := callback(ctx, pluginabi.MethodHostAuthGet, pluginapi.HostAuthGetRequest{AuthIndex: entry.AuthIndex})
	if errAuth != nil {
		b.remember(entry.ID, -1, "auth lookup failed: "+errAuth.Error())
		return
	}
	var credential pluginapi.HostAuthGetResponse
	if errDecode := json.Unmarshal(authRaw, &credential); errDecode != nil {
		b.remember(entry.ID, -1, "auth decode failed: "+errDecode.Error())
		return
	}
	accessToken, accountID := credentialTokens(credential.JSON)
	if accessToken == "" {
		b.remember(entry.ID, -1, "credential has no access token")
		return
	}

	headers := http.Header{
		"Authorization": []string{"Bearer " + accessToken},
		"Accept":        []string{"application/json"},
		"User-Agent":    []string{quotaUserAgent},
	}
	if provider == "claude" {
		headers.Set("anthropic-beta", beta)
	}
	if provider == "codex" && accountID != "" {
		headers.Set("Chatgpt-Account-Id", accountID)
	}

	httpRaw, errHTTP := callback(ctx, pluginabi.MethodHostHTTPDo, pluginapi.HTTPRequest{
		Method:  http.MethodGet,
		URL:     endpoint,
		Headers: headers,
	})
	if errHTTP != nil {
		b.remember(entry.ID, -1, "usage request failed: "+errHTTP.Error())
		return
	}
	var response pluginapi.HTTPResponse
	if errDecode := json.Unmarshal(httpRaw, &response); errDecode != nil {
		b.remember(entry.ID, -1, "usage response decode failed: "+errDecode.Error())
		return
	}
	if response.StatusCode != http.StatusOK {
		b.remember(entry.ID, -1, fmt.Sprintf("usage endpoint returned %d", response.StatusCode))
		return
	}

	var score float64
	var ok bool
	if provider == "claude" {
		score, ok = scoreClaudePayload(response.Body)
	} else {
		score, ok = scoreCodexPayload(response.Body)
	}
	if !ok {
		b.remember(entry.ID, -1, "usage payload had no usable window")
		return
	}
	b.remember(entry.ID, score, "")
}

func fetchCatalog(callback hostCallbackFunc) []pluginapi.HostAuthFileEntry {
	if callback == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, errCall := callback(ctx, pluginabi.MethodHostAuthList, map[string]any{})
	if errCall != nil {
		return nil
	}
	var listed struct {
		Files []pluginapi.HostAuthFileEntry `json:"files"`
	}
	if errDecode := json.Unmarshal(response, &listed); errDecode != nil {
		return nil
	}
	return listed.Files
}

type ranked struct {
	candidate pluginapi.SchedulerAuthCandidate
	quota     observation
	hasQuota  bool
	order     int
}

// pick ranks candidates by remaining quota and returns the best usable credential.
// An empty Handled response delegates to the host's built-in scheduler.
func pick(request pluginapi.SchedulerPickRequest, callback hostCallbackFunc) (pluginapi.SchedulerPickResponse, error) {
	if len(request.Candidates) == 0 {
		return pluginapi.SchedulerPickResponse{}, nil
	}
	runtime.mu.Lock()
	cfg := runtime.cfg
	runtime.mu.Unlock()

	if entries := fetchCatalog(callback); len(entries) > 0 {
		runtime.setCatalog(entries)
	}

	list := make([]ranked, 0, len(request.Candidates))
	for index, candidate := range request.Candidates {
		provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
		if containsFold(cfg.IgnoreList, provider) {
			continue
		}
		list = append(list, ranked{candidate: candidate, order: index})
	}
	if len(list) == 0 {
		return pluginapi.SchedulerPickResponse{}, nil
	}

	var wait sync.WaitGroup
	slots := make(chan struct{}, 4)
	for position := range list {
		candidate := list[position].candidate
		if runtime.fresh(candidate.ID) {
			observation, _ := runtime.observe(candidate.ID)
			list[position].quota = observation
			list[position].hasQuota = observation.score >= 0
			continue
		}
		if !runtime.claim(candidate.ID) {
			// Another pick is already probing this credential; keep the stale value if any.
			if observation, ok := runtime.observe(candidate.ID); ok {
				list[position].quota = observation
				list[position].hasQuota = observation.score >= 0
			}
			continue
		}
		entry := runtime.catalogEntry(candidate)
		wait.Add(1)
		slots <- struct{}{}
		go func(index int, item pluginapi.HostAuthFileEntry) {
			defer wait.Done()
			defer func() { <-slots }()
			defer runtime.release(item.ID)
			runtime.probeQuota(callback, item)
			if observation, ok := runtime.observe(item.ID); ok {
				list[index].quota = observation
				list[index].hasQuota = observation.score >= 0
			}
		}(position, entry)
	}
	wait.Wait()

	sort.SliceStable(list, func(left, right int) bool {
		leftRank, rightRank := tier(list[left].quota.score, cfg), tier(list[right].quota.score, cfg)
		if leftRank != rightRank {
			return leftRank < rightRank
		}
		if list[left].hasQuota && list[right].hasQuota && list[left].quota.score != list[right].quota.score {
			return list[left].quota.score > list[right].quota.score
		}
		if list[left].candidate.Priority != list[right].candidate.Priority {
			return list[left].candidate.Priority < list[right].candidate.Priority
		}
		return list[left].order < list[right].order
	})

	chosen := list[0]
	ordered := make([]string, 0, len(list))
	for _, item := range list {
		ordered = append(ordered, item.candidate.ID)
	}
	runtime.recordPick(chosen, ordered)
	return pluginapi.SchedulerPickResponse{AuthID: chosen.candidate.ID, Handled: true}, nil
}

func containsFold(list []string, value string) bool {
	for _, entry := range list {
		if strings.EqualFold(strings.TrimSpace(entry), strings.TrimSpace(value)) {
			return true
		}
	}
	return false
}

const maxPickRecords = 32

func (b *balancer) recordPick(chosen ranked, ordered []string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.pickSeq++
	b.picks = append(b.picks, pickRecord{
		Seq:     b.pickSeq,
		At:      time.Now(),
		AuthID:  chosen.candidate.ID,
		Score:   chosen.quota.score,
		Tier:    tierName(tier(chosen.quota.score, b.cfg)),
		Ordered: ordered,
	})
	if len(b.picks) > maxPickRecords {
		b.picks = append([]pickRecord(nil), b.picks[len(b.picks)-maxPickRecords:]...)
	}
}

func (b *balancer) summary() map[string]any {
	b.mu.Lock()
	defer b.mu.Unlock()
	rows := make([]map[string]any, 0, len(b.cache))
	for authID, entry := range b.cache {
		row := map[string]any{
			"auth_id":          authID,
			"remaining":        entry.score,
			"tier":             tierName(tier(entry.score, b.cfg)),
			"observed_seconds": int(time.Since(entry.fetchedAt).Seconds()),
		}
		if entry.err != "" {
			row["error"] = entry.err
		}
		rows = append(rows, row)
	}
	sort.Slice(rows, func(left, right int) bool {
		return rows[left]["auth_id"].(string) < rows[right]["auth_id"].(string)
	})
	return map[string]any{
		"config": map[string]any{
			"cache_ttl":          b.cfg.CacheTTL,
			"timeout":            b.cfg.Timeout,
			"low_threshold":      b.cfg.Low,
			"critical_threshold": b.cfg.Critical,
			"quota_providers":    b.cfg.Providers,
		},
		"credentials": rows,
		"picks":       append([]pickRecord(nil), b.picks...),
		"pick_seq":    b.pickSeq,
	}
}

func dispatch(method string, raw []byte, callback hostCallbackFunc) (any, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		var request struct {
			SchemaVersion uint32 `json:"schema_version"`
			ConfigYAML    []byte `json:"config_yaml"`
		}
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		cfg, err := parseSettings(request.ConfigYAML)
		if err != nil {
			return nil, err
		}
		applySettings(cfg)
		return registerResult(request.SchemaVersion), nil
	case pluginabi.MethodSchedulerPick:
		var request pluginapi.SchedulerPickRequest
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
		return pick(request, callback)
	case pluginabi.MethodManagementRegister:
		return managementRegistration(), nil
	case pluginabi.MethodManagementHandle:
		return managementHandle(raw, callback)
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		runtime.mu.Lock()
		runtime.cache = map[string]observation{}
		runtime.catalog = nil
		runtime.picks = nil
		runtime.pickSeq = 0
		runtime.mu.Unlock()
		return struct{}{}, nil
	default:
		return nil, fmt.Errorf("unsupported method %s", method)
	}
}

// managementRegistration exposes one diagnostic route under the plugin prefix.
func managementRegistration() pluginapi.ManagementRegistrationResponse {
	// Paths are relative to /v0/management. /plugins/<id>/status follows the
	// convention the plugin store UI and other scheduler plugins already use.
	return pluginapi.ManagementRegistrationResponse{Routes: []pluginapi.ManagementRoute{
		{
			Method:      http.MethodGet,
			Path:        "/plugins/" + pluginID + "/status",
			Description: "Quota Balance observations per credential",
		},
		{
			Method:      http.MethodPost,
			Path:        "/plugins/" + pluginID + "/refresh",
			Description: "Re-probe quota for all known credentials",
		},
	}}
}

func managementHandle(raw []byte, callback hostCallbackFunc) (any, error) {
	var request pluginapi.ManagementRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &request); err != nil {
			return nil, err
		}
	}
	if strings.Contains(strings.ToLower(request.Path), "refresh") {
		targets := request.Query["auth_id"]
		if len(targets) == 0 {
			targets = runtime.knownAuthIDs()
		}
		for _, candidate := range targets {
			if entry, ok := runtime.catalogEntryByID(candidate); ok {
				runtime.probeQuota(callback, entry)
			}
		}
	}
	body, errMarshal := json.Marshal(runtime.summary())
	if errMarshal != nil {
		return nil, errMarshal
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers:    http.Header{"Content-Type": []string{"application/json"}},
		Body:       body,
	}, nil
}

func (b *balancer) knownAuthIDs() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	ids := make([]string, 0, len(b.catalog))
	for _, entry := range b.catalog {
		if entry.ID != "" {
			ids = append(ids, entry.ID)
		}
	}
	return ids
}

func (b *balancer) catalogEntryByID(authID string) (pluginapi.HostAuthFileEntry, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, entry := range b.catalog {
		if entry.ID == authID || entry.AuthIndex == authID {
			return entry, true
		}
	}
	return pluginapi.HostAuthFileEntry{}, false
}

type registerPayload struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  map[string]any     `json:"capabilities"`
}

func registerResult(schemaVersion uint32) registerPayload {
	runtime.mu.Lock()
	cfg := runtime.cfg
	runtime.mu.Unlock()
	return registerPayload{
		SchemaVersion: schemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             "Quota Balance",
			Version:          pluginVersion,
			Author:           pluginAuthor,
			GitHubRepository: pluginRepo,
			ConfigFields: []pluginapi.ConfigField{
				{Name: "cache_ttl", Type: pluginapi.ConfigFieldTypeString, Default: cfg.CacheTTL, Description: "How long a quota observation keeps ranking a credential (Go duration)."},
				{Name: "timeout", Type: pluginapi.ConfigFieldTypeString, Default: cfg.Timeout, Description: "Per-credential quota probe timeout (Go duration)."},
				{Name: "low_threshold", Type: pluginapi.ConfigFieldTypeNumber, Default: cfg.Low, Description: "Remaining fraction below which a credential is demoted behind healthy ones."},
				{Name: "critical_threshold", Type: pluginapi.ConfigFieldTypeNumber, Default: cfg.Critical, Description: "Remaining fraction below which a credential is used only as a last resort."},
				{Name: "ignore_providers", Type: pluginapi.ConfigFieldTypeArray, Default: []string{}, Description: "Providers left entirely to the built-in scheduler."},
				{Name: "quota_providers", Type: pluginapi.ConfigFieldTypeArray, Default: cfg.Providers, Description: "Providers probed for quota. Supported: claude, codex."},
				{Name: "claude_usage_endpoint", Type: pluginapi.ConfigFieldTypeString, Default: "", Description: "Override the Claude usage URL (mirror or gateway). Empty uses api.anthropic.com."},
				{Name: "codex_usage_endpoint", Type: pluginapi.ConfigFieldTypeString, Default: "", Description: "Override the Codex usage URL (mirror or gateway). Empty uses chatgpt.com."},
			},
		},
		Capabilities: map[string]any{
			"scheduler":                   true,
			"scheduler_across_priorities": true,
			"management_api":              true,
		},
	}
}
