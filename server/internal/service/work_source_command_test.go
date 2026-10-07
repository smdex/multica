package service

import (
	"fmt"
	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"strings"
	"testing"
)

func TestValidateWorkSourceCommandReport(t *testing.T) {
	for _, tc := range []struct {
		name, command, result string
		valid                 bool
	}{
		{"empty list", "list", "[]", true},
		{"list", "list", `[{"id":"a"}]`, true},
		{"null", "list", "null", false},
		{"scalar", "list", "42", false},
		{"wrong object", "list", "{}", false},
		{"null row", "list", "[null]", false},
		{"empty id", "list", `[{"id":" "}]`, false},
		{"duplicate", "list", `[{"id":"a"},{"id":"a"}]`, false},
		{"wrong id type", "list", `[{"id":42}]`, false},
		{"trailing", "list", "[] []", false},
		{"detail", "read", `{"id":"a","revision":"r1"}`, true},
		{"wrong detail id", "read", `{"id":"b","revision":"r1"}`, false},
		{"missing revision", "read", `{"id":"a"}`, false},
		{"detail null", "read", "null", false},
		{"detail array", "read", `[{"id":"a","revision":"r1"}]`, false},
		{"detail trailing", "read", `{"id":"a","revision":"r1"} {}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := db.WorkSourceCommand{Command: tc.command, NativeID: pgtype.Text{String: "a", Valid: true}}
			p := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: tc.result, Valid: true}}
			err := validateWorkSourceCommandReport(cmd, &p)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
	cmd := db.WorkSourceCommand{Command: "list", LimitCount: pgtype.Int4{Int32: 1, Valid: true}}
	p := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: `[{"id":"a"},{"id":"b"}]`, Valid: true}}
	if validateWorkSourceCommandReport(cmd, &p) == nil {
		t.Fatal("accepted over requested limit")
	}
	rows := make([]string, 51)
	for i := range rows {
		rows[i] = `{"id":"` + strings.Repeat("a", i+1) + `"}`
	}
	cmd.LimitCount = pgtype.Int4{}
	p.Result.String = "[" + strings.Join(rows, ",") + "]"
	if validateWorkSourceCommandReport(cmd, &p) == nil {
		t.Fatal("accepted over default limit")
	}
	p.Result.String = strings.Repeat(" ", 2<<20) + "[]"
	if validateWorkSourceCommandReport(cmd, &p) == nil {
		t.Fatal("accepted oversized result")
	}
	p = ReportWorkSourceCommandParams{Status: "failed", Error: pgtype.Text{String: strings.Repeat("x", (8<<10)+1), Valid: true}}
	if validateWorkSourceCommandReport(cmd, &p) == nil {
		t.Fatal("accepted oversized diagnostic")
	}
	p.Error.String = strings.Repeat("x", 8<<10)
	if validateWorkSourceCommandReport(cmd, &p) != nil || len(p.Error.String) != 8<<10 {
		t.Fatal("changed valid diagnostic")
	}
}

func TestWorkSourceCommandCanonicalResult(t *testing.T) {
	cmd := db.WorkSourceCommand{Command: "read", NativeID: pgtype.Text{String: "a", Valid: true}}
	a := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: `{"id":"a","revision":"r1","title":"one"}`, Valid: true}}
	b := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: `{ "title":"one", "revision":"r1", "id":"a" }`, Valid: true}}
	if validateWorkSourceCommandReport(cmd, &a) != nil || validateWorkSourceCommandReport(cmd, &b) != nil || a.Result != b.Result {
		t.Fatal("logical replay not canonical")
	}
}

func TestWorkSourceCommandLegacyCanonicalBytes(t *testing.T) {
	const old = `{"id":"a","title":"one","description":null,"status":"","priority":0,"issue_type":"","owner":null,"created_at":"","created_by":null,"updated_at":"","dependency_count":0,"dependent_count":0,"comment_count":0,"revision":"r1"}`
	cmd := db.WorkSourceCommand{Command: "read", NativeID: pgtype.Text{String: "a", Valid: true}}
	for _, input := range []string{`{"id":"a","revision":"r1","title":"one"}`, old} {
		p := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: input, Valid: true}}
		if err := validateWorkSourceCommandReport(cmd, &p); err != nil {
			t.Fatal(err)
		}
		if p.Result.String != old {
			t.Fatalf("legacy terminal bytes changed: %s", p.Result.String)
		}
	}
}

func TestWorkSourceCommandDependencyReport(t *testing.T) {
	cmd := db.WorkSourceCommand{Command: "read", NativeID: pgtype.Text{String: "a", Valid: true}}
	for _, tc := range []struct {
		name, evidence  string
		valid, complete bool
	}{
		{"observed join", `,"dependency_count":2,"dependencies":[{"id":"b","dependency_type":"blocks"},{"id":"c","dependency_type":"blocks"}]`, true, false},
		{"qualified join marker", `,"dependency_count":2,"dependencies":[{"id":"b","dependency_type":"blocks"},{"id":"c","dependency_type":"blocks"}],"dependencies_complete":true`, true, true},
		{"qualified zero marker", `,"dependency_count":0,"dependencies":[],"dependencies_complete":true`, true, true},
		{"unqualified zero marker", `,"dependency_count":0,"dependencies_complete":true`, false, false},
		{"legacy not upgraded", `,"dependency_count":0`, true, false},
		{"partial retained unsupported", `,"dependency_count":2,"dependencies":[{"id":"b","dependency_type":"blocks"}]`, true, false},
		{"missing count asserted complete", `,"dependencies_complete":true`, false, false},
		{"partial asserted complete", `,"dependency_count":2,"dependencies":[{"id":"b","dependency_type":"blocks"}],"dependencies_complete":true`, false, false},
		{"null count", `,"dependency_count":null,"dependencies":[],"dependencies_complete":true`, false, false},
		{"missing count", `,"dependencies":[],"dependencies_complete":true`, false, false},
		{"null dependencies", `,"dependency_count":0,"dependencies":null,"dependencies_complete":true`, false, false},
		{"null edge", `,"dependency_count":1,"dependencies":[null],"dependencies_complete":true`, false, false},
		{"blank endpoint", `,"dependency_count":1,"dependencies":[{"id":" ","dependency_type":"blocks"}],"dependencies_complete":true`, false, false},
		{"blank type", `,"dependency_count":1,"dependencies":[{"id":"b","dependency_type":" "}],"dependencies_complete":true`, false, false},
		{"wrong endpoint type", `,"dependency_count":1,"dependencies":[{"id":42,"dependency_type":"blocks"}],"dependencies_complete":true`, false, false},
		{"wrong relation type", `,"dependency_count":1,"dependencies":[{"id":"b","dependency_type":42}],"dependencies_complete":true`, false, false},
		{"wrong dependencies shape", `,"dependency_count":0,"dependencies":{},"dependencies_complete":true`, false, false},
		{"fractional count", `,"dependency_count":1.5,"dependencies":[],"dependencies_complete":true`, false, false},
		{"string count", `,"dependency_count":"0","dependencies":[],"dependencies_complete":true`, false, false},
		{"extra entries", `,"dependency_count":0,"dependencies":[{"id":"b","dependency_type":"blocks"}],"dependencies_complete":true`, false, false},
		{"negative count", `,"dependency_count":-1,"dependencies":[],"dependencies_complete":true`, false, false},
		{"duplicate tuple", `,"dependency_count":2,"dependencies":[{"id":"b","dependency_type":"blocks"},{"id":"b","dependency_type":"blocks"}],"dependencies_complete":true`, false, false},
		{"same target different type", `,"dependency_count":2,"dependencies":[{"id":"b","dependency_type":"blocks"},{"id":"b","dependency_type":"relates-to"}],"dependencies_complete":true`, true, true},
		{"external unknown", `,"dependency_count":1,"dependencies":[{"id":"external:project:capability","dependency_type":" Future Kind "}],"dependencies_complete":true`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: `{"id":"a","revision":"r1"` + tc.evidence + `}`, Valid: true}}
			err := validateWorkSourceCommandReport(cmd, &p)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
			if err == nil && strings.Contains(p.Result.String, `"dependencies_complete":true`) != tc.complete {
				t.Fatalf("report completeness was lost or synthesized: %s", p.Result.String)
			}
			if tc.name == "observed join" && (!strings.Contains(p.Result.String, `"id":"b","dependency_type":"blocks"`) || !strings.Contains(p.Result.String, `"id":"c","dependency_type":"blocks"`)) {
				t.Fatalf("report discarded predecessors: %s", p.Result.String)
			}
			if err == nil {
				canonical := p.Result
				if validateWorkSourceCommandReport(cmd, &p) != nil || p.Result != canonical {
					t.Fatalf("canonical receipt cannot replay byte-identically: %s", canonical.String)
				}
				if tc.name == "external unknown" && !strings.Contains(p.Result.String, `"id":"external:project:capability","dependency_type":" Future Kind "`) {
					t.Fatalf("opaque external edge changed: %s", p.Result.String)
				}
			}
		})
	}
}

func TestWorkSourceCommandDependencyBound(t *testing.T) {
	cmd := db.WorkSourceCommand{Command: "read", NativeID: pgtype.Text{String: "a", Valid: true}}
	for _, count := range []int{512, 513} {
		rows := make([]string, count)
		for i := range rows {
			rows[i] = fmt.Sprintf(`{"id":"b-%d","dependency_type":"blocks"}`, i)
		}
		p := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: fmt.Sprintf(`{"id":"a","revision":"r1","dependency_count":%d,"dependencies":[%s],"dependencies_complete":true}`, count, strings.Join(rows, ",")), Valid: true}}
		if err := validateWorkSourceCommandReport(cmd, &p); (err == nil) != (count == 512) {
			t.Fatalf("count=%d err=%v", count, err)
		}
	}
}

func TestWorkSourceCommandTopologyChangesCanonicalResult(t *testing.T) {
	cmd := db.WorkSourceCommand{Command: "read", NativeID: pgtype.Text{String: "a", Valid: true}}
	canonical := func(dependencies string) pgtype.Text {
		t.Helper()
		p := ReportWorkSourceCommandParams{Status: "succeeded", Result: pgtype.Text{String: `{"id":"a","revision":"unchanged","dependency_count":1,"dependencies":` + dependencies + `}`, Valid: true}}
		if err := validateWorkSourceCommandReport(cmd, &p); err != nil {
			t.Fatal(err)
		}
		return p.Result
	}
	original := canonical(`[{"id":"b","dependency_type":"blocks"}]`)
	if canonical(`[{"dependency_type":"blocks","id":"b"}]`) != original {
		t.Fatal("identical topology must replay canonically")
	}
	for _, changed := range []string{`[{"id":"c","dependency_type":"blocks"}]`, `[{"id":"b","dependency_type":"relates-to"}]`} {
		if canonical(changed) == original {
			t.Fatal("changed topology with unchanged item revision must differ for the existing terminal equality fence")
		}
	}
}
