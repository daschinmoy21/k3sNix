// Package inventory tracks which store paths exist on this node.
// The have-set stays local. It is never uploaded anywhere.
package inventory

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"k3snix/internal/closure"
)

// lockSuffix marks the lock file nix holds beside a store path it is
// writing.
const lockSuffix = ".lock"

// Store is a cached listing of a Nix store directory.
type Store struct {
	Dir string
	TTL time.Duration

	// refreshMu serializes scans. Without it two overlapping Refreshes
	// publish in lock-acquisition order, so an older ReadDir can finish
	// last and overwrite a newer listing.
	refreshMu sync.Mutex

	mu      sync.Mutex
	have    map[string]struct{} // set of full paths present in Dir
	byHash  map[string]string   // 32-char store hash -> full path
	fetched time.Time
}

// New returns a Store listing dir. An empty dir defaults to /nix/store and
// an unset TTL defaults to 2s. TTL == 0 refreshes on every lookup.
func New(dir string) *Store {
	if dir == "" {
		dir = "/nix/store"
	}
	return &Store{Dir: dir, TTL: 2 * time.Second}
}

// Refresh rescans the store directory. Keys are full paths. Entries whose
// name starts with "." are skipped, and so are paths nix is still writing:
// nix holds "<path>.lock" while it builds or substitutes <path> and deletes
// it once the path is registered, so a path with a lock beside it may be
// half-written. The lock files themselves are never paths, but a read-only
// store path named "*.lock" is (see isLock). A lock left by a
// crashed nix keeps its path out of the have-set, which over-reports missing
// bytes rather than claiming bytes the node may not hold. Scans run one at a
// time.
func (s *Store) Refresh() error {
	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()
	return s.refresh()
}

// isLock reports whether e is a lock file nix holds rather than a store path
// whose name happens to end in ".lock", such as a fetched Cargo.lock. Nix
// creates its locks writable by their owner; a registered store path has
// been made read-only.
func isLock(e os.DirEntry) bool {
	if !strings.HasSuffix(e.Name(), lockSuffix) || !e.Type().IsRegular() {
		return false
	}
	info, err := e.Info()
	if err != nil {
		// Removed since ReadDir: nix just released it. Treat it as held
		// for this scan; the next one sees the path unlocked.
		return true
	}
	return info.Mode().Perm()&0o200 != 0
}

func (s *Store) refresh() error {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	locks := map[string]struct{}{}  // lock file names
	locked := map[string]struct{}{} // names of the paths they lock
	for _, e := range ents {
		if isLock(e) {
			locks[e.Name()] = struct{}{}
			locked[strings.TrimSuffix(e.Name(), lockSuffix)] = struct{}{}
		}
	}
	have := make(map[string]struct{}, len(ents))
	byHash := make(map[string]string, len(ents))
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if _, ok := locks[name]; ok {
			continue
		}
		if _, ok := locked[name]; ok {
			continue
		}
		path := filepath.Join(s.Dir, name)
		have[path] = struct{}{}
		if len(name) >= 32 {
			if _, dup := byHash[name[:32]]; !dup {
				byHash[name[:32]] = path
			}
		}
	}
	s.mu.Lock()
	s.have = have
	s.byHash = byHash
	s.fetched = time.Now()
	s.mu.Unlock()
	return nil
}

// stale reports whether the listing needs a rescan.
func (s *Store) stale() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.TTL == 0 || s.have == nil || time.Since(s.fetched) > s.TTL
}

// snapshot returns the have-set, refreshing it first when stale. A failed
// refresh keeps the previous listing. Refreshes are serialized and the
// staleness is re-checked after acquiring the lock, so a caller that
// queued behind another scan does not repeat it.
func (s *Store) snapshot() map[string]struct{} {
	if s.stale() {
		s.refreshMu.Lock()
		if s.stale() {
			_ = s.refresh()
		}
		s.refreshMu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.have
}

func (s *Store) alias(path string) string {
	return filepath.Join(s.Dir, filepath.Base(path))
}

// Missing computes missing NAR bytes for c using each path's NarSize from
// the closure. A path counts as present when the full path matches the
// listing or when the basename exists under the store directory.
func (s *Store) Missing(c closure.Closure) closure.Missing {
	have := s.snapshot()
	mapped := make(map[string]struct{}, len(c.Paths))
	for _, p := range c.Paths {
		if _, ok := have[p.Path]; ok {
			mapped[p.Path] = struct{}{}
			continue
		}
		if _, ok := have[s.alias(p.Path)]; ok {
			mapped[p.Path] = struct{}{}
		}
	}
	return closure.MissingAgainst(c, mapped)
}

// LookupHash returns the full path whose 32-character store hash equals
// hash, or "" when nothing matches. Any other length never matches.
func (s *Store) LookupHash(hash string) string {
	s.snapshot() // refresh when stale; the index is read under mu below
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byHash[hash]
}

// Count is the number of listed store paths.
func (s *Store) Count() int {
	return len(s.snapshot())
}
