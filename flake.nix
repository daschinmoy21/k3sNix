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
          ;
        default = k3snix;
        nix-snapshotter = nix-snapshotter.packages.${system}.nix-snapshotter;
      };

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
      };
    };
}
