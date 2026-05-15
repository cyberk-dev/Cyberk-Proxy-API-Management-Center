// promptlog-dedupe collapses agent-loop duplicate entries left in
// prompts-YYYY-MM-DD.jsonl files by the pre-fix extractor (see
// internal/promptlog/extract.go), where OpenAI Chat / Responses agent loops
// re-logged the same user prompt on every tool roundtrip.
//
// Dedup rule: within each (key_hash, session_id) bucket sorted by ts, drop
// any entry whose (provider, prompt) equals the most recently kept entry
// AND whose time gap is ≤ --max-gap. Empty session_id is never deduped —
// without a session header, two identical lines could be unrelated requests.
//
// Files are rewritten atomically (tmp + rename); a .bak.<unix> copy is kept
// by default. Use --dry-run first to preview the cull.
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"
)

type entryMeta struct {
	ts       time.Time
	keyHash  string
	session  string
	provider string
	prompt   string
}

func main() {
	dir := flag.String("dir", "", "directory containing prompts-YYYY-MM-DD.jsonl files (required)")
	dryRun := flag.Bool("dry-run", false, "preview the cull without modifying files")
	maxGap := flag.Duration("max-gap", time.Hour, "treat consecutive identical prompts within this window as duplicates (0 = no time limit)")
	keepBackup := flag.Bool("backup", true, "rename each modified file to .bak.<unix> before rewriting")
	verbose := flag.Bool("verbose", false, "print per-entry decisions (chatty; use with --dry-run on small samples)")
	ignoreSession := flag.Bool("ignore-session", false, "bucket by key_hash only (covers clients like opencode that omit session headers — RISKY: relies on --max-gap to keep unrelated activity apart)")
	flag.Parse()

	if *dir == "" {
		fmt.Fprintln(os.Stderr, "usage: promptlog-dedupe --dir <prompts-dir> [--dry-run] [--max-gap 1h] [--backup=false]")
		os.Exit(2)
	}

	files, err := filepath.Glob(filepath.Join(*dir, "prompts-*.jsonl"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "glob: %v\n", err)
		os.Exit(1)
	}
	sort.Strings(files)
	if len(files) == 0 {
		fmt.Println("no prompts-*.jsonl files found")
		return
	}

	mode := "DRY-RUN"
	if !*dryRun {
		mode = "REWRITE"
	}
	fmt.Printf("mode=%s max-gap=%s backup=%v ignore-session=%v files=%d\n\n", mode, *maxGap, *keepBackup, *ignoreSession, len(files))

	var totalKept, totalDropped int
	for _, path := range files {
		kept, dropped, err := dedupeFile(path, *maxGap, *dryRun, *keepBackup, *verbose, *ignoreSession)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			continue
		}
		totalKept += kept
		totalDropped += dropped
		verb := "would drop"
		if !*dryRun {
			verb = "dropped"
		}
		fmt.Printf("%s: kept %d, %s %d\n", filepath.Base(path), kept, verb, dropped)
	}
	fmt.Printf("\ntotal kept: %d\ntotal dropped: %d\n", totalKept, totalDropped)
}

func dedupeFile(path string, maxGap time.Duration, dryRun, keepBackup, verbose, ignoreSession bool) (kept, dropped int, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0, err
	}
	rawLines, metas, err := scanEntries(f)
	f.Close()
	if err != nil {
		return 0, 0, err
	}

	drop := computeDrops(metas, maxGap, verbose, filepath.Base(path), ignoreSession)

	droppedCount := 0
	for _, d := range drop {
		if d {
			droppedCount++
		}
	}
	if droppedCount == 0 || dryRun {
		return len(rawLines) - droppedCount, droppedCount, nil
	}

	if keepBackup {
		bak := fmt.Sprintf("%s.bak.%d", path, time.Now().Unix())
		if err := copyFile(path, bak); err != nil {
			return 0, 0, fmt.Errorf("backup: %w", err)
		}
	}

	tmp := path + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, 0, err
	}
	bw := bufio.NewWriter(out)
	for i, line := range rawLines {
		if drop[i] {
			continue
		}
		if _, err := bw.Write(line); err != nil {
			out.Close()
			os.Remove(tmp)
			return 0, 0, err
		}
		if err := bw.WriteByte('\n'); err != nil {
			out.Close()
			os.Remove(tmp)
			return 0, 0, err
		}
	}
	if err := bw.Flush(); err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, 0, err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(tmp)
		return 0, 0, err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return 0, 0, err
	}
	if err := os.Rename(tmp, path); err != nil {
		return 0, 0, err
	}
	return len(rawLines) - droppedCount, droppedCount, nil
}

// scanEntries reads JSONL, returning the raw bytes of each non-empty line
// (without trailing newline) plus parsed metadata used for dedup. Malformed
// lines preserve their raw bytes and get a zero meta — they are NEVER
// dropped, since we cannot prove they are duplicates.
func scanEntries(r io.Reader) ([][]byte, []entryMeta, error) {
	sc := bufio.NewScanner(r)
	// Match the writer's 16 MiB token cap so large entries do not abort the
	// scan with bufio.ErrTooLong.
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	var lines [][]byte
	var metas []entryMeta
	for sc.Scan() {
		b := sc.Bytes()
		if len(b) == 0 {
			continue
		}
		cp := make([]byte, len(b))
		copy(cp, b)
		lines = append(lines, cp)

		var e struct {
			Timestamp time.Time `json:"ts"`
			KeyHash   string    `json:"key_hash"`
			SessionID string    `json:"session_id"`
			Provider  string    `json:"provider"`
			Prompt    string    `json:"prompt"`
		}
		var m entryMeta
		if err := json.Unmarshal(cp, &e); err == nil {
			m = entryMeta{
				ts:       e.Timestamp,
				keyHash:  e.KeyHash,
				session:  e.SessionID,
				provider: e.Provider,
				prompt:   e.Prompt,
			}
		}
		metas = append(metas, m)
	}
	if err := sc.Err(); err != nil {
		return nil, nil, err
	}
	return lines, metas, nil
}

// computeDrops returns a slice parallel to metas marking which lines to drop.
// Bucketing by (key_hash, session_id) and sorting by ts means a single linear
// scan suffices: each entry is compared only to the most recently kept entry
// in its bucket. When ignoreSession is true, bucketing collapses by key_hash
// alone — needed for clients (opencode, raw curl) that omit session headers.
func computeDrops(metas []entryMeta, maxGap time.Duration, verbose bool, fileLabel string, ignoreSession bool) []bool {
	drop := make([]bool, len(metas))
	type bucketKey struct{ keyHash, session string }
	buckets := map[bucketKey][]int{}
	for i, m := range metas {
		if m.keyHash == "" || m.prompt == "" {
			continue
		}
		if !ignoreSession && m.session == "" {
			continue
		}
		k := bucketKey{m.keyHash, ""}
		if !ignoreSession {
			k.session = m.session
		}
		buckets[k] = append(buckets[k], i)
	}
	for bk, idxs := range buckets {
		sort.Slice(idxs, func(a, b int) bool {
			return metas[idxs[a]].ts.Before(metas[idxs[b]].ts)
		})
		var lastKept *entryMeta
		for _, i := range idxs {
			cur := &metas[i]
			if lastKept != nil &&
				lastKept.provider == cur.provider &&
				lastKept.prompt == cur.prompt {
				gap := cur.ts.Sub(lastKept.ts)
				if maxGap == 0 || gap <= maxGap {
					drop[i] = true
					if verbose {
						fmt.Printf("  drop %s key=%s session=%s gap=%s prompt=%q\n",
							fileLabel, bk.keyHash, shorten(bk.session), gap, snippet(cur.prompt))
					}
					continue
				}
			}
			lastKept = cur
		}
	}
	return drop
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

func shorten(s string) string {
	if len(s) <= 12 {
		return s
	}
	return s[:12]
}

func snippet(s string) string {
	const max = 80
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
