package main

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
	"github.com/wanglongan587/cloud/v2-server/internal/projectrepos"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestInterruptedTurnsDeliveredBeforeListener(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := t.Context()
	admin, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	schema := fmt.Sprintf("boot_delivery_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	defer admin.Exec(ctx, "DROP SCHEMA "+schema+" CASCADE")
	dsn = withSearchPath(t, dsn, schema+",public")

	preset, err := store.LoadPreset("users.json")
	if err != nil {
		t.Fatal(err)
	}
	db, err := store.Open(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SeedUsers(ctx, preset); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	projects, err := projectrepos.New(db, projectrepos.Options{Root: filepath.Join(root, "repositories")})
	if err != nil {
		t.Fatal(err)
	}
	scope, project, err := prepareDemo(ctx, db, projects, preset.Users[0].Email)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := db.CreateChatConversation(ctx, store.NewChatConversation{
		TenantID: scope.TenantID, UserID: scope.UserID, CreateKey: "startup", ProjectID: project.ID, ExecutorID: "opencode:fixture", Title: "startup",
	})
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(root, "checkout")
	cmd := exec.Command("git", "clone", project.MirrorPath, checkout)
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("clone fixture: %v: %s", err, raw)
	}
	if err := os.WriteFile(filepath.Join(checkout, "change"), []byte("interrupted work"), 0600); err != nil {
		t.Fatal(err)
	}
	session := execution.Session{ID: "startup-session", WorkspaceID: "startup-workspace", Target: &execution.Target{ProjectID: project.ID}}
	ref := execution.Ref{SessionID: session.ID, TurnID: "startup-turn"}
	caller := execution.Caller{Kind: execution.CallerChat, ID: conversation.ID, AttemptID: "startup"}
	if err := db.CreateExecutionSession(ctx, scope, session, execution.Turn{Ref: ref, Caller: caller, Prompt: execution.TextPrompt("prepare")}); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordWorkspace(ctx, session.WorkspaceID, store.WorkspaceLocation{Path: checkout, BaseCommit: project.Commit}); err != nil {
		t.Fatal(err)
	}

	stale := filepath.Join(root, "delivery-scratch", "delivery-interrupted", "objects")
	if err := os.MkdirAll(stale, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(stale, "blob"), []byte("abandoned scratch object"), 0600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("V2_DATA_DIR", root)
	t.Setenv("V2_LOGIN", "email")
	t.Setenv("V2_USERS_FILE", "users.json")
	t.Setenv("V2_NODE_ASSETS_DIR", "")
	t.Setenv("V2_FRONTEND_DIR", "")
	t.Setenv("V2_PLUGIN_MARKETPLACE_URL", "")
	checked := errors.New("first listener request checked")
	err = runWithListener("127.0.0.1:0", dsn, false, false, true, func(srv *http.Server) error {
		scratch, err := os.ReadDir(filepath.Join(root, "delivery-scratch"))
		if err != nil || len(scratch) != 0 {
			return fmt.Errorf("boot retained delivery scratch: %v, %v", scratch, err)
		}
		recorder := httptest.NewRecorder()
		srv.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if recorder.Code != http.StatusOK {
			return fmt.Errorf("first request: got %d", recorder.Code)
		}
		view, err := db.WorkspaceDeliveryView(ctx, scope, store.DeliverySource{Kind: execution.CallerChat, ID: conversation.ID})
		if err != nil || view.Good == nil || view.Good.Status != store.DeliveryDelivered || view.Good.TurnID != ref.TurnID || view.InProgress {
			return fmt.Errorf("first request preceded delivery: %+v, %v", view, err)
		}
		return checked
	})
	if !errors.Is(err, checked) {
		t.Fatalf("startup: %v", err)
	}
}

// withSearchPath returns dsn with search_path set, for both connection string forms pgx accepts: a
// postgres:// URL gets a query parameter, a keyword/value string (as CI uses) gets one more pair.
func withSearchPath(t *testing.T, dsn, searchPath string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		database, err := url.Parse(dsn)
		if err != nil {
			t.Fatal(err)
		}
		query := database.Query()
		query.Set("search_path", searchPath)
		database.RawQuery = query.Encode()
		return database.String()
	}
	return dsn + " search_path='" + searchPath + "'"
}
