// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"
)

func TestPutAccountsOmittedSchedulerFieldsRetainCurrentValues(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"tpl","base_url":"https://api.example.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))

	// The API schema defaults an omitted weight to 100. It must not become zero
	// (which removes the account from weighted scheduler selection).
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc-default","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var defaultAccount Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &defaultAccount))
	require.Equal(t, 100, *defaultAccount.Weight)

	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk","status":"disabled","weight":42,"max_concurrency":3}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// Only required identity fields are sent. Omitted status/weight/max
	// concurrency must retain the existing scheduler configuration.
	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(*created.ID), `{"name":"acc-renamed","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var updated Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.Equal(t, AccountStatusDisabled, *updated.Status)
	require.Equal(t, 42, *updated.Weight)
	require.Equal(t, 3, *updated.MaxConcurrency)

	// Unknown enum values are rejected instead of being persisted as an
	// unroutable account state.
	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(*created.ID), `{"name":"acc","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk","status":"unknown"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

func TestPutAccountsOmittedBaseURLRetainsOverride(t *testing.T) {
	h := newTestHandler(t)
	r := chi.NewRouter()
	r.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	rec := do(http.MethodPost, "/api/admin/templates", `{"name":"tpl-base","base_url":"https://template.example.com","supported_formats":["openai-chat"]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var tpl Template
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &tpl))
	rec = do(http.MethodPost, "/api/admin/accounts", `{"name":"acc-base","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk","base_url":"https://account.example.com"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// A narrow PUT that omits base_url must not silently clear the account
	// override. Explicit null remains the clear operation.
	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(*created.ID), `{"name":"acc-base-renamed","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var retained Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &retained))
	require.NotNil(t, retained.BaseURL)
	require.Equal(t, "https://account.example.com", *retained.BaseURL)

	rec = do(http.MethodPut, "/api/admin/accounts/"+itoa(*created.ID), `{"name":"acc-base-cleared","template_id":`+itoa(tpl.ID)+`,"upstream_key":"sk","base_url":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cleared Account
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cleared))
	require.Nil(t, cleared.BaseURL)
}
