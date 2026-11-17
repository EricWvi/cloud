package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
	"github.com/wanglongan587/cloud/v2-server/internal/store/storetest"
)

func TestRolesAreTenantSharedAndResolveByName(t *testing.T) {
	s := storetest.Open(t)
	owner, tenant := storetest.Login(t, s, "roles-owner")
	memberToken := storetest.SignIn(t, s, "roles-member", "roles-member", store.RoleMember)
	member, err := s.SessionUser(t.Context(), memberToken)
	if err != nil {
		t.Fatal(err)
	}

	created, err := s.CreateRole(t.Context(), tenant, owner, store.RoleInput{
		Name: "代码审查", Description: "检查改动", Content: "# 代码审查\n",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Version != 1 || created.Content != "# 代码审查\n" {
		t.Fatalf("created = %+v", created)
	}
	if _, err := s.CreateRole(t.Context(), tenant, owner, store.RoleInput{
		Name: "代码审查", Description: "重复", Content: "",
	}); !errors.Is(err, store.ErrRoleNameConflict) {
		t.Fatalf("duplicate error = %v", err)
	}

	// The catalog belongs to the workspace: a member reads it, and a role the member creates is
	// visible to everyone, so a shared workflow never fails for lack of another user's role.
	memberRoles, err := s.ListRoles(t.Context(), tenant)
	if err != nil {
		t.Fatal(err)
	}
	if len(memberRoles) != 1 || memberRoles[0].ID != created.ID {
		t.Fatalf("member roles = %+v", memberRoles)
	}
	written, err := s.CreateRole(t.Context(), tenant, member.ID, store.RoleInput{
		Name: "技术写作", Content: "# 技术写作",
	})
	if err != nil {
		t.Fatal(err)
	}
	if all, err := s.ListRoles(t.Context(), tenant); err != nil || len(all) != 2 {
		t.Fatalf("roles after member create = %+v %v", all, err)
	}

	for _, reference := range []string{"代码审查", "技术写作"} {
		resolved, err := s.ResolveWorkflowRole(t.Context(), tenant, reference)
		if err != nil || resolved.Name != reference {
			t.Fatalf("resolve %q = %+v %v", reference, resolved, err)
		}
	}
	// A graph may store the id instead of the name; that fallback stays inside the tenant.
	if resolved, err := s.ResolveWorkflowRole(t.Context(), tenant, written.ID); err != nil || resolved.ID != written.ID {
		t.Fatalf("resolve by id = %+v %v", resolved, err)
	}
	if _, err := s.ResolveWorkflowRole(t.Context(), "00000000-0000-4000-8000-000000000000", "代码审查"); !errors.Is(err, store.ErrRoleNotFound) {
		t.Fatalf("cross-tenant resolve error = %v", err)
	}
	if _, err := s.ResolveWorkflowRole(t.Context(), tenant, "不存在的角色"); !errors.Is(err, store.ErrRoleNotFound) {
		t.Fatalf("missing role error = %v", err)
	}
}

func TestRoleUpdatesKeepIdentityAndDeletionFreesTheName(t *testing.T) {
	s := storetest.Open(t)
	owner, tenant := storetest.Login(t, s, "roles-edit-owner")

	role, err := s.CreateRole(t.Context(), tenant, owner, store.RoleInput{
		Name: "release", Description: "发布检查", Content: "# Release",
	})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := s.UpdateRole(t.Context(), tenant, role.ID, store.RoleInput{
		Name: "release", Description: "发布前核对", Content: "# Release v2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != role.ID || updated.Version != 2 || updated.Content != "# Release v2" {
		t.Fatalf("updated = %+v", updated)
	}
	if _, err := s.CreateRole(t.Context(), tenant, owner, store.RoleInput{Name: "TRY"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UpdateRole(t.Context(), tenant, role.ID, store.RoleInput{Name: "Try"}); !errors.Is(err, store.ErrRoleNameConflict) {
		t.Fatalf("rename onto live name error = %v", err)
	}

	if err := s.DeleteRole(t.Context(), tenant, role.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetRole(t.Context(), tenant, role.ID); !errors.Is(err, store.ErrRoleNotFound) {
		t.Fatalf("deleted role error = %v", err)
	}
	if _, err := s.ResolveWorkflowRole(t.Context(), tenant, "release"); !errors.Is(err, store.ErrRoleNotFound) {
		t.Fatalf("deleted role still resolves: %v", err)
	}
	if err := s.DeleteRole(t.Context(), tenant, role.ID); !errors.Is(err, store.ErrRoleNotFound) {
		t.Fatalf("second delete error = %v", err)
	}
	recreated, err := s.CreateRole(t.Context(), tenant, owner, store.RoleInput{Name: "release"})
	if err != nil || recreated.ID == role.ID {
		t.Fatalf("recreate after delete = %+v %v", recreated, err)
	}
}

func TestRoleImportKeepsIdentityOnOverwrite(t *testing.T) {
	s := storetest.Open(t)
	owner, tenant := storetest.Login(t, s, "roles-import-owner")

	raw := []byte("---\nname: 事故处理\ndescription: 事故处理手册\n---\n\n# Runbook\n")
	imported, err := s.ImportRole(t.Context(), tenant, owner, raw, false)
	if err != nil {
		t.Fatal(err)
	}
	if imported.Name != "事故处理" || imported.Content != "# Runbook\n" {
		t.Fatalf("imported = %+v", imported)
	}
	if _, err := s.ImportRole(t.Context(), tenant, owner, raw, false); !errors.Is(err, store.ErrRoleNameConflict) {
		t.Fatalf("import conflict error = %v", err)
	}
	overwritten, err := s.ImportRole(t.Context(), tenant, owner, []byte("---\nname: incidents\ndescription: 覆盖\n---\n\n# Replacement\n"), true)
	if err != nil {
		t.Fatal(err)
	}
	if overwritten.Name != "incidents" {
		t.Fatalf("unrelated import overwrote = %+v", overwritten)
	}
	same := []byte("---\nname: INCIDENTS\ndescription: 覆盖说明\n---\n\n# Replacement v2\n")
	if _, err := s.ImportRole(t.Context(), tenant, owner, same, false); !errors.Is(err, store.ErrRoleNameConflict) {
		t.Fatalf("case-insensitive import conflict error = %v", err)
	}
	kept, err := s.ImportRole(t.Context(), tenant, owner, same, true)
	if err != nil {
		t.Fatal(err)
	}
	if kept.ID != overwritten.ID || kept.Version != overwritten.Version+1 || kept.Description != "覆盖说明" {
		t.Fatalf("overwrite kept = %+v, was %+v", kept, overwritten)
	}
	if _, err := s.ImportRole(t.Context(), tenant, owner, []byte("# no front matter\n"), false); err == nil {
		t.Fatal("import without front matter succeeded")
	}
}
