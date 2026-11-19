package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestSignInExternalFindsOrCreatesWithoutJoining(t *testing.T) {
	f := newSquadFixture(t)
	first, e := f.store.SignInExternal(f.ctx, "huawei-corp", "uuid~abc", "张三")
	if e != nil || first.DisplayName != "张三" || first.Status != "active" {
		t.Fatalf("first = %+v %v", first, e)
	}
	again, e := f.store.SignInExternal(f.ctx, "huawei-corp", "uuid~abc", "张三丰")
	if e != nil || again.ID != first.ID || again.DisplayName != "张三丰" {
		t.Fatalf("again = %+v %v", again, e)
	}
	// The same subject in another namespace is another person.
	other, e := f.store.SignInExternal(f.ctx, "github", "uuid~abc", "")
	if e != nil || other.ID == first.ID || other.DisplayName != "uuid~abc" {
		t.Fatalf("other = %+v %v", other, e)
	}
	if tenants, e := f.store.Tenants(f.ctx, first.ID); e != nil || len(tenants) != 0 {
		t.Fatalf("signing in joined %v %v", tenants, e)
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE users SET status = 'disabled' WHERE id = $1`, first.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SignInExternal(f.ctx, "huawei-corp", "uuid~abc", "改名"); !errors.Is(e, store.ErrAccountDisabled) {
		t.Fatalf("disabled = %v", e)
	}
	var name string
	if e := f.db.QueryRow(f.ctx, `SELECT display_name FROM users WHERE id = $1`, first.ID).Scan(&name); e != nil || name != "张三丰" {
		t.Fatalf("disabled account changed: %q %v", name, e)
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE users SET status = 'active', deleted_at = now() WHERE id = $1`, first.ID); e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.SignInExternal(f.ctx, "huawei-corp", "uuid~abc", "张三"); !errors.Is(e, store.ErrAccountDisabled) {
		t.Fatalf("deleted = %v", e)
	}
}
