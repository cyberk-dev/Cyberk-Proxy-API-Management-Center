package usagestore

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
)

type TokenStats struct {
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

type Detail struct {
	Timestamp string     `json:"timestamp"`
	LatencyMs int64      `json:"latency_ms"`
	Source    string     `json:"source"`
	AuthIndex string     `json:"auth_index"`
	Tokens    TokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
}

type ModelData struct {
	TotalRequests   int64    `json:"total_requests"`
	TotalTokens     int64    `json:"total_tokens"`
	SuccessCount    int64    `json:"success_count"`
	FailureCount    int64    `json:"failure_count"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	CachedTokens    int64    `json:"cached_tokens"`
	ReasoningTokens int64    `json:"reasoning_tokens"`
	LastActiveMs    int64    `json:"last_active_ms,omitempty"`
	Details         []Detail `json:"details"`
}

type APIData struct {
	TotalRequests   int64                 `json:"total_requests"`
	TotalTokens     int64                 `json:"total_tokens"`
	SuccessCount    int64                 `json:"success_count"`
	FailureCount    int64                 `json:"failure_count"`
	InputTokens     int64                 `json:"input_tokens"`
	OutputTokens    int64                 `json:"output_tokens"`
	CachedTokens    int64                 `json:"cached_tokens"`
	ReasoningTokens int64                 `json:"reasoning_tokens"`
	LastActiveMs    int64                 `json:"last_active_ms,omitempty"`
	Models          map[string]*ModelData `json:"models"`
}

type UsageSnapshot struct {
	TotalRequests int64               `json:"total_requests"`
	SuccessCount  int64               `json:"success_count"`
	FailureCount  int64               `json:"failure_count"`
	TotalTokens   int64               `json:"total_tokens"`
	APIs          map[string]*APIData `json:"apis"`
}

type ExportPayload struct {
	Version    int            `json:"version"`
	ExportedAt string         `json:"exported_at"`
	Usage      *UsageSnapshot `json:"usage"`
}

type ModelSummary struct {
	TotalRequests   int64    `json:"total_requests"`
	SuccessCount    int64    `json:"success_count"`
	FailureCount    int64    `json:"failure_count"`
	TotalTokens     int64    `json:"total_tokens"`
	InputTokens     int64    `json:"input_tokens"`
	OutputTokens    int64    `json:"output_tokens"`
	CachedTokens    int64    `json:"cached_tokens"`
	ReasoningTokens int64    `json:"reasoning_tokens"`
	LastActive      string   `json:"last_active,omitempty"`
	Details         []Detail `json:"details"`
}

type APISummary struct {
	TotalRequests int64                    `json:"total_requests"`
	TotalTokens   int64                    `json:"total_tokens"`
	Models        map[string]*ModelSummary `json:"models"`
}

type UsageSummarySnapshot struct {
	TotalRequests int64                   `json:"total_requests"`
	SuccessCount  int64                   `json:"success_count"`
	FailureCount  int64                   `json:"failure_count"`
	TotalTokens   int64                   `json:"total_tokens"`
	APIs          map[string]*APISummary  `json:"apis"`
}

type Store struct {
	mu   sync.RWMutex
	data UsageSnapshot
}

func New() *Store {
	return &Store{
		data: UsageSnapshot{
			APIs: make(map[string]*APIData),
		},
	}
}

func (s *Store) HandleUsage(_ context.Context, record usage.Record) {
	if s == nil {
		return
	}

	ts := record.RequestedAt
	if ts.IsZero() {
		ts = time.Now()
	}

	model := record.Model
	if model == "" {
		model = "unknown"
	}
	apiKey := record.APIKey
	if apiKey == "" {
		apiKey = "unknown"
	}

	totalTokens := record.Detail.TotalTokens
	if totalTokens == 0 {
		totalTokens = record.Detail.InputTokens + record.Detail.OutputTokens + record.Detail.ReasoningTokens
	}

	detail := Detail{
		Timestamp: ts.Format(time.RFC3339Nano),
		LatencyMs: record.Latency.Milliseconds(),
		Source:    record.Source,
		AuthIndex: record.AuthIndex,
		Tokens: TokenStats{
			InputTokens:     record.Detail.InputTokens,
			OutputTokens:    record.Detail.OutputTokens,
			ReasoningTokens: record.Detail.ReasoningTokens,
			CachedTokens:    record.Detail.CachedTokens,
			TotalTokens:     totalTokens,
		},
		Failed: record.Failed,
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	apiData := s.data.APIs[apiKey]
	if apiData == nil {
		apiData = &APIData{Models: make(map[string]*ModelData)}
		s.data.APIs[apiKey] = apiData
	}

	modelData := apiData.Models[model]
	if modelData == nil {
		modelData = &ModelData{}
		apiData.Models[model] = modelData
	}

	modelData.Details = append(modelData.Details, detail)
	modelData.TotalRequests++
	modelData.TotalTokens += totalTokens
	modelData.InputTokens += detail.Tokens.InputTokens
	modelData.OutputTokens += detail.Tokens.OutputTokens
	modelData.CachedTokens += detail.Tokens.CachedTokens
	modelData.ReasoningTokens += detail.Tokens.ReasoningTokens
	if detail.Failed {
		modelData.FailureCount++
	} else {
		modelData.SuccessCount++
	}
	if tsMs := ts.UnixMilli(); tsMs > modelData.LastActiveMs {
		modelData.LastActiveMs = tsMs
	}

	apiData.TotalRequests++
	apiData.TotalTokens += totalTokens
	apiData.InputTokens += detail.Tokens.InputTokens
	apiData.OutputTokens += detail.Tokens.OutputTokens
	apiData.CachedTokens += detail.Tokens.CachedTokens
	apiData.ReasoningTokens += detail.Tokens.ReasoningTokens
	if detail.Failed {
		apiData.FailureCount++
	} else {
		apiData.SuccessCount++
	}
	if modelData.LastActiveMs > apiData.LastActiveMs {
		apiData.LastActiveMs = modelData.LastActiveMs
	}

	s.data.TotalRequests++
	s.data.TotalTokens += totalTokens
	if record.Failed {
		s.data.FailureCount++
	} else {
		s.data.SuccessCount++
	}
}

func (s *Store) Snapshot() *UsageSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	cp := s.data
	cp.APIs = make(map[string]*APIData, len(s.data.APIs))
	for k, v := range s.data.APIs {
		cp.APIs[k] = v
	}
	return &cp
}

// SummarySnapshot returns aggregated usage without details arrays.
// If since is non-zero, only details with timestamp >= since are counted.
func (s *Store) SummarySnapshot(since time.Time) *UsageSummarySnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	filterTime := !since.IsZero()

	snap := &UsageSummarySnapshot{
		APIs: make(map[string]*APISummary, len(s.data.APIs)),
	}
	for apiKey, apiData := range s.data.APIs {
		as := &APISummary{
			Models: make(map[string]*ModelSummary, len(apiData.Models)),
		}
		for modelName, modelData := range apiData.Models {
			ms := &ModelSummary{Details: []Detail{}}
			var lastActiveTs string
			for _, d := range modelData.Details {
				if filterTime {
					ts, err := time.Parse(time.RFC3339Nano, d.Timestamp)
					if err != nil || ts.Before(since) {
						continue
					}
				}
				ms.TotalRequests++
				if d.Failed {
					ms.FailureCount++
				} else {
					ms.SuccessCount++
				}
				ms.InputTokens += d.Tokens.InputTokens
				ms.OutputTokens += d.Tokens.OutputTokens
				ms.CachedTokens += d.Tokens.CachedTokens
				ms.ReasoningTokens += d.Tokens.ReasoningTokens
				ms.TotalTokens += d.Tokens.TotalTokens
				if d.Timestamp > lastActiveTs {
					lastActiveTs = d.Timestamp
				}
			}
			if ms.TotalRequests == 0 {
				continue
			}
			ms.LastActive = lastActiveTs
			as.Models[modelName] = ms
			as.TotalRequests += ms.TotalRequests
			as.TotalTokens += ms.TotalTokens
		}
		if as.TotalRequests == 0 {
			continue
		}
		snap.APIs[apiKey] = as
		snap.TotalRequests += as.TotalRequests
		snap.TotalTokens += as.TotalTokens
		for _, ms := range as.Models {
			snap.SuccessCount += ms.SuccessCount
			snap.FailureCount += ms.FailureCount
		}
	}
	return snap
}

// RateLimitResolver mirrors (*ratelimit.Config).Resolve so the usagestore
// package can compute the rate-limit panel without importing ratelimit
// (which would create a cycle). The handler bridges to the real config store.
type RateLimitResolver interface {
	Resolve(apiKey, model string) (limit int, window time.Duration, applies bool)
}

type KeyDetailModelStats struct {
	Model           string `json:"model"`
	TotalRequests   int64  `json:"total_requests"`
	SuccessCount    int64  `json:"success_count"`
	FailureCount    int64  `json:"failure_count"`
	InputTokens     int64  `json:"input_tokens"`
	OutputTokens    int64  `json:"output_tokens"`
	CachedTokens    int64  `json:"cached_tokens"`
	ReasoningTokens int64  `json:"reasoning_tokens"`
	TotalTokens     int64  `json:"total_tokens"`
	LastActive      string `json:"last_active,omitempty"`
}

type KeyRateLimitWindow struct {
	Model    string `json:"model"`
	Window   string `json:"window"`
	WindowMs int64  `json:"window_ms"`
	Limit    int    `json:"limit"`
	Used     int    `json:"used"`
	ResetsAt int64  `json:"resets_at"`
}

type RecentDetailEntry struct {
	Detail
	Model string `json:"model"`
}

type KeyDetailResponse struct {
	APIKey          string                `json:"api_key"`
	TotalRequests   int64                 `json:"total_requests"`
	SuccessCount    int64                 `json:"success_count"`
	FailureCount    int64                 `json:"failure_count"`
	InputTokens     int64                 `json:"input_tokens"`
	OutputTokens    int64                 `json:"output_tokens"`
	CachedTokens    int64                 `json:"cached_tokens"`
	ReasoningTokens int64                 `json:"reasoning_tokens"`
	TotalTokens     int64                 `json:"total_tokens"`
	Models          []KeyDetailModelStats `json:"models"`
	RecentDetails   []RecentDetailEntry   `json:"recent_details"`
	RateLimits      []KeyRateLimitWindow  `json:"rate_limits"`
}

const (
	defaultRecentDetailsLimit = 500
	maxRecentDetailsLimit     = 5000
)

// KeyDetail returns a bounded snapshot of one API key's usage.
//
// When `since` is the zero value, aggregates are read directly from the
// running counters (O(models)). Otherwise details are scanned once with the
// `since` filter applied — JSON marshaling cost is still O(min(limit, N))
// because RecentDetails is capped.
//
// `rl` may be nil; in that case RateLimits is empty. `now` is injected so
// tests can pin the rate-limit window cutoff.
func (s *Store) KeyDetail(apiKey string, since time.Time, limit int, rl RateLimitResolver, now time.Time) *KeyDetailResponse {
	if limit <= 0 {
		limit = defaultRecentDetailsLimit
	}
	if limit > maxRecentDetailsLimit {
		limit = maxRecentDetailsLimit
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	apiData, ok := s.data.APIs[apiKey]
	if !ok {
		return nil
	}

	resp := &KeyDetailResponse{
		APIKey:        apiKey,
		Models:        make([]KeyDetailModelStats, 0, len(apiData.Models)),
		RecentDetails: []RecentDetailEntry{},
		RateLimits:    []KeyRateLimitWindow{},
	}

	filterTime := !since.IsZero()
	sinceMs := int64(0)
	if filterTime {
		sinceMs = since.UnixMilli()
	}

	// Collect candidate recent details from every model. We always touch every
	// detail when filtering by time; when not filtering we can shortcut by
	// taking the tail of each model's already-time-ordered slice.
	type candidate struct {
		entry RecentDetailEntry
		tsMs  int64
	}
	candidates := make([]candidate, 0, limit*2)

	for modelName, modelData := range apiData.Models {
		ms := KeyDetailModelStats{Model: modelName}
		var lastActiveMs int64

		if filterTime {
			for _, d := range modelData.Details {
				tsMs := parseTimestampMs(d.Timestamp)
				if tsMs < sinceMs {
					continue
				}
				ms.TotalRequests++
				ms.TotalTokens += d.Tokens.TotalTokens
				ms.InputTokens += d.Tokens.InputTokens
				ms.OutputTokens += d.Tokens.OutputTokens
				ms.CachedTokens += d.Tokens.CachedTokens
				ms.ReasoningTokens += d.Tokens.ReasoningTokens
				if d.Failed {
					ms.FailureCount++
				} else {
					ms.SuccessCount++
				}
				if tsMs > lastActiveMs {
					lastActiveMs = tsMs
				}
				candidates = append(candidates, candidate{
					entry: RecentDetailEntry{Detail: d, Model: modelName},
					tsMs:  tsMs,
				})
			}
		} else {
			ms.TotalRequests = modelData.TotalRequests
			ms.TotalTokens = modelData.TotalTokens
			ms.InputTokens = modelData.InputTokens
			ms.OutputTokens = modelData.OutputTokens
			ms.CachedTokens = modelData.CachedTokens
			ms.ReasoningTokens = modelData.ReasoningTokens
			ms.SuccessCount = modelData.SuccessCount
			ms.FailureCount = modelData.FailureCount
			lastActiveMs = modelData.LastActiveMs

			// Take the tail of this model's details (they are appended in time
			// order). Cross-model merging happens after the loop.
			start := len(modelData.Details) - limit
			if start < 0 {
				start = 0
			}
			for _, d := range modelData.Details[start:] {
				tsMs := parseTimestampMs(d.Timestamp)
				candidates = append(candidates, candidate{
					entry: RecentDetailEntry{Detail: d, Model: modelName},
					tsMs:  tsMs,
				})
			}
		}

		if ms.TotalRequests == 0 {
			continue
		}
		if lastActiveMs > 0 {
			ms.LastActive = time.UnixMilli(lastActiveMs).UTC().Format(time.RFC3339Nano)
		}

		resp.Models = append(resp.Models, ms)
		resp.TotalRequests += ms.TotalRequests
		resp.TotalTokens += ms.TotalTokens
		resp.InputTokens += ms.InputTokens
		resp.OutputTokens += ms.OutputTokens
		resp.CachedTokens += ms.CachedTokens
		resp.ReasoningTokens += ms.ReasoningTokens
		resp.SuccessCount += ms.SuccessCount
		resp.FailureCount += ms.FailureCount

		// Rate-limit panel: per-model count within the configured window.
		if rl != nil {
			if rlLimit, window, applies := rl.Resolve(apiKey, modelName); applies && rlLimit > 0 && window > 0 {
				cutoffMs := now.UnixMilli() - window.Milliseconds()
				used := 0
				earliestMs := int64(0)
				for _, d := range modelData.Details {
					tsMs := parseTimestampMs(d.Timestamp)
					if tsMs < cutoffMs {
						continue
					}
					used++
					if earliestMs == 0 || tsMs < earliestMs {
						earliestMs = tsMs
					}
				}
				if used > 0 {
					resp.RateLimits = append(resp.RateLimits, KeyRateLimitWindow{
						Model:    modelName,
						Window:   window.String(),
						WindowMs: window.Milliseconds(),
						Limit:    rlLimit,
						Used:     used,
						ResetsAt: earliestMs + window.Milliseconds(),
					})
				}
			}
		}
	}

	// Sort models by TotalRequests desc, tie-break by name asc.
	sort.Slice(resp.Models, func(i, j int) bool {
		if resp.Models[i].TotalRequests != resp.Models[j].TotalRequests {
			return resp.Models[i].TotalRequests > resp.Models[j].TotalRequests
		}
		return resp.Models[i].Model < resp.Models[j].Model
	})

	// Sort candidates by timestamp desc and cap at `limit`.
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].tsMs > candidates[j].tsMs })
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	resp.RecentDetails = make([]RecentDetailEntry, len(candidates))
	for i, c := range candidates {
		resp.RecentDetails[i] = c.entry
	}

	sort.Slice(resp.RateLimits, func(i, j int) bool { return resp.RateLimits[i].Model < resp.RateLimits[j].Model })

	return resp
}

func (s *Store) Export() *ExportPayload {
	snap := s.Snapshot()
	return &ExportPayload{
		Version:    1,
		ExportedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Usage:      snap,
	}
}

// flatRecord is a single usage-queue record (new format).
type flatRecord struct {
	Timestamp string     `json:"timestamp"`
	LatencyMs int64      `json:"latency_ms"`
	Source    string     `json:"source"`
	AuthIndex string     `json:"auth_index"`
	Tokens    TokenStats `json:"tokens"`
	Failed    bool       `json:"failed"`
	Provider  string     `json:"provider"`
	Model     string     `json:"model"`
	APIKey    string     `json:"api_key"`
}

func (s *Store) Import(payload []byte) (added int, err error) {
	// Detect format: array = flat records, object = old export format.
	payload = trimBOM(payload)
	if len(payload) == 0 {
		return 0, nil
	}

	switch payload[0] {
	case '[':
		return s.importFlatRecords(payload)
	case '{':
		return s.importOldFormat(payload)
	default:
		return 0, json.Unmarshal(payload, &struct{}{})
	}
}

func (s *Store) importFlatRecords(payload []byte) (int, error) {
	var records []flatRecord
	if err := json.Unmarshal(payload, &records); err != nil {
		return 0, err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, rec := range records {
		apiKey := rec.APIKey
		if apiKey == "" {
			apiKey = "unknown"
		}
		model := rec.Model
		if model == "" {
			model = "unknown"
		}
		totalTokens := rec.Tokens.TotalTokens
		if totalTokens == 0 {
			totalTokens = rec.Tokens.InputTokens + rec.Tokens.OutputTokens + rec.Tokens.ReasoningTokens
		}

		apiData := s.data.APIs[apiKey]
		if apiData == nil {
			apiData = &APIData{Models: make(map[string]*ModelData)}
			s.data.APIs[apiKey] = apiData
		}
		modelData := apiData.Models[model]
		if modelData == nil {
			modelData = &ModelData{}
			apiData.Models[model] = modelData
		}

		detail := Detail{
			Timestamp: rec.Timestamp,
			LatencyMs: rec.LatencyMs,
			Source:    rec.Source,
			AuthIndex: rec.AuthIndex,
			Tokens:    TokenStats{
				InputTokens:     rec.Tokens.InputTokens,
				OutputTokens:    rec.Tokens.OutputTokens,
				ReasoningTokens: rec.Tokens.ReasoningTokens,
				CachedTokens:    rec.Tokens.CachedTokens,
				TotalTokens:     totalTokens,
			},
			Failed: rec.Failed,
		}
		modelData.Details = append(modelData.Details, detail)
		modelData.TotalRequests++
		modelData.TotalTokens += totalTokens
		apiData.TotalRequests++
		apiData.TotalTokens += totalTokens
	}

	s.recalcTotalsLocked()
	return len(records), nil
}

func (s *Store) importOldFormat(payload []byte) (int, error) {
	var export ExportPayload
	if err := json.Unmarshal(payload, &export); err != nil {
		return 0, err
	}
	if export.Usage == nil || export.Usage.APIs == nil {
		return 0, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	added := 0
	for apiName, apiData := range export.Usage.APIs {
		if apiData == nil {
			continue
		}
		existing := s.data.APIs[apiName]
		if existing == nil {
			existing = &APIData{Models: make(map[string]*ModelData)}
			s.data.APIs[apiName] = existing
		}

		for modelName, modelData := range apiData.Models {
			if modelData == nil {
				continue
			}
			existingModel := existing.Models[modelName]
			if existingModel == nil {
				existingModel = &ModelData{}
				existing.Models[modelName] = existingModel
			}

			existingModel.Details = append(existingModel.Details, modelData.Details...)
			existingModel.TotalRequests += int64(len(modelData.Details))
			for _, d := range modelData.Details {
				existingModel.TotalTokens += d.Tokens.TotalTokens
			}
			existing.TotalRequests += int64(len(modelData.Details))
			for _, d := range modelData.Details {
				existing.TotalTokens += d.Tokens.TotalTokens
			}
			added += len(modelData.Details)
		}
	}

	s.recalcTotalsLocked()
	return added, nil
}

// recalcTotalsLocked rebuilds every running counter from the raw Details slices.
// It is the source of truth after Import: callers may append details without
// touching the aggregates (e.g. the old export format never had them) and rely
// on this pass to make the store consistent.
func (s *Store) recalcTotalsLocked() {
	s.data.TotalRequests = 0
	s.data.SuccessCount = 0
	s.data.FailureCount = 0
	s.data.TotalTokens = 0
	for _, apiData := range s.data.APIs {
		apiData.TotalRequests = 0
		apiData.TotalTokens = 0
		apiData.SuccessCount = 0
		apiData.FailureCount = 0
		apiData.InputTokens = 0
		apiData.OutputTokens = 0
		apiData.CachedTokens = 0
		apiData.ReasoningTokens = 0
		apiData.LastActiveMs = 0
		for _, modelData := range apiData.Models {
			modelData.TotalRequests = 0
			modelData.TotalTokens = 0
			modelData.SuccessCount = 0
			modelData.FailureCount = 0
			modelData.InputTokens = 0
			modelData.OutputTokens = 0
			modelData.CachedTokens = 0
			modelData.ReasoningTokens = 0
			modelData.LastActiveMs = 0
			for _, d := range modelData.Details {
				modelData.TotalRequests++
				modelData.TotalTokens += d.Tokens.TotalTokens
				modelData.InputTokens += d.Tokens.InputTokens
				modelData.OutputTokens += d.Tokens.OutputTokens
				modelData.CachedTokens += d.Tokens.CachedTokens
				modelData.ReasoningTokens += d.Tokens.ReasoningTokens
				if d.Failed {
					modelData.FailureCount++
				} else {
					modelData.SuccessCount++
				}
				if tsMs := parseTimestampMs(d.Timestamp); tsMs > modelData.LastActiveMs {
					modelData.LastActiveMs = tsMs
				}
			}
			apiData.TotalRequests += modelData.TotalRequests
			apiData.TotalTokens += modelData.TotalTokens
			apiData.SuccessCount += modelData.SuccessCount
			apiData.FailureCount += modelData.FailureCount
			apiData.InputTokens += modelData.InputTokens
			apiData.OutputTokens += modelData.OutputTokens
			apiData.CachedTokens += modelData.CachedTokens
			apiData.ReasoningTokens += modelData.ReasoningTokens
			if modelData.LastActiveMs > apiData.LastActiveMs {
				apiData.LastActiveMs = modelData.LastActiveMs
			}
		}
		s.data.TotalRequests += apiData.TotalRequests
		s.data.TotalTokens += apiData.TotalTokens
		s.data.SuccessCount += apiData.SuccessCount
		s.data.FailureCount += apiData.FailureCount
	}
}

func parseTimestampMs(ts string) int64 {
	if ts == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

func trimBOM(b []byte) []byte {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		return b[3:]
	}
	return b
}

func (s *Store) RegisterPlugin() {
	usage.RegisterPlugin(s)
}
