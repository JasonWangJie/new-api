package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

var (
	ErrInvalidUserQuotaAdjustment = errors.New("invalid user quota adjustment")
	ErrUserQuotaPermission        = errors.New("cannot adjust quota for this user role")
	ErrManualPaymentRequired      = errors.New("manual credit requires confirmed payment amount, idempotency key and expected quota change")
	ErrManualPaymentInvalid       = errors.New("invalid manual payment amount or idempotency key")
	ErrUserQuotaPreviewChanged    = errors.New("quota change differs from preview; preview and confirm again")
	ErrManualQuotaConflict        = errors.New("idempotency key was already used with different parameters")
)

// UserQuotaAdjustment is the committed database snapshot, excluding pending
// relay deductions in the cache. Replays return the original snapshot.
type UserQuotaAdjustment struct {
	UserID     int    `json:"user_id"`
	Username   string `json:"username"`
	Before     int    `json:"before"`
	After      int    `json:"after"`
	DeltaQuota string `json:"delta_quota"`
	Order      *TopUp `json:"order,omitempty"`
	Replayed   bool   `json:"replayed"`
	Group      string `json:"-"`
}

type ManualQuotaOrderInput struct {
	OperatorID         int
	OperatorUsername   string
	PaymentAmountCents *int64
	IdempotencyKey     string
	ExpectedDeltaQuota string
}

type manualQuotaOrderSnapshot struct {
	Version            int    `json:"version"`
	OperatorID         int    `json:"operator_id"`
	OperatorUsername   string `json:"operator_username"`
	IdempotencyKey     string `json:"idempotency_key"`
	Mode               string `json:"mode"`
	Value              int    `json:"value"`
	Before             int    `json:"before"`
	After              int    `json:"after"`
	CreditedQuota      string `json:"credited_quota"`
	CreditedAmount     string `json:"credited_amount"`
	QuotaPerUnit       string `json:"quota_per_unit"`
	PaymentAmountCents int64  `json:"payment_amount_cents"`
}

func userQuotaAfter(before int, mode string, value int) (int, error) {
	if mode != "add" && mode != "subtract" && mode != "override" {
		return 0, ErrInvalidUserQuotaAdjustment
	}
	if mode != "override" && value <= 0 {
		return 0, ErrInvalidUserQuotaAdjustment
	}
	if before > common.MaxWalletQuota || before < -common.MaxWalletQuota || value > common.MaxWalletQuota || value < -common.MaxWalletQuota {
		return 0, ErrWalletQuotaLimitExceeded
	}
	quota := decimal.NewFromInt(int64(value))
	switch mode {
	case "add":
		quota = decimal.NewFromInt(int64(before)).Add(quota)
	case "subtract":
		quota = decimal.NewFromInt(int64(before)).Sub(quota)
	}
	after, err := common.WalletQuotaFromDecimalStrict(quota)
	if err != nil {
		return 0, ErrWalletQuotaLimitExceeded
	}
	return after, nil
}

func PreviewUserQuota(userID, operatorRole int, mode string, value int) (*UserQuotaAdjustment, error) {
	if userID <= 0 {
		return nil, ErrInvalidUserQuotaAdjustment
	}
	var user User
	if err := DB.First(&user, userID).Error; err != nil {
		return nil, err
	}
	if operatorRole < common.RoleAdminUser || (operatorRole != common.RoleRootUser && operatorRole <= user.Role) {
		return nil, ErrUserQuotaPermission
	}
	after, err := userQuotaAfter(user.Quota, mode, value)
	if err != nil {
		return nil, err
	}
	return &UserQuotaAdjustment{UserID: user.Id, Username: user.Username, Group: user.Group, Before: user.Quota, After: after,
		DeltaQuota: strconv.FormatInt(int64(after)-int64(user.Quota), 10)}, nil
}

// ValidateManualPaymentAmount bounds public integer cents and ensures the legacy
// Money column round-trips them exactly through the invoice converter.
func ValidateManualPaymentAmount(cents int64) (float64, error) {
	if cents < 0 || cents > common.MaxWalletQuota {
		return 0, ErrManualPaymentInvalid
	}
	money := decimal.NewFromInt(cents).Shift(-2).InexactFloat64()
	if cents == 0 {
		return money, nil
	}
	roundTrip, err := topUpInvoiceAmountCents(money)
	if err != nil || roundTrip != cents {
		return 0, ErrManualPaymentInvalid
	}
	return money, nil
}

func (topUp *TopUp) manualQuotaSnapshot() (*manualQuotaOrderSnapshot, error) {
	if topUp.PaymentProvider != PaymentProviderManualBackend || topUp.PaymentMethod != PaymentMethodManualBackend ||
		topUp.Status != common.TopUpStatusSuccess || topUp.ManualAdjustment == nil {
		return nil, ErrManualPaymentInvalid
	}
	var snapshot manualQuotaOrderSnapshot
	if err := common.UnmarshalJsonStr(*topUp.ManualAdjustment, &snapshot); err != nil {
		return nil, ErrManualPaymentInvalid
	}
	after, err := userQuotaAfter(snapshot.Before, snapshot.Mode, snapshot.Value)
	delta := int64(snapshot.After) - int64(snapshot.Before)
	if err != nil || snapshot.Version != 1 || snapshot.OperatorID <= 0 || after != snapshot.After || delta <= 0 ||
		topUp.Amount != delta || snapshot.CreditedQuota != strconv.FormatInt(delta, 10) {
		return nil, ErrManualPaymentInvalid
	}
	key, err := uuid.Parse(snapshot.IdempotencyKey)
	if err != nil || key.String() != snapshot.IdempotencyKey {
		return nil, ErrManualPaymentInvalid
	}
	money, err := ValidateManualPaymentAmount(snapshot.PaymentAmountCents)
	if err != nil || topUp.Money != money {
		return nil, ErrManualPaymentInvalid
	}
	unit, err := decimal.NewFromString(snapshot.QuotaPerUnit)
	if err != nil || !unit.IsPositive() || snapshot.CreditedAmount != decimal.NewFromInt(delta).Div(unit).String() {
		return nil, ErrManualPaymentInvalid
	}
	return &snapshot, nil
}

// AfterFind publishes only billing fields. Operator and replay inputs never leave
// the private snapshot, even in the user's order response.
func (topUp *TopUp) AfterFind(_ *gorm.DB) error {
	snapshot, err := topUp.manualQuotaSnapshot()
	if err == nil {
		topUp.CreditedQuota = snapshot.CreditedQuota
		topUp.CreditedAmount = snapshot.CreditedAmount
		topUp.PaymentAmountCents = &snapshot.PaymentAmountCents
		topUp.PaymentCurrency = "CNY"
	}
	return nil
}

func AdjustUserQuota(userID, operatorRole int, mode string, value int, orderInput *ManualQuotaOrderInput) (*UserQuotaAdjustment, error) {
	if userID <= 0 {
		return nil, ErrInvalidUserQuotaAdjustment
	}
	var adjustment UserQuotaAdjustment
	err := DB.Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).First(&user, userID).Error; err != nil {
			return err
		}
		if operatorRole < common.RoleAdminUser || (operatorRole != common.RoleRootUser && operatorRole <= user.Role) {
			return ErrUserQuotaPermission
		}
		var tradeNo string
		if orderInput != nil && orderInput.IdempotencyKey != "" {
			key, err := uuid.Parse(orderInput.IdempotencyKey)
			if err != nil || key.String() != orderInput.IdempotencyKey || orderInput.OperatorID <= 0 {
				return ErrManualPaymentInvalid
			}
			hash := sha256.Sum256(fmt.Appendf(nil, "%d:%d:%s", orderInput.OperatorID, userID, key.String()))
			tradeNo = fmt.Sprintf("MANUAL_%x", hash)
			var existing TopUp
			err = tx.Where("trade_no = ?", tradeNo).First(&existing).Error
			if err == nil {
				snapshot, err := existing.manualQuotaSnapshot()
				if err != nil || existing.UserId != userID || snapshot.OperatorID != orderInput.OperatorID ||
					snapshot.Mode != mode || snapshot.Value != value || orderInput.PaymentAmountCents == nil ||
					snapshot.PaymentAmountCents != *orderInput.PaymentAmountCents || snapshot.CreditedQuota != orderInput.ExpectedDeltaQuota {
					return ErrManualQuotaConflict
				}
				adjustment = UserQuotaAdjustment{UserID: userID, Username: user.Username, Before: snapshot.Before, After: snapshot.After,
					DeltaQuota: snapshot.CreditedQuota, Order: &existing, Replayed: true}
				return nil
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}
		after, err := userQuotaAfter(user.Quota, mode, value)
		if err != nil {
			return err
		}
		delta := int64(after) - int64(user.Quota)
		adjustment = UserQuotaAdjustment{UserID: user.Id, Username: user.Username, Before: user.Quota, After: after,
			DeltaQuota: strconv.FormatInt(delta, 10)}
		if orderInput != nil && orderInput.ExpectedDeltaQuota != "" && orderInput.ExpectedDeltaQuota != adjustment.DeltaQuota {
			return ErrUserQuotaPreviewChanged
		}
		if delta > 0 {
			if orderInput == nil || orderInput.PaymentAmountCents == nil || orderInput.IdempotencyKey == "" || orderInput.ExpectedDeltaQuota == "" {
				return ErrManualPaymentRequired
			}
			if tradeNo == "" {
				return ErrManualPaymentInvalid
			}
			money, err := ValidateManualPaymentAmount(*orderInput.PaymentAmountCents)
			if err != nil {
				return err
			}
			if math.IsNaN(common.QuotaPerUnit) || math.IsInf(common.QuotaPerUnit, 0) || common.QuotaPerUnit <= 0 {
				return ErrManualPaymentInvalid
			}
			unit := decimal.NewFromFloat(common.QuotaPerUnit)
			snapshot := manualQuotaOrderSnapshot{Version: 1, OperatorID: orderInput.OperatorID, OperatorUsername: orderInput.OperatorUsername,
				IdempotencyKey: orderInput.IdempotencyKey, Mode: mode, Value: value, Before: user.Quota, After: after,
				CreditedQuota: adjustment.DeltaQuota, CreditedAmount: decimal.NewFromInt(delta).Div(unit).String(),
				QuotaPerUnit: unit.String(), PaymentAmountCents: *orderInput.PaymentAmountCents}
			encoded, err := common.Marshal(snapshot)
			if err != nil {
				return err
			}
			privateSnapshot := string(encoded)
			now := common.GetTimestamp()
			order := TopUp{UserId: userID, Amount: delta, Money: money, TradeNo: tradeNo,
				PaymentMethod: PaymentMethodManualBackend, PaymentProvider: PaymentProviderManualBackend,
				CreateTime: now, CompleteTime: now, Status: common.TopUpStatusSuccess, ManualAdjustment: &privateSnapshot}
			if err := tx.Create(&order).Error; err != nil {
				return err
			}
			_ = order.AfterFind(tx)
			adjustment.Order = &order
		}
		if after != user.Quota {
			// The condition also protects SQLite, which has no SELECT FOR UPDATE.
			result := tx.Model(&User{}).Where("id = ? AND quota = ?", userID, user.Quota).Update("quota", after)
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrUserQuotaPreviewChanged
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Apply only the first committed difference, preserving outstanding reservations.
	delta := int64(adjustment.After) - int64(adjustment.Before)
	if !adjustment.Replayed && delta != 0 {
		if err := cacheIncrUserQuota(userID, delta); err != nil {
			common.SysError(fmt.Sprintf("failed to sync manual quota adjustment for user %d: %s", userID, err))
		}
	}
	return &adjustment, nil
}
