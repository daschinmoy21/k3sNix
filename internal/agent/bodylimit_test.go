package agent

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"k3snix/internal/closure"
)

func TestMissingBytesLargeClosure(t *testing.T) {
	// A system-sized closure sent as a paths list is several MiB; it must
	// be answered, not rejected as too large.
	const n = 8000
	req := MissingReq{Paths: make([]closure.Path, n)}
	for i := range req.Paths {
		refs := make([]string, 8)
		for j := range refs {
			refs[j] = fmt.Sprintf("/nix/store/%032d-ref-%d", (i+j+1)%n, j)
		}
		req.Paths[i] = closure.Path{
			Path:       fmt.Sprintf("/nix/store/%032d-pkg-%d", i, i),
			NarSize:    1024,
			NarHash:    "sha256:1b8m03r63zqhnjf7l5wnldhh7c134ap5vpj0850ymkq1iyzicy5s",
			References: refs,
		}
	}
	body := mustMarshal(t, req)
	if len(body) <= 1<<20 {
		t.Fatalf("request is only %d bytes; the test needs a multi-MiB body", len(body))
	}

	s := newServer(t, t.TempDir())
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()
	status, out, raw := postMissing(t, srv.URL, string(body))
	if status != http.StatusOK {
		t.Fatalf("status %d for a %d-byte request: %s", status, len(body), raw)
	}
	if got := decodeMissing(t, out); got.TotalPaths != n {
		t.Fatalf("total_paths = %d, want %d", got.TotalPaths, n)
	}
}

func TestMissingBytesConfiguredLimit(t *testing.T) {
	s := newServer(t, t.TempDir())
	s.MaxRequestBytes = 64
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	body := `{"label":"` + strings.Repeat("x", 64) + `"}`
	resp, err := http.Post(srv.URL+"/v1/missing_bytes", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
}
