package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeOrigin records every request it serves and answers /nix-cache-info
// with a fixed body.
type fakeOrigin struct {
	mu   sync.Mutex
	hits []string // "METHOD PATH" per request
	body string
	srv  *httptest.Server
}

func newFakeOrigin(t *testing.T) *fakeOrigin {
	t.Helper()
	f := &fakeOrigin{
		body: "StoreDir: /nix/store\nWantMassQuery: 1\nPriority: 40\n",
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/nix-cache-info", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.hits = append(f.hits, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "text/x-nix-cache-info")
		fmt.Fprint(w, f.body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOrigin) hitCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.hits)
}

func metricValue(t *testing.T, srv *httptest.Server, name string) string {
	t.Helper()
	resp, err := http.Get(srv.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	for _, line := range strings.Split(string(body), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == name {
			return fields[1]
		}
	}
	t.Fatalf("metric %s not found in:\n%s", name, body)
	return ""
}

func TestOriginProbeCounters(t *testing.T) {
	origin := newFakeOrigin(t)
	dir := t.TempDir()
	mustDir(t, filepath.Join(dir, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a"))
	s := newServer(t, dir)
	s.Origin = origin.srv.URL
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "0" {
		t.Fatalf("fresh origin_requests = %s, want 0", got)
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != "0" {
		t.Fatalf("fresh origin_bytes = %s, want 0", got)
	}

	// Startup, healthz and missing-bytes must never contact the origin.
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("healthz status %d", resp.StatusCode)
	}
	if code, _, _ := postMissing(t, srv.URL,
		`{"paths":[{"path":"/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-a","narSize":10}]}`); code != 200 {
		t.Fatalf("missing_bytes status %d", code)
	}
	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "0" {
		t.Fatalf("origin_requests after healthz+missing = %s, want 0", got)
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != "0" {
		t.Fatalf("origin_bytes after healthz+missing = %s, want 0", got)
	}
	if n := origin.hitCount(); n != 0 {
		t.Fatalf("fake origin hit %d times before any probe", n)
	}

	// The probe itself performs exactly one GET of {origin}/nix-cache-info.
	probe, err := http.Post(srv.URL+"/v1/origin_probe", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	probeBody, _ := io.ReadAll(probe.Body)
	probe.Body.Close()
	if probe.StatusCode != 200 {
		t.Fatalf("probe status %d body %s", probe.StatusCode, probeBody)
	}
	if n := origin.hitCount(); n != 1 {
		t.Fatalf("fake origin hits = %d, want 1", n)
	}
	origin.mu.Lock()
	hit := origin.hits[0]
	origin.mu.Unlock()
	if hit != "GET /nix-cache-info" {
		t.Fatalf("origin request %q, want GET /nix-cache-info", hit)
	}

	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "1" {
		t.Fatalf("origin_requests after probe = %s, want 1", got)
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != strconv.Itoa(len(origin.body)) {
		t.Fatalf("origin_bytes after probe = %s, want %s", got, strconv.Itoa(len(origin.body)))
	}

	// A second probe moves both counters again.
	probe2, err := http.Post(srv.URL+"/v1/origin_probe", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, probe2.Body)
	probe2.Body.Close()
	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "2" {
		t.Fatalf("origin_requests after second probe = %s, want 2", got)
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != strconv.Itoa(2*len(origin.body)) {
		t.Fatalf("origin_bytes after second probe = %s, want %s", got, strconv.Itoa(2*len(origin.body)))
	}
}

func TestOriginProbeUnconfigured(t *testing.T) {
	s := newServer(t, t.TempDir())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/origin_probe", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("probe without origin status %d, want 400", resp.StatusCode)
	}
	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "0" {
		t.Fatalf("origin_requests = %s, want 0", got)
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != "0" {
		t.Fatalf("origin_bytes = %s, want 0", got)
	}

	// GET is not a probe.
	get, err := http.Get(srv.URL + "/v1/origin_probe")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, get.Body)
	get.Body.Close()
	if get.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET probe status %d, want 405", get.StatusCode)
	}
}

func TestOriginProbeErrorBodyIsBounded(t *testing.T) {
	// An origin that answers 503 with an endless body must not hold the
	// probe until the client timeout: the drain stops at originMaxBody.
	stop := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		chunk := make([]byte, 32<<10)
		for {
			select {
			case <-stop:
				return
			case <-r.Context().Done():
				return
			default:
			}
			if _, err := w.Write(chunk); err != nil {
				return
			}
		}
	}))
	defer upstream.Close()
	defer close(stop)

	s := newServer(t, t.TempDir())
	s.Origin = upstream.URL
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	start := time.Now()
	resp, err := http.Post(srv.URL+"/v1/origin_probe", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d against a 503 origin, want 502", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > originClientTimeout/2 {
		t.Fatalf("probe took %v draining an endless error body", elapsed)
	}
}
