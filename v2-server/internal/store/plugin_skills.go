package store

import (
	"context"
	"sort"

	"github.com/wanglongan587/cloud/v2-server/internal/execution"
)

// UpsertPluginSkills records the skill names a skill package materializes at runtime, replacing
// any names recorded for that package before. pluginID is the package's full "namespace/identifier".
func (s *Store) UpsertPluginSkills(ctx context.Context, pluginID string, names []string) error {
	if pluginID == "" {
		return execution.ErrInvalidRequest
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM v2_plugin_skills WHERE plugin_id=$1`, pluginID); err != nil {
		return err
	}
	for _, name := range names {
		if name == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO v2_plugin_skills(plugin_id, skill_name) VALUES($1,$2) ON CONFLICT DO NOTHING`, pluginID, name); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// InstalledPluginSkillNames maps each installed skill package in the space ("namespace/identifier")
// to the skill names it materializes at runtime, taken from what was recorded at install time.
func (s *Store) InstalledPluginSkillNames(ctx context.Context, tenant, space string) (map[string][]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT DISTINCT a.source_namespace||'/'||a.identifier, k.skill_name
		FROM v2_space_agents a
		JOIN v2_plugin_skills k ON k.plugin_id = a.source_namespace||'/'||a.identifier
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
