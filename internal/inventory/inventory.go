// Package inventory tracks which store paths exist on this node.
// The have-set stays local. It is never uploaded anywhere.
package inventory

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"k3snix/internal/closure"
)

// Store is a cached listing of a Nix store directory.
type Store struct {
	Dir string
	TTL time.Duration

	mu      sync.Mutex
	have    map[string]uint64 // full path -> Lstat size
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

// Refresh rescans the store directory. Keys are full paths, values are the
// Lstat sizes. Entries whose name starts with "." are skipped.
func (s *Store) Refresh() error {
	ents, err := os.ReadDir(s.Dir)
	if err != nil {
		return err
	}
	have := make(map[string]uint64, len(ents))
	for _, e := range ents {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		var size uint64
		if info, err := e.Info(); err == nil {
			size = uint64(info.Size())
		}
		have[filepath.Join(s.Dir, name)] = size
	}
	s.mu.Lock()
	s.have = have
	s.fetched = time.Now()
	s.mu.Unlock()
	return nil
}

// snapshot returns the have-set, refreshing it first when stale. A failed
// refresh keeps the previous listing.
func (s *Store) snapshot() map[string]uint64 {
	s.mu.Lock()
	stale := s.TTL == 0 || s.have == nil || time.Since(s.fetched) > s.TTL
	s.mu.Unlock()
	if stale {
		_ = s.Refresh()
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

// LookupHash returns the full path whose basename starts with hash, or ""
// when nothing matches.
func (s *Store) LookupHash(hash string) string {
	have := s.snapshot()
	names := make([]string, 0, len(have))
	for p := range have {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		if strings.HasPrefix(filepath.Base(p), hash) {
			return p
		}
	}
	return ""
}

// Count is the number of listed store paths.
func (s *Store) Count() int {
	return len(s.snapshot())
}
