package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ErrAccountDisabled reports a sign-in by an account that was disabled or deleted.
var ErrAccountDisabled = errors.New("account disabled")

// SignInExternal finds or creates the account of an identity an external login method verified,
// keyed only by (source, subject), and refreshes its display name. It never touches memberships:
// signing in does not join a workspace (A6 D3, D4). A disabled or deleted account is
// ErrAccountDisabled and is not changed.
func (s *Store) SignInExternal(ctx context.Context, source, subject, displayName string) (User, error) {
	if source == "" || subject == "" {
		return User{}, errors.New("external identity needs a source and a subject")
	}
	if displayName == "" {
		displayName = subject
	}
	var u User
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `
			INSERT INTO users (source, subject, display_name) VALUES ($1, $2, $3)
			ON CONFLICT (source, subject) DO UPDATE SET display_name = excluded.display_name
			WHERE users.status = 'active' AND users.deleted_at IS NULL`, source, subject, displayName); e != nil {
			return fmt.Errorf("upsert external user: %w", e)
		}
		return tx.QueryRow(ctx, `
			SELECT id, display_name, status, version, created_at, deleted_at
			FROM users WHERE source = $1 AND subject = $2`, source, subject).
			Scan(&u.ID, &u.DisplayName, &u.Status, &u.Version, &u.CreatedAt, &u.DeletedAt)
	})
	if e != nil {
		return User{}, fmt.Errorf("sign in external user: %w", e)
	}
	if u.Status != "active" || u.DeletedAt != nil {
		return User{}, ErrAccountDisabled
	}
	return u, nil
}
