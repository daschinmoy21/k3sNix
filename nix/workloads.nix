# The pinned test workloads, keyed by label. closures.nix records their
# manifests and images.nix packs the same roots into OCI images, so both
# delivery designs carry byte-identical store paths.
{ pkgs }:

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
in
{
  "churn-v1" = churnV1;
  "churn-v2" = churnV2;
  "churn-v3" = churnV3;
  "churn-v4" = churnV4;
  "share-a" = pkgs.hello;
  "share-b" = pkgs.coreutils;
  "unrelated-a" = unrelatedA;
  "unrelated-b" = unrelatedB;
}
