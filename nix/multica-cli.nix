{
  lib,
  buildGoModule,
  go_1_26,
}:

let
  desktopPackage = lib.importJSON ../apps/desktop/package.json;
  buildGo126Module = buildGoModule.override { go = go_1_26; };
in
buildGo126Module {
  pname = "multica";
  version = desktopPackage.version;

  src = lib.cleanSourceWith {
    src = ../server;
    filter =
      path: type:
      type != "directory" || baseNameOf path != "bin";
  };

  vendorHash = "sha256-b6elV4j+7R6L29q8tbCI3MAOY8X63ndzlmCMv7jo7mM=";

  subPackages = [ "cmd/multica" ];

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${desktopPackage.version}"
    "-X main.commit=nix"
    "-X main.date=unknown"
  ];

  # The server module contains database-backed and integration tests that are
  # outside the CLI package boundary. Keep the derivation check focused on the
  # installed command instead.
  doCheck = false;
  doInstallCheck = true;
  installCheckPhase = ''
    runHook preInstallCheck
    "$out/bin/multica" version --output json \
      | grep -Eq '"version"[[:space:]]*:[[:space:]]*"${desktopPackage.version}"'
    runHook postInstallCheck
  '';

  meta = {
    description = "Multica CLI — local agent runtime and management tool";
    homepage = "https://github.com/multica-ai/multica";
    license = {
      shortName = "multica-license";
      fullName = "Multica License";
      url = "https://github.com/multica-ai/multica/blob/main/LICENSE";
      free = false;
    };
    mainProgram = "multica";
    platforms = lib.platforms.linux;
  };
}
