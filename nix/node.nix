# The one NixOS node configuration every benchmark cluster uses
# (docs/evaluation.md, Clusters). A node is a variant: an arm and a runtime.
# Clusters add only hardware and network settings: the disks, the boot
# loader, and which interface is the private one.
#
# Arms differ mostly at cluster level: the scheduler, the extender and the
# agent DaemonSet are manifests applied by the harness. On the node an arm
# only decides whether the host store is prepared for pods (GC roots for the
# agent), and every node carries its arm and runtime as labels so the
# harness can check what it is measuring.
#
# The k3snix-snapshotter arm is not a variant here. It depends on the #1
# spike; if that succeeds, the arm becomes a variant that also imports
# nixosModules.k3s-with-snapshotter.
{
  config,
  lib,
  pkgs,
  ...
}:
let
  cfg = config.k3snix.node;

  hostStoreArms = [
    "default-hoststore"
    "k3snix"
  ];
  hostStore = lib.elem cfg.arm hostStoreArms;

  registryPort =
    if cfg.registry == null then null else lib.toInt (lib.last (lib.splitString ":" cfg.registry));
in
{
  options.k3snix.node = {
    enable = lib.mkEnableOption "the k3snix benchmark node configuration";

    arm = lib.mkOption {
      type = lib.types.enum (
        [
          "vanilla"
          "lrscheduler"
        ]
        ++ hostStoreArms
      );
      description = "Benchmark arm this node runs (docs/evaluation.md, Arms).";
    };

    runtime = lib.mkOption {
      type = lib.types.enum [
        "containerd"
        "docker"
      ];
      default = "containerd";
      description = ''
        Container runtime: K3s's embedded containerd, or Docker through the
        bundled cri-dockerd (`--docker`), which matches the LRScheduler paper.
      '';
    };

    role = lib.mkOption {
      type = lib.types.enum [
        "server"
        "agent"
      ];
      description = "K3s role. Every cluster has one server and at least three agents.";
    };

    serverAddr = lib.mkOption {
      type = lib.types.str;
      default = "";
      example = "https://10.0.0.10:6443";
      description = "The server's private address; required on agents.";
    };

    tokenFile = lib.mkOption {
      type = lib.types.path;
      example = "/run/keys/k3s-token";
      description = "File holding the cluster join token; kept out of the store.";
    };

    privateInterface = lib.mkOption {
      type = lib.types.str;
      example = "wg0";
      description = ''
        Interface of the private network between nodes (the VPC NIC, or the
        WireGuard interface on netcup). Flannel runs over it, and the
        cluster, agent and registry ports are opened on it alone.
      '';
    };

    nodeIP = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      description = "Private address the node advertises; null lets K3s pick it from the interface.";
    };

    registry = lib.mkOption {
      type = lib.types.nullOr lib.types.str;
      default = null;
      example = "10.0.0.10:5000";
      description = ''
        host:port of the in-cluster plain-HTTP registry serving the baseline
        OCI images. Both runtimes are told to pull from it over HTTP.
      '';
    };
  };

  config = lib.mkIf cfg.enable {
    assertions = [
      {
        assertion = cfg.role == "agent" -> cfg.serverAddr != "";
        message = "k3snix.node: an agent needs serverAddr";
      }
      {
        assertion = cfg.registry == null || builtins.match "[^:/]+:[0-9]+" cfg.registry != null;
        message = "k3snix.node.registry must be host:port, got ${toString cfg.registry}";
      }
    ];

    services.k3s = {
      enable = true;
      inherit (cfg) role serverAddr tokenFile;
      nodeIP = cfg.nodeIP;
      nodeLabel = [
        "k3snix.io/arm=${cfg.arm}"
        "k3snix.io/runtime=${cfg.runtime}"
      ];
      # The same components on every arm; ingress and the service load
      # balancer are unused and only add noise to the measurements.
      disable = lib.optionals (cfg.role == "server") [
        "traefik"
        "servicelb"
      ];
      extraFlags = [
        "--flannel-iface=${cfg.privateInterface}"
      ]
      ++ lib.optional (cfg.runtime == "docker") "--docker";
      # Nothing is evicted during a run: the harness resets the stores and
      # image caches between runs (#60), so the kubelet's image GC must not
      # remove baseline images mid-run while nix keeps every store path.
      extraKubeletConfig = {
        imageGCHighThresholdPercent = 100;
        imageGCLowThresholdPercent = 99;
      };
    };

    virtualisation.docker = lib.mkIf (cfg.runtime == "docker") {
      enable = true;
      daemon.settings.insecure-registries = lib.optional (cfg.registry != null) cfg.registry;
    };

    environment.etc."rancher/k3s/registries.yaml" =
      lib.mkIf (cfg.registry != null && cfg.runtime == "containerd")
        {
          text = builtins.toJSON {
            mirrors.${cfg.registry}.endpoint = [ "http://${cfg.registry}" ];
          };
        };

    # Host-store arms: the agent registers fetched paths through the nix
    # daemon and keeps one root per pod here (-gcroots-dir), so
    # nix-collect-garbage never removes a path a running pod needs.
    systemd.tmpfiles.rules = lib.optional hostStore "d /nix/var/nix/gcroots/k3snix 0755 root root -";
    nix.settings.experimental-features = [ "nix-command" ];
    # As with the kubelet's image GC: stores are reset between runs, never
    # collected during one.
    nix.gc.automatic = false;

    boot.kernelModules = [
      "br_netfilter"
      "overlay"
    ];
    # The kubelet refuses to start with swap on.
    swapDevices = lib.mkForce [ ];

    networking.firewall = {
      enable = true;
      # Flannel's VXLAN traffic fails the strict reverse-path check.
      checkReversePath = "loose";
      # Pods reach the agent on the node IP (an init container posts to
      # HOST_IP:9860) and each other through these.
      trustedInterfaces = [
        "cni0"
        "flannel.1"
      ];
      # Only SSH is open publicly; everything else listens on the private
      # network alone (docs/evaluation.md, Clusters).
      allowedTCPPorts = [ 22 ];
      interfaces.${cfg.privateInterface} = {
        allowedTCPPorts = [
          10250 # kubelet
          9860 # k3snix agent: peer cache and missing_bytes
        ]
        ++ lib.optional (cfg.role == "server") 6443
        ++ lib.optional (registryPort != null) registryPort;
        allowedUDPPorts = [ 8472 ]; # flannel VXLAN
      };
    };
  };
}
