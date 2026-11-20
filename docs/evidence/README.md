# Comparison evidence

These files preserve the 2026-10-21 local byte experiment described in
[comparison.md](../comparison.md). They do not measure startup time or
execution across multiple physical machines.

- `compare-live.tsv`: 24 Nix copies and 48 Docker pulls. Its historical
  `model raw`, `live raw`, and `units` fields count workload content only;
  `live wire` counts every payload layer, including the shared path-less
  layer when absent. Future script headers name these scopes explicitly.
- `k3s-docker-live.tsv`: 48 k3snix-rule decisions, 96 default-scheduler
  decisions, and 48 pinned Docker costs. Both OCI byte columns include
  path-less overhead. Each free decision has the same cost as the pinned
  measurement for that state.
- `*.nodes-{fine,coarse}.json`: six seed snapshots per style, with worker
  assignments, registration timestamps, Docker runtime versions and image
  sizes. States rotate across fixed workers; each state occupies worker-1
  twice. This balances inputs, not the scheduler's observed choices.
- `*.image-sizes.json` and `*.model.json`: the actual Docker/CRI sizes and
  the model rerun with those sizes and four total nodes.
- `*.metadata.json`: the run's HEAD, dirty-tree status, diff fingerprints
  and measurement-harness fingerprint. This is a snapshot of an uncommitted
  run, not a fingerprint of the final reviewed tree. Generated evidence,
  documentation, and the explicitly marked user host-load report were added
  afterwards. The measurement script itself still matches its recorded hash.
- `*.summary.md`: regenerated from the main TSV. Free-run averages are
  labeled observed means. Uniform-placement expectations are not inferred.

## Verification after the run

The continuation review independently checked all 192 main rows against
the saved runtime model, all 12 seed snapshots, worker/state assignments,
runtime sizes, zero ImageLocality scores and the measurement-script hash.
The regenerated summary verifies row counts, balanced input rotation,
model/raw equality, and agreement between free and pinned costs.

Two Grok reviews covered the experiment and the comparison code. Follow-up
changes omit offline ImageLocality scoring without runtime sizes, reject
inconsistent fine-layer diffIDs for a shared store path, clarify supporting
byte-column scopes, and replace “expected” with “observed mean.” These
changes do not alter the recorded main measurements. The Docker experiment
was not rerun during this verification.

Recreate the summary with:

```sh
nix develop -c python3 hack/summarize-k3s-docker.py docs/evidence/k3s-docker-live.tsv
```
