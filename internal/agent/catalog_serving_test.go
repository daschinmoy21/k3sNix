package agent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"k3snix/internal/nar"
	"k3snix/internal/nixbase32"
)

const (
	catHash  = "cccccccccccccccccccccccccccccccc"
	catName  = catHash + "-cat"
	depHash  = "dddddddddddddddddddddddddddddddd"
	depName  = depHash + "-dep"
	freeHash = "ffffffffffffffffffffffffffffffff"
)

// catalogStore seeds two store paths, cat (referring to dep) and an
// uncatalogued one, and writes a catalog whose cat and dep entries carry
// the real NAR hash and size. It returns the store dir and the catalog dir.
func catalogStore(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{catName, depName, freeHash + "-free"} {
		mustDir(t, filepath.Join(dir, name))
		mustFile(t, filepath.Join(dir, name, "f"), []byte("contents of "+name))
	}
	type entry struct {
		Path       string   `json:"path"`
		NarSize    int      `json:"narSize"`
		NarHash    string   `json:"narHash"`
		References []string `json:"references"`
	}
	var paths []entry
	for _, e := range []struct {
		name string
		refs []string
	}{
		{catName, []string{"/nix/store/" + depName, "/nix/store/" + catName}},
		{depName, nil},
	} {
		var buf bytes.Buffer
		if err := nar.Write(&buf, filepath.Join(dir, e.name)); err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(buf.Bytes())
		paths = append(paths, entry{
			Path:       "/nix/store/" + e.name,
			NarSize:    buf.Len(),
			NarHash:    "sha256-" + base64.StdEncoding.EncodeToString(sum[:]), // SRI, as nix path-info writes it
			References: e.refs,
		})
	}
	doc, err := json.Marshal(map[string]any{"label": "cat", "paths": paths})
	if err != nil {
		t.Fatal(err)
	}
	catDir := t.TempDir()
	mustFile(t, filepath.Join(catDir, "cat.json"), doc)
	return dir, catDir
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, body
}

func TestNarinfoFromCatalogNeedsNoDump(t *testing.T) {
	dir, catDir := catalogStore(t)
	s := newServer(t, dir)
	if err := s.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	s.DumpFunc = func(path string, w io.Writer) error {
		calls.Add(1)
		return nar.Write(w, path)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, info := get(t, srv.URL+"/"+catHash+".narinfo")
	if code != 200 {
		t.Fatalf("narinfo status %d: %s", code, info)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("catalog-backed narinfo dumped %d times, want 0", n)
	}
	fields := parseNarinfo(t, info)
	if got, want := fields["References"], catName+" "+depName; got != want {
		t.Fatalf("References %q, want %q (catalog refs, sorted)", got, want)
	}

	// The NAR the agent then serves is exactly what the narinfo promised.
	code, body := get(t, srv.URL+"/nar/"+catHash+".nar")
	if code != 200 {
		t.Fatalf("nar status %d: %s", code, body)
	}
	sum := sha256.Sum256(body)
	if got := "sha256:" + nixbase32.EncodeToString(sum[:]); got != fields["NarHash"] {
		t.Fatalf("NAR hash %s, narinfo NarHash %s", got, fields["NarHash"])
	}
	if strconv.Itoa(len(body)) != fields["NarSize"] {
		t.Fatalf("NAR size %d, narinfo NarSize %s", len(body), fields["NarSize"])
	}
}

func TestNARRefusedWhenDumpDisagreesWithCatalog(t *testing.T) {
	dir, catDir := catalogStore(t)
	// The store content changes after the catalog was written.
	mustFile(t, filepath.Join(dir, catName, "f"), []byte("tampered"))
	s := newServer(t, dir)
	if err := s.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, body := get(t, srv.URL+"/nar/"+catHash+".nar")
	if code != http.StatusInternalServerError {
		t.Fatalf("mismatching NAR status %d, want 500: %q", code, body)
	}
	if bytes.Contains(body, []byte("nix-archive-1")) {
		t.Fatal("served NAR bytes that disagree with the catalog")
	}
	if got := metricValue(t, srv, "k3snix_nar_hash_mismatches"); got != "1" {
		t.Fatalf("k3snix_nar_hash_mismatches = %s, want 1", got)
	}
	if got := metricValue(t, srv, "k3snix_nar_served"); got != "0" {
		t.Fatalf("k3snix_nar_served = %s, want 0", got)
	}
	// An uncatalogued path still serves: there is nothing to disagree with.
	if code, _ := get(t, srv.URL+"/nar/"+freeHash+".nar"); code != 200 {
		t.Fatalf("uncatalogued NAR status %d, want 200", code)
	}
}

func TestCatalogOnly(t *testing.T) {
	dir, catDir := catalogStore(t)
	s := newServer(t, dir)
	if err := s.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	s.CatalogOnly = true
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	for _, u := range []string{"/" + catHash + ".narinfo", "/nar/" + catHash + ".nar", "/" + depHash + ".narinfo"} {
		if code, body := get(t, srv.URL+u); code != 200 {
			t.Fatalf("GET %s = %d, want 200: %s", u, code, body)
		}
	}
	for _, u := range []string{"/" + freeHash + ".narinfo", "/nar/" + freeHash + ".nar"} {
		if code, _ := get(t, srv.URL+u); code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404 for a path outside the catalog", u, code)
		}
	}
}

func TestNarinfoSignature(t *testing.T) {
	dir, catDir := catalogStore(t)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ParseSigningKey("k3snix-test-1:" + base64.StdEncoding.EncodeToString(priv))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := key.PublicKey(), "k3snix-test-1:"+base64.StdEncoding.EncodeToString(pub); got != want {
		t.Fatalf("PublicKey %q, want %q", got, want)
	}
	s := newServer(t, dir)
	if err := s.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	s.SignKey = key
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	_, info := get(t, srv.URL+"/"+catHash+".narinfo")
	fields := parseNarinfo(t, info)
	name, b64, ok := strings.Cut(fields["Sig"], ":")
	if !ok || name != "k3snix-test-1" {
		t.Fatalf("Sig %q", fields["Sig"])
	}
	sig, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatal(err)
	}
	// Built independently of narFingerprint, in the form nix signs.
	msg := fmt.Sprintf("1;%s;%s;%s;%s,%s", fields["StorePath"], fields["NarHash"], fields["NarSize"],
		filepath.Join(dir, catName), filepath.Join(dir, depName))
	if !ed25519.Verify(pub, []byte(msg), sig) {
		t.Fatalf("signature does not verify over %q", msg)
	}
}

func TestParseSigningKeyErrors(t *testing.T) {
	for _, in := range []string{"", "noname", ":AAAA", "k:not-base64!", "k:" + base64.StdEncoding.EncodeToString([]byte("short"))} {
		if _, err := ParseSigningKey(in); err == nil {
			t.Errorf("ParseSigningKey(%q) accepted a bad key", in)
		}
	}
	file := filepath.Join(t.TempDir(), "key")
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := os.WriteFile(file, []byte("k:"+base64.StdEncoding.EncodeToString(priv)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSigningKey(file); err != nil {
		t.Fatalf("LoadSigningKey with trailing newline: %v", err)
	}
}

func postProbe(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url+"/v1/origin_probe", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, out
}

func TestOriginNARProbe(t *testing.T) {
	// The origin is another agent in catalog-only mode, as deployed.
	dir, catDir := catalogStore(t)
	origin := newServer(t, dir)
	if err := origin.LoadCatalog(catDir); err != nil {
		t.Fatal(err)
	}
	origin.CatalogOnly = true
	originSrv := httptest.NewServer(origin.Handler())
	defer originSrv.Close()

	s := newServer(t, t.TempDir())
	s.Origin = originSrv.URL
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	code, body := postProbe(t, srv.URL, `{"hash":"`+catHash+`"}`)
	if code != 200 {
		t.Fatalf("probe status %d: %s", code, body)
	}
	var res OriginNARProbe
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	_, info := get(t, originSrv.URL+"/"+catHash+".narinfo")
	_, narBody := get(t, originSrv.URL+"/nar/"+catHash+".nar")
	if !res.Verified || res.NarSize != int64(len(narBody)) || res.Bytes != int64(len(info)+len(narBody)) {
		t.Fatalf("probe result %+v, want verified, nar %d, bytes %d", res, len(narBody), len(info)+len(narBody))
	}
	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "2" {
		t.Fatalf("origin_requests = %s, want 2 (narinfo and NAR)", got)
	}
	if got := metricValue(t, srv, "k3snix_origin_bytes"); got != strconv.FormatInt(res.Bytes, 10) {
		t.Fatalf("origin_bytes = %s, want %d", got, res.Bytes)
	}
	// The probe never materializes anything locally.
	if got := metricValue(t, srv, "k3snix_store_paths"); got != "0" {
		t.Fatalf("store_paths = %s after a probe, want 0", got)
	}

	// A path the origin does not serve fails without moving the counters.
	if code, _ := postProbe(t, srv.URL, `{"hash":"`+freeHash+`"}`); code != http.StatusBadGateway {
		t.Fatalf("probe of an unserved path status %d, want 502", code)
	}
	if code, _ := postProbe(t, srv.URL, `{"hash":"short"}`); code != http.StatusBadRequest {
		t.Fatalf("probe with a bad hash status %d, want 400", code)
	}
	if got := metricValue(t, srv, "k3snix_origin_requests"); got != "2" {
		t.Fatalf("origin_requests = %s after failed probes, want 2", got)
	}
}

func TestOriginNARProbeRejectsBadNAR(t *testing.T) {
	const narinfo = "StorePath: /nix/store/" + catName + "\nURL: nar/" + catHash + ".nar\nCompression: none\n" +
		"NarHash: sha256:0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73\nNarSize: 5\n" // sha256 of ""
	for _, tc := range []struct {
		name, nar string
	}{
		{"wrong bytes", "hello"},
		{"too long", "hello world"},
		{"too short", "hi"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mux := http.NewServeMux()
			mux.HandleFunc("/"+catHash+".narinfo", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, narinfo) })
			mux.HandleFunc("/nar/"+catHash+".nar", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.nar) })
			upstream := httptest.NewServer(mux)
			defer upstream.Close()

			s := newServer(t, t.TempDir())
			s.Origin = upstream.URL
			srv := httptest.NewServer(s.Handler())
			defer srv.Close()

			if code, body := postProbe(t, srv.URL, `{"hash":"`+catHash+`"}`); code != http.StatusBadGateway {
				t.Fatalf("status %d, want 502: %s", code, body)
			}
			if got := metricValue(t, srv, "k3snix_origin_verify_failures"); got != "1" {
				t.Fatalf("verify_failures = %s, want 1", got)
			}
			if got := metricValue(t, srv, "k3snix_origin_bytes"); got != "0" {
				t.Fatalf("origin_bytes = %s, want 0", got)
			}
		})
	}
}

func TestOriginNARProbeSizeCap(t *testing.T) {
	var narHits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/"+catHash+".narinfo", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "URL: nar/x.nar\nNarHash: sha256:0mdqa9w1p6cmli6976v4wi0sw9r4p5prkj7lzfd1877wk11c9c73\nNarSize: 2048\n")
	})
	mux.HandleFunc("/nar/x.nar", func(w http.ResponseWriter, r *http.Request) { narHits.Add(1) })
	upstream := httptest.NewServer(mux)
	defer upstream.Close()

	s := newServer(t, t.TempDir())
	s.Origin = upstream.URL
	s.MaxOriginNARBytes = 1024
	srv := httptest.NewServer(s.Handler())
	defer srv.Close()

	if code, _ := postProbe(t, srv.URL, `{"hash":"`+catHash+`"}`); code != http.StatusBadGateway {
		t.Fatalf("status %d, want 502 for a NAR over the cap", code)
	}
	if n := narHits.Load(); n != 0 {
		t.Fatalf("NAR fetched %d times despite exceeding the cap", n)
	}
}
