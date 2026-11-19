//go:build linux

package main

import (
	"bytes"
	"strings"
	"testing"
)

// Listing only reads: it works while a server holds the data directory.
func TestReclaimsListsWhileTheServerRuns(t *testing.T) {
	db := adminInviteDatabase(t)
	data := t.TempDir()
	t.Setenv("V2_DATA_DIR", data)
	lock, err := lockDirectory(data)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out bytes.Buffer
	if err := reclaimsCommand([]string{"-db", db, "kept"}, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "WORKSPACE") {
		t.Fatalf("listing = %q", out.String())
	}
	if err := reclaimsCommand([]string{"-db", db, "gone"}, &out); err == nil {
		t.Fatal("an unknown status was accepted")
	}
}

// Discarding removes files, so it refuses while a server holds the data directory.
func TestDiscardKeptRefusesWhileTheServerRuns(t *testing.T) {
	data := t.TempDir()
	t.Setenv("V2_DATA_DIR", data)
	lock, err := lockDirectory(data)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	var out bytes.Buffer
	err = discardKeptCommand([]string{"-db", "postgres://unused.invalid/none", "some-workspace"}, &out)
	if err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("discard with the server running = %v", err)
	}
}
