package ratelimit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type counterKey struct {
	apiKey string
	model  string
}

type Limiter struct {
	mu    sync.Mutex
	hits  map[counterKey][]time.Time
	dirty atomic.Bool
	now   func() time.Time
}

func NewLimiter() *Limiter {
	return &Limiter{
		hits: map[counterKey][]time.Time{},
		now:  time.Now,
	}
}

func (l *Limiter) Take(apiKey, model string, limit int, window time.Duration) (allowed bool, remaining int, resetAt time.Time) {
	if limit <= 0 || window <= 0 {
		return true, 0, time.Time{}
	}
	now := l.now()
	k := counterKey{apiKey: apiKey, model: model}

	l.mu.Lock()
	defer l.mu.Unlock()

	list := l.hits[k]

	if n := len(list); n > 0 && now.Before(list[n-1]) {
		now = list[n-1].Add(time.Nanosecond)
	}
	cutoff := now.Add(-window)

	i := sort.Search(len(list), func(i int) bool { return list[i].After(cutoff) })
	if i > 0 {
		list = list[i:]
	}

	if len(list) >= limit {
		if len(list) > limit {
			list = list[len(list)-limit:]
		}
		l.hits[k] = list
		return false, 0, list[0].Add(window)
	}

	list = append(list, now)
	l.hits[k] = list
	l.dirty.Store(true)
	return true, limit - len(list), list[0].Add(window)
}

func (l *Limiter) TakeDirty() bool {
	return l.dirty.CompareAndSwap(true, false)
}

func (l *Limiter) PruneIdle() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for k, v := range l.hits {
		if len(v) == 0 {
			delete(l.hits, k)
		}
	}
}

type snapshotFile struct {
	Version int                `json:"version"`
	SavedAt time.Time          `json:"saved_at"`
	Hits    map[string][]int64 `json:"hits"`
}

const snapshotVersion = 1

func (l *Limiter) Save(path string) error {
	snap := snapshotFile{
		Version: snapshotVersion,
		SavedAt: l.now(),
		Hits:    map[string][]int64{},
	}

	l.mu.Lock()
	for k, ts := range l.hits {
		if len(ts) == 0 {
			continue
		}
		key := k.apiKey + "\x00" + k.model
		arr := make([]int64, len(ts))
		for i, t := range ts {
			arr[i] = t.UnixNano()
		}
		snap.Hits[key] = arr
	}
	l.mu.Unlock()

	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".ratelimit-state-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

func (l *Limiter) Load(path string, maxWindow time.Duration) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if len(data) == 0 {
		return nil
	}

	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err != nil {
		return err
	}

	now := l.now()
	cutoff := now.Add(-maxWindow)

	l.mu.Lock()
	defer l.mu.Unlock()

	for keyStr, arr := range snap.Hits {
		parts := strings.SplitN(keyStr, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		ck := counterKey{apiKey: parts[0], model: parts[1]}
		fresh := make([]time.Time, 0, len(arr))
		for _, ns := range arr {
			t := time.Unix(0, ns)
			if t.After(cutoff) && !t.After(now) {
				fresh = append(fresh, t)
			}
		}
		if len(fresh) > 0 {
			sort.Slice(fresh, func(i, j int) bool { return fresh[i].Before(fresh[j]) })
			l.hits[ck] = fresh
		}
	}
	return nil
}

func (l *Limiter) Size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.hits)
}
