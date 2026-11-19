package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

func TestDatabaseProfileIsFixedOnFirstStart(t *testing.T) {
	f := newSquadFixture(t)
	if e := f.store.ClaimProfile(f.ctx, store.ProfileDeployment); e != nil {
		t.Fatal(e)
	}
	if e := f.store.ClaimProfile(f.ctx, store.ProfileDeployment); e != nil {
		t.Fatalf("same profile again = %v", e)
	}
	if e := f.store.ClaimProfile(f.ctx, store.ProfileDevelopment); !errors.Is(e, store.ErrProfileMismatch) {
		t.Fatalf("development start of a deployment database = %v", e)
	}
}

// A database created before the record existed could only come from a development start.
func TestDatabaseWithWorkspacesButNoProfileIsDevelopment(t *testing.T) {
	f := newSquadFixture(t)
	f.login(t, "owner", store.RoleOwner)
	if e := f.store.ClaimProfile(f.ctx, store.ProfileDeployment); !errors.Is(e, store.ErrProfileMismatch) {
		t.Fatalf("deployment start of an existing development database = %v", e)
	}
	if e := f.store.ClaimProfile(f.ctx, store.ProfileDevelopment); e != nil {
		t.Fatal(e)
	}
}
