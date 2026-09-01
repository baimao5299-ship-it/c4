// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestClientIPSourceXForwardedForContract locks the generated enum and JSON
// field shape used by the admin log API. This catches generated-code drift when
// the OpenAPI source gains another trusted proxy header.
func TestClientIPSourceXForwardedForContract(t *testing.T) {
	source := UsageLogClientIPSourceXForwardedFor
	row := UsageLog{ClientIPSource: &source}
	payload, err := json.Marshal(row)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"ClientIPSource":"x_forwarded_for"`)

	errSource := ErrLogClientIPSourceXForwardedFor
	errRow := ErrLog{ClientIPSource: &errSource}
	payload, err = json.Marshal(errRow)
	require.NoError(t, err)
	require.Contains(t, string(payload), `"ClientIPSource":"x_forwarded_for"`)
}
