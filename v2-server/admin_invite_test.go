//go:build linux

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// adminInviteDatabase is TEST_DATABASE_URL pointed at a fresh schema, dropped afterwards.
func adminInviteDatabase(t *testing.T) string {
	t.Helper()
	url := os.Getenv("TEST_DATABASE_URL")
	if url == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	admin, err := pgx.Connect(t.Context(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close(context.Background()) })
	var nonce [8]byte
	_, _ = rand.Read(nonce[:])
	schema := "invite_" + hex.EncodeToString(nonce[:])
	if _, err := admin.Exec(t.Context(), "CREATE SCHEMA "+schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec(context.Background(), "DROP SCHEMA "+schema+" CASCADE") })
	// TEST_DATABASE_URL may be a URL or a keyword/value DSN; both pass search_path to the server.
	if !strings.Contains(url, "://") {
		return url + " search_path=" + schema + ",public"
	}
	sep := "?"
	if strings.Contains(url, "?") {
		sep = "&"
	}
	return url + sep + "search_path=" + schema + ",public"
}

var ownerLink = regexp.MustCompile(`(?m)^(\S*)/invite/([A-Za-z0-9_-]{43})$`)

func TestAdminInvitePrintsAOneTimeOwnerLink(t *testing.T) {
	db := adminInviteDatabase(t)
	t.Setenv("V2_LOGIN", "email")
	t.Setenv("V2_PUBLIC_URL", "")
	var out bytes.Buffer
	if err := adminInvite([]string{"-db", db}, &out); err == nil || !strings.Contains(err.Error(), "-team") {
		t.Fatalf("no workspace and no -team = %v", err)
	}
	out.Reset()
	if err := adminInvite([]string{"-db", db, "-team", "研发部"}, &out); err != nil {
		t.Fatal(err)
	}
	m := ownerLink.FindStringSubmatch(out.String())
	if m == nil || m[1] != "" || !strings.Contains(out.String(), "V2_PUBLIC_URL is not set") {
		t.Fatalf("output = %q", out.String())
	}

	// With one workspace the flag may be left out; the link is absolute under V2_PUBLIC_URL.
	t.Setenv("V2_PUBLIC_URL", "https://ora.example.com/")
	out.Reset()
	if err := adminInvite([]string{"-db", db}, &out); err != nil {
		t.Fatal(err)
	}
	if m := ownerLink.FindStringSubmatch(out.String()); m == nil || m[1] != "https://ora.example.com" || !strings.Contains(out.String(), `"研发部"`) {
		t.Fatalf("output = %q", out.String())
	}

	out.Reset()
	if err := adminInvite([]string{"-db", db, "-team", "运维部"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := adminInvite([]string{"-db", db}, &out); err == nil || !strings.Contains(err.Error(), "研发部") || !strings.Contains(err.Error(), "运维部") {
		t.Fatalf("several workspaces and no -team = %v", err)
	}
}

func TestAdminInviteChecksConfigurationFirst(t *testing.T) {
	t.Setenv("V2_LOGIN", "unknown")
	if err := adminInvite([]string{"-db", "invalid database URL", "-team", "x"}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "V2_LOGIN") {
		t.Fatalf("unknown login = %v", err)
	}
	t.Setenv("V2_LOGIN", "email")
	if err := adminInvite([]string{"-db", "invalid database URL", "extra"}, &bytes.Buffer{}); err == nil {
		t.Fatal("stray argument accepted")
	}
}
