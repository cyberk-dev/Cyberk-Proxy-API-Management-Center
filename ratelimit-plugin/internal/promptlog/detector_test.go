package promptlog

import (
	"strings"
	"testing"
	"time"
)

// observeMany pumps n copies of prompt through the detector at one-minute
// intervals so the time-based scan condition can fire when expected.
func observeMany(d *templateDetector, prompt string, n int) {
	now := time.Unix(0, 0)
	for i := 0; i < n; i++ {
		d.observe(prompt, now.Add(time.Duration(i)*time.Minute))
	}
}

func newTestDetector(t *testing.T, minLen, minOccur, window int) (*templateDetector, *TemplateStore) {
	t.Helper()
	store, err := NewTemplateStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := TemplatesConfig{Enabled: true, MinLen: minLen, MinOccur: minOccur, Window: window, ScanEvery: 1}
	return newTemplateDetector(store, cfg), store
}

func TestDetector_RegistersRepeatedPrefix(t *testing.T) {
	d, store := newTestDetector(t, 50, 3, 100)
	prefix := strings.Repeat("X", 60) // 60 runes — above MinLen=50
	// Each prompt diverges immediately after `prefix` so the LONGEST common
	// prefix shared by all 5 is exactly len(prefix) runes.
	for i := 0; i < 5; i++ {
		d.observe(prefix+string(rune('A'+i))+"-tail", time.Unix(int64(i), 0))
	}
	d.scanLocked(time.Unix(99, 0))

	tpls := store.List()
	if len(tpls) != 1 {
		t.Fatalf("expected exactly one registered template, got %d: %+v", len(tpls), tpls)
	}
	if tpls[0].Text != prefix {
		t.Errorf("text got %q want %q", tpls[0].Text, prefix)
	}
	if tpls[0].Source != "detector" {
		t.Errorf("source got %q want %q", tpls[0].Source, "detector")
	}
}

func TestDetector_BelowThresholdNotRegistered(t *testing.T) {
	d, store := newTestDetector(t, 50, 3, 100)
	prefix := strings.Repeat("Y", 60)
	// Only 2 occurrences — below MinOccur=3.
	for i := 0; i < 2; i++ {
		d.observe(prefix+"-suffix"+string(rune('a'+i)), time.Unix(int64(i), 0))
	}
	d.scanLocked(time.Unix(99, 0))

	if got := len(store.List()); got != 0 {
		t.Errorf("expected no templates below MinOccur, got %d", got)
	}
}

func TestDetector_TooShortNotRegistered(t *testing.T) {
	d, store := newTestDetector(t, 200, 3, 100)
	prefix := strings.Repeat("Z", 50) // way under MinLen=200
	for i := 0; i < 10; i++ {
		d.observe(prefix+"x", time.Unix(int64(i), 0))
	}
	d.scanLocked(time.Unix(99, 0))
	if got := len(store.List()); got != 0 {
		t.Errorf("expected no templates below MinLen, got %d", got)
	}
}

func TestDetector_PicksLongestCommonPrefix(t *testing.T) {
	// Five prompts share 100 chars, of which two ALSO share an additional
	// 50 chars before diverging. The deeper, more-specific 150-char prefix
	// only has 2 hits — below MinOccur, so detector must register the 100
	// char prefix shared by all 5.
	d, store := newTestDetector(t, 50, 3, 100)
	common := strings.Repeat("A", 100)
	deeper := common + strings.Repeat("B", 50)
	d.observe(deeper+"-x", time.Unix(1, 0))
	d.observe(deeper+"-y", time.Unix(2, 0))
	for i := 0; i < 3; i++ {
		d.observe(common+strings.Repeat("C", i+1)+"-z", time.Unix(int64(10+i), 0))
	}
	d.scanLocked(time.Unix(99, 0))

	var got string
	for _, tpl := range store.List() {
		if tpl.Text == common {
			got = tpl.Text
			break
		}
	}
	if got != common {
		t.Errorf("expected 100-char common prefix registered, got templates: %+v", store.List())
	}
}

func TestDetector_EvictsOldEntries(t *testing.T) {
	// Window=4. Push the same prompt 3 times (would qualify), then push 4
	// distinct prompts — old occurrences evict, leaving 0 hits for the
	// original; scan should not register it.
	d, store := newTestDetector(t, 50, 3, 4)
	tpl := strings.Repeat("E", 80)
	for i := 0; i < 3; i++ {
		d.observe(tpl+"-"+string(rune('a'+i)), time.Unix(int64(i), 0))
	}
	for i := 0; i < 4; i++ {
		d.observe(strings.Repeat("Q", 80)+"-"+string(rune('m'+i)), time.Unix(int64(10+i), 0))
	}
	d.scanLocked(time.Unix(99, 0))

	for _, t2 := range store.List() {
		if t2.Text == tpl {
			t.Errorf("evicted prefix should not be registered, but got %q", t2.Text)
		}
	}
}

func TestDetector_DoesNotReRegisterExisting(t *testing.T) {
	d, store := newTestDetector(t, 50, 3, 100)
	prefix := strings.Repeat("R", 60)
	for i := 0; i < 5; i++ {
		d.observe(prefix+"-"+string(rune('a'+i)), time.Unix(int64(i), 0))
	}
	d.scanLocked(time.Unix(99, 0))
	count1 := len(store.List())

	// Run again with more observations of the same prefix.
	for i := 5; i < 10; i++ {
		d.observe(prefix+"-"+string(rune('a'+i)), time.Unix(int64(i), 0))
	}
	d.scanLocked(time.Unix(199, 0))
	count2 := len(store.List())

	if count1 != count2 {
		t.Errorf("expected stable template count after re-scan, got %d → %d", count1, count2)
	}
}

func TestDetector_NilDetectorIsSafe(t *testing.T) {
	var d *templateDetector
	d.observe("anything", time.Now())
}

func TestDetector_HonorsMaxRunesObserved(t *testing.T) {
	// MinLen=50 → maxRunesObserved=200. A common prefix of 250 chars must
	// be capped to 200 in the trie, so the registered template length is at
	// most 200, never 250.
	d, store := newTestDetector(t, 50, 3, 100)
	prefix := strings.Repeat("M", 250)
	for i := 0; i < 5; i++ {
		d.observe(prefix+"-"+string(rune('a'+i)), time.Unix(int64(i), 0))
	}
	d.scanLocked(time.Unix(99, 0))
	for _, tpl := range store.List() {
		if tpl.Length > 200 {
			t.Errorf("template length %d exceeds detector cap 200", tpl.Length)
		}
	}
}
