{
  description = "K3sNix: place K3s pods using Nix store-path inventory";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nix-snapshotter.url = "github:pdtpartners/nix-snapshotter";
  };

  outputs =
    {
      self,
      nixpkgs,
      nix-snapshotter,
    }:
    let
      system = "x86_64-linux";
      pkgs = import nixpkgs { inherit system; };
      lib = pkgs.lib;

      src = lib.fileset.toSource {
        root = ./.;
        fileset = lib.fileset.unions [
          ./go.mod
          ./cmd
          ./internal
          ./testdata
        ];
      };

      k3snix = pkgs.buildGoModule {
        pname = "k3snix";
        version = "0.1.0";
        inherit src;
        vendorHash = null;
        env.CGO_ENABLED = "0";
        subPackages = [
          "cmd/agent"
          "cmd/extender"
          "cmd/bench"
        ];
        ldflags = [
          "-s"
          "-w"
        ];
        postInstall = ''
          mv $out/bin/agent $out/bin/k3snix-agent
          mv $out/bin/extender $out/bin/k3snix-extender
          mv $out/bin/bench $out/bin/k3snix-bench
        '';
        meta.mainProgram = "k3snix-bench";
      };

      k3snixWrapped = pkgs.symlinkJoin {
        name = "k3snix-wrapped";
        paths = [ k3snix ];
        nativeBuildInputs = [ pkgs.makeWrapper ];
        postBuild = ''
          wrapProgram $out/bin/k3snix-agent --prefix PATH : ${lib.makeBinPath [ pkgs.nix ]}
          wrapProgram $out/bin/k3snix-extender --prefix PATH : ${lib.makeBinPath [ pkgs.nix ]}
          wrapProgram $out/bin/k3snix-bench --prefix PATH : ${lib.makeBinPath [ pkgs.nix ]}
        '';
      };

      image = pkgs.dockerTools.buildLayeredImage {
        name = "k3snix";
        tag = "dev";
        contents = [ k3snix ];
        config = {
          Entrypoint = [ "/bin/k3snix-agent" ];
        };
      };

      closures = pkgs.callPackage ./nix/closures.nix {
        nixpkgsRev = nixpkgs.rev or "";
      };

      # OCI images of the same workloads, and the store-path versus layer
      # comparison computed from both (docs/comparison.md).
      ociImages = import ./nix/images.nix { inherit pkgs; };

      comparison = import ./nix/comparison.nix {
        inherit pkgs k3snix closures;
        images = ociImages;
      };

      goTests = pkgs.runCommand "k3snix-go-test" {
        nativeBuildInputs = [ pkgs.go ];
        inherit src;
      } ''
        export GOCACHE=$NIX_BUILD_TOP/go-cache
        export GOPATH=$NIX_BUILD_TOP/gopath
        export GO111MODULE=on
        export CGO_ENABLED=0
        mkdir src
        cp -r $src/. src/
        chmod -R u+w src
        cd src
        go test ./...
        touch $out
      '';
    in
    {
      packages.${system} = {
        inherit
          k3snix
          k3snixWrapped
          image
          closures
          comparison
          ;
        # Upstream static binary: mount this file alone into Alpine dind nodes.
        # Keep in step with pkgs.k3s; the live script verifies the versions.
        k3s-static = pkgs.fetchurl {
          url = "https://github.com/k3s-io/k3s/releases/download/v1.36.4%2Bk3s1/k3s";
          # Recursive hash because executable=true; upstream file SHA256:
          # 835873f37245fc615f547a2fe2af9402a347875f13fa64a1f136de644955ea3f
          hash = "sha256-FqkykQd8H08lobR7tc63XhJHBofnt0jUQhtwUCAMzcg=";
          executable = true;
        };
        default = k3snix;
        nix-snapshotter = nix-snapshotter.packages.${system}.nix-snapshotter;
      };

      # The OCI images by granularity and label, e.g. .#ociImages.fine.share-a
      legacyPackages.${system}.ociImages = ociImages;

      apps.${system} = {
        agent = {
          type = "app";
          program = "${k3snixWrapped}/bin/k3snix-agent";
        };
        extender = {
          type = "app";
          program = "${k3snixWrapped}/bin/k3snix-extender";
        };
        bench = {
          type = "app";
          program = "${k3snix}/bin/k3snix-bench";
        };
      };

      devShells.${system}.default = pkgs.mkShell {
        packages = with pkgs; [
          go
          gopls
          gotools
          k3d
          k3s
          kubectl
          minikube
          docker
          jq
          curl
          python3
          nix
          gnused
          skopeo
        ];
        shellHook = ''
          echo "k3snix dev shell (go $(go version | cut -d' ' -f3))"
        '';
      };

      nixosModules.k3s-with-snapshotter =
        { lib, ... }:
        {
          imports = [ nix-snapshotter.nixosModules.default ];
          nixpkgs.overlays = [ nix-snapshotter.overlays.default ];
          services.nix-snapshotter.enable = lib.mkDefault true;
          virtualisation.containerd = {
            enable = lib.mkDefault true;
            nixSnapshotterIntegration = lib.mkDefault true;
          };
        };

      overlays.default = final: prev: {
        k3snix = k3snix;
      };

      checks.${system} = {
        inherit goTests;
        k3snix = k3snix;
        closures = closures;
        comparison = comparison;
      };
    };
}
