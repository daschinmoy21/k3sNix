package lrscheduler

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

// mockNodeInfoProvider implements NodeInfoProvider for testing.
type mockNodeInfoProvider struct {
	metrics      map[string]NodeMetrics
	cachedLayers map[string][]string
	k8sScores    map[string]int64
	errMetrics   error
	errLayers    error
	errK8s       error
}

func newMockNodeInfoProvider() *mockNodeInfoProvider {
	return &mockNodeInfoProvider{
		metrics:      make(map[string]NodeMetrics),
		cachedLayers: make(map[string][]string),
		k8sScores:    make(map[string]int64),
	}
}

func (m *mockNodeInfoProvider) GetNodeMetrics(nodeName string) (NodeMetrics, error) {
	if m.errMetrics != nil {
		return NodeMetrics{}, m.errMetrics
	}
	v, ok := m.metrics[nodeName]
	if !ok {
		return NodeMetrics{}, errors.New("node metrics not found")
	}
	return v, nil
}

func (m *mockNodeInfoProvider) GetNodeCachedLayers(nodeName string) ([]string, error) {
	if m.errLayers != nil {
		return nil, m.errLayers
	}
	v, ok := m.cachedLayers[nodeName]
	if !ok {
		return []string{}, nil
	}
	return v, nil
}

func (m *mockNodeInfoProvider) GetNodeK8sScore(nodeName string) (int64, error) {
	if m.errK8s != nil {
		return 0, m.errK8s
	}
	v, ok := m.k8sScores[nodeName]
	if !ok {
		return 0, nil
	}
	return v, nil
}

// ---------------------------------------------------------------------
// 1. Basic Scoring Tests (S = omega * S_layer + S_k8s)
// ---------------------------------------------------------------------

func TestBasicScoringFormulas(t *testing.T) {
	tests := []struct {
		name        string
		cachedBytes int64
		totalBytes  int64
		cpuUtil     float64
		memUtil     float64
		k8sScore    int64
		wantOmega   float64
		wantSLayer  float64
		wantScore   int64
	}{
		{
			name:        "ideal condition: high cache, low cpu, balanced -> omega=2.0",
			cachedBytes: 20 * 1024 * 1024, // 20MB > 10MB
			totalBytes:  40 * 1024 * 1024, // 40MB -> S_layer = 50.0
			cpuUtil:     0.40,             // 0.40 < 0.60
			memUtil:     0.40,             // STD = 0.0 < 0.16
			k8sScore:    60,
			wantOmega:   2.0,
			wantSLayer:  50.0,
			// S = 2.0 * 50.0 + 60 = 160
			wantScore: 160,
		},
		{
			name:        "cache below threshold -> omega=0.5",
			cachedBytes: 5 * 1024 * 1024,  // 5MB < 10MB
			totalBytes:  20 * 1024 * 1024, // 20MB -> S_layer = 25.0
			cpuUtil:     0.30,             // < 0.60
			memUtil:     0.30,             // STD < 0.16
			k8sScore:    70,
			wantOmega:   0.5,
			wantSLayer:  25.0,
			// S = 0.5 * 25.0 + 70 = 82.5 -> round = 83 (or 82)
			wantScore: 83,
		},
		{
			name:        "cpu above threshold -> omega=0.5",
			cachedBytes: 20 * 1024 * 1024,
			totalBytes:  20 * 1024 * 1024, // S_layer = 100.0
			cpuUtil:     0.80,             // > 0.60
			memUtil:     0.80,
			k8sScore:    50,
			wantOmega:   0.5,
			wantSLayer:  100.0,
			// S = 0.5 * 100.0 + 50 = 100
			wantScore: 100,
		},
		{
			name:        "unbalanced resource (high STD) -> omega=0.5",
			cachedBytes: 30 * 1024 * 1024,
			totalBytes:  30 * 1024 * 1024, // S_layer = 100.0
			cpuUtil:     0.20,             // CPU low
			memUtil:     0.80,             // MEM high -> |0.2 - 0.8|/2 = 0.30 > 0.16
			k8sScore:    40,
			wantOmega:   0.5,
			wantSLayer:  100.0,
			// S = 0.5 * 100.0 + 40 = 90
			wantScore: 90,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			sLayer := CalculateLayerScore(tc.cachedBytes, tc.totalBytes)
			if math.Abs(sLayer-tc.wantSLayer) > 1e-6 {
				t.Errorf("CalculateLayerScore() = %v, want %v", sLayer, tc.wantSLayer)
			}

			sCPU := CalculateCPUScore(tc.cpuUtil)
			sSTD := CalculateResourceBalanceScore(tc.cpuUtil, tc.memUtil)
			sWeight := CalculateWeightScore(tc.cachedBytes, sCPU, sSTD, DefaultHSize, DefaultHCPU, DefaultHSTD)
			omega := SelectOmega(sWeight, DefaultOmega1, DefaultOmega2)
			if omega != tc.wantOmega {
				t.Errorf("SelectOmega() = %v, want %v", omega, tc.wantOmega)
			}

			finalScore := CalculateFinalScore(omega, sLayer, float64(tc.k8sScore))
			rounded := int64(math.Round(finalScore))
			if rounded != tc.wantScore {
				t.Errorf("CalculateFinalScore rounded = %v, want %v (raw %v)", rounded, tc.wantScore, finalScore)
			}
		})
	}
}

// ---------------------------------------------------------------------
// 2. Weight Selection & Boundary Threshold Tests (Eq. 13)
// ---------------------------------------------------------------------

func TestWeightSelectionBoundaries(t *testing.T) {
	const (
		hSize = DefaultHSize // 10,485,760
		hCPU  = DefaultHCPU  // 0.60
		hSTD  = DefaultHSTD  // 0.16
	)

	// Threshold 1: D_c^n(t) > h_size (Strictly greater than)
	t.Run("Threshold HSize: below, at, above", func(t *testing.T) {
		validCPU := 0.40 // < 0.60
		validSTD := 0.05 // < 0.16

		// Below: hSize - 1
		wBelow := CalculateWeightScore(hSize-1, validCPU, validSTD, hSize, hCPU, hSTD)
		if wBelow != 0 || SelectOmega(wBelow, 2.0, 0.5) != 0.5 {
			t.Errorf("hSize - 1: got S_Weight=%d, want 0 (omega=0.5)", wBelow)
		}

		// At: exactly hSize
		wAt := CalculateWeightScore(hSize, validCPU, validSTD, hSize, hCPU, hSTD)
		if wAt != 0 || SelectOmega(wAt, 2.0, 0.5) != 0.5 {
			t.Errorf("exactly hSize: got S_Weight=%d, want 0 (omega=0.5)", wAt)
		}

		// Above: hSize + 1
		wAbove := CalculateWeightScore(hSize+1, validCPU, validSTD, hSize, hCPU, hSTD)
		if wAbove != 1 || SelectOmega(wAbove, 2.0, 0.5) != 2.0 {
			t.Errorf("hSize + 1: got S_Weight=%d, want 1 (omega=2.0)", wAbove)
		}
	})

	// Threshold 2: S_CPU < h_CPU (Strictly less than)
	t.Run("Threshold HCPU: below, at, above", func(t *testing.T) {
		validSize := hSize + 1024
		validSTD := 0.05

		// Below: hCPU - 0.01 (0.59)
		wBelow := CalculateWeightScore(validSize, hCPU-0.01, validSTD, hSize, hCPU, hSTD)
		if wBelow != 1 || SelectOmega(wBelow, 2.0, 0.5) != 2.0 {
			t.Errorf("hCPU - 0.01: got S_Weight=%d, want 1 (omega=2.0)", wBelow)
		}

		// At: exactly hCPU (0.60)
		wAt := CalculateWeightScore(validSize, hCPU, validSTD, hSize, hCPU, hSTD)
		if wAt != 0 || SelectOmega(wAt, 2.0, 0.5) != 0.5 {
			t.Errorf("exactly hCPU: got S_Weight=%d, want 0 (omega=0.5)", wAt)
		}

		// Above: hCPU + 0.01 (0.61)
		wAbove := CalculateWeightScore(validSize, hCPU+0.01, validSTD, hSize, hCPU, hSTD)
		if wAbove != 0 || SelectOmega(wAbove, 2.0, 0.5) != 0.5 {
			t.Errorf("hCPU + 0.01: got S_Weight=%d, want 0 (omega=0.5)", wAbove)
		}
	})

	// Threshold 3: S_STD < h_STD (Strictly less than)
	t.Run("Threshold HSTD: below, at, above", func(t *testing.T) {
		validSize := hSize + 1024
		validCPU := 0.40

		// Below: hSTD - 0.01 (0.15)
		wBelow := CalculateWeightScore(validSize, validCPU, hSTD-0.01, hSize, hCPU, hSTD)
		if wBelow != 1 || SelectOmega(wBelow, 2.0, 0.5) != 2.0 {
			t.Errorf("hSTD - 0.01: got S_Weight=%d, want 1 (omega=2.0)", wBelow)
		}

		// At: exactly hSTD (0.16)
		wAt := CalculateWeightScore(validSize, validCPU, hSTD, hSize, hCPU, hSTD)
		if wAt != 0 || SelectOmega(wAt, 2.0, 0.5) != 0.5 {
			t.Errorf("exactly hSTD: got S_Weight=%d, want 0 (omega=0.5)", wAt)
		}

		// Above: hSTD + 0.01 (0.17)
		wAbove := CalculateWeightScore(validSize, validCPU, hSTD+0.01, hSize, hCPU, hSTD)
		if wAbove != 0 || SelectOmega(wAbove, 2.0, 0.5) != 0.5 {
			t.Errorf("hSTD + 0.01: got S_Weight=%d, want 0 (omega=0.5)", wAbove)
		}
	})
}

// ---------------------------------------------------------------------
// 3. Dynamic Switching Tests
// ---------------------------------------------------------------------

func TestDynamicSwitching(t *testing.T) {
	cfg := DefaultConfig()
	cache := NewRegistryCache()
	provider := newMockNodeInfoProvider()

	// Register an image with 2 layers of 15MB each (total 30MB)
	imageName := "testrepo/app:v1"
	cache.Put(imageName, ImageMetadata{
		Name:      "testrepo/app",
		Tag:       "v1",
		TotalSize: 30 * 1024 * 1024,
		Layers: []LayerMetadata{
			{Size: 15 * 1024 * 1024, Layer: "sha256:layer1"},
			{Size: 15 * 1024 * 1024, Layer: "sha256:layer2"},
		},
	})

	plugin := New(cfg, cache, provider)
	pod := &Pod{
		Name: "test-pod",
		Containers: []Container{
			{Name: "c1", Image: imageName},
		},
	}

	node := "node-1"
	provider.k8sScores[node] = 50

	// Case A: Node has 1 layer cached (15MB > 10MB), CPU=0.3, Mem=0.3 (STD=0)
	// Conditions: Size: 15MB > 10MB (OK), CPU: 0.3 < 0.6 (OK), STD: 0 < 0.16 (OK)
	// -> omega = 2.0, S_layer = (15/30)*100 = 50.0
	// -> S = 2.0 * 50 + 50 = 150
	provider.cachedLayers[node] = []string{"sha256:layer1"}
	provider.metrics[node] = NodeMetrics{CPUUtilization: 0.30, MemUtilization: 0.30}

	scoreA, statusA := plugin.Score(context.Background(), nil, pod, node)
	if !statusA.IsSuccess() {
		t.Fatalf("Score failed: %v", statusA.Message())
	}
	if scoreA != 150 {
		t.Errorf("Case A: expected score 150 (omega=2.0), got %d", scoreA)
	}

	// Case B: Dynamic change - CPU spikes to 0.70 (>= 0.60)
	// -> Condition fails, omega switches dynamically to 0.5
	// -> S = 0.5 * 50 + 50 = 75
	provider.metrics[node] = NodeMetrics{CPUUtilization: 0.70, MemUtilization: 0.70}

	scoreB, statusB := plugin.Score(context.Background(), nil, pod, node)
	if !statusB.IsSuccess() {
		t.Fatalf("Score failed: %v", statusB.Message())
	}
	if scoreB != 75 {
		t.Errorf("Case B (CPU spike): expected score 75 (omega=0.5), got %d", scoreB)
	}

	// Case C: Dynamic change - CPU recovers (0.30), but memory drops to 0.0 -> STD = |0.3 - 0.0|/2 = 0.15 < 0.16
	// -> Condition satisfied again! omega switches back to 2.0
	// -> S = 2.0 * 50 + 50 = 150
	provider.metrics[node] = NodeMetrics{CPUUtilization: 0.30, MemUtilization: 0.0}

	scoreC, statusC := plugin.Score(context.Background(), nil, pod, node)
	if !statusC.IsSuccess() {
		t.Fatalf("Score failed: %v", statusC.Message())
	}
	if scoreC != 150 {
		t.Errorf("Case C (STD recovered): expected score 150 (omega=2.0), got %d", scoreC)
	}

	// Case D: Dynamic change - Node cache evicts layer1 (cached = 0)
	// -> Size <= 10MB condition fails, omega switches to 0.5
	// -> S_layer = 0 -> S = 0.5 * 0 + 50 = 50
	provider.cachedLayers[node] = []string{}

	scoreD, statusD := plugin.Score(context.Background(), nil, pod, node)
	if !statusD.IsSuccess() {
		t.Fatalf("Score failed: %v", statusD.Message())
	}
	if scoreD != 50 {
		t.Errorf("Case D (layer evicted): expected score 50 (omega=0.5), got %d", scoreD)
	}
}

// ---------------------------------------------------------------------
// 4. Score Combination Tests (Dominance, Extremes, Equal)
// ---------------------------------------------------------------------

func TestScoreCombinations(t *testing.T) {
	tests := []struct {
		name       string
		omega      float64
		sLayer     float64
		sK8s       float64
		wantResult float64
	}{
		{
			name:       "S_layer dominates: 100 vs 0 with omega=2.0",
			omega:      2.0,
			sLayer:     100.0,
			sK8s:       0.0,
			wantResult: 200.0,
		},
		{
			name:       "S_layer dominates: 100 vs 0 with omega=0.5",
			omega:      0.5,
			sLayer:     100.0,
			sK8s:       0.0,
			wantResult: 50.0,
		},
		{
			name:       "S_k8s dominates: 0 vs 100 with omega=2.0",
			omega:      2.0,
			sLayer:     0.0,
			sK8s:       100.0,
			wantResult: 100.0,
		},
		{
			name:       "S_k8s dominates: 0 vs 100 with omega=0.5",
			omega:      0.5,
			sLayer:     0.0,
			sK8s:       100.0,
			wantResult: 100.0,
		},
		{
			name:       "Both equal: 50 each with omega=2.0",
			omega:      2.0,
			sLayer:     50.0,
			sK8s:       50.0,
			wantResult: 150.0,
		},
		{
			name:       "Both equal: 50 each with omega=0.5",
			omega:      0.5,
			sLayer:     50.0,
			sK8s:       50.0,
			wantResult: 75.0,
		},
		{
			name:       "Both zero",
			omega:      2.0,
			sLayer:     0.0,
			sK8s:       0.0,
			wantResult: 0.0,
		},
		{
			name:       "Both maximum: 100 each with omega=2.0",
			omega:      2.0,
			sLayer:     100.0,
			sK8s:       100.0,
			wantResult: 300.0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := CalculateFinalScore(tc.omega, tc.sLayer, tc.sK8s)
			if math.Abs(got-tc.wantResult) > 1e-6 {
				t.Errorf("CalculateFinalScore(%v, %v, %v) = %v, want %v", tc.omega, tc.sLayer, tc.sK8s, got, tc.wantResult)
			}
		})
	}
}

// ---------------------------------------------------------------------
// 5. Edge Cases Tests
// ---------------------------------------------------------------------

func TestEdgeCases(t *testing.T) {
	cfg := DefaultConfig()
	cache := NewRegistryCache()
	provider := newMockNodeInfoProvider()
	plugin := New(cfg, cache, provider)

	t.Run("nil pod returns error", func(t *testing.T) {
		score, status := plugin.Score(context.Background(), nil, nil, "node-1")
		if status.IsSuccess() || score != 0 {
			t.Errorf("expected error for nil pod, got score=%d, status=%v", score, status)
		}
	})

	t.Run("empty node name returns error", func(t *testing.T) {
		pod := &Pod{Name: "p1"}
		score, status := plugin.Score(context.Background(), nil, pod, "")
		if status.IsSuccess() || score != 0 {
			t.Errorf("expected error for empty node, got score=%d, status=%v", score, status)
		}
	})

	t.Run("unconfigured nodeProvider returns error", func(t *testing.T) {
		nilPlugin := New(cfg, cache, nil)
		pod := &Pod{Name: "p1"}
		score, status := nilPlugin.Score(context.Background(), nil, pod, "node-1")
		if status.IsSuccess() || score != 0 {
			t.Errorf("expected error for unconfigured nodeProvider, got score=%d, status=%v", score, status)
		}
	})

	t.Run("provider layers error returns error", func(t *testing.T) {
		pErr := newMockNodeInfoProvider()
		pErr.errLayers = errors.New("cri socket timeout")
		plErr := New(cfg, cache, pErr)
		pod := &Pod{Name: "p1"}

		score, status := plErr.Score(context.Background(), nil, pod, "node-1")
		if status.IsSuccess() || score != 0 {
			t.Errorf("expected error on layer fetch failure, got %v", status)
		}
	})

	t.Run("provider metrics error returns error", func(t *testing.T) {
		pErr := newMockNodeInfoProvider()
		pErr.errMetrics = errors.New("cadvisor error")
		plErr := New(cfg, cache, pErr)
		pod := &Pod{Name: "p1"}

		score, status := plErr.Score(context.Background(), nil, pod, "node-1")
		if status.IsSuccess() || score != 0 {
			t.Errorf("expected error on metrics fetch failure, got %v", status)
		}
	})

	t.Run("provider k8s score error returns error", func(t *testing.T) {
		pErr := newMockNodeInfoProvider()
		pErr.errK8s = errors.New("scheduler pipeline failure")
		pErr.metrics["node-1"] = NodeMetrics{}
		plErr := New(cfg, cache, pErr)
		pod := &Pod{Name: "p1"}

		score, status := plErr.Score(context.Background(), nil, pod, "node-1")
		if status.IsSuccess() || score != 0 {
			t.Errorf("expected error on k8s score failure, got %v", status)
		}
	})

	t.Run("pod with no containers or empty images", func(t *testing.T) {
		p := newMockNodeInfoProvider()
		p.metrics["node-1"] = NodeMetrics{CPUUtilization: 0.2, MemUtilization: 0.2}
		p.k8sScores["node-1"] = 50
		pl := New(cfg, cache, p)

		pod := &Pod{
			Name: "empty-pod",
			Containers: []Container{
				{Name: "empty-img", Image: ""},
			},
		}

		score, status := pl.Score(context.Background(), nil, pod, "node-1")
		if !status.IsSuccess() {
			t.Fatalf("unexpected error: %v", status)
		}
		// totalImageBytes = 0 -> S_layer = 0, D_c^n(t) = 0 <= 10MB -> omega = 0.5
		// S = 0.5 * 0 + 50 = 50
		if score != 50 {
			t.Errorf("expected score 50, got %d", score)
		}
	})

	t.Run("image not in cache is treated as 0 cached bytes gracefully", func(t *testing.T) {
		p := newMockNodeInfoProvider()
		p.metrics["node-1"] = NodeMetrics{CPUUtilization: 0.2, MemUtilization: 0.2}
		p.k8sScores["node-1"] = 60
		p.cachedLayers["node-1"] = []string{"sha256:some-layer"}
		pl := New(cfg, cache, p)

		pod := &Pod{
			Name: "unknown-img-pod",
			Containers: []Container{
				{Name: "c1", Image: "unknown/image:latest"},
			},
		}

		score, status := pl.Score(context.Background(), nil, pod, "node-1")
		if !status.IsSuccess() {
			t.Fatalf("unexpected error: %v", status)
		}
		// S = 0.5 * 0 + 60 = 60
		if score != 60 {
			t.Errorf("expected score 60, got %d", score)
		}
	})

	t.Run("multi-container pod aggregation", func(t *testing.T) {
		img1 := "repo/img1:v1"
		img2 := "repo/img2:v1"

		cache.Put(img1, ImageMetadata{
			TotalSize: 20 * 1024 * 1024,
			Layers: []LayerMetadata{
				{Size: 20 * 1024 * 1024, Layer: "sha256:img1-l1"},
			},
		})
		cache.Put(img2, ImageMetadata{
			TotalSize: 20 * 1024 * 1024,
			Layers: []LayerMetadata{
				{Size: 20 * 1024 * 1024, Layer: "sha256:img2-l1"},
			},
		})

		p := newMockNodeInfoProvider()
		// Node has img1 cached, but not img2
		// Cached: 20MB, Total: 40MB -> S_layer = 50.0
		// 20MB > 10MB, CPU=0.2 < 0.6, STD=0 < 0.16 -> omega = 2.0
		p.cachedLayers["node-1"] = []string{"sha256:img1-l1"}
		p.metrics["node-1"] = NodeMetrics{CPUUtilization: 0.2, MemUtilization: 0.2}
		p.k8sScores["node-1"] = 40

		pl := New(cfg, cache, p)
		multiPod := &Pod{
			Name: "multi-pod",
			Containers: []Container{
				{Name: "c1", Image: img1},
				{Name: "c2", Image: img2},
			},
		}

		score, status := pl.Score(context.Background(), nil, multiPod, "node-1")
		if !status.IsSuccess() {
			t.Fatalf("unexpected error: %v", status)
		}
		// S = 2.0 * 50 + 40 = 140
		if score != 140 {
			t.Errorf("expected score 140 for multi-container pod, got %d", score)
		}
	})
}

// ---------------------------------------------------------------------
// 6. Normalization & ScoreExtensions Tests
// ---------------------------------------------------------------------

func TestNormalizeScore(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Normalize = true
	plugin := New(cfg, nil, nil)

	t.Run("empty scores list", func(t *testing.T) {
		status := plugin.NormalizeScore(context.Background(), nil, nil, nil)
		if !status.IsSuccess() {
			t.Errorf("NormalizeScore failed on empty slice: %v", status)
		}
	})

	t.Run("all zero scores remain 0", func(t *testing.T) {
		scores := NodeScoreList{
			{Name: "node-1", Score: 0},
			{Name: "node-2", Score: 0},
		}
		status := plugin.NormalizeScore(context.Background(), nil, nil, scores)
		if !status.IsSuccess() {
			t.Fatalf("NormalizeScore failed: %v", status)
		}
		for _, s := range scores {
			if s.Score != 0 {
				t.Errorf("expected 0, got %d", s.Score)
			}
		}
	})

	t.Run("proportional normalization to MaxNodeScore", func(t *testing.T) {
		scores := NodeScoreList{
			{Name: "node-1", Score: 150},
			{Name: "node-2", Score: 300},
			{Name: "node-3", Score: 75},
		}
		status := plugin.NormalizeScore(context.Background(), nil, nil, scores)
		if !status.IsSuccess() {
			t.Fatalf("NormalizeScore failed: %v", status)
		}

		// Max was 300 -> 300 maps to 100, 150 maps to 50, 75 maps to 25
		if scores[0].Score != 50 {
			t.Errorf("node-1: expected 50, got %d", scores[0].Score)
		}
		if scores[1].Score != 100 {
			t.Errorf("node-2: expected 100, got %d", scores[1].Score)
		}
		if scores[2].Score != 25 {
			t.Errorf("node-3: expected 25, got %d", scores[2].Score)
		}
	})

	t.Run("Normalize disabled leaves scores unchanged", func(t *testing.T) {
		cfgOff := DefaultConfig()
		cfgOff.Normalize = false
		plOff := New(cfgOff, nil, nil)

		scores := NodeScoreList{
			{Name: "node-1", Score: 150},
			{Name: "node-2", Score: 300},
		}
		status := plOff.NormalizeScore(context.Background(), nil, nil, scores)
		if !status.IsSuccess() {
			t.Fatalf("NormalizeScore failed: %v", status)
		}
		if scores[0].Score != 150 || scores[1].Score != 300 {
			t.Errorf("scores should remain unscaled when Normalize=false")
		}
	})
}

// ---------------------------------------------------------------------
// 7. CycleState and Plugin Interface Tests
// ---------------------------------------------------------------------

func TestCycleStateAndPluginMethods(t *testing.T) {
	plugin := New(DefaultConfig(), nil, nil)
	if plugin.Name() != PluginName {
		t.Errorf("Name() = %v, want %v", plugin.Name(), PluginName)
	}
	if plugin.ScoreExtensions() == nil {
		t.Errorf("ScoreExtensions() returned nil")
	}

	state := NewCycleState()
	state.Write("key1", "val1")
	val, ok := state.Read("key1")
	if !ok || val != "val1" {
		t.Errorf("CycleState Read failed: ok=%v, val=%v", ok, val)
	}

	state.Delete("key1")
	_, ok = state.Read("key1")
	if ok {
		t.Errorf("CycleState Delete failed: key1 still present")
	}

	// Nil cycle state safety
	var nilState *CycleState
	nilState.Write("k", "v")
	nilState.Delete("k")
	_, ok = nilState.Read("k")
	if ok {
		t.Errorf("Read on nil CycleState should return false")
	}
}

// ---------------------------------------------------------------------
// 8. RegistryCache and File Persistence Tests
// ---------------------------------------------------------------------

func TestRegistryCache(t *testing.T) {
	cache := NewRegistryCache()
	if cache.Count() != 0 {
		t.Fatalf("expected initial count 0, got %d", cache.Count())
	}

	meta := ImageMetadata{
		Name:      "ubuntu",
		Tag:       "22.04",
		TotalSize: 1000,
		Layers: []LayerMetadata{
			{Size: 600, Layer: "sha256:layerA"},
			{Size: 400, Layer: "sha256:layerB"},
		},
	}
	cache.Put("ubuntu:22.04", meta)

	if cache.Count() == 0 {
		t.Errorf("expected count > 0 after put")
	}

	retrieved, ok := cache.Get("ubuntu:22.04")
	if !ok || retrieved.TotalSize != 1000 {
		t.Errorf("Get failed: ok=%v, size=%d", ok, retrieved.TotalSize)
	}

	// CalculateNodeCachedBytes with all layers present
	cached, total, err := cache.CalculateNodeCachedBytes("ubuntu:22.04", []string{"sha256:layerA", "sha256:layerB"})
	if err != nil || cached != 1000 || total != 1000 {
		t.Errorf("all layers: got cached=%d, total=%d, err=%v", cached, total, err)
	}

	// CalculateNodeCachedBytes with partial layers present
	cached, total, err = cache.CalculateNodeCachedBytes("ubuntu:22.04", []string{"sha256:layerA"})
	if err != nil || cached != 600 || total != 1000 {
		t.Errorf("partial layers: got cached=%d, total=%d, err=%v", cached, total, err)
	}

	// CalculateNodeCachedBytes with no layers present
	cached, total, err = cache.CalculateNodeCachedBytes("ubuntu:22.04", []string{"sha256:other"})
	if err != nil || cached != 0 || total != 1000 {
		t.Errorf("no layers: got cached=%d, total=%d, err=%v", cached, total, err)
	}

	// Non-existent image
	_, _, err = cache.CalculateNodeCachedBytes("nonexistent:latest", []string{})
	if err == nil {
		t.Errorf("expected error for non-existent image")
	}

	// Persistence: SaveToFile and LoadFromFile
	tmpDir := t.TempDir()
	cachePath := filepath.Join(tmpDir, "cache.json")

	if err := cache.SaveToFile(cachePath); err != nil {
		t.Fatalf("SaveToFile failed: %v", err)
	}

	newCache := NewRegistryCache()
	if err := newCache.LoadFromFile(cachePath); err != nil {
		t.Fatalf("LoadFromFile failed: %v", err)
	}

	loadedMeta, ok := newCache.Get("ubuntu:22.04")
	if !ok || loadedMeta.TotalSize != 1000 {
		t.Errorf("LoadFromFile content mismatch: ok=%v, totalSize=%d", ok, loadedMeta.TotalSize)
	}
}

// ---------------------------------------------------------------------
// 9. RegistryWatcher HTTP Polling Mock Test
// ---------------------------------------------------------------------

func TestRegistryWatcherPolling(t *testing.T) {
	// Mock registry server implementing Docker Registry v2 API
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v2/_catalog":
			json.NewEncoder(w).Encode(CatalogResponse{
				Repositories: []string{"library/alpine"},
			})
		case "/v2/library/alpine/tags/list":
			json.NewEncoder(w).Encode(TagListResponse{
				Name: "library/alpine",
				Tags: []string{"3.18"},
			})
		case "/v2/library/alpine/manifests/3.18":
			var m ManifestV2
			m.SchemaVersion = 2
			m.Layers = []struct {
				MediaType string `json:"mediaType"`
				Size      int64  `json:"size"`
				Digest    string `json:"digest"`
			}{
				{MediaType: "application/vnd.docker.image.rootfs.diff.tar.gzip", Size: 3 * 1024 * 1024, Digest: "sha256:alpine-layer1"},
			}
			json.NewEncoder(w).Encode(m)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	cache := NewRegistryCache()
	watcher := NewRegistryWatcher(server.URL, 50*time.Millisecond, cache)

	// Test single synchronous poll
	if err := watcher.PollOnce(context.Background()); err != nil {
		t.Fatalf("PollOnce failed: %v", err)
	}

	meta, ok := cache.Get("library/alpine:3.18")
	if !ok {
		t.Fatalf("Image library/alpine:3.18 not found in cache after PollOnce")
	}
	if meta.TotalSize != 3*1024*1024 {
		t.Errorf("TotalSize = %d, want %d", meta.TotalSize, 3*1024*1024)
	}
	if len(meta.Layers) != 1 || meta.Layers[0].Layer != "sha256:alpine-layer1" {
		t.Errorf("Layers mismatch: %+v", meta.Layers)
	}

	// Test background watcher Start & Stop
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	watcher.Start(ctx)
	time.Sleep(100 * time.Millisecond)
	watcher.Stop()
}
