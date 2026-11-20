# The store-path versus layer comparison, computed in the sandbox from the
# closure manifests and the OCI images of the same workloads. Output:
#   images/<granularity>/<label>.json   layers of every image (oci-layers)
#   nar-gzip.json                       gzipped NAR size of every store path
#   result.json, tables.md              the comparison (k3snix-bench compare)
# The build fails if an image does not carry exactly its closure's store
# paths, if a NAR differs from its manifest's narSize, or if the nix ladder
# disagrees with summary.json.
{
  pkgs,
  k3snix,
  closures,
  images,
}:

let
  lib = pkgs.lib;
  workloads = import ./workloads.nix { inherit pkgs; };
  imageArgs = lib.concatStringsSep " " (
    lib.flatten (
      lib.mapAttrsToList (g: byLabel: lib.mapAttrsToList (label: img: "${g}:${label}:${img}") byLabel) images
    )
  );
in
pkgs.runCommand "store-path-vs-layer-comparison"
  {
    # The workload paths must be readable for nar-gzip to dump them.
    roots = lib.attrValues workloads;
  }
  ''
    set -euo pipefail
    mkdir -p $out/images
    for spec in ${imageArgs}; do
      IFS=: read -r g label img <<<"$spec"
      mkdir -p $out/images/$g
      ${k3snix}/bin/k3snix-bench oci-layers -label "$label" -granularity "$g" "$img" \
        > $out/images/$g/$label.json
    done
    ${k3snix}/bin/k3snix-bench nar-gzip -manifests ${closures}/manifests > $out/nar-gzip.json
    ${k3snix}/bin/k3snix-bench compare \
      -manifests ${closures}/manifests \
      -images $out/images \
      -nar-gzip $out/nar-gzip.json \
      -summary ${closures}/summary.json \
      -json $out/result.json \
      > $out/tables.md
  ''
