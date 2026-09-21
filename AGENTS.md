# AGENTS.md

Guidance for AI coding agents working in this repository.

## Workflow

- Never push or commit directly to `main`. Raise a pull request for every
  change.
- Branch off `main` for any work; do not commit on `main`.

## Development

- Everything is pinned by the flake; use `nix develop` or `direnv allow`.
- Run `go test ./...` and `nix build .#k3snix` before finishing a task.

## Layout

- `cmd/agent` — per-node DaemonSet (store-path inventory, binary cache)
- `cmd/extender` — kube-scheduler extender (scores nodes by missing NAR bytes)
- `cmd/bench` — placement and fetch model
- `nix/` — NixOS module and closure builders
- `deploy/` — Kubernetes manifests
- `hack/` — development and e2e scripts
