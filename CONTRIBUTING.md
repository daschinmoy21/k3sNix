# Contributing

## Workflow

Do not push to `main` directly. Open a pull request for every change, even
small ones. Branch off `main`, commit your work, and raise a PR against
`main`.

```sh
git checkout -b <your-branch>
git commit -m "<message>"
git push -u origin <your-branch>
```

PRs are reviewed before merge. Keep PRs focused on one change where
practical.

## Development

Everything is pinned by the flake. Run `direnv allow` to load the dev shell,
or `nix develop` directly.

Before opening a PR, make sure these pass:

```sh
go test ./...
nix build .#k3snix
nix flake check
```

If you touched the NixOS module or closures, rebuild the flake outputs and also run
`nix build .#closures`.
