package store_test

import (
	"errors"
	"testing"

	"github.com/wanglongan587/cloud/v2-server/internal/store"
)

// outsider is an account that belongs to no workspace, like someone who just signed in.
func (f *squadFixture) outsider(t *testing.T, name string) string {
	t.Helper()
	var id string
	if e := f.db.QueryRow(f.ctx, `INSERT INTO users (source, subject, display_name) VALUES ('test', $1, $1) RETURNING id`, name).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return id
}

func (f *squadFixture) squad(t *testing.T, name string, owner string) string {
	t.Helper()
	var id string
	if e := f.db.QueryRow(f.ctx, `INSERT INTO v2_squads (tenant_id, name, created_by) VALUES ($1, $2, $3) RETURNING id`, f.tenant, name, owner).Scan(&id); e != nil {
		t.Fatal(e)
	}
	return id
}

func (f *squadFixture) inSquad(t *testing.T, squad, user string) bool {
	t.Helper()
	var live bool
	if e := f.db.QueryRow(f.ctx, `SELECT EXISTS (SELECT 1 FROM v2_squad_members WHERE squad_id = $1 AND user_id = $2 AND ended_at IS NULL)`, squad, user).Scan(&live); e != nil {
		t.Fatal(e)
	}
	return live
}

func (f *squadFixture) member(t *testing.T, tenant, user string) string {
	t.Helper()
	var role string
	e := f.db.QueryRow(f.ctx, `SELECT role FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant, user).Scan(&role)
	if e != nil {
		return ""
	}
	return role
}

func TestInviteLinkJoinsOnceWithItsSquad(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", store.RoleOwner)
	squad := f.squad(t, "后端", owner)
	link, token, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, squad)
	if e != nil || token == "" || link.Role != store.RoleMember || link.Squad == nil || link.Squad.Name != "后端" || link.CreatedByName == nil || *link.CreatedByName != "owner" {
		t.Fatalf("link = %+v %q %v", link, token, e)
	}
	var stored []byte
	if e := f.db.QueryRow(f.ctx, `SELECT token_hash FROM v2_invite_links WHERE id = $1`, link.ID).Scan(&stored); e != nil || string(stored) == token || len(stored) != 32 {
		t.Fatalf("stored token = %x %v", stored, e)
	}

	alice := f.outsider(t, "alice")
	p, e := f.store.PreviewInvite(f.ctx, token, alice)
	if e != nil || p.State != store.InviteActive || p.AlreadyMember || p.Tenant.ID != f.tenant || p.Squad == nil || p.Squad.ID != squad {
		t.Fatalf("preview = %+v %v", p, e)
	}
	if f.member(t, f.tenant, alice) != "" {
		t.Fatal("preview joined the workspace")
	}
	tenant, e := f.store.AcceptInvite(f.ctx, token, alice)
	if e != nil || tenant != f.tenant || f.member(t, f.tenant, alice) != store.RoleMember || !f.inSquad(t, squad, alice) {
		t.Fatalf("accept = %q %v", tenant, e)
	}
	// Opening it again as the member who used it just returns the workspace.
	if tenant, e := f.store.AcceptInvite(f.ctx, token, alice); e != nil || tenant != f.tenant {
		t.Fatalf("member reopening = %q %v", tenant, e)
	}
	// Anyone else finds it used.
	bob := f.outsider(t, "bob")
	if p, e := f.store.PreviewInvite(f.ctx, token, bob); e != nil || p.State != store.InviteUsed {
		t.Fatalf("preview after use = %+v %v", p, e)
	}
	var unavailable store.InviteUnavailableError
	if _, e := f.store.AcceptInvite(f.ctx, token, bob); !errors.As(e, &unavailable) || unavailable.Reason != store.InviteUsed || f.member(t, f.tenant, bob) != "" {
		t.Fatalf("reuse = %v", e)
	}
	links, e := f.store.ListInviteLinks(f.ctx, f.tenant)
	if e != nil || len(links) != 1 || links[0].UsedByName == nil || *links[0].UsedByName != "alice" || links[0].UsedAt == nil {
		t.Fatalf("links = %+v %v", links, e)
	}
	if _, e := f.store.PreviewInvite(f.ctx, "not-a-token", bob); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("unknown token = %v", e)
	}
}

func TestAMemberOpeningALinkDoesNotUseItUp(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", store.RoleOwner)
	_, token, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, "")
	if e != nil {
		t.Fatal(e)
	}
	if p, e := f.store.PreviewInvite(f.ctx, token, owner); e != nil || !p.AlreadyMember {
		t.Fatalf("owner preview = %+v %v", p, e)
	}
	if _, e := f.store.AcceptInvite(f.ctx, token, owner); e != nil {
		t.Fatal(e)
	}
	alice := f.outsider(t, "alice")
	if _, e := f.store.AcceptInvite(f.ctx, token, alice); e != nil || f.member(t, f.tenant, alice) != store.RoleMember {
		t.Fatalf("link was used up by a member: %v", e)
	}
}

func TestRevokedAndExpiredLinksAreRefused(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", store.RoleOwner)
	revoked, revokedToken, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, "")
	if e != nil {
		t.Fatal(e)
	}
	if e := f.store.RevokeInviteLink(f.ctx, f.tenant, revoked.ID); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RevokeInviteLink(f.ctx, f.tenant, revoked.ID); e != nil {
		t.Fatalf("second revoke = %v", e)
	}
	expired, expiredToken, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.db.Exec(f.ctx, `UPDATE v2_invite_links SET expires_at = now() - interval '1 second' WHERE id = $1`, expired.ID); e != nil {
		t.Fatal(e)
	}
	alice := f.outsider(t, "alice")
	for token, reason := range map[string]string{revokedToken: store.InviteRevoked, expiredToken: store.InviteExpired} {
		var unavailable store.InviteUnavailableError
		if _, e := f.store.AcceptInvite(f.ctx, token, alice); !errors.As(e, &unavailable) || unavailable.Reason != reason {
			t.Fatalf("accept %s = %v", reason, e)
		}
	}
	if f.member(t, f.tenant, alice) != "" {
		t.Fatal("refused link joined the workspace")
	}
	if links, e := f.store.ListInviteLinks(f.ctx, f.tenant); e != nil || len(links) != 0 {
		t.Fatalf("unusable unused links listed: %+v %v", links, e)
	}

	used, usedToken, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, "")
	if e != nil {
		t.Fatal(e)
	}
	if _, e := f.store.AcceptInvite(f.ctx, usedToken, alice); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RevokeInviteLink(f.ctx, f.tenant, used.ID); !errors.Is(e, store.ErrInviteUsed) {
		t.Fatalf("revoke used = %v", e)
	}
	if e := f.store.RevokeInviteLink(f.ctx, f.tenant, "00000000-0000-0000-0000-000000000000"); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("revoke unknown = %v", e)
	}
}

func TestInviteLinkSquadMustBeLiveInTheWorkspace(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", store.RoleOwner)
	squad := f.squad(t, "旧小队", owner)
	if _, e := f.db.Exec(f.ctx, `UPDATE v2_squads SET archived_at = now() WHERE id = $1`, squad); e != nil {
		t.Fatal(e)
	}
	if _, _, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, squad); !errors.Is(e, store.ErrSquadNotFound) {
		t.Fatalf("archived squad = %v", e)
	}
	if _, _, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, "00000000-0000-0000-0000-000000000000"); !errors.Is(e, store.ErrSquadNotFound) {
		t.Fatalf("unknown squad = %v", e)
	}
}

func TestOwnerInviteCreatesTheWorkspaceAndItsFirstOwner(t *testing.T) {
	f := newSquadFixture(t)
	tenant, token, e := f.store.CreateOwnerInvite(f.ctx, "研发部")
	if e != nil {
		t.Fatal(e)
	}
	if names, e := f.store.TenantNames(f.ctx); e != nil || len(names) != 1 || names[0] != "研发部" {
		t.Fatalf("names = %v %v", names, e)
	}
	first := f.outsider(t, "first")
	if p, e := f.store.PreviewInvite(f.ctx, token, first); e != nil || p.Role != store.RoleOwner || p.Tenant.Name != "研发部" {
		t.Fatalf("preview = %+v %v", p, e)
	}
	if _, e := f.store.AcceptInvite(f.ctx, token, first); e != nil || f.member(t, tenant, first) != store.RoleOwner {
		t.Fatalf("accept = %v", e)
	}
	// The new workspace gets the space its projects and Agents need.
	var spaces int
	if e := f.db.QueryRow(f.ctx, `SELECT count(*) FROM v2_spaces WHERE tenant_id = $1`, tenant).Scan(&spaces); e != nil || spaces != 1 {
		t.Fatalf("spaces = %d %v", spaces, e)
	}
	// Running the command again for the same name reuses the workspace.
	if again, _, e := f.store.CreateOwnerInvite(f.ctx, "研发部"); e != nil || again != tenant {
		t.Fatalf("again = %q %v", again, e)
	}
}

func TestRemoveMemberKeepsTheAccountAndTheLastOwner(t *testing.T) {
	f := newSquadFixture(t)
	owner := f.login(t, "owner", store.RoleOwner)
	squad := f.squad(t, "后端", owner)
	_, token, e := f.store.CreateInviteLink(f.ctx, f.tenant, owner, squad)
	if e != nil {
		t.Fatal(e)
	}
	alice := f.outsider(t, "alice")
	if _, e := f.store.AcceptInvite(f.ctx, token, alice); e != nil {
		t.Fatal(e)
	}
	var version int64
	if e := f.db.QueryRow(f.ctx, `SELECT version FROM v2_squads WHERE id = $1`, squad).Scan(&version); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RemoveMember(f.ctx, f.tenant, alice); e != nil {
		t.Fatal(e)
	}
	if f.member(t, f.tenant, alice) != "" || f.inSquad(t, squad, alice) {
		t.Fatal("removed member still in the workspace or squad")
	}
	var after int64
	var history int
	if e := f.db.QueryRow(f.ctx, `SELECT version FROM v2_squads WHERE id = $1`, squad).Scan(&after); e != nil || after != version+1 {
		t.Fatalf("squad version %d -> %d %v", version, after, e)
	}
	if e := f.db.QueryRow(f.ctx, `SELECT count(*) FROM v2_squad_members WHERE squad_id = $1 AND user_id = $2`, squad, alice).Scan(&history); e != nil || history != 1 {
		t.Fatalf("squad history rows = %d %v", history, e)
	}
	var live bool
	if e := f.db.QueryRow(f.ctx, `SELECT deleted_at IS NULL FROM users WHERE id = $1`, alice).Scan(&live); e != nil || !live {
		t.Fatalf("account gone: %v", e)
	}
	if e := f.store.RemoveMember(f.ctx, f.tenant, alice); !errors.Is(e, store.ErrNotFound) {
		t.Fatalf("remove twice = %v", e)
	}
	if e := f.store.RemoveMember(f.ctx, f.tenant, owner); !errors.Is(e, store.ErrLastOwner) || f.member(t, f.tenant, owner) != store.RoleOwner {
		t.Fatalf("remove last owner = %v", e)
	}
	// With a second owner, either may be removed, but not both.
	second := f.outsider(t, "second")
	if _, e := f.db.Exec(f.ctx, `INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, 'owner')`, f.tenant, second); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RemoveMember(f.ctx, f.tenant, owner); e != nil {
		t.Fatal(e)
	}
	if e := f.store.RemoveMember(f.ctx, f.tenant, second); !errors.Is(e, store.ErrLastOwner) {
		t.Fatalf("remove the remaining owner = %v", e)
	}
}
