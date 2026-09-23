package agent

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"k3snix/internal/nar"
	"k3snix/internal/nixbase32"
)

func newServer(t *testing.T, storeDir string) *Server {
	t.Helper()
	s := New(storeDir)
	s.Store.TTL = 0
	return s
}

func mustDir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func postMissing(t *testing.T, url, body string) (int, map[string]json.RawMessage, []byte) {
	t.Helper()
	resp, err := http.Post(url+"/v1/missing_bytes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]json.RawMessage{}
	json.Unmarshal(raw, &out)
	return resp.StatusCode, out, raw
}

func decodeMissing(t *testing.T, out map[string]json.RawMessage) MissingResp {
	t.Helper()
	var resp MissingResp
	if err := json.Unmarshal(mustMarshal(t, out), &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestMissingBytesHTTP(t *testing.T) {
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-have"))
	s := newServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, out, _ := postMissing(t, srv.URL,
		`{"paths":[{"path":"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-have","narSize":10},
		          {"path":"/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-miss","narSize":40}]}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	resp := decodeMissing(t, out)
	if resp.Bytes != 40 || resp.HaveBytes != 10 || resp.TotalBytes != 50 ||
		resp.HavePaths != 1 || resp.TotalPaths != 2 {
		t.Fatalf("totals do not add up: %+v", resp)
	}
	var missing []string
	json.Unmarshal(out["missing"], &missing)
	if len(missing) != 1 || missing[0] != "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-miss" {
		t.Fatalf("missing %v", missing)
	}
	if _, ok := out["have"]; ok {
		t.Fatalf("response must not include the have-set: %v", out)
	}
	if _, ok := out["estimate_ms"]; !ok {
		t.Fatalf("response must include estimate_ms: %v", out)
	}

	if respBytes, ok := out["have_bytes"]; !ok || string(respBytes) != "10" {
		t.Fatalf("have_bytes %s", respBytes)
	}

	// Unknown fingerprint and label -> 404 with "unknown closure".
	for _, body := range []string{`{"id":"nope"}`, `{"label":"nope"}`} {
		code, _, raw := postMissing(t, srv.URL, body)
		if code != 404 || !strings.Contains(string(raw), "unknown closure") {
			t.Fatalf("status %d body %s", code, raw)
		}
	}

	// Neither paths nor id nor label -> 400.
	if code, _, _ = postMissing(t, srv.URL, `{}`); code != 400 {
		t.Fatalf("empty request status %d, want 400", code)
	}

	// POST only.
	respGet, err := http.Get(srv.URL + "/v1/missing_bytes")
	if err != nil {
		t.Fatal(err)
	}
	respGet.Body.Close()
	if respGet.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET missing_bytes status %d, want 405", respGet.StatusCode)
	}
}

func TestMissingBytesLabelLookup(t *testing.T) {
	store := t.TempDir()
	mustDir(t, filepath.Join(store, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-svc"))
	catDir := t.TempDir()
	mustFile(t, filepath.Join(catDir, "web.json"), []byte(
		`{"label":"web","paths":[
		  {"path":"/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-svc","narSize":5},
		  {"path":"/nix/store/cccccccccccccccccccccccccccccccc-nope","narSize":9}]}`))
	s := newServer(t, store)
	if err := s.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, out, _ := postMissing(t, srv.URL, `{"label":"web"}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	resp := decodeMissing(t, out)
	if resp.Bytes != 9 || resp.HaveBytes != 5 || resp.TotalBytes != 14 {
		t.Fatalf("%+v", resp)
	}
}

func TestBasenameAliasHTTP(t *testing.T) {
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "ffffffffffffffffffffffffffffffff-real"))
	s := newServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	// The closure says /nix/store, the store is a temp dir.
	code, out, _ := postMissing(t, srv.URL,
		`{"paths":[{"path":"/nix/store/ffffffffffffffffffffffffffffffff-real","narSize":7}]}`)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	resp := decodeMissing(t, out)
	if resp.HaveBytes != 7 || resp.Bytes != 0 || resp.HavePaths != 1 {
		t.Fatalf("alias not matched: %+v", resp)
	}

	narResp, err := http.Get(srv.URL + "/ffffffffffffffffffffffffffffffff.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	defer narResp.Body.Close()
	info, _ := io.ReadAll(narResp.Body)
	if narResp.StatusCode != 200 {
		t.Fatalf("narinfo status %d body %s", narResp.StatusCode, info)
	}
	if got := parseNarinfo(t, info)["StorePath"]; got != filepath.Join(dir, "ffffffffffffffffffffffffffffffff-real") {
		t.Fatalf("StorePath %q", got)
	}
}

func TestCoalescedDump(t *testing.T) {
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "dddddddddddddddddddddddddddddddd-pkg"))
	s := newServer(t, dir)

	body := []byte("nix-archive-1:coalesced-payload")
	var calls atomic.Int32
	started := make(chan struct{})
	var startOnce sync.Once
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	s.DumpFunc = func(path string, w io.Writer) error {
		calls.Add(1)
		startOnce.Do(func() { close(started) })
		<-release
		_, err := w.Write(body)
		return err
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	type result struct {
		status int
		clen   string
		body   []byte
	}
	get := func(done chan<- result) {
		resp, err := http.Get(srv.URL + "/nar/dddddddddddddddddddddddddddddddd.nar")
		if err != nil {
			done <- result{status: -1}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		done <- result{resp.StatusCode, resp.Header.Get("Content-Length"), b}
	}

	r1 := make(chan result, 1)
	go get(r1)
	<-started                         // the leader is inside DumpFunc, holding the call
	time.Sleep(50 * time.Millisecond) // let the second request pile onto it
	r2 := make(chan result, 1)
	go get(r2)
	time.Sleep(50 * time.Millisecond)
	releaseOnce.Do(func() { close(release) })

	res := []result{<-r1, <-r2}
	for _, r := range res {
		if r.status != 200 {
			t.Fatalf("status %d", r.status)
		}
		if r.clen == "" {
			t.Fatalf("Content-Length not set")
		}
		if !bytes.Equal(r.body, body) {
			t.Fatalf("body %q", r.body)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("dump calls = %d, want 1", n)
	}
	if res[0].clen != res[1].clen {
		t.Fatalf("Content-Length differs: %s vs %s", res[0].clen, res[1].clen)
	}
}

func TestDumpFailureRetries(t *testing.T) {
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee-pkg"))
	s := newServer(t, dir)

	body := []byte("good-nar-payload")
	var calls atomic.Int32
	var fail atomic.Bool
	fail.Store(true)
	s.DumpFunc = func(path string, w io.Writer) error {
		calls.Add(1)
		if fail.Load() {
			return errors.New("dump exploded")
		}
		_, err := w.Write(body)
		return err
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nar/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.nar")
	if err != nil {
		t.Fatal(err)
	}
	failed, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode == 200 {
		t.Fatalf("failed dump served as 200 (body %q)", failed)
	}
	if bytes.Contains(failed, body) {
		t.Fatalf("failed dump served NAR bytes: %q", failed)
	}

	fail.Store(false)
	resp2, err := http.Get(srv.URL + "/nar/eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee.nar")
	if err != nil {
		t.Fatal(err)
	}
	good, _ := io.ReadAll(resp2.Body)
	resp2.Body.Close()
	if resp2.StatusCode != 200 || !bytes.Equal(good, body) {
		t.Fatalf("retry status %d body %q", resp2.StatusCode, good)
	}
	if resp2.Header.Get("Content-Length") == "" {
		t.Fatalf("Content-Length not set")
	}
	if n := calls.Load(); n != 2 {
		t.Fatalf("dump calls = %d, want 2", n)
	}
}

func TestNarinfoMatchesNARBody(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "cccccccccccccccccccccccccccccccc-tiny")
	mustDir(t, p)
	mustFile(t, filepath.Join(p, "f"), []byte("hello-nar"))
	s := newServer(t, dir)
	var calls atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		calls.Add(1)
		return nar.Write(w, path)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	narResp, err := http.Get(srv.URL + "/nar/cccccccccccccccccccccccccccccccc.nar")
	if err != nil {
		t.Fatal(err)
	}
	narBody, _ := io.ReadAll(narResp.Body)
	narResp.Body.Close()
	if narResp.StatusCode != 200 {
		t.Fatalf("nar status %d", narResp.StatusCode)
	}
	if !bytes.Contains(narBody, []byte("nix-archive-1")) || !bytes.Contains(narBody, []byte("hello-nar")) {
		t.Fatalf("bad nar body %q", narBody)
	}
	if got := narResp.Header.Get("Content-Type"); got != "application/x-nix-nar" {
		t.Fatalf("nar content type %q", got)
	}

	infoResp, err := http.Get(srv.URL + "/cccccccccccccccccccccccccccccccc.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := io.ReadAll(infoResp.Body)
	infoResp.Body.Close()
	if infoResp.StatusCode != 200 {
		t.Fatalf("narinfo status %d body %s", infoResp.StatusCode, info)
	}
	if got := infoResp.Header.Get("Content-Type"); got != "text/x-nix-narinfo" {
		t.Fatalf("narinfo content type %q", got)
	}

	fields := parseNarinfo(t, info)
	sum := sha256.Sum256(narBody)
	wantHash := "sha256:" + nixbase32.EncodeToString(sum[:])
	if fields["NarHash"] != wantHash {
		t.Fatalf("NarHash %q, want %q", fields["NarHash"], wantHash)
	}
	if fields["NarSize"] != strconv.Itoa(len(narBody)) {
		t.Fatalf("NarSize %q, want %d", fields["NarSize"], len(narBody))
	}
	if fields["FileHash"] != wantHash {
		t.Fatalf("FileHash %q, want %q", fields["FileHash"], wantHash)
	}
	if fields["FileSize"] != strconv.Itoa(len(narBody)) {
		t.Fatalf("FileSize %q, want %d", fields["FileSize"], len(narBody))
	}
	if fields["StorePath"] != p {
		t.Fatalf("StorePath %q", fields["StorePath"])
	}
	if fields["URL"] != "nar/cccccccccccccccccccccccccccccccc.nar" {
		t.Fatalf("URL %q", fields["URL"])
	}
	if fields["Compression"] != "none" {
		t.Fatalf("Compression %q", fields["Compression"])
	}

	// The successful dump stays cached: narinfo must not have dumped again.
	if n := calls.Load(); n != 1 {
		t.Fatalf("dump calls = %d, want 1", n)
	}
}

func TestDumpMissingTempDir(t *testing.T) {
	// Minimal container images ship no /tmp; the dump must create the
	// system temp dir instead of failing with ENOENT.
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b"))
	mustFile(t, filepath.Join(dir, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-b", "payload"), []byte("seeded"))
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "no-such-tmpdir"))
	s := newServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.narinfo")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("narinfo status %d body %s", resp.StatusCode, info)
	}
	fields := parseNarinfo(t, info)
	if fields["StorePath"] == "" || fields["NarHash"] == "" {
		t.Fatalf("bad narinfo %q", info)
	}

	narResp, err := http.Get(srv.URL + "/nar/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb.nar")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(narResp.Body)
	narResp.Body.Close()
	if narResp.StatusCode != 200 || !bytes.Contains(body, []byte("seeded")) {
		t.Fatalf("nar status %d body %q", narResp.StatusCode, body)
	}
}

func TestNarinfoNotFound(t *testing.T) {
	s := newServer(t, t.TempDir())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, path := range []string{
		"/ffffffffffffffffffffffffffffffff.narinfo",
		"/nar/ffffffffffffffffffffffffffffffff.nar",
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 404 {
			t.Fatalf("%s status %d, want 404", path, resp.StatusCode)
		}
	}
}

func TestCacheInfoHealthMetrics(t *testing.T) {
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a"))
	s := newServer(t, dir)
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/nix-cache-info")
	if err != nil {
		t.Fatal(err)
	}
	info, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/x-nix-cache-info" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	body := string(info)
	for _, want := range []string{"StoreDir: " + dir, "WantMassQuery: 1", "Priority: 30"} {
		if !strings.Contains(body, want) {
			t.Fatalf("nix-cache-info missing %q: %s", want, body)
		}
	}

	resp, err = http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	metrics, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	m := string(metrics)
	for _, want := range []string{"k3snix_nar_served", "k3snix_nar_bytes_served", "k3snix_store_paths"} {
		if !strings.Contains(m, want) {
			t.Fatalf("metrics missing %q: %s", want, m)
		}
	}
}

func parseNarinfo(t *testing.T, body []byte) map[string]string {
	t.Helper()
	fields := map[string]string{}
	for _, line := range strings.Split(string(body), "\n") {
		if i := strings.Index(line, ": "); i >= 0 {
			fields[line[:i]] = line[i+2:]
		}
	}
	return fields
}
