{
  pkgs,
  nixpkgsRev ? "",
}:

let
  base = pkgs.runCommand "churn-base" { } ''
    mkdir -p $out
    dd bs=1024 count=8192 if=/dev/zero of=$out/blob
  '';

  versionDir =
    v:
    pkgs.runCommand "churn-version-v${v}" { } ''
      mkdir -p $out
      echo v${v} > $out/version
    '';

  payload =
    kib:
    pkgs.runCommand "churn-payload-${toString kib}" { } ''
      mkdir -p $out
      dd bs=1024 count=${toString kib} if=/dev/zero of=$out/payload-${toString kib}
    '';

  churn =
    v: extra:
    pkgs.symlinkJoin {
      name = "churn-v${v}";
      paths = [
        base
        (versionDir v)
      ] ++ extra;
    };

  churnV1 = churn "1" [ ];
  churnV2 = churn "2" [ (payload 200) ];
  churnV3 = churn "3" [
    (payload 200)
    (payload 272)
  ];
  churnV4 = churn "4" [
    (payload 200)
    (payload 272)
    (payload 352)
  ];

  unrelatedA = pkgs.runCommand "unrelated-a" { } ''
    dd bs=1024 count=64 if=/dev/zero of=$out
  '';

  unrelatedB = pkgs.runCommand "unrelated-b" { } ''
    dd bs=1024 count=96 if=/dev/zero of=$out
  '';

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
    exportReferencesGraph = {
      "churn-v1" = [ churnV1 ];
      "churn-v2" = [ churnV2 ];
      "churn-v3" = [ churnV3 ];
      "churn-v4" = [ churnV4 ];
      "share-a" = [ pkgs.hello ];
      "share-b" = [ pkgs.coreutils ];
      "unrelated-a" = [ unrelatedA ];
      "unrelated-b" = [ unrelatedB ];
    };
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
          churn: {
            steps: [
              {
                from: "churn-v1",
                to: "churn-v2",
                delta: ($b2 - $b1),
                prevBytes: $b1,
                ratioPct: (($b2 - $b1) / $b1 * 10000 | round / 100)
              },
              {
                from: "churn-v2",
                to: "churn-v3",
                delta: ($b3 - $b2),
                prevBytes: $b2,
                ratioPct: (($b3 - $b2) / $b2 * 10000 | round / 100)
              },
              {
                from: "churn-v3",
                to: "churn-v4",
                delta: ($b4 - $b3),
                prevBytes: $b3,
                ratioPct: (($b4 - $b3) / $b3 * 10000 | round / 100)
              }
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
