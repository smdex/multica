//go:build unix

package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func writePiNativeSession(t *testing.T, path, cwd, id string) string {
	t.Helper()
	data := strings.Join([]string{
		`{"type":"session","version":3,"id":"` + id + `","timestamp":"2026-09-19T10:00:00Z","cwd":"` + cwd + `"}`,
		`{"type":"message","id":"root-user","parentId":null,"timestamp":"2026-09-19T10:00:01Z","message":{"role":"user","content":"root prompt"}}`,
		`{"type":"message","id":"old-answer","parentId":"root-user","timestamp":"2026-09-19T10:00:02Z","message":{"role":"assistant","content":[{"type":"text","text":"old branch"}],"model":"gpt-test"}}`,
		`{"type":"label","id":"bookmark","parentId":"root-user","targetId":"root-user","label":"start","timestamp":"2026-09-19T10:00:02Z"}`,
		`{"type":"message","id":"active-user","parentId":"bookmark","timestamp":"2026-09-19T10:00:03Z","message":{"role":"user","content":"active prompt"}}`,
		`{"type":"compaction","id":"compact","parentId":"active-user","timestamp":"2026-09-19T10:00:04Z","summary":"opaque context","firstKeptEntryId":"bookmark"}`,
		`{"type":"message","id":"active-answer","parentId":"compact","timestamp":"2026-09-19T10:00:05Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"considering"},{"type":"text","text":"active answer"}],"model":"gpt-test"}}`,
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return data
}

func TestPiNativeHistoryReadsActiveBranchAndPreparesOwnedClone(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	sourcePath := filepath.Join(sessions, "project", "source.jsonl")
	sourceBytes := writePiNativeSession(t, sourcePath, cwd, "source-session")
	clonePath := filepath.Join(root, "preparation", "owned.jsonl")
	// Mirror createBranchedSession: retain the active branch, remove labels,
	// rechain it, rebase compaction, then append resolved labels with fresh IDs.
	var clonedRows []string
	for _, line := range strings.Split(sourceBytes, "\n") {
		if strings.Contains(line, `"id":"old-answer"`) || strings.Contains(line, `"id":"bookmark"`) || line == "" {
			continue
		}
		line = strings.ReplaceAll(line, `"id":"source-session"`, `"id":"owned-session"`)
		line = strings.ReplaceAll(line, `"parentId":"bookmark"`, `"parentId":"root-user"`)
		line = strings.ReplaceAll(line, `"firstKeptEntryId":"bookmark"`, `"firstKeptEntryId":"active-user"`)
		clonedRows = append(clonedRows, line)
	}
	clonedRows = append(clonedRows, `{"type":"label","id":"new-label","parentId":"active-answer","targetId":"root-user","label":"start","timestamp":"2026-09-19T10:00:02Z"}`)
	clonedData := strings.Join(clonedRows, "\n") + "\n"
	sourceFingerprint, err := piActiveBranchFingerprint([]byte(sourceBytes))
	if err != nil {
		t.Fatal(err)
	}
	cloneFingerprint, err := piActiveBranchFingerprint([]byte(clonedData))
	if err != nil || sourceFingerprint != cloneFingerprint {
		t.Fatalf("valid label clone differs: %v", err)
	}
	for _, mutated := range []string{
		strings.Replace(clonedData, `"label":"start"`, `"label":"lost"`, 1),
		strings.Replace(clonedData, `"summary":"opaque context"`, `"summary":"lost context"`, 1),
		strings.Replace(clonedData, `"firstKeptEntryId":"active-user"`, `"firstKeptEntryId":"root-user"`, 1),
	} {
		fingerprint, err := piActiveBranchFingerprint([]byte(mutated))
		if err == nil && fingerprint == sourceFingerprint {
			t.Fatal("clone semantic corruption accepted")
		}
	}
	cloneFixture := filepath.Join(root, "clone-fixture.jsonl")
	if err := os.WriteFile(cloneFixture, []byte(strings.Join(clonedRows, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}

	requestLog := filepath.Join(root, "requests.jsonl")
	argsLog := filepath.Join(root, "args")
	fake := filepath.Join(root, "pi")
	writeTestExecutable(t, fake, []byte(`#!/bin/sh
stage=""
destination=""
printf '%s\n' "$*" > "`+argsLog+`"
while [ "$#" -gt 0 ]; do
  if [ "$1" = "--session" ]; then stage="$2"; shift 2; continue; fi
  if [ "$1" = "--session-dir" ]; then destination="$2"; shift 2; continue; fi
  shift
done
[ -n "$destination" ] || exit 28
clone="$destination/owned.jsonl"
while IFS= read -r line; do
  printf '%s\n' "$line" >> "`+requestLog+`"
  id=$(printf '%s' "$line" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
  i=0
  while [ "$i" -lt 32 ]; do
    printf '{"id":"stale-%s","type":"response","command":"stale","success":true}\n' "$i"
    i=$((i + 1))
  done
  case "$line" in
    *'"type":"clone"'*)
      cmp "$stage" "`+sourcePath+`" || exit 27
      cat "`+cloneFixture+`" > "$clone"
      printf '{"id":"%s","type":"response","command":"clone","success":true,"data":{"cancelled":false}}\n' "$id"
      ;;
    *'"type":"get_state"'*)
      printf '{"id":"%s","type":"response","command":"get_state","success":true,"data":{"sessionFile":"%s","sessionId":"owned-session","isStreaming":false}}\n' "$id" "$clone"
      ;;
    *) exit 19 ;;
  esac
done
`))

	backend, err := New("pi", Config{ExecutablePath: fake, Env: map[string]string{
		"HOME":                        root,
		"PI_CODING_AGENT_SESSION_DIR": sessions,
	}})
	if err != nil {
		t.Fatal(err)
	}
	provider, ok := backend.(NativeSessionProvider)
	if !ok {
		t.Fatal("Pi must expose native history")
	}
	importer, ok := backend.(NativeSessionImporter)
	if !ok {
		t.Fatal("Pi must expose verified native import")
	}

	page, err := provider.ListNativeSessions(context.Background(), NativeSessionListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Sessions) != 1 {
		t.Fatalf("listed sessions = %+v", page)
	}
	source := page.Sessions[0]
	if source.Handle != sourcePath || source.Revision == "" {
		t.Fatalf("source summary = %+v", source)
	}
	browsed, err := provider.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: source.Handle, Revision: source.Revision})
	if err != nil {
		t.Fatal(err)
	}
	if browsed.ResumeSessionID != "" {
		t.Fatalf("browsed source was made resumable: %+v", browsed)
	}
	if len(browsed.Messages) != 3 || browsed.Messages[1].Content != "active prompt" || browsed.Messages[2].Content != "active answer" {
		t.Fatalf("active branch = %+v", browsed.Messages)
	}

	opts := NativeSessionPrepareOptions{ImportID: uuid.NewString(), Handle: source.Handle, Revision: source.Revision, DestinationDir: filepath.Join(root, "preparation")}
	prepared, err := importer.PrepareNativeSession(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.ResumeSessionID != clonePath || prepared.ResumeSessionID == sourcePath || prepared.Snapshot.ResumeSessionID != clonePath {
		t.Fatalf("prepared ownership = %+v", prepared)
	}
	if got, err := os.ReadFile(sourcePath); err != nil || string(got) != sourceBytes {
		t.Fatalf("source changed during preparation: %v %q", err, string(got))
	}
	// Owned handles are never authorized through source browsing.
	if _, err := provider.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: clonePath}); nativeSessionErrorCode(err) != NativeSessionNotFound {
		t.Fatalf("source reader accepted owned path: %v", err)
	}
	ownedReader := backend.(NativeOwnedSessionProvider)
	ownedOpts := NativeOwnedSessionReadOptions{NativeSessionReadOptions: NativeSessionReadOptions{Handle: clonePath, Revision: prepared.Snapshot.Summary.Revision}, DestinationDir: opts.DestinationDir, NativeID: prepared.Snapshot.Summary.NativeID}
	if _, err := ownedReader.ReadOwnedNativeSession(context.Background(), ownedOpts); err != nil {
		t.Fatal(err)
	}
	wrong := ownedOpts
	wrong.DestinationDir = sessions
	if _, err := ownedReader.ReadOwnedNativeSession(context.Background(), wrong); err == nil {
		t.Fatal("owned reader accepted wrong root")
	}
	wrong = ownedOpts
	wrong.NativeID = "wrong"
	if _, err := ownedReader.ReadOwnedNativeSession(context.Background(), wrong); err == nil {
		t.Fatal("owned reader accepted wrong identity")
	}
	requests, err := os.ReadFile(requestLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(requests), `"type":"clone"`) != 1 || strings.Count(string(requests), `"type":"get_state"`) != 1 || strings.Contains(string(requests), `"type":"prompt"`) {
		t.Fatalf("unexpected Pi RPC traffic: %s", requests)
	}
	if args, err := os.ReadFile(argsLog); err != nil || !strings.Contains(string(args), "--no-extensions") {
		t.Fatalf("Pi preparation launch did not disable extensions: %v %q", err, args)
	}
	if _, err := importer.PrepareNativeSession(context.Background(), opts); err != nil {
		t.Fatalf("idempotent preparation: %v", err)
	}
	requests, _ = os.ReadFile(requestLog)
	if strings.Count(string(requests), `"type":"clone"`) != 1 {
		t.Fatalf("idempotent preparation cloned twice: %s", requests)
	}

	// A legacy orphan tmp file from an interrupted manifest write must not
	// block a fresh import. The current writer uses a unique temp and fsyncs
	// the containing directory after publication.
	orphanOpts := opts
	orphanOpts.ImportID = uuid.NewString()
	orphanManifest, _, err := nativePreparationPaths(orphanOpts, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(orphanManifest+".tmp", []byte("orphan"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := importer.PrepareNativeSession(context.Background(), orphanOpts); err != nil {
		t.Fatalf("orphan manifest temp blocked recovery: %v", err)
	}

	// A process that crashed after durably admitting clone may have created an
	// owned session without recording which one. Do not issue a second clone.
	ambiguousOpts := opts
	ambiguousOpts.ImportID = uuid.NewString()
	_, ambiguousStage, err := nativePreparationPaths(ambiguousOpts, "pi")
	if err != nil {
		t.Fatal(err)
	}
	if err := writeNativeStage(ambiguousStage, []byte(sourceBytes)); err != nil {
		t.Fatal(err)
	}
	if err := writeNativeAtomicJSON(nativePreparationIntentPath(ambiguousStage), nativePreparationIntent{
		ImportID: ambiguousOpts.ImportID, Provider: "pi", SourceHandle: ambiguousOpts.Handle, SourceRevision: ambiguousOpts.Revision, Phase: "cloning",
	}); err != nil {
		t.Fatal(err)
	}
	beforeRequests, _ := os.ReadFile(requestLog)
	_, err = importer.PrepareNativeSession(context.Background(), ambiguousOpts)
	if nativeSessionErrorCode(err) != NativeSessionResumeUnavailable {
		t.Fatalf("ambiguous preparation error = %v (%s)", err, nativeSessionErrorCode(err))
	}
	afterRequests, _ := os.ReadFile(requestLog)
	if string(afterRequests) != string(beforeRequests) {
		t.Fatalf("ambiguous recovery issued native RPC: before=%s after=%s", beforeRequests, afterRequests)
	}
}

func TestPiNativeListingDoesNotHydrateOversizedCandidatesAndRejectsSymlinks(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	large := filepath.Join(sessions, "project", "large.jsonl")
	writePiNativeSession(t, large, cwd, "large-session")
	file, err := os.OpenFile(large, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write(make([]byte, nativeSessionMaxBytes+1)); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	backend := &piBackend{cfg: Config{Env: map[string]string{"PI_CODING_AGENT_SESSION_DIR": sessions}}}
	page, err := backend.ListNativeSessions(context.Background(), NativeSessionListOptions{})
	if err != nil || len(page.Sessions) != 1 {
		t.Fatalf("metadata list hydrated oversized candidate: page=%+v err=%v", page, err)
	}
	_, err = backend.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: large, Revision: page.Sessions[0].Revision})
	if nativeSessionErrorCode(err) != NativeSessionHistoryTooLarge {
		t.Fatalf("oversized hydration error = %v (%s)", err, nativeSessionErrorCode(err))
	}

	escape := filepath.Join(root, "escape.jsonl")
	writePiNativeSession(t, escape, cwd, "escape-session")
	link := filepath.Join(sessions, "project", "link.jsonl")
	if err := os.Symlink(escape, link); err != nil {
		t.Fatal(err)
	}
	_, err = backend.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: link})
	if nativeSessionErrorCode(err) != NativeSessionNotFound {
		t.Fatalf("symlink leaf error = %v (%s)", err, nativeSessionErrorCode(err))
	}
}

func TestPiNativeListingChargesMalformedHeadersAndRejectsStaleRevision(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	sessions := filepath.Join(root, "sessions")
	if err := os.MkdirAll(filepath.Join(sessions, "project"), 0o700); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 129; index++ {
		path := filepath.Join(sessions, "project", fmt.Sprintf("broken-%03d.jsonl", index))
		if err := os.WriteFile(path, append([]byte(`{"type":"not-a-session","padding":"`), append(make([]byte, nativeSessionHeaderBytes), []byte(`"}\n`)...)...), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	backend := &piBackend{cfg: Config{Env: map[string]string{"PI_CODING_AGENT_SESSION_DIR": sessions}}}
	page, err := backend.ListNativeSessions(context.Background(), NativeSessionListOptions{})
	if err != nil || len(page.Sessions) != 0 || !page.Truncated || page.NextCursor == "" {
		t.Fatalf("malformed bounded list = %+v err=%v", page, err)
	}

	valid := filepath.Join(sessions, "project", "valid.jsonl")
	writePiNativeSession(t, valid, cwd, "revision-session")
	page, err = backend.ListNativeSessions(context.Background(), NativeSessionListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var summary NativeSessionSummary
	for _, candidate := range page.Sessions {
		if candidate.Handle == valid {
			summary = candidate
			break
		}
	}
	if summary.Revision == "" {
		t.Fatalf("valid session absent from page: %+v", page)
	}
	if err := os.WriteFile(valid, append([]byte(writePiNativeSession(t, valid, cwd, "revision-session")), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = backend.ReadNativeSession(context.Background(), NativeSessionReadOptions{Handle: valid, Revision: summary.Revision})
	if nativeSessionErrorCode(err) != NativeSessionSourceChanged {
		t.Fatalf("stale source revision error = %v (%s)", err, nativeSessionErrorCode(err))
	}
}

func TestPiNativeRootEnvironmentPrecedence(t *testing.T) {
	home := t.TempDir()
	for _, rel := range []string{".pi/agent/sessions", "custom/sessions", "explicit"} {
		if err := os.MkdirAll(filepath.Join(home, rel), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, tc := range []struct{ name, agentDir, sessionDir, want string }{
		{"default", "", "", ".pi/agent/sessions"},
		{"agent", "~/custom", "", "custom/sessions"},
		{"session", "~/custom", "~/explicit", "explicit"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &piBackend{cfg: Config{Env: map[string]string{"HOME": home, "PI_CODING_AGENT_DIR": tc.agentDir, "PI_CODING_AGENT_SESSION_DIR": tc.sessionDir}}}
			roots, err := b.piNativeRoots()
			want, resolveErr := filepath.EvalSymlinks(filepath.Join(home, tc.want))
			if err != nil || resolveErr != nil || len(roots) != 1 || roots[0] != want {
				t.Fatalf("roots=%v err=%v want=%s", roots, err, want)
			}
		})
	}
}
