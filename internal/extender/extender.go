// Package extender implements the kube-scheduler extender: it rescores the
// feasible nodes for a pod by how many bytes of the pod's closure each node
// is missing, as reported by the node agents.
package extender

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"k3snix/internal/closure"
)

// RootAnnotation names the pod's root store path. The extender looks the
// root up in the catalog; the closure and its ID always come from the
// catalog, never from the annotation.
const RootAnnotation = "k3snix.io/root"

// MaxScore is the highest score an extender may return
// (extenderv1.MaxExtenderPriority).
const MaxScore = 10

// Defaults for the load-aware, byte-aware score. The load thresholds are
// LRScheduler's h_CPU and h_STD (docs/evaluation.md); the scale and the
// loaded factor are starting points to tune from the Week 5 runs.
const (
	DefaultScaleBytes   = 256 << 20
	DefaultHighCPU      = 0.6
	DefaultHighSpread   = 0.16
	DefaultLoadedFactor = 0.25
)

const (
	defaultAgentTimeout = 500 * time.Millisecond
	maxRequestBytes     = 32 << 20 // node objects for a large cluster
	maxAgentReplyBytes  = 32 << 20
)

// Server answers the extender verbs.
type Server struct {
	Catalog *closure.Catalog

	// AgentURL maps a node to its agent's base URL. "{ip}" is replaced by
	// the node's InternalIP, bracketed when it is IPv6, and "{node}" by its
	// name. The scheduler sends full node objects unless nodeCacheCapable is
	// set, so {ip} is available by default.
	AgentURL string

	// AgentTimeout bounds one missing_bytes call. A node whose agent does
	// not answer in time scores 0; the cycle never waits longer than this.
	AgentTimeout time.Duration

	// ScaleBytes makes the score byte-aware: a node scores
	// MaxScore × (1 − missing ÷ max(closure bytes, ScaleBytes)). For a
	// closure of ScaleBytes or more that is the share of the closure the
	// node holds. A smaller closure costs little to fetch anywhere, so every
	// node scores near MaxScore and the cache stops outweighing the balance
	// plugins. Zero scores the held share alone.
	ScaleBytes uint64

	// Load awareness, after LRScheduler's dynamic weight. A node's score is
	// multiplied by LoadedFactor when its CPU use is at least HighCPU, and
	// every node's is when the standard deviation of CPU use across the
	// candidates is at least HighSpread: a hot or unbalanced cluster leaves
	// placement to the balance plugins. A node whose agent reports no load
	// (an older agent, or one not yet sampled) counts as unloaded.
	HighCPU      float64
	HighSpread   float64
	LoadedFactor float64

	Client *http.Client
	Log    *log.Logger

	mu        sync.Mutex
	cycles    int64
	unknown   int64 // cycles for a pod without a known closure
	agentErrs int64
	latencyUS int64 // summed prioritize latency
}

// New returns a Server using cat to resolve pods' closures.
func New(cat *closure.Catalog) *Server {
	return &Server{
		Catalog:      cat,
		AgentURL:     "http://{ip}:9860",
		AgentTimeout: defaultAgentTimeout,
		ScaleBytes:   DefaultScaleBytes,
		HighCPU:      DefaultHighCPU,
		HighSpread:   DefaultHighSpread,
		LoadedFactor: DefaultLoadedFactor,
		Client:       &http.Client{},
		Log:          log.New(os.Stderr, "k3snix-extender ", log.LstdFlags),
	}
}

// Handler returns the extender's HTTP routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/filter", s.handleFilter)
	mux.HandleFunc("/prioritize", s.handlePrioritize)
	return mux
}

func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	cycles, unknown, agentErrs, latency := s.cycles, s.unknown, s.agentErrs, s.latencyUS
	s.mu.Unlock()
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "k3snix_extender_prioritize_total %d\n", cycles)
	fmt.Fprintf(w, "k3snix_extender_unknown_closure_total %d\n", unknown)
	fmt.Fprintf(w, "k3snix_extender_agent_errors_total %d\n", agentErrs)
	fmt.Fprintf(w, "k3snix_extender_prioritize_seconds_sum %g\n", float64(latency)/1e6)
}

func decodeArgs(w http.ResponseWriter, r *http.Request) (Args, bool) {
	var args Args
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return args, false
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBytes)
	if err := json.NewDecoder(r.Body).Decode(&args); err != nil {
		http.Error(w, "bad request: "+err.Error(), http.StatusBadRequest)
		return args, false
	}
	return args, true
}

// handleFilter passes every node through: resource fit and every other
// predicate stay with the scheduler, and cache state never makes a node
// infeasible. The node objects are echoed byte for byte.
func (s *Server) handleFilter(w http.ResponseWriter, r *http.Request) {
	args, ok := decodeArgs(w, r)
	if !ok {
		return
	}
	writeJSON(w, FilterResult{Nodes: args.Nodes, NodeNames: args.NodeNames})
}

// candidate is one node to score.
type candidate struct {
	name string
	ip   string
}

func candidates(args Args) ([]candidate, error) {
	if args.NodeNames != nil {
		out := make([]candidate, len(*args.NodeNames))
		for i, n := range *args.NodeNames {
			out[i] = candidate{name: n}
		}
		return out, nil
	}
	if len(args.Nodes) == 0 || string(args.Nodes) == "null" {
		return nil, nil
	}
	var list NodeList
	if err := json.Unmarshal(args.Nodes, &list); err != nil {
		return nil, fmt.Errorf("nodes: %w", err)
	}
	out := make([]candidate, len(list.Items))
	for i, n := range list.Items {
		out[i] = candidate{name: n.Metadata.Name, ip: n.InternalIP()}
	}
	return out, nil
}

// NodeTrace records how one node was scored in a cycle.
type NodeTrace struct {
	Node         string   `json:"node"`
	MissingBytes uint64   `json:"missing_bytes"`
	TotalBytes   uint64   `json:"total_bytes"`
	CPUUtil      *float64 `json:"cpu_util,omitempty"`
	MemoryUtil   *float64 `json:"mem_util,omitempty"`
	LoadFactor   float64  `json:"load_factor"`
	Score        int64    `json:"score"`
	Error        string   `json:"error,omitempty"`
}

// Trace is the line logged for every prioritize call.
type Trace struct {
	Pod       string      `json:"pod"`
	Root      string      `json:"root,omitempty"`
	Closure   string      `json:"closure,omitempty"`
	Label     string      `json:"label,omitempty"`
	Skipped   string      `json:"skipped,omitempty"`
	CPUSpread float64     `json:"cpu_spread"` // std dev of CPU use over the nodes that reported it
	LatencyMS float64     `json:"latency_ms"`
	Nodes     []NodeTrace `json:"nodes"`
}

// handlePrioritize scores every candidate 0 to MaxScore by the share of the
// pod's closure bytes the node already holds. A pod without a known
// closure, and a node whose agent fails or times out, scores 0: the
// extender never fails the cycle and never blocks it longer than
// AgentTimeout.
func (s *Server) handlePrioritize(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	args, ok := decodeArgs(w, r)
	if !ok {
		return
	}
	nodes, err := candidates(args)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	meta := args.Pod.Metadata
	tr := Trace{Pod: meta.Namespace + "/" + meta.Name, Root: meta.Annotations[RootAnnotation]}
	tr.Nodes = make([]NodeTrace, len(nodes))
	for i, n := range nodes {
		tr.Nodes[i] = NodeTrace{Node: n.name}
	}

	var c closure.Closure
	switch {
	case tr.Root == "":
		tr.Skipped = "no " + RootAnnotation + " annotation"
	case s.Catalog == nil:
		tr.Skipped = "no catalog"
	default:
		if c, ok = s.Catalog.ByRoot(tr.Root); ok {
			tr.Closure, tr.Label = c.Fingerprint(), c.Label
		} else {
			tr.Skipped = "root not in catalog"
		}
	}
	if tr.Skipped == "" {
		tr.CPUSpread = s.scoreAll(r.Context(), c, tr.Closure, nodes, tr.Nodes)
	}

	out := make([]HostPriority, len(tr.Nodes))
	errs := 0
	for i, n := range tr.Nodes {
		out[i] = HostPriority{Host: n.Node, Score: n.Score}
		if n.Error != "" {
			errs++
		}
	}
	elapsed := time.Since(start)
	tr.LatencyMS = float64(elapsed.Microseconds()) / 1000
	s.mu.Lock()
	s.cycles++
	if tr.Skipped != "" {
		s.unknown++
	}
	s.agentErrs += int64(errs)
	s.latencyUS += elapsed.Microseconds()
	s.mu.Unlock()
	s.trace(tr)
	writeJSON(w, out)
}

// scoreAll asks every node's agent in parallel, fills in traces, and
// returns the spread of CPU use across the nodes that reported it.
func (s *Server) scoreAll(ctx context.Context, c closure.Closure, id string, nodes []candidate, traces []NodeTrace) float64 {
	timeout := s.AgentTimeout
	if timeout <= 0 {
		timeout = defaultAgentTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var wg sync.WaitGroup
	for i, n := range nodes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t := &traces[i]
			m, err := s.missing(ctx, n, c, id)
			if err != nil {
				t.Error = err.Error()
				return
			}
			t.MissingBytes, t.TotalBytes = m.Bytes, m.TotalBytes
			t.CPUUtil, t.MemoryUtil = m.CPUUtil, m.MemoryUtil
		}()
	}
	wg.Wait()

	var cpus []float64
	for _, t := range traces {
		if t.Error == "" && t.CPUUtil != nil {
			cpus = append(cpus, *t.CPUUtil)
		}
	}
	spread := stddev(cpus)
	for i := range traces {
		t := &traces[i]
		if t.Error != "" {
			continue
		}
		t.LoadFactor = 1
		if spread >= s.HighSpread || (t.CPUUtil != nil && *t.CPUUtil >= s.HighCPU) {
			t.LoadFactor = s.LoadedFactor
		}
		t.Score = Score(t.MissingBytes, t.TotalBytes, s.ScaleBytes, t.LoadFactor)
	}
	return spread
}

// Score maps a node's missing bytes to 0..MaxScore:
// MaxScore × (1 − missing ÷ max(total, scale)) × loadFactor, rounded. With a
// scale of 0, or a closure of at least scale bytes, the base is the share of
// the closure the node holds. An empty closure scores 0.
func Score(missing, total, scale uint64, loadFactor float64) int64 {
	if total == 0 || missing > total {
		return 0
	}
	denom := max(total, scale)
	base := MaxScore * (1 - float64(missing)/float64(denom))
	return int64(math.Round(base * loadFactor))
}

// stddev is the population standard deviation; 0 for fewer than two values.
func stddev(xs []float64) float64 {
	if len(xs) < 2 {
		return 0
	}
	var mean float64
	for _, x := range xs {
		mean += x
	}
	mean /= float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (x - mean) * (x - mean)
	}
	return math.Sqrt(v / float64(len(xs)))
}

// missingReply is the subset of the agent's /v1/missing_bytes reply used.
type missingReply struct {
	Bytes      uint64   `json:"bytes"`
	TotalBytes uint64   `json:"total_bytes"`
	CPUUtil    *float64 `json:"cpu_util"`
	MemoryUtil *float64 `json:"mem_util"`
}

var errUnknownClosure = errors.New("agent does not know the closure")

// missing asks one agent for the closure's missing bytes by ID, and with the
// full path list when that agent's catalog lacks the closure.
func (s *Server) missing(ctx context.Context, n candidate, c closure.Closure, id string) (missingReply, error) {
	base := s.agentURL(n)
	if base == "" {
		return missingReply{}, fmt.Errorf("node %s has no InternalIP", n.name)
	}
	m, err := s.post(ctx, base, map[string]any{"id": id})
	if errors.Is(err, errUnknownClosure) {
		m, err = s.post(ctx, base, map[string]any{"paths": c.Paths})
	}
	return m, err
}

func (s *Server) agentURL(n candidate) string {
	u := s.AgentURL
	if strings.Contains(u, "{ip}") {
		if n.ip == "" {
			return ""
		}
		ip := n.ip
		if net.ParseIP(ip) != nil && strings.Contains(ip, ":") {
			ip = "[" + ip + "]" // an IPv6 literal needs brackets before :port
		}
		u = strings.ReplaceAll(u, "{ip}", ip)
	}
	return strings.TrimRight(strings.ReplaceAll(u, "{node}", n.name), "/")
}

func (s *Server) post(ctx context.Context, base string, body any) (missingReply, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return missingReply{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/v1/missing_bytes", bytes.NewReader(data))
	if err != nil {
		return missingReply{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	client := s.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return missingReply{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return missingReply{}, errUnknownClosure
	}
	if resp.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return missingReply{}, fmt.Errorf("agent status %d", resp.StatusCode)
	}
	var m missingReply
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxAgentReplyBytes)).Decode(&m); err != nil {
		return missingReply{}, fmt.Errorf("agent reply: %w", err)
	}
	return m, nil
}

func (s *Server) trace(tr Trace) {
	if s.Log == nil {
		return
	}
	line, err := json.Marshal(tr)
	if err != nil {
		s.Log.Printf("trace: %v", err)
		return
	}
	s.Log.Printf("prioritize %s", line)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}
