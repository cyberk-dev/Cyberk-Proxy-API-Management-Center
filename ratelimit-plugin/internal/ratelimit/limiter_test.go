package ratelimit

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fakeClock(t time.Time) (func() time.Time, func(d time.Duration)) {
	var mu sync.Mutex
	cur := t
	get := func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return cur
	}
	adv := func(d time.Duration) {
		mu.Lock()
		defer mu.Unlock()
		cur = cur.Add(d)
	}
	return get, adv
}

func TestLimiter_SlidingWindow(t *testing.T) {
	clock, advance := fakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiter()
	l.now = clock

	for i := 0; i < 3; i++ {
		ok, _, _ := l.Take("k", "m", 3, time.Hour)
		if !ok {
			t.Fatalf("req %d should pass", i)
		}
	}

	ok, _, resetAt := l.Take("k", "m", 3, time.Hour)
	if ok {
		t.Fatal("4th should reject")
	}
	if resetAt.IsZero() {
		t.Error("resetAt should be set on reject")
	}

	advance(59 * time.Minute)
	ok, _, _ = l.Take("k", "m", 3, time.Hour)
	if ok {
		t.Fatal("still within 1h window, should reject")
	}

	advance(2 * time.Minute)
	ok, _, _ = l.Take("k", "m", 3, time.Hour)
	if !ok {
		t.Fatal("oldest expired, should pass")
	}
}

func TestLimiter_IsolatedByKeyAndModel(t *testing.T) {
	l := NewLimiter()
	l.Take("a", "m1", 1, time.Hour)

	if ok, _, _ := l.Take("b", "m1", 1, time.Hour); !ok {
		t.Error("key b shouldn't count against a")
	}
	if ok, _, _ := l.Take("a", "m2", 1, time.Hour); !ok {
		t.Error("model m2 shouldn't count against m1")
	}
}

func TestLimiter_Remaining(t *testing.T) {
	l := NewLimiter()
	_, remaining, _ := l.Take("k", "m", 10, time.Hour)
	if remaining != 9 {
		t.Errorf("got %d, want 9", remaining)
	}
	for i := 0; i < 5; i++ {
		l.Take("k", "m", 10, time.Hour)
	}
	_, remaining, _ = l.Take("k", "m", 10, time.Hour)
	if remaining != 3 {
		t.Errorf("after 7 takes, remaining got %d, want 3", remaining)
	}
}

func TestLimiter_ClockSkewBackward(t *testing.T) {
	clock, advance := fakeClock(time.Unix(1_700_000_000, 0))
	l := NewLimiter()
	l.now = clock

	l.Take("k", "m", 10, time.Hour)
	l.Take("k", "m", 10, time.Hour)

	advance(-2 * time.Hour)

	for i := 0; i < 5; i++ {
		ok, _, _ := l.Take("k", "m", 10, time.Hour)
		if !ok {
			t.Fatalf("iter %d should pass after skew clamp", i)
		}
	}

	l.mu.Lock()
	list := l.hits[counterKey{"k", "m"}]
	for i := 1; i < len(list); i++ {
		if list[i].Before(list[i-1]) {
			t.Errorf("list not monotonic at %d", i)
		}
	}
	l.mu.Unlock()
}

func TestLimiter_HeadTrimOnLimitReduction(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 20; i++ {
		l.Take("k", "m", 100, time.Hour)
	}
	for i := 0; i < 5; i++ {
		l.Take("k", "m", 3, time.Hour)
	}

	l.mu.Lock()
	n := len(l.hits[counterKey{"k", "m"}])
	l.mu.Unlock()
	if n > 3 {
		t.Errorf("head trim failed: list len %d, want <= 3", n)
	}
}

func TestLimiter_ZeroLimitAllowsAll(t *testing.T) {
	l := NewLimiter()
	ok, _, _ := l.Take("k", "m", 0, time.Hour)
	if !ok {
		t.Error("zero limit should allow")
	}
}

func TestLimiter_ConcurrentTake(t *testing.T) {
	l := NewLimiter()
	const goroutines = 50
	const perG = 20
	total := goroutines * perG
	limit := total / 2

	var allowed int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perG; j++ {
				if ok, _, _ := l.Take("k", "m", limit, time.Hour); ok {
					mu.Lock()
					allowed++
					mu.Unlock()
				}
			}
		}()
	}
	wg.Wait()

	if allowed != int64(limit) {
		t.Errorf("race-exposed counting bug: allowed=%d, want=%d", allowed, limit)
	}
}

func TestLimiter_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	l1 := NewLimiter()
	for i := 0; i < 3; i++ {
		l1.Take("k1", "m1", 10, time.Hour)
	}
	l1.Take("k2", "m2", 10, time.Hour)

	if err := l1.Save(path); err != nil {
		t.Fatal(err)
	}

	l2 := NewLimiter()
	if err := l2.Load(path, time.Hour); err != nil {
		t.Fatal(err)
	}

	_, remaining, _ := l2.Take("k1", "m1", 10, time.Hour)
	if remaining != 6 {
		t.Errorf("after 3 restored + 1 new, remaining got %d, want 6", remaining)
	}

	_, remaining, _ = l2.Take("k2", "m2", 10, time.Hour)
	if remaining != 8 {
		t.Errorf("k2: got %d, want 8", remaining)
	}
}

func TestLimiter_LoadDropsExpired(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	old := time.Now().Add(-10 * time.Hour)
	l1 := NewLimiter()
	l1.now = func() time.Time { return old }
	l1.Take("k", "m", 10, time.Hour)
	if err := l1.Save(path); err != nil {
		t.Fatal(err)
	}

	l2 := NewLimiter()
	if err := l2.Load(path, time.Hour); err != nil {
		t.Fatal(err)
	}

	_, remaining, _ := l2.Take("k", "m", 10, time.Hour)
	if remaining != 9 {
		t.Errorf("expired state should be dropped: remaining=%d", remaining)
	}
}

func TestLimiter_LoadMissingFile(t *testing.T) {
	l := NewLimiter()
	if err := l.Load("/nonexistent/path/state.json", time.Hour); err != nil {
		t.Errorf("missing file should not error: %v", err)
	}
}

func TestLimiter_TakeDirty(t *testing.T) {
	l := NewLimiter()
	if l.TakeDirty() {
		t.Error("fresh limiter should not be dirty")
	}
	l.Take("k", "m", 10, time.Hour)
	if !l.TakeDirty() {
		t.Error("after Take, should be dirty")
	}
	if l.TakeDirty() {
		t.Error("second TakeDirty should return false")
	}
}
