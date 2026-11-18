package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// MemberInviteTTL is how long an owner's member link stays usable (A6 D4).
const MemberInviteTTL = 7 * 24 * time.Hour

// OwnerInviteTTL is how long an admin-invite owner link stays usable (A6 D4).
const OwnerInviteTTL = 24 * time.Hour

// ErrInviteUsed reports revoking a link someone already used.
var ErrInviteUsed = errors.New("invite already used")

// ErrLastOwner reports removing the workspace's only owner.
var ErrLastOwner = errors.New("last owner")

// Invite link states, as the preview reports them.
const (
	InviteActive  = "active"
	InviteUsed    = "used"
	InviteExpired = "expired"
	InviteRevoked = "revoked"
)

// InviteUnavailableError reports a link that can no longer be used, and why.
type InviteUnavailableError struct{ Reason string }

func (e InviteUnavailableError) Error() string { return "invite " + e.Reason }

// InviteSquad names the squad a link joins.
type InviteSquad struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// InviteLink is one link as its workspace's owners see it. The token is never stored, so it is
// never listed: only the response that generated a link carries it.
type InviteLink struct {
	ID            string       `json:"id"`
	Role          string       `json:"role"`
	Squad         *InviteSquad `json:"squad"`
	CreatedByName *string      `json:"createdByName"`
	CreatedAt     time.Time    `json:"createdAt"`
	ExpiresAt     time.Time    `json:"expiresAt"`
	UsedByName    *string      `json:"usedByName"`
	UsedAt        *time.Time   `json:"usedAt"`
}

// InvitePreview is what opening a link shows the signed-in visitor, without changing anything.
type InvitePreview struct {
	State         string       `json:"state"`
	Tenant        InviteTenant `json:"tenant"`
	Squad         *InviteSquad `json:"squad"`
	Role          string       `json:"role"`
	AlreadyMember bool         `json:"alreadyMember"`
}

// InviteTenant names the workspace a link joins.
type InviteTenant struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// newInviteToken is 256 random bits; only its SHA-256 reaches the database.
func newInviteToken() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", fmt.Errorf("invite token: %w", e)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// CreateInviteLink makes a one-time member link into the workspace, optionally joining one live
// squad. The caller is an owner (the route checks it). It returns the link and its token, which no
// later read can recover.
func (s *Store) CreateInviteLink(ctx context.Context, tenant, creator, squad string) (InviteLink, string, error) {
	token, e := newInviteToken()
	if e != nil {
		return InviteLink{}, "", e
	}
	var id string
	e = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		var squadArg any
		if squad != "" {
			var live bool
			if e := tx.QueryRow(ctx, `
				SELECT EXISTS (SELECT 1 FROM v2_squads WHERE id = $2 AND tenant_id = $1 AND archived_at IS NULL)`,
				tenant, squad).Scan(&live); e != nil {
				return fmt.Errorf("look up squad: %w", e)
			}
			if !live {
				return ErrSquadNotFound
			}
			squadArg = squad
		}
		return tx.QueryRow(ctx, `
			INSERT INTO v2_invite_links (token_hash, tenant_id, role, squad_id, created_by, expires_at)
			VALUES ($1, $2, 'member', $3, $4, now() + $5::interval)
			RETURNING id`, hash(token), tenant, squadArg, creator, MemberInviteTTL.String()).Scan(&id)
	})
	if e != nil {
		return InviteLink{}, "", e
	}
	links, e := s.inviteLinks(ctx, tenant, id)
	if e != nil {
		return InviteLink{}, "", e
	}
	if len(links) != 1 {
		return InviteLink{}, "", fmt.Errorf("read created invite link: %d rows", len(links))
	}
	return links[0], token, nil
}

// CreateOwnerInvite is the admin-invite command's write: it makes sure the named workspace exists
// and adds a one-time owner link into it. No HTTP route reaches it.
func (s *Store) CreateOwnerInvite(ctx context.Context, tenantName string) (tenantID, token string, e error) {
	token, e = newInviteToken()
	if e != nil {
		return "", "", e
	}
	e = pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if e := tx.QueryRow(ctx, `
			INSERT INTO tenants (name) VALUES ($1)
			ON CONFLICT (name) DO UPDATE SET name = excluded.name
			RETURNING id`, tenantName).Scan(&tenantID); e != nil {
			return fmt.Errorf("ensure tenant: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO v2_invite_links (token_hash, tenant_id, role, expires_at)
			VALUES ($1, $2, 'owner', now() + $3::interval)`, hash(token), tenantID, OwnerInviteTTL.String()); e != nil {
			return fmt.Errorf("create owner invite: %w", e)
		}
		return nil
	})
	return tenantID, token, e
}

// TenantNames lists every workspace name, for the admin-invite command to pick or report.
func (s *Store) TenantNames(ctx context.Context) ([]string, error) {
	rows, e := s.pool.Query(ctx, `SELECT name FROM tenants ORDER BY name`)
	if e != nil {
		return nil, fmt.Errorf("list tenant names: %w", e)
	}
	names, e := pgx.CollectRows(rows, pgx.RowTo[string])
	if e != nil {
		return nil, fmt.Errorf("list tenant names: %w", e)
	}
	return names, nil
}

// ListInviteLinks returns the workspace's links still usable, then the used ones, newest first.
// Expired and revoked links that nobody used are left out.
func (s *Store) ListInviteLinks(ctx context.Context, tenant string) ([]InviteLink, error) {
	return s.inviteLinks(ctx, tenant, "")
}

func (s *Store) inviteLinks(ctx context.Context, tenant, id string) ([]InviteLink, error) {
	rows, e := s.pool.Query(ctx, `
		SELECT l.id, l.role, sq.id, sq.name, cu.display_name, l.created_at, l.expires_at, uu.display_name, l.used_at
		FROM v2_invite_links l
		LEFT JOIN v2_squads sq ON sq.id = l.squad_id
		LEFT JOIN users cu ON cu.id = l.created_by
		LEFT JOIN users uu ON uu.id = l.used_by
		WHERE l.tenant_id = $1 AND ($2 = '' OR l.id::text = $2)
			AND (l.used_at IS NOT NULL OR (l.revoked_at IS NULL AND l.expires_at > now()))
		ORDER BY l.used_at IS NOT NULL, COALESCE(l.used_at, l.created_at) DESC, l.id`, tenant, id)
	if e != nil {
		return nil, fmt.Errorf("list invite links: %w", e)
	}
	defer rows.Close()
	links := make([]InviteLink, 0)
	for rows.Next() {
		var l InviteLink
		var squadID, squadName *string
		if e := rows.Scan(&l.ID, &l.Role, &squadID, &squadName, &l.CreatedByName, &l.CreatedAt, &l.ExpiresAt, &l.UsedByName, &l.UsedAt); e != nil {
			return nil, fmt.Errorf("scan invite link: %w", e)
		}
		if squadID != nil {
			l.Squad = &InviteSquad{ID: *squadID, Name: *squadName}
		}
		links = append(links, l)
	}
	if e := rows.Err(); e != nil {
		return nil, fmt.Errorf("list invite links: %w", e)
	}
	return links, nil
}

// RevokeInviteLink ends a link nobody used yet. A used link is ErrInviteUsed; an unknown or foreign
// one is ErrNotFound. Revoking twice is not an error.
func (s *Store) RevokeInviteLink(ctx context.Context, tenant, id string) error {
	var used bool
	e := s.pool.QueryRow(ctx, `
		UPDATE v2_invite_links SET revoked_at = COALESCE(revoked_at, now())
		WHERE tenant_id = $1 AND id::text = $2 AND used_at IS NULL
		RETURNING false`, tenant, id).Scan(&used)
	if e == nil {
		return nil
	}
	if !errors.Is(e, pgx.ErrNoRows) {
		return fmt.Errorf("revoke invite link: %w", e)
	}
	e = s.pool.QueryRow(ctx, `SELECT used_at IS NOT NULL FROM v2_invite_links WHERE tenant_id = $1 AND id::text = $2`, tenant, id).Scan(&used)
	if errors.Is(e, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if e != nil {
		return fmt.Errorf("read invite link: %w", e)
	}
	return ErrInviteUsed
}

// inviteRow is a link as preview and accept read it.
type inviteRow struct {
	id, tenant, tenantName, role, state string
	squadID, squadName                  *string
}

func readInvite(ctx context.Context, q pgx.Tx, token string, lock bool) (inviteRow, error) {
	query := `
		SELECT l.id, l.tenant_id, t.name, l.role, sq.id, sq.name,
			CASE WHEN l.used_at IS NOT NULL THEN 'used'
				WHEN l.revoked_at IS NOT NULL THEN 'revoked'
				WHEN l.expires_at <= now() THEN 'expired'
				ELSE 'active' END
		FROM v2_invite_links l
		JOIN tenants t ON t.id = l.tenant_id AND t.status = 'active'
		LEFT JOIN v2_squads sq ON sq.id = l.squad_id AND sq.archived_at IS NULL
		WHERE l.token_hash = $1`
	if lock {
		query += ` FOR UPDATE OF l`
	}
	var r inviteRow
	e := q.QueryRow(ctx, query, hash(token)).Scan(&r.id, &r.tenant, &r.tenantName, &r.role, &r.squadID, &r.squadName, &r.state)
	if errors.Is(e, pgx.ErrNoRows) {
		return inviteRow{}, ErrNotFound
	}
	if e != nil {
		return inviteRow{}, fmt.Errorf("read invite: %w", e)
	}
	return r, nil
}

func isMember(ctx context.Context, tx pgx.Tx, tenant, user string) (bool, error) {
	var joined bool
	if e := tx.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM memberships WHERE tenant_id = $1 AND user_id = $2)`, tenant, user).Scan(&joined); e != nil {
		return false, fmt.Errorf("look up membership: %w", e)
	}
	return joined, nil
}

// PreviewInvite tells the signed-in user where a link leads and whether it is still usable. It
// changes nothing. An unknown token, or one into a disabled workspace, is ErrNotFound.
func (s *Store) PreviewInvite(ctx context.Context, token, user string) (InvitePreview, error) {
	var p InvitePreview
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, e := readInvite(ctx, tx, token, false)
		if e != nil {
			return e
		}
		joined, e := isMember(ctx, tx, r.tenant, user)
		if e != nil {
			return e
		}
		p = InvitePreview{State: r.state, Tenant: InviteTenant{ID: r.tenant, Name: r.tenantName}, Role: r.role, AlreadyMember: joined}
		if r.squadID != nil {
			p.Squad = &InviteSquad{ID: *r.squadID, Name: *r.squadName}
		}
		return nil
	})
	return p, e
}

// AcceptInvite joins the signed-in user to the link's workspace in one transaction: the link row is
// locked, checked unused, unexpired and unrevoked, marked used by this user, and the membership
// (with the link's role, and its squad when that squad is still live) is added. A user who is
// already a member just gets the workspace back and the link stays unused. It returns the
// workspace id; an unusable link is InviteUnavailableError.
func (s *Store) AcceptInvite(ctx context.Context, token, user string) (string, error) {
	var tenant string
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		r, e := readInvite(ctx, tx, token, true)
		if e != nil {
			return e
		}
		tenant = r.tenant
		joined, e := isMember(ctx, tx, r.tenant, user)
		if e != nil || joined {
			return e
		}
		if r.state != InviteActive {
			return InviteUnavailableError{Reason: r.state}
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_invite_links SET used_by = $2, used_at = now() WHERE id = $1`, r.id, user); e != nil {
			return fmt.Errorf("use invite: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO memberships (tenant_id, user_id, role) VALUES ($1, $2, $3)`, r.tenant, user, r.role); e != nil {
			return fmt.Errorf("join invited workspace: %w", e)
		}
		// A workspace the admin-invite command just created has no space yet; every workspace
		// needs the one projects and Agents hang off.
		if e := ensureDemoSpace(ctx, tx, r.tenant, user); e != nil {
			return e
		}
		if e := seedStarterSkills(ctx, tx, r.tenant, user); e != nil {
			return e
		}
		if r.squadID != nil {
			if e := joinInvitedSquad(ctx, tx, r.tenant, *r.squadID, user); e != nil {
				return e
			}
		}
		return nil
	})
	return tenant, e
}

// RemoveMember ends one person's membership of the workspace: the membership row goes and their
// live squad memberships in this workspace end, keeping history; the account stays and can still
// sign in. The last owner cannot be removed. Unknown members are ErrNotFound.
func (s *Store) RemoveMember(ctx context.Context, tenant, user string) error {
	return pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		// Lock the workspace's owner rows so two owners removing each other cannot both succeed.
		rows, e := tx.Query(ctx, `
			SELECT user_id::text, role FROM memberships WHERE tenant_id = $1 ORDER BY user_id FOR UPDATE`, tenant)
		if e != nil {
			return fmt.Errorf("lock memberships: %w", e)
		}
		owners, found := 0, false
		var role string
		for rows.Next() {
			var id, r string
			if e := rows.Scan(&id, &r); e != nil {
				rows.Close()
				return fmt.Errorf("lock memberships: %w", e)
			}
			if r == RoleOwner {
				owners++
			}
			if id == user {
				found, role = true, r
			}
		}
		if e := rows.Err(); e != nil {
			return fmt.Errorf("lock memberships: %w", e)
		}
		if !found {
			return ErrNotFound
		}
		if role == RoleOwner && owners == 1 {
			return ErrLastOwner
		}
		if _, e := tx.Exec(ctx, `DELETE FROM memberships WHERE tenant_id = $1 AND user_id = $2`, tenant, user); e != nil {
			return fmt.Errorf("remove member: %w", e)
		}
		ended, e := tx.Query(ctx, `
			UPDATE v2_squad_members m SET ended_at = now()
			FROM v2_squads sq
			WHERE sq.id = m.squad_id AND sq.tenant_id = $1 AND m.user_id = $2 AND m.ended_at IS NULL
			RETURNING m.squad_id::text`, tenant, user)
		if e != nil {
			return fmt.Errorf("end squad memberships: %w", e)
		}
		squads, e := pgx.CollectRows(ended, pgx.RowTo[string])
		if e != nil {
			return fmt.Errorf("end squad memberships: %w", e)
		}
		for _, sq := range squads {
			if e := bumpSquad(ctx, tx, sq); e != nil {
				return e
			}
		}
		return nil
	})
}
