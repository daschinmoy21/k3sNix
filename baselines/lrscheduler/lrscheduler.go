package lrscheduler

import (
	"context"
	"fmt"
	"math"
	"time"
)

const (
	// PluginName is the name of the LRScheduler plugin.
	PluginName = "LRScheduler"

	// DefaultPollInterval is the registry polling interval (10 seconds, Section V-1).
	DefaultPollInterval = 10 * time.Second

	// DefaultOmega1 is the weight for S_layer when layer-caching is prioritized (Algorithm 1).
	DefaultOmega1 = 2.0

	// DefaultOmega2 is the weight for S_layer when Kubernetes resources are prioritized (Algorithm 1).
	DefaultOmega2 = 0.5

	// DefaultHSize is the cache size threshold in bytes: 10 MB (Section IV-B, Eq. 13).
	DefaultHSize int64 = 10 * 1024 * 1024 // 10,485,760 bytes

	// DefaultHCPU is the CPU utilization threshold: 60% (Section IV-B, Eq. 13).
	DefaultHCPU = 0.60

	// DefaultHSTD is the resource balance threshold: 0.16 (Section IV-B, Eq. 13).
	DefaultHSTD = 0.16

	// MaxNodeScore is the maximum score in the Kubernetes scheduling framework.
	MaxNodeScore int64 = 100

	// MinNodeScore is the minimum score in the Kubernetes scheduling framework.
	MinNodeScore int64 = 0
)

// Config configures the LRScheduler score plugin.
type Config struct {
	// Omega1 is the weight applied to S_layer when S_Weight == 1 (default: 2.0).
	Omega1 float64
	// Omega2 is the weight applied to S_layer when S_Weight == 0 (default: 0.5).
	Omega2 float64
	// HSize is the cache size threshold in bytes (default: 10MB).
	HSize int64
	// HCPU is the CPU utilization threshold ratio in [0, 1] (default: 0.60).
	HCPU float64
	// HSTD is the resource balancing threshold (default: 0.16).
	HSTD float64
	// PollInterval is the periodic interval for registry polling (default: 10s).
	PollInterval time.Duration
	// RegistryURL is the target OCI/Docker registry URL (optional).
	RegistryURL string
	// Normalize specifies whether NormalizeScore scales final scores to [0, 100].
	Normalize bool
}

// DefaultConfig returns the default LRScheduler configuration matching arXiv:2506.03694.
func DefaultConfig() Config {
	return Config{
		Omega1:       DefaultOmega1,
		Omega2:       DefaultOmega2,
		HSize:        DefaultHSize,
		HCPU:         DefaultHCPU,
		HSTD:         DefaultHSTD,
		PollInterval: DefaultPollInterval,
		RegistryURL:  "",
		Normalize:    false,
	}
}

// CalculateLayerScore computes S_Layer from Eq. (3):
// S_Layer = (D_c^n(t) / sum_{l in L_c} d_l) * 100
// Returns a value in [0, 100]. Returns 0 if totalBytes <= 0.
func CalculateLayerScore(cachedBytes, totalBytes int64) float64 {
	if totalBytes <= 0 {
		return 0.0
	}
	if cachedBytes <= 0 {
		return 0.0
	}
	if cachedBytes >= totalBytes {
		return 100.0
	}
	return (float64(cachedBytes) / float64(totalBytes)) * 100.0
}

// CalculateResourceBalanceScore computes S_STD from Eq. (11):
// S_STD = |p_n(t)/p_n - e_n(t)/e_n| / 2
// Measures the standard deviation / imbalance between CPU and memory utilization.
func CalculateResourceBalanceScore(cpuUtil, memUtil float64) float64 {
	return math.Abs(cpuUtil-memUtil) / 2.0
}

// CalculateCPUScore computes S_CPU from Eq. (12):
// S_CPU = p_n(t) / p_n
// Represents the CPU utilization ratio of node n.
func CalculateCPUScore(cpuUtil float64) float64 {
	return cpuUtil
}

// CalculateWeightScore computes S_Weight from Eq. (13):
// S_Weight = [D_c^n(t) > h_size] * [S_CPU < h_CPU] * [S_STD < h_STD]
// Returns 1 if all three conditions are strictly met; returns 0 otherwise.
func CalculateWeightScore(cachedBytes int64, sCPU, sSTD float64, hSize int64, hCPU, hSTD float64) int {
	condSize := cachedBytes > hSize
	condCPU := sCPU < hCPU
	condSTD := sSTD < hSTD

	if condSize && condCPU && condSTD {
		return 1
	}
	return 0
}

// SelectOmega selects omega according to Algorithm 1 (Dynamic weight switching):
// If S_Weight == 1: omega = omega1 (2.0)
// If S_Weight == 0: omega = omega2 (0.5)
func SelectOmega(sWeight int, omega1, omega2 float64) float64 {
	if sWeight == 1 {
		return omega1
	}
	return omega2
}

// CalculateFinalScore computes the composite score S from Eq. (4):
// S = omega * S_Layer + S_K8s
func CalculateFinalScore(omega, sLayer, sK8s float64) float64 {
	return (omega * sLayer) + sK8s
}

// LRScheduler is a Kubernetes Scheduling Framework Score plugin implementing
// the layer-aware, resource-adaptive LRScheduler baseline (arXiv:2506.03694).
type LRScheduler struct {
	config       Config
	cache        *RegistryCache
	nodeProvider NodeInfoProvider
	watcher      *RegistryWatcher
}

// New creates a new LRScheduler score plugin.
func New(config Config, cache *RegistryCache, nodeProvider NodeInfoProvider) *LRScheduler {
	if config.Omega1 == 0 {
		config.Omega1 = DefaultOmega1
	}
	if config.Omega2 == 0 {
		config.Omega2 = DefaultOmega2
	}
	if config.HSize == 0 {
		config.HSize = DefaultHSize
	}
	if config.HCPU == 0 {
		config.HCPU = DefaultHCPU
	}
	if config.HSTD == 0 {
		config.HSTD = DefaultHSTD
	}
	if config.PollInterval == 0 {
		config.PollInterval = DefaultPollInterval
	}
	if cache == nil {
		cache = NewRegistryCache()
	}

	var watcher *RegistryWatcher
	if config.RegistryURL != "" {
		watcher = NewRegistryWatcher(config.RegistryURL, config.PollInterval, cache)
	}

	return &LRScheduler{
		config:       config,
		cache:        cache,
		nodeProvider: nodeProvider,
		watcher:      watcher,
	}
}

// Name returns the name of the plugin.
func (s *LRScheduler) Name() string {
	return PluginName
}

// ScoreExtensions returns the ScoreExtensions interface if normalization is enabled.
func (s *LRScheduler) ScoreExtensions() ScoreExtensions {
	return s
}

// NormalizeScore normalizes scores across nodes to the range [MinNodeScore, MaxNodeScore].
func (s *LRScheduler) NormalizeScore(ctx context.Context, state *CycleState, p *Pod, scores NodeScoreList) *Status {
	if !s.config.Normalize || len(scores) == 0 {
		return NewStatus(Success, "")
	}

	var maxScore int64
	for _, nodeScore := range scores {
		if nodeScore.Score > maxScore {
			maxScore = nodeScore.Score
		}
	}

	if maxScore == 0 {
		return NewStatus(Success, "")
	}

	for i := range scores {
		scores[i].Score = (scores[i].Score * MaxNodeScore) / maxScore
	}

	return NewStatus(Success, "")
}

// Score computes the LRScheduler score for a node: S = omega * S_layer + S_k8s.
func (s *LRScheduler) Score(ctx context.Context, state *CycleState, p *Pod, nodeName string) (int64, *Status) {
	if p == nil {
		return 0, NewStatus(Error, "pod cannot be nil")
	}
	if nodeName == "" {
		return 0, NewStatus(Error, "nodeName cannot be empty")
	}
	if s.nodeProvider == nil {
		return 0, NewStatus(Error, "nodeProvider is not configured")
	}

	// 1. Query node's cached layers
	nodeLayers, err := s.nodeProvider.GetNodeCachedLayers(nodeName)
	if err != nil {
		return 0, NewStatus(Error, fmt.Sprintf("failed to get cached layers for node %s: %v", nodeName, err))
	}

	// 2. Query node's resource utilization metrics
	metrics, err := s.nodeProvider.GetNodeMetrics(nodeName)
	if err != nil {
		return 0, NewStatus(Error, fmt.Sprintf("failed to get metrics for node %s: %v", nodeName, err))
	}

	// 3. Query default Kubernetes score S_k8s for the node
	sK8s, err := s.nodeProvider.GetNodeK8sScore(nodeName)
	if err != nil {
		return 0, NewStatus(Error, fmt.Sprintf("failed to get K8s score for node %s: %v", nodeName, err))
	}

	// 4. Calculate total cached bytes and total image bytes across all containers in the pod
	var totalCachedBytes int64
	var totalImageBytes int64

	for _, container := range p.Containers {
		if container.Image == "" {
			continue
		}
		cachedBytes, totalBytes, err := s.cache.CalculateNodeCachedBytes(container.Image, nodeLayers)
		if err != nil {
			// Image metadata might not be in cache yet; treat as uncached
			continue
		}
		totalCachedBytes += cachedBytes
		totalImageBytes += totalBytes
	}

	// 5. Calculate S_Layer (Eq. 3)
	sLayer := CalculateLayerScore(totalCachedBytes, totalImageBytes)

	// 6. Calculate S_CPU (Eq. 12)
	sCPU := CalculateCPUScore(metrics.CPUUtilization)

	// 7. Calculate S_STD (Eq. 11)
	sSTD := CalculateResourceBalanceScore(metrics.CPUUtilization, metrics.MemUtilization)

	// 8. Calculate S_Weight (Eq. 13)
	sWeight := CalculateWeightScore(totalCachedBytes, sCPU, sSTD, s.config.HSize, s.config.HCPU, s.config.HSTD)

	// 9. Select dynamic weight omega (Algorithm 1)
	omega := SelectOmega(sWeight, s.config.Omega1, s.config.Omega2)

	// 10. Calculate final composite score S = omega * S_Layer + S_K8s (Eq. 4)
	finalScore := CalculateFinalScore(omega, sLayer, float64(sK8s))

	return int64(math.Round(finalScore)), NewStatus(Success, "")
}

// RegistryCache returns the underlying RegistryCache.
func (s *LRScheduler) RegistryCache() *RegistryCache {
	return s.cache
}

// Watcher returns the background RegistryWatcher, if configured.
func (s *LRScheduler) Watcher() *RegistryWatcher {
	return s.watcher
}
