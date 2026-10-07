{ pkgs, lib, inputs, ... }:

let
  androidSdk = inputs.android-nixpkgs.sdk.${pkgs.stdenv.hostPlatform.system} (sdkPkgs: with sdkPkgs; [
    platform-tools
    cmdline-tools-latest
    build-tools-35-0-0
    build-tools-36-0-0
    platforms-android-36
    ndk-28-2-13676358
    cmake-3-22-1
  ]);
  multicaCli = pkgs.callPackage ./nix/multica-cli.nix { };
  multicaDesktop = pkgs.callPackage ./nix/multica-desktop.nix {
    multica-cli = multicaCli;
  };

  # Keep machine-specific overrides out of the shared environment. A local
  # file may override ports, services, or process commands without becoming a
  # tracked project dependency.
  localConfig = lib.optional (builtins.pathExists ./devenv.local.nix) ./devenv.local.nix;

  withSecrets = reason: command: ''
    secretspec run --provider keyring --profile development --reason "${reason}" -- ${command}
  '';
in
{
  imports = localConfig;

  outputs.multica-cli = multicaCli;
  outputs.multica-desktop = multicaDesktop;

  packages = with pkgs; [
    curl
    git
    jq
    secretspec
    zellij
    jdk17
    androidSdk
  ];

  env = {
    ANDROID_HOME = "${androidSdk}/share/android-sdk";
    ANDROID_SDK_ROOT = "${androidSdk}/share/android-sdk";
    JAVA_HOME = "${pkgs.jdk17}";
    GRADLE_OPTS = "-Dorg.gradle.project.android.aapt2FromMavenOverride=${androidSdk}/share/android-sdk/build-tools/36.0.0/aapt2";
  };

  dotenv.enable = true;

  languages.go.enable = true;
  languages.javascript = {
    enable = true;
    package = pkgs.nodejs_22;
    pnpm = {
      enable = true;
      package = pkgs.pnpm_10;
    };
  };
  languages.typescript.enable = true;

  services.postgres = {
    enable = true;
    package = pkgs.postgresql_17;
    initialDatabases = [
      { name = "multica_multica_533"; }
    ];
  };

  scripts = {
    multica-install = {
      description = "Install the locked JavaScript dependencies";
      exec = "pnpm install --frozen-lockfile";
    };

    multica-migrate = {
      description = "Apply backend database migrations";
      exec = "cd server && go run ./cmd/migrate up";
    };

    multica-health = {
      description = "Check the live API and web development endpoints";
      exec = ''
        curl --fail --silent --show-error "http://127.0.0.1:''${PORT:-8080}/readyz" >/dev/null
        curl --fail --silent --show-error "http://127.0.0.1:''${FRONTEND_PORT:-3000}/login" >/dev/null
        echo "Multica API and web are healthy"
      '';
    };

    multica-secrets-check = {
      description = "Check optional development secrets without printing values";
      exec = ''
        secretspec check --provider keyring --profile development \
          --reason "Check Multica development secret configuration" --json
      '';
    };

    multica-daemon = {
      description = "Run the authenticated local Multica agent daemon";
      exec = "cd server && go run ./cmd/multica daemon restart --profile local";
    };

    multica.exec = "env -C server go run ./cmd/multica $@";
  };

  tasks = {
    "multica:install" = {
      description = "Install locked frontend dependencies";
      exec = "pnpm install --frozen-lockfile";
    };

    "multica:migrate" = {
      description = "Apply backend migrations before development";
      exec = "cd server && go run ./cmd/migrate up";
      after = [ "devenv:processes:postgres@ready" ];
    };

    "multica:check" = {
      description = "Run frontend typechecking and linting";
      exec = "pnpm typecheck && pnpm lint";
    };
  };

  processes = {
    api = {
      exec = withSecrets "Run the Multica development API" ''
        bash -lc 'cd server && go run ./cmd/migrate up && exec go run -ldflags "-X main.commit=$(git rev-parse --short HEAD)" ./cmd/server'
      '';
      after = [ "devenv:processes:postgres@ready" ];
      ready.exec = ''curl --fail --silent http://127.0.0.1:''${PORT:-8080}/readyz >/dev/null'';
      restart.on = "on_failure";
      restart.max = 3;
    };

    web = {
      exec = withSecrets "Run the Multica development web app" "pnpm dev:web";
      after = [ "devenv:processes:api@ready" ];
      ready.exec = ''curl --fail --silent http://127.0.0.1:''${FRONTEND_PORT:-3000}/login >/dev/null'';
      restart.on = "on_failure";
      restart.max = 3;
    };
  };
}
