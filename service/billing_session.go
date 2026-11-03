package service

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"

	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
)

// ---------------------------------------------------------------------------
// BillingSession — 统一计费会话
// ---------------------------------------------------------------------------

// BillingSession 封装单次请求的预扣费/结算/退款生命周期。
// 实现 relaycommon.BillingSettler 接口。
type BillingSession struct {
	relayInfo           *relaycommon.RelayInfo
	funding             FundingSource
	durable             bool // async task reservations bypass process-local batch updates
	preConsumedQuota    int  // 实际预扣额度（信任用户可能为 0）
	tokenConsumed       int  // 令牌额度实际扣减量
	trusted             bool // 是否命中信任额度旁路
	fundingSettled      bool // funding.Settle 已成功，资金来源已提交
	settled             bool // Settle 全部完成（资金 + 令牌）
	refundStarted       bool // Non-idempotent wallet/token refunds must run at most once.
	refunded            bool // All confirmed deductions were refunded without uncertainty.
	reservationErr      error
	uncertainQuotaErr   error
	settlementUncertain bool
	attemptedQuota      int
	failureStage        string
	mu                  sync.Mutex
}

// BillingReservationFailure records confirmed financial holds separately from
// an attempted write whose commit may be unknown. It contains no raw errors.
type BillingReservationFailure struct {
	FundingSource         string `json:"funding_source"`
	ConfirmedFundingQuota int    `json:"confirmed_funding_quota"`
	ConfirmedTokenQuota   int    `json:"confirmed_token_quota"`
	PairedQuota           int    `json:"paired_quota"`
	AttemptedQuota        int    `json:"attempted_quota"`
	Uncertain             bool   `json:"uncertain"`
	Stage                 string `json:"stage"`
}

func (s *BillingSession) ReservationFailure() *BillingReservationFailure {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservationErr == nil {
		return nil
	}
	return &BillingReservationFailure{
		FundingSource: s.funding.Source(), ConfirmedFundingQuota: s.reservedFundingQuota(),
		ConfirmedTokenQuota: s.tokenConsumed, PairedQuota: s.preConsumedQuota,
		AttemptedQuota: s.attemptedQuota, Uncertain: s.uncertainQuotaErr != nil, Stage: s.failureStage,
	}
}

// Settle 根据实际消耗额度进行结算。
// 资金来源和令牌额度分两步提交；写入结果不确定时锁闭会话，
// 保留已提交阶段，禁止重复结算或按旧预扣额度自动退款。
func (s *BillingSession) Settle(actualQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservationErr != nil {
		return s.reservationErr
	}
	if s.refundStarted {
		return fmt.Errorf("billing refund has already started")
	}
	if s.settled {
		return nil
	}
	delta := actualQuota - s.preConsumedQuota
	if s.durable && delta > 0 {
		return fmt.Errorf("durable task charge exceeds reserved quota")
	}
	if delta == 0 {
		s.settled = true
		return nil
	}
	// 1) 调整资金来源（仅在尚未提交时执行，防止重复调用）
	if !s.fundingSettled {
		if err := s.funding.Settle(delta); err != nil {
			s.reservationErr, s.uncertainQuotaErr = err, err
			s.settlementUncertain = true
			s.attemptedQuota, s.failureStage = actualQuota, "funding_settle"
			return err
		}
		s.fundingSettled = true
	}
	// 2) 调整令牌额度
	var tokenErr error
	if !s.relayInfo.IsPlayground {
		if s.durable || delta < 0 && !common.BatchUpdateEnabled {
			tokenErr = model.AdjustTokenQuotaDurable(s.relayInfo.TokenId, s.relayInfo.TokenKey, -delta)
		} else if delta > 0 {
			tokenErr = model.DecreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, delta)
		} else {
			tokenErr = model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, -delta)
		}
		if tokenErr != nil {
			// Preserve the confirmed funding settlement while freezing the
			// uncertain token adjustment; a repeated call must still fail.
			common.SysLog(fmt.Sprintf("error adjusting token quota after funding settled (userId=%d, tokenId=%d, delta=%d): %s",
				s.relayInfo.UserId, s.relayInfo.TokenId, delta, tokenErr.Error()))
			s.reservationErr, s.uncertainQuotaErr = tokenErr, tokenErr
			s.settlementUncertain = true
			s.attemptedQuota, s.failureStage = actualQuota, "token_settle"
		}
	}
	// 3) 更新 relayInfo 上的订阅 PostDelta（用于日志）
	if s.funding.Source() == BillingSourceSubscription {
		s.relayInfo.SubscriptionPostDelta += int64(delta)
	}
	s.settled = tokenErr == nil
	return tokenErr
}

// Refund 退还所有预扣费。异步视频提交等待退款结果落账，其余请求异步执行。
func (s *BillingSession) Refund(c *gin.Context) {
	asyncMedia := c.GetBool("async_media_execution")
	s.mu.Lock()
	if s.settled || s.refundStarted || s.fundingSettled && !s.settlementUncertain {
		s.mu.Unlock()
		return
	}
	if !s.needsRefundLocked() {
		s.refundStarted, s.refunded = true, true
		s.mu.Unlock()
		if asyncMedia {
			recordAsyncVideoSubmissionRefund(c, s.relayInfo, 0, nil)
		}
		return
	}
	s.refundStarted = true
	quota := s.reservedFundingQuota()
	uncertainErr := s.uncertainQuotaErr
	settlementUncertain := s.settlementUncertain
	s.mu.Unlock()

	logger.LogInfo(c, fmt.Sprintf("用户 %d 请求失败, 返还预扣费（token_quota=%s, funding=%s）",
		s.relayInfo.UserId,
		logger.FormatQuota(s.tokenConsumed),
		s.funding.Source(),
	))

	// 复制需要的值到闭包中
	tokenId := s.relayInfo.TokenId
	tokenKey := s.relayInfo.TokenKey
	isPlayground := s.relayInfo.IsPlayground
	tokenConsumed := s.tokenConsumed
	funding := s.funding

	refund := func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		refundErr := uncertainErr
		// The upstream operation has already completed. A failed settlement
		// can include a committed credit, so neither side may be refunded
		// from its pre-settlement snapshot without reconciliation.
		if settlementUncertain {
			if asyncMedia {
				recordAsyncVideoSubmissionRefund(c, s.relayInfo, quota, refundErr)
			}
			return
		}
		// 1) 退还资金来源
		if err := funding.Refund(); err != nil {
			common.SysLog("error refunding billing source: " + err.Error())
			refundErr = errors.Join(refundErr, err)
		} else {
			switch funding := funding.(type) {
			case *WalletFunding:
				funding.consumed = 0
			case *SubscriptionFunding:
				funding.preConsumed = 0
			}
		}
		// 2) 退还令牌额度
		if tokenConsumed > 0 && !isPlayground {
			var err error
			if s.durable || !common.BatchUpdateEnabled {
				err = model.AdjustTokenQuotaDurable(tokenId, tokenKey, tokenConsumed)
			} else {
				err = model.IncreaseTokenQuota(tokenId, tokenKey, tokenConsumed)
			}
			if err != nil {
				common.SysLog("error refunding token quota: " + err.Error())
				refundErr = errors.Join(refundErr, err)
			} else {
				s.tokenConsumed = 0
			}
		}
		s.refunded = refundErr == nil
		if refundErr != nil {
			s.reservationErr = errors.Join(s.reservationErr, refundErr)
			s.uncertainQuotaErr = refundErr
			s.failureStage = "refund"
		}
		if asyncMedia {
			recordAsyncVideoSubmissionRefund(c, s.relayInfo, quota, refundErr)
		}
	}
	if asyncMedia {
		refund()
		return
	}
	gopool.Go(refund)
}

// NeedsRefund 返回是否存在需要退还的预扣状态。
func (s *BillingSession) NeedsRefund() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.needsRefundLocked()
}

func (s *BillingSession) needsRefundLocked() bool {
	if s.settled || s.refundStarted || s.fundingSettled && !s.settlementUncertain {
		// fundingSettled 时资金来源已提交结算，不能再退预扣费
		return false
	}
	return s.tokenConsumed > 0 || s.reservedFundingQuota() > 0 || s.uncertainQuotaErr != nil
}

// Funding can be reserved before a token refusal, so it is not necessarily
// equal to the amount that fully covered both sides of the reservation.
func (s *BillingSession) reservedFundingQuota() int {
	switch funding := s.funding.(type) {
	case *WalletFunding:
		return funding.consumed
	case *SubscriptionFunding:
		return int(funding.preConsumed)
	default:
		return s.preConsumedQuota
	}
}

// GetPreConsumedQuota 返回实际预扣的额度。
func (s *BillingSession) GetPreConsumedQuota() int {
	return s.preConsumedQuota
}

func (s *BillingSession) Reserve(targetQuota int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.reservationErr != nil {
		return s.reservationErr
	}
	if s.settled || s.refundStarted || s.fundingSettled {
		return fmt.Errorf("billing session is no longer reservable")
	}

	imageRequest := false
	if s.relayInfo != nil {
		_, imageRequest = s.relayInfo.Request.(*dto.ImageRequest)
		imageRequest = imageRequest || s.relayInfo.ImageRequestCount > 0
	}
	if s.trusted && !imageRequest || targetQuota <= s.preConsumedQuota {
		return nil
	}

	delta := targetQuota - s.preConsumedQuota
	if delta <= 0 {
		return nil
	}

	s.attemptedQuota = targetQuota
	if err := s.reserveFunding(delta, imageRequest); err != nil {
		s.reservationErr = err
		s.failureStage = "funding_reserve"
		if !errors.Is(err, ErrInsufficientWalletQuota) && !errors.Is(err, model.ErrInsufficientSubscriptionQuota) {
			s.uncertainQuotaErr = err
		}
		return err
	}
	if err := s.reserveToken(delta); err != nil {
		// Leave the confirmed funding debit in its ledger for the single
		// full refund. An eager compensation could commit without its reply,
		// making a later refund credit that same amount twice.
		s.reservationErr = err
		s.failureStage = "token_reserve"
		if !errors.Is(err, errInsufficientTokenQuota) {
			s.uncertainQuotaErr = err
		}
		return err
	}

	s.preConsumedQuota += delta
	s.tokenConsumed += delta
	if imageRequest {
		s.trusted = false
	}
	s.syncRelayInfo()
	return nil
}

// ---------------------------------------------------------------------------
// PreConsume — 统一预扣费入口（含信任额度旁路）
// ---------------------------------------------------------------------------

type billingPreConsumeOutcome uint8

const (
	billingPreConsumeUnknown billingPreConsumeOutcome = iota
	billingPreConsumeUncharged
	billingPreConsumeRollbackFailed
)

// preConsume 执行预扣费：信任检查 -> 令牌预扣 -> 资金来源预扣。
// A failure reports whether all deductions are confirmed absent or compensated.
func (s *BillingSession) preConsume(c *gin.Context, quota int) (billingPreConsumeOutcome, *types.NewAPIError) {
	effectiveQuota := quota

	// ---- 信任额度旁路 ----
	if s.shouldTrust(c) {
		s.trusted = true
		effectiveQuota = 0
		logger.LogInfo(c, fmt.Sprintf("用户 %d 额度充足, 信任且不需要预扣费 (funding=%s)", s.relayInfo.UserId, s.funding.Source()))
	} else if effectiveQuota > 0 {
		logger.LogInfo(c, fmt.Sprintf("用户 %d 需要预扣费 %s (funding=%s)", s.relayInfo.UserId, logger.FormatQuota(effectiveQuota), s.funding.Source()))
	}

	// ---- 1) 预扣令牌额度 ----
	if effectiveQuota > 0 {
		if err := preConsumeTokenQuotaWithMode(s.relayInfo, effectiveQuota, s.durable); err != nil {
			outcome := billingPreConsumeUnknown
			if errors.Is(err, errInsufficientTokenQuota) {
				outcome = billingPreConsumeUncharged
			}
			return outcome, types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		s.tokenConsumed = effectiveQuota
	}

	// ---- 2) 预扣资金来源 ----
	if err := s.funding.PreConsume(effectiveQuota); err != nil {
		// 预扣费失败，回滚令牌额度
		if s.tokenConsumed > 0 && !s.relayInfo.IsPlayground {
			var rollbackErr error
			if s.durable || !common.BatchUpdateEnabled {
				rollbackErr = model.AdjustTokenQuotaDurable(s.relayInfo.TokenId, s.relayInfo.TokenKey, s.tokenConsumed)
			} else {
				rollbackErr = model.IncreaseTokenQuota(s.relayInfo.TokenId, s.relayInfo.TokenKey, s.tokenConsumed)
			}
			if rollbackErr != nil {
				common.SysLog(fmt.Sprintf("error rolling back token quota (userId=%d, tokenId=%d, amount=%d, fundingErr=%s): %s",
					s.relayInfo.UserId, s.relayInfo.TokenId, s.tokenConsumed, err.Error(), rollbackErr.Error()))
				// Do not forget the outstanding debit or fall back to another
				// funding source after an unsuccessful compensation.
				return billingPreConsumeRollbackFailed, types.NewError(errors.Join(err, rollbackErr), types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
			}
			s.tokenConsumed = 0
		}
		if errors.Is(err, ErrInsufficientWalletQuota) {
			userQuota, quotaErr := model.GetUserQuota(s.relayInfo.UserId, false)
			if quotaErr != nil {
				userQuota = 0
			}
			return billingPreConsumeUncharged, types.NewErrorWithStatusCode(
				fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		if errors.Is(err, model.ErrNoActiveSubscription) || errors.Is(err, model.ErrInsufficientSubscriptionQuota) {
			return billingPreConsumeUncharged, types.NewErrorWithStatusCode(fmt.Errorf("订阅额度不足或未配置订阅: %s", err.Error()), types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		return billingPreConsumeUnknown, types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}

	s.preConsumedQuota = effectiveQuota

	// ---- 同步 RelayInfo 兼容字段 ----
	s.syncRelayInfo()

	return billingPreConsumeUnknown, nil
}

func (s *BillingSession) reserveFunding(delta int, requireAvailableQuota bool) error {
	switch funding := s.funding.(type) {
	case *WalletFunding:
		if requireAvailableQuota || s.durable {
			// Image quantity is known before submission, including retries and
			// overrides. Reserve atomically instead of admitting wallet debt.
			if err := funding.PreConsume(delta); err != nil {
				if errors.Is(err, ErrInsufficientWalletQuota) {
					return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
				}
				return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
			}
			return nil
		}
		// 与结算补扣（SettleBilling 正差额 → WalletFunding.Settle）语义一致：
		// 全额无条件扣减，余额不足的部分记为欠费（余额可为负），不中断请求，
		// 保证日志记录的预扣额度与用户余额的实际变动始终对账一致。
		// DecreaseUserQuota 仅在数据库错误时失败。
		if err := funding.Settle(delta); err != nil {
			return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		funding.consumed += delta
		return nil
	case *SubscriptionFunding:
		if err := funding.Reserve(delta); err != nil {
			if errors.Is(err, model.ErrInsufficientSubscriptionQuota) {
				return types.NewErrorWithStatusCode(err, types.ErrorCodeInsufficientUserQuota, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
			}
			return types.NewError(err, types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
		}
		return nil
	default:
		return types.NewError(fmt.Errorf("unsupported funding source: %s", s.funding.Source()), types.ErrorCodeUpdateDataError, types.ErrOptionWithSkipRetry())
	}
}

func (s *BillingSession) reserveToken(delta int) error {
	if delta <= 0 || s.relayInfo.IsPlayground {
		return nil
	}
	if err := preConsumeTokenQuotaWithMode(s.relayInfo, delta, s.durable); err != nil {
		return types.NewErrorWithStatusCode(err, types.ErrorCodePreConsumeTokenQuotaFailed, http.StatusForbidden, types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
	}
	return nil
}

// shouldTrust 统一信任额度检查，适用于钱包和订阅。
func (s *BillingSession) shouldTrust(c *gin.Context) bool {
	// 异步任务（ForcePreConsume=true）必须预扣全额，不允许信任旁路
	if s.durable || s.relayInfo.ForcePreConsume {
		return false
	}

	trustQuota := common.GetTrustQuota()
	if trustQuota <= 0 {
		return false
	}

	// 检查令牌是否充足
	tokenTrusted := s.relayInfo.TokenUnlimited
	if !tokenTrusted {
		tokenQuota := c.GetInt("token_quota")
		tokenTrusted = tokenQuota > trustQuota
	}
	if !tokenTrusted {
		return false
	}

	switch s.funding.Source() {
	case BillingSourceWallet:
		return s.relayInfo.UserQuota > trustQuota
	case BillingSourceSubscription:
		// 订阅不能启用信任旁路。原因：
		// 1. PreConsumeUserSubscription 要求 amount>0 来创建预扣记录并锁定订阅
		// 2. SubscriptionFunding.PreConsume 忽略参数，始终用 s.amount 预扣
		// 3. 若信任旁路将 effectiveQuota 设为 0，会导致 preConsumedQuota 与实际订阅预扣不一致
		return false
	default:
		return false
	}
}

// syncRelayInfo 将 BillingSession 的状态同步到 RelayInfo 的兼容字段上。
func (s *BillingSession) syncRelayInfo() {
	info := s.relayInfo
	info.FinalPreConsumedQuota = s.preConsumedQuota
	info.BillingSource = s.funding.Source()

	if sub, ok := s.funding.(*SubscriptionFunding); ok {
		info.SubscriptionId = sub.subscriptionId
		info.SubscriptionPreConsumed = sub.preConsumed
		info.SubscriptionPostDelta = 0
		info.SubscriptionAmountTotal = sub.AmountTotal
		info.SubscriptionAmountUsedAfterPreConsume = sub.AmountUsedAfter
		info.SubscriptionPlanId = sub.PlanId
		info.SubscriptionPlanTitle = sub.PlanTitle
	} else {
		info.SubscriptionId = 0
		info.SubscriptionPreConsumed = 0
	}
}

// ---------------------------------------------------------------------------
// NewBillingSession 工厂 — 根据计费偏好创建会话并处理回退
// ---------------------------------------------------------------------------

// NewBillingSession 根据用户计费偏好创建 BillingSession，处理 subscription_first / wallet_first 的回退。
func NewBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int) (*BillingSession, *types.NewAPIError) {
	return newBillingSession(c, relayInfo, preConsumedQuota, false)
}

func newBillingSession(c *gin.Context, relayInfo *relaycommon.RelayInfo, preConsumedQuota int, durable bool) (session *BillingSession, apiErr *types.NewAPIError) {
	// Before the first mutation, query or quota-validation failures cannot
	// leave a charge. Every attempted reservation replaces this evidence.
	outcome := billingPreConsumeUncharged
	defer func() {
		if durable && apiErr != nil {
			recordAsyncVideoPreConsumeFailure(c, relayInfo, outcome)
		}
	}()
	if relayInfo == nil {
		return nil, types.NewError(fmt.Errorf("relayInfo is nil"), types.ErrorCodeInvalidRequest, types.ErrOptionWithSkipRetry())
	}

	pref := common.NormalizeBillingPreference(relayInfo.UserSetting.BillingPreference)

	// 钱包路径需要先检查用户额度
	tryWallet := func() (*BillingSession, *types.NewAPIError) {
		userQuota, err := model.GetUserQuota(relayInfo.UserId, false)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if userQuota <= 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("用户额度不足, 剩余额度: %s", logger.FormatQuota(userQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		if userQuota-preConsumedQuota < 0 {
			return nil, types.NewErrorWithStatusCode(
				fmt.Errorf("预扣费额度失败, 用户剩余额度: %s, 需要预扣费额度: %s", logger.FormatQuota(userQuota), logger.FormatQuota(preConsumedQuota)),
				types.ErrorCodeInsufficientUserQuota, http.StatusForbidden,
				types.ErrOptionWithSkipRetry(), types.ErrOptionWithNoRecordErrorLog())
		}
		relayInfo.UserQuota = userQuota

		session := &BillingSession{
			relayInfo: relayInfo,
			funding:   &WalletFunding{userId: relayInfo.UserId, durable: durable, subject: relayInfo.CanvasSubject},
			durable:   durable,
		}
		var apiErr *types.NewAPIError
		outcome, apiErr = session.preConsume(c, preConsumedQuota)
		if apiErr != nil {
			return nil, apiErr
		}
		return session, nil
	}

	trySubscription := func() (*BillingSession, *types.NewAPIError) {
		subConsume := int64(preConsumedQuota)
		if subConsume <= 0 {
			subConsume = 1
		}
		session := &BillingSession{
			relayInfo: relayInfo,
			durable:   durable,
			funding: &SubscriptionFunding{
				subject:   relayInfo.CanvasSubject,
				requestId: relayInfo.RequestId,
				userId:    relayInfo.UserId,
				modelName: relayInfo.GetBillingModelName(),
				amount:    subConsume,
				durable:   durable,
			},
		}
		// 必须传 subConsume 而非 preConsumedQuota，保证 SubscriptionFunding.amount、
		// preConsume 参数和 FinalPreConsumedQuota 三者一致，避免订阅多扣费。
		var apiErr *types.NewAPIError
		outcome, apiErr = session.preConsume(c, int(subConsume))
		if apiErr != nil {
			return nil, apiErr
		}
		return session, nil
	}

	switch pref {
	case "subscription_only":
		return trySubscription()
	case "wallet_only":
		return tryWallet()
	case "wallet_first":
		session, err := tryWallet()
		if err != nil {
			if err.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				return trySubscription()
			}
			return nil, err
		}
		return session, nil
	case "subscription_first":
		fallthrough
	default:
		hasSub, subCheckErr := model.HasActiveUserSubscription(relayInfo.UserId)
		if subCheckErr != nil {
			return nil, types.NewError(subCheckErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
		}
		if !hasSub {
			return tryWallet()
		}
		session, apiErr := trySubscription()
		if apiErr != nil {
			if apiErr.GetErrorCode() == types.ErrorCodeInsufficientUserQuota {
				// 仅当用户的活跃订阅允许钱包回退时才回退到钱包，否则返回订阅额度不足错误
				allowOverflow, overflowErr := model.UserActiveSubscriptionsAllowWalletOverflow(relayInfo.UserId)
				if overflowErr != nil {
					return nil, types.NewError(overflowErr, types.ErrorCodeQueryDataError, types.ErrOptionWithSkipRetry())
				}
				if allowOverflow {
					return tryWallet()
				}
				return nil, apiErr
			}
			return nil, apiErr
		}
		return session, nil
	}
}
