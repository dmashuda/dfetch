{
  description = "dfetch — run SQL queries against remote data sources via a local SQLite engine";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
    go-overlay = {
      url = "github:purpleclay/go-overlay";
      inputs = {
        nixpkgs.follows = "nixpkgs";
        flake-utils.follows = "flake-utils";
      };
    };
    gomod2nix = {
      url = "github:nix-community/gomod2nix";
      inputs = {
        nixpkgs.follows = "nixpkgs";
        flake-utils.follows = "flake-utils";
      };
    };
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
      go-overlay,
      gomod2nix,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = import nixpkgs {
          inherit system;
          overlays = [
            go-overlay.overlays.default
            gomod2nix.overlays.default
          ];
        };

        # go-overlay tracks every Go release within hours, so fromGoMod resolves
        # the exact toolchain pinned in go.mod (1.26.4) even before nixpkgs ships
        # it — no go.mod patching or GOTOOLCHAIN network fetch needed.
        go = pkgs.go-bin.fromGoMod ./go.mod;

        version = self.shortRev or self.dirtyShortRev or "dev";
      in
      {
        packages.default = pkgs.buildGoApplication {
          pname = "dfetch";
          inherit version go;
          src = ./.;
          # Per-module hashes live in the committed gomod2nix.toml (regenerate
          # with `make gomod2nix` after any go.mod/go.sum change), so dependency
          # bumps no longer require recomputing a single repo-wide vendorHash.
          modules = ./gomod2nix.toml;
          # mattn/go-sqlite3 bundles its own SQLite C source, so cgo needs only a
          # C compiler (provided by stdenv); no system sqlite dependency.
          env.CGO_ENABLED = "1";
          # Build just the CLI, not the internal packages / tools.
          subPackages = [ "." ];
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
          meta = {
            description = "Query and join data across any data source with SQL, on demand";
            homepage = "https://github.com/dmashuda/dfetch";
            license = pkgs.lib.licenses.mit;
            mainProgram = "dfetch";
          };
        };

        devShells.default = pkgs.mkShell {
          CGO_ENABLED = "1";
          packages = [
            go # toolchain pinned to go.mod (cgo build)
            pkgs.gcc # C compiler for mattn/go-sqlite3
            pkgs.gopls
            pkgs.golangci-lint # make lint
            pkgs.gnumake
            pkgs.goreleaser # release builds
            pkgs.jdk # make generate (ANTLR parser regen)
            pkgs.sqlite # poke localdb / ad-hoc SQL
            pkgs.gomod2nix # make gomod2nix (regen lockfile after dep bumps)
          ];
        };
      }
    );
}
