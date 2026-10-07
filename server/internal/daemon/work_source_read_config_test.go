package daemon

import (
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestLoadConfigCopiesWorkSourceReads(t *testing.T) {
	stageFakeAgent(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))

	raw := `[{"workspace_id":"01234567-89ab-cdef-0123-456789abcdef","source_handle":"primary","beads_dir":"/abs/.beads","executable":"/abs/bin/bd"}]`
	bindings, err := cli.ParseWorkSourceReads(raw)
	if err != nil {
		t.Fatal(err)
	}
	cfg := cli.CLIConfig{WorkSourceReads: bindings}
	if err := cli.SaveCLIConfigForProfile(cfg, ""); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig(Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.WorkSourceReads) != 1 || got.WorkSourceReads[0] != bindings[0] {
		t.Fatalf("unexpected WorkSourceReads: %+v", got.WorkSourceReads)
	}
	// No aliasing: mutating daemon state must not touch the CLI copy.
	got.WorkSourceReads[0].SourceHandle = "mutated"
	if bindings[0].SourceHandle != "primary" {
		t.Fatal("daemon WorkSourceReads aliases the CLI config slice")
	}
}

func TestLoadConfigEmptyWorkSourceReadsIsNil(t *testing.T) {
	stageFakeAgent(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("SHELL", filepath.Join(t.TempDir(), "missing-shell"))
	got, err := LoadConfig(Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got.WorkSourceReads != nil {
		t.Fatalf("expected nil, got %+v", got.WorkSourceReads)
	}
}
