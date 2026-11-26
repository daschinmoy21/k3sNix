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
	return startLoadedAgent(t, catalog, nil, have...)
}

// startLoadedAgent is startAgent for a node reporting load; nil reports none,
// like an agent from before load reporting.
func startLoadedAgent(t *testing.T, catalog string, load *agent.NodeLoad, have ...string) string {
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
	if load != nil {
		a.SetLoad(*load)
	}
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
	// The fixture closure is 1000 bytes: score by held share unless a test
	// is about the byte-aware scale.
	s.ScaleBytes = 0
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
	const mb = 1 << 20
	for _, tc := range []struct {
		missing, total, scale uint64
		factor                float64
		want                  int64
	}{
		// Held share: no scale, or a closure at least as big as the scale.
		{0, 1000, 0, 1, 10},
		{1000, 1000, 0, 1, 0},
		{24, 1000, 0, 1, 10}, // a 2.4% upgrade delta still scores the warm node 10
		{500, 1000, 0, 1, 5},
		{1024 * mb, 2048 * mb, 256 * mb, 1, 5},
		// Byte-aware: a cold node missing all of a 5 MB closure costs little.
		{5 * mb, 5 * mb, 256 * mb, 1, 10},
		{128 * mb, 128 * mb, 256 * mb, 1, 5},
		// Load factor.
		{0, 1000, 0, 0.25, 3},
		{0, 0, 0, 1, 0},
		{5, 1, 0, 1, 0},
	} {
		if got := Score(tc.missing, tc.total, tc.scale, tc.factor); got != tc.want {
			t.Errorf("Score(%d, %d, %d, %v) = %d, want %d", tc.missing, tc.total, tc.scale, tc.factor, got, tc.want)
		}
	}
}

func fixturePaths() []string {
	return []string{
		"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-web", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb-lib", "cccccccccccccccccccccccccccccccc-libc",
	}
}

// A fully cached but loaded node loses to a cold idle node when the closure
// is small: fetching it costs little, and the warm node is busy.
func TestLoadedWarmNodeLosesOnSmallClosure(t *testing.T) {
	cat := catalogDir(t)
	warm := startLoadedAgent(t, cat, &agent.NodeLoad{CPU: 0.9, Memory: 0.5}, fixturePaths()...)
	cold := startLoadedAgent(t, cat, &agent.NodeLoad{CPU: 0.1, Memory: 0.2})
	s, logs := newExtender(t, cat)
	s.ScaleBytes = 1 << 20 // the 1000-byte closure is small
	s.HighSpread = 1       // isolate the per-node CPU threshold
	names := []string{warm, cold}
	got := prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), NodeNames: &names})
	if got[cold] <= got[warm] {
		t.Fatalf("scores %v: the cold idle node should beat the loaded warm node", got)
	}

	// The trace records each node's load and factor.
	var tr Trace
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(logs.String()), "prioritize ")), &tr); err != nil {
		t.Fatal(err)
	}
	for _, n := range tr.Nodes {
		if n.CPUUtil == nil || n.MemoryUtil == nil {
			t.Errorf("trace for %s lacks load: %+v", n.Node, n)
		}
		if want := map[string]float64{warm: 0.25, cold: 1}[n.Node]; n.LoadFactor != want {
			t.Errorf("node %s load factor %v, want %v", n.Node, n.LoadFactor, want)
		}
	}
}

// A large closure still wins on a moderately loaded node.
func TestLargeClosureWinsOnModeratelyLoadedNode(t *testing.T) {
	cat := catalogDir(t)
	warm := startLoadedAgent(t, cat, &agent.NodeLoad{CPU: 0.5}, fixturePaths()...)
	cold := startLoadedAgent(t, cat, &agent.NodeLoad{CPU: 0.45})
	s, _ := newExtender(t, cat)
	s.ScaleBytes = 1000 // the closure is as large as the scale
	names := []string{warm, cold}
	got := prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), NodeNames: &names})
	if got[warm] != 10 || got[cold] != 0 {
		t.Fatalf("scores %v, want the warm node 10 and the cold node 0", got)
	}
}

// An unbalanced cluster scales every node's score down, so the balance
// plugins decide; a node without load data counts as unloaded.
func TestUnbalancedClusterScalesEveryScore(t *testing.T) {
	cat := catalogDir(t)
	warm := startLoadedAgent(t, cat, &agent.NodeLoad{CPU: 0.1}, fixturePaths()...)
	busy := startLoadedAgent(t, cat, &agent.NodeLoad{CPU: 0.55}, fixturePaths()...)
	old := startAgent(t, cat, fixturePaths()...) // reports no load
	s, logs := newExtender(t, cat)
	names := []string{warm, busy, old}
	got := prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), NodeNames: &names})
	// CPU 0.1 and 0.55 spread by 0.225 >= 0.16; the old agent adds no sample.
	for _, n := range names {
		if got[n] != 3 {
			t.Errorf("node %s score %d, want 3 (10 x 0.25)", n, got[n])
		}
	}
	var tr Trace
	if err := json.Unmarshal([]byte(strings.TrimPrefix(strings.TrimSpace(logs.String()), "prioritize ")), &tr); err != nil {
		t.Fatal(err)
	}
	if tr.CPUSpread < 0.224 || tr.CPUSpread > 0.226 {
		t.Fatalf("cpu_spread %v, want 0.225", tr.CPUSpread)
	}

	// Balanced again: the node without load data is unloaded and scores 10.
	s.HighSpread = 1
	got = prioritize(t, s, Args{Pod: podJSON(map[string]string{RootAnnotation: root}), NodeNames: &names})
	if got[old] != 10 || got[warm] != 10 {
		t.Fatalf("scores %v, want 10 for the idle node and the node without load", got)
	}
}

func TestAgentURLBracketsIPv6(t *testing.T) {
	s := New(nil)
	for _, tc := range []struct{ ip, want string }{
		{"10.0.0.7", "http://10.0.0.7:9860"},
		{"2001:db8::1", "http://[2001:db8::1]:9860"},
		{"::ffff:10.0.0.7", "http://[::ffff:10.0.0.7]:9860"},
	} {
		if got := s.agentURL(candidate{name: "n", ip: tc.ip}); got != tc.want {
			t.Errorf("agentURL(%s) = %s, want %s", tc.ip, got, tc.want)
		}
	}
}
