# k3snix versus normal K3s with Docker

The headline comparison is **k3snix placement and store-path delivery versus
stock K3s scheduling and Docker image delivery**, using exactly the same
pinned software in both arms. Docker images are measured in both fine
(one store path per layer) and coarse (flattened) forms.

The vanilla arm is a real four-node K3s cluster with Docker on every node.
The k3snix arm applies the documented extender rule to **live agents' missing
NAR byte answers**, then runs stock `nix copy`. The extender, pod-start fetch
path and running workloads remain Phase 2 work. This is a live placement and
transfer-byte check, not an end-to-end k3snix cluster or a startup benchmark.

## Live cluster comparison

`hack/k3s-docker-live.sh` constructs the following isolated state:

| Node | Seeded software | K3s role |
|---|---|---|
| `node-control` | no workload images | server, `node-role.kubernetes.io/control-plane:NoSchedule` |
| `node-worker-1` | rotates churn-v1 / share-a / empty | agent, first worker registered |
| `node-worker-2` | rotates share-a / empty / churn-v1 | agent, second worker registered |
| `node-worker-3` | rotates empty / churn-v1 / share-a | agent, third worker registered |

Each node is a privileged `docker.io/library/docker:29-dind` container with
its own dockerd and anonymous Docker data volume, on a private Docker
network. The pinned upstream K3s static binary (`.#k3s-static`) is mounted
read-only as one file. No host `/nix/store` is mounted. One `k3s server
--docker` and three `k3s agent --docker` processes use embedded cri-dockerd;
the server runs the unmodified default kube-scheduler. All four nodes must
report `docker://` and be Ready. The three workers must be schedulable,
untainted, and report equal allocatable CPU and memory. The neutral server
is excluded by its NoSchedule taint and never retains workload images.

Workers register serially, with distinct `metadata.creationTimestamp`
values. Before each repetition the worker have-sets rotate, rather than
rebuilding the cluster. The script verifies the full timestamp order is
control / worker-1 / worker-2 / worker-3, and records that order and the
state-to-worker assignment. Over six default repetitions each of the three
states occupies the first worker position exactly twice. `K3SNIX_K3SD_RUNS`
must be a positive multiple of three. State names (`node-churn-v1`,
`node-hello`, `node-empty`) in the result identify have-sets, independently
of physical worker names.

Traefik, servicelb, metrics-server, local-storage, CoreDNS and the network
policy controller are disabled. Flannel remains. The sandbox image is
pre-pulled and every infrastructure layer diffID is checked against all
workload diffIDs: no overlap is allowed. Infrastructure transfers are excluded.
The host must identify itself as Docker Engine; Podman shims fail the check.
K3s `--docker` is the upstream [cri-dockerd runtime option](https://docs.k3s.io/advanced#using-docker-as-the-container-runtime).

For each target and repetition:

- **k3snix:** create three fresh Nix stores, seed them from a signed,
  catalog-only agent origin, and start one real inventory agent per store.
  POST the same target manifest to all three `/v1/missing_bytes` endpoints.
  Require every answer to equal the model, choose the least missing bytes,
  record all ties, and copy to the first tied node alphabetically. Measure
  the origin's `k3snix_nar_bytes_served` counter delta, then delete all stores.
- **Vanilla, for each image style:** create a Pod with `default-scheduler`,
  `IfNotPresent`, no affinity and no `nodeName`. Record the actual binding
  and wait for the image to exist in the chosen dockerd. Images have no
  command, so the container need not start; measurement Pods use zero
  termination grace. After the registry log settles,
  check the exact layer blob GET set and the sum of `http.response.written`
  against the pushed manifest and the chosen node's model transfer. Delete
  the Pod, wait until kubelet no longer desires its UID, remove its runtime
  containers and target image (unless it was the seed). Require three
  consecutive snapshots of image IDs **and** layer diffIDs equal to the seed. Fail on any
  unexpected bytes or unrestored state.
- **Pinned vanilla:** in the first repetition only, also submit a Pod with
  `spec.nodeName` set to each worker in turn. This bypasses scheduling and
  independently measures the cost if that target lands on each state, with
  the same pull validation and full reset. These 48 deterministic costs
  (8 targets × 3 states × 2 styles) are reported beside the free decisions.

Before another Pod is submitted, every node's Docker inventory and the
kubelet's published workload-image inventory must match the seed for three
consecutive snapshots, including after the last Pod. Kubelet
status updates are set to 1 s to make that reset observable promptly;
the scheduler configuration is unchanged. Its actual reported image sizes
are checked against ImageLocality's floor. A disposable Docker daemon pulls
all images from the same registry to capture `docker image inspect .Size`.
The model is rerun with those runtime sizes and four total nodes. Every
measured target's Docker size must match its probe size; seeded images'
`Node.status.images.sizeBytes` must also match before every decision.
The floor calculation includes the tainted server, because ImageLocality
uses all nodes in its snapshot, including filtered nodes
([upstream lines 70–78](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/scheduler/framework/plugins/imagelocality/image_locality.go#L70-L78)).
The entire cluster and its data volumes are recreated between fine and coarse images.

The Nix side has a private `XDG_CACHE_HOME`, disabled substituters and zero
narinfo cache TTLs. The OCI side uses a fresh registry, a private skopeo HOME
and policy, and direct archive-to-registry pushes; workloads never enter the
host Docker image store. Only our uniquely prefixed `k3snix-k3sd-` resources
and temporary directories are removed by the exit trap.

### Results

Measured on 2026-10-21 with K3s **v1.36.4+k3s1**, worker Docker
**29.9.1**, host Docker Engine **29.8.1**, and **R=6**. All 192 measurements
passed, including 48 k3snix decisions, 96 free Pods and 48 pinned Pods.

The [raw TSV](./evidence/k3s-docker-live.tsv),
[generated summary](./evidence/k3s-docker-live.tsv.summary.md),
[fine node snapshots](./evidence/k3s-docker-live.tsv.nodes-fine.json),
[coarse node snapshots](./evidence/k3s-docker-live.tsv.nodes-coarse.json),
[runtime image sizes](./evidence/k3s-docker-live.tsv.image-sizes.json),
[live model](./evidence/k3s-docker-live.tsv.model.json) and
[metadata](./evidence/k3s-docker-live.tsv.metadata.json) are saved together.
Node snapshots include actual image `sizeBytes`, creation timestamps,
registration order and each repetition's seed assignment. Metadata records
HEAD, real Git status, staged/unstaged diff fingerprints and the script hash;
this run used uncommitted changes. The comparison derivation records the
base image/NAR units, while the saved live model applies runtime sizes and
four total nodes. Post-run metadata annotations preserve the user's host-load
report separately from measurements made by the harness.

C / H / E = churn-v1 / hello / empty states; seed states rotate across fixed worker identities.
Free distribution counts choices; observed means describe the recorded runs per target/style, not a scheduler expectation.
Pinned costs measure each state once per target/style. Raw = NAR / whole-image layer tar bytes.
K3snix ties are listed; the first node alphabetically is copied to.

#### Raw bytes

| Target | k3snix choice (ties) | k3snix bytes | Fine free C/H/E | Fine observed mean | Fine pinned C/H/E | Coarse free C/H/E | Coarse observed mean | Coarse pinned C/H/E |
|---|---|---:|---|---:|---|---|---:|---|
| churn-v1 | churn-v1 | 0 | 1 / 3 / 2 | 7,017,813.3 | 0 / 8,417,280 / 8,427,520 | 2 / 1 / 3 | 5,602,986.7 | 0 / 8,396,800 / 8,407,040 |
| churn-v2 | churn-v1 | 206,248 | 2 / 1 / 3 | 5,838,506.7 | 235,520 / 8,632,320 / 8,642,560 | 3 / 1 / 2 | 8,605,013.3 | 8,601,600 / 8,601,600 / 8,611,840 |
| churn-v3 | churn-v1 | 485,336 | 2 / 2 / 2 | 6,123,520.0 | 522,240 / 8,919,040 / 8,929,280 | 0 / 3 / 3 | 8,893,440.0 | 8,888,320 / 8,888,320 / 8,898,560 |
| churn-v4 | churn-v1 | 846,344 | 2 / 3 / 1 | 6,490,453.3 | 890,880 / 9,287,680 / 9,297,920 | 2 / 2 / 2 | 9,250,133.3 | 9,246,720 / 9,246,720 / 9,256,960 |
| share-a | hello | 0 | 5 / 0 / 1 | 38,729,386.7 | 38,727,680 / 0 / 38,737,920 | 4 / 2 / 0 | 25,804,800.0 | 38,707,200 / 0 / 38,717,440 |
| share-b | hello | 13,334,096 | 0 / 2 / 4 | 39,069,013.3 | 51,855,360 / 13,475,840 / 51,865,600 | 2 / 2 / 2 | 51,797,333.3 | 51,793,920 / 51,793,920 / 51,804,160 |
| unrelated-a | churn-v1,empty,hello | 65,648 | 1 / 2 / 3 | 76,800.0 | 71,680 / 71,680 / 81,920 | 4 / 1 / 1 | 73,386.7 | 71,680 / 71,680 / 81,920 |
| unrelated-b | churn-v1,empty,hello | 98,416 | 2 / 2 / 2 | 105,813.3 | 102,400 / 102,400 / 112,640 | 2 / 3 / 1 | 104,106.7 | 102,400 / 102,400 / 112,640 |

#### Actual wire bytes (agent uncompressed; registry gzip)

| Target | k3snix choice (ties) | k3snix bytes | Fine free C/H/E | Fine observed mean | Fine pinned C/H/E | Coarse free C/H/E | Coarse observed mean | Coarse pinned C/H/E |
|---|---|---:|---|---:|---|---|---:|---|
| churn-v1 | churn-v1 | 0 | 1 / 3 / 2 | 10,211.8 | 0 / 12,213 / 12,316 | 2 / 1 / 3 | 8,007.5 | 0 / 11,934 / 12,037 |
| churn-v2 | churn-v1 | 206,248 | 2 / 1 / 3 | 8,877.5 | 1,036 / 12,721 / 12,824 | 3 / 1 / 2 | 12,406.3 | 12,372 / 12,372 / 12,475 |
| churn-v3 | churn-v1 | 485,336 | 2 / 2 / 2 | 9,435.3 | 1,611 / 13,296 / 13,399 | 0 / 3 / 3 | 12,947.5 | 12,896 / 12,896 / 12,999 |
| churn-v4 | churn-v1 | 846,344 | 2 / 3 / 1 | 10,073.2 | 2,266 / 13,951 / 14,054 | 2 / 2 / 2 | 13,559.3 | 13,525 / 13,525 / 13,628 |
| share-a | hello | 0 | 5 / 0 / 1 | 13,974,941.2 | 13,974,924 / 0 / 13,975,027 | 4 / 2 / 0 | 9,311,524.0 | 13,967,286 / 0 / 13,967,389 |
| share-b | hello | 13,334,096 | 0 / 2 / 4 | 14,379,374.0 | 19,011,666 / 5,114,584 / 19,011,769 | 2 / 2 / 2 | 19,008,275.3 | 19,008,241 / 19,008,241 / 19,008,344 |
| unrelated-a | churn-v1,empty,hello | 65,648 | 1 / 2 / 3 | 333.5 | 282 / 282 / 385 | 4 / 1 / 1 | 299.2 | 282 / 282 / 385 |
| unrelated-b | churn-v1,empty,hello | 98,416 | 2 / 2 / 2 | 331.3 | 297 / 297 / 400 | 2 / 3 / 1 | 314.2 | 297 / 297 / 400 |

192/192 live measurements matched the model and restored seeded state (48 k3snix, 96 free, 48 pinned).

In kube-scheduler v1.36.4, equal scores break by feasible-list order
(approximately the first registered feasible worker), **not a random draw**
when no extender is configured. `Randomizer = rand.Int()` is assigned only
in the extender-scoring branch
([schedule_one.go line 1043](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/scheduler/schedule_one.go#L1039-L1046));
the heap compares only total score and that randomizer
([lines 1087–1090](https://github.com/kubernetes/kubernetes/blob/v1.36.4/pkg/scheduler/schedule_one.go#L1087-L1090)).
Parallel filtering can change feasible-list order. The
previous harness made the first registered server the churn-v1 state,
biasing placement toward that state. The neutral server and balanced seed
rotation remove that fixed association. The reported free-run means describe only these six observations per
target/style. Rotation balances the input states, but does not guarantee
equal choices: the first registered state won only 52 of 96 free decisions.
These means do not estimate a general stock-K3s placement distribution.
Pinned costs show the cost of every possible state independently of that
distribution. Their arithmetic mean would describe a hypothetical uniform
choice among the three states; uniform selection is not established here.

ImageLocality gives all workers zero here. It checks the requested image
name, so an unseeded target gets no credit for shared layers. For exact
seeded-image hits (churn-v1 and share-a), runtime image size is multiplied
by its spread over all four nodes, then mapped from 23–1000 MiB onto
0–100; both hits remain below the floor. A larger exact image could score
above zero, while partial layer reuse still would not earn credit. Both
the runtime-sized model and live floor check verify this. Fine share-a reports
54,495,811 bytes through Docker/CRI; its full compressed layer transfer is
13,975,027 bytes. Runtime image sizes are not transfer sizes.

Raw savings describe content reuse and placement. They are **not network
savings**: the current agent serves uncompressed NARs, while skopeo pushes
gzipped layers. The wire table reports this difference honestly. Churn is
zero-filled and compresses almost away; real coreutils gives the meaningful
compression comparison. Equal-compressor gzip numbers remain in the model.

## How to reproduce

```sh
nix develop -c nix build .#comparison
nix develop -c cat result/tables.md
nix develop -c bash hack/k3s-docker-live.sh       # R=6; saves docs/evidence/; needs Docker Engine
# Save the TSV, summary, runtime evidence and metadata:
K3SNIX_K3SD_RUNS=6 K3SNIX_K3SD_OUT=docs/evidence/k3s-docker-live.tsv \
  nix develop -c bash hack/k3s-docker-live.sh
nix develop -c bash hack/compare-live.sh          # supporting 72-transfer check
```

Optional ports are `K3SNIX_K3SD_AGENT_PORT` (default 19880, plus the next
three ports for inventories) and `K3SNIX_K3SD_REGISTRY_PORT` (19890).
The registry listens on host loopback only. Its container also listens on
5000 inside the private Docker network. The static binary must match
`nix develop -c k3s --version`; the script fails if the flake changes without
updating its pinned URL and hash. Infrastructure Docker tags are mutable;
recorded runtime versions, rather than tag names, identify this run.

## Same workload and missing-byte computation

### Same software, 1:1

The workloads are the eight pinned closures from Phase 1 (`nix/workloads.nix`):
the churn ladder v1 to v4, the sharing pair (GNU hello and coreutils, which
share glibc, libidn2, libunistring and libgcc) and the unrelated pair.

Both delivery designs are built from **the same derivations**:

- `nix/closures.nix` records each workload's manifest: every store path with
  its narSize and narHash.
- `nix/images.nix` packs the same root into an OCI image with
  `dockerTools.buildLayeredImage`. The image has no other contents; the root
  enters only through the config's `Env`, so the image carries exactly the
  root's closure.

`k3snix-bench compare` checks this before it computes anything. For every
label and both granularities, the set of store paths found inside the
image's layers must equal the manifest's path set: nothing missing and
nothing extra. Fine images must hold exactly one store path per layer, and
coarse images exactly one layer of store paths. The build fails otherwise.

Moving `nix/workloads.nix` out of `closures.nix` did not change anything: the
closures derivation still has the same output path
(`/nix/store/ga29li47…-closure-manifests`), so every Phase 1 number still
applies.

### Three delivery designs

| Design | Unit | Unit ID | Unit size |
|---|---|---|---|
| `nix` | store path | store path (input-addressed here) | NAR bytes (`narSize`) |
| `oci-fine` | layer, one per store path | layer diffID (sha256 of the tar) | layer tar bytes |
| `oci-coarse` | one layer for the whole closure | layer diffID | layer tar bytes |

`oci-fine` uses `buildLayeredImage`'s default layering (popularity contest,
`maxLayers = 100`). Since no closure here has more than 10 paths, it produces
one layer per path. `oci-coarse` uses the same builder with the layering
pipeline `[["popularity_contest"], ["limit_layers", 1]]`. So the two images
differ only in layering: the tar format, mtimes and ownership are the same.
`oci-coarse` is an explicitly **flattened** baseline, like a squashed image
or `dockerTools.buildImage`. It is not a model of a Dockerfile build: a
Dockerfile can keep its dependencies in an earlier layer and change only an
application layer, which reuses more than `oci-coarse` does. The two designs
bracket layer-based delivery: `oci-fine` is the best case and `oci-coarse`
the worst.

### Same logic

There is one missing-bytes function for both, `closure.MissingAgainst`: the
code the k3snix agent runs for `POST /v1/missing_bytes`. A design only decides
what the units are (`compare.Inputs.Units`). A node's have-set is the union of
the unit IDs of everything it holds (`compare.Inputs.Have`). Units are
identified by the pinned store path or layer sha256. A unit shared by two workloads is held once, exactly as `/nix/store` and a
container runtime's content store dedupe.

Sizes are compared two ways:

- **raw**: NAR bytes against layer tar bytes, the content each side moves
  before compression;
- **gzip**: both run through Go's `compress/gzip` at the default level, the
  same compressor on both sides. `nar-gzip` dumps each store path with the
  pure-Go NAR writer and checks that its size equals the manifest's narSize
  before compressing it.

### The path-less layer

`buildLayeredImage` always adds one *customisation layer*. With no contents
it is an empty directory: 10,240 bytes raw and 101 bytes gzipped, and the same
diffID (`sha256:f003e077…`) in every image. `Check` verifies that it really is
the same everywhere. It carries no workload content, but if it were counted,
the unrelated pair would look 12% shared and a hello node would get coarse-layer
credit for churn. It is therefore left out of the OCI rows and reported
separately as per-image overhead. The live check does count it, and confirms
that a node which holds any image does not fetch it again.

## Supporting byte model

`nix develop -c nix build .#comparison` runs in the Nix sandbox. `oci-layers` hashes every
archive layer and checks the config diffIDs; `nar-gzip` dumps and checks NAR
sizes before compressing; `compare` checks the closure/image path equality
and churn bytes independently against `closures.nix`'s `summary.json`.
Outputs include `result.json`, `tables.md`, per-image layer lists and NAR gzip
sizes. `placement[].transferByNode` includes whole-image missing bytes on
every node, computed through the same `closure.MissingAgainst` function as
the content matrix and live agent API.

### Version update: the churn ladder

The bytes a node on the previous version must fetch:

| Step | nix | oci-fine | oci-coarse |
|---|---|---|---|
| v1 → v2 | 206,248 B (2.40%) | 235,520 B (2.73%) | 8,601,600 B (100%) |
| v2 → v3 | 280,248 B (3.16%) | 307,200 B (3.44%) | 8,888,320 B (100%) |
| v3 → v4 | 362,440 B (3.92%) | 389,120 B (4.19%) | 9,246,720 B (100%) |
| v1 → v4 | 846,344 B (9.16%) | 890,880 B (9.59%) | 9,246,720 B (100%) |

Gzipped, v1 → v2 is 780 B for nix, 988 B for oci-fine and 9,069 B for
oci-coarse. The churn payloads are zero-filled, so they compress almost
entirely away: **the gzip numbers for churn say nothing about real
software**, and only the raw numbers should be quoted for this workload.

### Shared libraries: coreutils on a node that runs hello

| | nix | oci-fine | oci-coarse |
|---|---|---|---|
| raw | 13,334,096 of 51,052,016 B (26.1%) | 13,475,840 of 51,855,360 B (26.0%) | 51,793,920 of 51,793,920 B (100%) |
| gzip | 4,969,140 of 18,138,668 B (27.4%) | 4,972,954 of 18,165,777 B (27.4%) | 18,164,835 B (100%) |
| units | 6 of 10 paths | 6 of 10 layers | 1 of 1 layer |

This is real software (glibc 2.42, coreutils 9.11), so the gzip row is
meaningful: compressed, fine layers and NARs are within 0.1% of each other.

### Negative control

The unrelated pair shares nothing in any design: 100% missing on every node.

## Other baselines (model only)

`result.json` also models `default-hoststore` (no placement signal, all nodes
tie) and LRScheduler's byte-only layer term over whole images. Neither runs
in the live cluster. Fine layers make the layer scorer choose the same
nodes as k3snix for churn updates and shared-library targets. Coarse images
share only the path-less layer, so both seeded nodes can tie while fetching
the whole workload. LRScheduler's load terms and dynamic weight are omitted;
this is not a full implementation of that scheduler. The full model
placement table remains in `tables.md`. Offline results omit the vanilla
ImageLocality arms until `-runtime-image-sizes` is provided. The saved live
model includes those arms with the actual Docker/CRI sizes.

## Supporting transfer validation

The model is a computation. `hack/compare-live.sh` checks it against real
transfers made by each ecosystem's stock client, on the same nodes and
workloads.

| | Nix side | OCI side |
|---|---|---|
| Origin | `k3snix-agent -catalog-only`, signed narinfo | `registry:2`, images pushed from their Nix-built archives with `skopeo` |
| Client | stock `nix copy` | stock `docker pull` in a Docker-in-Docker daemon (Docker Engine 29) |
| Node | a fresh, empty Nix store directory | a fresh dind daemon with an empty image store |
| Bytes measured at | the origin: `k3snix_nar_bytes_served` | the origin: the registry's access log (`http.response.written` of layer blob GETs) |
| Also checked | the new valid paths in the store | the exact set of blobs fetched |

For each of the 3 nodes and 8 targets (and, on the OCI side, both
granularities), the script:

1. creates a fresh node and checks that it is empty;
2. seeds it with what the node holds, using the same client and the same
   origin, and checks that the store holds exactly that closure;
3. reads the origin's counter, pulls the target, and reads the counter
   again;
4. requires the difference to equal the model:
   - Nix: NAR bytes equal to the model's raw missing bytes, and new paths
     equal to the model's missing path count;
   - OCI: the blob set the registry sent equal to the layers the target has
     and the seed images lack (by diffID); the uncompressed size of the
     content layers among them equal to the model's raw missing bytes; and
     their count equal to the model's missing layer count;
5. deletes the node.

### Keeping the two sides from contaminating each other

- **Fresh state for every measurement.** A new Nix store directory, or a
  new dind container with its own anonymous volume. It is deleted with
  `docker rm -v` afterwards, so no layer survives into the next measurement.
- **One source each.** `nix copy` runs with `--from` the agent and no
  substituters, and Nix's narinfo disk cache is redirected to a private
  `XDG_CACHE_HOME` with positive and negative TTLs of 0. dind pulls only from
  the test registry.
- **The OCI side never uses the host's Nix store or Docker image store for
  the workloads.** Archives go from the Nix build straight to the registry
  with `skopeo` (with a private `HOME`, so its blob-info cache starts empty);
  nothing is `docker load`ed on the host. The only host-Docker images are the
  test infrastructure (`registry:2`, `docker:29-dind`).
- **The Nix side never talks to Docker.**
- **Real Docker, not a shim.** The script requires `docker version` to report
  a Docker Engine server and fails on Podman. On this machine Docker Engine
  29.8.1 is the system daemon and `podman.dockerCompat` is off.

### Live results

Run on 2026-10-21 (Docker Engine 29.8.1 host, `docker:29-dind` nodes
reporting Docker 29.9.1 with the containerd image store, `registry:2` 2.8.3,
Nix 2.35.2): **72 of 72 transfers matched the model exactly**, 24 by `nix copy`
and 48 by `docker pull`.

Selected rows (bytes; "wire" is what the origin actually sent):

| Design | Node → target | Model (raw) | Live (raw) | Live (wire) | Units fetched |
|---|---|---|---|---|---|
| nix | churn-v1 → churn-v2 | 206,248 | 206,248 | 206,248 | 3 paths |
| oci-fine | churn-v1 → churn-v2 | 235,520 | 235,520 | 1,036 | 3 layers |
| oci-coarse | churn-v1 → churn-v2 | 8,601,600 | 8,601,600 | 12,372 | 1 layer |
| nix | hello → coreutils | 13,334,096 | 13,334,096 | 13,334,096 | 6 paths |
| oci-fine | hello → coreutils | 13,475,840 | 13,475,840 | 5,114,584 | 6 layers |
| oci-coarse | hello → coreutils | 51,793,920 | 51,793,920 | 19,008,241 | 1 layer |
| nix | empty → coreutils | 51,052,016 | 51,052,016 | 51,052,016 | 10 paths |
| oci-fine | empty → coreutils | 51,855,360 | 51,855,360 | 19,011,769 | 10 layers |

What each column proves:

- **Nix side.** "Live raw" is the agent's `k3snix_nar_bytes_served` counter
  for that pull alone, an independent measurement, and the new paths in the
  store are counted. Both equal the model.
- **OCI side.** The registry log shows which blobs were sent and how many
  bytes ("live wire"). The script requires that set to be exactly the layers
  the model says are missing, and the wire bytes to equal the sizes in the
  pushed manifest. "Live raw" is the uncompressed tar size of exactly those
  blobs. On an empty node the wire total also includes the 103-byte gzipped
  path-less layer (for example 385 against 282 bytes for unrelated-a), which
  a node holding any image already has.

**Wire bytes differ because of compression, not sharing.** The registry sends
gzipped layers, and the k3snix agent currently serves NARs uncompressed. For
real software (coreutils on the hello node) that is 13.3 MB from the agent
against 5.1 MB from the registry. For the zero-filled churn payloads the gap
is extreme: 206 KB against 1 KB. With the same compressor on both sides (the
gzip columns of the model), fine layers and NARs are within 0.1%. **Serving
compressed NARs** (`Compression: zstd` or `xz` in the narinfo, as
`cache.nixos.org` does) is therefore a Phase 2 item: without it, the wire
comparison against a registry is unfair to k3snix.

The [saved 72-transfer table](./evidence/compare-live.tsv) is also printed
by the script; `K3SNIX_CMP_OUT=file` saves another run as TSV.

In that historical table, `model raw`, `live raw` and `units` cover workload
content only; `live wire` includes the path-less overhead blob on empty
Docker nodes. These columns deliberately describe different scopes. Future
script output labels them `model content raw`, `live content raw`,
`content units` and `live whole-image wire` to make that distinction explicit.
The 192-measurement main table uses whole-image raw and wire sizes throughout.

## Limits

- **Uncontrolled host load.** The user reported Overwatch running on the
  host and approximately 17% overall CPU usage during part of this run.
  The harness did not record host utilization. Equal-score choices depend
  on parallel-filter completion order, so the distribution and observed mean
  bytes describe this run under its host load. Pinned byte costs and
  registry/NAR validation remain conditional on the measured have-set.
- **Bytes, not time.** Transfer time, unpacking, and pod start time need the
  running workloads, the fetch path, and the multi-machine evaluation of Phase 2 and 3.
- **Two extremes, not Dockerfiles.** `oci-fine` (Nix-built, one store path
  per layer, fixed mtimes) is the best case for layer reuse, and `oci-coarse`
  (flattened) the worst. A Dockerfile image groups many packages per layer
  and can keep a dependency layer across versions while replacing only its
  application layer. Whether a rebuilt layer keeps its diffID depends on
  whether the build reproduces the same bytes (timestamps, package
  versions). A representative Dockerfile baseline with the same payload is
  future work.
- **ImageLocality sizes.** The offline model omits vanilla scoring without
  runtime sizes: neither raw tar size nor a compressed layer sum reliably
  matches CRI accounting. This live run supplies `-runtime-image-sizes`
  (granularity → label → bytes) and `-image-locality-nodes 4`. Its model
  records the size source, the node count, and each scored image size;
  sizes are cross-checked against live cri-dockerd Node.status. No equality
  between compressed layer sums and Docker image sizes is assumed. Every
  scaled seeded size here is below the floor. Larger images could behave
  differently.
- **Small closures.** At most 10 paths, so `oci-fine` never reaches the
  100-layer limit. A closure larger than `maxLayers` would be merged into
  shared layers and lose sharing; it is not modelled here.
- **Synthetic churn.** The churn payloads are zeros. Raw bytes are
  meaningful; their gzip sizes are not.
- **Wire compression differs between the stacks.** The registry sends layers
  gzipped by skopeo, while the agent sends NARs uncompressed. The gzip
  columns compare the two with one compressor; the live table reports what
  each stack actually sent.
- **Metadata is left out.** narinfo on one side, manifests and config blobs on
  the other: a few hundred bytes to a few KiB per workload.
