// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository_test

import (
	"context"
	"testing"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/repository"
)

// TestPGUpstreamModelFormatsAdditiveMigration starts from the exact current
// upstream table with the new column removed, preserving an existing row. It
// verifies that production AutoMigrate adds the capability column without
// losing data and gives legacy rows the intentional unknown-protocol value.
func TestPGUpstreamModelFormatsAdditiveMigration(t *testing.T) {
	ctx := context.Background()
	repos := newPGRepos(t)
	legacy, err := repos.Upstreams.CreateUpstream(ctx, &domain.Upstream{
		Name:         "legacy-upstream",
		BaseURL:      "https://legacy.example.com",
		MultiplierBP: 800,
		Enabled:      true,
	})
	require.NoError(t, err)

	db := pgTestDB(t)
	_, err = db.ExecContext(ctx, `ALTER TABLE upstreams DROP COLUMN model_formats`)
	require.NoError(t, err)

	upgraded, err := repository.New(entsql.OpenDB(dialect.Postgres, db), true)
	require.NoError(t, err)
	got, err := upgraded.Upstreams.GetUpstream(ctx, legacy.ID)
	require.NoError(t, err)
	require.Equal(t, legacy.Name, got.Name)
	require.Equal(t, legacy.BaseURL, got.BaseURL)
	require.Equal(t, legacy.MultiplierBP, got.MultiplierBP)
	require.True(t, got.Enabled)
	require.NotNil(t, got.ModelFormats)
	require.Empty(t, got.ModelFormats)

	var nullable, defaultValue, storedValue string
	err = db.QueryRowContext(ctx, `
		SELECT c.is_nullable, COALESCE(c.column_default, ''), u.model_formats::text
		FROM information_schema.columns c
		JOIN upstreams u ON u.id = $1
		WHERE c.table_schema = current_schema()
		  AND c.table_name = 'upstreams'
		  AND c.column_name = 'model_formats'`, legacy.ID).
		Scan(&nullable, &defaultValue, &storedValue)
	require.NoError(t, err)
	require.Equal(t, "NO", nullable)
	require.Contains(t, defaultValue, "{}")
	require.JSONEq(t, `{}`, storedValue)
}
