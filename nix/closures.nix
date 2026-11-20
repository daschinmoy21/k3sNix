{
  pkgs,
  nixpkgsRev ? "",
}:

let
  workloads = import ./workloads.nix { inherit pkgs; };

  labels = [
    "churn-v1"
    "churn-v2"
    "churn-v3"
    "churn-v4"
    "share-a"
    "share-b"
    "unrelated-a"
    "unrelated-b"
  ];
in
pkgs.runCommand "closure-manifests"
  {
    __structuredAttrs = true;
    nativeBuildInputs = [ pkgs.jq ];
    exportReferencesGraph = builtins.mapAttrs (_: root: [ root ]) workloads;
    inherit nixpkgsRev;
  }
  ''
    set -euo pipefail

    manifests="$out/manifests"
    mkdir -p "$manifests"

    for label in ${pkgs.lib.concatStringsSep " " labels}; do
      root=$(jq -r --arg l "$label" '.exportReferencesGraph[$l][0]' "$NIX_ATTRS_JSON_FILE")
      jq -c --arg label "$label" --arg root "$root" '
        {
          label: $label,
          root: $root,
          paths: (
            [.[$label][] | { path, narSize, narHash, references: (.references // []) }]
            | sort_by(.path)
          )
        }
      ' "$NIX_ATTRS_JSON_FILE" > "$manifests/$label.json"
    done

    declare -A bytes=()
    for label in ${pkgs.lib.concatStringsSep " " labels}; do
      bytes[$label]=$(jq -r '[.paths[].narSize] | add' "$manifests/$label.json")
    done

    jq -n \
      --arg rev "$nixpkgsRev" \
      --slurpfile m1 "$manifests/churn-v1.json" \
      --slurpfile m2 "$manifests/churn-v2.json" \
      --slurpfile m3 "$manifests/churn-v3.json" \
      --slurpfile m4 "$manifests/churn-v4.json" \
      --slurpfile sa "$manifests/share-a.json" \
      --slurpfile sb "$manifests/share-b.json" \
      --slurpfile ua "$manifests/unrelated-a.json" \
      --slurpfile ub "$manifests/unrelated-b.json" '
      def storeName: capture("^/nix/store/[a-z0-9]{32}-(?<name>.+)$").name;
      def sums: { pathCount: (.paths | length), bytes: ([.paths[].narSize] | add) };
      def versionOf($f; $prefix):
        [$f.paths[].path | storeName | select(startswith($prefix)) | ltrimstr($prefix)]
        | first // "";
      # Bytes a node holding $a must fetch to hold $b: the narSize of every
      # path in $b that $a lacks. It exceeds the closure-size delta because
      # the root path changes on every version too.
      def transfer($a; $b):
        ([$a.paths[].path]) as $have
        | [$b.paths[] | select(.path as $p | $have | index([$p]) | not) | .narSize]
        | add // 0;
      def step($from; $to; $a; $b; $prev; $next):
        transfer($a; $b) as $t
        | {
            from: $from,
            to: $to,
            delta: ($next - $prev),
            prevBytes: $prev,
            ratioPct: (($next - $prev) / $prev * 10000 | round / 100),
            transferBytes: $t,
            transferPct: ($t / $next * 10000 | round / 100)
          };
      def intersect($a; $b):
        ([$a.paths[].path] | sort) as $ap
        | ([$b.paths[].path] | sort) as $bp
        | ($ap - ($ap - $bp))
        | { count: length, names: (map(storeName) | unique) };
      {
        "churn-v1": ($m1[0] | sums),
        "churn-v2": ($m2[0] | sums),
        "churn-v3": ($m3[0] | sums),
        "churn-v4": ($m4[0] | sums),
        "share-a": ($sa[0] | sums),
        "share-b": ($sb[0] | sums),
        "unrelated-a": ($ua[0] | sums),
        "unrelated-b": ($ub[0] | sums)
      } as $labels
      | $labels["churn-v1"].bytes as $b1
      | $labels["churn-v2"].bytes as $b2
      | $labels["churn-v3"].bytes as $b3
      | $labels["churn-v4"].bytes as $b4
      | {
          nixpkgsRev: $rev,
          labels: $labels,
          # delta and ratioPct compare closure sizes (the 1-5% ladder the
          # build checks); transferBytes and transferPct are what a node on
          # the previous version actually fetches, as a share of the new
          # closure. fromV1 does the same from a node holding only churn-v1.
          churn: {
            steps: [
              step("churn-v1"; "churn-v2"; $m1[0]; $m2[0]; $b1; $b2),
              step("churn-v2"; "churn-v3"; $m2[0]; $m3[0]; $b2; $b3),
              step("churn-v3"; "churn-v4"; $m3[0]; $m4[0]; $b3; $b4)
            ],
            fromV1: [
              step("churn-v1"; "churn-v2"; $m1[0]; $m2[0]; $b1; $b2),
              step("churn-v1"; "churn-v3"; $m1[0]; $m3[0]; $b1; $b3),
              step("churn-v1"; "churn-v4"; $m1[0]; $m4[0]; $b1; $b4)
            ]
          },
          sharing: {
            shared: intersect($sa[0]; $sb[0]),
            unrelatedShared: intersect($ua[0]; $ub[0])
          },
          versions: {
            hello: versionOf($sa[0]; "hello-"),
            coreutils: versionOf($sb[0]; "coreutils-")
          }
        }
      ' > "$out/summary.json"

    fail=0
    check_step() {
      local from="$1" to="$2" delta prev
      delta=$(( ''${bytes[$to]} - ''${bytes[$from]} ))
      prev=''${bytes[$from]}
      if [ $(( delta * 100 )) -lt "$prev" ] || [ $(( delta * 100 )) -gt $(( prev * 5 )) ]; then
        echo "FAIL: churn $from -> $to: delta=$delta prev=$prev (need delta*100 in [prev, prev*5])" >&2
        fail=1
      fi
    }
    check_step churn-v1 churn-v2
    check_step churn-v2 churn-v3
    check_step churn-v3 churn-v4

    sharedCount=$(jq -r '.sharing.shared.count' "$out/summary.json")
    sharedGlibc=$(jq -r '[.sharing.shared.names[] | select(test("glibc"))] | length > 0' "$out/summary.json")
    unrelatedShared=$(jq -r '.sharing.unrelatedShared.count' "$out/summary.json")

    if [ "$sharedCount" -eq 0 ]; then
      echo "FAIL: share-a and share-b share no paths" >&2
      fail=1
    fi
    if [ "$sharedGlibc" != "true" ]; then
      echo "FAIL: shared paths between share-a and share-b contain no glibc" >&2
      fail=1
    fi
    if [ "$unrelatedShared" -ne 0 ]; then
      echo "FAIL: unrelated-a and unrelated-b share $unrelatedShared paths" >&2
      fail=1
    fi

    if [ "$fail" -ne 0 ]; then
      exit 1
    fi
  ''
