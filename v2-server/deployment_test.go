//go:build linux

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoginPreflightBeforeEffects(t *testing.T) {
	for _, login := range []string{"unknown", "idaas", "email,email", "email,", " , "} {
		t.Run(login, func(t *testing.T) {
			t.Setenv("V2_LOGIN", login)
			root := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("V2_DATA_DIR", root)
			err := run("127.0.0.1:0", "invalid database URL", false, false, false)
			if err == nil || !strings.Contains(err.Error(), "V2_LOGIN") {
				t.Fatalf("startup accepted V2_LOGIN=%q: %v", login, err)
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("startup touched filesystem before rejecting the login configuration")
			}
		})
	}
}

func TestLoginDefaultsToEmailInListedOrder(t *testing.T) {
	for value, want := range map[string]string{"": "email", "  ": "email", " email ": "email"} {
		got, err := parseLogin(value, true)
		if err != nil || strings.Join(got, ",") != want {
			t.Fatalf("parseLogin(%q) = %v, %v; want %s", value, got, err, want)
		}
	}
	got, err := parseLogin("", false)
	if err != nil || !got.has("email") || got.has("huawei-idaas") {
		t.Fatalf("default login = %v, %v", got, err)
	}
}

func TestPublicURL(t *testing.T) {
	for value, want := range map[string]string{
		"":                             "",
		"https://ora.example.com":      "https://ora.example.com",
		"https://ora.example.com/ora/": "https://ora.example.com/ora",
		"http://127.0.0.1:8090":        "http://127.0.0.1:8090",
	} {
		got, err := parsePublicURL(value)
		if err != nil || (got == nil) != (want == "") || (got != nil && got.String() != want) {
			t.Fatalf("parsePublicURL(%q) = %v, %v; want %q", value, got, err, want)
		}
	}
	for _, value := range []string{"ora.example.com", "ftp://ora.example.com", "https://ora.example.com/?a=1", "https://ora.example.com/#x", "https://user@ora.example.com", "https://"} {
		if _, err := parsePublicURL(value); err == nil {
			t.Fatalf("parsePublicURL(%q) accepted", value)
		}
	}
	t.Setenv("V2_PUBLIC_URL", "ora.example.com")
	root := filepath.Join(t.TempDir(), "must-not-exist")
	t.Setenv("V2_DATA_DIR", root)
	if err := run("127.0.0.1:0", "invalid database URL", false, false, false); err == nil || !strings.Contains(err.Error(), "V2_PUBLIC_URL") {
		t.Fatalf("startup accepted invalid V2_PUBLIC_URL: %v", err)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("startup touched filesystem before rejecting V2_PUBLIC_URL")
	}
}

func TestOptionalIntegrationUnsetAndEmpty(t *testing.T) {
	const key = "V2_PLUGIN_MARKETPLACE_URL"
	old, wasSet := os.LookupEnv(key)
	t.Cleanup(func() {
		if wasSet {
			os.Setenv(key, old)
		} else {
			os.Unsetenv(key)
		}
	})
	os.Unsetenv(key)
	if got := envDefault(key, "development"); got != "development" {
		t.Fatalf("unset=%q", got)
	}
	t.Setenv(key, "")
	if got := envDefault(key, "development"); got != "" {
		t.Fatalf("empty=%q", got)
	}
	t.Setenv(key, "https://internal.example/market")
	if got := envDefault(key, "development"); got != "https://internal.example/market" {
		t.Fatalf("custom=%q", got)
	}
}

func TestDeploymentPreflightPreservesExistingData(t *testing.T) {
	root := t.TempDir()
	file := filepath.Join(root, "existing-data")
	if err := os.WriteFile(file, []byte("preserve existing data"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("V2_LOGIN", "idaas")
	t.Setenv("V2_DATA_DIR", root)
	t.Setenv("V2_USERS_FILE", "nonexistent-preset")
	if err := run("127.0.0.1:0", "invalid database URL", false, false, false); err == nil {
		t.Fatal("deployment unexpectedly started")
	}
	if data, err := os.ReadFile(file); err != nil || string(data) != "preserve existing data" {
		t.Fatalf("existing data changed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(root, "server.lock")); !os.IsNotExist(err) {
		t.Fatal("preflight acquired data directory")
	}
}
