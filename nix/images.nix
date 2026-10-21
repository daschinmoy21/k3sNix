# OCI images of the pinned workloads, for the layer-level baselines.
#
# Every image is built from the same root store path as the closure manifest
# of its label (workloads.nix), so an image carries exactly the store paths of
# that closure and nothing else: contents is empty, and the root enters the
# image only through the config's Env, whose references pull in its closure.
# The one extra layer buildLayeredImage always adds (the customisation layer,
# here an empty directory) is identical in every image.
#
# Two granularities, both from buildLayeredImage so the tar format, mtimes and
# ownership are the same and only the layering differs:
#   fine   one layer per store path (no closure exceeds maxLayers)
#   coarse every store path in one layer, an explicitly flattened image
{ pkgs }:

let
  lib = pkgs.lib;
  workloads = import ./workloads.nix { inherit pkgs; };

  image =
    granularity: label: root:
    pkgs.dockerTools.buildLayeredImage (
      {
        name = "k3snix-oci/${label}";
        tag = granularity;
        contents = [ ];
        config.Env = [ "K3SNIX_ROOT=${root}" ];
      }
      // lib.optionalAttrs (granularity == "fine") { maxLayers = 100; }
      // lib.optionalAttrs (granularity == "coarse") {
        layeringPipeline = [
          [ "popularity_contest" ]
          [
            "limit_layers"
            1
          ]
        ];
      }
    );

  granularities = [
    "fine"
    "coarse"
  ];
in
lib.genAttrs granularities (g: lib.mapAttrs (image g) workloads)
