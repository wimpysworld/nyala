{
  description = "Nyala flake";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs/nixos-unstable";
  };

  outputs =
    {
      self,
      nixpkgs,
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
      goFor =
        pkgs:
        pkgs.go_1_26.overrideAttrs (_: {
          version = "1.26.11";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go1.26.11.src.tar.gz";
            hash = "sha256-1EEBsP8n7SpMda1GwXucdDWSCeLrLKfJ32y0/baOGWk=";
          };
        });
      localPackage =
        system:
        let
          pkgs = pkgsFor system;
          buildGoModule = pkgs.buildGoModule.override { go = goFor pkgs; };
          # Chromium is the Stage 3 runtime browser, Linux-only
          # (nixpkgs chromium is platforms = lib.platforms.linux). On Darwin
          # the build proceeds with no Chromium and no wrap.
          onLinux = pkgs.stdenv.hostPlatform.isLinux;
        in
        buildGoModule {
          pname = "nyala";
          version = "0.0.0";
          src = nixpkgs.lib.cleanSource ./.;
          subPackages = [ "cmd/nyala" ];
          vendorHash = "sha256-0w90+L2XAHUawniM7m07MQeDq+HN0YLKxzLWIzhxuvU=";
          # The shipped binary is static and cgo-free.
          env.CGO_ENABLED = "0";
          nativeBuildInputs = [
            pkgs.pkg-config
          ]
          ++ pkgs.lib.optionals onLinux [ pkgs.makeWrapper ];
          ldflags = [
            "-s"
            "-w"
          ];

          # Point the binary at the pinned Chromium so the engine resolves a
          # browser without a system install. --set-default lets CHROME_PATH in
          # the environment still override it. Prepend /run/opengl-driver/lib so the
          # NixOS Mesa driver is discoverable to the child Chromium for GPU-backed
          # WebGL (docs/ARCHITECTURE.md, Launch and Chromium discovery). Linux only.
          postFixup = pkgs.lib.optionalString onLinux ''
            wrapProgram $out/bin/nyala \
              --set-default CHROME_PATH "${pkgs.chromium}/bin/chromium" \
              --prefix LD_LIBRARY_PATH : "/run/opengl-driver/lib"
          '';

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
          go = goFor pkgs;
        in
        {
          default = pkgs.mkShell {
            packages =
              (with pkgs; [
                actionlint
                cosign
                gh
                just
                libsixel
                pkg-config
                poppler-utils
              ])
              # Stage 3 runtime browser; Linux-only (nixpkgs chromium is
              # platforms = lib.platforms.linux).
              ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.chromium ]
              ++ import ./nix/loader.nix {
                pkgs = pkgs // {
                  inherit go;
                };
              };

            shellHook = import ./nix/hooks.nix {
              pkgs = pkgs // {
                inherit go;
              };
            };
          };
        }
      );

      packages = forAllSystems (
        system:
        let
          nyala = localPackage system;
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

      checks = forAllSystems (
        system:
        let
          pkgs = pkgsFor system;
          sixelInterop = (localPackage system).overrideAttrs (old: {
            pname = "nyala-sixel-interop";
            doCheck = true;
            nativeCheckInputs = (old.nativeCheckInputs or [ ]) ++ [ pkgs.libsixel ];
            checkPhase = ''
              runHook preCheck
              go test -run '^TestLibsixelRoundTrip$' -tags sixelinterop -count=1 ./internal/render/sixel/
              runHook postCheck
            '';
          });
        in
        {
          nyala = self.packages.${system}.nyala;
          sixel-interop = sixelInterop;
        }
      );

      formatter = forAllSystems (system: (pkgsFor system).nixfmt);
    };
}
