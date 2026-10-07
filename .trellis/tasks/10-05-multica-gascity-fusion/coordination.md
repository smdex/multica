# Fusion swarm operating contract

Active task: `.trellis/tasks/10-05-multica-gascity-fusion`.

## User outcome

Deliver Multica's polished human UI backed by real GasCity orchestration and Beads/Dolt tasks, durable events/signals and agent communication, bidirectional GitLab issues, and first-class Jujutsu. Challenge Hermes Kanban inclusion rather than blindly layering competing schedulers. All managed repositories are initialized with `jj git init --colocate`; isolated work uses jj workspaces, never Git worktrees. A prototype, simulated scheduler, unconnected UI, or test-only adapter is not a complete delivery.

## Safety and ownership

- Existing user work at start: `.gitattributes`, `AGENTS.md`, `CLAUDE.md`, `devenv.lock`, `nix/multica-desktop.nix`, the existing `.agents/artifacts/deep-research-report*.md`, skills, `.codegraph`, `.codex`, `.factory`, and `.trellis` scaffolding. Preserve it. The jj working change at start was `ourstlyk`, named `fix(nix): refresh desktop dependency hash after upstream sync`, atop `lrukpqxv` / `6e66c80c4`. The coordinator owns commits and shared-file integration. Workers do not commit, switch revisions, or move bookmarks in the shared workspace.
- Main checkout already has `.jj` and `.git`. Reference source checkouts: `/home/smaximov/src/gascity` at `2f33858ba` (clean initially), `/home/smaximov/src/paperclip` at `c46e41e81` (dirty, includes private runtime files). Inspect reference source read-only, never secrets or `.dev` state. No upstream pulls/rebases over user work.
- Research workers may write ONLY their assigned research file under this task's `research/`. Implementation starts after the architecture and Trellis activation gate. Use explicit file ownership and expand overlapping work into independent nodes.
- No destructive data operations, external issue creation, remote pushes, purchases, or real installed-agent runs without scoped authorization. Default tests use fake executables. Local acceptance uses disposable fixtures and managed services.

## Current delivery authorization

On October 7, 2026, the user requested continued code/CLI/unit/integration verification, assigned all browser E2E and visual checking to themselves, and authorized publishing verified milestones or complete features with jj to their fork's `main`. The fork is `origin`, `git@github.com:smdex/multica.git`; `upstream` is not authorized for pushes. Dry-run and confirm fast-forward ancestry before each push. Publish only accepted code and its setup/verification instructions, not unfinished command checkpoints, unrelated local edits, private agent transcripts, or unqualified source writes. Preserve the original local history and working copy. `docs/plans/native-swarm-milestones.md` is the user-facing setup/checklist. Browser ownership does not waive implementation, nonvisual verification or honest remaining-work reporting.

## Required workflow and tools

1. Read `AGENTS.md`, `.trellis/workflow.md`, relevant `.trellis/spec` indexes and guidelines, then the two existing deep research reports. Keep planning and findings in the task, not chat.
2. Use the source graph (`codegraph --help`, status/query as appropriate) and structural source queries (`ast-grep`, not the unrelated Unix `sg`). Verify important claims with exact source paths, symbols, and tests. An index is a navigation aid, not proof.
3. Use waggle artifact handoffs. `waggle mint --target PATH --snapshot` returns the handoff line. Source handoffs require `--require symbol:NAME`; consumers resolve and read that symbol, check coverage, and judge accepted/rejected. Hand over tokens and concise findings, not pasted artifact contents.
4. Headroom MCP is configured and connected in the coordinator; use bounded compression with retrievable originals for large outputs. Context-mode availability is being investigated. Do not claim unavailable tools were used. Search/select missing integrations before adding external tools. Prefer installed/configured tools, do not reconfigure user-level services casually.
5. This checkout uses `devenv.nix` services. Run verification through `devenv shell -- ...`. Use `devenv processes list` / `devenv up -d`. Do not mix with `make up`, manual PostgreSQL instances, or unrelated checkout ports/databases.
6. Load `/jujutsu-vcs` before repository mutation. Coordinator exclusively owns snapshot and write operations in the shared checkout. Worker metadata reads use `jj --ignore-working-copy --no-pager --color=never` or `GIT_OPTIONAL_LOCKS=0 git ...`: even plain `jj status`, `log`, and `diff` snapshot/export and can race the Git index. Coordinator makes atomic conventional commits, never includes unrelated user changes. Workers request a coordinator commit when their owned paths are verified. Ordinary jj operations remain allowed in assigned isolated workspaces and disposable probe repositories.
7. Use todo progress within each task. Source code edits use edit/apply_patch/write tools, not Python/sed/perl scripts.

## DAG discipline and model routing

- Research, architectural design, and task planning use `gpt-6-astra`, high reasoning.
- Implementation uses `gpt-6.1-sol` or `gpt-6-luna`, low reasoning as instructed by the host. Verification and plan adjustment use `gpt-6.1-sol` with normal or high reasoning.
- Every node must end in `swarm complete_node` with findings, evidence, validation, open_questions, confidence, and what_i_did_not_check, or be decomposed using `swarm expand_node`. Do not merely end the turn or DM a report.
- Keep ready work wide. Composite nodes and root have independent adversarial gates. A gate accounts for every audited node id and injects gaps for missing acceptance paths. Never label mocked or substitute checks as real E2E.
- Downstream nodes consume durable artifacts. Record hashes/revisions of external source inspected. Treat research claims as hypotheses until verified.

## Acceptance baseline

1. Existing Multica issue creation/read/edit and web/desktop navigation remain functional and API-compatible.
2. Real GasCity is integrated at a maintainable boundary with one authoritative task/workflow state, durable dependency gates, attempts/leases/handoffs, recovery, cancellation, and error reporting.
3. Beads/Dolt persistence, event cursors/replay, and agent mail are operational and tenant-scoped with restart/retry behavior checked.
4. GitLab inbound/outbound issue synchronization is authenticated, idempotent, loop-resistant, conflict-aware, and recoverable after outages. External service credentials must never be invented or exposed.
5. Repository initialization, per-task isolation, resumption, cleanup, diff/history, bookmark/MR flow, and agent instructions default to jj. No internal Git worktree fallback.
6. UI exposes configuration, work progression, failures/recovery, and required communication. Shared UI routes work in web and desktop.
7. Installation/configuration, readiness and diagnostics, schema parsing, malformed response tests, backend tests, frontend checks, real runtime integration, and browser E2E are covered. Mark any unavailable credentials or external acceptance dependencies explicitly.
