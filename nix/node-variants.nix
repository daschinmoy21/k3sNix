# Evaluates every node variant (each arm on each runtime, as server and as
# agent) with stand-in hardware, and checks what makes the variants differ.
# Only evaluation is forced: the check depends on no system closure, so it
# stays cheap in CI.
{
  pkgs,
  nixpkgs,
  nodeModule,
}:
let
  lib = nixpkgs.lib;

  variants = lib.cartesianProduct {
    arm = [
      "vanilla"
      "lrscheduler"
      "default-hoststore"
      "k3snix"
    ];
    runtime = [
      "containerd"
      "docker"
    ];
    role = [
      "server"
      "agent"
    ];
  };

  # What a cluster's own module supplies: disks, boot loader, network.
  standInHardware = {
    boot.loader.grub.device = "nodev";
    fileSystems."/" = {
      device = "/dev/disk/by-label/nixos";
      fsType = "ext4";
    };
    system.stateVersion = lib.trivial.release;
  };

  evalNode =
    v:
    (lib.nixosSystem {
      inherit (pkgs.stdenv.hostPlatform) system;
      modules = [
        nodeModule
        standInHardware
        {
          networking.hostName = "node";
          k3snix.node = {
            enable = true;
            inherit (v) arm runtime role;
            privateInterface = "eth1";
            tokenFile = "/run/keys/k3s-token";
            registry = "10.0.0.10:5000";
            serverAddr = lib.optionalString (v.role == "agent") "https://10.0.0.10:6443";
          };
        }
      ];
    }).config;

  check =
    v:
    let
      c = evalNode v;
      name = "${v.arm}/${v.runtime}/${v.role}";
      exec = toString c.systemd.services.k3s.serviceConfig.ExecStart;
      fw = c.networking.firewall;
      private = fw.interfaces.eth1.allowedTCPPorts;
      expect = cond: msg: if cond then true else throw "node variant ${name}: ${msg}";
      hostStore = lib.elem v.arm [
        "default-hoststore"
        "k3snix"
      ];
      ok = lib.all (x: x) [
        (expect (
          lib.hasInfix "--docker" exec == (v.runtime == "docker")
        ) "--docker flag does not match the runtime")
        (expect (lib.hasInfix "k3snix.io/arm=${v.arm}" exec) "arm label missing")
        (expect (lib.hasInfix "k3snix.io/runtime=${v.runtime}" exec) "runtime label missing")
        (expect (lib.hasInfix "--flannel-iface=eth1" exec) "flannel is not on the private interface")
        (expect (
          c.virtualisation.docker.enable == (v.runtime == "docker")
        ) "docker enabled on the wrong runtime")
        (expect (
          c.environment.etc ? "rancher/k3s/registries.yaml" == (v.runtime == "containerd")
        ) "registries.yaml does not match the runtime")
        (expect (
          lib.any (r: lib.hasInfix "gcroots/k3snix" r) c.systemd.tmpfiles.rules == hostStore
        ) "GC root directory does not match the arm")
        (expect (lib.elem 6443 private == (v.role == "server")) "API port on the wrong role")
        (expect (lib.all (p: lib.elem p private) [
          9860
          10250
          5000
        ]) "agent, kubelet or registry port not open on the private interface")
        (expect (
          !lib.any (p: lib.elem p fw.allowedTCPPorts) [
            9860
            6443
            10250
            5000
          ]
        ) "a cluster port is open publicly")
      ];
      # Forces the whole system to evaluate, including module assertions,
      # without making the check depend on the system closure.
      drv = builtins.unsafeDiscardStringContext c.system.build.toplevel.drvPath;
    in
    assert ok;
    "${name} ${drv}\n  ${builtins.unsafeDiscardStringContext exec}\n";
in
pkgs.writeText "k3snix-node-variants" (lib.concatMapStrings check variants)
