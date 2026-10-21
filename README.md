# K3sNix

Place K3s pods using Nix store-path inventory on each node, and serve missing
paths from a K3s-managed in-cluster binary cache.

This is the implementation repo. The design, evaluation protocol, and results
live in the BTech report.

## Status

Skeleton. The three entry points exist and build; no behaviour is implemented yet.

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
```

The dev shell provides Go, k3d, k3s, kubectl, minikube, the Docker client, jq,
and Python.

## Dependencies

- `nixpkgs` (nixos-unstable)
- `pdtpartners/nix-snapshotter` — optional container rootfs path for one benchmark arm, not vendored (see `docs/evaluation.md`)
