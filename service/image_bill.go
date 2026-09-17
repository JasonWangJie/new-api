package service

import (
	"errors"
	"fmt"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
)

// FixAsyncImageBill evaluates the captured price configuration exactly once.
// It has no debit, log-store write, or additive cache update. An expression
// failure is an error rather than a fallback to an estimated reservation.
func FixAsyncImageBill(c *gin.Context, task model.AsyncImageTask, info *relaycommon.RelayInfo, usage *dto.Usage, images []ImageBytes, requestFacts AsyncImageRequest, funding model.ImageFundingSelection) (model.AsyncImageBill, error) {
	count := len(images)
	if info == nil || usage == nil || count < 1 || count > dto.MaxImageN || info.Billing != nil {
		return model.AsyncImageBill{}, errors.New("invalid deferred image billing context")
	}
	if err := EstimateImageBillingForRequest(info, count, false); err != nil {
		return model.AsyncImageBill{}, err
	}
	info.BillingImageCount = &count
	billingUsage := effectiveBillingUsage(usage)
	if billingUsage == nil {
		return model.AsyncImageBill{}, errors.New("image billing usage is missing")
	}
	// Reject invalid quantities before shared arithmetic and normalization.
	counts := []int{billingUsage.PromptTokens, billingUsage.CompletionTokens, billingUsage.TotalTokens, billingUsage.ClaudeCacheCreation1hTokens, billingUsage.ClaudeCacheCreation5mTokens, billingUsage.PromptTokensDetails.CachedCreationTokens, billingUsage.PromptTokensDetails.CacheWriteTokens, billingUsage.PromptTokensDetails.CachedTokens, billingUsage.PromptTokensDetails.ImageTokens, billingUsage.PromptTokensDetails.AudioTokens, billingUsage.CompletionTokenDetails.ImageTokens, billingUsage.CompletionTokenDetails.AudioTokens}
	for _, value := range counts {
		if value < 0 || value > common.MaxQuota {
			return model.AsyncImageBill{}, errors.New("invalid upstream image token usage")
		}
	}
	summary := calculateTextQuotaSummary(c, info, billingUsage)
	if info.PriceData.UsePrice && !summary.hasBillableUsage() {
		// Actual image outputs are the billing subject for legacy unit prices.
		// Keep upstream token usage unchanged even when it is entirely zero.
		summary.Quota = info.PriceData.QuotaToPreConsume
		summary.FixedPriceBilling = true
	}
	var tieredResult *billingexpr.TieredResult
	var perImageResults []billingexpr.TieredResult
	tokenPricing := !info.PriceData.UsePrice
	specifications := ImageBillingSpecifications(requestFacts, images)
	if snapshot := info.TieredBillingSnapshot; snapshot != nil && snapshot.BillingMode == "tiered_expr" {
		request := billingexpr.RequestInput{}
		if info.BillingRequestInput != nil {
			request = *info.BillingRequestInput
		}
		request.ImageCount = &count
		params := BuildTieredTokenParams(billingUsage, summary.IsClaudeUsageSemantic, billingexpr.UsedVarsByHash(snapshot.ExprString, snapshot.ExprHash))
		result, err := billingexpr.ComputeTieredQuotaWithRequest(snapshot, params, request)
		if err != nil {
			return model.AsyncImageBill{}, err
		}
		if result.ImageCount != nil && result.BillingUnit == billingexpr.BillingUnitRequest {
			// Evaluate image-count expressions against each actual specification.
			// Uniform explicit aliases keep the requested tier, while mixed auto
			// outputs are summed individually rather than using their largest tier.
			var beforeGroup float64
			one := 1
			for _, specification := range specifications {
				body, err := common.Marshal(specification)
				if err != nil {
					return model.AsyncImageBill{}, err
				}
				perRequest := request
				perRequest.Body = body
				perRequest.ImageCount = &one
				part, err := billingexpr.ComputeTieredQuotaWithRequest(snapshot, params, perRequest)
				if err != nil {
					return model.AsyncImageBill{}, err
				}
				if part.BillingUnit != billingexpr.BillingUnitRequest {
					return model.AsyncImageBill{}, errors.New("mixed image and token pricing cannot allocate aggregate upstream usage")
				}
				if part.Clamp != nil {
					return model.AsyncImageBill{}, errors.New("actual image bill exceeds the single-request quota boundary")
				}
				beforeGroup += part.ActualQuotaBeforeGroup
				perImageResults = append(perImageResults, part)
			}
			// Round the complete bill once, matching uniform image_count pricing.
			// Token-priced leaves above retain the aggregate usage exactly once.
			quota, err := billingexpr.QuotaRoundStrict(beforeGroup * snapshot.GroupRatio)
			if err != nil {
				return model.AsyncImageBill{}, err
			}
			result = perImageResults[0]
			result.ActualQuotaAfterGroup = quota
			result.ActualQuotaBeforeGroup = beforeGroup
			result.ImageCount = &count
			if len(perImageResults) > 1 {
				result.MatchedTier = "multiple_images"
			}
		}
		noteQuotaClamp(info, result.Clamp)
		tokenPricing = result.BillingUnit == billingexpr.BillingUnitToken
		tieredResult = &result
		summary.Quota = composeTieredTextQuota(info, summary, result.ActualQuotaAfterGroup, &result)
	}
	if tokenPricing && info.PriceData.QuotaToPreConsume > 0 && (!summary.hasBillableUsage() || usage.BillingUsage != nil && usage.BillingUsage.Estimated) {
		return model.AsyncImageBill{}, errors.New("actual upstream token usage is unavailable for the captured nonzero image price")
	}
	if summary.Quota < 0 {
		return model.AsyncImageBill{}, errors.New("negative image bill rejected")
	}
	other := GenerateTextOtherInfo(c, info, summary.ModelRatio, summary.GroupRatio, summary.CompletionRatio, summary.CacheTokens, summary.CacheRatio, summary.ModelPrice, info.PriceData.GroupRatioInfo.GroupSpecialRatio)
	other.SetPublic("async_image", true)
	other.SetPublic("image_count", count)
	other.SetPublic("image_billing_specifications", specifications)
	if tieredResult != nil {
		InjectTieredBillingInfo(other, info, tieredResult)
	}
	attachQuotaSaturation(c, info, other)
	priceSnapshot, err := common.Marshal(map[string]any{"price": info.PriceData, "other_ratios": info.PriceData.OtherRatios(), "expression": info.TieredBillingSnapshot, "actual": tieredResult, "per_image": perImageResults, "specifications": specifications, "image_count": count, "quota_per_unit": common.QuotaPerUnit})
	if err != nil {
		return model.AsyncImageBill{}, err
	}
	usageBytes, err := common.Marshal(usage)
	if err != nil {
		return model.AsyncImageBill{}, err
	}
	var user model.User
	if err := model.DB.WithContext(c.Request.Context()).Where("id = ?", task.UserId).Take(&user).Error; err != nil {
		return model.AsyncImageBill{}, err
	}
	log := model.Log{UserId: task.UserId, Username: user.Username, TokenId: task.TokenId, TokenName: c.GetString("token_name"), ChannelId: task.ChannelId, Type: model.LogTypeConsume, ModelName: info.GetBillingModelName(), CreatedAt: time.Now().Unix(), Quota: summary.Quota, PromptTokens: summary.PromptTokens, CompletionTokens: summary.CompletionTokens, UseTime: int(time.Since(info.StartTime).Seconds()), Group: task.Group, RequestId: "async-image:" + task.TaskId, Content: fmt.Sprintf("异步图片生成，数量 %d", count), Other: other.JSONString()}
	logBytes, err := common.Marshal(log)
	if err != nil {
		return model.AsyncImageBill{}, err
	}
	bill := model.AsyncImageBill{TaskId: task.TaskId, BillingRequestId: log.RequestId, UserId: task.UserId, TokenId: task.TokenId, ChannelId: task.ChannelId, Quota: summary.Quota, FundingSource: funding.Source, SubscriptionId: funding.SubscriptionId, Snapshot: string(priceSnapshot), Usage: string(usageBytes), LogPayload: string(logBytes), CreatedAt: log.CreatedAt}
	if funding.Source == "subscription" {
		bill.SubscriptionAmount = int64(bill.Quota)
	}
	bill.Fingerprint = ImageIdentityHash(bill.TaskId, bill.BillingRequestId, fmt.Sprint(bill.Quota), bill.FundingSource, fmt.Sprint(bill.SubscriptionId), bill.Snapshot, bill.Usage, bill.LogPayload)
	return bill, nil
}

// ImageBillingSpecifications exposes only pricing scalars, never prompt or
// reference bytes. The original n stays frozen independently of image_count.
func ImageBillingSpecifications(request AsyncImageRequest, images []ImageBytes) []map[string]any {
	specifications := make([]map[string]any, len(images))
	for i, image := range images {
		tier := request.Resolution
		if !request.ExplicitTier || tier == "" || tier == "AUTO" {
			tier = ImageNativeTier(image.Width, image.Height, request.Platform)
			if width, height, native := imageDimensions(request.Size); native && request.Platform == "openai" && !request.ExplicitTier {
				tier = ImageNativeTier(width, height, request.Platform)
			}
		}
		if tier == "0.5K" {
			tier = "1K"
		}
		specifications[i] = map[string]any{"model": request.Model, "n": request.Count, "size": fmt.Sprintf("%dx%d", image.Width, image.Height), "resolution": tier, "billing_resolution": tier, "actual_width": image.Width, "actual_height": image.Height}
		if request.Size != "" && request.Size != "auto" && request.Platform == "openai" {
			specifications[i]["size"] = request.Size
		}
		if request.Platform == "gemini" {
			specifications[i]["generationConfig"] = map[string]any{"imageConfig": map[string]string{"imageSize": tier, "aspectRatio": request.AspectRatio}}
			specifications[i]["extra_body"] = map[string]any{"google": map[string]any{"image_config": map[string]string{"image_size": tier, "aspect_ratio": request.AspectRatio}}}
		}
		for _, name := range []string{"quality", "style", "background", "output_format", "output_compression", "moderation", "input_fidelity"} {
			if raw, ok := request.Native[name]; ok {
				var value any
				if common.Unmarshal(raw, &value) == nil {
					specifications[i][name] = value
				}
			}
		}
	}
	return specifications
}
