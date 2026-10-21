# K3sNix

Place K3s pods using Nix store-path inventory on each node, and serve missing
paths from a K3s-managed in-cluster binary cache.

This is the implementation repo. The design, evaluation protocol, and results
live in the BTech report.

## Status

Phase 1, the data plane, is complete; see [docs/phase1.md](docs/phase1.md)
for the acceptance checklist and measured results, and
[docs/phases.md](docs/phases.md) for the roadmap.

- Pinned workload closures (churn ladder, glibc-sharing pair, unrelated
  pair) with manifests, content-derived closure IDs and a catalog
- `k3snix-agent` per node: store inventory, `POST /v1/missing_bytes`, and a
  Nix binary cache (`/nix-cache-info`, `*.narinfo`, `nar/*.nar`) for peers
- A LAN origin cache: the agent in `-catalog-only` mode with signed narinfo,
  and `POST /v1/origin_probe` to measure it
- A three-node k3d smoke and a demo on the real closures

The scheduler extender (`cmd/extender`) is a Phase 2 stub. The bench
(`cmd/bench`) implements the byte comparison. Nothing fetches store paths on a
pod start yet.

## Layout

- `cmd/agent` — per-node DaemonSet: store-path inventory, missing-bytes API, Nix binary-cache server
- `cmd/extender` — kube-scheduler extender: scores feasible nodes by missing NAR bytes
- `cmd/bench` — placement and fetch model, crossover sweeps
- `internal/` — packages behind the binaries
- `nix/` — NixOS module and closure builders
- `deploy/` — Kubernetes manifests
- `hack/` — development and e2e scripts
- `testdata/` — fixtures

## Development

Everything is pinned by the flake. Run `direnv allow` to load the dev shell, or
`nix develop` directly.

```sh
go test ./...
nix build .#k3snix
nix run .#agent
hack/e2e-k3d.sh             # three-node k3d smoke (needs Docker)
hack/demo-real-closures.sh  # missing bytes and NAR parity on the real closures
nix build .#comparison      # store paths vs OCI layers on the same workloads
nix develop -c bash hack/k3s-docker-live.sh # k3snix rule vs real K3s + Docker, fine/coarse
nix develop -c bash hack/compare-live.sh    # supporting nix copy / docker pull validation
```

[docs/comparison.md](docs/comparison.md) describes the k3snix versus normal
K3s with Docker comparison, its method, live results and limitations.

The dev shell provides Go, k3d, k3s, kubectl, minikube, the Docker client, jq,
skopeo, and Python.

## Dependencies

- `nixpkgs` (nixos-unstable)
- `pdtpartners/nix-snapshotter` — optional container rootfs path for one benchmark arm, not vendored (see `docs/evaluation.md`)
