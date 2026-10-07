{ pkgs, lib, inputs, ... }:
let
  inherit (pkgs.stdenv.hostPlatform) system;
  llmPkgs = inputs.llm-agents.packages.${system};
  withLocalEnv = command: ''
    export PORT=''${PORT:-8140}
    export FRONTEND_PORT=''${FRONTEND_PORT:-3140}
    export FRONTEND_ORIGIN=http://plan.mtech:3140
    export MULTICA_APP_URL=$FRONTEND_ORIGIN
    export MULTICA_PUBLIC_URL=http://plan.mtech:8140
    export LOCAL_UPLOAD_BASE_URL=""
    export REMOTE_API_URL=http://127.0.0.1:8140
    export NEXT_PUBLIC_API_URL=""
    export NEXT_PUBLIC_WS_URL=""
    export CORS_ALLOWED_ORIGINS="http://localhost:3140,http://127.0.0.1:3140,http://plan.mtech:3140,http://plan.mtech.zt:3140,http://100.64.0.2:3140,http://10.147.17.105:3140,http://172.25.26.105:3140,http://10.10.100.10:3140,http://[fd7a:115c:a1e0::2]:3140,http://[fd42:8117:1b80:bf1e:3699:9342:8117:1b80]:3140"
    export POSTGRES_USER=multica
    export POSTGRES_PASSWORD=multica
    export POSTGRES_DB=multica_multica_533
    export DATABASE_URL="postgresql://smaximov@localhost/multica_multica_533?host=$PGHOST&sslmode=disable"
    ${command}
  '';
in
{
  # Workstation-local override: avoid the shared keyring-backed secretspec
  # wrapper and use collision-free ports so the resident Docker stack on
  # 8100/3100/3101 is preserved.
  packages = [
    llmPkgs.jcode
    llmPkgs.agentty
    llmPkgs.codex
    llmPkgs.pi
    llmPkgs.loopx
    llmPkgs.chainlink
    llmPkgs.luvus
    llmPkgs.ax
    llmPkgs.sidecar
    llmPkgs.mcptoon
    llmPkgs.lean-ctx
    llmPkgs.ctx
    llmPkgs.trellis
    llmPkgs.jscpd
    llmPkgs.terminal-use
    llmPkgs.terminal-browser
    llmPkgs.waggle
    llmPkgs.tgrab
    llmPkgs.semble
    llmPkgs.memsearch
    llmPkgs.vix
    llmPkgs.dirac
    llmPkgs.agentsview
    llmPkgs.tura
    llmPkgs.fx
    llmPkgs.beads
    llmPkgs.beads-rust
    llmPkgs.mardi-gras
  ];

  env = {
    PORT = "8140";
    FRONTEND_PORT = "3140";
    MULTICA_SERVER_URL = "http://127.0.0.1:8140";
    MULTICA_APP_URL = "http://plan.mtech:3140";
    COREPACK_ENABLE_PROJECT_SPEC = "0";
    COREPACK_ENABLE_STRICT = "0";
    MULTICA_DEV_VERIFICATION_CODE = "000000";
    npm_config_manage_package_manager_versions = "false";

    APP_ENV = "development";
    ALLOW_SIGNUP = "true";
    DISABLE_WORKSPACE_CREATION = "false";
    MULTICA_CLOUD_URL = "";
    ALLOWED_EMAILS = "";
    ALLOWED_EMAIL_DOMAINS = "";
  };

  processes.api.exec = lib.mkForce (withLocalEnv ''
    cd server
    go run ./cmd/migrate up
    exec go run -ldflags "-X main.commit=$(git rev-parse --short HEAD)" ./cmd/server
  '');

  processes.web.exec = lib.mkForce (withLocalEnv "pnpm dev:web");

  processes.daemon = {
    exec = ''
      cd server
      go build -o bin/multica ./cmd/multica
      exec ./bin/multica daemon start --foreground --profile=local
    '';
    after = [ "devenv:processes:api@ready" ];
  };

  scripts.multica-health.exec = lib.mkForce ''
    curl --fail --silent --show-error http://127.0.0.1:8140/readyz >/dev/null
    curl --fail --silent --show-error http://127.0.0.1:3140/login >/dev/null
    echo "Multica dev API and web are healthy on 8140/3140"
  '';
}
