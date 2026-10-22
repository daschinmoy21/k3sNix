package extender

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"k3snix/internal/agent"
	"k3snix/internal/closure"
)

const manifest = `{"label": "web", "root": "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "paths": [
	{"path": "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "narSize": 100},
	{"path": "/nix/store/bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib", "narSize": 300},
	{"path": "/nix/store/cccccccccccccccccccccccccccccccc-libc", "narSize": 600}
]}`

const root = "/nix/store/aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web"

func catalogDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "web.json"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startAgent runs a real node agent whose store holds the named paths. An
// agent started without a catalog only answers path-list queries.
func startAgent(t *testing.T, catalog string, have ...string) string {
	t.Helper()
	store := t.TempDir()
	for _, name := range have {
		if err := os.Mkdir(filepath.Join(store, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	a := agent.New(store)
	a.Store.TTL = 0
	a.Log = log.New(io.Discard, "", 0)
	if catalog != "" {
		if err := a.LoadCatalog(catalog); err != nil {
			t.Fatal(err)
		}
	}
	srv := httptest.NewServer(a.Handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func newExtender(t *testing.T, catalog string) (*Server, *bytes.Buffer) {
	t.Helper()
	cat, err := closure.LoadCatalog(catalog)
	if err != nil {
		t.Fatal(err)
	}
	s := New(cat)
	s.AgentURL = "http://{node}"
	var logs bytes.Buffer
	s.Log = log.New(&logs, "", 0)
	return s, &logs
}

func podJSON(annotations map[string]string) Pod {
	return Pod{Metadata: ObjectMeta{Name: "p", Namespace: "default", Annotations: annotations}}
}

func call(t *testing.T, h http.Handler, verb string, args any) []byte {
	t.Helper()
	body, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+verb, bytes.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("%s: status %d: %s", verb, rec.Code, rec.Body)
	}
	return rec.Body.Bytes()
}

func prioritize(t *testing.T, s *Server, args Args) map[string]int64 {
	t.Helper()
	var list []HostPriority
	if err := json.Unmarshal(call(t, s.Handler(), "prioritize", args), &list); err != nil {
		t.Fatal(err)
	}
	out := map[string]int64{}
	for _, hp := range list {
		out[hp.Host] = hp.Score
	}
	return out
}

func TestPrioritizeScoresByMissingBytes(t *testing.T) {
	cat := catalogDir(t)
	warm := startAgent(t, cat,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib", "cccccccccccccccccccccccccccccccc-libc")
	libc := startAgent(t, cat, "cccccccccccccccccccccccccccccccc-libc")
	cold := startAgent(t, cat)
	// An agent without the catalog still answers through the path list.
	nocat := startAgent(t, "", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib")

	s, logs := newExtender(t, cat)
	names := []string{warm, libc, cold, nocat}
	got := prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), NodeNames: &names})
	want := map[string]int64{warm: 10, libc: 6, cold: 0, nocat: 3}
	for node, score := range want {
		if got[node] != score {
			t.Errorf("node %s score %d, want %d", node, got[node], score)
		}
	}

	// One trace line per cycle names the closure and every node's bytes.
	line := strings.TrimPrefix(strings.TrimSpace(logs.String()), "prioritize ")
	var tr Trace
	if err := json.Unmarshal([]byte(line), &tr); err != nil {
		t.Fatalf("trace %q: %v", logs.String(), err)
	}
	if tr.Label != "web" || tr.Closure == "" || len(tr.Nodes) != 4 {
		t.Fatalf("trace %+v", tr)
	}
	for _, n := range tr.Nodes {
		if n.Node == libc && (n.MissingBytes != 400 || n.TotalBytes != 1000) {
			t.Errorf("libc trace %+v, want 400 of 1000 missing", n)
		}
	}
}

func TestPrioritizeUsesNodeInternalIP(t *testing.T) {
	cat := catalogDir(t)
	warm := startAgent(t, cat,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib", "cccccccccccccccccccccccccccccccc-libc")
	s, _ := newExtender(t, cat)
	s.AgentURL = "http://{ip}"
	nodes := `{"items": [
		{"metadata": {"name": "n1"}, "status": {"addresses": [
			{"type": "Hostname", "address": "n1"}, {"type": "InternalIP", "address": "` + warm + `"}]}},
		{"metadata": {"name": "n2"}, "status": {"addresses": [{"type": "Hostname", "address": "n2"}]}}
	]}`
	got := prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), Nodes: json.RawMessage(nodes)})
	if got["n1"] != 10 || got["n2"] != 0 {
		t.Fatalf("scores %v, want n1=10 and n2=0 (no InternalIP)", got)
	}
}

func TestPrioritizeNeutralWithoutKnownClosure(t *testing.T) {
	cat := catalogDir(t)
	warm := startAgent(t, cat,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib", "cccccccccccccccccccccccccccccccc-libc")
	s, _ := newExtender(t, cat)
	names := []string{warm}
	for _, ann := range []map[string]string{
		nil,
		{RootAnnotation: "/nix/store/dddddddddddddddddddddddddddddddd-other"},
	} {
		got := prioritize(t, s, Args{Pod: podJSON(ann), NodeNames: &names})
		if got[warm] != 0 {
			t.Errorf("annotations %v: score %d, want 0", ann, got[warm])
		}
	}
}

func TestPrioritizeAgentTimeoutScoresZero(t *testing.T) {
	cat := catalogDir(t)
	warm := startAgent(t, cat,
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib", "cccccccccccccccccccccccccccccccc-libc")
	release := make(chan struct{})
	stuck := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(stuck.Close)
	t.Cleanup(func() { close(release) })
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(broken.Close)

	s, _ := newExtender(t, cat)
	s.AgentTimeout = 100 * time.Millisecond
	names := []string{warm, strings.TrimPrefix(stuck.URL, "http://"), strings.TrimPrefix(broken.URL, "http://"), "127.0.0.1:1"}
	start := time.Now()
	got := prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), NodeNames: &names})
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("prioritize took %v with a stuck agent", elapsed)
	}
	if got[warm] != 10 {
		t.Errorf("warm score %d, want 10", got[warm])
	}
	for _, n := range names[1:] {
		if got[n] != 0 {
			t.Errorf("failed agent %s score %d, want 0", n, got[n])
		}
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if !strings.Contains(rec.Body.String(), "k3snix_extender_agent_errors_total 3\n") {
		t.Errorf("metrics:\n%s", rec.Body)
	}
}

func TestFilterPassesNodesThrough(t *testing.T) {
	s, _ := newExtender(t, catalogDir(t))
	nodes := `{"kind":"NodeList","items":[{"metadata":{"name":"n1","labels":{"a":"b"}},"spec":{"podCIDR":"10.0.0.0/24"}}]}`
	var res struct {
		Nodes json.RawMessage `json:"nodes"`
	}
	out := call(t, s.Handler(), "filter", Args{Pod: podJSON(nil), Nodes: json.RawMessage(nodes)})
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatal(err)
	}
	if string(res.Nodes) != nodes {
		t.Fatalf("filter returned nodes %s, want %s", res.Nodes, nodes)
	}

	names := []string{"a", "b"}
	var named FilterResult
	if err := json.Unmarshal(call(t, s.Handler(), "filter", Args{NodeNames: &names}), &named); err != nil {
		t.Fatal(err)
	}
	if named.NodeNames == nil || len(*named.NodeNames) != 2 || named.FailedNodes != nil {
		t.Fatalf("filter with node names: %+v", named)
	}
}

func TestScore(t *testing.T) {
	for _, tc := range []struct {
		missing, total uint64
		want           int64
	}{
		{0, 1000, 10},
		{1000, 1000, 0},
		{24, 1000, 10}, // a 2.4% upgrade delta still scores the warm node 10
		{500, 1000, 5},
		{0, 0, 0},
		{5, 1, 0},
	} {
		if got := Score(tc.missing, tc.total); got != tc.want {
			t.Errorf("Score(%d, %d) = %d, want %d", tc.missing, tc.total, got, tc.want)
		}
	}
}
