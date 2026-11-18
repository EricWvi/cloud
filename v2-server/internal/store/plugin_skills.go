package store

import (
	"context"
	"sort"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// UpsertPluginSkills records the skill names a skill package materializes at runtime. digest is the
// package's verified SHA-256: the names belong to those exact bytes, so recording them never
// replaces what another space, source or version resolves to, and a repeated record is a no-op.
func (s *Store) UpsertPluginSkills(ctx context.Context, digest string, names []string) error {
	if digest == "" {
		return execution.ErrInvalidRequest
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO v2_skill_package_names(digest, skill_name) VALUES($1,$2) ON CONFLICT DO NOTHING`, digest, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// InstalledPluginSkillNames maps each installed skill package in the space ("namespace/identifier")
// to the skill names it materializes at runtime, resolved through the package digest the selection
// froze at install time. A selection installed before names were recorded has no entry.
func (s *Store) InstalledPluginSkillNames(ctx context.Context, tenant, space string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.source_namespace||'/'||a.identifier, k.skill_name
		FROM v2_space_agents a
		JOIN v2_skill_plans p ON p.selection_id = a.id
		JOIN v2_skill_package_names k ON k.digest = p.digest
		WHERE a.tenant_id=$1::uuid AND a.space_id=$2::uuid AND a.kind='skill' AND a.desired_state='installed'
		ORDER BY 1, 2`, tenant, space)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for id := range out {
		sort.Strings(out[id])
	}
	return out, nil
}
