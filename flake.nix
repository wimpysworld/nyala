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
      goFor =
        pkgs:
        pkgs.go_1_26.overrideAttrs (_: {
          version = "1.26.5";
          src = pkgs.fetchurl {
            url = "https://go.dev/dl/go1.26.5.src.tar.gz";
            hash = "sha256-SVvkvIcXasVnOS5bQRar2YRm0z17SdQedkzMaXay3EI=";
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
          vendorHash = "sha256-KLosw7GHUKK1Po3nwpt/3Wqxtv/Gu1i3gUTtwUXCjl0=";
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
          # WebGL (docs/PERFORMANCE.md, [S10][S11]). Linux only.
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
                go
                gocyclo
                golangci-lint
                goreleaser
                govulncheck
                ineffassign
                just
                libsixel
                pkg-config
                poppler-utils
              ])
              # Stage 3 runtime browser; Linux-only (nixpkgs chromium is
              # platforms = lib.platforms.linux).
              ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.chromium ];

            # Prepend the NixOS Mesa driver so `just run` launches Chromium with the
            # GPU driver discoverable for GPU-backed WebGL
            # (docs/PERFORMANCE.md, [S10]). Linux only.
            shellHook = pkgs.lib.optionalString pkgs.stdenv.hostPlatform.isLinux ''
              if [ -d /run/opengl-driver/lib ]; then
                export LD_LIBRARY_PATH="/run/opengl-driver/lib''${LD_LIBRARY_PATH:+:$LD_LIBRARY_PATH}"
              fi
            '';
          };
        }
      );

      packages = forAllSystems (
        system:
        let
          nyala = localPackage system;
          externalNyala = nix-packages.packages.${system}.nyala or null;
        in
        {
          inherit nyala;
          default = nyala;
        }
        // nixpkgs.lib.optionalAttrs (externalNyala != null) {
          nix-packages-nyala = externalNyala;
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
