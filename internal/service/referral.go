// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/is7qin/c3api/internal/domain"
	"github.com/is7qin/c3api/internal/notify"
	"github.com/is7qin/c3api/internal/repository"
)

const inviteAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZ"

func randomLetters12() (string, error) {
	out := make([]byte, 12)
	for i := range out {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(inviteAlphabet))))
		if err != nil {
			return "", err
		}
		out[i] = inviteAlphabet[n.Int64()]
	}
	return string(out), nil
}

type referralStore interface {
	EnsureInviteCode(context.Context, int64, string) (*domain.User, error)
	GetReferralSummary(context.Context, int64, time.Time) (*domain.ReferralSummary, error)
	ListReferralRewards(context.Context, int64, int, int) ([]*domain.ReferralReward, int64, error)
	ListReferrals(context.Context, int64, int64, int, int) ([]*domain.ReferralRecord, int64, error)
	ListBalanceLedger(context.Context, int64, int, int) ([]*domain.BalanceLedgerEntry, int64, error)
}

func (s *Service) ListReferrals(ctx context.Context, inviterID, inviteeID int64, limit, offset int) ([]*domain.ReferralRecord, int64, error) {
	if inviterID < 0 || inviteeID < 0 || limit > 100 || offset < 0 {
		return nil, 0, ErrInvalidInput
	}
	store, err := s.referralStore()
	if err != nil {
		return nil, 0, err
	}
	return store.ListReferrals(ctx, inviterID, inviteeID, limit, offset)
}

func (s *Service) referralStore() (referralStore, error) {
	store, ok := s.store.(referralStore)
	if !ok {
		return nil, errorsNewReferralUnavailable()
	}
	return store, nil
}

func errorsNewReferralUnavailable() error { return fmt.Errorf("referral storage is not available") }

// GetReferralSummary lazily assigns a code to users created before the
// referral migration, then returns monetary totals in the canonical integer unit.
func (s *Service) GetReferralSummary(ctx context.Context, userID int64) (*domain.ReferralSummary, error) {
	store, err := s.referralStore()
	if err != nil {
		return nil, err
	}
	summary, err := store.GetReferralSummary(ctx, userID, time.Now())
	if err != nil {
		return nil, mapRepoErr(err)
	}
	if summary.InviteCode != "" {
		return summary, nil
	}
	for attempt := 0; attempt < 5; attempt++ {
		code, genErr := randomLetters12()
		if genErr != nil {
			return nil, genErr
		}
		if _, setErr := store.EnsureInviteCode(ctx, userID, code); setErr != nil {
			if strings.Contains(setErr.Error(), "collision") {
				continue
			}
			return nil, mapRepoErr(setErr)
		}
		return store.GetReferralSummary(ctx, userID, time.Now())
	}
	return nil, fmt.Errorf("invite code generation exhausted")
}

func (s *Service) ListReferralRewards(ctx context.Context, userID int64, limit, offset int) ([]*domain.ReferralReward, int64, error) {
	if userID <= 0 || limit > 100 || offset < 0 {
		return nil, 0, ErrInvalidInput
	}
	store, err := s.referralStore()
	if err != nil {
		return nil, 0, err
	}
	return store.ListReferralRewards(ctx, userID, limit, offset)
}

func (s *Service) ListBalanceLedger(ctx context.Context, userID int64, limit, offset int) ([]*domain.BalanceLedgerEntry, int64, error) {
	if userID < 0 || limit > 100 || offset < 0 {
		return nil, 0, ErrInvalidInput
	}
	store, err := s.referralStore()
	if err != nil {
		return nil, 0, err
	}
	return store.ListBalanceLedger(ctx, userID, limit, offset)
}

// CreditUserBalance adjusts a user's balance, records exact before/after
// balances and creates a 24-hour frozen referral reward for positive credits.
func (s *Service) CreditUserBalance(ctx context.Context, userID, amount, actorID int64, sourceID string) (*domain.BalanceLedgerEntry, error) {
	return s.CreditUserBalanceWithNote(ctx, userID, amount, actorID, sourceID, "")
}

func (s *Service) CreditUserBalanceWithNote(ctx context.Context, userID, amount, actorID int64, sourceID, note string) (*domain.BalanceLedgerEntry, error) {
	if userID <= 0 || amount <= 0 || strings.TrimSpace(sourceID) == "" {
		return nil, ErrInvalidInput
	}
	var entry *domain.BalanceLedgerEntry
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		creditor, ok := tx.(interface {
			ApplyBalanceCredit(context.Context, int64, int64, domain.BalanceLedgerKind, string, *string, *int64) (*domain.BalanceLedgerEntry, error)
		})
		if !ok {
			// Compatibility for lightweight stores used by old integrations. The
			// production Repository always implements the audited path above.
			if err := tx.UpdateUserBalance(ctx, userID, amount); err != nil {
				return err
			}
			entry = &domain.BalanceLedgerEntry{UserID: userID, Kind: domain.BalanceLedgerAdminCredit, SourceID: sourceID, Delta: amount}
			return nil
		}
		var actor *int64
		if actorID > 0 {
			actor = &actorID
		}
		var notePtr *string
		if strings.TrimSpace(note) != "" {
			n := strings.TrimSpace(note)
			notePtr = &n
		}
		var err error
		entry, err = creditor.ApplyBalanceCredit(ctx, userID, amount, domain.BalanceLedgerAdminCredit, sourceID, notePtr, actor)
		return err
	})
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	return entry, nil
}

// SetUserBalanceWithNote atomically sets an absolute balance target and audits
// the effective delta. Positive deltas create referral rewards; reductions do
// not. Lightweight legacy stores retain compatible behavior in tests.
func (s *Service) SetUserBalanceWithNote(ctx context.Context, userID, target, actorID int64, sourceID, note string) (*domain.BalanceLedgerEntry, error) {
	if userID <= 0 || target < 0 || strings.TrimSpace(sourceID) == "" {
		return nil, ErrInvalidInput
	}
	current, err := s.store.GetUser(ctx, userID)
	if err != nil {
		return nil, mapRepoErr(err)
	}
	var entry *domain.BalanceLedgerEntry
	err = s.store.WithTx(ctx, func(tx repository.TxStore) error {
		var actor *int64
		if actorID > 0 {
			actor = &actorID
		}
		var notePtr *string
		if strings.TrimSpace(note) != "" {
			n := strings.TrimSpace(note)
			notePtr = &n
		}
		if setter, ok := tx.(interface {
			ApplyBalanceSet(context.Context, int64, int64, string, *string, *int64) (*domain.BalanceLedgerEntry, error)
		}); ok {
			var setErr error
			entry, setErr = setter.ApplyBalanceSet(ctx, userID, target, sourceID, notePtr, actor)
			return setErr
		}
		delta := target - current.Balance
		if delta != 0 {
			if updateErr := tx.UpdateUserBalance(ctx, userID, delta); updateErr != nil {
				return updateErr
			}
		}
		entry = &domain.BalanceLedgerEntry{UserID: userID, Kind: domain.BalanceLedgerAdminCredit, SourceID: sourceID, Delta: delta, BalanceBefore: current.Balance, BalanceAfter: target, ActorUserID: actor}
		return nil
	})
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	return entry, nil
}

// ClaimReferralRewards atomically locks all matured rewards, credits their sum,
// records the resulting balance, and marks exactly those rewards credited.
func (s *Service) ClaimReferralRewards(ctx context.Context, userID int64, requestID string) (*domain.BalanceLedgerEntry, error) {
	if userID <= 0 || strings.TrimSpace(requestID) == "" {
		return nil, ErrInvalidInput
	}
	var entry *domain.BalanceLedgerEntry
	err := s.store.WithTx(ctx, func(tx repository.TxStore) error {
		claimer, ok := tx.(interface {
			ClaimAvailableReferralRewards(context.Context, int64, time.Time, string) (*domain.BalanceLedgerEntry, error)
		})
		if !ok {
			return errorsNewReferralUnavailable()
		}
		var err error
		entry, err = claimer.ClaimAvailableReferralRewards(ctx, userID, time.Now(), requestID)
		return err
	})
	if err != nil {
		return nil, mapRepoErr(err)
	}
	s.inv.Users()
	s.publish(ctx, notify.Change{Users: true})
	return entry, nil
}
