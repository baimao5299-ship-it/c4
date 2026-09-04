package domain

import "time"

const ReferralRateBPS = 500

type Referral struct {
	ID        int64
	InviterID int64
	InviteeID int64
	CreatedAt time.Time
}

type ReferralRecord struct {
	ID           int64
	InviterID    int64
	InviterEmail string
	InviteeID    int64
	InviteeEmail string
	CreatedAt    time.Time
}

type ReferralRewardSource string

const (
	ReferralRewardSourceRedemption  ReferralRewardSource = "redemption"
	ReferralRewardSourceAdminCredit ReferralRewardSource = "admin_credit"
)

type ReferralRewardStatus string

const (
	ReferralRewardPending  ReferralRewardStatus = "pending"
	ReferralRewardCredited ReferralRewardStatus = "credited"
	ReferralRewardReversed ReferralRewardStatus = "reversed"
)

type ReferralReward struct {
	ID             int64
	InviterID      int64
	InviteeID      int64
	InviteeEmail   string
	SourceType     ReferralRewardSource
	SourceID       string
	IdempotencyKey string
	BaseAmount     int64
	RateBPS        int
	RewardAmount   int64
	Status         ReferralRewardStatus
	AvailableAt    time.Time
	CreditedAt     *time.Time
	CreatedAt      time.Time
}

type ReferralSummary struct {
	InviteCode      string
	InviterID       *int64
	InviteCount     int64
	PendingAmount   int64
	AvailableAmount int64
	CreditedAmount  int64
	NextAvailableAt *time.Time
}

type BalanceLedgerKind string

const (
	BalanceLedgerRedemption    BalanceLedgerKind = "redemption"
	BalanceLedgerAdminCredit   BalanceLedgerKind = "admin_credit"
	BalanceLedgerReferralClaim BalanceLedgerKind = "referral_claim"
)

type BalanceLedgerEntry struct {
	ID             int64
	UserID         int64
	Kind           BalanceLedgerKind
	SourceID       string
	Note           *string
	IdempotencyKey string
	Delta          int64
	BalanceBefore  int64
	BalanceAfter   int64
	ActorUserID    *int64
	CreatedAt      time.Time
}
