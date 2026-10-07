package service

import (
	"testing"

	"github.com/jackc/pgx/v5/pgtype"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestChatTaskResumePolicyRequiresNativeOnlyForImportedChat(t *testing.T) {
	fresh := db.ChatSession{InteractionMode: "chat"}
	if got := chatTaskResumePolicy(fresh); got != "allow_fresh" {
		t.Fatalf("fresh interactive chat policy = %q, want allow_fresh", got)
	}
	imported := db.ChatSession{InteractionMode: "chat", NativeImportID: pgtype.Text{String: "source-thread", Valid: true}}
	if got := chatTaskResumePolicy(imported); got != "require_native" {
		t.Fatalf("imported interactive chat policy = %q, want require_native", got)
	}
	importedAutonomous := db.ChatSession{InteractionMode: "autonomous", NativeImportID: pgtype.Text{String: "source-thread", Valid: true}}
	if got := chatTaskResumePolicy(importedAutonomous); got != "require_native" {
		t.Fatalf("imported autonomous chat policy = %q, want require_native", got)
	}
}
