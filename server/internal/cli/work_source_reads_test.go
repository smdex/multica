package cli

import (
	"strings"
	"testing"
)

func TestParseWorkSourceReadsValid(t *testing.T) {
	raw := `[{"workspace_id":"01234567-89ab-cdef-0123-456789abcdef","source_handle":"primary","beads_dir":"/abs/.beads","executable":"/abs/bin/bd"}]`
	b, err := ParseWorkSourceReads(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 1 || b[0].SourceHandle != "primary" || b[0].BeadsDir != "/abs/.beads" {
		t.Fatalf("unexpected: %+v", b)
	}
}

func TestParseWorkSourceReadsEmptyClears(t *testing.T) {
	for _, raw := range []string{"", "  ", "[]"} {
		b, err := ParseWorkSourceReads(raw)
		if err != nil {
			t.Fatal(err)
		}
		if b != nil {
			t.Fatalf("expected nil for %q", raw)
		}
	}
}

func TestParseWorkSourceReadsRejectsInvalid(t *testing.T) {
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	cases := map[string]string{
		"not json":       `nope`,
		"not array":      `{"workspace_id":"` + uuid + `"}`,
		"uppercase uuid": `[{"workspace_id":"01234567-89AB-CDEF-0123-456789ABCDEF","source_handle":"h","beads_dir":"/a","executable":"/b"}]`,
		"non-uuid ws":    `[{"workspace_id":"my-ws","source_handle":"h","beads_dir":"/a","executable":"/b"}]`,
		"blank handle":   `[{"workspace_id":"` + uuid + `","source_handle":"  ","beads_dir":"/a","executable":"/b"}]`,
		"relative dir":   `[{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"rel/.beads","executable":"/b"}]`,
		"relative exe":   `[{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"/a","executable":"bd"}]`,
		"empty dir":      `[{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"","executable":"/b"}]`,
		"unknown field":  `[{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"/a","executable":"/b","extra":1}]`,
		"cross-dup pair": `[{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"/a","executable":"/b"},{"workspace_id":"` + uuid + `","source_handle":"h","beads_dir":"/c","executable":"/d"}]`,
	}
	for name, raw := range cases {
		if _, err := ParseWorkSourceReads(raw); err == nil {
			t.Errorf("%s: expected rejection", name)
		}
	}
}

func TestParseWorkSourceReadsSameWorkspaceDifferentHandleOK(t *testing.T) {
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	raw := `[{"workspace_id":"` + uuid + `","source_handle":"a","beads_dir":"/a","executable":"/b"},` +
		`{"workspace_id":"` + uuid + `","source_handle":"b","beads_dir":"/c","executable":"/d"}]`
	if _, err := ParseWorkSourceReads(raw); err != nil {
		t.Fatal(err)
	}
}

func TestParseWorkSourceReadsErrorMessageNamesField(t *testing.T) {
	_, err := ParseWorkSourceReads(`[{"workspace_id":"x","source_handle":"h","beads_dir":"/a","executable":"/b"}]`)
	if err == nil || !strings.Contains(err.Error(), "workspace_id") {
		t.Fatalf("expected workspace_id in error, got %v", err)
	}
}

func TestParseWorkSourceReadsRejectsTrailingJSON(t *testing.T) {
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	for name, raw := range map[string]string{
		"array plus object": `[] {"workspace_id":"` + uuid + `"}`,
		"two arrays":        `[] []`,
		"array plus scalar": `[] 7`,
	} {
		if _, err := ParseWorkSourceReads(raw); err == nil {
			t.Errorf("%s: expected trailing-JSON rejection", name)
		}
	}
}

func TestValidateWorkSourceReadsRejectsRelativeAndDuplicateBindings(t *testing.T) {
	uuid := "01234567-89ab-cdef-0123-456789abcdef"
	if err := ValidateWorkSourceReads([]WorkSourceReadBinding{{WorkspaceID: uuid, SourceHandle: "h", BeadsDir: "rel", Executable: "/abs/bd"}}); err == nil {
		t.Error("relative beads_dir must be rejected")
	}
	if err := ValidateWorkSourceReads([]WorkSourceReadBinding{{WorkspaceID: uuid, SourceHandle: "h", BeadsDir: "/abs", Executable: "bd"}}); err == nil {
		t.Error("relative executable must be rejected")
	}
	if err := ValidateWorkSourceReads([]WorkSourceReadBinding{
		{WorkspaceID: uuid, SourceHandle: "h", BeadsDir: "/a", Executable: "/b"},
		{WorkspaceID: uuid, SourceHandle: "h", BeadsDir: "/c", Executable: "/d"},
	}); err == nil {
		t.Error("duplicate binding must be rejected")
	}
	if err := ValidateWorkSourceReads([]WorkSourceReadBinding{
		{WorkspaceID: uuid, SourceHandle: "h", BeadsDir: "/a", Executable: "/b"},
		{WorkspaceID: uuid, SourceHandle: "h2", BeadsDir: "/c", Executable: "/d"},
	}); err != nil {
		t.Errorf("valid bindings rejected: %v", err)
	}
}

func TestParseWorkSourceReadsRejectsNull(t *testing.T) {
	if _, err := ParseWorkSourceReads("null"); err == nil {
		t.Fatal("null must be rejected; only a JSON array or empty string clears")
	}
}
