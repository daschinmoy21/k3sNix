package lrscheduler

import "context"

// Code represents framework status codes mirroring Kubernetes Scheduling Framework.
type Code int

const (
	// Success indicates that plugin execution completed successfully.
	Success Code = iota
	// Error indicates that an internal plugin error occurred.
	Error
	// Unschedulable indicates that the node cannot schedule the pod.
	Unschedulable
	// Wait indicates the scheduling cycle should wait.
	Wait
	// Skip indicates the plugin skips scoring.
	Skip
)

// Status provides return status for scheduling framework extension points.
type Status struct {
	code    Code
	message string
}

func (s *Status) Code() Code      { return s.code }
func (s *Status) Message() string { return s.message }
func (s *Status) IsSuccess() bool { return s == nil || s.code == Success }

func NewStatus(code Code, msg string) *Status {
	return &Status{code: code, message: msg}
}

// CycleState provides state storage across a single scheduling cycle.
type CycleState struct {
	storage map[string]any
}

func NewCycleState() *CycleState {
	return &CycleState{storage: make(map[string]any)}
}

func (c *CycleState) Read(key string) (any, bool) {
	if c == nil {
		return nil, false
	}
	v, ok := c.storage[key]
	return v, ok
}

func (c *CycleState) Write(key string, val any) {
	if c == nil {
		return
	}
	c.storage[key] = val
}

func (c *CycleState) Delete(key string) {
	if c == nil {
		return
	}
	delete(c.storage, key)
}

// Container represents a container specification within a Pod.
type Container struct {
	Name  string `json:"name"`
	Image string `json:"image"` // Image reference (e.g. "nginx:latest", "repo/img@sha256:...")
}

// Pod holds the scheduled pod specification.
type Pod struct {
	Name       string      `json:"name"`
	Namespace  string      `json:"namespace"`
	Containers []Container `json:"containers"`
}

// Node holds basic node information.
type Node struct {
	Name string `json:"name"`
	IP   string `json:"ip"`
}

// NodeScore is a pair of node name and its computed score.
type NodeScore struct {
	Name  string `json:"name"`
	Score int64  `json:"score"`
}

// NodeScoreList is a slice of NodeScore.
type NodeScoreList []NodeScore

// Plugin is the base plugin interface.
type Plugin interface {
	Name() string
}

// ScoreExtensions allows post-score normalization.
type ScoreExtensions interface {
	NormalizeScore(ctx context.Context, state *CycleState, p *Pod, scores NodeScoreList) *Status
}

// ScorePlugin is the scheduling framework Score plugin interface.
type ScorePlugin interface {
	Plugin
	Score(ctx context.Context, state *CycleState, p *Pod, nodeName string) (int64, *Status)
	ScoreExtensions() ScoreExtensions
}

// LayerMetadata represents a single container image layer (from Listing 1 of the paper).
type LayerMetadata struct {
	Size  int64  `json:"size"`
	Layer string `json:"layer"` // Layer digest / hash (e.g. sha256:...)
}

// ImageMetadata represents metadata for an image (from Listing 1 of the paper).
type ImageMetadata struct {
	ID              string          `json:"id"`
	Name            string          `json:"name"`
	NameWithoutRepo string          `json:"name_without_repo"`
	Tag             string          `json:"tag"`
	TotalSize       int64           `json:"total_size"`
	Layers          []LayerMetadata `json:"l_meta"`
}

// ImageMetadataLists represents the registry cache format (Listing 1 of the paper).
type ImageMetadataLists struct {
	CacheFile string                   `json:"catch_file"`
	Lists     map[string]ImageMetadata `json:"lists"`
}

// NodeMetrics provides node utilization data for calculating LRScheduler dynamic weights.
type NodeMetrics struct {
	// CPUUtilization is p_n(t) / p_n, the CPU utilization ratio in [0, 1].
	CPUUtilization float64
	// MemUtilization is e_n(t) / e_n, the Memory utilization ratio in [0, 1].
	MemUtilization float64
}

// NodeInfoProvider provides access to node resource utilization, cached image layers,
// and default Kubernetes scores.
type NodeInfoProvider interface {
	// GetNodeMetrics returns the CPU and memory utilization of the node.
	GetNodeMetrics(nodeName string) (NodeMetrics, error)
	// GetNodeCachedLayers returns the layer digests present on the node.
	GetNodeCachedLayers(nodeName string) ([]string, error)
	// GetNodeK8sScore returns the default Kubernetes scheduler score S_k8s for the node.
	GetNodeK8sScore(nodeName string) (int64, error)
}
