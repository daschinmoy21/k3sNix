# OCI Baseline — Bytes Comparison Against Docker Images

This plan compares k3snix's store-path delivery with ordinary OCI images on the
same software and the same cluster, and measures the bytes a node must pull. It
is the part of [docs/evaluation.md](./docs/evaluation.md) that can run now, on
one machine, before the scheduler exists. Timings wait for the multi-node
cluster.

The question: **for the same workloads, how many bytes does a node that already
runs one of them need to run another, with Nix store paths versus OCI images?**

## The claim this should support

1. Against typical images (one coarse layer, as from a Dockerfile), store-path
   delivery moves far less data on version updates and between applications
   that share libraries.
2. Against the best case for OCI, images built by Nix with about one layer per
   store path, transfer is roughly a tie on small closures. The gap reopens when
   a closure has more paths than the layer limit.
3. Where transfer ties, the advantage is placement: the stock `ImageLocality`
   score only rewards a node holding the whole image, so it cannot see that a
   node already holds most of an image's files. That is measured in Phase 2.

We report where each side wins, ties or loses. The report does not say k3snix is
better everywhere, because the fine-image arm shows it is not.

## Arms

| Arm | What a workload ships as | Built by |
|---|---|---|
| `nix` | its closure's store paths, as NARs | `nix/closures.nix` (existing) |
| `oci-fine` | OCI image, about one layer per store path | `dockerTools.buildLayeredImage` |
| `oci-coarse` | OCI image, a single layer | `dockerTools.buildImage` with the whole closure in one layer |

All three are built from the same pinned closures, so every arm ships exactly
the same files. Only the packaging differs.

## Fairness rules

- **Same software.** Images are built from the closures in `nix/closures.nix`,
  from the same nixpkgs revision. No base image is added to either side.
- **Same compression.** OCI layers are normally gzip-compressed; the agent
  serves uncompressed NARs. Every number is reported both uncompressed and
  gzip-compressed (NARs compressed with the same gzip level as the layers).
- **Same starting state.** A node starts with exactly the previous workload, in
  its own arm's form, and nothing else. Stores and image caches are reset
  between runs.
- **Same cluster.** On-cluster runs use the same k3d cluster and the same
  containerd for both arms. Images come from a k3d registry on the local
  network, so neither arm touches the internet.
- **Same measure.** Transfer bytes, defined below, for both arms.

## Measure

**Nix.** The missing NAR bytes of the target closure on a node holding the
source closure, as `/v1/missing_bytes` already reports it.

**OCI.** The bytes of the target image's layers whose digest is not already on
the node, plus the image manifest and config.

Both come out as a value and as a percentage of the target's full size. The
percentage base is the same for every arm: the target's full size in that arm.

Two ways of measuring OCI transfer, which must agree:

1. **Offline,** from the image manifests: layer digests and sizes are known
   once the images are built, so the matrix is computed without pulling
   anything. This matches the simulated node stores of
   `hack/demo-real-closures.sh`.
2. **On the cluster:** a k3d node pulls the source image, then the target image,
   and the bytes it fetched are read from the registry's access log
   (`http.response.written`) per blob. This confirms that containerd really
   reuses the layers the offline count assumes it reuses.

## Scenarios

| Scenario | Source → target | What it shows |
|---|---|---|
| Version update | churn v1→v2, v2→v3, v3→v4, v1→v3, v1→v4 | Small changes inside a mostly unchanged app |
| Shared libraries | hello → coreutils, coreutils → hello | Unrelated apps sharing glibc and friends |
| Negative control | unrelated-a → unrelated-b | No overlap; all arms must report 100% |
| Large closure | an app with more than 100 store paths, v1 → v2 (to be chosen) | Fine images hit `buildLayeredImage`'s layer limit and merge paths |

The large-closure workload is added to `nix/closures.nix`. It must exceed
`buildLayeredImage`'s default `maxLayers = 100`, and its v2 should change one
low-level dependency, so the change lands in the merged last layer.

## Expected results

| Scenario | `nix` | `oci-fine` | `oci-coarse` |
|---|---|---|---|
| churn v1 → v2 | ~2.4% | about the same: the base path is its own layer and is reused | ~100% |
| hello → coreutils | ~26% | about the same: the glibc layer has the same digest in both images | ~100% |
| unrelated pair | 100% | 100% | 100% |
| large closure | small | larger: the change sits in the merged last layer | ~100% |

If `oci-fine` does not tie on the small scenarios, find out why before
reporting it (for example, non-reproducible layer tarballs) rather than
counting it as a win.

## Deliverables

- `nix/images.nix`: the `oci-fine` and `oci-coarse` image of every workload,
  exposed as flake outputs next to `closures`.
- `hack/oci-baseline.sh`: builds the images, computes the offline matrix for all
  three arms, compressed and uncompressed, then runs the on-cluster pulls on k3d
  for a subset and fails if they disagree with the offline numbers.
- A `summary.json` and a markdown table side by side with the `nix` numbers,
  ready for the report.

## Not in scope here

- **Timings.** Loopback on one machine says nothing about network transfer;
  time-to-ready is measured on the multi-node cluster ([docs/evaluation.md](./docs/evaluation.md)).
- **Placement.** The extender and the LRScheduler baseline belong to Phase 2.
- **The docker runtime.** k3d nodes run containerd; K3s with `--docker` needs
  real nodes, so that run happens on the VPS cluster.
- **Peer distribution of OCI layers** (for example a P2P registry). It belongs
  to the full benchmark if it is kept as a baseline.

## Open questions

- Which app to use for the large-closure scenario, and how to make its v2.
- Whether to also include a real Dockerfile image (for example Debian plus
  `apt install hello`) as a "typical image" arm. It is realistic but does not
  ship exactly the same files, so it would be reported separately.
