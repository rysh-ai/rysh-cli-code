{
  # Rysh CLI nix flake (design 011, D3).
  #
  #   nix run   github:rysh-ai/rysh-cli
  #   nix profile install github:rysh-ai/rysh-cli
  #
  # NOTE: rysh-cli is a Go module that `replace`s ../rysh-shared, so this flake
  # expects the monorepo layout (rysh-cli/ next to rysh-shared/) as its source
  # root. `vendorHash` must be filled on first build: run once with
  # `lib.fakeHash`, nix prints the correct hash, paste it back. This is the
  # standard buildGoModule bootstrap and is flagged here honestly rather than
  # shipped as a claimed-verified value.
  description = "Rysh — agentic terminal multiplexer for code development";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = "0.0.0-dev";
      in {
        packages.default = pkgs.buildGoModule {
          pname = "rysh";
          inherit version;
          # Monorepo root (rysh-cli beside rysh-shared).
          src = ../.;
          modRoot = "rysh-cli";
          # GOFLAGS mirrors the GOWORK=off build the CLI uses elsewhere.
          env.GOWORK = "off";
          # TODO(release): replace with the real hash printed on first build.
          vendorHash = pkgs.lib.fakeHash;
          ldflags = [ "-s" "-w" "-X" "main.version=${version}" ];
          doCheck = false;
          meta = with pkgs.lib; {
            description = "Agentic terminal multiplexer for code development";
            homepage = "https://rysh.ai";
            mainProgram = "rysh";
          };
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/rysh";
        };
      });
}
