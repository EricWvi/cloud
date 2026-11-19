package server_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/wanglongan587/cloud/v2-server/internal/server"
	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestDisablingAnAccountEndsItsSession(t *testing.T) {
	s, sql := openStoreSQL(t)
	token := storetest.SignIn(t, s, "demo", "演示用户", store.RoleOwner)
	srv := httptest.NewServer(server.New(s, server.Options{SessionTTL: time.Hour}))
	t.Cleanup(srv.Close)
	me := func() int {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/me", nil)
		req.AddCookie(&http.Cookie{Name: server.SessionCookie, Value: token})
		res, e := http.DefaultClient.Do(req)
		if e != nil {
			t.Fatal(e)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if status := me(); status != 200 {
		t.Fatalf("me = %d", status)
	}
	if _, e := sql.Exec(context.Background(), `UPDATE users SET status = 'disabled' WHERE subject = $1`, demoEmail); e != nil {
		t.Fatal(e)
	}
	// The session row is still there and unexpired; the account's status alone ends it.
	if status := me(); status != 401 {
		t.Fatalf("me after disabling = %d", status)
	}
}
