package controller

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"

	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupManageUserTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType, previousLogDatabaseType := common.MainDatabaseType(), common.LogDatabaseType()
	dialect := os.Getenv("TEST_MANAGE_USER_DIALECT")
	if dialect == "" {
		dialect = "sqlite"
	}
	databaseTypes := map[string]common.DatabaseType{
		"sqlite": common.DatabaseTypeSQLite, "mysql": common.DatabaseTypeMySQL, "postgres": common.DatabaseTypePostgreSQL,
	}
	require.Contains(t, databaseTypes, dialect)
	dsn := os.Getenv("TEST_" + strings.ToUpper(dialect) + "_DSN")
	db, _ := newAuditTestDatabase(t, dialect, dsn)
	logDB := db
	if os.Getenv("TEST_MANAGE_USER_SEPARATE_LOG_DB") == "1" {
		logDB, _ = newAuditTestDatabase(t, dialect, dsn)
	}
	model.DB, model.LOG_DB = db, logDB
	common.RedisEnabled = false
	common.SetDatabaseTypes(databaseTypes[dialect], databaseTypes[dialect])

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		sqlDB, err := db.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
		if logDB != db {
			sqlLogDB, err := logDB.DB()
			if err == nil {
				_ = sqlLogDB.Close()
			}
		}
	})
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.UserSession{}, &model.CasbinRule{}, &model.AuthzRole{}, &model.TopUp{}))
	require.NoError(t, logDB.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	versionQuery := "SELECT version()"
	if dialect == "sqlite" {
		versionQuery = "SELECT sqlite_version()"
	}
	var version string
	require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
	t.Logf("database: %s %s, separate log database: %v", dialect, version, logDB != db)
	return db
}

func performManageUserRequest(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/manage", strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 9999)
	c.Set("role", common.RoleRootUser)
	c.Set("username", "root-operator")
	c.Set(common.RequestIdKey, "quota-test-request")
	ManageUser(c)
	return recorder
}

func TestManageUserDisableAdvancesAuthVersionOnceAndRevokesSession(t *testing.T) {
	db := setupManageUserTestDB(t)
	now := time.Now().Unix()
	user := model.User{
		Username: "managed-disable-user", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1,
	}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&model.UserSession{
		SID: "managed-disable-session", UserID: user.Id, Version: 1, UserAuthVersion: 1,
		Status: model.UserSessionStatusActive, RefreshHash: "refresh-hash", LoginMethod: "password",
		LastActiveAt: now, ExpiresAt: now + 3600,
	}).Error)

	recorder := performManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"disable"}`, user.Id))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":true`)

	var updated model.User
	require.NoError(t, db.First(&updated, user.Id).Error)
	assert.Equal(t, common.UserStatusDisabled, updated.Status)
	assert.EqualValues(t, 2, updated.AuthVersion)
	var session model.UserSession
	require.NoError(t, db.First(&session, "sid = ?", "managed-disable-session").Error)
	assert.Equal(t, model.UserSessionStatusRevoked, session.Status)
}

func TestManageUserDemoteAdvancesAuthVersionAndRevokesSessionsOnce(t *testing.T) {
	db := setupManageUserTestDB(t)
	previousMaster := common.IsMasterNode
	common.IsMasterNode = false
	t.Cleanup(func() { common.IsMasterNode = previousMaster })
	require.NoError(t, authz.Init(db))

	now := time.Now().Unix()
	user := model.User{
		Username: "managed-demote-user", Password: "password", Role: common.RoleAdminUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1,
	}
	require.NoError(t, db.Create(&user).Error)
	for _, sid := range []string{"managed-demote-session-one", "managed-demote-session-two"} {
		require.NoError(t, db.Create(&model.UserSession{
			SID: sid, UserID: user.Id, Version: 1, UserAuthVersion: 1,
			Status: model.UserSessionStatusActive, RefreshHash: "refresh-" + sid, LoginMethod: "password",
			LastActiveAt: now, ExpiresAt: now + 3600,
		}).Error)
	}

	sessionUpdateCount := 0
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:count_demote_session_updates", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "user_sessions" {
			sessionUpdateCount++
		}
	}))

	recorder := performManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"demote"}`, user.Id))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.Contains(t, recorder.Body.String(), `"success":true`)

	var updated model.User
	require.NoError(t, db.First(&updated, user.Id).Error)
	assert.Equal(t, common.RoleCommonUser, updated.Role)
	assert.EqualValues(t, 2, updated.AuthVersion)
	var sessions []model.UserSession
	require.NoError(t, db.Where("user_id = ?", user.Id).Order("sid asc").Find(&sessions).Error)
	require.Len(t, sessions, 2)
	for _, session := range sessions {
		assert.Equal(t, model.UserSessionStatusRevoked, session.Status)
		assert.Equal(t, "admin_demote", session.RevokedReason)
	}
	assert.Equal(t, 1, sessionUpdateCount)
}

func TestManageUserDeleteReturnsImmediatelyAndUnknownActionFails(t *testing.T) {
	db := setupManageUserTestDB(t)
	deleted := model.User{
		Username: "managed-delete-user", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "delete-aff",
	}
	require.NoError(t, db.Create(&deleted).Error)

	recorder := performManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"delete"}`, deleted.Id))
	assert.Contains(t, recorder.Body.String(), `"success":true`)
	var deletedCount int64
	require.NoError(t, db.Unscoped().Model(&model.User{}).Where("id = ? AND deleted_at IS NOT NULL", deleted.Id).Count(&deletedCount).Error)
	assert.EqualValues(t, 1, deletedCount)

	unchanged := model.User{
		Username: "managed-unknown-user", Password: "password", Role: common.RoleCommonUser,
		Status: common.UserStatusEnabled, Group: "default", AuthVersion: 1, AffCode: "unknown-aff",
	}
	require.NoError(t, db.Create(&unchanged).Error)
	recorder = performManageUserRequest(t, fmt.Sprintf(`{"id":%d,"action":"unknown"}`, unchanged.Id))
	assert.Contains(t, recorder.Body.String(), `"success":false`)
	require.NoError(t, db.First(&unchanged, unchanged.Id).Error)
	assert.EqualValues(t, 1, unchanged.AuthVersion)
	assert.Equal(t, common.UserStatusEnabled, unchanged.Status)
}

func createQuotaTestOperator(t *testing.T, db *gorm.DB, role int) model.User {
	t.Helper()
	if role == 0 {
		role = common.RoleRootUser
	}
	operator := model.User{Id: 9999, Username: "root-operator", Role: role, Status: common.UserStatusEnabled, AuthVersion: 1, AffCode: "root-operator-aff"}
	require.NoError(t, db.Create(&operator).Error)
	return operator
}

func quotaAdjustmentRequest(t *testing.T, id int, mode string, value, before int) string {
	t.Helper()
	req := ManageRequest{Id: id, Action: "add_quota", Mode: mode, Value: value}
	delta := int64(0)
	switch mode {
	case "add":
		delta = int64(value)
	case "override":
		delta = int64(value) - int64(before)
	}
	if delta > 0 {
		cents := int64(0)
		req.PaymentAmountCents = &cents
		req.IdempotencyKey = uuid.NewString()
		req.ExpectedDeltaQuota = strconv.FormatInt(delta, 10)
	}
	encoded, err := common.Marshal(req)
	require.NoError(t, err)
	return string(encoded)
}

func TestManualQuotaOrders(t *testing.T) {
	t.Run("manual_payment_method_is_not_an_online_checkout", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		confirmPaymentComplianceForTest(t)
		oldMethods := operation_setting.PayMethods
		oldPrice, oldMinimum := operation_setting.Price, operation_setting.MinTopUp
		operation_setting.PayMethods = []map[string]string{{"type": model.PaymentMethodManualBackend, "name": "manual"}, {"type": "alipay", "name": "Alipay"}}
		operation_setting.Price = 7.3
		operation_setting.MinTopUp = 1
		t.Cleanup(func() {
			operation_setting.PayMethods = oldMethods
			operation_setting.Price = oldPrice
			operation_setting.MinTopUp = oldMinimum
		})
		user := model.User{Username: "manual-checkout", Group: "default", Quota: 1000}
		require.NoError(t, db.Create(&user).Error)
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/api/user/topup/info", nil)
		GetTopUpInfo(ctx)
		assert.NotContains(t, recorder.Body.String(), model.PaymentMethodManualBackend)
		recorder = httptest.NewRecorder()
		ctx, _ = gin.CreateTestContext(recorder)
		ctx.Set("id", user.Id)
		ctx.Request = httptest.NewRequest(http.MethodPost, "/api/user/epay", strings.NewReader(`{"amount":1,"payment_method":"manual_backend"}`))
		RequestEpay(ctx)
		assert.Contains(t, recorder.Body.String(), `"message":"error"`)
		assert.Contains(t, recorder.Body.String(), "支付方式不存在")
		var count int64
		require.NoError(t, db.Model(&model.TopUp{}).Count(&count).Error)
		assert.Zero(t, count)
	})
	for _, mode := range []string{"add", "override"} {
		t.Run(mode+"_paid_order_replay_and_conflict", func(t *testing.T) {
			db := setupManageUserTestDB(t)
			createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "manual-order", Group: "default", Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			value := 250000
			if mode == "override" {
				value += user.Quota
			}
			body := quotaAdjustmentRequest(t, user.Id, mode, value, user.Quota)
			var req ManageRequest
			require.NoError(t, common.UnmarshalJsonStr(body, &req))
			cents := int64(1234)
			req.PaymentAmountCents = &cents
			encoded, err := common.Marshal(req)
			require.NoError(t, err)
			body = string(encoded)
			server := miniredis.RunT(t)
			oldRDB := common.RDB
			common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
			common.RedisEnabled = true
			t.Cleanup(func() { _ = common.RDB.Close(); common.RDB = oldRDB })
			_, err = model.GetUserCache(user.Id)
			require.NoError(t, err)
			var quotaKey string
			for _, key := range server.Keys() {
				if server.HGet(key, "Id") == strconv.Itoa(user.Id) {
					quotaKey = key
					break
				}
			}
			require.NotEmpty(t, quotaKey)
			server.HSet(quotaKey, "Quota", "900")
			first := performManageUserRequest(t, body)
			require.Equal(t, http.StatusOK, first.Code)
			require.Contains(t, first.Body.String(), `"success":true`)
			assert.NotContains(t, first.Body.String(), "operator_username")
			var order model.TopUp
			require.NoError(t, db.Where("user_id = ?", user.Id).First(&order).Error)
			assert.Equal(t, model.PaymentMethodManualBackend, order.PaymentMethod)
			assert.Equal(t, model.PaymentProviderManualBackend, order.PaymentProvider)
			assert.Equal(t, common.TopUpStatusSuccess, order.Status)
			assert.EqualValues(t, 250000, order.Amount)
			assert.Equal(t, "250000", order.CreditedQuota)
			assert.Equal(t, "0.5", order.CreditedAmount)
			assert.Equal(t, &cents, order.PaymentAmountCents)
			assert.Equal(t, 12.34, order.Money)
			assert.Equal(t, order.CreateTime, order.CompleteTime)
			orders, _, err := model.GetUserTopUps(user.Id, &common.PageInfo{Page: 1, PageSize: 20})
			require.NoError(t, err)
			require.Len(t, orders, 1)
			public, err := common.Marshal(orders)
			require.NoError(t, err)
			assert.NotContains(t, string(public), "root-operator")
			assert.NotContains(t, string(public), "manual_adjustment")
			assert.NotContains(t, string(public), req.IdempotencyKey)
			assert.Contains(t, string(public), `"credited_amount":"0.5"`)
			// A replay returns the original order even after another balance change.
			require.NoError(t, db.Model(&user).Update("quota", 999).Error)
			second := performManageUserRequest(t, body)
			require.Contains(t, second.Body.String(), `"replayed":true`)
			assert.Contains(t, second.Body.String(), order.TradeNo)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 999, user.Quota)
			assert.Equal(t, "250900", server.HGet(quotaKey, "Quota"), "replays must preserve reservations without a second increment")
			var orderCount, logCount int64
			require.NoError(t, db.Model(&model.TopUp{}).Count(&orderCount).Error)
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logCount).Error)
			assert.EqualValues(t, 1, orderCount)
			assert.EqualValues(t, 1, logCount)
			cents++
			encoded, err = common.Marshal(req)
			require.NoError(t, err)
			conflict := performManageUserRequest(t, string(encoded))
			assert.Equal(t, http.StatusConflict, conflict.Code)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 999, user.Quota)
			_, err = model.RechargeEpay(order.TradeNo, "alipay", "")
			assert.ErrorIs(t, err, model.ErrPaymentMethodMismatch)
			require.NoError(t, model.ManualCompleteTopUp(order.TradeNo, ""))
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logCount).Error)
			assert.EqualValues(t, 1, logCount, "a successful order cannot emit another financial log")
			require.NoError(t, db.Model(&order).Update("status", common.TopUpStatusPending).Error)
			assert.ErrorIs(t, model.ManualCompleteTopUp(order.TradeNo, ""), model.ErrPaymentMethodMismatch)
		})
	}
	t.Run("missing_confirmation_and_stale_override_leave_no_effect", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		createQuotaTestOperator(t, db, common.RoleRootUser)
		user := model.User{Username: "manual-stale", Quota: 1000}
		require.NoError(t, db.Create(&user).Error)
		for _, body := range []string{
			fmt.Sprintf(`{"id":%d,"action":"add_quota","mode":"add","value":500}`, user.Id),
			strings.Replace(quotaAdjustmentRequest(t, user.Id, "add", 500, 1000), `"payment_amount_cents":0`, `"payment_amount_cents":null`, 1),
			strings.Replace(quotaAdjustmentRequest(t, user.Id, "add", 500, 1000), `"payment_amount_cents":0`, `"payment_amount_cents":-1`, 1),
		} {
			response := performManageUserRequest(t, body)
			assert.Equal(t, http.StatusBadRequest, response.Code)
		}
		body := quotaAdjustmentRequest(t, user.Id, "override", 1500, 1000)
		for _, balance := range []int{1100, 1600} {
			require.NoError(t, db.Model(&user).Update("quota", balance).Error)
			response := performManageUserRequest(t, body)
			assert.Equal(t, http.StatusConflict, response.Code)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, balance, user.Quota)
		}
		var count int64
		require.NoError(t, db.Model(&model.TopUp{}).Count(&count).Error)
		assert.Zero(t, count)
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&count).Error)
		assert.Zero(t, count)
	})
	t.Run("large_override_and_actor_user_idempotency_scope", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		createQuotaTestOperator(t, db, common.RoleRootUser)
		user := model.User{Username: "manual-limit", Quota: -common.MaxWalletQuota}
		require.NoError(t, db.Create(&user).Error)
		body := quotaAdjustmentRequest(t, user.Id, "override", common.MaxWalletQuota, user.Quota)
		response := performManageUserRequest(t, body)
		require.Contains(t, response.Body.String(), `"success":true`)
		var order model.TopUp
		require.NoError(t, db.Where("user_id = ?", user.Id).First(&order).Error)
		assert.Equal(t, strconv.FormatInt(2*int64(common.MaxWalletQuota), 10), order.CreditedQuota)
		assert.EqualValues(t, 2*int64(common.MaxWalletQuota), order.Amount)
		key := uuid.NewString()
		zero := int64(0)
		for _, username := range []string{"scope-user-a", "scope-user-b"} {
			target := model.User{Username: username, Quota: 1000, AffCode: username}
			require.NoError(t, db.Create(&target).Error)
			for _, actor := range []int{9999, 9998} {
				input := &model.ManualQuotaOrderInput{OperatorID: actor, PaymentAmountCents: &zero, IdempotencyKey: key, ExpectedDeltaQuota: "500"}
				result, err := model.AdjustUserQuota(target.Id, common.RoleRootUser, "add", 500, input)
				require.NoError(t, err)
				assert.False(t, result.Replayed)
				replay, err := model.AdjustUserQuota(target.Id, common.RoleRootUser, "add", 500, input)
				require.NoError(t, err)
				assert.True(t, replay.Replayed)
				assert.Equal(t, result.Order.Id, replay.Order.Id)
			}
			require.NoError(t, db.First(&target, target.Id).Error)
			assert.Equal(t, 2000, target.Quota)
		}
		var count int64
		require.NoError(t, db.Model(&model.TopUp{}).Count(&count).Error)
		assert.EqualValues(t, 5, count)
	})
	t.Run("preview_enforces_admin_and_target_role", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		target := model.User{Username: "manual-role", Role: common.RoleAdminUser, Quota: 1000}
		require.NoError(t, db.Create(&target).Error)
		for _, role := range []int{common.RoleCommonUser, common.RoleAdminUser} {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Set("role", role)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/user/quota/preview", strings.NewReader(fmt.Sprintf(`{"id":%d,"mode":"add","value":100}`, target.Id)))
			PreviewUserQuota(ctx)
			assert.Equal(t, http.StatusForbidden, recorder.Code)
			assert.NotContains(t, recorder.Body.String(), `"before"`)
		}
		router := gin.New()
		router.POST("/api/user/quota/preview", middleware.AdminAuth(), PreviewUserQuota)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/user/quota/preview", strings.NewReader(fmt.Sprintf(`{"id":%d,"mode":"add","value":100}`, target.Id)))
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusUnauthorized, recorder.Code)
		assert.NotContains(t, recorder.Body.String(), `"before"`)
	})
	t.Run("order_insert_failure_rolls_back_credit", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		createQuotaTestOperator(t, db, common.RoleRootUser)
		user := model.User{Username: "manual-rollback", Quota: 1000}
		require.NoError(t, db.Create(&user).Error)
		require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:fail_manual_order", func(tx *gorm.DB) {
			if tx.Statement.Table == "top_ups" {
				tx.AddError(errors.New("order unavailable"))
			}
		}))
		response := performManageUserRequest(t, quotaAdjustmentRequest(t, user.Id, "add", 500, 1000))
		assert.Contains(t, response.Body.String(), `"success":false`)
		require.NoError(t, db.First(&user, user.Id).Error)
		assert.Equal(t, 1000, user.Quota)
		var count int64
		require.NoError(t, db.Model(&model.TopUp{}).Count(&count).Error)
		assert.Zero(t, count)
	})
	t.Run("preview_uses_target_pricing_and_exact_discount_match", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		user := model.User{Username: "manual-preview", Group: "vip", Quota: 1000}
		require.NoError(t, db.Create(&user).Error)
		oldRatios, oldPrice, oldUnit := common.TopupGroupRatio2JSONString(), operation_setting.Price, common.QuotaPerUnit
		oldDiscount, oldDisplay := operation_setting.GetPaymentSetting().AmountDiscount, operation_setting.GetGeneralSetting().QuotaDisplayType
		t.Cleanup(func() {
			require.NoError(t, common.UpdateTopupGroupRatioByJSONString(oldRatios))
			operation_setting.Price = oldPrice
			common.QuotaPerUnit = oldUnit
			operation_setting.GetPaymentSetting().AmountDiscount = oldDiscount
			operation_setting.GetGeneralSetting().QuotaDisplayType = oldDisplay
		})
		require.NoError(t, common.UpdateTopupGroupRatioByJSONString(`{"vip":2,"default":1}`))
		operation_setting.Price = 7.3
		common.QuotaPerUnit = 500000
		operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{1: 0.5}
		operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeUSD
		for _, tc := range []struct {
			quota int
			cents int64
		}{{250000, 730}, {500000, 730}, {750000, 2190}} {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Set("role", common.RoleRootUser)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/api/user/quota/preview", strings.NewReader(fmt.Sprintf(`{"id":%d,"mode":"add","value":%d}`, user.Id, tc.quota)))
			PreviewUserQuota(ctx)
			var response struct {
				Success bool
				Data    struct {
					SuggestedPaymentAmountCents int64  `json:"suggested_payment_amount_cents"`
					DeltaQuota                  string `json:"delta_quota"`
				}
			}
			require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
			assert.True(t, response.Success)
			assert.Equal(t, tc.cents, response.Data.SuggestedPaymentAmountCents)
			assert.Equal(t, strconv.Itoa(tc.quota), response.Data.DeltaQuota)
		}
		operation_setting.GetGeneralSetting().QuotaDisplayType = operation_setting.QuotaDisplayTypeTokens
		operation_setting.GetPaymentSetting().AmountDiscount = map[int]float64{250000: 0.5}
		cents, err := manualQuotaPaymentQuote(250000, "vip")
		require.NoError(t, err)
		assert.EqualValues(t, 365, cents)
		require.NoError(t, db.First(&user, user.Id).Error)
		assert.Equal(t, 1000, user.Quota)
		var count int64
		require.NoError(t, db.Model(&model.TopUp{}).Count(&count).Error)
		assert.Zero(t, count)
	})
}

func TestManageUserQuotaRecordsTopupAndAudit(t *testing.T) {
	for _, tc := range []struct {
		name, mode, action, content string
		value, wantQuota            int
	}{
		{"add", "add", "user.quota_add", "Increased user quota by 500", 500, 1500},
		{"subtract", "subtract", "user.quota_subtract", "Decreased user quota by 500", 500, 500},
		{"override_up", "override", "user.quota_override", "Overrode user quota from 1000 to 1500", 1500, 1500},
		{"override_down", "override", "user.quota_override", "Overrode user quota from 1000 to 500", 500, 500},
		{"override_unchanged", "override", "user.quota_override", "Overrode user quota from 1000 to 1000", 1000, 1000},
		{"override_zero", "override", "user.quota_override", "Overrode user quota from 1000 to 0", 0, 0},
		{"override_negative", "override", "user.quota_override", "Overrode user quota from 1000 to -1", -1, -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			user := model.User{Username: "quota-owner", Role: common.RoleCommonUser, Quota: 1000, AffCode: "quota-owner-aff"}
			require.NoError(t, db.Create(&user).Error)
			createQuotaTestOperator(t, db, common.RoleRootUser)

			recorder := performManageUserRequest(t, quotaAdjustmentRequest(t, user.Id, tc.mode, tc.value, user.Quota))
			assert.Equal(t, http.StatusOK, recorder.Code)
			require.Contains(t, recorder.Body.String(), `"success":true`)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, tc.wantQuota, user.Quota)
			var orderCount int64
			require.NoError(t, db.Model(&model.TopUp{}).Count(&orderCount).Error)
			if tc.wantQuota > 1000 {
				assert.EqualValues(t, 1, orderCount)
			} else {
				assert.Zero(t, orderCount)
			}

			logs, total, err := model.GetAllLogs(model.LogTypeTopup, 0, 0, "", "", "", 0, 20, 0, "", "", "")
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			require.Len(t, logs, 1)
			assert.Equal(t, user.Id, logs[0].UserId)
			assert.Equal(t, user.Username, logs[0].Username)
			assert.Equal(t, tc.content, logs[0].Content)
			model.FormatAdminLogs(logs)
			var other model.AuditOther
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			assert.Equal(t, &model.AuditAdminInfo{AdminID: 9999, AdminUsername: "root-operator", AdminRole: common.RoleRootUser, AuthMethod: "session"}, other.AdminInfo)

			logs, total, err = model.GetUserLogs(user.Id, model.LogTypeTopup, 0, 0, "", "", 0, 20, "", "", "")
			require.NoError(t, err)
			assert.EqualValues(t, 1, total)
			require.Len(t, logs, 1)
			other = model.AuditOther{}
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			assert.Nil(t, other.AdminInfo)
			require.NotNil(t, other.Op)
			assert.Equal(t, tc.action, other.Op.Action)
			params, err := common.Marshal(other.Op.Params)
			require.NoError(t, err)
			expectedParams := model.AuditFields{"target_user_id": user.Id, "target_username": user.Username, "mode": tc.mode, "requested_quota": tc.value, "from": 1000, "to": tc.wantQuota}
			if tc.mode != "override" {
				expectedParams["quota"] = tc.value
			}
			if tc.wantQuota > 1000 {
				var order model.TopUp
				require.NoError(t, db.Where("user_id = ?", user.Id).First(&order).Error)
				expectedParams["order_id"] = order.Id
				expectedParams["trade_no"] = order.TradeNo
				expectedParams["payment_amount_cents"] = 0
				expectedParams["payment_method"] = model.PaymentMethodManualBackend
			}
			expected, err := common.Marshal(expectedParams)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(params))
			assert.Equal(t, "quota-test-request", logs[0].RequestId)
			assert.Empty(t, logs[0].Ip, "recipient logs must not disclose the administrator IP")

			logs, total, err = model.GetUserLogs(9999, model.LogTypeTopup, 0, 0, "", "", 0, 20, "", "", "")
			require.NoError(t, err)
			assert.Zero(t, total)
			assert.Empty(t, logs)
			var audits []model.AuditLog
			require.NoError(t, model.LOG_DB.Find(&audits).Error)
			require.Len(t, audits, 1)
			assert.Equal(t, 9999, audits[0].UserId)
			assert.Equal(t, "root-operator", audits[0].Username)
			assert.Equal(t, tc.action, audits[0].Action)
			assert.True(t, audits[0].Success)
			params, err = common.Marshal(audits[0].Other.Op.Params)
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(params))
			assert.Equal(t, "quota-test-request", audits[0].RequestId)
		})
	}
}

func TestManageUserQuotaFailuresDoNotRecordTopup(t *testing.T) {
	for _, tc := range []struct {
		name, mode string
		value      int
		failUpdate bool
	}{
		{"zero_add", "add", 0, false},
		{"negative_subtract", "subtract", -1, false},
		{"invalid_mode", "invalid", 500, false},
		{"add_update_error", "add", 500, true},
		{"subtract_update_error", "subtract", 500, true},
		{"override_update_error", "override", 500, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "quota-owner", Role: common.RoleCommonUser, Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			if tc.failUpdate {
				require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:fail_quota_update", func(tx *gorm.DB) {
					if tx.Statement.Table == "users" {
						tx.AddError(errors.New("quota update unavailable"))
					}
				}))
			}
			recorder := performManageUserRequest(t, quotaAdjustmentRequest(t, user.Id, tc.mode, tc.value, user.Quota))
			assert.Contains(t, recorder.Body.String(), `"success":false`)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 1000, user.Quota)
			var orderCount int64
			require.NoError(t, db.Model(&model.TopUp{}).Count(&orderCount).Error)
			assert.Zero(t, orderCount, "a failed balance update rolls the order back too")
			var logCount, auditCount int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logCount).Error)
			require.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).Count(&auditCount).Error)
			assert.Zero(t, logCount)
			assert.EqualValues(t, 1, auditCount)
			var audit model.AuditLog
			require.NoError(t, model.LOG_DB.First(&audit).Error)
			assert.False(t, audit.Success)
			params, err := common.Marshal(audit.Other.Op.Params)
			require.NoError(t, err)
			assert.NotContains(t, string(params), `"from"`)
			assert.NotContains(t, string(params), `"to"`)
			assert.NotContains(t, string(params), `"target_username"`)
			assert.NotContains(t, string(params), "quota update unavailable")
			reason := "invalid_parameters"
			if tc.failUpdate {
				reason = "database_error"
			}
			assert.Contains(t, string(params), `"failure_reason":"`+reason+`"`)
			assert.Contains(t, string(params), fmt.Sprintf(`"requested_quota":%d`, tc.value))
		})
	}
}

func TestManageUserQuotaLogFailureKeepsSuccessfulAdjustment(t *testing.T) {
	for _, failedTable := range []string{"logs", "audit_logs"} {
		t.Run(failedTable, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			createQuotaTestOperator(t, db, common.RoleRootUser)
			user := model.User{Username: "quota-owner", Role: common.RoleCommonUser, Quota: 1000}
			require.NoError(t, db.Create(&user).Error)
			require.NoError(t, model.LOG_DB.Callback().Create().Before("gorm:create").Register("test:fail_quota_log", func(tx *gorm.DB) {
				if tx.Statement.Table == failedTable {
					tx.AddError(errors.New("quota log unavailable"))
				}
			}))
			recorder := performManageUserRequest(t, quotaAdjustmentRequest(t, user.Id, "add", 500, user.Quota))
			assert.Contains(t, recorder.Body.String(), `"success":true`)
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, 1500, user.Quota)
			var logCount, auditCount int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&logCount).Error)
			require.NoError(t, model.LOG_DB.Model(&model.AuditLog{}).Count(&auditCount).Error)
			if failedTable == "logs" {
				assert.Zero(t, logCount)
				assert.EqualValues(t, 1, auditCount)
			} else {
				assert.EqualValues(t, 1, logCount)
				assert.Zero(t, auditCount)
			}
		})
	}
}

func TestManageUserQuotaTargetsAndWalletBounds(t *testing.T) {
	for _, tc := range []struct {
		name, mode, reason                                string
		before, value, targetID, targetRole, operatorRole int
		deleted, failRead                                 bool
	}{
		{name: "zero_id", mode: "add", value: 1, reason: "invalid_parameters"},
		{name: "negative_id", mode: "subtract", value: 1, targetID: -1, reason: "invalid_parameters"},
		{name: "missing", mode: "override", value: 1, targetID: 12345, reason: "target_not_found"},
		{name: "deleted", mode: "add", value: 1, targetID: 1, deleted: true, reason: "target_not_found"},
		{name: "peer_admin", mode: "add", value: 1, targetID: 1, targetRole: common.RoleAdminUser, operatorRole: common.RoleAdminUser, reason: "permission_denied"},
		{name: "higher_role", mode: "override", value: 1, targetID: 1, targetRole: common.RoleRootUser, operatorRole: common.RoleAdminUser, reason: "permission_denied"},
		{name: "read_error", mode: "add", value: 1, targetID: 1, failRead: true, reason: "database_error"},
		{name: "add_overflow", mode: "add", before: common.MaxWalletQuota, value: 1, targetID: 1, reason: "quota_limit_exceeded"},
		{name: "subtract_underflow", mode: "subtract", before: -common.MaxWalletQuota, value: 1, targetID: 1, reason: "quota_limit_exceeded"},
		{name: "oversized_add", mode: "add", value: common.MaxWalletQuota + 1, targetID: 1, reason: "quota_limit_exceeded"},
		{name: "oversized_subtract", mode: "subtract", value: common.MaxWalletQuota + 1, targetID: 1, reason: "quota_limit_exceeded"},
		{name: "oversized_override", mode: "override", value: -common.MaxWalletQuota - 1, targetID: 1, reason: "quota_limit_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			createQuotaTestOperator(t, db, tc.operatorRole)
			user := model.User{Id: 1, Username: "private-target-name", Role: tc.targetRole, Quota: tc.before}
			require.NoError(t, db.Create(&user).Error)
			if tc.deleted {
				require.NoError(t, db.Delete(&user).Error)
			}
			if tc.failRead {
				require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:quota_read_error", func(tx *gorm.DB) {
					if tx.Statement.Table == "users" && len(tx.Statement.Selects) == 0 {
						tx.AddError(errors.New("private database error"))
					}
				}))
			}
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/user/manage", strings.NewReader(fmt.Sprintf(`{"id":%d,"action":"add_quota","mode":%q,"value":%d}`, tc.targetID, tc.mode, tc.value)))
			role := tc.operatorRole
			if role == 0 {
				role = common.RoleRootUser
			}
			c.Set("id", 9999)
			c.Set("role", role)
			ManageUser(c)
			require.Contains(t, recorder.Body.String(), `"success":false`)
			if tc.failRead {
				require.NoError(t, db.Callback().Query().Remove("test:quota_read_error"))
			}
			require.NoError(t, db.Unscoped().First(&user, user.Id).Error)
			assert.Equal(t, tc.before, user.Quota)
			var audits []model.AuditLog
			require.NoError(t, model.LOG_DB.Find(&audits).Error)
			require.Len(t, audits, 1)
			assert.False(t, audits[0].Success)
			params, err := common.Marshal(audits[0].Other.Op.Params)
			require.NoError(t, err)
			expected, err := common.Marshal(model.AuditFields{"target_user_id": tc.targetID, "mode": tc.mode, "requested_quota": tc.value, "failure_reason": tc.reason})
			require.NoError(t, err)
			assert.JSONEq(t, string(expected), string(params))
			assert.NotContains(t, audits[0].Content, user.Username)
			var count int64
			require.NoError(t, model.LOG_DB.Model(&model.Log{}).Count(&count).Error)
			assert.Zero(t, count)
		})
	}
}

func TestManageUserQuotaMiddlewareKeepsOneOperationPerRequest(t *testing.T) {
	db := setupManageUserTestDB(t)
	pat := "quota-middleware-test-token"
	operator := model.User{Id: 9999, Username: "root-operator", Role: common.RoleRootUser, Status: common.UserStatusEnabled, AuthVersion: 1, AccessToken: &pat, Quota: 1000}
	require.NoError(t, db.Create(&operator).Error)
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.AccessTokenAudit())
	router.POST("/api/user/manage", middleware.AdminAuth(), ManageUser)
	for _, tc := range []struct {
		body, action string
		success      bool
	}{
		{`{"id":9999,"action":"add_quota","mode":"add","value":100,"payment_amount_cents":0,"idempotency_key":"53ae3ce4-c8ca-4f90-8bc1-9701e243634c","expected_delta_quota":"100"}`, "user.quota_add", true},
		{`{"id":9999,"action":"add_quota","mode":"subtract","value":0}`, "user.quota_subtract", false},
		{`{"id":9999,"action":"add_quota","mode":"invalid","value":1}`, "generic", false},
		{`{"id":`, "generic", false},
	} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/api/user/manage", strings.NewReader(tc.body))
		request.Header.Set("Authorization", "Bearer "+pat)
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(recorder, request)
		assert.Equal(t, http.StatusOK, recorder.Code)
		assert.Contains(t, recorder.Body.String(), fmt.Sprintf(`"success":%t`, tc.success))
		requestID := recorder.Header().Get(common.RequestIdKey)
		require.NotEmpty(t, requestID)
		var audits []model.AuditLog
		require.NoError(t, model.LOG_DB.Where("request_id = ?", requestID).Find(&audits).Error)
		require.Len(t, audits, 2, "one operation audit and one PAT request audit")
		for _, audit := range audits {
			assert.Equal(t, tc.success, audit.Success)
			if audit.Category != model.AuditCategoryOperation {
				continue
			}
			assert.Equal(t, tc.action, audit.Action)
			assert.Equal(t, operator.Id, audit.UserId)
			assert.Equal(t, "/api/user/manage", audit.Route)
			if tc.success {
				params, err := common.Marshal(audit.Other.Op.Params)
				require.NoError(t, err)
				assert.Contains(t, string(params), `"target_user_id":9999`)
				assert.Contains(t, string(params), `"target_username":"root-operator"`)
			}
		}
		var count int64
		require.NoError(t, model.LOG_DB.Model(&model.Log{}).Where("request_id = ?", requestID).Count(&count).Error)
		if tc.success {
			assert.EqualValues(t, 1, count)
		} else {
			assert.Zero(t, count)
		}
	}
	require.NoError(t, db.First(&operator, operator.Id).Error)
	assert.Equal(t, 1100, operator.Quota)
}

func TestManageUserQuotaConcurrentSnapshots(t *testing.T) {
	t.Run("same_key_credits_once_under_concurrency", func(t *testing.T) {
		db := setupManageUserTestDB(t)
		user := model.User{Username: "concurrent-same-key", Quota: 1000}
		require.NoError(t, db.Create(&user).Error)
		var ready sync.WaitGroup
		ready.Add(2)
		release := make(chan struct{})
		require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:same_key_start", func(tx *gorm.DB) {
			if tx.Statement.Table == "users" {
				ready.Done()
				<-release
			}
		}))
		input := &model.ManualQuotaOrderInput{OperatorID: 9999, PaymentAmountCents: new(int64), IdempotencyKey: uuid.NewString(), ExpectedDeltaQuota: "500"}
		type result struct {
			adjustment *model.UserQuotaAdjustment
			err        error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				adjustment, err := model.AdjustUserQuota(user.Id, common.RoleRootUser, "add", 500, input)
				results <- result{adjustment, err}
			}()
		}
		ready.Wait()
		close(release)
		var committed, replayed int
		for range 2 {
			result := <-results
			if result.err != nil {
				require.True(t, common.UsingMainDatabase(common.DatabaseTypeSQLite))
				assert.Contains(t, strings.ToLower(result.err.Error()), "locked")
				continue
			}
			require.NotNil(t, result.adjustment)
			if result.adjustment.Replayed {
				replayed++
			} else {
				committed++
			}
		}
		require.NoError(t, db.Callback().Query().Remove("test:same_key_start"))
		assert.Equal(t, 1, committed)
		if !common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			assert.Equal(t, 1, replayed)
		}
		require.NoError(t, db.First(&user, user.Id).Error)
		assert.Equal(t, 1500, user.Quota)
		var count int64
		require.NoError(t, db.Model(&model.TopUp{}).Count(&count).Error)
		assert.EqualValues(t, 1, count)
	})
	db := setupManageUserTestDB(t)
	user := model.User{Username: "concurrent-quota", Quota: 1000}
	require.NoError(t, db.Create(&user).Error)
	var ready sync.WaitGroup
	ready.Add(2)
	release := make(chan struct{})
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:concurrent_quota_start", func(tx *gorm.DB) {
		if tx.Statement.Table == "users" {
			ready.Done()
			<-release
		}
	}))
	type result struct {
		adjustment *model.UserQuotaAdjustment
		err        error
		value      int
	}
	results := make(chan result, 2)
	for _, value := range []int{10, 20} {
		go func(value int) {
			adjustment, err := model.AdjustUserQuota(user.Id, common.RoleRootUser, "add", value, &model.ManualQuotaOrderInput{OperatorID: 9999, PaymentAmountCents: new(int64), IdempotencyKey: uuid.NewString(), ExpectedDeltaQuota: strconv.Itoa(value)})
			results <- result{adjustment, err, value}
		}(value)
	}
	ready.Wait()
	close(release)
	var committed []model.UserQuotaAdjustment
	for range 2 {
		result := <-results
		if result.err != nil {
			require.True(t, common.UsingMainDatabase(common.DatabaseTypeSQLite), "row-locking databases must serialize both adjustments: %v", result.err)
			assert.Contains(t, strings.ToLower(result.err.Error()), "locked")
			assert.Nil(t, result.adjustment)
			continue
		}
		require.NotNil(t, result.adjustment)
		assert.Equal(t, result.value, result.adjustment.After-result.adjustment.Before)
		committed = append(committed, *result.adjustment)
	}
	require.NoError(t, db.Callback().Query().Remove("test:concurrent_quota_start"))
	require.NotEmpty(t, committed)
	sort.Slice(committed, func(i, j int) bool { return committed[i].Before < committed[j].Before })
	balance := 1000
	for _, adjustment := range committed {
		assert.Equal(t, balance, adjustment.Before)
		balance = adjustment.After
	}
	require.NoError(t, db.First(&user, user.Id).Error)
	assert.Equal(t, balance, user.Quota)
}

func TestManageUserQuotaCacheUsesCommittedIntegerDifference(t *testing.T) {
	for _, tc := range []struct {
		name, mode                               string
		before, cached, value, after, wantCached int
		failUpdate, failCache, missingCache      bool
	}{
		{name: "add_preserves_reservations", mode: "add", before: 1000, cached: 900, value: 500, after: 1500, wantCached: 1400},
		{name: "subtract_preserves_reservations", mode: "subtract", before: 1000, cached: 900, value: 500, after: 500, wantCached: 400},
		{name: "override_preserves_reservations", mode: "override", before: 1000, cached: 900, value: 2000, after: 2000, wantCached: 1900},
		{name: "large_odd_difference", mode: "override", before: common.MaxWalletQuota - 1, cached: common.MaxWalletQuota - 1, value: -common.MaxWalletQuota, after: -common.MaxWalletQuota, wantCached: -common.MaxWalletQuota},
		{name: "rollback_does_not_change_cache", mode: "subtract", before: 1000, cached: 900, value: 500, after: 1000, wantCached: 900, failUpdate: true},
		{name: "cache_error_keeps_committed_change", mode: "add", before: 1000, value: 500, after: 1500, failCache: true},
		{name: "missing_cache_is_not_partially_created", mode: "add", before: 1000, value: 500, after: 1500, missingCache: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupManageUserTestDB(t)
			operator := createQuotaTestOperator(t, db, common.RoleRootUser)
			server := miniredis.RunT(t)
			oldRDB := common.RDB
			common.RDB = redis.NewClient(&redis.Options{Addr: server.Addr(), MaxRetries: -1})
			common.RedisEnabled = true
			t.Cleanup(func() { _ = common.RDB.Close(); common.RDB = oldRDB })
			user := model.User{Username: "cached-quota", Quota: tc.before, AuthVersion: 1}
			require.NoError(t, db.Create(&user).Error)
			cache, err := model.GetUserCache(user.Id)
			require.NoError(t, err)
			assert.Equal(t, tc.before, cache.Quota)
			_, err = model.GetUserCache(operator.Id)
			require.NoError(t, err)
			keys := server.Keys()
			var quotaKey string
			for _, key := range keys {
				if server.HGet(key, "Id") == strconv.Itoa(user.Id) && server.HGet(key, "Quota") != "" {
					quotaKey = key
					break
				}
			}
			require.NotEmpty(t, quotaKey)
			server.HSet(quotaKey, "Quota", strconv.Itoa(tc.cached))
			if tc.missingCache {
				server.Del(quotaKey)
			}
			if tc.failCache {
				server.SetError("ERR quota cache unavailable")
			}
			if tc.failUpdate {
				require.NoError(t, db.Callback().Update().After("gorm:update").Register("test:cache_quota_rollback", func(tx *gorm.DB) {
					if tx.Statement.Table == "users" {
						tx.AddError(errors.New("quota rollback"))
					}
				}))
			}
			recorder := performManageUserRequest(t, quotaAdjustmentRequest(t, user.Id, tc.mode, tc.value, user.Quota))
			assert.Contains(t, recorder.Body.String(), fmt.Sprintf(`"success":%t`, !tc.failUpdate))
			require.NoError(t, db.First(&user, user.Id).Error)
			assert.Equal(t, tc.after, user.Quota)
			if tc.missingCache {
				// Log username lookup may hydrate the whole user after commit.
				if server.Exists(quotaKey) {
					assert.Equal(t, strconv.Itoa(user.Id), server.HGet(quotaKey, "Id"))
					assert.NotEmpty(t, server.HGet(quotaKey, "CacheSchema"))
					assert.Equal(t, strconv.Itoa(tc.after), server.HGet(quotaKey, "Quota"))
				}
			} else if !tc.failCache {
				assert.Equal(t, strconv.Itoa(tc.wantCached), server.HGet(quotaKey, "Quota"))
			}
		})
	}
}
