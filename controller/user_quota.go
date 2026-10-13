package controller

import (
	"errors"
	"math"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

func manageUserQuota(c *gin.Context, req ManageRequest) {
	action := "generic"
	params := model.AuditFields{
		"target_user_id":  req.Id,
		"mode":            req.Mode,
		"requested_quota": req.Value,
	}
	switch req.Mode {
	case "add":
		action = "user.quota_add"
	case "subtract":
		action = "user.quota_subtract"
	case "override":
		action = "user.quota_override"
	default:
		params["action"] = "add_quota"
		params["method"] = c.Request.Method
		params["route"] = c.FullPath()
	}
	success := false
	defer func() {
		content := auditContentEN(action, params)
		if !success {
			// Failed requests have no committed balance changes to render.
			content = "Failed user quota adjustment"
		}
		model.RecordOperationAuditLog(c.GetInt("id"), c.GetInt("role"), content, c.ClientIP(), action, params,
			auditOperatorInfo(c), &model.AuditRequestInfo{
				Method: c.Request.Method, Route: c.FullPath(), Status: c.Writer.Status(), Success: success,
			}, c)
		markAuditLogged(c)
	}()

	adjustment, err := model.AdjustUserQuota(req.Id, c.GetInt("role"), req.Mode, req.Value, &model.ManualQuotaOrderInput{
		OperatorID: c.GetInt("id"), OperatorUsername: c.GetString("username"), PaymentAmountCents: req.PaymentAmountCents,
		IdempotencyKey: req.IdempotencyKey, ExpectedDeltaQuota: req.ExpectedDeltaQuota,
	})
	if err != nil {
		switch {
		case errors.Is(err, model.ErrManualPaymentRequired), errors.Is(err, model.ErrManualPaymentInvalid):
			params["failure_reason"] = "payment_confirmation_invalid"
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "code": "MANUAL_PAYMENT_INVALID", "message": common.TranslateMessage(c, i18n.MsgManualPaymentInvalid)})
		case errors.Is(err, model.ErrUserQuotaPreviewChanged), errors.Is(err, model.ErrManualQuotaConflict):
			params["failure_reason"] = "quota_order_conflict"
			c.JSON(http.StatusConflict, gin.H{"success": false, "code": "MANUAL_QUOTA_CONFLICT", "message": common.TranslateMessage(c, i18n.MsgManualQuotaConflict)})
		case errors.Is(err, model.ErrInvalidUserQuotaAdjustment):
			params["failure_reason"] = "invalid_parameters"
			if (req.Mode == "add" || req.Mode == "subtract") && req.Value <= 0 {
				common.ApiErrorI18n(c, i18n.MsgUserQuotaChangeZero)
			} else {
				common.ApiErrorI18n(c, i18n.MsgInvalidParams)
			}
		case errors.Is(err, model.ErrUserQuotaPermission):
			params["failure_reason"] = "permission_denied"
			common.ApiErrorI18n(c, i18n.MsgUserNoPermissionHigherLevel)
		case errors.Is(err, gorm.ErrRecordNotFound):
			params["failure_reason"] = "target_not_found"
			common.ApiErrorI18n(c, i18n.MsgUserNotExists)
		case errors.Is(err, model.ErrWalletQuotaLimitExceeded):
			params["failure_reason"] = "quota_limit_exceeded"
			common.ApiError(c, err)
		default:
			params["failure_reason"] = "database_error"
			common.ApiError(c, err)
		}
		return
	}

	params["target_username"] = adjustment.Username
	params["from"] = adjustment.Before
	params["to"] = adjustment.After
	if req.Mode != "override" {
		params["quota"] = req.Value
	}
	success = true
	if adjustment.Order != nil {
		params["order_id"] = adjustment.Order.Id
		params["trade_no"] = adjustment.Order.TradeNo
		params["payment_amount_cents"] = *adjustment.Order.PaymentAmountCents
		params["payment_method"] = model.PaymentMethodManualBackend
	}
	if adjustment.Replayed {
		params["replayed"] = true
	}
	operation := model.AuditOperation{Action: action, Params: params}
	if !adjustment.Replayed {
		model.RecordLogWithAdminInfo(adjustment.UserID, model.LogTypeTopup,
			auditContentEN(action, params), auditOperatorInfo(c), &operation, c)
	}
	common.ApiSuccess(c, adjustment)
}

// PreviewUserQuota computes a read-only quote using the target user's group.
// The payment amount remains an administrator-confirmed value at submission.
func PreviewUserQuota(c *gin.Context) {
	var req ManageRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorI18n(c, i18n.MsgInvalidParams)
		return
	}
	preview, err := model.PreviewUserQuota(req.Id, c.GetInt("role"), req.Mode, req.Value)
	if err != nil {
		if errors.Is(err, model.ErrUserQuotaPermission) {
			c.JSON(http.StatusForbidden, gin.H{"success": false, "message": common.TranslateMessage(c, i18n.MsgUserNoPermissionHigherLevel)})
			return
		}
		common.ApiError(c, err)
		return
	}
	delta, _ := strconv.ParseInt(preview.DeltaQuota, 10, 64)
	suggestedCents := int64(0)
	if delta > 0 {
		suggestedCents, err = manualQuotaPaymentQuote(delta, preview.Group)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"success": false, "message": common.TranslateMessage(c, i18n.MsgManualPaymentInvalid)})
			return
		}
	}
	common.ApiSuccess(c, gin.H{"before": preview.Before, "after": preview.After, "delta_quota": preview.DeltaQuota,
		"suggested_payment_amount_cents": suggestedCents, "payment_currency": "CNY", "payment_method": model.PaymentMethodManualBackend})
}

func manualQuotaPaymentQuote(delta int64, group string) (int64, error) {
	ratio := common.GetTopupGroupRatio(group)
	if ratio == 0 {
		ratio = 1
	}
	for _, quantity := range []float64{common.QuotaPerUnit, operation_setting.Price, ratio} {
		if math.IsNaN(quantity) || math.IsInf(quantity, 0) || quantity < 0 {
			return 0, model.ErrManualPaymentInvalid
		}
	}
	if common.QuotaPerUnit == 0 || delta <= 0 {
		return 0, model.ErrManualPaymentInvalid
	}
	amount := decimal.NewFromInt(delta).Div(decimal.NewFromFloat(common.QuotaPerUnit))
	discountKey := amount
	if operation_setting.GetQuotaDisplayType() == operation_setting.QuotaDisplayTypeTokens {
		discountKey = decimal.NewFromInt(delta)
	}
	discount := 1.0
	if discountKey.Equal(discountKey.Truncate(0)) && discountKey.LessThanOrEqual(decimal.NewFromInt(common.MaxWalletQuota)) {
		if configured, ok := operation_setting.GetPaymentSetting().AmountDiscount[int(discountKey.IntPart())]; ok && configured > 0 {
			discount = configured
		}
	}
	if math.IsNaN(discount) || math.IsInf(discount, 0) {
		return 0, model.ErrManualPaymentInvalid
	}
	cents := amount.Mul(decimal.NewFromFloat(operation_setting.Price)).Mul(decimal.NewFromFloat(ratio)).Mul(decimal.NewFromFloat(discount)).Shift(2).Round(0)
	if cents.IsNegative() || cents.GreaterThan(decimal.NewFromInt(common.MaxWalletQuota)) {
		return 0, model.ErrManualPaymentInvalid
	}
	value := cents.IntPart()
	_, err := model.ValidateManualPaymentAmount(value)
	return value, err
}
