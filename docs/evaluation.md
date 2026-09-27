# Evaluation Plan — Arms, Clusters, and Metrics

This document fixes what the final benchmark compares, where it runs, and what
it measures. It refines Phase 3 of [phases.md](./phases.md).

The question the benchmark answers: **does placing pods by missing Nix store
bytes, and delivering those bytes from peers and a LAN origin, start pods
faster and move fewer bytes than vanilla K3s and than a layer-aware scheduler
(LRScheduler)?** It also answers a second question: **does k3snix need
nix-snapshotter, or does a plain host store do as well?** The preferred
outcome is the second: same or better numbers without nix-snapshotter.

## Delivery designs

Two ways to get a pod's software onto a node are compared.

**OCI images (baselines).** The workload is an OCI image pulled from a registry
inside the cluster. The runtime unpacks its layers into its own image store.

**Host store (k3snix, preferred).** The workload is a Nix closure. No
snapshotter and no containerd changes are involved:

1. The pod names its root store path (for example `/nix/store/…-app`). The
   extender resolves it through the catalog, whose manifests record `root`.
   The closure ID is still the fingerprint, never an annotation value.
2. The extender scores nodes by missing NAR bytes.
3. An init container asks the node agent to fetch the missing paths, peers
   first and origin second. The agent verifies each NAR against its narinfo
   before the path becomes visible, and adds a GC root for the pod.
4. The app container runs from a small shared image with the host
   `/nix/store` mounted **read-only**, and executes the store path directly.
5. On pod deletion the agent removes the pod's GC root.

Consequences to state in the report:

- Every pod can read the whole host store (read-only). Isolation is weaker
  than nix-snapshotter's per-pod rootfs. A CSI driver that mounts only the
  pod's closure is a stretch goal.
- The workload namespace needs a Pod Security level that allows `hostPath`.
- The agent needs the host nix-daemon socket to add paths and GC roots.
- Nodes need multi-user Nix, not NixOS.

**nix-snapshotter (optional run).** The pod rootfs is assembled from store
paths by the nix-snapshotter containerd plugin. It needs NixOS (the pinned
version has only a NixOS module and a rootless Home Manager module; its manual
install guide is empty) and works with containerd only.

## Arms

| Arm | Scheduler | Delivery | Runtimes |
|---|---|---|---|
| `vanilla` | default kube-scheduler (includes ImageLocality) | OCI images | containerd, docker |
| `lrscheduler` | LRScheduler, reimplemented | OCI images | containerd, docker |
| `default-hoststore` | default kube-scheduler | host store | containerd, docker |
| `k3snix` | k3snix extender | host store | containerd, docker |
| `k3snix-snapshotter` | k3snix extender | nix-snapshotter | containerd |

`default-hoststore` separates the two effects: `k3snix` against it is the gain
from placement, and it against `vanilla` is the gain from delivery. Without it
a win cannot be attributed to either.

The docker runtime is K3s started with `--docker` (bundled cri-dockerd). It
matches the LRScheduler paper, which used Docker 20.10. Runtimes are never
mixed within a comparison: every arm is compared against the others on the
same runtime.

**Decision rule for nix-snapshotter.** If `k3snix` on the host store matches
`k3snix-snapshotter` within run-to-run dispersion on time-to-ready and bytes
moved, the host store is the recommended design and the snapshotter run is
reported as a comparison. The #1 spike is time-boxed; if nix-snapshotter does
not run on K3s within it, the `k3snix-snapshotter` arm is dropped and the
report says why.

## LRScheduler baseline

No source code is published, so it is reimplemented from the paper
(arXiv 2506.03694, MSN 2024) as a scheduling-framework Score plugin in a
separate Go module (`baselines/lrscheduler/`). The main module keeps zero
Kubernetes dependencies. An extender cannot reproduce the score faithfully
because extender scores are capped at 10 and rescaled.

- Layer score: `S_layer = (bytes of the pod image's layers present on the node
  / total image bytes) × 100`
- Final score: `S = ω · S_layer + S_k8s`
- Dynamic weight: `ω = ω1 = 2` when node load is low and balanced, else
  `ω = ω2 = 0.5`, with the paper's thresholds `h_size = 10`, `h_CPU = 0.6`,
  `h_STD = 0.16`
- Layer metadata polled from the registry's `/v2/_catalog` every 10 s, as in
  the paper

Deviations from the paper are recorded next to the code: K3s instead of
Kubernetes 1.23, containerd as well as Docker, and any detail the paper leaves
open.

## Workloads and images

All arms run the same software: the pinned closures from `nix/closures.nix`
(churn ladder v1 to v4, the glibc-sharing pair, the unrelated pair) plus a pod
burst. OCI images for the baselines are built from those same closures in two
granularities:

- **fine**: `dockerTools.buildLayeredImage`, about one layer per store path
  (up to its layer limit). This is the strongest case for LRScheduler.
- **coarse**: a single-layer image, like a typical Dockerfile build.

Granularity is a crossover axis: k3snix's placement advantage over LRScheduler
is expected to be small on fine images and larger on coarse ones.

## Clusters

| Cluster | Machines | Network | Used for |
|---|---|---|---|
| `local` | NixOS VMs on the dev machine (k3d for the Phase 1 smoke) | virtual, same host | correctness only; no reported timings |
| `netcup` | netcup VPS nodes | WireGuard between nodes | commodity VPS numbers, limited bandwidth |
| `gcp` | n2 VMs in one zone, internal IPs | VPC | clean numbers and the bandwidth sweep |

Rules:

- One NixOS node configuration in the flake, with one variant per arm and
  runtime, is shared by every cluster. Only hardware and network settings
  differ. NixOS is required only for `k3snix-snapshotter`, but using it on
  every node keeps the OS constant.
- At least one server and three workers per cluster (the paper used one and
  four).
- The agent (9860), origin, and registry listen only on the private network.
  Nothing is exposed on a public IP.
- GCP clusters are created by script and deleted after each session. No spot
  VMs for measured runs.
- Between runs, node stores, runtime image stores, and the registry are reset
  to the scenario's starting state.

The flake is `x86_64-linux` only, so every node is x86-64.

## Metrics

Per run, per pod:

- time to ready (p50, p95, p99)
- bytes from origin or registry, bytes from peers, local hits
- download time
- scheduling latency per decision
- disk bytes in `/nix/store` against the runtime's image store

Per run, per cluster:

- resource balance (standard deviation of node utilisation), as in the paper
- CPU on the warmest node
- predicted against actually fetched missing bytes (k3snix arms)

Every result records the cluster, node specs, measured RTT and bandwidth
between nodes, runtime, arm, image granularity, scenario, and the git commit.

## Run plan

- `local`: every arm and runtime once, as a correctness pass.
- `netcup`: every arm on both runtimes, all scenarios, repeated runs in
  randomised arm order.
- `gcp`: as `netcup`, plus a bandwidth sweep (0 to 30 Mbps with `tc`, matching
  the paper's range) on the churn and sharing scenarios.

The report presents crossover charts explained by byte counters, not a single
speedup figure.

## Open questions

- How these arms map to baselines B0 to B5 in spec.md (report repository).
  `lrscheduler` is presumably B2's layer scorer and `vanilla` B0; the harness
  items in phases.md use the B numbers until reconciled.
- netcup node count: one VPS cannot host a three-node cluster by itself.
