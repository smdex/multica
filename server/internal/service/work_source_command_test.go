package service

import (
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
