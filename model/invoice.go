package model

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/shopspring/decimal"
	"gorm.io/gorm"
)

const (
	InvoiceSourceUser         = "user"
	InvoiceSourceAdminHistory = "admin_history"

	InvoiceStatusPending   = "pending"
	InvoiceStatusCompleted = "completed"
	InvoiceStatusRejected  = "rejected"

	invoiceSQLBatchSize = 100
)

var (
	ErrInvoiceDisabled          = errors.New("企业开票功能未启用")
	ErrInvoiceOrderInvalid      = errors.New("所选充值订单不可开票")
	ErrInvoiceOrderClaimed      = errors.New("所选充值订单已被其他开票记录占用")
	ErrInvoiceAmountInvalid     = errors.New("充值订单开票金额无效")
	ErrInvoiceAmountBelowMin    = errors.New("所选订单合计金额未达到最低开票金额")
	ErrInvoiceRequestNotFound   = errors.New("开票申请不存在")
	ErrInvoiceRequestNotPending = errors.New("开票申请已处理")
)

type InvoiceRequest struct {
	Id               int                  `json:"id"`
	UserId           int                  `json:"user_id" gorm:"index;index:idx_invoice_user_status,priority:1"`
	Source           string               `json:"source" gorm:"type:varchar(32);not null"`
	Status           string               `json:"status" gorm:"type:varchar(20);index;index:idx_invoice_user_status,priority:2"`
	CompanyName      string               `json:"company_name" gorm:"type:varchar(200)"`
	TaxId            string               `json:"tax_id" gorm:"type:varchar(64)"`
	Email            string               `json:"email" gorm:"type:varchar(254)"`
	TotalAmountCents int64                `json:"total_amount_cents" gorm:"type:bigint;not null"`
	CreateTime       int64                `json:"create_time" gorm:"index"`
	ProcessedTime    int64                `json:"processed_time"`
	OperatorId       int                  `json:"operator_id" gorm:"index"`
	OperatorUsername string               `json:"operator_username" gorm:"type:varchar(64)"`
	RejectReason     string               `json:"reject_reason" gorm:"type:text"`
	Note             string               `json:"note" gorm:"type:text"`
	Username         string               `json:"username" gorm:"-:all"`
	DisplayName      string               `json:"display_name" gorm:"-:all"`
	Items            []InvoiceRequestItem `json:"items" gorm:"-:all"`
}

type InvoiceRequestItem struct {
	Id            int    `json:"id"`
	RequestId     int    `json:"request_id" gorm:"index;uniqueIndex:idx_invoice_request_topup,priority:1"`
	TopUpId       int    `json:"top_up_id" gorm:"index;uniqueIndex:idx_invoice_request_topup,priority:2"`
	TradeNo       string `json:"trade_no" gorm:"type:varchar(255)"`
	PaymentMethod string `json:"payment_method" gorm:"-:all"`
	AmountCents   int64  `json:"amount_cents" gorm:"type:bigint;not null"`
	CreateTime    int64  `json:"create_time"`
	CompleteTime  int64  `json:"complete_time"`
}

type InvoiceOrderClaim struct {
	TopUpId   int `json:"top_up_id" gorm:"primaryKey;autoIncrement:false"`
	RequestId int `json:"request_id" gorm:"index;not null"`
}

type InvoiceEligibleOrder struct {
	TopUpId       int    `json:"top_up_id"`
	TradeNo       string `json:"trade_no"`
	PaymentMethod string `json:"payment_method"`
	AmountCents   int64  `json:"amount_cents"`
	CreateTime    int64  `json:"create_time"`
	CompleteTime  int64  `json:"complete_time"`
}

type InvoiceOperator struct {
	Id       int
	Username string
}

type InvoiceProfile struct {
	CompanyName string `json:"company_name"`
	TaxId       string `json:"tax_id"`
	Email       string `json:"email"`
}

func topUpInvoiceAmountCents(money float64) (int64, error) {
	if math.IsNaN(money) || math.IsInf(money, 0) || money <= 0 {
		return 0, ErrInvoiceAmountInvalid
	}
	formatted := strconv.FormatFloat(money, 'f', 2, 64)
	amount, err := decimal.NewFromString(formatted)
	if err != nil {
		return 0, ErrInvoiceAmountInvalid
	}
	cents := amount.Shift(2).BigInt()
	if !cents.IsInt64() || cents.Sign() <= 0 {
		return 0, ErrInvoiceAmountInvalid
	}
	return cents.Int64(), nil
}

func isLegacyEpayInvoiceTopUp(topUp *TopUp) bool {
	prefix := fmt.Sprintf("USR%dNO", topUp.UserId)
	tail, ok := strings.CutPrefix(topUp.TradeNo, prefix)
	if !ok || len(tail) != 16 {
		return false
	}
	for i, char := range tail {
		if i < 6 {
			if (char < '0' || char > '9') && (char < 'a' || char > 'z') && (char < 'A' || char > 'Z') {
				return false
			}
			continue
		}
		if char < '0' || char > '9' {
			return false
		}
	}
	switch strings.ToLower(topUp.PaymentMethod) {
	case PaymentMethodStripe, PaymentMethodCreem, PaymentMethodWaffo, PaymentMethodWaffoPancake, PaymentMethodBalance:
		return false
	default:
		return true
	}
}

func isEpayInvoiceTopUp(topUp *TopUp) bool {
	if topUp.PaymentProvider == PaymentProviderEpay {
		return true
	}
	return topUp.PaymentProvider == "" && isLegacyEpayInvoiceTopUp(topUp)
}

func invoiceSearchPattern(value string) string {
	escaped := strings.NewReplacer("!", "!!", "%", "!%", "_", "!_").Replace(strings.TrimSpace(value))
	return "%" + escaped + "%"
}

func invoiceOrderQuery(tx *gorm.DB, userId int, keyword string) *gorm.DB {
	prefix := fmt.Sprintf("USR%dNO%%", userId)
	query := tx.Model(&TopUp{}).
		Select("top_ups.*").
		Joins("LEFT JOIN invoice_order_claims ON invoice_order_claims.top_up_id = top_ups.id").
		Where("top_ups.user_id = ? AND top_ups.status = ? AND top_ups.money > 0", userId, common.TopUpStatusSuccess).
		Where("invoice_order_claims.top_up_id IS NULL").
		Where("NOT EXISTS (SELECT 1 FROM subscription_orders WHERE subscription_orders.trade_no = top_ups.trade_no)").
		Where("(top_ups.payment_provider = ? OR (COALESCE(top_ups.payment_provider, '') = '' AND top_ups.trade_no LIKE ?))", PaymentProviderEpay, prefix)
	if strings.TrimSpace(keyword) != "" {
		query = query.Where("top_ups.trade_no LIKE ? ESCAPE '!'", invoiceSearchPattern(keyword))
	}
	return query
}

func ListInvoiceEligibleOrders(userId int, keyword string, pageInfo *common.PageInfo) ([]InvoiceEligibleOrder, int64, error) {
	query := invoiceOrderQuery(DB, userId, keyword)
	var topUps []TopUp
	if err := query.Order("top_ups.id DESC").Find(&topUps).Error; err != nil {
		return nil, 0, err
	}
	eligible := make([]InvoiceEligibleOrder, 0, len(topUps))
	for i := range topUps {
		if !isEpayInvoiceTopUp(&topUps[i]) {
			continue
		}
		amountCents, err := topUpInvoiceAmountCents(topUps[i].Money)
		if err != nil {
			continue
		}
		eligible = append(eligible, InvoiceEligibleOrder{
			TopUpId: topUps[i].Id, TradeNo: topUps[i].TradeNo, PaymentMethod: topUps[i].PaymentMethod, AmountCents: amountCents,
			CreateTime: topUps[i].CreateTime, CompleteTime: topUps[i].CompleteTime,
		})
	}
	total := int64(len(eligible))
	pageSize := pageInfo.GetPageSize()
	if pageSize <= 0 {
		pageSize = common.ItemsPerPage
	}
	start := max(pageInfo.GetStartIdx(), 0)
	start = min(start, len(eligible))
	end := min(start+pageSize, len(eligible))
	return eligible[start:end], total, nil
}

func normalizeInvoiceOrderIDs(orderIDs []int) ([]int, error) {
	if len(orderIDs) == 0 {
		return nil, ErrInvoiceOrderInvalid
	}
	seen := make(map[int]struct{}, len(orderIDs))
	result := make([]int, 0, len(orderIDs))
	for _, orderID := range orderIDs {
		if orderID <= 0 {
			return nil, ErrInvoiceOrderInvalid
		}
		if _, exists := seen[orderID]; exists {
			return nil, ErrInvoiceOrderInvalid
		}
		seen[orderID] = struct{}{}
		result = append(result, orderID)
	}
	slices.Sort(result)
	return result, nil
}

func createInvoiceRequest(userId int, source string, orderIDs []int, companyName, taxId, email, note string, minAmountCents int64, operator InvoiceOperator) (*InvoiceRequest, error) {
	normalizedIDs, err := normalizeInvoiceOrderIDs(orderIDs)
	if err != nil {
		return nil, err
	}
	var created InvoiceRequest
	err = DB.Transaction(func(tx *gorm.DB) error {
		topUps := make([]TopUp, 0, len(normalizedIDs))
		for idBatch := range slices.Chunk(normalizedIDs, invoiceSQLBatchSize) {
			var batch []TopUp
			if err := lockForUpdate(tx).Where("id IN ?", idBatch).Order("id").Find(&batch).Error; err != nil {
				return err
			}
			topUps = append(topUps, batch...)
		}
		if len(topUps) != len(normalizedIDs) {
			return ErrInvoiceOrderInvalid
		}
		tradeNos := make([]string, 0, len(topUps))
		for i := range topUps {
			tradeNos = append(tradeNos, topUps[i].TradeNo)
		}
		for tradeNoBatch := range slices.Chunk(tradeNos, invoiceSQLBatchSize) {
			var subscriptionCount int64
			if err := tx.Model(&SubscriptionOrder{}).Where("trade_no IN ?", tradeNoBatch).Count(&subscriptionCount).Error; err != nil {
				return err
			}
			if subscriptionCount != 0 {
				return ErrInvoiceOrderInvalid
			}
		}
		for idBatch := range slices.Chunk(normalizedIDs, invoiceSQLBatchSize) {
			var claimCount int64
			if err := tx.Model(&InvoiceOrderClaim{}).Where("top_up_id IN ?", idBatch).Count(&claimCount).Error; err != nil {
				return err
			}
			if claimCount != 0 {
				return ErrInvoiceOrderClaimed
			}
		}

		items := make([]InvoiceRequestItem, 0, len(topUps))
		claims := make([]InvoiceOrderClaim, 0, len(topUps))
		var total int64
		for i := range topUps {
			topUp := &topUps[i]
			if topUp.UserId != userId || topUp.Status != common.TopUpStatusSuccess || !isEpayInvoiceTopUp(topUp) {
				return ErrInvoiceOrderInvalid
			}
			amountCents, err := topUpInvoiceAmountCents(topUp.Money)
			if err != nil {
				return err
			}
			if amountCents > math.MaxInt64-total {
				return ErrInvoiceAmountInvalid
			}
			total += amountCents
			items = append(items, InvoiceRequestItem{
				TopUpId: topUp.Id, TradeNo: topUp.TradeNo, PaymentMethod: topUp.PaymentMethod, AmountCents: amountCents,
				CreateTime: topUp.CreateTime, CompleteTime: topUp.CompleteTime,
			})
		}
		if total < minAmountCents {
			return ErrInvoiceAmountBelowMin
		}

		now := common.GetTimestamp()
		status := InvoiceStatusPending
		processedTime := int64(0)
		if source == InvoiceSourceAdminHistory {
			status = InvoiceStatusCompleted
			processedTime = now
		}
		created = InvoiceRequest{
			UserId: userId, Source: source, Status: status,
			CompanyName: companyName, TaxId: taxId, Email: email,
			TotalAmountCents: total, CreateTime: now, ProcessedTime: processedTime,
			OperatorId: operator.Id, OperatorUsername: operator.Username, Note: note,
		}
		if err := tx.Create(&created).Error; err != nil {
			return err
		}
		for i := range items {
			items[i].RequestId = created.Id
			claims = append(claims, InvoiceOrderClaim{TopUpId: items[i].TopUpId, RequestId: created.Id})
		}
		if err := tx.CreateInBatches(&items, invoiceSQLBatchSize).Error; err != nil {
			return err
		}
		if err := tx.CreateInBatches(&claims, invoiceSQLBatchSize).Error; err != nil {
			return ErrInvoiceOrderClaimed
		}
		created.Items = items
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

func CreateUserInvoiceRequest(userId int, orderIDs []int, companyName, taxId, email string, minAmountCents int64) (*InvoiceRequest, error) {
	return createInvoiceRequest(userId, InvoiceSourceUser, orderIDs, companyName, taxId, email, "", minAmountCents, InvoiceOperator{})
}

func CreateHistoricalInvoiceRequest(userId int, orderIDs []int, note string, operator InvoiceOperator) (*InvoiceRequest, error) {
	return createInvoiceRequest(userId, InvoiceSourceAdminHistory, orderIDs, "", "", "", note, 0, operator)
}

func GetLatestUserInvoiceProfile(userId int) (InvoiceProfile, error) {
	var profile InvoiceProfile
	err := DB.Model(&InvoiceRequest{}).
		Select("company_name", "tax_id", "email").
		Where("user_id = ? AND source = ?", userId, InvoiceSourceUser).
		Order("id DESC").
		Limit(1).
		Scan(&profile).Error
	return profile, err
}

func populateInvoiceRequests(requests []InvoiceRequest) error {
	if len(requests) == 0 {
		return nil
	}
	requestIDs := make([]int, 0, len(requests))
	userIDs := make([]int, 0, len(requests))
	for i := range requests {
		requestIDs = append(requestIDs, requests[i].Id)
		userIDs = append(userIDs, requests[i].UserId)
	}
	var items []InvoiceRequestItem
	if err := DB.Where("request_id IN ?", requestIDs).Order("id").Find(&items).Error; err != nil {
		return err
	}
	topUpIDs := make([]int, 0, len(items))
	seenTopUpIDs := make(map[int]struct{}, len(items))
	for i := range items {
		if _, exists := seenTopUpIDs[items[i].TopUpId]; exists {
			continue
		}
		seenTopUpIDs[items[i].TopUpId] = struct{}{}
		topUpIDs = append(topUpIDs, items[i].TopUpId)
	}
	paymentMethods := make(map[int]string, len(topUpIDs))
	for idBatch := range slices.Chunk(topUpIDs, invoiceSQLBatchSize) {
		var topUps []TopUp
		if err := DB.Select("id", "payment_method").Where("id IN ?", idBatch).Find(&topUps).Error; err != nil {
			return err
		}
		for i := range topUps {
			paymentMethods[topUps[i].Id] = topUps[i].PaymentMethod
		}
	}
	itemsByRequest := make(map[int][]InvoiceRequestItem, len(requestIDs))
	for i := range items {
		items[i].PaymentMethod = paymentMethods[items[i].TopUpId]
		itemsByRequest[items[i].RequestId] = append(itemsByRequest[items[i].RequestId], items[i])
	}
	var users []User
	if err := DB.Unscoped().Select("id", "username", "display_name").Where("id IN ?", userIDs).Find(&users).Error; err != nil {
		return err
	}
	usersByID := make(map[int]User, len(users))
	for _, user := range users {
		usersByID[user.Id] = user
	}
	for i := range requests {
		requests[i].Items = itemsByRequest[requests[i].Id]
		if user, ok := usersByID[requests[i].UserId]; ok {
			requests[i].Username = user.Username
			requests[i].DisplayName = user.DisplayName
		}
	}
	return nil
}

func ListInvoiceRequests(userId int, status, keyword string, pageInfo *common.PageInfo) ([]InvoiceRequest, int64, error) {
	query := DB.Model(&InvoiceRequest{})
	if userId > 0 {
		query = query.Where("invoice_requests.user_id = ?", userId)
	}
	if status != "" {
		query = query.Where("invoice_requests.status = ?", status)
	}
	if strings.TrimSpace(keyword) != "" {
		pattern := invoiceSearchPattern(keyword)
		itemMatch := "EXISTS (SELECT 1 FROM invoice_request_items WHERE invoice_request_items.request_id = invoice_requests.id AND invoice_request_items.trade_no LIKE ? ESCAPE '!')"
		if userId == 0 {
			query = query.Joins("JOIN users ON users.id = invoice_requests.user_id")
			if parsedID, parseErr := strconv.Atoi(strings.TrimSpace(keyword)); parseErr == nil {
				query = query.Where("(users.id = ? OR users.username LIKE ? ESCAPE '!' OR users.display_name LIKE ? ESCAPE '!' OR users.email LIKE ? ESCAPE '!' OR invoice_requests.company_name LIKE ? ESCAPE '!' OR invoice_requests.tax_id LIKE ? ESCAPE '!' OR invoice_requests.email LIKE ? ESCAPE '!' OR "+itemMatch+")", parsedID, pattern, pattern, pattern, pattern, pattern, pattern, pattern)
			} else {
				query = query.Where("(users.username LIKE ? ESCAPE '!' OR users.display_name LIKE ? ESCAPE '!' OR users.email LIKE ? ESCAPE '!' OR invoice_requests.company_name LIKE ? ESCAPE '!' OR invoice_requests.tax_id LIKE ? ESCAPE '!' OR invoice_requests.email LIKE ? ESCAPE '!' OR "+itemMatch+")", pattern, pattern, pattern, pattern, pattern, pattern, pattern)
			}
		} else {
			query = query.Where("(invoice_requests.company_name LIKE ? ESCAPE '!' OR invoice_requests.tax_id LIKE ? ESCAPE '!' OR invoice_requests.email LIKE ? ESCAPE '!' OR "+itemMatch+")", pattern, pattern, pattern, pattern)
		}
	}
	var total int64
	if err := query.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, 0, err
	}
	pageSize := pageInfo.GetPageSize()
	if pageSize <= 0 {
		pageSize = common.ItemsPerPage
	}
	var requests []InvoiceRequest
	if err := query.Select("invoice_requests.*").Order("invoice_requests.id DESC").Limit(pageSize).Offset(max(pageInfo.GetStartIdx(), 0)).Find(&requests).Error; err != nil {
		return nil, 0, err
	}
	if err := populateInvoiceRequests(requests); err != nil {
		return nil, 0, err
	}
	return requests, total, nil
}

func GetInvoiceRequest(id int, userId int) (*InvoiceRequest, error) {
	query := DB.Where("id = ?", id)
	if userId > 0 {
		query = query.Where("user_id = ?", userId)
	}
	var request InvoiceRequest
	if err := query.First(&request).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrInvoiceRequestNotFound
		}
		return nil, err
	}
	requests := []InvoiceRequest{request}
	if err := populateInvoiceRequests(requests); err != nil {
		return nil, err
	}
	return &requests[0], nil
}

func ProcessInvoiceRequest(id int, targetStatus, rejectReason string, operator InvoiceOperator) (*InvoiceRequest, error) {
	if targetStatus != InvoiceStatusCompleted && targetStatus != InvoiceStatusRejected {
		return nil, ErrInvoiceRequestNotPending
	}
	var processed InvoiceRequest
	err := DB.Transaction(func(tx *gorm.DB) error {
		if err := lockForUpdate(tx).Where("id = ?", id).First(&processed).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrInvoiceRequestNotFound
			}
			return err
		}
		if processed.Status != InvoiceStatusPending {
			return ErrInvoiceRequestNotPending
		}
		now := common.GetTimestamp()
		updates := map[string]any{
			"status": targetStatus, "processed_time": now,
			"operator_id": operator.Id, "operator_username": operator.Username,
			"reject_reason": rejectReason,
		}
		result := tx.Model(&InvoiceRequest{}).Where("id = ? AND status = ?", id, InvoiceStatusPending).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrInvoiceRequestNotPending
		}
		if targetStatus == InvoiceStatusRejected {
			if err := tx.Where("request_id = ?", id).Delete(&InvoiceOrderClaim{}).Error; err != nil {
				return err
			}
		}
		processed.Status = targetStatus
		processed.ProcessedTime = now
		processed.OperatorId = operator.Id
		processed.OperatorUsername = operator.Username
		processed.RejectReason = rejectReason
		return nil
	})
	if err != nil {
		return nil, err
	}
	return GetInvoiceRequest(processed.Id, 0)
}
