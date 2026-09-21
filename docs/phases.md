# Phase Roadmap — What We Build in Each Phase

The architecture deck splits the system into three phases. This document lists
the build items per phase, the exit criteria, and how the deck phases map to the
execution phases in spec.md. Phase 1 has its own detailed plan in
[phase1.md](./phase1.md).

## Phase 0 prerequisite — feasibility gate

One item from spec.md must be settled before or during Phase 1, because Phases 2
and 3 depend on it: nix-snapshotter running a real pod rootfs on a multi-node
K3s cluster. The flake already wires the module, but nobody has proven it end to
end. Treat it as an early spike that can run in parallel with Phase 1. If it
fails, the delivery leg changes and every later phase is affected.

## Phase 1 — data plane: closure analysis and cache inventory

Build items:

- [ ] Pinned flake builds the workload closures and emits path-info manifests
      with path, narSize, narHash, and references
- [ ] Content-derived closure IDs (a fingerprint over the path list plus sizes)
      and the catalog that maps IDs to manifests
- [ ] Go parser for nix path-info JSON and the project format, plus diff and
      missing computations
- [ ] Node agent inventory: TTL-refreshed have-set over `/nix/store`, the
      missing-bytes API, and nothing sent to the API server
- [ ] Node agent peer serving: nix-cache-info, narinfo, and NAR endpoints;
      complete NARs only; coalesced dumps; `nix-store --dump` with a pure-Go
      fallback
- [ ] Controlled LAN origin cache, preloaded and instrumented, with live origin
      counters
- [ ] Three workload families with measured deltas: the churn ladder v1 to v4, a
      glibc-sharing pair, and an unrelated negative control
- [ ] Smoke e2e on three k3d nodes seeded warm, mid, and cold

Exit criteria are the acceptance checklist in [phase1.md](./phase1.md). Exit
demo: every node answers missing_bytes correctly for its state and a peer serves
a hash-verifiable NAR, with no scheduling code in the tree.

## Phase 2 — control plane: cache-aware scheduling and delta fetch

Box 3, cache-aware scheduling. Build items:

- [ ] Extender binary implementing the kube-scheduler extender verbs: filter as
      a pass-through, and prioritize rescoring feasible nodes 0 to 10 from
      missing NAR bytes
- [ ] Hand-rolled extender JSON types so the Go module keeps zero k8s
      dependencies
- [ ] A second kube-scheduler Deployment with a KubeSchedulerConfiguration
      profile named k3snix, the extender URL, weight, timeout, and RBAC; pods
      opt in with `schedulerName: k3snix`
- [ ] Fallback semantics: an agent timeout or error scores 0 and never blocks
      the queue, and extender failure leaves default scheduling
- [ ] Score and decision-latency traces logged per scheduling cycle
- [ ] Resource fit stays in the loop so cache attraction does not overload the
      warm node (anti-heating remains a stretch goal)

Box 4, delta fetch and lazy mount. Build items:

- [ ] Agent fetch path: resolve the missing set, then try peers first and the
      origin second
- [ ] NAR downloads verified against the narinfo hash before they become
      visible; a partial NAR is never published
- [ ] Concurrent fetches of one path coalesce into a single transfer
- [ ] Fetch-side counters completed: origin bytes and requests, peer bytes,
      local hits, and predicted versus actually fetched missing bytes
- [ ] Materialization lands paths in `/nix/store` so the Phase 3 runtime can
      mount them; pod-start mount behaviour itself belongs to Phase 3

Exit criteria:

- [ ] On a constructed skew, a pod with `schedulerName: k3snix` binds to the
      warm node, and the decision is explained by logged scores
- [ ] A scheduled node ends the run holding the closure, having fetched only the
      missing NARs, peers before origin, proven by counters and a store diff
- [ ] The spec.md section 7 gates that touch these components pass: coalescing,
      no partial NAR visibility, and default scheduling on extender failure

## Phase 3 — runtime and benchmark

Box 5, runtime. Build items:

- [ ] nix-snapshotter live on K3s through the k3s-with-snapshotter module (the
      Phase 0 spike made real)
- [ ] Pod rootfs assembled from store paths with no image copy, and a
      per-container writable upper layer
- [ ] Pod writes never mutate `/nix/store`, and removing a pod never deletes
      paths another pod requires

Box 6, benchmark. Build items:

- [ ] Harness with adapters for B0 through B4, and B5 once the extender lands;
      one command per backend
- [ ] Store procedures as scripts, not prose: cold node, warm node, skew
      constructed from a real v1 deploy, and per-run store snapshots published
- [ ] Metrics per spec.md section 6.5: origin and peer bytes, local hits,
      predicted versus fetched missing bytes, time-to-ready p50, p95, and p99,
      schedule latency, warm-node CPU, cache-hit ratio, and disk bytes in
      `/nix/store` versus the containerd content store
- [ ] Deterministic traces: version churn, cross-application sharing, unrelated
      closures, and a pod burst
- [ ] Result schema and table generator, randomized backend order, and repeated
      runs with reported dispersion
- [ ] Crossover sweeps against B2 and B4 that name the region where store-path
      scoring wins and the region where it does not

Exit criteria:

- [ ] Every backend produces a results-table row from one command on a pinned
      cluster
- [ ] All correctness gates of spec.md section 7 pass
- [ ] The report contains crossover charts explained from byte counters rather
      than a single speedup percentage

## How the deck phases map to spec.md execution phases

| Deck phase | spec.md phases | Notes |
|---|---|---|
| Phase 1 | Phase 2 (agent, serving half) plus the closure work | fetch path deliberately excluded |
| Phase 2 | Phase 3 (extender) plus the fetch half of spec Phase 2 | B2's layer scorer also belongs in this window |
| Phase 3 | Phase 0 gate, Phase 1 harness, Phase 4 crossover | runtime gate first, then measurement |

The two phase systems disagree on numbering, and the deck's Benchmark box covers
what spec.md calls the harness and the crossover evaluation. Reconcile the
wording before the viva so the panel hears one story.

## Sequencing notes

- Phase 2 needs Phase 1's catalog and serving; do not start scoring before
  serving exists.
- The B2 baseline and the harness are the long poles; start them during Phase 2
  rather than after Phase 3, because spec.md forbids a results chapter without
  B2.
- The snapshotter spike runs in parallel with Phase 1 and blocks only Phase 3.
