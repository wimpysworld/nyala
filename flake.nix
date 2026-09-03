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
          vendorHash = "sha256-SMFsDpOdSoKlDqxgyfKP4h0a6wNDgUUZAXu4ENdgRLI=";
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
          # Nyala drives Chromium only, so the Playwright MCP server ships
          # Chromium only. The stock package wraps PLAYWRIGHT_BROWSERS_PATH
          # with Chromium, the headless shell, Firefox, and WebKit (2.5 GB).
          # Swapping the browser set in both the driver and playwright-test
          # cuts the closure to 0.9 GB. The headless shell is not needed: the
          # wrapper defaults PLAYWRIGHT_MCP_BROWSER to the chromium channel,
          # which launches the full Chrome for Testing even in headless mode.
          playwrightMcpChromium =
            let
              browsers = pkgs.playwright-driver.browsers-chromium;
              playwright-test = pkgs.playwright-test.overrideAttrs (old: {
                installPhase =
                  builtins.replaceStrings [ "${pkgs.playwright-driver.browsers}" ] [ "${browsers}" ]
                    old.installPhase;
              });
            in
            pkgs.playwright-mcp.override {
              inherit playwright-test;
              playwright-driver = pkgs.playwright-driver // {
                inherit browsers;
              };
            };
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
                goperf
                goreleaser
                govulncheck
                ineffassign
                just
                libsixel
                pkg-config
                playwrightMcpChromium
                poppler-utils
              ])
              # Stage 3 runtime browser; Linux-only (nixpkgs chromium is
              # platforms = lib.platforms.linux).
              ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.chromium ];

            # Prepend the NixOS Mesa driver so `just run` launches Chromium with the
            # GPU driver discoverable for GPU-backed WebGL
            # (docs/ARCHITECTURE.md, Launch and Chromium discovery). Linux only.
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
