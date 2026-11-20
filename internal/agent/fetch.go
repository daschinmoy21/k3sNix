package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k3snix/internal/closure"
	"k3snix/internal/nixbase32"
)

// Fetch timeouts and bounds.
const (
	fetchNarinfoTimeout = 5 * time.Second
	fetchNARTimeout     = 10 * time.Minute
	defaultFetchWorkers = 4
	maxFetchRequest     = 4 << 10
)

// FetchReq names the closure /v1/fetch materializes, by catalog ID, label
// or root store path. Only catalog closures can be fetched: the catalog's
// narHash and narSize are what every downloaded NAR is checked against, so
// a peer never has to be trusted.
//
// PodUID, when set, makes the agent hold a GC root for that pod on the
// closure once the fetch succeeds.
type FetchReq struct {
	ID     string `json:"id"`
	Label  string `json:"label"`
	Root   string `json:"root"`
	PodUID string `json:"pod_uid"`
}

// FetchedPath reports how one closure path reached the store.
type FetchedPath struct {
	Path      string `json:"path"`
	Source    string `json:"source"` // "peer <url>" or "origin"
	Bytes     int64  `json:"bytes"`  // NAR bytes this request downloaded
	Coalesced bool   `json:"coalesced,omitempty"`
}

// FetchResp reports a /v1/fetch call. PredictedBytes is the missing_bytes
// answer at the start; FetchedBytes is what was downloaded, so the two
// agree unless another request fetched a path concurrently.
type FetchResp struct {
	Closure        string        `json:"closure"`
	Label          string        `json:"label,omitempty"`
	TotalPaths     int           `json:"total_paths"`
	LocalHits      int           `json:"local_hits"`
	PredictedBytes uint64        `json:"predicted_bytes"`
	FetchedBytes   int64         `json:"fetched_bytes"`
	PeerBytes      int64         `json:"peer_bytes"`
	OriginBytes    int64         `json:"origin_bytes"`
	DurationMS     float64       `json:"duration_ms"`
	Fetched        []FetchedPath `json:"fetched"`
	GCRoot         string        `json:"gc_root,omitempty"`
}

// Materializer makes a verified NAR visible as a store path.
type Materializer interface {
	// Materialize installs p from nar, a complete file whose bytes already
	// match p's narHash and narSize. The path must become visible
	// atomically: before it returns, the store either lacks it or holds all
	// of it. closure holds the catalog entries of the closure p belongs
	// to, by full path; p's references are installed already.
	Materialize(p closure.Path, nar *os.File, closure map[string]closure.Path) error
}

// fetchCall coalesces concurrent fetches of one store path.
type fetchCall struct {
	done   chan struct{}
	source string
	err    error
}

// fetchState is the fetch side of Server; it is embedded so the zero value
// works.
type fetchState struct {
	fetches map[string]*fetchCall

	fetchRequests    int64
	fetchFailures    int64
	fetchLocalHits   int64
	fetchPaths       int64
	fetchJoined      int64
	fetchPeerReqs    int64
	fetchPeerBytes   int64
	fetchPredicted   int64
	fetchFetched     int64
	fetchVerifyFails int64
	gcRuns           int64
	gcRemovedBytes   int64
}

func (s *Server) resolveFetch(req FetchReq) (closure.Closure, error) {
	if s.Catalog == nil {
		return closure.Closure{}, errors.New("no catalog")
	}
	var (
		c  closure.Closure
		ok bool
	)
	switch {
	case req.ID != "":
		c, ok = s.Catalog.Lookup(req.ID)
	case req.Label != "":
		c, ok = s.Catalog.ByLabel(req.Label)
	case req.Root != "":
		c, ok = s.Catalog.ByRoot(req.Root)
	default:
		return closure.Closure{}, errors.New("one of id, label or root is required")
	}
	if !ok {
		return closure.Closure{}, errors.New("unknown closure")
	}
	for _, p := range c.Paths {
		if p.NarHashNix32() == "" || p.NarSize == 0 {
			return closure.Closure{}, fmt.Errorf("catalog has no narHash and narSize for %s", p.Path)
		}
	}
	return c, nil
}

func (s *Server) handleFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	if s.Materializer == nil {
		http.Error(w, "fetching is disabled (no -materialize)", http.StatusBadRequest)
		return
	}
	var req FetchReq
	r.Body = http.MaxBytesReader(w, r.Body, maxFetchRequest)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.PodUID != "" && (s.RootsDir == "" || !podUID.MatchString(req.PodUID)) {
		http.Error(w, fmt.Sprintf("cannot root pod %q: bad uid or no -gcroots-dir", req.PodUID), http.StatusBadRequest)
		return
	}
	c, err := s.resolveFetch(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.gcMu.RLock()
	defer s.gcMu.RUnlock()
	resp, err := s.Fetch(r.Context(), c)
	if err == nil && req.PodUID != "" {
		if err = s.addRoot(req.PodUID, c); err == nil {
			resp.GCRoot = filepath.Join(s.RootsDir, req.PodUID)
		}
	}
	if err != nil {
		s.mu.Lock()
		s.fetchFailures++
		s.mu.Unlock()
		s.logError("fetch", c.Label, err)
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// Fetch materializes every path of c that the store lacks: from a peer
// that serves it when one does, otherwise from the origin. Each NAR is
// downloaded completely and checked against the catalog's narHash and
// narSize before the Materializer sees it, so a partial or wrong NAR never
// becomes a store path. Paths are installed references first.
func (s *Server) Fetch(ctx context.Context, c closure.Closure) (FetchResp, error) {
	start := time.Now()
	if err := s.Store.Refresh(); err != nil {
		return FetchResp{}, fmt.Errorf("store listing: %w", err)
	}
	m := s.Store.Missing(c)
	resp := FetchResp{
		Closure:        c.Fingerprint(),
		Label:          c.Label,
		TotalPaths:     m.TotalPaths,
		LocalHits:      m.HavePaths,
		PredictedBytes: m.MissingBytes,
		Fetched:        []FetchedPath{},
	}
	s.mu.Lock()
	s.fetchRequests++
	s.fetchLocalHits += int64(m.HavePaths)
	s.fetchPredicted += int64(m.MissingBytes)
	s.mu.Unlock()

	byPath := make(map[string]closure.Path, len(c.Paths))
	for _, p := range c.Paths {
		byPath[p.Path] = p
	}
	peers := s.peerURLs()
	var mu sync.Mutex
	for _, level := range referenceLevels(m.MissingPaths, byPath) {
		var (
			wg       sync.WaitGroup
			firstErr error
			sem      = make(chan struct{}, s.fetchWorkers())
		)
		for _, path := range level {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				fp, err := s.fetchCoalesced(ctx, byPath[path], byPath, peers)
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					if firstErr == nil {
						firstErr = fmt.Errorf("%s: %w", path, err)
					}
					return
				}
				if fp.Source == "local" {
					resp.LocalHits++
					return
				}
				resp.Fetched = append(resp.Fetched, fp)
				resp.FetchedBytes += fp.Bytes
				if fp.Source == "origin" {
					resp.OriginBytes += fp.Bytes
				} else {
					resp.PeerBytes += fp.Bytes
				}
			}()
		}
		wg.Wait()
		if firstErr != nil {
			return resp, firstErr
		}
	}
	sort.Slice(resp.Fetched, func(i, j int) bool { return resp.Fetched[i].Path < resp.Fetched[j].Path })
	if err := s.Store.Refresh(); err != nil {
		return resp, err
	}
	resp.DurationMS = float64(time.Since(start).Microseconds()) / 1000
	return resp, nil
}

func (s *Server) fetchWorkers() int {
	if s.FetchWorkers > 0 {
		return s.FetchWorkers
	}
	return defaultFetchWorkers
}

// referenceLevels groups missing paths so every path comes after the
// missing paths it refers to: level 0 refers to no missing path, level n
// only to paths in lower levels. Paths within a level are independent.
func referenceLevels(missing []string, byPath map[string]closure.Path) [][]string {
	want := make(map[string]bool, len(missing))
	for _, p := range missing {
		want[p] = true
	}
	depth := map[string]int{}
	var visit func(p string, seen map[string]bool) int
	visit = func(p string, seen map[string]bool) int {
		if d, ok := depth[p]; ok {
			return d
		}
		seen[p] = true
		d := 0
		for _, ref := range byPath[p].References {
			if ref == p || !want[ref] || seen[ref] {
				continue // self-reference, present already, or a cycle
			}
			if rd := visit(ref, seen) + 1; rd > d {
				d = rd
			}
		}
		delete(seen, p)
		depth[p] = d
		return d
	}
	var levels [][]string
	sorted := append([]string(nil), missing...)
	sort.Strings(sorted)
	for _, p := range sorted {
		d := visit(p, map[string]bool{})
		for len(levels) <= d {
			levels = append(levels, nil)
		}
		levels[d] = append(levels[d], p)
	}
	return levels
}

// fetchCoalesced fetches p once however many requests want it at the same
// time. Requests that join a running fetch report it as coalesced with 0
// bytes, so the bytes are counted once.
func (s *Server) fetchCoalesced(ctx context.Context, p closure.Path, byPath map[string]closure.Path, peers []string) (FetchedPath, error) {
	s.mu.Lock()
	if s.fetches == nil {
		s.fetches = map[string]*fetchCall{}
	}
	if c, ok := s.fetches[p.Path]; ok {
		s.fetchJoined++
		s.mu.Unlock()
		select {
		case <-c.done:
		case <-ctx.Done():
			return FetchedPath{}, ctx.Err()
		}
		return FetchedPath{Path: p.Path, Source: c.source, Coalesced: true}, c.err
	}
	c := &fetchCall{done: make(chan struct{})}
	s.fetches[p.Path] = c
	s.mu.Unlock()

	// Another request may have installed the path after this one listed
	// the store.
	if s.present(p) {
		c.source = "local"
		s.mu.Lock()
		delete(s.fetches, p.Path)
		s.fetchLocalHits++
		s.mu.Unlock()
		close(c.done)
		return FetchedPath{Path: p.Path, Source: "local"}, nil
	}

	// The leader's fetch must not die with its own request while others
	// wait on it.
	source, n, err := s.fetchPath(context.WithoutCancel(ctx), p, byPath, peers)
	c.source, c.err = source, err
	s.mu.Lock()
	delete(s.fetches, p.Path)
	if err == nil {
		s.fetchPaths++
		s.fetchFetched += n
	}
	s.mu.Unlock()
	close(c.done)
	return FetchedPath{Path: p.Path, Source: source, Bytes: n}, err
}

// present reports whether p is installed in the store and not being
// written by nix (no lock file beside it).
func (s *Server) present(p closure.Path) bool {
	full := filepath.Join(s.Store.Dir, filepath.Base(p.Path))
	if _, err := os.Lstat(full); err != nil {
		return false
	}
	_, err := os.Lstat(full + ".lock")
	return errors.Is(err, os.ErrNotExist)
}

// fetchPath tries every peer in order and then the origin. A source that
// lacks the path, fails, or serves bytes that do not match the catalog is
// skipped.
func (s *Server) fetchPath(ctx context.Context, p closure.Path, byPath map[string]closure.Path, peers []string) (string, int64, error) {
	var errs []string
	for _, peer := range peers {
		n, reqs, err := s.fetchFrom(ctx, peer, p, byPath)
		if err == nil {
			s.mu.Lock()
			s.fetchPeerReqs += reqs
			s.fetchPeerBytes += n
			s.mu.Unlock()
			return "peer " + peer, n, nil
		}
		if !errors.Is(err, errNotServed) {
			errs = append(errs, fmt.Sprintf("peer %s: %v", peer, err))
		}
	}
	if s.Origin == "" {
		errs = append(errs, "no origin configured")
		return "", 0, fmt.Errorf("no source served the path: %s", strings.Join(errs, "; "))
	}
	n, reqs, err := s.fetchFrom(ctx, s.Origin, p, byPath)
	if err != nil {
		errs = append(errs, fmt.Sprintf("origin: %v", err))
		return "", 0, fmt.Errorf("no source served the path: %s", strings.Join(errs, "; "))
	}
	// The narinfo bytes are small and not part of the NAR count; the
	// origin counters keep the NAR bytes so they compare with narSize.
	s.countOrigin(reqs, n)
	return "origin", n, nil
}

var errNotServed = errors.New("not served")

// fetchFrom downloads p's narinfo and NAR from base, verifies the NAR
// against the catalog, and materializes it. It returns the NAR bytes and
// the number of requests made.
func (s *Server) fetchFrom(ctx context.Context, base string, p closure.Path, byPath map[string]closure.Path) (int64, int64, error) {
	base = strings.TrimRight(base, "/")
	hash := filepath.Base(p.Path)[:32]
	nctx, cancel := context.WithTimeout(ctx, fetchNarinfoTimeout)
	defer cancel()
	info, err := getSmall(nctx, base+"/"+hash+".narinfo")
	if err != nil {
		return 0, 0, err
	}
	fields := parseNarinfoFields(info)
	// Peers may keep their store elsewhere (the k3d agents use
	// /data/store), so only the store path's name has to match.
	if filepath.Base(fields["StorePath"]) != filepath.Base(p.Path) {
		return 0, 1, fmt.Errorf("narinfo is for %q", fields["StorePath"])
	}
	if c := fields["Compression"]; c != "" && c != "none" {
		return 0, 1, fmt.Errorf("narinfo compression %q is not supported", c)
	}
	url := fields["URL"]
	if url == "" || strings.Contains(url, "://") || strings.HasPrefix(url, "/") {
		return 0, 1, fmt.Errorf("narinfo URL %q is not a relative path", url)
	}

	f, err := s.downloadNAR(ctx, base+"/"+url, p)
	if err != nil {
		return 0, 2, err
	}
	defer func() {
		f.Close()
		os.Remove(f.Name())
	}()
	if err := s.Materializer.Materialize(p, f, byPath); err != nil {
		return 0, 2, fmt.Errorf("materialize: %w", err)
	}
	return int64(p.NarSize), 2, nil
}

// downloadNAR streams url into a temp file and checks its size and SHA-256
// against the catalog entry. The file is removed unless it verifies.
func (s *Server) downloadNAR(ctx context.Context, url string, p closure.Path) (*os.File, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchNARTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, originMaxBody))
		return nil, fmt.Errorf("NAR: unexpected status %d", resp.StatusCode)
	}
	if err := os.MkdirAll(os.TempDir(), 0o700); err != nil {
		return nil, err
	}
	f, err := os.CreateTemp("", "k3snix-fetch-")
	if err != nil {
		return nil, err
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
			os.Remove(f.Name())
		}
	}()
	h := sha256.New()
	size := int64(p.NarSize)
	// One byte past narSize so an oversized body fails.
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, size+1))
	if err != nil {
		return nil, err
	}
	got, want := nixbase32.EncodeToString(h.Sum(nil)), p.NarHashNix32()
	if n != size || got != want {
		s.mu.Lock()
		s.fetchVerifyFails++
		s.mu.Unlock()
		return nil, fmt.Errorf("NAR is sha256:%s (%d bytes), catalog says sha256:%s (%d bytes)", got, n, want, size)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	ok = true
	return f, nil
}

// getSmall GETs a small document. A 404 is errNotServed.
func getSmall(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, originMaxBody))
		return nil, errNotServed
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, originMaxBody))
		return nil, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, originMaxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > originMaxBody {
		return nil, errors.New("response too large")
	}
	return body, nil
}

// peerURLs returns the peer caches to try, in order: the static -peers list,
// then every address -peers-dns resolves to except this node's own.
func (s *Server) peerURLs() []string {
	out := append([]string(nil), s.Peers...)
	if s.PeersDNS == "" {
		return out
	}
	addrs, err := net.LookupHost(s.PeersDNS)
	if err != nil {
		s.logError("peers", s.PeersDNS, err)
		return out
	}
	local := localAddrs()
	sort.Strings(addrs)
	port := s.PeerPort
	if port == 0 {
		port = 9860
	}
	for _, a := range addrs {
		if local[a] {
			continue
		}
		out = append(out, "http://"+net.JoinHostPort(a, strconv.Itoa(port)))
	}
	return out
}

func localAddrs() map[string]bool {
	out := map[string]bool{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			out[ipn.IP.String()] = true
		}
	}
	return out
}

func (s *Server) writeFetchMetrics(w io.Writer) {
	s.mu.Lock()
	f := s.fetchState
	s.mu.Unlock()
	fmt.Fprintf(w, "k3snix_fetch_requests %d\n", f.fetchRequests)
	fmt.Fprintf(w, "k3snix_fetch_failures %d\n", f.fetchFailures)
	fmt.Fprintf(w, "k3snix_fetch_local_hits %d\n", f.fetchLocalHits)
	fmt.Fprintf(w, "k3snix_fetch_paths %d\n", f.fetchPaths)
	fmt.Fprintf(w, "k3snix_fetch_coalesced %d\n", f.fetchJoined)
	fmt.Fprintf(w, "k3snix_fetch_peer_requests %d\n", f.fetchPeerReqs)
	fmt.Fprintf(w, "k3snix_fetch_peer_bytes %d\n", f.fetchPeerBytes)
	fmt.Fprintf(w, "k3snix_fetch_predicted_bytes %d\n", f.fetchPredicted)
	fmt.Fprintf(w, "k3snix_fetch_fetched_bytes %d\n", f.fetchFetched)
	fmt.Fprintf(w, "k3snix_fetch_verify_failures %d\n", f.fetchVerifyFails)
	roots, _ := s.roots()
	fmt.Fprintf(w, "k3snix_gc_roots %d\n", len(roots))
	fmt.Fprintf(w, "k3snix_gc_runs %d\n", f.gcRuns)
	fmt.Fprintf(w, "k3snix_gc_removed_bytes %d\n", f.gcRemovedBytes)
}
