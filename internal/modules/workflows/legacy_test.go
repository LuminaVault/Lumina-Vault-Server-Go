package workflows

import (
	"context"
	"encoding/json"
	"lumina/internal/skills"
	"lumina/internal/vaultpaths"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func writeLegacySkill(t *testing.T, paths vaultpaths.Paths, tenant uuid.UUID, name, schedule string) {
	t.Helper()
	path := filepath.Join(paths.TenantRoot(tenant), "skills", name, "SKILL.md")
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	meta := "metadata:\n  capability: low\n"
	if schedule != "" {
		meta += "  schedule: \"" + schedule + "\"\n"
	}
	require.NoError(t, os.WriteFile(path,
		[]byte("---\nname: "+name+"\ndescription: legacy "+name+"\n"+meta+"---\nSay hi.\n"), 0o644))
}

// LegacyJobWorkflowMigrator: a scheduled skill becomes a published "Job · <name>"
// workflow (schedule trigger -> skill -> output, the cron in the user's timezone)
// linked from skills_state; an unscheduled, bad-cron or already-linked skill is
// left alone; a second pass creates nothing; an existing legacy workflow is linked,
// not duplicated; a schedule_override wins over the manifest.
func TestLegacyMigratorCreatesAndLinksJobWorkflows(t *testing.T) {
	v := setup(t, nil)
	ctx := context.Background()
	paths := vaultpaths.Paths{Root: t.TempDir()}
	catalog := skills.NewVaultCatalog(paths)
	_, err := v.pool.Exec(ctx, `UPDATE users SET timezone = 'Europe/Lisbon' WHERE id = $1`, wfAnn)
	require.NoError(t, err)

	writeLegacySkill(t, paths, wfAnn, "morning", "0 7 * * *")
	writeLegacySkill(t, paths, wfAnn, "manual", "")
	writeLegacySkill(t, paths, wfAnn, "badcron", "not a cron")
	writeLegacySkill(t, paths, wfAnn, "overridden", "0 7 * * *")
	_, err = v.pool.Exec(ctx, `INSERT INTO skills_state (tenant_id, source, name, enabled, schedule_override)
		VALUES ($1, 'vault', 'overridden', false, '30 9 * * 1')`, wfAnn)
	require.NoError(t, err)
	writeLegacySkill(t, paths, wfAnn, "existing", "0 8 * * *")
	existing := uuid.New()
	_, err = v.pool.Exec(ctx, `INSERT INTO workflows (id, tenant_id, name, draft_definition, is_legacy_job, legacy_skill_name)
		VALUES ($1, $2, 'Job · existing', '{}', true, 'existing')`, existing, wfAnn)
	require.NoError(t, err)

	n, err := v.m.MigrateLegacyJobs(ctx, catalog)
	require.NoError(t, err)
	require.Equal(t, 2, n, "morning and overridden are created; existing is linked")

	linked := func(name string) *uuid.UUID {
		var id *uuid.UUID
		err := v.pool.QueryRow(ctx, `SELECT workflow_id FROM skills_state
			WHERE tenant_id = $1 AND source = 'vault' AND name = $2`, wfAnn, name).Scan(&id)
		if err != nil {
			return nil
		}
		return id
	}
	require.Nil(t, linked("manual"))
	require.Nil(t, linked("badcron"))
	require.Equal(t, existing, *linked("existing"))

	morning := linked("morning")
	require.NotNil(t, morning)
	var name, legacyName string
	var description *string
	var isLegacy, enabled bool
	var revision int
	var draft, published []byte
	require.NoError(t, v.pool.QueryRow(ctx, `
		SELECT w.name, w.description_text, w.is_legacy_job, w.legacy_skill_name, w.enabled, w.draft_revision,
			w.draft_definition, v.definition
		FROM workflows w JOIN workflow_versions v ON v.id = w.published_version_id AND v.version = 1
		WHERE w.id = $1`, *morning).
		Scan(&name, &description, &isLegacy, &legacyName, &enabled, &revision, &draft, &published))
	require.Equal(t, "Job · morning", name)
	require.Equal(t, "legacy morning", *description)
	require.True(t, isLegacy)
	require.Equal(t, "morning", legacyName)
	require.True(t, enabled)
	require.Equal(t, 1, revision)
	require.JSONEq(t, string(draft), string(published))

	def, err := decodeDefinition(published)
	require.NoError(t, err)
	require.Equal(t, Schedule, def.Trigger)
	require.Equal(t, StringMap{"cron": "0 7 * * *", "timezone": "Europe/Lisbon"}, def.TriggerConfiguration)
	require.Len(t, def.Nodes, 3)
	trigger, skill, output := def.Nodes[0], def.Nodes[1], def.Nodes[2]
	require.Equal(t, Trigger, trigger.Kind)
	require.Equal(t, "Schedule", trigger.Name)
	require.Equal(t, Skill, skill.Kind)
	require.Equal(t, StringMap{"skillName": "morning"}, skill.Configuration)
	require.EqualValues(t, 260, skill.X)
	require.Equal(t, Output, output.Kind)
	require.Equal(t, StringMap{"value": "{{nodes." + upper(skill.Id.UUID) + ".text}}"}, output.Configuration)
	require.Len(t, def.Edges, 2)
	require.Equal(t, trigger.Id, def.Edges[0].SourceNodeID)
	require.Equal(t, skill.Id, def.Edges[0].TargetNodeID)
	require.Equal(t, skill.Id, def.Edges[1].SourceNodeID)
	require.Equal(t, output.Id, def.Edges[1].TargetNodeID)

	var raw map[string]any
	require.NoError(t, json.Unmarshal(published, &raw))
	require.EqualValues(t, 1, raw["schemaVersion"])

	over := linked("overridden")
	require.NotNil(t, over)
	var overDef []byte
	require.NoError(t, v.pool.QueryRow(ctx, `SELECT draft_definition FROM workflows WHERE id = $1`, *over).Scan(&overDef))
	od, err := decodeDefinition(overDef)
	require.NoError(t, err)
	require.Equal(t, "30 9 * * 1", od.TriggerConfiguration["cron"])

	n, err = v.m.MigrateLegacyJobs(ctx, catalog)
	require.NoError(t, err)
	require.Zero(t, n)
	var total int
	require.NoError(t, v.pool.QueryRow(ctx,
		`SELECT count(*) FROM workflows WHERE tenant_id = $1 AND is_legacy_job`, wfAnn).Scan(&total))
	require.Equal(t, 3, total)
}
