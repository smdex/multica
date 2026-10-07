package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/multica-ai/multica/server/internal/cli"
)

func TestApplyConfigSetWorkSourceReadsPersistsClearsRejects(t *testing.T) {
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	exe := filepath.Join(t.TempDir(), "bd")
	dir := filepath.Join(t.TempDir(), ".beads")
	valid := `[{"workspace_id":"` + uuid + `","source_handle":"primary","beads_dir":"` + dir + `","executable":"` + exe + `"}]`

	var cfg cli.CLIConfig
	if err := applyConfigSet(&cfg, "work_source_reads", valid); err != nil {
		t.Fatal(err)
	}
	if len(cfg.WorkSourceReads) != 1 || cfg.WorkSourceReads[0].Executable != exe {
		t.Fatalf("unexpected: %+v", cfg.WorkSourceReads)
	}

	// Empty clears.
	if err := applyConfigSet(&cfg, "work_source_reads", ""); err != nil {
		t.Fatal(err)
	}
	if cfg.WorkSourceReads != nil {
		t.Fatalf("expected cleared, got %+v", cfg.WorkSourceReads)
	}

	// Malformed rejected, and nothing persisted on the struct.
	if err := applyConfigSet(&cfg, "work_source_reads", `{not json`); err == nil {
		t.Fatal("expected malformed JSON rejection")
	}
	if cfg.WorkSourceReads != nil {
		t.Fatalf("malformed input must not persist: %+v", cfg.WorkSourceReads)
	}

	// Duplicates rejected.
	dup := `[{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"/a","executable":"/b"},` +
		`{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"/c","executable":"/d"}]`
	if err := applyConfigSet(&cfg, "work_source_reads", dup); err == nil {
		t.Fatal("expected duplicate rejection")
	}
}

func TestRunConfigSetWorkSourceReadsProfileIsolation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	exe := filepath.Join(t.TempDir(), "bd")
	dir := filepath.Join(t.TempDir(), ".beads")
	valid := `[{"workspace_id":"` + uuid + `","source_handle":"primary","beads_dir":"` + dir + `","executable":"` + exe + `"}]`

	cmd := newConfigTestCmd()
	_ = cmd.Flags().Set("profile", "dev")
	stderr := captureStderr(t)
	defer stderr.restore()
	if err := runConfigSet(cmd, []string{"work_source_reads", valid}); err != nil {
		t.Fatal(err)
	}

	// The dev profile holds the binding; the default profile stays empty.
	devCfg, err := cli.LoadCLIConfigForProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	if len(devCfg.WorkSourceReads) != 1 {
		t.Fatalf("dev profile missing binding: %+v", devCfg.WorkSourceReads)
	}
	defCfg, err := cli.LoadCLIConfigForProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if defCfg.WorkSourceReads != nil {
		t.Fatalf("default profile must be untouched: %+v", defCfg.WorkSourceReads)
	}

	// Round-trip: persisted JSON decodes back through the strict parser.
	devPath, err := cli.CLIConfigPathForProfile("dev")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(devPath)
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		WorkSourceReads json.RawMessage `json:"work_source_reads"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.ParseWorkSourceReads(string(probe.WorkSourceReads)); err != nil {
		t.Fatalf("persisted value must re-parse: %v", err)
	}
}

func TestRunConfigSetWorkSourceReadsMalformedNotPersisted(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cmd := newConfigTestCmd()
	stderr := captureStderr(t)
	defer stderr.restore()
	if err := runConfigSet(cmd, []string{"work_source_reads", `[{"workspace_id":"nope"}]`}); err == nil {
		t.Fatal("expected rejection before persistence")
	}
	defCfg, err := cli.LoadCLIConfigForProfile("")
	if err != nil {
		t.Fatal(err)
	}
	if defCfg.WorkSourceReads != nil {
		t.Fatalf("nothing must be persisted on failure: %+v", defCfg.WorkSourceReads)
	}
}
