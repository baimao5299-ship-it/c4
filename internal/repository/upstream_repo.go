// SPDX-License-Identifier: AGPL-3.0-or-later
// Dual-licensed: AGPL-3.0-or-later (open source) or commercial license (closed-source
// deployment exemption); see LICENSE and LICENSE.commercial. Copyright (c) 2026 is7Qin.

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqlgraph"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/groupupstream"
	"github.com/is7qin/c3api/internal/ent/upstream"
)

// UpstreamRepo persists the operator-facing relay inventory. It deliberately
// does not participate in request routing; accounts/templates remain the hot
// path source of truth until an operator explicitly binds them there.
type UpstreamRepo struct {
	client *ent.Client
	driver dialect.Driver
	pool   *pgxpool.Pool
}

// ErrUpstreamValidationLockUnavailable marks the intentionally supported
// lightweight repository path that has no pgx pool. The service keeps its
// process-local mutex in that case; production composition uses NewWithPG and
// therefore takes the cross-instance lock below.
var ErrUpstreamValidationLockUnavailable = errors.New("upstream validation advisory lock unavailable")

// upstreamValidationLockKey is shared by every C4 instance. The session-level
// lock is held on a dedicated pool connection for the complete validation
// operation, including network probes, and therefore cannot be lost when the
// ordinary query connection is returned to the pool.
const upstreamValidationLockKey int64 = 0x55707664 // "UpVd"

// upstreamValidationSnapshotLimit is one larger than the service's accepted
// inventory size. ListAllUpstreams uses it as a database-side sentinel so an
// accidentally huge inventory cannot be fully materialized before the service
// rejects the validation request.
const upstreamValidationSnapshotLimit = 5001

// AcquireUpstreamValidationLock serializes paid model probes across instances.
// A nil pool means this repository was built for a lightweight/local test path;
// returning an explicit error avoids silently claiming cross-instance safety in
// production when the dedicated PostgreSQL pool was not wired.
func (r *UpstreamRepo) AcquireUpstreamValidationLock(ctx context.Context) (release func(), ok bool, err error) {
	if r == nil || r.pool == nil {
		return nil, false, fmt.Errorf("%w: repository.NewWithPG did not provide a pool", ErrUpstreamValidationLockUnavailable)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	conn, err := r.pool.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	var acquired bool
	if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, upstreamValidationLockKey).Scan(&acquired); err != nil {
		conn.Release()
		return nil, false, err
	}
	if !acquired {
		conn.Release()
		return nil, false, nil
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, upstreamValidationLockKey)
			conn.Release()
		})
	}, true, nil
}

func (r *UpstreamRepo) CreateUpstream(ctx context.Context, u *domain.Upstream) (*domain.Upstream, error) {
	b := r.client.Upstream.Create().
		SetName(u.Name).
		SetBaseURL(u.BaseURL).
		SetMultiplierBp(u.MultiplierBP).
		SetEnabled(u.Enabled).
		SetBalanceEndpoint(u.BalanceEndpoint).
		SetBalanceMethod(u.BalanceMethod).
		SetBalanceAuth(u.BalanceAuth).
		SetBalancePath(u.BalancePath).
		SetBalanceCurrencyPath(u.BalanceCurrencyPath).
		SetBalanceStatus(upstream.BalanceStatus(normalizeBalanceStatus(u.BalanceStatus)))
	if u.UpstreamKey != nil {
		b.SetUpstreamKey(*u.UpstreamKey)
	}
	if u.Note != nil && strings.TrimSpace(*u.Note) != "" {
		b.SetNote(strings.TrimSpace(*u.Note))
	}
	if u.BalanceAmount != nil {
		b.SetBalanceAmount(*u.BalanceAmount)
	}
	if u.BalanceCurrency != nil {
		b.SetBalanceCurrency(*u.BalanceCurrency)
	}
	if u.BalanceCheckedAt != nil {
		b.SetBalanceCheckedAt(*u.BalanceCheckedAt)
	}
	if u.Models != nil {
		b.SetModels(append([]string{}, u.Models...))
	}
	if u.ModelsCheckedAt != nil {
		b.SetModelsCheckedAt(*u.ModelsCheckedAt)
	}
	if u.ModelsError != nil && strings.TrimSpace(*u.ModelsError) != "" {
		b.SetModelsError(domain.TruncateErrMsg(strings.TrimSpace(*u.ModelsError)))
	}
	row, err := b.Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, u.Name)
		}
		return nil, err
	}
	return toDomainUpstream(row), nil
}

func (r *UpstreamRepo) GetUpstream(ctx context.Context, id int64) (*domain.Upstream, error) {
	row, err := r.client.Upstream.Query().
		Where(upstream.IDEQ(id), upstream.DeletedAtIsNil()).
		Only(ctx)
	if err != nil {
		return nil, errMissingID(err, id)
	}
	return toDomainUpstream(row), nil
}

func (r *UpstreamRepo) ListUpstreams(ctx context.Context, q ListQuery) ([]*domain.Upstream, int64, error) {
	pred := r.client.Upstream.Query().Where(upstream.DeletedAtIsNil())
	if q.Name != "" {
		pred = pred.Where(upstream.NameContainsFold(q.Name))
	}
	if len(q.StatusList) > 0 {
		for _, status := range q.StatusList {
			switch status {
			case "active":
				pred = pred.Where(upstream.EnabledEQ(true))
			case "disabled":
				pred = pred.Where(upstream.EnabledEQ(false))
			default:
				return nil, 0, fmt.Errorf("invalid upstream status %q", status)
			}
		}
	}
	total, err := pred.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	order, err := q.sortOrder(upstreamSortFields)
	if err != nil {
		return nil, 0, err
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	if q.Offset < 0 {
		q.Offset = 0
	}
	rows, err := pred.Order(order).Offset(q.Offset).Limit(q.Limit).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.Upstream, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainUpstream(row))
	}
	return out, int64(total), nil
}

// ListAllUpstreams returns one stable, ID-ordered inventory snapshot for
// long-running management operations. It intentionally avoids OFFSET paging:
// an upstream created or soft-deleted while validation is in progress cannot
// shift later rows and cause a probe to be skipped or duplicated.
func (r *UpstreamRepo) ListAllUpstreams(ctx context.Context) ([]*domain.Upstream, error) {
	rows, err := r.client.Upstream.Query().
		Where(upstream.DeletedAtIsNil()).
		Order(upstream.ByID()).
		Limit(upstreamValidationSnapshotLimit).
		All(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*domain.Upstream, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainUpstream(row))
	}
	return out, nil
}

func (r *UpstreamRepo) UpdateUpstream(ctx context.Context, u *domain.Upstream) (*domain.Upstream, error) {
	b := r.client.Upstream.UpdateOneID(u.ID).
		Where(upstream.DeletedAtIsNil()).
		SetName(u.Name).
		SetBaseURL(u.BaseURL).
		SetMultiplierBp(u.MultiplierBP).
		SetEnabled(u.Enabled).
		SetBalanceEndpoint(u.BalanceEndpoint).
		SetBalanceMethod(u.BalanceMethod).
		SetBalanceAuth(u.BalanceAuth).
		SetBalancePath(u.BalancePath).
		SetBalanceCurrencyPath(u.BalanceCurrencyPath).
		SetBalanceStatus(upstream.BalanceStatus(normalizeBalanceStatus(u.BalanceStatus)))
	if u.ExpectedUpdatedAt != nil {
		b.Where(upstream.UpdatedAtEQ(*u.ExpectedUpdatedAt))
	}
	if u.ClearUpstreamKey {
		b.ClearUpstreamKey()
	} else if u.UpstreamKey != nil {
		b.SetUpstreamKey(*u.UpstreamKey)
	}
	if u.Note != nil && strings.TrimSpace(*u.Note) != "" {
		b.SetNote(strings.TrimSpace(*u.Note))
	} else {
		b.ClearNote()
	}
	if u.BalanceAmount != nil {
		b.SetBalanceAmount(*u.BalanceAmount)
	} else {
		b.ClearBalanceAmount()
	}
	if u.BalanceCurrency != nil {
		b.SetBalanceCurrency(*u.BalanceCurrency)
	} else {
		b.ClearBalanceCurrency()
	}
	if u.BalanceCheckedAt != nil {
		b.SetBalanceCheckedAt(*u.BalanceCheckedAt)
	} else {
		b.ClearBalanceCheckedAt()
	}
	if u.ResetTelemetry {
		b.SetBalanceStatus(upstream.BalanceStatusUnconfigured).
			ClearBalanceAmount().
			ClearBalanceCurrency().
			ClearBalanceCheckedAt().
			SetRequestCount(0).
			SetSuccessCount(0).
			SetFailureCount(0).
			SetLatencyTotalMs(0).
			SetLatencyMaxMs(0).
			ClearLastCheckedAt().
			ClearLastSuccessAt().
			ClearLastFailureAt().
			ClearLastError().
			SetModels([]string{}).
			ClearModelsCheckedAt().
			ClearModelsError()
	}
	row, err := b.Save(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			current, getErr := r.GetUpstream(ctx, u.ID)
			if getErr != nil {
				return nil, getErr
			}
			if u.ExpectedUpdatedAt != nil && !current.UpdatedAt.Equal(*u.ExpectedUpdatedAt) {
				return nil, fmt.Errorf("%w: id=%d changed", ErrConflict, u.ID)
			}
			return nil, fmt.Errorf("%w: id=%d", ErrNotFound, u.ID)
		}
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: name=%q", ErrConflict, u.Name)
		}
		return nil, err
	}
	return toDomainUpstream(row), nil
}

// RecordUpstreamModels stores the bounded capability catalogue from a probe.
// The endpoint and write-only key are part of the predicate so a slow probe
// cannot attach an old model list to a newly edited upstream.
func (r *UpstreamRepo) RecordUpstreamModels(ctx context.Context, expected *domain.Upstream, models []string, modelErr *string) (*domain.Upstream, error) {
	if expected == nil || expected.ID <= 0 {
		return nil, fmt.Errorf("%w: missing expected upstream", ErrNotFound)
	}
	b := r.client.Upstream.Update().Where(
		upstream.IDEQ(expected.ID),
		upstream.DeletedAtIsNil(),
		upstream.BaseURLEQ(expected.BaseURL),
	)
	if modelErr != nil && strings.TrimSpace(*modelErr) != "" {
		code := domain.TruncateErrMsg(strings.TrimSpace(*modelErr))
		if models != nil {
			// A non-nil list is an explicit completed validation snapshot. Publish
			// exactly the subset that answered a real request, including an empty
			// subset when every model failed. ModelsCheckedAt distinguishes this
			// confirmed empty snapshot from an endpoint that has never been
			// inspected. The service passes nil for incomplete catalogue/transport
			// failures, which intentionally keeps the previous snapshot.
			clean := append([]string{}, models...)
			b.SetModels(clean).SetModelsCheckedAt(time.Now()).SetModelsError(code)
		} else {
			b.SetModelsError(code)
		}
	} else {
		clean := append([]string{}, models...)
		b.SetModels(clean).SetModelsCheckedAt(time.Now())
		b.ClearModelsError()
	}
	if expected.UpstreamKey == nil {
		b.Where(upstream.UpstreamKeyIsNil())
	} else {
		b.Where(upstream.UpstreamKeyEQ(*expected.UpstreamKey))
	}
	n, err := b.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return r.upstreamTelemetryConflict(ctx, expected.ID)
	}
	return r.GetUpstream(ctx, expected.ID)
}

func (r *UpstreamRepo) SetUpstreamEnabled(ctx context.Context, id int64, enabled bool) (*domain.Upstream, error) {
	n, err := r.client.Upstream.Update().
		Where(upstream.IDEQ(id), upstream.DeletedAtIsNil()).
		SetEnabled(enabled).
		Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	return r.GetUpstream(ctx, id)
}

func (r *UpstreamRepo) DeleteUpstream(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	tx, err := r.client.Tx(ctx)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	n, err := tx.Upstream.Update().
		Where(upstream.IDEQ(id), upstream.DeletedAtIsNil()).
		SetDeletedAt(time.Now()).
		Save(ctx)
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: id=%d missing", ErrNotFound, id)
	}
	if _, err := tx.GroupUpstream.Delete().Where(groupupstream.UpstreamIDEQ(id)).Exec(ctx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	return nil
}

// RecordUpstreamProbe updates cumulative health counters in one SQL UPDATE, so
// concurrent probes cannot lose increments. The read configuration is part of
// the predicate: a probe that began before its endpoint or key was changed is
// discarded instead of attaching old telemetry to the new connection.
func (r *UpstreamRepo) RecordUpstreamProbe(ctx context.Context, expected *domain.Upstream, success bool, latencyMS int64, probeErr *string) (*domain.Upstream, error) {
	if expected == nil || expected.ID <= 0 {
		return nil, fmt.Errorf("%w: missing expected upstream", ErrNotFound)
	}
	if latencyMS < 0 {
		latencyMS = 0
	}
	if r.driver != nil {
		var result entsql.Result
		var errArg any
		var keyArg any
		if probeErr != nil {
			errArg = *probeErr
		}
		if expected.UpstreamKey != nil {
			keyArg = *expected.UpstreamKey
		}
		const query = `UPDATE "upstreams"
			SET "last_checked_at" = now(),
				"request_count" = "request_count" + 1,
				"latency_total_ms" = "latency_total_ms" + $3,
				"latency_max_ms" = GREATEST("latency_max_ms", $3),
				"success_count" = "success_count" + CASE WHEN $2 THEN 1 ELSE 0 END,
				"failure_count" = "failure_count" + CASE WHEN $2 THEN 0 ELSE 1 END,
				"last_success_at" = CASE WHEN $2 THEN now() ELSE "last_success_at" END,
				"last_failure_at" = CASE WHEN $2 THEN "last_failure_at" ELSE now() END,
				"last_error" = CASE WHEN $2 THEN NULL ELSE $4::text END,
				"updated_at" = now()
			WHERE "id" = $1 AND "deleted_at" IS NULL
				AND "base_url" = $5
				AND "upstream_key" IS NOT DISTINCT FROM $6`
		if err := r.driver.Exec(ctx, query, []any{expected.ID, success, latencyMS, errArg, expected.BaseURL, keyArg}, &result); err != nil {
			return nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			return r.upstreamTelemetryConflict(ctx, expected.ID)
		}
		return r.GetUpstream(ctx, expected.ID)
	}
	// Lightweight test/tool repositories may not carry a raw driver. Keep the
	// Ent path as a compatibility fallback; production uses the atomic SQL above
	// so concurrent probes cannot overwrite the maximum latency.
	now := time.Now()
	b := r.client.Upstream.Update().
		Where(upstream.IDEQ(expected.ID), upstream.DeletedAtIsNil(), upstream.BaseURLEQ(expected.BaseURL)).
		SetLastCheckedAt(now).
		AddRequestCount(1).
		AddLatencyTotalMs(latencyMS)
	if expected.UpstreamKey == nil {
		b.Where(upstream.UpstreamKeyIsNil())
	} else {
		b.Where(upstream.UpstreamKeyEQ(*expected.UpstreamKey))
	}
	if current, err := r.client.Upstream.Query().Where(upstream.IDEQ(expected.ID), upstream.DeletedAtIsNil()).Only(ctx); err == nil && latencyMS > current.LatencyMaxMs {
		b.SetLatencyMaxMs(latencyMS)
	}
	if success {
		b.AddSuccessCount(1).ClearLastError().SetLastSuccessAt(now)
	} else {
		b.AddFailureCount(1).SetLastFailureAt(now).SetNillableLastError(probeErr)
	}
	n, err := b.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return r.upstreamTelemetryConflict(ctx, expected.ID)
	}
	return r.GetUpstream(ctx, expected.ID)
}

// RecordUpstreamBalance persists only the bounded management snapshot. The
// caller has already classified a failed read as stale or unavailable, so this
// method never needs an upstream response body or a credential. It only writes
// when the complete balance-reader configuration still matches the read that
// started this request.
func (r *UpstreamRepo) RecordUpstreamBalance(ctx context.Context, expected *domain.Upstream, amount, currency *string, status string, checkedAt *time.Time) (*domain.Upstream, error) {
	if expected == nil || expected.ID <= 0 {
		return nil, fmt.Errorf("%w: missing expected upstream", ErrNotFound)
	}
	var keyArg any
	var amountArg any
	var currencyArg any
	var checkedAtArg any
	if expected.UpstreamKey != nil {
		keyArg = *expected.UpstreamKey
	}
	if amount != nil {
		amountArg = *amount
	}
	if currency != nil {
		currencyArg = *currency
	}
	if checkedAt != nil {
		checkedAtArg = *checkedAt
	}
	if r.driver != nil {
		var result entsql.Result
		const query = `UPDATE "upstreams"
			SET "balance_amount" = $2,
				"balance_currency" = $3,
				"balance_status" = $4,
				"balance_checked_at" = $5,
				"updated_at" = now()
			WHERE "id" = $1 AND "deleted_at" IS NULL
				AND "base_url" = $6
				AND "upstream_key" IS NOT DISTINCT FROM $7
				AND "balance_endpoint" = $8
				AND "balance_method" = $9
				AND "balance_auth" = $10
				AND "balance_path" = $11
				AND "balance_currency_path" = $12`
		if err := r.driver.Exec(ctx, query, []any{expected.ID, amountArg, currencyArg, normalizeBalanceStatus(status), checkedAtArg, expected.BaseURL, keyArg, expected.BalanceEndpoint, expected.BalanceMethod, expected.BalanceAuth, expected.BalancePath, expected.BalanceCurrencyPath}, &result); err != nil {
			return nil, err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if affected == 0 {
			return r.upstreamTelemetryConflict(ctx, expected.ID)
		}
		return r.GetUpstream(ctx, expected.ID)
	}
	b := r.client.Upstream.Update().
		Where(
			upstream.IDEQ(expected.ID),
			upstream.DeletedAtIsNil(),
			upstream.BaseURLEQ(expected.BaseURL),
			upstream.BalanceEndpointEQ(expected.BalanceEndpoint),
			upstream.BalanceMethodEQ(expected.BalanceMethod),
			upstream.BalanceAuthEQ(expected.BalanceAuth),
			upstream.BalancePathEQ(expected.BalancePath),
			upstream.BalanceCurrencyPathEQ(expected.BalanceCurrencyPath),
		).
		SetBalanceStatus(upstream.BalanceStatus(normalizeBalanceStatus(status)))
	if expected.UpstreamKey == nil {
		b.Where(upstream.UpstreamKeyIsNil())
	} else {
		b.Where(upstream.UpstreamKeyEQ(*expected.UpstreamKey))
	}
	if amount != nil {
		b.SetBalanceAmount(*amount)
	} else {
		b.ClearBalanceAmount()
	}
	if currency != nil {
		b.SetBalanceCurrency(*currency)
	} else {
		b.ClearBalanceCurrency()
	}
	if checkedAt != nil {
		b.SetBalanceCheckedAt(*checkedAt)
	} else {
		b.ClearBalanceCheckedAt()
	}
	n, err := b.Save(ctx)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return r.upstreamTelemetryConflict(ctx, expected.ID)
	}
	return r.GetUpstream(ctx, expected.ID)
}

func (r *UpstreamRepo) upstreamTelemetryConflict(ctx context.Context, id int64) (*domain.Upstream, error) {
	if _, err := r.GetUpstream(ctx, id); err != nil {
		return nil, err
	}
	return nil, fmt.Errorf("%w: id=%d configuration changed", ErrConflict, id)
}

func normalizeBalanceStatus(status string) string {
	switch status {
	case domain.UpstreamBalanceFresh, domain.UpstreamBalanceStale, domain.UpstreamBalanceUnavailable:
		return status
	default:
		return domain.UpstreamBalanceUnconfigured
	}
}
