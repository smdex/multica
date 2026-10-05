{
  lib,
  stdenv,
  fetchPnpmDeps,
  pnpmConfigHook,
  pnpm_10,
  nodejs_22,
  electron_43,
  copyDesktopItems,
  makeDesktopItem,
  makeWrapper,
  multica-cli,
}:

let
  desktopPackage = lib.importJSON ../apps/desktop/package.json;
  root = ../.;
  rootString = toString root;
  includedRoots = [
    "package.json"
    "pnpm-lock.yaml"
    "pnpm-workspace.yaml"
    "LICENSE"
    "NOTICE"
    "apps/desktop"
    "packages"
  ];
  excludedRoots = [
    "apps/desktop/node_modules"
    "apps/desktop/dist"
    "apps/desktop/out"
    "apps/desktop/resources/bin"
  ];
  source = builtins.path {
    path = root;
    name = "multica-desktop-source";
    filter =
      path: _type:
      let
        pathString = toString path;
        relative =
          if pathString == rootString then
            ""
          else
            lib.removePrefix "${rootString}/" pathString;
        matches =
          prefix:
          relative == prefix
          || lib.hasPrefix "${prefix}/" relative
          || lib.hasPrefix "${relative}/" prefix;
        excluded =
          prefix:
          relative == prefix || lib.hasPrefix "${prefix}/" relative;
        isNodeModules =
          relative == "node_modules"
          || lib.hasSuffix "/node_modules" relative
          || lib.hasInfix "/node_modules/" relative;
      in
      relative == ""
      || (lib.any matches includedRoots && !isNodeModules && !lib.any excluded excludedRoots);
  };
  electron = electron_43;
  pnpm = pnpm_10;
in
stdenv.mkDerivation (finalAttrs: {
  pname = "multica-desktop";
  version = desktopPackage.version;

  src = source;

  pnpmWorkspaces = [ "@multica/desktop..." ];
  pnpmDeps = fetchPnpmDeps {
    inherit pnpm;
    inherit (finalAttrs)
      pname
      version
      src
      pnpmWorkspaces
      ;
    fetcherVersion = 3;
    hash = "sha256-y2QFYyD+miXiIIfLuDppL5ngA5loW/RcNJjGeBNYQF0=";
  };

  nativeBuildInputs = [
    nodejs_22
    pnpm
    pnpmConfigHook
    copyDesktopItems
    makeWrapper
  ];

  env.ELECTRON_SKIP_BINARY_DOWNLOAD = "1";

  # Nix owns upgrades for this package. Keep electron-updater available in the
  # application bundle, but do not arm its background checks in Nix builds.
  postPatch = ''
    substituteInPlace apps/desktop/src/main/index.ts \
      --replace-fail \
        '    setupAutoUpdater(() => mainWindow);' \
        '    if (process.env["MULTICA_DISABLE_AUTO_UPDATE"] !== "1") setupAutoUpdater(() => mainWindow);'
  '';

  preBuild = ''
    mkdir -p apps/desktop/resources/bin
    cp ${lib.getExe multica-cli} apps/desktop/resources/bin/multica
    chmod +x apps/desktop/resources/bin/multica
  '';

  buildPhase = ''
    runHook preBuild

    pnpm --filter @multica/desktop exec electron-vite build

    mkdir -p deploy/out deploy/resources/bin deploy/node_modules
    cp apps/desktop/package.json deploy/package.json
    cp -r apps/desktop/out/. deploy/out/
    install -Dm755 ${lib.getExe multica-cli} deploy/resources/bin/multica
    install -Dm644 apps/desktop/resources/icon.png deploy/resources/icon.png
    install -Dm644 LICENSE deploy/resources/LICENSE
    install -Dm644 NOTICE deploy/resources/NOTICE

    node ${./copy-node-runtime.mjs} \
      "$PWD/apps/desktop/package.json" \
      "$PWD/deploy/node_modules" \
      @electron-toolkit/utils \
      fix-path \
      electron-updater

    runHook postBuild
  '';

  installPhase = ''
    runHook preInstall

    mkdir -p $out/lib/multica-desktop
    cp -r deploy $out/lib/multica-desktop/app

    install -Dm644 LICENSE $out/share/licenses/multica-desktop/LICENSE
    install -Dm644 NOTICE $out/share/doc/multica-desktop/NOTICE

    for icon in apps/desktop/build/icons/*.png; do
      size="$(basename "$icon" .png)"
      install -Dm644 "$icon" "$out/share/icons/hicolor/$size/apps/multica-desktop.png"
    done

    makeWrapper ${lib.getExe electron} $out/bin/multica-desktop \
      --set-default ELECTRON_FORCE_IS_PACKAGED 1 \
      --set MULTICA_DISABLE_AUTO_UPDATE 1 \
      --add-flags $out/lib/multica-desktop/app \
      --add-flags "\''${NIXOS_OZONE_WL:+\''${WAYLAND_DISPLAY:+--ozone-platform-hint=auto --enable-features=WaylandWindowDecorations --enable-wayland-ime=true}}" \
      --inherit-argv0

    runHook postInstall
  '';

  desktopItems = [
    (makeDesktopItem {
      name = "multica-desktop";
      desktopName = "Multica";
      comment = desktopPackage.description;
      exec = "multica-desktop %U";
      icon = "multica-desktop";
      startupWMClass = "Multica";
      terminal = false;
      categories = [ "Office" ];
      mimeTypes = [ "x-scheme-handler/multica" ];
    })
  ];

  meta = {
    description = desktopPackage.description;
    homepage = desktopPackage.homepage;
    license = {
      shortName = "multica-license";
      fullName = "Multica License";
      url = "https://github.com/multica-ai/multica/blob/main/LICENSE";
      free = false;
    };
    mainProgram = "multica-desktop";
    platforms = lib.platforms.linux;
  };
})
