{
  description = "wassup: one live architecture diagram of a Kubernetes-hosted system";
  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin" ];
      forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in {
      packages = forAll (pkgs: rec {
        wassup = pkgs.buildGoModule {
          pname = "wassup";
          version = "0.1.0";
          src = ./.;
          subPackages = [ "cmd/wassup" ];
          vendorHash = null; # set after the first `nix build` reports it
          ldflags = [ "-s" "-w" ];
          meta.license = pkgs.lib.licenses.mit;
        };
        default = wassup;
      });
    };
}
