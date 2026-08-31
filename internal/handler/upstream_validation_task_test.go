// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUpstreamValidationTaskRoutesReturnStableStatus(t *testing.T) {
	h := New(nil)
	server := h.Router()

	startReq := httptest.NewRequest(http.MethodPost, "/api/admin/upstreams/validate-all/start", nil)
	startRec := httptest.NewRecorder()
	server.ServeHTTP(startRec, startReq)
	require.Equal(t, http.StatusAccepted, startRec.Code)
	require.Equal(t, "no-store", startRec.Header().Get("Cache-Control"))
	var started struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
	}
	require.NoError(t, json.Unmarshal(startRec.Body.Bytes(), &started))
	require.NotEmpty(t, started.TaskID)
	require.Equal(t, "queued", started.Status)

	var status struct {
		Status string `json:"status"`
		Error  string `json:"error"`
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest(http.MethodGet, "/api/admin/upstreams/validate-all/tasks/"+started.TaskID, nil)
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, "no-store", rec.Header().Get("Cache-Control"))
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &status))
		if status.Status == "failed" {
			require.Contains(t, status.Error, "upstream")
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("validation task did not settle")
}

func TestUpstreamValidationTaskUnknownIDIsNotFound(t *testing.T) {
	h := New(nil)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/admin/upstreams/validate-all/tasks/missing", nil)
	h.Router().ServeHTTP(rec, req)
	require.Equal(t, http.StatusNotFound, rec.Code)
}
