# LRScheduler: Layer-Aware & Resource-Adaptive Container Scheduler Baseline

This package implements the **LRScheduler** baseline algorithm as a Kubernetes Scheduling Framework `Score` plugin, following the paper:

> **LRScheduler: A Layer-aware and Resource-adaptive Container Scheduler in Edge Computing**  
> *arXiv:2506.03694* (IEEE MSN 2024)

---

## 1. Algorithm Overview & Mathematical Formulation

LRScheduler optimizes container placement in edge environments by balancing **image layer caching** (to minimize download latency and network traffic) against **node resource utilization** (CPU and memory balancing).

### Core Composite Scoring Formula (Eq. 4)
$$S = \omega \cdot S_{\text{Layer}} + S_{\text{K8s}}$$

Where:
- $S_{\text{Layer}}$: The layer-level score representing image layer reuse on the node.
- $S_{\text{K8s}}$: The default Kubernetes system-level score (e.g. `NodeResourcesFit` / LeastAllocated or BalancedAllocation).
- $\omega$: A dynamic weight factor switching between layer prioritization and resource prioritization.

---

### Layer-Level Score $S_{\text{Layer}}$ (Eq. 2 & Eq. 3)
$$\mathcal{D}_c^n(t) = \sum_{l \in \mathbf{L}_c \cap \mathbf{L}_n(t)} d_l$$

$$S_{\text{Layer}} = \left( \frac{\mathcal{D}_c^n(t)}{\sum_{l \in \mathbf{L}_c} d_l} \right) \times 100$$

- $\mathbf{L}_c$: The set of image layers required by container $c$.
- $\mathbf{L}_n(t)$: The set of cached image layers present on node $n$ at scheduling time $t$.
- $d_l$: Byte size of layer $l$.
- $\mathcal{D}_c^n(t)$: Total cached byte size already available on node $n$ for container $c$.
- If $\sum d_l = 0$, $S_{\text{Layer}} = 0$.
- $S_{\text{Layer}} \in [0, 100]$.

---

### Dynamic Weight Switching (Algorithm 1 & Eqs. 11–13)

LRScheduler dynamically adjusts $\omega \in \{\omega_1, \omega_2\} = \{2.0, 0.5\}$ based on three conditions:

1. **Layer Cache Size Threshold ($h_{\text{size}}$):**
   $$\mathcal{D}_c^n(t) > h_{\text{size}}$$
   where $h_{\text{size}} = 10\text{ MB} = 10 \times 1024 \times 1024\text{ bytes} = 10,485,760\text{ bytes}$.
   *(Strictly greater than: $\le 10\text{ MB}$ evaluates to false).*

2. **CPU Utilization Threshold ($h_{\text{CPU}}$):**
   $$S_{\text{CPU}} = \frac{p_n(t)}{p_n} < h_{\text{CPU}}$$
   where $h_{\text{CPU}} = 0.60$ (60% CPU utilization).
   *(Strictly less than: $\ge 0.60$ evaluates to false).*

3. **Resource Balancing Threshold ($h_{\text{STD}}$):**
   $$S_{\text{STD}} = \frac{\left| \frac{p_n(t)}{p_n} - \frac{e_n(t)}{e_n} \right|}{2} < h_{\text{STD}}$$
   where $\frac{p_n(t)}{p_n}$ is CPU utilization ratio and $\frac{e_n(t)}{e_n}$ is Memory utilization ratio.  
   $h_{\text{STD}} = 0.16$.  
   *(Strictly less than: $\ge 0.16$ evaluates to false).*

#### Indicator Function (Eq. 13)
$$S_{\text{Weight}} = \llbracket \mathcal{D}_c^n(t) > h_{\text{size}} \rrbracket \times \llbracket S_{\text{CPU}} < h_{\text{CPU}} \rrbracket \times \llbracket S_{\text{STD}} < h_{\text{STD}} \rrbracket$$

#### Weight Assignment (Algorithm 1)
$$\omega = \begin{cases} \omega_1 = 2.0 & \text{if } S_{\text{Weight}} = 1 \\ \omega_2 = 0.5 & \text{if } S_{\text{Weight}} = 0 \end{cases}$$

---

## 2. Deviations from the Paper Implementation

To integrate cleanly with modern Kubernetes / K3s architectures and avoid anti-patterns from the 2021 prototype, the following deviations are deliberately implemented:

| Aspect | Paper Prototype (arXiv:2506.03694, Sec. V) | This Implementation (`k3snix/baselines/lrscheduler`) | Rationale |
| :--- | :--- | :--- | :--- |
| **Node Daemon Query** | Directly contacted Docker daemon at `http://NodeIP:2375/images/json` over unencrypted HTTP. | Abstracted behind `NodeInfoProvider` interface. | Modern Kubernetes and K3s use `containerd` / CRI rather than Docker daemon on port 2375. The interface supports containerd, Docker, or mock providers. |
| **K8s Dependencies** | Imported monolithic `k8s.io/kubernetes` scheduler packages. | Self-contained Go module (`k3snix/baselines/lrscheduler`) with decoupled `ScorePlugin` and `ScoreExtensions` interfaces. | Prevents pulling hundreds of transitive Kubernetes dependencies into the core `k3snix` module (zero external k8s dependency policy). |
| **Multi-Container Pods** | Equations defined for a single container $c$. | Aggregates cached and total layer sizes across all containers in `Pod.Containers`. | Supports standard Kubernetes multi-container pods without panic or miscalculation. |
| **Registry Manifest Support** | Handled only Docker v2 schema 2 manifests. | Supports both Docker Registry v2 schema 2 and OCI Image Manifest specification (`application/vnd.oci.image.manifest.v1+json`). | Future-proof compatibility with modern OCI distribution registries. |
| **Configurability** | Hardcoded thresholds ($10\text{ MB}$, $0.60$, $0.16$, $\omega_1=2.0$, $\omega_2=0.5$). | Exposes `Config` struct with `DefaultConfig()` supplying exact paper constants. | Allows edge researchers to fine-tune parameters for differing network topologies and hardware constraints. |

---

## 3. Package Structure

```
baselines/lrscheduler/
├── go.mod                 # Standalone Go module (k3snix/baselines/lrscheduler)
├── types.go               # Scheduler framework interfaces, status codes, and data structures
├── registry.go            # RegistryCache, OCI/Docker v2 RegistryWatcher (10s polling), file persistence
├── lrscheduler.go         # Core scoring algorithm, dynamic weight switching, LRScheduler plugin
├── lrscheduler_test.go    # Unit tests (basic scoring, boundaries, dynamic switching, edge cases)
└── README.md              # Mathematical documentation and deviation analysis
```

---

## 4. Running Tests

```bash
cd baselines/lrscheduler
go test -v -race ./...
```

