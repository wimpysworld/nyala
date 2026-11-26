{
  pkgs,
  self,
  go,
  ...
}:
let
  buildGoModule = pkgs.buildGoModule.override { inherit go; };
  # The release workflow writes nix/version into the commit that the latest tag names.
  version =
    if builtins.pathExists ./version then
      pkgs.lib.trim (builtins.readFile ./version)
    else
      "0.0.0-${self.sourceInfo.shortRev or (self.sourceInfo.dirtyShortRev or "dirty")}";
  # Chromium is the Stage 3 runtime browser, Linux-only
  # (nixpkgs chromium is platforms = lib.platforms.linux). On Darwin
  # the build proceeds with no Chromium and no wrap.
  onLinux = pkgs.stdenv.hostPlatform.isLinux;
in
buildGoModule {
  pname = "nyala";
  inherit version;
  src = pkgs.lib.cleanSource ../.;
  subPackages = [ "cmd/nyala" ];
  vendorHash = "sha256-lJJ2WFgTWD8qX+6Bzh2hO1WbISXh8aESh7B0xBX7A/E=";
  # The shipped binary is static and cgo-free.
  env.CGO_ENABLED = "0";
  nativeBuildInputs = [
    pkgs.pkg-config
  ]
  ++ pkgs.lib.optionals onLinux [ pkgs.makeWrapper ];
  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
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
    license = pkgs.lib.licenses.blueOak100;
    mainProgram = "nyala";
  };
}
