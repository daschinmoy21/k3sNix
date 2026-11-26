# The agent with -materialize=nix on a real Nix node (#52), run from the
# k3snix:nix image with the mounts of deploy/nix-node/10-agent-daemonset.yaml.
#
#   origin: holds the pinned closures and serves them, -catalog-only, like
#           the cluster origin.
#   node:   a store holding only its own system. The agent fetches the
#           share-a closure (hello) through the host nix-daemon and roots it
#           for a pod.
#
# Checks: the paths are valid in the node's Nix database with the catalog's
# narHash and references, the pod's GC root keeps them through
# nix-collect-garbage, and they are collected once the root is gone.
#
#   nix build .#tests.agent-nix -L
{
  pkgs,
  k3snix,
  imageNix,
  closures,
}:
pkgs.testers.runNixOSTest {
  name = "k3snix-agent-nix";

  nodes = {
    origin = {
      # Only this system's closure in the store, not the host's whole store.
      virtualisation.useNixStoreImage = true;
      virtualisation.writableStore = true;
      networking.firewall.allowedTCPPorts = [ 9877 ];
      # The manifests reference every path they describe, so the origin's
      # store holds all of them. The node's catalog is copied from here.
      environment.etc."k3snix-catalog".source = "${closures}/manifests";
      systemd.services.k3snix-origin = {
        wantedBy = [ "multi-user.target" ];
        path = [ pkgs.nix ];
        serviceConfig.ExecStart = "${k3snix}/bin/k3snix-agent -listen=:9877 -store=/nix/store -catalog=${closures}/manifests -catalog-only";
      };
    };

    node = {
      virtualisation.useNixStoreImage = true;
      virtualisation.writableStore = true;
      virtualisation.diskSize = 4096;
      virtualisation.memorySize = 2048;
      virtualisation.docker.enable = true;
      environment.systemPackages = [
        pkgs.curl
        pkgs.jq
      ];
      # The image tarball only: its contents load into docker, not the store.
      environment.etc."k3snix-image.tar.gz".source = imageNix;
    };
  };

  testScript = ''
    import json

    uid = "0f0e0d0c-0000-4000-8000-000000000052"
    roots = "/nix/var/nix/gcroots/k3snix"

    start_all()
    origin.wait_for_open_port(9877)
    node.wait_for_unit("docker.service")

    # The node's catalog is copied from the origin as text. Naming the
    # manifests in this script would put every path they reference into
    # the node's store (the test driver adds the script's references to
    # each VM), and the fetch would have nothing to do.
    node.succeed("mkdir -p /catalog")
    for name in origin.succeed("ls /etc/k3snix-catalog").split():
        data = origin.succeed(f"cat /etc/k3snix-catalog/{name}")
        node.succeed(f"cat > /catalog/{name} <<'EOF'\n{data}\nEOF")
    manifest = json.loads(node.succeed("cat /catalog/share-a.json"))
    root = manifest["root"]
    paths = {p["path"]: p for p in manifest["paths"]}
    node.fail(f"test -e {root}")

    node.succeed("docker load -i /etc/k3snix-image.tar.gz")
    node.succeed(f"mkdir -p {roots}")
    node.succeed(
        "docker run -d --name agent --network host"
        " -v /nix/store:/host/nix/store:ro"
        " -v /nix/var/nix/daemon-socket:/nix/var/nix/daemon-socket"
        f" -v {roots}:/host/gcroots"
        " -v /catalog:/catalog:ro"
        " k3snix:nix -listen=:9860 -store=/host/nix/store -nix-store-dir=/nix/store"
        " -catalog=/catalog -origin=http://origin:9877 -materialize=nix"
        " -gcroots-dir=/host/gcroots"
    )
    node.wait_for_open_port(9860)

    with subtest("fetch imports the closure through the host nix-daemon"):
        out = node.succeed(
            "curl -sSf -X POST http://localhost:9860/v1/fetch"
            f" -d '{{\"label\":\"share-a\",\"pod_uid\":\"{uid}\"}}'"
        )
        r = json.loads(out)
        print(json.dumps(r, indent=2))
        assert r["fetched_bytes"] > 0, r
        assert r["fetched_bytes"] == r["origin_bytes"] == r["predicted_bytes"], r
        assert r["gc_root"] == f"/host/gcroots/{uid}", r

    with subtest("every path is valid with the catalog's hash and references"):
        for p, meta in paths.items():
            node.succeed(f"nix-store --verify-path {p}")
            got = node.succeed(f"nix-store -q --hash {p}").strip()
            assert got == meta["narHash"], (p, got, meta["narHash"])
            refs = sorted(node.succeed(f"nix-store -q --references {p}").split())
            assert refs == sorted(meta.get("references", [])), (p, refs)
        print(node.succeed(f"{root}/bin/hello"))

    with subtest("the pod's root keeps the closure through garbage collection"):
        assert node.succeed(f"readlink {roots}/{uid}").strip() == root
        assert root in node.succeed("nix-store --gc --print-roots")
        node.succeed("nix-collect-garbage")
        node.succeed(f"test -e {root}")
        for p in paths:
            node.succeed(f"nix-store --verify-path {p}")

    with subtest("without the root, the closure is collected"):
        node.succeed(f"rm {roots}/{uid}")
        node.succeed("nix-collect-garbage")
        node.fail(f"test -e {root}")
  '';
}
