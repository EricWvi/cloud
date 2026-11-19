package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// Database profiles, see ClaimProfile.
const (
	ProfileDevelopment = "development"
	ProfileDeployment  = "deployment"
)

// ErrProfileMismatch reports starting a database as the other profile than it was created with.
var ErrProfileMismatch = errors.New("database profile mismatch")

// ClaimProfile records on first start whether this database is a development or a deployment
// database and returns ErrProfileMismatch when a later start asks for the other one. A database
// that already holds workspaces but no record predates the record; only development starts could
// create one then, so it is recorded as development.
func (s *Store) ClaimProfile(ctx context.Context, want string) error {
	if want != ProfileDevelopment && want != ProfileDeployment {
		return fmt.Errorf("unknown database profile %q", want)
	}
	var stored string
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := tx.Exec(ctx, `LOCK TABLE v2_instance IN EXCLUSIVE MODE`); e != nil {
			return fmt.Errorf("lock database profile: %w", e)
		}
		e := tx.QueryRow(ctx, `SELECT profile FROM v2_instance`).Scan(&stored)
		if e == nil {
			return nil
		}
		if !errors.Is(e, pgx.ErrNoRows) {
			return fmt.Errorf("read database profile: %w", e)
		}
		var existing bool
		if e := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenants)`).Scan(&existing); e != nil {
			return fmt.Errorf("inspect database: %w", e)
		}
		stored = want
		if existing {
			stored = ProfileDevelopment
		}
		if _, e := tx.Exec(ctx, `INSERT INTO v2_instance (profile) VALUES ($1)`, stored); e != nil {
			return fmt.Errorf("record database profile: %w", e)
		}
		return nil
	})
	if e != nil {
		return e
	}
	if stored != want {
		return fmt.Errorf("%w: this database is a %s database and cannot be started as a %s one; use a new database to change", ErrProfileMismatch, stored, want)
	}
	return nil
}
