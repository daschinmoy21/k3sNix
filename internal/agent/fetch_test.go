package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k3snix/internal/closure"
	"k3snix/internal/nar"
	"k3snix/internal/nixbase32"
)

// Store path names for the fetch closure: app refers to lib, lib to libc.
const (
	fAppName  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-app"
	fLibName  = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib"
	fLibcName = "cccccccccccccccccccccccccccccccc-libc"
)

// fetchFixture is an origin store holding a three-path closure with real
// NARs, and a catalog describing it.
type fetchFixture struct {
	origin string // origin store directory
	catDir string
	paths  map[string]closure.Path // by basename
}

func newFetchFixture(t *testing.T) fetchFixture {
	t.Helper()
	store := t.TempDir()
	mustDir(t, filepath.Join(store, fLibcName, "lib"))
	mustFile(t, filepath.Join(store, fLibcName, "lib", "libc.so"), bytes.Repeat([]byte("c"), 4096))
	mustDir(t, filepath.Join(store, fLibName, "lib"))
	mustFile(t, filepath.Join(store, fLibName, "lib", "liblib.so"), []byte("lib"))
	if err := os.Symlink("/nix/store/"+fLibcName+"/lib/libc.so", filepath.Join(store, fLibName, "lib", "libc.so")); err != nil {
		t.Fatal(err)
	}
	mustDir(t, filepath.Join(store, fAppName, "bin"))
	if err := os.WriteFile(filepath.Join(store, fAppName, "bin", "app"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	refs := map[string][]string{
		fAppName:  {fAppName, fLibName},
		fLibName:  {fLibName, fLibcName},
		fLibcName: {},
	}
	f := fetchFixture{origin: store, catDir: t.TempDir(), paths: map[string]closure.Path{}}
	var list []closure.Path
	for _, name := range []string{fAppName, fLibName, fLibcName} {
		var buf bytes.Buffer
		if err := nar.Write(&buf, filepath.Join(store, name)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(buf.Bytes())
		p := closure.Path{
			Path:    "/nix/store/" + name,
			NarSize: uint64(buf.Len()),
			NarHash: "sha256:" + nixbase32.EncodeToString(sum[:]),
		}
		for _, r := range refs[name] {
			p.References = append(p.References, "/nix/store/"+r)
		}
		f.paths[name] = p
		list = append(list, p)
	}
	doc, err := json.Marshal(closure.Closure{Label: "app", Root: "/nix/store/" + fAppName, Paths: list})
	if err != nil {
		t.Fatal(err)
	}
	mustFile(t, filepath.Join(f.catDir, "app.json"), doc)
	return f
}

func (f fetchFixture) totalBytes() int64 {
	var n int64
	for _, p := range f.paths {
		n += int64(p.NarSize)
	}
	return n
}

// serve starts an agent over dir with the fixture catalog. A non-nil
// wrap intercepts its handler.
func (f fetchFixture) serve(t *testing.T, dir string, wrap func(http.Handler) http.Handler) (*Server, *httptest.Server) {
	t.Helper()
	s := newServer(t, dir)
	s.Log = log.New(io.Discard, "", 0)
	if err := s.LoadCatalog(f.catDir); err != nil {
		t.Fatal(err)
	}
	s.Materializer = DirMaterializer{Dir: dir}
	var h http.Handler = s.Handler()
	if wrap != nil {
		h = wrap(h)
	}
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { chmodTree(dir) })
	return s, srv
}

// chmodTree makes restored read-only files removable for t.TempDir.
func chmodTree(dir string) {
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			os.Chmod(p, 0o755)
		}
		return nil
	})
}

func postFetch(t *testing.T, url, body string) (int, FetchResp, string) {
	t.Helper()
	resp, err := http.Post(url+"/v1/fetch", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var out FetchResp
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("fetch reply %s: %v", raw, err)
		}
	}
	return resp.StatusCode, out, string(raw)
}

// assertSameNARs checks every fixture path in dir dumps to its catalog hash.
func (f fetchFixture) assertSameNARs(t *testing.T, dir string) {
	t.Helper()
	for name, p := range f.paths {
		var buf bytes.Buffer
		if err := nar.Write(&buf, filepath.Join(dir, name)); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		sum := sha256.Sum256(buf.Bytes())
		if got := "sha256:" + nixbase32.EncodeToString(sum[:]); got != p.NarHash {
			t.Fatalf("%s restored as %s, catalog says %s", name, got, p.NarHash)
		}
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), ".k3snix-tmp-") {
			t.Fatalf("temp entry %s left in the store", e.Name())
		}
	}
}

func TestFetchOriginThenPeer(t *testing.T) {
	f := newFetchFixture(t)
	_, origin := f.serve(t, f.origin, nil)

	// The first node has no peers: everything comes from the origin.
	dir1 := t.TempDir()
	n1, srv1 := f.serve(t, dir1, nil)
	n1.Origin = origin.URL
	code, r1, raw := postFetch(t, srv1.URL, `{"label":"app"}`)
	if code != http.StatusOK {
		t.Fatalf("fetch status %d: %s", code, raw)
	}
	total := f.totalBytes()
	if r1.OriginBytes != total || r1.PeerBytes != 0 || r1.FetchedBytes != total ||
		int64(r1.PredictedBytes) != total || len(r1.Fetched) != 3 || r1.LocalHits != 0 {
		t.Fatalf("first fetch %+v, want all %d bytes from the origin", r1, total)
	}
	f.assertSameNARs(t, dir1)
	if got := metricValue(t, srv1, "k3snix_origin_bytes"); got != itoa(total) {
		t.Fatalf("k3snix_origin_bytes %s, want %d", got, total)
	}
	if code, out, _ := postMissing(t, srv1.URL, `{"label":"app"}`); code != 200 || decodeMissing(t, out).Bytes != 0 {
		t.Fatalf("missing_bytes after fetch: %d %v", code, out)
	}

	// The second node lists the first as a peer: no origin bytes at all.
	dir2 := t.TempDir()
	n2, srv2 := f.serve(t, dir2, nil)
	n2.Origin = origin.URL
	n2.Peers = []string{srv1.URL}
	code, r2, raw := postFetch(t, srv2.URL, `{"root":"/nix/store/`+fAppName+`"}`)
	if code != http.StatusOK {
		t.Fatalf("peer fetch status %d: %s", code, raw)
	}
	if r2.PeerBytes != total || r2.OriginBytes != 0 {
		t.Fatalf("second fetch %+v, want all %d bytes from the peer", r2, total)
	}
	for _, fp := range r2.Fetched {
		if fp.Source != "peer "+srv1.URL {
			t.Fatalf("%s came from %q", fp.Path, fp.Source)
		}
	}
	f.assertSameNARs(t, dir2)
	if got := metricValue(t, srv2, "k3snix_origin_requests"); got != "0" {
		t.Fatalf("k3snix_origin_requests %s on the peer-fed node, want 0", got)
	}
	if got := metricValue(t, srv2, "k3snix_fetch_peer_bytes"); got != itoa(total) {
		t.Fatalf("k3snix_fetch_peer_bytes %s, want %d", got, total)
	}
}

func TestFetchOnlyTheDelta(t *testing.T) {
	f := newFetchFixture(t)
	_, origin := f.serve(t, f.origin, nil)
	dir := t.TempDir()
	// The node already holds libc, as after an earlier deploy.
	var buf bytes.Buffer
	if err := nar.Write(&buf, filepath.Join(f.origin, fLibcName)); err != nil {
		t.Fatal(err)
	}
	if err := nar.Restore(&buf, filepath.Join(dir, fLibcName)); err != nil {
		t.Fatal(err)
	}
	n, srv := f.serve(t, dir, nil)
	n.Origin = origin.URL
	code, r, raw := postFetch(t, srv.URL, `{"label":"app"}`)
	if code != http.StatusOK {
		t.Fatalf("fetch status %d: %s", code, raw)
	}
	want := int64(f.paths[fAppName].NarSize + f.paths[fLibName].NarSize)
	if r.LocalHits != 1 || int64(r.PredictedBytes) != want || r.FetchedBytes != want || len(r.Fetched) != 2 {
		t.Fatalf("fetch %+v, want only app and lib (%d bytes)", r, want)
	}
	f.assertSameNARs(t, dir)

	// Fetching again moves nothing.
	_, again, _ := postFetch(t, srv.URL, `{"label":"app"}`)
	if again.FetchedBytes != 0 || again.LocalHits != 3 {
		t.Fatalf("second fetch %+v, want 3 local hits and nothing fetched", again)
	}
}

func TestFetchSkipsCorruptPeer(t *testing.T) {
	f := newFetchFixture(t)
	_, origin := f.serve(t, f.origin, nil)
	// A peer that answers narinfo honestly but serves wrong NAR bytes.
	_, honest := f.serve(t, f.origin, nil)
	liar := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/nar/") {
			resp, err := http.Get(honest.URL + r.URL.Path)
			if err != nil {
				http.Error(w, err.Error(), 500)
				return
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			body[len(body)-16] ^= 0xff
			w.Write(body)
			return
		}
		resp, err := http.Get(honest.URL + r.URL.Path)
		if err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		defer resp.Body.Close()
		w.WriteHeader(resp.StatusCode)
		io.Copy(w, resp.Body)
	}))
	defer liar.Close()

	dir := t.TempDir()
	n, srv := f.serve(t, dir, nil)
	n.Origin = origin.URL
	n.Peers = []string{liar.URL}
	code, r, raw := postFetch(t, srv.URL, `{"label":"app"}`)
	if code != http.StatusOK {
		t.Fatalf("fetch status %d: %s", code, raw)
	}
	if r.OriginBytes != f.totalBytes() || r.PeerBytes != 0 {
		t.Fatalf("fetch %+v, want every path from the origin after the peer failed verification", r)
	}
	if got := metricValue(t, srv, "k3snix_fetch_verify_failures"); got != "3" {
		t.Fatalf("k3snix_fetch_verify_failures %s, want 3", got)
	}
	f.assertSameNARs(t, dir)
}

func TestFetchFailsWithoutSourceAndLeavesNoPartialPath(t *testing.T) {
	f := newFetchFixture(t)
	dir := t.TempDir()
	_, srv := f.serve(t, dir, nil)
	code, _, raw := postFetch(t, srv.URL, `{"label":"app"}`)
	if code != http.StatusBadGateway {
		t.Fatalf("fetch without sources: status %d, want 502: %s", code, raw)
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != 0 {
		t.Fatalf("store holds %d entries after a failed fetch", len(ents))
	}
	if got := metricValue(t, srv, "k3snix_fetch_failures"); got != "1" {
		t.Fatalf("k3snix_fetch_failures %s, want 1", got)
	}
	if code, _, _ := postFetch(t, srv.URL, `{"label":"nope"}`); code != http.StatusNotFound {
		t.Fatalf("unknown closure: status %d, want 404", code)
	}
}

func TestFetchCoalescesConcurrentRequests(t *testing.T) {
	f := newFetchFixture(t)
	var narGets atomic.Int32
	gate := make(chan struct{})
	_, origin := f.serve(t, f.origin, func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/nar/") {
				narGets.Add(1)
				<-gate
			}
			h.ServeHTTP(w, r)
		})
	})
	dir := t.TempDir()
	n, srv := f.serve(t, dir, nil)
	n.Origin = origin.URL

	const callers = 4
	var wg sync.WaitGroup
	results := make([]FetchResp, callers)
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			code, r, raw := postFetch(t, srv.URL, `{"label":"app"}`)
			if code != http.StatusOK {
				t.Errorf("caller %d: status %d: %s", i, code, raw)
			}
			results[i] = r
		}()
	}
	// Let the callers pile up on the first level (libc) before releasing.
	for metricValue(t, srv, "k3snix_fetch_requests") != itoa(callers) {
		time.Sleep(time.Millisecond)
	}
	close(gate)
	wg.Wait()

	if got := narGets.Load(); got != 3 {
		t.Fatalf("origin served %d NARs for %d concurrent fetches, want 3", got, callers)
	}
	var fetched int64
	for _, r := range results {
		fetched += r.FetchedBytes
	}
	if fetched != f.totalBytes() {
		t.Fatalf("callers report %d fetched bytes together, want %d (counted once)", fetched, f.totalBytes())
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != itoa(f.totalBytes()) {
		t.Fatalf("k3snix_origin_bytes %s, want %d", got, f.totalBytes())
	}
	f.assertSameNARs(t, dir)
}

func TestReferenceLevels(t *testing.T) {
	byPath := map[string]closure.Path{
		"/s/app":  {References: []string{"/s/app", "/s/lib", "/s/conf"}},
		"/s/lib":  {References: []string{"/s/libc"}},
		"/s/libc": {},
		"/s/conf": {},
		"/s/a":    {References: []string{"/s/b"}}, // a cycle never hangs
		"/s/b":    {References: []string{"/s/a"}},
	}
	levels := referenceLevels([]string{"/s/app", "/s/lib", "/s/conf", "/s/a", "/s/b"}, byPath)
	pos := map[string]int{}
	for i, l := range levels {
		for _, p := range l {
			pos[p] = i
		}
	}
	if len(pos) != 5 {
		t.Fatalf("levels %v lost paths", levels)
	}
	// libc is present already, so lib waits on nothing.
	if !(pos["/s/lib"] < pos["/s/app"] && pos["/s/conf"] < pos["/s/app"]) {
		t.Fatalf("levels %v: app is not after its references", levels)
	}
}

func TestFetchNeedsCatalogHashes(t *testing.T) {
	// A closure without narHash cannot be verified, so it is never fetched.
	catDir := t.TempDir()
	mustFile(t, filepath.Join(catDir, "e2e.json"), []byte(`{"label": "e2e", "paths": [
		{"path": "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a", "narSize": 10}]}`))
	s := newServer(t, t.TempDir())
	if err := s.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	s.Materializer = NixMaterializer{}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	code, _, raw := postFetch(t, srv.URL, `{"label":"e2e"}`)
	if code != http.StatusNotFound || !strings.Contains(raw, "narHash") {
		t.Fatalf("status %d, want 404 naming narHash: %s", code, raw)
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestFetchIntoNixStore(t *testing.T) {
	if _, err := exec.LookPath("nix"); err != nil {
		t.Skip("nix not on PATH")
	}
	f := newFetchFixture(t)
	_, origin := f.serve(t, f.origin, nil)

	// The agent lists the chroot store's /nix/store; nix copy writes it.
	root := t.TempDir()
	storeDir := filepath.Join(root, "nix", "store")
	mustDir(t, storeDir)
	n, srv := f.serve(t, storeDir, nil)
	n.Origin = origin.URL
	n.Materializer = NixMaterializer{To: root}
	t.Cleanup(func() { chmodTree(root) })

	code, r, raw := postFetch(t, srv.URL, `{"label":"app"}`)
	if code != http.StatusOK {
		t.Fatalf("fetch status %d: %s", code, raw)
	}
	if r.FetchedBytes != f.totalBytes() {
		t.Fatalf("fetch %+v, want %d bytes", r, f.totalBytes())
	}
	f.assertSameNARs(t, storeDir)
	// nix registered the paths with the catalog's hash and references.
	out, err := exec.Command("nix", "--extra-experimental-features", "nix-command",
		"path-info", "--store", root, "--json", "/nix/store/"+fAppName).Output()
	if err != nil {
		t.Fatalf("nix path-info: %v", err)
	}
	if !strings.Contains(string(out), "/nix/store/"+fLibName) {
		t.Fatalf("nix path-info lacks the lib reference: %s", out)
	}
}
