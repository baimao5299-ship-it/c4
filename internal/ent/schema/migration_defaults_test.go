// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package schema

import (
	"testing"

	entschema "entgo.io/ent/dialect/sql/schema"
	"github.com/stretchr/testify/require"

	entmigrate "github.com/is7qin/c3api/internal/ent/migrate"
)

func TestNewJSONColumnsBackfillExistingRows(t *testing.T) {
	requireColumnDefault(t, entmigrate.GroupsColumns, "allowed_models", "[]")
	requireColumnDefault(t, entmigrate.UpstreamsColumns, "models", "[]")
}

func requireColumnDefault(t *testing.T, columns []*entschema.Column, name string, want any) {
	t.Helper()
	for _, column := range columns {
		if column.Name == name {
			require.Equal(t, want, column.Default)
			return
		}
	}
	require.Failf(t, "missing column", "column %q was not generated", name)
}
