// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/require"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/handler/httpface"
	"github.com/is7qin/c3api/internal/repository"
	"github.com/is7qin/c3api/internal/service"
)

// accountBindingStore mirrors the production repository's UpstreamStore while
// filling the one test-fixture gap: fakeStore.UpdateAccountsBatch predates the
// upstream_id field and intentionally does not persist it. Keeping the override
// local makes this regression suite independent of other handler tests.
type accountBindingStore struct{ *upstreamTestStore }

func (s *accountBindingStore) UpdateAccountsBatch(ctx context.Context, ids []int64, p repository.AccountPatch) error {
	if err := s.upstreamTestStore.UpdateAccountsBatch(ctx, ids, p); err != nil {
		return err
	}
	if p.UpstreamID == nil {
		return nil
	}
	s.fakeStore.mu.Lock()
	defer s.fakeStore.mu.Unlock()
	for _, id := range ids {
		a := s.fakeStore.accs[id]
		if *p.UpstreamID <= 0 {
			a.UpstreamID = nil
			continue
		}
		bound := *p.UpstreamID
		a.UpstreamID = &bound
	}
	return nil
}

type bindingInvalidator struct {
	mu            sync.Mutex
	accountCalls  []bindingAccountCall
	templateCalls int
}

type bindingAccountCall struct {
	groups     []int64
	keyChanged bool
}

func (i *bindingInvalidator) Users()       {}
func (i *bindingInvalidator) Multipliers() {}
func (i *bindingInvalidator) Templates()   { i.mu.Lock(); i.templateCalls++; i.mu.Unlock() }
func (i *bindingInvalidator) Accounts(groups []int64, keyChanged bool) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.accountCalls = append(i.accountCalls, bindingAccountCall{groups: append([]int64(nil), groups...), keyChanged: keyChanged})
}

func newAccountBindingRouter(t *testing.T, store *accountBindingStore, inv *bindingInvalidator) func(string, string, string) *httptest.ResponseRecorder {
	t.Helper()
	svc := service.New(store, fakeSched{}, inv, nil, nil, &fakeKeys{}, nil)
	h := New(svc)
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer admin-tok" {
				httpface.WriteErr(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, req)
		})
	})
	r.Mount("/", h.Router())
	return func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer admin-tok")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}
}

func TestAccountUpstreamBindingThreeStateContract(t *testing.T) {
	store := &accountBindingStore{upstreamTestStore: newUpstreamTestStore()}
	inv := &bindingInvalidator{}
	do := newAccountBindingRouter(t, store, inv)

	tpl, err := store.CreateTemplate(context.Background(), &domain.Template{
		Name: "binding-template", BaseURL: "https://api.example.test",
		SupportedFormats: []domain.RequestFormat{domain.FormatOpenAIChat},
	})
	require.NoError(t, err)
	u1, err := store.CreateUpstream(context.Background(), &domain.Upstream{Name: "relay-one", BaseURL: "https://relay-one.example.test", Enabled: true, MultiplierBP: 10000})
	require.NoError(t, err)
	u2, err := store.CreateUpstream(context.Background(), &domain.Upstream{Name: "relay-two", BaseURL: "https://relay-two.example.test", Enabled: true, MultiplierBP: 10000})
	require.NoError(t, err)

	// Create: an explicit id binds the account and is visible in the response.
	rec := do(http.MethodPost, "/api/admin/accounts", `{"name":"bound","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-account","upstream_id":`+strconv.FormatInt(u1.ID, 10)+`}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var created Account
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &created))
	require.NotNil(t, created.ID)
	require.NotNil(t, created.UpstreamID)
	require.Equal(t, u1.ID, *created.UpstreamID)
	id := strconv.FormatInt(*created.ID, 10)

	// PUT omission retains the existing binding for older clients/forms.
	rec = do(http.MethodPut, "/api/admin/accounts/"+id,
		`{"name":"bound-renamed","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-account","status":"active","weight":100,"max_concurrency":8}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var omitted Account
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &omitted))
	require.NotNil(t, omitted.UpstreamID)
	require.Equal(t, u1.ID, *omitted.UpstreamID, "omitted upstream_id must retain binding")

	// PUT null is a deliberate clear, unlike omission.
	rec = do(http.MethodPut, "/api/admin/accounts/"+id,
		`{"name":"bound-cleared","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-account","status":"active","weight":100,"max_concurrency":8,"upstream_id":null}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cleared Account
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &cleared))
	require.Nil(t, cleared.UpstreamID, "explicit null must clear binding")

	// PUT with the second id binds it again.
	rec = do(http.MethodPut, "/api/admin/accounts/"+id,
		`{"name":"bound-two","template_id":`+strconv.FormatInt(tpl.ID, 10)+`,"upstream_key":"sk-account","status":"active","weight":100,"max_concurrency":8,"upstream_id":`+strconv.FormatInt(u2.ID, 10)+`}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var rebound Account
	require.NoError(t, jsonUnmarshal(rec.Body.Bytes(), &rebound))
	require.NotNil(t, rebound.UpstreamID)
	require.Equal(t, u2.ID, *rebound.UpstreamID)

	// Batch positive id binds; batch null is an omitted/no-op value, while 0
	// is the documented explicit clear sentinel.
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+id+`],"fields":{"upstream_id":`+strconv.FormatInt(u1.ID, 10)+`}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bound, err := store.GetAccount(context.Background(), *created.ID)
	require.NoError(t, err)
	require.NotNil(t, bound.UpstreamID)
	require.Equal(t, u1.ID, *bound.UpstreamID)

	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+id+`],"fields":{"name":"null-noop","upstream_id":null}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bound, err = store.GetAccount(context.Background(), *created.ID)
	require.NoError(t, err)
	require.NotNil(t, bound.UpstreamID)
	require.Equal(t, u1.ID, *bound.UpstreamID, "batch null must leave binding unchanged")

	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+id+`],"fields":{"upstream_id":0}}`)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	bound, err = store.GetAccount(context.Background(), *created.ID)
	require.NoError(t, err)
	require.Nil(t, bound.UpstreamID, "batch zero must clear binding")

	// Positive references must point to a live upstream; invalid ids are rejected
	// before the account write and leave the previous clear state intact.
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+id+`],"fields":{"upstream_id":999999}}`)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	rec = do(http.MethodPost, "/api/admin/accounts/batch-update",
		`{"ids":[`+id+`],"fields":{"upstream_id":-1}}`)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	bound, err = store.GetAccount(context.Background(), *created.ID)
	require.NoError(t, err)
	require.Nil(t, bound.UpstreamID, "rejected bindings must not mutate account")

	inv.mu.Lock()
	defer inv.mu.Unlock()
	var changed int
	for _, call := range inv.accountCalls {
		if call.keyChanged {
			changed++
		}
	}
	require.GreaterOrEqual(t, changed, 3, "bind, clear and rebind must invalidate clients")
}

func TestUpstreamChangeInvalidatesClientsForBoundAccounts(t *testing.T) {
	store := &accountBindingStore{upstreamTestStore: newUpstreamTestStore()}
	inv := &bindingInvalidator{}
	svc := service.New(store, fakeSched{}, inv, nil, nil, &fakeKeys{}, nil)

	u, err := store.CreateUpstream(context.Background(), &domain.Upstream{
		Name: "relay", BaseURL: "https://relay-old.example.test", Enabled: true, MultiplierBP: 10000,
	})
	require.NoError(t, err)
	updated := *u
	updated.BaseURL = "https://relay-new.example.test"
	updated.Name = "relay-renamed"
	_, err = svc.UpdateUpstream(context.Background(), &updated)
	require.NoError(t, err)

	inv.mu.Lock()
	defer inv.mu.Unlock()
	require.Equal(t, 1, inv.templateCalls, "upstream endpoint changes must refresh client/template snapshots")
}
