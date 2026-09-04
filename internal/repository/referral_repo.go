// SPDX-License-Identifier: AGPL-3.0-or-later

package repository

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	"entgo.io/ent/dialect/sql/sqlgraph"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/ent"
	"github.com/is7qin/c3api/internal/ent/balanceledger"
	"github.com/is7qin/c3api/internal/ent/referral"
	"github.com/is7qin/c3api/internal/ent/referralreward"
	"github.com/is7qin/c3api/internal/ent/user"
)

type ReferralRepo struct {
	client *ent.Client
	driver dialect.Driver
}

func (r *ReferralRepo) GetUserByInviteCode(ctx context.Context, code string) (*domain.User, error) {
	row, err := r.client.User.Query().Where(user.InviteCodeEQ(strings.ToUpper(strings.TrimSpace(code)))).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("%w: invite code", ErrNotFound)
	}
	if err != nil {
		return nil, err
	}
	return toDomainUser(row), nil
}

func (r *ReferralRepo) EnsureInviteCode(ctx context.Context, userID int64, code string) (*domain.User, error) {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return nil, fmt.Errorf("empty invite code")
	}
	n, err := r.client.User.Update().
		Where(user.IDEQ(userID), user.InviteCodeIsNil()).
		SetInviteCode(code).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: invite code collision", ErrConflict)
		}
		return nil, err
	}
	if n == 0 {
		row, getErr := r.client.User.Get(ctx, userID)
		if getErr != nil {
			return nil, errMissingID(getErr, userID)
		}
		return toDomainUser(row), nil
	}
	row, err := r.client.User.Get(ctx, userID)
	if err != nil {
		return nil, errMissingID(err, userID)
	}
	return toDomainUser(row), nil
}

func (r *ReferralRepo) CreateReferral(ctx context.Context, inviterID, inviteeID int64) (*domain.Referral, error) {
	if inviterID <= 0 || inviteeID <= 0 || inviterID == inviteeID {
		return nil, fmt.Errorf("invalid referral")
	}
	row, err := r.client.Referral.Create().SetInviterID(inviterID).SetInviteeID(inviteeID).Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: invitee already bound", ErrConflict)
		}
		return nil, err
	}
	return toDomainReferral(row), nil
}

func (r *ReferralRepo) GetReferralByInvitee(ctx context.Context, inviteeID int64) (*domain.Referral, error) {
	row, err := r.client.Referral.Query().Where(referral.InviteeIDEQ(inviteeID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil, fmt.Errorf("%w: invitee_id=%d", ErrNotFound, inviteeID)
	}
	if err != nil {
		return nil, err
	}
	return toDomainReferral(row), nil
}

func (r *ReferralRepo) Summary(ctx context.Context, userID int64, now time.Time) (*domain.ReferralSummary, error) {
	u, err := r.client.User.Get(ctx, userID)
	if err != nil {
		return nil, errMissingID(err, userID)
	}
	result := &domain.ReferralSummary{}
	if u.InviteCode != nil {
		result.InviteCode = *u.InviteCode
	}
	if rel, err := r.client.Referral.Query().Where(referral.InviteeIDEQ(userID)).Only(ctx); err == nil {
		result.InviterID = &rel.InviterID
	} else if !ent.IsNotFound(err) {
		return nil, err
	}
	count, err := r.client.Referral.Query().Where(referral.InviterIDEQ(userID)).Count(ctx)
	if err != nil {
		return nil, err
	}
	result.InviteCount = int64(count)
	rewards, err := r.client.ReferralReward.Query().Where(referralreward.InviterIDEQ(userID)).All(ctx)
	if err != nil {
		return nil, err
	}
	for _, reward := range rewards {
		switch reward.Status {
		case referralreward.StatusCredited:
			result.CreditedAmount += reward.RewardAmount
		case referralreward.StatusPending:
			if reward.AvailableAt.After(now) {
				result.PendingAmount += reward.RewardAmount
				if result.NextAvailableAt == nil || reward.AvailableAt.Before(*result.NextAvailableAt) {
					t := reward.AvailableAt
					result.NextAvailableAt = &t
				}
			} else {
				result.AvailableAmount += reward.RewardAmount
			}
		}
	}
	return result, nil
}

// ApplyBalanceCredit mutates balance and writes both audit ledgers in the
// caller's transaction. A duplicate idempotency key rolls the whole operation
// back, including the balance update.
func (r *ReferralRepo) ApplyBalanceCredit(ctx context.Context, userID, amount int64, kind domain.BalanceLedgerKind, sourceID string, note *string, actorID *int64) (*domain.BalanceLedgerEntry, error) {
	if userID <= 0 || amount == 0 || strings.TrimSpace(sourceID) == "" {
		return nil, fmt.Errorf("invalid balance adjustment")
	}
	idempotencyKey := fmt.Sprintf("%s:%s:%d", kind, sourceID, userID)
	if existing, err := r.client.BalanceLedger.Query().Where(balanceledger.IdempotencyKeyEQ(idempotencyKey)).Only(ctx); err == nil {
		return toDomainBalanceLedger(existing), nil
	} else if !ent.IsNotFound(err) {
		return nil, err
	}
	after, err := r.incrementBalanceReturning(ctx, userID, amount)
	if err != nil {
		return nil, err
	}
	entry, err := r.client.BalanceLedger.Create().
		SetUserID(userID).
		SetKind(balanceledger.Kind(kind)).
		SetSourceID(sourceID).
		SetNillableNote(note).
		SetIdempotencyKey(idempotencyKey).
		SetDelta(amount).
		SetBalanceBefore(after - amount).
		SetBalanceAfter(after).
		SetNillableActorUserID(actorID).
		Save(ctx)
	if err != nil {
		if sqlgraph.IsUniqueConstraintError(err) {
			return nil, fmt.Errorf("%w: duplicate balance credit", ErrConflict)
		}
		return nil, err
	}
	if amount > 0 && (kind == domain.BalanceLedgerRedemption || kind == domain.BalanceLedgerAdminCredit) {
		if err := r.createRewardForCredit(ctx, userID, amount, kind, sourceID); err != nil {
			return nil, err
		}
	}
	return toDomainBalanceLedger(entry), nil
}

// ApplyBalanceSet preserves the legacy absolute-balance API without racing
// concurrent usage debits. The row lock makes the target-to-delta conversion
// part of the same transaction as the audit ledger and referral reward.
func (r *ReferralRepo) ApplyBalanceSet(ctx context.Context, userID, target int64, sourceID string, note *string, actorID *int64) (*domain.BalanceLedgerEntry, error) {
	if userID <= 0 || target < 0 || strings.TrimSpace(sourceID) == "" {
		return nil, fmt.Errorf("invalid balance target")
	}
	row, err := r.client.User.Query().Where(user.IDEQ(userID), func(s *entsql.Selector) { s.ForUpdate() }).Only(ctx)
	if err != nil {
		return nil, errMissingID(err, userID)
	}
	delta := target - row.Balance
	if delta == 0 {
		return &domain.BalanceLedgerEntry{UserID: userID, Kind: domain.BalanceLedgerAdminCredit, SourceID: sourceID, Delta: 0, BalanceBefore: target, BalanceAfter: target, ActorUserID: actorID}, nil
	}
	return r.ApplyBalanceCredit(ctx, userID, delta, domain.BalanceLedgerAdminCredit, sourceID, note, actorID)
}

func (r *ReferralRepo) createRewardForCredit(ctx context.Context, inviteeID, amount int64, kind domain.BalanceLedgerKind, sourceID string) error {
	rel, err := r.client.Referral.Query().Where(referral.InviteeIDEQ(inviteeID)).Only(ctx)
	if ent.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	rewardAmount := referralRewardAmount(amount)
	if rewardAmount <= 0 {
		return nil
	}
	sourceType := referralreward.SourceTypeRedemption
	if kind == domain.BalanceLedgerAdminCredit {
		sourceType = referralreward.SourceTypeAdminCredit
	}
	idem := fmt.Sprintf("%s:%s:%d", sourceType, sourceID, inviteeID)
	_, err = r.client.ReferralReward.Create().
		SetInviterID(rel.InviterID).
		SetInviteeID(inviteeID).
		SetSourceType(sourceType).
		SetSourceID(sourceID).
		SetIdempotencyKey(idem).
		SetBaseAmount(amount).
		SetRateBps(domain.ReferralRateBPS).
		SetRewardAmount(rewardAmount).
		SetStatus(referralreward.StatusPending).
		SetAvailableAt(time.Now().Add(24 * time.Hour)).
		Save(ctx)
	if err != nil && sqlgraph.IsUniqueConstraintError(err) {
		return fmt.Errorf("%w: duplicate referral reward", ErrConflict)
	}
	return err
}

func referralRewardAmount(baseAmount int64) int64 {
	if baseAmount <= 0 {
		return 0
	}
	return baseAmount / 20 // exact floor of 5%, without overflow
}

func (r *ReferralRepo) ClaimAvailable(ctx context.Context, inviterID int64, now time.Time, sourceID string) (*domain.BalanceLedgerEntry, error) {
	idem := fmt.Sprintf("%s:%s:%d", domain.BalanceLedgerReferralClaim, sourceID, inviterID)
	if existing, err := r.client.BalanceLedger.Query().Where(balanceledger.IdempotencyKeyEQ(idem)).Only(ctx); err == nil {
		return toDomainBalanceLedger(existing), nil
	} else if !ent.IsNotFound(err) {
		return nil, err
	}
	rows, err := r.client.ReferralReward.Query().Where(
		referralreward.InviterIDEQ(inviterID),
		referralreward.StatusEQ(referralreward.StatusPending),
		referralreward.AvailableAtLTE(now),
		func(s *entsql.Selector) { s.ForUpdate() },
	).All(ctx)
	if err != nil {
		return nil, err
	}
	var total int64
	ids := make([]int64, 0, len(rows))
	for _, row := range rows {
		if row.RewardAmount > 0 && total <= int64(^uint64(0)>>1)-row.RewardAmount {
			total += row.RewardAmount
			ids = append(ids, row.ID)
		}
	}
	if total == 0 {
		return nil, fmt.Errorf("%w: no available referral reward", ErrNotFound)
	}
	entry, err := r.ApplyBalanceCredit(ctx, inviterID, total, domain.BalanceLedgerReferralClaim, sourceID, nil, nil)
	if err != nil {
		return nil, err
	}
	n, err := r.client.ReferralReward.Update().Where(
		referralreward.IDIn(ids...),
		referralreward.StatusEQ(referralreward.StatusPending),
	).SetStatus(referralreward.StatusCredited).SetCreditedAt(now).Save(ctx)
	if err != nil {
		return nil, err
	}
	if n != len(ids) {
		return nil, errors.New("referral rewards changed during claim")
	}
	return entry, nil
}

func (r *ReferralRepo) ListRewards(ctx context.Context, userID int64, limit, offset int) ([]*domain.ReferralReward, int64, error) {
	q := r.client.ReferralReward.Query().Where(referralreward.InviterIDEQ(userID))
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Order(ent.Desc(referralreward.FieldID)).Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.ReferralReward, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainReferralReward(row))
	}
	inviteeIDs := make([]int64, 0, len(out))
	for _, reward := range out {
		inviteeIDs = append(inviteeIDs, reward.InviteeID)
	}
	if len(inviteeIDs) > 0 {
		users, usersErr := r.client.User.Query().Where(user.IDIn(inviteeIDs...)).All(ctx)
		if usersErr != nil {
			return nil, 0, usersErr
		}
		emails := make(map[int64]string, len(users))
		for _, u := range users {
			emails[u.ID] = u.Email
		}
		for _, reward := range out {
			reward.InviteeEmail = emails[reward.InviteeID]
		}
	}
	return out, int64(total), nil
}

func (r *ReferralRepo) ListReferrals(ctx context.Context, inviterID, inviteeID int64, limit, offset int) ([]*domain.ReferralRecord, int64, error) {
	q := r.client.Referral.Query()
	if inviterID > 0 {
		q = q.Where(referral.InviterIDEQ(inviterID))
	}
	if inviteeID > 0 {
		q = q.Where(referral.InviteeIDEQ(inviteeID))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Order(ent.Desc(referral.FieldID)).Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	ids := make([]int64, 0, len(rows)*2)
	for _, row := range rows {
		ids = append(ids, row.InviterID, row.InviteeID)
	}
	users, err := r.client.User.Query().Where(user.IDIn(ids...)).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	emails := make(map[int64]string, len(users))
	for _, u := range users {
		emails[u.ID] = u.Email
	}
	out := make([]*domain.ReferralRecord, 0, len(rows))
	for _, row := range rows {
		out = append(out, &domain.ReferralRecord{ID: row.ID, InviterID: row.InviterID, InviterEmail: emails[row.InviterID], InviteeID: row.InviteeID, InviteeEmail: emails[row.InviteeID], CreatedAt: row.CreatedAt})
	}
	return out, int64(total), nil
}

func (r *ReferralRepo) ListBalanceLedger(ctx context.Context, userID int64, limit, offset int) ([]*domain.BalanceLedgerEntry, int64, error) {
	q := r.client.BalanceLedger.Query()
	if userID > 0 {
		q = q.Where(balanceledger.UserIDEQ(userID))
	}
	total, err := q.Count(ctx)
	if err != nil {
		return nil, 0, err
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := q.Order(ent.Desc(balanceledger.FieldID)).Limit(limit).Offset(offset).All(ctx)
	if err != nil {
		return nil, 0, err
	}
	out := make([]*domain.BalanceLedgerEntry, 0, len(rows))
	for _, row := range rows {
		out = append(out, toDomainBalanceLedger(row))
	}
	return out, int64(total), nil
}

func (r *ReferralRepo) incrementBalanceReturning(ctx context.Context, userID, amount int64) (int64, error) {
	const query = `UPDATE "users" SET "balance" = "balance" + $1, "updated_at" = CURRENT_TIMESTAMP WHERE "id" = $2 AND "balance" + $1 >= 0 RETURNING "balance"`
	rows := &entsql.Rows{}
	if err := r.driver.Query(ctx, query, []any{amount, userID}, rows); err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if _, err := r.client.User.Get(ctx, userID); err != nil {
			return 0, fmt.Errorf("%w: id=%d", ErrNotFound, userID)
		}
		return 0, fmt.Errorf("%w: balance cannot become negative", ErrConflict)
	}
	var after int64
	if err := rows.Scan(&after); err != nil {
		return 0, err
	}
	return after, nil
}

func toDomainReferral(row *ent.Referral) *domain.Referral {
	return &domain.Referral{ID: row.ID, InviterID: row.InviterID, InviteeID: row.InviteeID, CreatedAt: row.CreatedAt}
}

func toDomainReferralReward(row *ent.ReferralReward) *domain.ReferralReward {
	return &domain.ReferralReward{ID: row.ID, InviterID: row.InviterID, InviteeID: row.InviteeID,
		SourceType: domain.ReferralRewardSource(row.SourceType), SourceID: row.SourceID, IdempotencyKey: row.IdempotencyKey,
		BaseAmount: row.BaseAmount, RateBPS: row.RateBps, RewardAmount: row.RewardAmount,
		Status: domain.ReferralRewardStatus(row.Status), AvailableAt: row.AvailableAt, CreditedAt: row.CreditedAt, CreatedAt: row.CreatedAt}
}

func toDomainBalanceLedger(row *ent.BalanceLedger) *domain.BalanceLedgerEntry {
	return &domain.BalanceLedgerEntry{ID: row.ID, UserID: row.UserID, Kind: domain.BalanceLedgerKind(row.Kind),
		SourceID: row.SourceID, Note: row.Note, IdempotencyKey: row.IdempotencyKey, Delta: row.Delta,
		BalanceBefore: row.BalanceBefore, BalanceAfter: row.BalanceAfter, ActorUserID: row.ActorUserID, CreatedAt: row.CreatedAt}
}
