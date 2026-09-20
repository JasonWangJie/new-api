package model

import (
	"fmt"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func createInvoiceTestTopUp(t *testing.T, userID int, tradeNo string, money float64) TopUp {
	t.Helper()
	topUp := TopUp{
		UserId:          userID,
		TradeNo:         tradeNo,
		Money:           money,
		PaymentProvider: PaymentProviderEpay,
		PaymentMethod:   "alipay",
		Status:          common.TopUpStatusSuccess,
		CreateTime:      100,
		CompleteTime:    200,
	}
	require.NoError(t, DB.Create(&topUp).Error)
	return topUp
}

func TestInvoiceMigrationSQLite(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	var sqliteVersion string
	require.NoError(t, db.Raw("SELECT sqlite_version()").Scan(&sqliteVersion).Error)
	t.Logf("SQLite version: %s", sqliteVersion)
	require.NoError(t, db.AutoMigrate(&TopUp{}, &SubscriptionOrder{}))

	legacy := TopUp{
		UserId: 71, TradeNo: "migration-preserved", Money: 12.34,
		PaymentProvider: PaymentProviderEpay, Status: common.TopUpStatusSuccess,
	}
	require.NoError(t, db.Create(&legacy).Error)
	for range 2 {
		require.NoError(t, db.AutoMigrate(
			&InvoiceRequest{},
			&InvoiceRequestItem{},
			&InvoiceOrderClaim{},
		))
	}

	var preserved TopUp
	require.NoError(t, db.Where("id = ?", legacy.Id).First(&preserved).Error)
	assert.Equal(t, legacy.TradeNo, preserved.TradeNo)
	require.NoError(t, db.Create(&InvoiceOrderClaim{TopUpId: legacy.Id, RequestId: 1}).Error)
	require.Error(t, db.Create(&InvoiceOrderClaim{TopUpId: legacy.Id, RequestId: 2}).Error)
}

func TestInvoiceWorkflow(t *testing.T) {
	t.Run("eligible orders include verified legacy Epay and exclude unknown or subscription orders", func(t *testing.T) {
		truncateTables(t)
		explicit := createInvoiceTestTopUp(t, 61, "invoice-explicit", 10)
		legacy := createInvoiceTestTopUp(t, 61, "USR61NOabc1231700000000", 20)
		require.NoError(t, DB.Model(&legacy).Update("payment_provider", "").Error)
		invalidLegacy := createInvoiceTestTopUp(t, 61, "USR61NOabc-231700000000", 30)
		require.NoError(t, DB.Model(&invalidLegacy).Update("payment_provider", "").Error)
		unknown := createInvoiceTestTopUp(t, 61, "old-order-without-source", 40)
		require.NoError(t, DB.Model(&unknown).Update("payment_provider", "").Error)
		subscription := createInvoiceTestTopUp(t, 61, "invoice-subscription", 50)
		require.NoError(t, DB.Create(&SubscriptionOrder{
			UserId: 61, TradeNo: subscription.TradeNo, PaymentProvider: PaymentProviderEpay,
			Status: common.TopUpStatusSuccess,
		}).Error)

		firstPage, total, err := ListInvoiceEligibleOrders(61, "", &common.PageInfo{Page: 1, PageSize: 1})
		require.NoError(t, err)
		assert.EqualValues(t, 2, total)
		require.Len(t, firstPage, 1)
		assert.Equal(t, legacy.Id, firstPage[0].TopUpId)

		secondPage, total, err := ListInvoiceEligibleOrders(61, "", &common.PageInfo{Page: 2, PageSize: 1})
		require.NoError(t, err)
		assert.EqualValues(t, 2, total)
		require.Len(t, secondPage, 1)
		assert.Equal(t, explicit.Id, secondPage[0].TopUpId)
	})

	t.Run("amount threshold uses stored two decimal payment amount", func(t *testing.T) {
		truncateTables(t)
		order := createInvoiceTestTopUp(t, 11, "invoice-threshold", 10.126)

		_, err := CreateUserInvoiceRequest(11, []int{order.Id}, "ACME", "TAX-1", "billing@example.com", 1014)
		require.ErrorIs(t, err, ErrInvoiceAmountBelowMin)

		created, err := CreateUserInvoiceRequest(11, []int{order.Id}, "ACME", "TAX-1", "billing@example.com", 1013)
		require.NoError(t, err)
		assert.EqualValues(t, 1013, created.TotalAmountCents)
		require.Len(t, created.Items, 1)
		assert.EqualValues(t, 1013, created.Items[0].AmountCents)
	})

	t.Run("large cross-page selections are processed in database-safe batches", func(t *testing.T) {
		truncateTables(t)
		topUps := make([]TopUp, 101)
		for i := range topUps {
			topUps[i] = TopUp{
				UserId: 12, TradeNo: fmt.Sprintf("invoice-batch-%03d", i), Money: 1,
				PaymentProvider: PaymentProviderEpay, PaymentMethod: "alipay",
				Status: common.TopUpStatusSuccess, CreateTime: 100, CompleteTime: 200,
			}
		}
		require.NoError(t, DB.CreateInBatches(&topUps, invoiceSQLBatchSize).Error)
		orderIDs := make([]int, len(topUps))
		for i := range topUps {
			orderIDs[i] = topUps[i].Id
		}

		created, err := CreateUserInvoiceRequest(12, orderIDs, "Batch", "TAX-BATCH", "batch@example.com", 10100)
		require.NoError(t, err)
		assert.EqualValues(t, 10100, created.TotalAmountCents)
		assert.Len(t, created.Items, len(topUps))
	})

	t.Run("orders cannot be claimed across users or requests", func(t *testing.T) {
		truncateTables(t)
		order := createInvoiceTestTopUp(t, 21, "invoice-owner", 20)

		_, err := CreateUserInvoiceRequest(22, []int{order.Id}, "Other", "TAX-2", "other@example.com", 0)
		require.ErrorIs(t, err, ErrInvoiceOrderInvalid)

		_, err = CreateUserInvoiceRequest(21, []int{order.Id}, "Owner", "TAX-3", "owner@example.com", 0)
		require.NoError(t, err)
		_, err = CreateHistoricalInvoiceRequest(21, []int{order.Id}, "duplicate", InvoiceOperator{Id: 9, Username: "admin"})
		require.ErrorIs(t, err, ErrInvoiceOrderClaimed)
	})

	t.Run("rejection releases claims and preserves the old record", func(t *testing.T) {
		truncateTables(t)
		order := createInvoiceTestTopUp(t, 31, "invoice-retry", 30)
		first, err := CreateUserInvoiceRequest(31, []int{order.Id}, "First", "TAX-4", "first@example.com", 0)
		require.NoError(t, err)

		rejected, err := ProcessInvoiceRequest(first.Id, InvoiceStatusRejected, "details do not match", InvoiceOperator{Id: 7, Username: "reviewer"})
		require.NoError(t, err)
		assert.Equal(t, InvoiceStatusRejected, rejected.Status)
		assert.Equal(t, "details do not match", rejected.RejectReason)
		assert.Equal(t, 7, rejected.OperatorId)
		assert.Equal(t, "reviewer", rejected.OperatorUsername)
		assert.NotZero(t, rejected.ProcessedTime)

		second, err := CreateUserInvoiceRequest(31, []int{order.Id}, "Second", "TAX-5", "second@example.com", 0)
		require.NoError(t, err)
		assert.NotEqual(t, first.Id, second.Id)

		storedFirst, err := GetInvoiceRequest(first.Id, 31)
		require.NoError(t, err)
		assert.Equal(t, InvoiceStatusRejected, storedFirst.Status)
		assert.Equal(t, "details do not match", storedFirst.RejectReason)
	})

	t.Run("historical supplement bypasses threshold and records operator", func(t *testing.T) {
		truncateTables(t)
		order := createInvoiceTestTopUp(t, 41, "invoice-history", 0.01)

		created, err := CreateHistoricalInvoiceRequest(41, []int{order.Id}, "paper invoice", InvoiceOperator{Id: 8, Username: "operator"})
		require.NoError(t, err)
		assert.Equal(t, InvoiceSourceAdminHistory, created.Source)
		assert.Equal(t, InvoiceStatusCompleted, created.Status)
		assert.Equal(t, "paper invoice", created.Note)
		assert.Equal(t, 8, created.OperatorId)
		assert.Equal(t, "operator", created.OperatorUsername)
		assert.NotZero(t, created.ProcessedTime)

		_, err = CreateUserInvoiceRequest(41, []int{order.Id}, "Again", "TAX-6", "again@example.com", 0)
		require.ErrorIs(t, err, ErrInvoiceOrderClaimed)
	})

	t.Run("only one concurrent request can occupy an order", func(t *testing.T) {
		truncateTables(t)
		order := createInvoiceTestTopUp(t, 51, "invoice-concurrent", 50)

		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for range 2 {
			workers.Go(func() {
				<-start
				_, err := CreateUserInvoiceRequest(51, []int{order.Id}, "Concurrent", "TAX-7", "concurrent@example.com", 0)
				results <- err
			})
		}
		close(start)
		workers.Wait()
		close(results)

		var successCount int
		var claimedCount int
		for err := range results {
			switch {
			case err == nil:
				successCount++
			case assert.ErrorIs(t, err, ErrInvoiceOrderClaimed):
				claimedCount++
			}
		}
		assert.Equal(t, 1, successCount)
		assert.Equal(t, 1, claimedCount)
	})

	t.Run("only one reviewer can transition a pending request", func(t *testing.T) {
		truncateTables(t)
		order := createInvoiceTestTopUp(t, 81, "invoice-review-race", 80)
		created, err := CreateUserInvoiceRequest(81, []int{order.Id}, "Review", "TAX-8", "review@example.com", 0)
		require.NoError(t, err)

		start := make(chan struct{})
		results := make(chan error, 2)
		var workers sync.WaitGroup
		for _, status := range []string{InvoiceStatusCompleted, InvoiceStatusRejected} {
			workers.Go(func() {
				<-start
				_, processErr := ProcessInvoiceRequest(created.Id, status, "review decision", InvoiceOperator{Id: 10, Username: "reviewer"})
				results <- processErr
			})
		}
		close(start)
		workers.Wait()
		close(results)

		var successCount int
		var conflictCount int
		for processErr := range results {
			switch {
			case processErr == nil:
				successCount++
			case assert.ErrorIs(t, processErr, ErrInvoiceRequestNotPending):
				conflictCount++
			}
		}
		assert.Equal(t, 1, successCount)
		assert.Equal(t, 1, conflictCount)
	})
}
