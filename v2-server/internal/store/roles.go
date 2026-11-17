package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/wanglongan587/cloud/v2-server/internal/roledoc"
)

// Role is one tenant-owned Agent role together with its Markdown definition.
type Role struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Content     string    `json:"content"`
	Version     int64     `json:"version"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// RoleInput is a validated create or replacement payload.
type RoleInput struct {
	Name        string
	Description string
	Content     string
}

// ErrRoleNameConflict reports a live role with the same case-insensitive name.
var ErrRoleNameConflict = errors.New("role name conflict")

// ErrRoleNotFound reports a role outside the caller's tenant.
var ErrRoleNotFound = errors.New("role not found")

// ListRoles returns the tenant's live roles, newest update first. The catalog is shared by every
// member: a workflow edited by one person must be runnable by another.
func (s *Store) ListRoles(ctx context.Context, tenant string) ([]Role, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id::text, name, description, content, version, created_at, updated_at
		FROM v2_roles
		WHERE tenant_id = $1 AND deleted_at IS NULL
		ORDER BY updated_at DESC, id`, tenant)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	defer rows.Close()
	items := make([]Role, 0)
	for rows.Next() {
		var item Role
		if err := rows.Scan(&item.ID, &item.Name, &item.Description, &item.Content, &item.Version, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan role: %w", err)
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate roles: %w", err)
	}
	return items, nil
}

// GetRole returns one role in the tenant.
func (s *Store) GetRole(ctx context.Context, tenant, id string) (Role, error) {
	item, err := queryRole(ctx, s.pool, tenant, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Role{}, ErrRoleNotFound
	}
	if err != nil {
		return Role{}, fmt.Errorf("get role: %w", err)
	}
	return item, nil
}

// CreateRole persists one new tenant role.
func (s *Store) CreateRole(ctx context.Context, tenant, caller string, in RoleInput) (Role, error) {
	doc, err := roledoc.Validate(roledoc.Document{Name: in.Name, Description: in.Description, Content: in.Content})
	if err != nil {
		return Role{}, err
	}
	var item Role
	err = s.pool.QueryRow(ctx, `
		INSERT INTO v2_roles (tenant_id, name, description, content, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id::text, name, description, content, version, created_at, updated_at`,
		tenant, doc.Name, doc.Description, doc.Content, caller).
		Scan(&item.ID, &item.Name, &item.Description, &item.Content, &item.Version, &item.CreatedAt, &item.UpdatedAt)
	if uniqueViolation(err) {
		return Role{}, ErrRoleNameConflict
	}
	if err != nil {
		return Role{}, fmt.Errorf("create role: %w", err)
	}
	return item, nil
}

// UpdateRole replaces one role while preserving its identity.
func (s *Store) UpdateRole(ctx context.Context, tenant, id string, in RoleInput) (Role, error) {
	doc, err := roledoc.Validate(roledoc.Document{Name: in.Name, Description: in.Description, Content: in.Content})
	if err != nil {
		return Role{}, err
	}
	var item Role
	err = s.pool.QueryRow(ctx, `
		UPDATE v2_roles
		SET name = $1, description = $2, content = $3, version = version + 1, updated_at = now()
		WHERE id = $4 AND tenant_id = $5 AND deleted_at IS NULL
		RETURNING id::text, name, description, content, version, created_at, updated_at`,
		doc.Name, doc.Description, doc.Content, id, tenant).
		Scan(&item.ID, &item.Name, &item.Description, &item.Content, &item.Version, &item.CreatedAt, &item.UpdatedAt)
	if uniqueViolation(err) {
		return Role{}, ErrRoleNameConflict
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return Role{}, ErrRoleNotFound
	}
	if err != nil {
		return Role{}, fmt.Errorf("update role: %w", err)
	}
	return item, nil
}

// DeleteRole soft-deletes one role and frees its name for a later create or import. Existing
// workflow graphs keep the stored name; a run refuses to start until a role with that name exists
// again, which is also how desktop treats a deleted Agent definition.
func (s *Store) DeleteRole(ctx context.Context, tenant, id string) error {
	tag, err := s.pool.Exec(ctx, `
		UPDATE v2_roles
		SET deleted_at = now(), updated_at = now(), version = version + 1
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, tenant)
	if err != nil {
		return fmt.Errorf("delete role: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrRoleNotFound
	}
	return nil
}

// ImportRole parses one role Markdown file and creates or explicitly overwrites the tenant's entry.
func (s *Store) ImportRole(ctx context.Context, tenant, caller string, raw []byte, overwrite bool) (Role, error) {
	doc, err := roledoc.Parse(raw)
	if err != nil {
		return Role{}, err
	}
	existing, err := queryRoleByName(ctx, s.pool, tenant, doc.Name)
	switch {
	case err == nil:
		if !overwrite {
			return Role{}, ErrRoleNameConflict
		}
		return s.UpdateRole(ctx, tenant, existing.ID, RoleInput{
			Name: doc.Name, Description: doc.Description, Content: doc.Content,
		})
	case errors.Is(err, pgx.ErrNoRows):
		return s.CreateRole(ctx, tenant, caller, RoleInput{
			Name: doc.Name, Description: doc.Description, Content: doc.Content,
		})
	default:
		return Role{}, fmt.Errorf("look up imported role: %w", err)
	}
}

// ResolveWorkflowRole reads the role a graph node references, for the run it belongs to. A
// reference is resolved by name first, because that is what the editor stores; an id is the
// fallback for graphs written before roles were selected by name, as on desktop.
func (s *Store) ResolveWorkflowRole(ctx context.Context, tenant, reference string) (Role, error) {
	reference = strings.TrimSpace(reference)
	if reference == "" {
		return Role{}, ErrRoleNotFound
	}
	item, err := queryRoleByName(ctx, s.pool, tenant, reference)
	if err == nil {
		return item, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return Role{}, fmt.Errorf("resolve role %q: %w", reference, err)
	}
	if !accessUUID.MatchString(reference) {
		return Role{}, ErrRoleNotFound
	}
	item, err = queryRole(ctx, s.pool, tenant, reference)
	if errors.Is(err, pgx.ErrNoRows) {
		return Role{}, ErrRoleNotFound
	}
	if err != nil {
		return Role{}, fmt.Errorf("resolve role %q: %w", reference, err)
	}
	return item, nil
}

func queryRole(ctx context.Context, q dbTX, tenant, id string) (Role, error) {
	var item Role
	err := q.QueryRow(ctx, `
		SELECT id::text, name, description, content, version, created_at, updated_at
		FROM v2_roles
		WHERE id = $1 AND tenant_id = $2 AND deleted_at IS NULL`, id, tenant).
		Scan(&item.ID, &item.Name, &item.Description, &item.Content, &item.Version, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}

func queryRoleByName(ctx context.Context, q dbTX, tenant, name string) (Role, error) {
	var item Role
	err := q.QueryRow(ctx, `
		SELECT id::text, name, description, content, version, created_at, updated_at
		FROM v2_roles
		WHERE tenant_id = $1 AND lower(name) = lower($2) AND deleted_at IS NULL`, tenant, name).
		Scan(&item.ID, &item.Name, &item.Description, &item.Content, &item.Version, &item.CreatedAt, &item.UpdatedAt)
	return item, err
}
