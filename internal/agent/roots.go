package agent

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"k3snix/internal/closure"
)

// Pod GC roots. After /v1/fetch materializes a closure for a pod, the agent
// records a root for the pod: a symlink named by the pod UID, in RootsDir,
// pointing at the closure's root store path. With the nix materializer
// RootsDir lives under /nix/var/nix/gcroots, so nix-collect-garbage keeps
// every path a running pod needs. With the dir materializer, /v1/gc applies
// the same rule to the agent's own store. Roots of pods that no longer
// exist on the node are removed by Reconcile.

// podUID matches a Kubernetes UID: hex digits and dashes.
var podUID = regexp.MustCompile(`^[0-9a-f-]{1,64}$`)

// addRoot points RootsDir/uid at the closure's root path, atomically.
func (s *Server) addRoot(uid string, c closure.Closure) error {
	if s.RootsDir == "" {
		return errors.New("pod roots are disabled (no -gcroots-dir)")
	}
	if !podUID.MatchString(uid) {
		return fmt.Errorf("bad pod uid %q", uid)
	}
	if c.Root == "" {
		return errors.New("closure has no root path")
	}
	if err := os.MkdirAll(s.RootsDir, 0o755); err != nil {
		return err
	}
	target := filepath.Join(s.Store.Dir, filepath.Base(c.Root))
	tmp := filepath.Join(s.RootsDir, ".tmp-"+uid)
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.RootsDir, uid))
}

// roots returns pod UID -> root store path for every pod root.
func (s *Server) roots() (map[string]string, error) {
	out := map[string]string{}
	if s.RootsDir == "" {
		return out, nil
	}
	ents, err := os.ReadDir(s.RootsDir)
	if errors.Is(err, os.ErrNotExist) {
		return out, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !podUID.MatchString(e.Name()) {
			continue
		}
		target, err := os.Readlink(filepath.Join(s.RootsDir, e.Name()))
		if err != nil {
			continue
		}
		out[e.Name()] = target
	}
	return out, nil
}

// Reconcile removes the root of every pod whose directory is gone from
// PodsDir, the kubelet's pod directory (/var/lib/kubelet/pods). The kubelet
// removes a pod's directory once the pod is deleted and its volumes are torn
// down, so a root outlives its pod by at most one reconcile interval. It
// returns how many roots it removed.
func (s *Server) Reconcile() (int, error) {
	if s.RootsDir == "" || s.PodsDir == "" {
		return 0, nil
	}
	if _, err := os.Stat(s.PodsDir); err != nil {
		// Without the kubelet directory every pod would look deleted.
		return 0, fmt.Errorf("pods dir: %w", err)
	}
	roots, err := s.roots()
	if err != nil {
		return 0, err
	}
	removed := 0
	for uid := range roots {
		if _, err := os.Stat(filepath.Join(s.PodsDir, uid)); !errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err := os.Remove(filepath.Join(s.RootsDir, uid)); err == nil {
			removed++
			if s.Log != nil {
				s.Log.Printf("roots: pod %s is gone, removed its root", uid)
			}
		}
	}
	return removed, nil
}

// RunReconciler calls Reconcile every interval until stop is closed.
func (s *Server) RunReconciler(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if _, err := s.Reconcile(); err != nil {
				s.logError("roots", s.RootsDir, err)
			}
		}
	}
}

// GCResp reports a /v1/gc run.
type GCResp struct {
	Roots        int      `json:"roots"`
	LivePaths    int      `json:"live_paths"`
	Removed      []string `json:"removed"`
	RemovedBytes uint64   `json:"removed_bytes"`
}

// handleGC deletes every catalog path in the agent's own store that no pod
// root needs. Paths the catalog does not describe are never touched, and
// GC refuses to run on a nix-managed store, where nix-collect-garbage does
// the same job from the same roots. A root whose closure is not in the
// catalog keeps the whole store, because its references are unknown.
func (s *Server) handleGC(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 4<<10))
	if _, ok := s.Materializer.(DirMaterializer); !ok {
		http.Error(w, "gc runs only on a -materialize=dir store; use nix-collect-garbage", http.StatusBadRequest)
		return
	}
	if s.Catalog == nil {
		http.Error(w, "no catalog", http.StatusBadRequest)
		return
	}
	// No fetch runs while the live set is computed and paths are removed.
	s.gcMu.Lock()
	defer s.gcMu.Unlock()
	roots, err := s.roots()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	live := map[string]bool{}
	for uid, target := range roots {
		c, ok := s.Catalog.ByRoot(filepath.Join("/nix/store", filepath.Base(target)))
		if !ok {
			http.Error(w, fmt.Sprintf("root of pod %s (%s) is not a catalog closure; not collecting", uid, target),
				http.StatusConflict)
			return
		}
		for _, p := range c.Paths {
			live[filepath.Base(p.Path)] = true
		}
	}
	resp := GCResp{Roots: len(roots), LivePaths: len(live), Removed: []string{}}
	ents, err := os.ReadDir(s.Store.Dir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	for _, e := range ents {
		name := e.Name()
		meta, inCatalog := s.catalogPaths[name]
		if !inCatalog || live[name] || strings.HasPrefix(name, ".") {
			continue
		}
		removeTree(filepath.Join(s.Store.Dir, name))
		resp.Removed = append(resp.Removed, meta.Path)
		resp.RemovedBytes += meta.NarSize
	}
	sort.Strings(resp.Removed)
	s.Store.Refresh()
	s.mu.Lock()
	s.gcRuns++
	s.gcRemovedBytes += int64(resp.RemovedBytes)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
