package usagestore

import (
	"context"
	"encoding/json"
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
	TotalRequests int64    `json:"total_requests"`
	TotalTokens   int64    `json:"total_tokens"`
	Details       []Detail `json:"details"`
}

type APIData struct {
	TotalRequests int64                `json:"total_requests"`
	TotalTokens   int64                `json:"total_tokens"`
	Models        map[string]*ModelData `json:"models"`
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

	apiData.TotalRequests++
	apiData.TotalTokens += totalTokens

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

func (s *Store) KeySnapshot(apiKey string) *UsageSnapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	apiData, ok := s.data.APIs[apiKey]
	if !ok {
		return nil
	}

	cpModels := make(map[string]*ModelData, len(apiData.Models))
	for modelName, modelData := range apiData.Models {
		details := make([]Detail, len(modelData.Details))
		copy(details, modelData.Details)
		cpModels[modelName] = &ModelData{
			TotalRequests: modelData.TotalRequests,
			TotalTokens:   modelData.TotalTokens,
			Details:       details,
		}
	}

	cpAPI := &APIData{
		TotalRequests: apiData.TotalRequests,
		TotalTokens:   apiData.TotalTokens,
		Models:        cpModels,
	}

	var successCount, failureCount int64
	for _, md := range cpModels {
		for _, d := range md.Details {
			if d.Failed {
				failureCount++
			} else {
				successCount++
			}
		}
	}

	return &UsageSnapshot{
		TotalRequests: apiData.TotalRequests,
		SuccessCount:  successCount,
		FailureCount:  failureCount,
		TotalTokens:   apiData.TotalTokens,
		APIs:          map[string]*APIData{apiKey: cpAPI},
	}
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

func (s *Store) recalcTotalsLocked() {
	s.data.TotalRequests = 0
	s.data.SuccessCount = 0
	s.data.FailureCount = 0
	s.data.TotalTokens = 0
	for _, apiData := range s.data.APIs {
		s.data.TotalRequests += apiData.TotalRequests
		s.data.TotalTokens += apiData.TotalTokens
		for _, modelData := range apiData.Models {
			for _, d := range modelData.Details {
				if d.Failed {
					s.data.FailureCount++
				} else {
					s.data.SuccessCount++
				}
			}
		}
	}
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
