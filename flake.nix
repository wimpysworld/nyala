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
      # Tesseract pinned to the eng and osd traineddata. It backs the OCR tests
      # built under the "ocr" tag; the shipped nyala binary stays cgo-free.
      tesseractFor =
        system:
        (pkgsFor system).tesseract.override {
          enableLanguages = [
            "eng"
            "osd"
          ];
        };
      localPackage =
        system:
        let
          pkgs = pkgsFor system;
          buildGoModule = pkgs.buildGoModule.override { go = pkgs.go_1_26; };
          tesseract = tesseractFor system;
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
          vendorHash = "sha256-rtuzpZo3+oKDY+Bh0rBvYyFYky3hTrF2qENuXgo/DNM=";
          # The shipped binary is static and cgo-free: the gosseract cgo lives
          # behind the "ocr" tag, off here, and subPackages excludes it. The
          # tesseract inputs only serve the OCR-tagged test build.
          env.CGO_ENABLED = "0";
          nativeBuildInputs = [
            pkgs.pkg-config
          ]
          ++ pkgs.lib.optionals onLinux [ pkgs.makeWrapper ];
          buildInputs = [
            tesseract
            pkgs.leptonica
          ];
          ldflags = [
            "-s"
            "-w"
          ];

          # Point the binary at the pinned Chromium so the engine resolves a
          # browser without a system install. --set-default lets CHROME_PATH in
          # the environment still override it. Linux only.
          postFixup = pkgs.lib.optionalString onLinux ''
            wrapProgram $out/bin/nyala \
              --set-default CHROME_PATH "${pkgs.chromium}/bin/chromium"
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
          tesseract = tesseractFor system;
        in
        {
          default = pkgs.mkShell {
            packages =
              (with pkgs; [
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
                leptonica
                pkg-config
                poppler-utils
                tesseract
              ])
              # Stage 3 runtime browser; Linux-only (nixpkgs chromium is
              # platforms = lib.platforms.linux).
              ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [ pkgs.chromium ];

            # gosseract links libtesseract directly, so the OCR tests need
            # TESSDATA_PREFIX to find the eng and osd traineddata. The tagged
            # tests run with `go test -tags ocr ./...`.
            shellHook = ''
              export TESSDATA_PREFIX="${tesseract}/share/tessdata"
            '';
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
