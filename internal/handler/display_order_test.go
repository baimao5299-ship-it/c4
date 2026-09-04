// SPDX-License-Identifier: AGPL-3.0-or-later

package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/service"
)

func TestGroupCategoryRoundTripAndReorderEndpoint(t *testing.T) {
	store := newFakeStore()
	svc := service.New(store, fakeSched{}, service.NopInvalidator{}, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	router := chi.NewRouter()
	router.Mount("/", h.Router())
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	ids := make([]int64, 0, 3)
	for _, name := range []string{"first", "second", "third"} {
		rec := do(http.MethodPost, "/api/admin/groups", `{"name":"`+name+`","category":"GPT"}`)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var group Group
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &group))
		require.NotNil(t, group.Category)
		require.Equal(t, "GPT", *group.Category)
		ids = append(ids, *group.ID)
	}

	rec := do(http.MethodPost, "/api/admin/groups/reorder", `{"ids":[`+itoa(ids[2])+`,`+itoa(ids[0])+`,`+itoa(ids[1])+`]}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var reordered ReorderResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &reordered))
	require.Equal(t, 3, reordered.Reordered)
	require.Equal(t, int64(0), *store.groups[ids[2]].DisplayOrder)
	require.Equal(t, int64(1), *store.groups[ids[0]].DisplayOrder)
	require.Equal(t, int64(2), *store.groups[ids[1]].DisplayOrder)

	rec = do(http.MethodPut, "/api/admin/groups/"+itoa(ids[0]), `{"name":"first","category":"Claude"}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = do(http.MethodGet, "/api/admin/groups/"+itoa(ids[0]), "")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var updated Group
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &updated))
	require.NotNil(t, updated.Category)
	require.Equal(t, "Claude", *updated.Category)
	require.Equal(t, int64(1), *store.groups[ids[0]].DisplayOrder, "ordinary edits must preserve display order")

	rec = do(http.MethodPut, "/api/admin/groups/"+itoa(ids[0]), `{"name":"first","category":"`+strings.Repeat("x", 51)+`"}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}
