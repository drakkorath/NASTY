package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

var (
	db             *bbolt.DB
	pkgBucket      = []byte("nix_packages")
	settingsBucket = []byte("settings")

	// indexVersion increments every time the cached index is replaced.
	indexVersion atomic.Int64

	// IndexReady receives a signal after each successful sync so the UI can redraw.
	IndexReady = make(chan struct{}, 1)

	syncRunning atomic.Bool
)

// PkgResult is a plain search result. Checkbox state lives in the UI.
type PkgResult struct {
	Name        string // attribute path, usable directly in `with pkgs; [ ... ]`
	Description string
}

type pkgEntry struct {
	Attr string
	Desc string
}

// initDB opens the bbolt cache and starts a background index sync if it is empty.
func initDB() error {
	cacheDir, err := os.UserCacheDir()
	if err != nil {
		cacheDir = "/tmp"
	}
	dbPath := filepath.Join(cacheDir, "nasty", "packages.db")

	if err := os.MkdirAll(filepath.Dir(dbPath), 0755); err != nil {
		return fmt.Errorf("failed to create cache directory: %w", err)
	}

	db, err = bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		if errors.Is(err, bbolt.ErrTimeout) {
			return errors.New("NASTY is already running (its package cache is locked by another instance)")
		}
		return fmt.Errorf("could not open bbolt db: %w", err)
	}

	err = db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists(pkgBucket); err != nil {
			return err
		}
		_, err := tx.CreateBucketIfNotExists(settingsBucket)
		return err
	})
	if err != nil {
		return fmt.Errorf("failed to create buckets: %w", err)
	}

	go SyncNixIndex(false, log.Printf)
	return nil
}

// closeDB flushes and closes the database.
func closeDB() error {
	if db != nil {
		return db.Close()
	}
	return nil
}

// ---- settings ---------------------------------------------------------------

func getSetting(key string) string {
	if db == nil {
		return ""
	}
	var v string
	_ = db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(settingsBucket); b != nil {
			v = string(b.Get([]byte(key)))
		}
		return nil
	})
	return v
}

func setSetting(key, value string) {
	if db == nil {
		return
	}
	_ = db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists(settingsBucket)
		if err != nil {
			return err
		}
		return b.Put([]byte(key), []byte(value))
	})
}

// ---- index sync -------------------------------------------------------------

func packageCount() int {
	count := 0
	_ = db.View(func(tx *bbolt.Tx) error {
		if b := tx.Bucket(pkgBucket); b != nil {
			count = b.Stats().KeyN
		}
		return nil
	})
	return count
}

// indexCommands are tried in order. nix-env works on a stock channel-based
// NixOS; `nix search` needs the experimental features enabled explicitly.
var indexCommands = [][]string{
	{"nix-env", "-qaP", "--json", "-f", "<nixpkgs>"},
	{"nix", "--extra-experimental-features", "nix-command flakes", "search", "nixpkgs", "^", "--json"},
}

// SyncNixIndex rebuilds the package cache. When force is false it is skipped if
// the cache already looks populated. Only one sync runs at a time.
func SyncNixIndex(force bool, logf func(string, ...any)) {
	if db == nil {
		return
	}
	if logf == nil {
		logf = log.Printf
	}
	if !syncRunning.CompareAndSwap(false, true) {
		return
	}
	defer syncRunning.Store(false)

	if !force {
		if n := packageCount(); n > 1000 {
			logf("Package cache holds %d entries; skipping auto-sync.", n)
			return
		}
	}

	logf("Building package index (this can take a few minutes)...")

	var entries []pkgEntry
	var lastErr error
	for _, args := range indexCommands {
		entries, lastErr = runIndexCommand(args)
		if lastErr == nil && len(entries) > 0 {
			break
		}
		if lastErr != nil {
			logf("Index command %q failed: %v", args[0], lastErr)
		}
	}
	if len(entries) == 0 {
		logf("Package index not updated (no usable data from nix-env or nix search).")
		return
	}

	sort.Slice(entries, func(i, j int) bool { return entries[i].Attr < entries[j].Attr })

	err := db.Update(func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(pkgBucket); err != nil && err != bbolt.ErrBucketNotFound {
			return err
		}
		b, err := tx.CreateBucket(pkgBucket)
		if err != nil {
			return err
		}
		b.FillPercent = 0.9 // keys are inserted in sorted order
		for _, e := range entries {
			if err := b.Put([]byte(e.Attr), []byte(e.Desc)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		logf("Error writing package index: %v", err)
		return
	}

	indexVersion.Add(1)
	select {
	case IndexReady <- struct{}{}:
	default:
	}
	logf("Package index ready: %d packages.", len(entries))
}

func runIndexCommand(args []string) ([]pkgEntry, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}

	entries, parseErr := parseIndex(stdout)
	if parseErr != nil {
		cancel() // stop the process; its output is unusable
	}
	io.Copy(io.Discard, stdout) // drain so Wait can't block on a full pipe
	waitErr := cmd.Wait()

	if parseErr != nil {
		return nil, parseErr
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if len(msg) > 300 {
			msg = msg[len(msg)-300:]
		}
		return nil, fmt.Errorf("%w: %s", waitErr, msg)
	}
	return entries, nil
}

// parseIndex streams a top-level JSON object of {attr: {...}} without holding
// the whole map in memory. Handles both nix-env and nix search output.
func parseIndex(r io.Reader) ([]pkgEntry, error) {
	dec := json.NewDecoder(r)

	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return nil, fmt.Errorf("unexpected JSON: expected an object")
	}

	type rawPkg struct {
		Description any `json:"description"` // nix search
		Meta        struct {
			Description any `json:"description"` // nix-env
		} `json:"meta"`
	}

	var out []pkgEntry
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil, fmt.Errorf("unexpected JSON key")
		}

		var p rawPkg
		if err := dec.Decode(&p); err != nil {
			return nil, err
		}

		attr := normalizeAttr(key)
		if !validPkgRegex.MatchString(attr) {
			continue // ApplyChanges would reject it anyway
		}

		desc := asString(p.Description)
		if desc == "" {
			desc = asString(p.Meta.Description)
		}
		out = append(out, pkgEntry{Attr: attr, Desc: strings.TrimSpace(desc)})
	}
	return out, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// normalizeAttr turns `legacyPackages.x86_64-linux.htop` into `htop`.
func normalizeAttr(key string) string {
	if strings.HasPrefix(key, "legacyPackages.") {
		if parts := strings.SplitN(key, ".", 3); len(parts) == 3 {
			return parts[2]
		}
	}
	return key
}

// ---- search -----------------------------------------------------------------

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func isBoundary(c byte) bool { return c == '.' || c == '-' || c == '_' }

func isSubsequence(q, s string) bool {
	i := 0
	for j := 0; j < len(s) && i < len(q); j++ {
		if s[j] == q[i] {
			i++
		}
	}
	return i == len(q)
}

// scoreMatch ranks a lowercase query against a lowercase attribute name.
// 0 means no match; higher is better.
func scoreMatch(q, name string) int {
	switch {
	case name == q:
		return 1000
	case strings.HasSuffix(name, "."+q):
		return 900 // e.g. python3Packages.requests for "requests"
	case strings.HasPrefix(name, q):
		return 800 - minInt(len(name)-len(q), 100)
	}
	if i := strings.Index(name, q); i >= 0 {
		score := 500 - minInt(i, 100)
		if i > 0 && isBoundary(name[i-1]) {
			score += 100
		}
		return score
	}
	if len(q) >= 3 && isSubsequence(q, name) {
		return 100
	}
	return 0
}

// SearchPackages ranks attribute names (exact, base-name, prefix, substring,
// fuzzy) and falls back to description matches for queries of 3+ characters.
// An empty query returns the first `limit` packages alphabetically.
func SearchPackages(query string, limit int) []PkgResult {
	if db == nil || limit <= 0 {
		return nil
	}
	q := strings.ToLower(strings.TrimSpace(query))

	type scored struct {
		r     PkgResult
		score int
	}
	var hits []scored

	_ = db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket(pkgBucket)
		if b == nil {
			return nil
		}
		c := b.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			name := string(k) // copy: k is only valid inside the transaction
			if q == "" {
				hits = append(hits, scored{PkgResult{name, string(v)}, 0})
				if len(hits) >= limit {
					return nil
				}
				continue
			}
			s := scoreMatch(q, strings.ToLower(name))
			if s == 0 && len(q) >= 3 && strings.Contains(strings.ToLower(string(v)), q) {
				s = 10
			}
			if s > 0 {
				hits = append(hits, scored{PkgResult{name, string(v)}, s})
			}
		}
		return nil
	})

	if q != "" {
		// Stable: ties stay in alphabetical order because keys iterate sorted.
		sort.SliceStable(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	}
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]PkgResult, len(hits))
	for i, h := range hits {
		out[i] = h.r
	}
	return out
}
