// Package agent implements the node DaemonSet: a missing-bytes API plus a
// Nix binary cache served from the local store.
package agent

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"k3snix/internal/closure"
	"k3snix/internal/inventory"
	"k3snix/internal/nar"
	"k3snix/internal/nixbase32"
)

// DumpFunc writes the NAR serialization of storePath to w.
type DumpFunc func(storePath string, w io.Writer) error

// Server serves missing-byte counts and NARs for the local store.
type Server struct {
	Store    *inventory.Store
	Catalog  *closure.Catalog
	Log      *log.Logger
	DumpFunc DumpFunc

	// DumpTTL bounds how long an idle finished dump is reused. Expired
	// entries and their temp files are evicted on the next dump request or
	// release. Zero means defaultDumpTTL.
	DumpTTL time.Duration

	// MaxDumpBytes caps the total size of cached dump files. Once a dump
	// pushes the total over it, idle entries are evicted least recently
	// used first. Entries a caller still holds are never evicted, so the
	// total can exceed the cap while they are in use. Zero means
	// defaultMaxDumpBytes.
	MaxDumpBytes int64

	// Origin is the base URL of the upstream cache that /v1/origin_probe
	// measures. Empty disables probing: the endpoint answers 400 and the
	// origin counters stay untouched. Nothing else on this server ever
	// contacts the origin.
	Origin string

	// closures mirrors the catalog contents so narinfo can find references
	// for a store path without enumerating the Catalog's private indexes.
	closures []closure.Closure

	mu             sync.Mutex
	dumps          map[string]*dumpCall
	dumpBytes      int64 // total size of the finished dumps in dumps
	narServed      int64
	bytesServed    int64
	originRequests int64
	originBytes    int64
}

// dumpResult is a finished NAR dump kept on disk so waiters can copy the
// same bytes that the narinfo hash describes.
type dumpResult struct {
	file string
	size int64
	hash string // nix base32 of the SHA-256 over the NAR bytes
}

type dumpCall struct {
	done chan struct{}
	res  dumpResult
	err  error
	refs int       // callers holding res; a held entry is never evicted
	used time.Time // when the last caller released res
}

const (
	// defaultDumpTTL applies when Server.DumpTTL is unset.
	defaultDumpTTL = 5 * time.Minute
	// defaultMaxDumpBytes applies when Server.MaxDumpBytes is unset.
	defaultMaxDumpBytes = 2 << 30
)

// New returns a Server serving storeDir.
func New(storeDir string) *Server {
	return &Server{
		Store:        inventory.New(storeDir),
		DumpFunc:     NARStoreDump,
		DumpTTL:      defaultDumpTTL,
		MaxDumpBytes: defaultMaxDumpBytes,
		dumps:        map[string]*dumpCall{},
		Log:          log.New(os.Stderr, "k3snix-agent ", log.LstdFlags),
	}
}

// LoadCatalog loads a directory of closure JSON files. It exits non-zero at
// the call site when it fails.
func (s *Server) LoadCatalog(dir string) error {
	cat, err := closure.LoadCatalog(dir)
	if err != nil {
		return err
	}
	s.Catalog = cat

	ents, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			return err
		}
		c, err := closure.Parse(data)
		if err != nil {
			return fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		s.closures = append(s.closures, c)
	}
	return nil
}

// Handler returns the agent's HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealth)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/missing_bytes", s.handleMissing)
	mux.HandleFunc("/v1/origin_probe", s.handleOriginProbe)
	mux.HandleFunc("/nix-cache-info", s.handleCacheInfo)
	mux.HandleFunc("/", s.handleCache)
	return mux
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "ok store=%s paths=%d\n", s.Store.Dir, s.Store.Count())
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	narServed, bytesServed := s.narServed, s.bytesServed
	originRequests, originBytes := s.originRequests, s.originBytes
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "k3snix_nar_served %d\n", narServed)
	fmt.Fprintf(w, "k3snix_nar_bytes_served %d\n", bytesServed)
	fmt.Fprintf(w, "k3snix_store_paths %d\n", s.Store.Count())
	fmt.Fprintf(w, "k3snix_origin_requests %d\n", originRequests)
	fmt.Fprintf(w, "k3snix_origin_bytes %d\n", originBytes)
}

type MissingReq struct {
	ID    string         `json:"id"`
	Label string         `json:"label"`
	Paths []closure.Path `json:"paths"`
}

// MissingResp uses the HTTP field names, which differ from closure.Missing's
// JSON names, so the mapping happens here at the handler boundary.
type MissingResp struct {
	Bytes      uint64   `json:"bytes"`
	HaveBytes  uint64   `json:"have_bytes"`
	TotalBytes uint64   `json:"total_bytes"`
	HavePaths  int      `json:"have_paths"`
	TotalPaths int      `json:"total_paths"`
	EstimateMS float64  `json:"estimate_ms"`
	Missing    []string `json:"missing"`
}

func (s *Server) handleMissing(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	var req MissingReq
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return
	}

	var c closure.Closure
	switch {
	case len(req.Paths) > 0:
		c = closure.Closure{Paths: req.Paths}
	case req.ID != "":
		if s.Catalog == nil {
			http.Error(w, "unknown closure", http.StatusNotFound)
			return
		}
		var ok bool
		if c, ok = s.Catalog.Lookup(req.ID); !ok {
			http.Error(w, "unknown closure", http.StatusNotFound)
			return
		}
	case req.Label != "":
		if s.Catalog == nil {
			http.Error(w, "unknown closure", http.StatusNotFound)
			return
		}
		var ok bool
		if c, ok = s.Catalog.ByLabel(req.Label); !ok {
			http.Error(w, "unknown closure", http.StatusNotFound)
			return
		}
	default:
		http.Error(w, "one of paths, id or label is required", http.StatusBadRequest)
		return
	}

	start := time.Now()
	m := s.Store.Missing(c)
	resp := MissingResp{
		Bytes:      m.MissingBytes,
		HaveBytes:  m.HaveBytes,
		TotalBytes: m.TotalBytes,
		HavePaths:  m.HavePaths,
		TotalPaths: m.TotalPaths,
		EstimateMS: float64(time.Since(start).Microseconds()) / 1000,
		Missing:    m.MissingPaths,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) handleCacheInfo(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-nix-cache-info")
	fmt.Fprintf(w, "StoreDir: %s\nWantMassQuery: 1\nPriority: 30\n", s.Store.Dir)
}

func (s *Server) handleCache(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	switch {
	case strings.HasSuffix(p, ".narinfo"):
		hash := strings.TrimSuffix(p, ".narinfo")
		if !validStoreHash(hash) {
			http.NotFound(w, r)
			return
		}
		s.serveNarinfo(w, r, hash)
	case strings.HasPrefix(p, "nar/") && strings.HasSuffix(p, ".nar"):
		hash := strings.TrimSuffix(strings.TrimPrefix(p, "nar/"), ".nar")
		if !validStoreHash(hash) {
			http.NotFound(w, r)
			return
		}
		s.serveNAR(w, r, hash)
	default:
		http.NotFound(w, r)
	}
}

// validStoreHash accepts only a full 32-character store hash. LookupHash
// matches exactly, so a short probe like /a.narinfo can never prefix-match
// a store path, enumerate the store, or trigger a dump; this check only
// keeps malformed URLs out of the dump path early.
func validStoreHash(s string) bool {
	return len(s) == 32
}

func (s *Server) serveNarinfo(w http.ResponseWriter, r *http.Request, hash string) {
	path := s.Store.LookupHash(hash)
	if path == "" {
		http.NotFound(w, r)
		return
	}
	res, release, err := s.dumpCoalesced(path)
	defer release()
	if err != nil {
		s.logError("narinfo", path, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/x-nix-narinfo")
	fmt.Fprintf(w, "StorePath: %s\n", path)
	fmt.Fprintf(w, "URL: nar/%s.nar\n", hash)
	fmt.Fprintf(w, "Compression: none\n")
	fmt.Fprintf(w, "FileHash: sha256:%s\n", res.hash)
	fmt.Fprintf(w, "FileSize: %d\n", res.size)
	fmt.Fprintf(w, "NarHash: sha256:%s\n", res.hash)
	fmt.Fprintf(w, "NarSize: %d\n", res.size)
	fmt.Fprintf(w, "References: %s\n", strings.Join(s.references(path), " "))
}

func (s *Server) serveNAR(w http.ResponseWriter, r *http.Request, hash string) {
	path := s.Store.LookupHash(hash)
	if path == "" {
		http.NotFound(w, r)
		return
	}
	res, release, err := s.dumpCoalesced(path)
	defer release()
	if err != nil {
		s.logError("nar", path, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(res.file)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/x-nix-nar")
	w.Header().Set("Content-Length", strconv.FormatInt(res.size, 10))
	w.WriteHeader(http.StatusOK)
	n, _ := io.Copy(w, f)
	s.mu.Lock()
	s.narServed++
	s.bytesServed += n
	s.mu.Unlock()
}

// dumpCoalesced returns a finished dump for path and a release func that the
// caller must call once it no longer needs res.file, error or not.
// Overlapping requests call DumpFunc once; waiters copy the file the leader
// produced. An entry is never evicted while a caller holds it, however long
// the dump took. A failed dump is dropped from the cache so the next request
// dumps again.
func (s *Server) dumpCoalesced(path string) (dumpResult, func(), error) {
	s.mu.Lock()
	s.evictLocked(time.Now())
	c, ok := s.dumps[path]
	if !ok {
		c = &dumpCall{done: make(chan struct{})}
		s.dumps[path] = c
	}
	c.refs++
	s.mu.Unlock()
	release := func() { s.release(c) }

	if ok {
		<-c.done
		return c.res, release, c.err
	}

	res, err := s.dumpOnce(path)
	c.res, c.err = res, err
	s.mu.Lock()
	if err != nil {
		delete(s.dumps, path)
	} else {
		s.dumpBytes += res.size
	}
	s.mu.Unlock()
	close(c.done)
	return res, release, err
}

// release drops one caller's hold on c and sweeps the cache, so a dump that
// pushed the total over MaxDumpBytes is trimmed as soon as it goes idle.
func (s *Server) release(c *dumpCall) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c.refs--
	c.used = time.Now()
	s.evictLocked(c.used)
}

// evictLocked unlinks idle dumps: first those unused for DumpTTL, then,
// least recently used first, as many as it takes to bring the total under
// MaxDumpBytes. Held entries are skipped; the leader holds its entry until
// after the dump finishes, so running dumps are skipped too. Callers must
// hold s.mu.
func (s *Server) evictLocked(now time.Time) {
	ttl := s.DumpTTL
	if ttl <= 0 {
		ttl = defaultDumpTTL
	}
	limit := s.MaxDumpBytes
	if limit <= 0 {
		limit = defaultMaxDumpBytes
	}

	var idle []string
	for path, c := range s.dumps {
		if c.refs > 0 {
			continue
		}
		if now.Sub(c.used) >= ttl {
			s.dropLocked(path, c)
			continue
		}
		idle = append(idle, path)
	}
	if s.dumpBytes <= limit {
		return
	}
	sort.Slice(idle, func(i, j int) bool {
		return s.dumps[idle[i]].used.Before(s.dumps[idle[j]].used)
	})
	for _, path := range idle {
		if s.dumpBytes <= limit {
			return
		}
		s.dropLocked(path, s.dumps[path])
	}
}

// dropLocked removes a finished dump from the cache and unlinks its file.
// Callers must hold s.mu.
func (s *Server) dropLocked(path string, c *dumpCall) {
	delete(s.dumps, path)
	s.dumpBytes -= c.res.size
	if c.res.file != "" {
		_ = os.Remove(c.res.file)
	}
}

// dumpOnce dumps path to a temp file and hashes the result. The temp file is
// removed when the dump fails, so a partial NAR is never served. Minimal
// images (dockerTools.buildLayeredImage) ship no /tmp, so it is created
// before use.
func (s *Server) dumpOnce(path string) (dumpResult, error) {
	// 0700 is deliberate: single-process container, no shared /tmp.
	if err := os.MkdirAll(os.TempDir(), 0o700); err != nil {
		return dumpResult{}, err
	}
	tmp, err := os.CreateTemp("", "k3snix-nar-")
	if err != nil {
		return dumpResult{}, err
	}
	name := tmp.Name()
	kept := false
	defer func() {
		tmp.Close()
		if !kept {
			os.Remove(name)
		}
	}()

	if err := s.DumpFunc(path, tmp); err != nil {
		return dumpResult{}, fmt.Errorf("dump %s: %w", path, err)
	}
	if _, err := tmp.Seek(0, io.SeekStart); err != nil {
		return dumpResult{}, err
	}
	h := sha256.New()
	size, err := io.Copy(h, tmp)
	if err != nil {
		return dumpResult{}, err
	}
	kept = true
	return dumpResult{file: name, size: size, hash: nixbase32.EncodeToString(h.Sum(nil))}, nil
}

// references returns the basenames a store path refers to: from the catalog
// when the path appears there, else from nix-store, else nothing.
func (s *Server) references(path string) []string {
	base := filepath.Base(path)
	for _, c := range s.closures {
		for _, p := range c.Paths {
			if filepath.Base(p.Path) != base {
				continue
			}
			refs := make([]string, 0, len(p.References))
			for _, ref := range p.References {
				refs = append(refs, filepath.Base(ref))
			}
			return refs
		}
	}
	out, err := exec.Command("nix-store", "-q", "--references", path).Output()
	if err != nil {
		return nil
	}
	var refs []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			refs = append(refs, filepath.Base(line))
		}
	}
	return refs
}

func (s *Server) logError(kind, path string, err error) {
	if s.Log != nil {
		s.Log.Printf("%s %s: %v", kind, path, err)
	}
}

// NARStoreDump dumps path with nix-store --dump when that binary exists and
// the command succeeds, otherwise with the pure-Go nar writer. If nix-store
// wrote partial output before failing, the target is reset before the
// fallback so the result is never a mixed stream.
func NARStoreDump(path string, w io.Writer) error {
	if _, err := exec.LookPath("nix-store"); err == nil {
		cmd := exec.Command("nix-store", "--dump", path)
		cmd.Stdout = w
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err == nil {
			return nil
		}
		if rw, ok := w.(interface {
			io.Seeker
			Truncate(int64) error
		}); ok {
			_ = rw.Truncate(0)
			_, _ = rw.Seek(0, io.SeekStart)
		}
	}
	return nar.Write(w, path)
}
