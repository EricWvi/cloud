//go:build linux

package main

import (
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// deploymentEnv configures a deployment start: IDaaS only, against a provider nothing calls here.
func deploymentEnv(t *testing.T, addr string) {
	t.Setenv("V2_LOGIN", "huawei-idaas")
	t.Setenv("V2_PUBLIC_URL", "http://"+addr)
	t.Setenv("V2_IDAAS_BASE_URL", "http://127.0.0.1:9")
	t.Setenv("V2_IDAAS_CLIENT_ID", "ora")
	t.Setenv("V2_IDAAS_CLIENT_SECRET", "secret")
	t.Setenv("V2_DATA_DIR", filepath.Join(t.TempDir(), "data"))
	t.Setenv("V2_PLUGIN_MARKETPLACE_URL", "")
	// The preset that a development start would seed: a deployment start must not read it.
	t.Setenv("V2_USERS_FILE", "users.json")
}

// refusedStart runs a start that must fail before serving; one that serves instead is stopped and
// fails the test rather than hanging it.
func refusedStart(t *testing.T, db string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- run(freeAddr(t), db, false, false, false) }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("start was not refused")
		}
		return err
	case <-time.After(30 * time.Second):
		_ = syscall.Kill(syscall.Getpid(), syscall.SIGTERM)
		<-done
		t.Fatal("start was not refused and served instead")
		return nil
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return "127.0.0.1:" + strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

// The deployment start must leave the database without the users.json accounts, the demo team and
// the demo project (#1059: a deployment carries no demo data).
func TestDeploymentStartSeedsNoDemoData(t *testing.T) {
	db := adminInviteDatabase(t)
	addr := freeAddr(t)
	deploymentEnv(t, addr)
	done := make(chan error, 1)
	go func() { done <- run(addr, db, false, false, false) }()
	// No kept-alive connection may outlive the requests, or shutdown would wait for it.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	ready := false
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		select {
		case err := <-done:
			t.Fatalf("deployment start failed: %v", err)
		default:
		}
		if res, err := client.Get("http://" + addr + "/healthz"); err == nil {
			res.Body.Close()
			if ready = res.StatusCode == 200; ready {
				break
			}
		}
	}
	if !ready {
		t.Fatal("deployment start never became ready")
	}
	res, err := client.Get("http://" + addr + "/auth/providers")
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()

	conn, err := pgx.Connect(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var users, tenants, projects int
	var profile string
	if err := conn.QueryRow(t.Context(), `SELECT (SELECT count(*) FROM users), (SELECT count(*) FROM tenants), (SELECT count(*) FROM v2_projects), (SELECT profile FROM v2_instance)`).Scan(&users, &tenants, &projects, &profile); err != nil {
		t.Fatal(err)
	}
	if users != 0 || tenants != 0 || projects != 0 || profile != "deployment" {
		t.Fatalf("deployment database holds users=%d tenants=%d projects=%d profile=%q", users, tenants, projects, profile)
	}

	if err := syscall.Kill(syscall.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("shutdown: %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("deployment start did not stop")
	}

	// The same database is now a deployment database: a development start is refused before it
	// seeds anything.
	t.Setenv("V2_LOGIN", "email")
	err = refusedStart(t, db)
	if !strings.Contains(err.Error(), "deployment database") {
		t.Fatalf("development start of a deployment database = %v", err)
	}
	if err := conn.QueryRow(t.Context(), `SELECT count(*) FROM users`).Scan(&users); err != nil || users != 0 {
		t.Fatalf("refused start seeded users=%d %v", users, err)
	}
}

func TestDeploymentStartRefusesADevelopmentDatabase(t *testing.T) {
	db := adminInviteDatabase(t)
	conn, err := pgx.Connect(t.Context(), db)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	addr := freeAddr(t)
	deploymentEnv(t, addr)
	// A development database: a previous email start recorded it.
	t.Setenv("V2_LOGIN", "email")
	if err := adminInvite([]string{"-db", db, "-team", "演示团队"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("V2_LOGIN", "huawei-idaas")
	err = refusedStart(t, db)
	if !strings.Contains(err.Error(), "development database") {
		t.Fatalf("deployment start of a development database = %v", err)
	}
}

func TestDeploymentConfigurationIsCheckedBeforeEffects(t *testing.T) {
	for name, tc := range map[string]map[string]string{
		"no public URL":         {"V2_PUBLIC_URL": ""},
		"plain http public URL": {"V2_PUBLIC_URL": "http://ora.example.com"},
		"no client id":          {"V2_IDAAS_CLIENT_ID": ""},
		"no client secret":      {"V2_IDAAS_CLIENT_SECRET": ""},
		"plain http IDaaS":      {"V2_IDAAS_BASE_URL": "http://idaas.example.com"},
		"unknown IDaaS API":     {"V2_IDAAS_API": "v3"},
		"legacy value":          {"V2_LOGIN": "idaas"},
	} {
		t.Run(name, func(t *testing.T) {
			deploymentEnv(t, "127.0.0.1:1")
			t.Setenv("V2_PUBLIC_URL", "https://ora.example.com")
			for k, v := range tc {
				t.Setenv(k, v)
			}
			root := filepath.Join(t.TempDir(), "must-not-exist")
			t.Setenv("V2_DATA_DIR", root)
			if err := run("127.0.0.1:0", "invalid database URL", false, false, false); err == nil {
				t.Fatal("deployment started with a bad configuration")
			}
			if _, err := os.Stat(root); !os.IsNotExist(err) {
				t.Fatal("startup touched the data directory before rejecting the configuration")
			}
		})
	}
	// Fixtures drive the development accounts, which a deployment has none of.
	deploymentEnv(t, "127.0.0.1:1")
	t.Setenv("V2_PUBLIC_URL", "https://ora.example.com")
	for _, fixture := range [][2]bool{{true, false}, {false, true}} {
		if err := run("127.0.0.1:0", "invalid database URL", false, fixture[0], fixture[1]); err == nil || !strings.Contains(err.Error(), "fixture flags require the email login") {
			t.Fatalf("fixture flags %v = %v", fixture, err)
		}
	}
	// -cleanup only removes Node conversation directories while the server is stopped; it works in
	// a deployment too.
	if err := run("127.0.0.1:0", "invalid database URL", true, false, false); err != nil {
		t.Fatalf("deployment cleanup = %v", err)
	}
	// It needs no login configuration either.
	t.Setenv("V2_IDAAS_CLIENT_ID", "")
	t.Setenv("V2_IDAAS_CLIENT_SECRET", "")
	if err := run("127.0.0.1:0", "invalid database URL", true, false, false); err != nil {
		t.Fatalf("cleanup without IDaaS configuration = %v", err)
	}
}
