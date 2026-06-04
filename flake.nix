{
  description = "Nyala flake";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
    nix-packages.url = "github:wimpysworld/nix-packages";
    nix-packages.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs =
    {
      self,
      nixpkgs,
      nix-packages,
      ...
    }:
    let
      supportedSystems = [
        "x86_64-linux"
        "aarch64-darwin"
        "aarch64-linux"
      ];
      forAllSystems = nixpkgs.lib.genAttrs supportedSystems;
      pkgsFor = system: import nixpkgs { inherit system; };
      localPackage =
        system:
        let
          pkgs = pkgsFor system;
          buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_26; };
        in
        buildGoModule {
          pname = "nyala";
          version = "0.0.0";
          src = nixpkgs.lib.cleanSource ./.;
          subPackages = [ "cmd/nyala" ];
          vendorHash = "sha256-7t9ZaHHX2ECoC+qJvOuMV9b4IiBy+iS6GcyOZO7ptNQ=";
          ldflags = [
            "-s"
            "-w"
          ];

          meta = {
            description = "A browser for the terminal.";
            homepage = "https://github.com/wimpysworld/nyala";
            license = nixpkgs.lib.licenses.blueOak100;
            mainProgram = "nyala";
          };
        };
    in
    {
      devShells = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
        in
        {
          default = pkgs.mkShell {
            packages = with pkgs; [
              actionlint
              cosign
              gh
              go_1_26
              gocyclo
              golangci-lint
              goreleaser
              govulncheck
              ineffassign
              just
            ];
          };
        }
      );

      packages = forAllSystems (
        system:
        let
          nyalaPkgs = nix-packages.packages.${system} or { };
          nyala = nyalaPkgs.nyala or (localPackage system);
        in
        {
          inherit nyala;
          default = nyala;
        }
      );

      apps = forAllSystems (
        system:
        let
          nyala = self.packages.${system}.nyala;
          app = {
            type = "app";
            program = "${nyala}/bin/nyala";
          };
        in
        {
          nyala = app;
          default = app;
        }
      );

      checks = forAllSystems (system: {
        nyala = self.packages.${system}.nyala;
      });

      formatter = forAllSystems (system: (pkgsFor system).nixfmt);
    };
}
