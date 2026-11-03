package service

import (
	"context"
	"fmt"
	"maps"
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/types"
)

// ValidateMixedVideoProfilePricing requires every enabled capability variant to
// supply the host facts referenced by its public price, including omitted defaults.
func ValidateMixedVideoProfilePricing(expression string, profiles []jsplugin.VideoProfile) error {
	for _, profile := range profiles {
		for _, mode := range profile.Modes {
			operation := "create"
			if mode.Name == "edit_video" {
				operation = "edit"
			} else if mode.Name == "extend_video" {
				operation = "extend"
			}
			body := make(map[string]any)
			if mode.Duration != nil {
				body["seconds"] = float64(mode.Duration.Default)
			}
			if len(mode.Resolutions) > 0 {
				body["resolution"] = mode.DefaultResolution
			}
			facts, err := MixedVideoPublicFacts(body, operation, nil, profile)
			if err == nil {
				err = billing_setting.ValidateMixedVideoBillingFacts(expression, facts)
			}
			if err != nil {
				return fmt.Errorf("mixed video mode %s: %w", mode.Name, err)
			}
		}
	}
	return nil
}

// ValidateMixedVideoPricing checks the complete price draft before it becomes
// active. Disabled policies and models without a price can be staged separately.
func ValidateMixedVideoPricing(ctx context.Context, expressions map[string]string) error {
	var policies []model.ImageGroupPolicy
	if err := model.DB.WithContext(ctx).Where("platform = ? AND enabled = ? AND async_enabled = ?", "mixed", true, true).Find(&policies).Error; err != nil {
		return err
	}
	for _, policy := range policies {
		var catalog []ImageModelCapability
		if err := common.UnmarshalJsonStr(policy.Models, &catalog); err != nil {
			return err
		}
		for _, capability := range catalog {
			expression, configured := expressions[capability.Id]
			if capability.MediaType != "video" || !configured {
				continue
			}
			profiles, err := MixedVideoModelProfiles(ctx, policy, capability.Id, capability.VideoOverrides, false)
			if err == nil {
				err = ValidateMixedVideoProfilePricing(expression, profiles)
			}
			if err != nil {
				return fmt.Errorf("mixed video model %s in group %s: %w", capability.Id, policy.Group, err)
			}
		}
	}
	return nil
}

// FreezeMixedVideoBilling pins the public model's host-owned request price.
// It has no provider or mapped-model fallback, including during recovery.
func FreezeMixedVideoBilling(modelName string, facts map[string]any, group types.GroupRatioInfo) (MediaBillingSnapshot, error) {
	expression, configured := billing_setting.GetMixedVideoBillingExpr(modelName)
	if !configured {
		return MediaBillingSnapshot{}, fmt.Errorf("mixed video model %s has no public billing expression", modelName)
	}
	if err := billing_setting.ValidateMixedVideoBillingFacts(expression, facts); err != nil {
		return MediaBillingSnapshot{}, err
	}
	if math.IsNaN(group.GroupRatio) || math.IsInf(group.GroupRatio, 0) || group.GroupRatio < 0 {
		return MediaBillingSnapshot{}, fmt.Errorf("mixed video billing group ratio is invalid")
	}
	frozen := maps.Clone(facts)
	cost, trace, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: frozen})
	if err != nil {
		return MediaBillingSnapshot{}, err
	}
	if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
		return MediaBillingSnapshot{}, fmt.Errorf("mixed video billing cost must be finite and non-negative")
	}
	quota, clamp := common.QuotaRoundChecked(cost * common.QuotaPerUnit * group.GroupRatio)
	snapshot := &billingexpr.BillingSnapshot{
		BillingMode: billing_setting.BillingModeTieredExpr, ModelName: modelName,
		ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), ExprVersion: billingexpr.ExprVersion(expression),
		GroupRatio: group.GroupRatio, QuotaPerUnit: common.QuotaPerUnit,
		EstimatedQuotaBeforeGroup: cost * common.QuotaPerUnit, EstimatedQuotaAfterGroup: quota, EstimatedTier: trace.MatchedTier,
		TaskUsageBilling: true, MixedVideoBilling: true, MixedVideoSchemaVersion: billing_setting.MixedVideoBillingSchemaVersion, UsageFacts: frozen,
	}
	return MediaBillingSnapshot{
		Price:  types.PriceData{Quota: quota, QuotaToPreConsume: quota, FreeModel: quota == 0, GroupRatioInfo: group},
		Tiered: snapshot, Clamp: clamp,
	}, nil
}

// ValidateFrozenMixedVideoBilling fails closed on incompatible or corrupted
// billing state; it never consults live prices or a plugin's usage schema.
func ValidateFrozenMixedVideoBilling(snapshot *billingexpr.BillingSnapshot) error {
	if snapshot == nil || !snapshot.MixedVideoBilling || snapshot.MixedVideoSchemaVersion != billing_setting.MixedVideoBillingSchemaVersion || !snapshot.TaskUsageBilling || snapshot.ExprHash != billingexpr.ExprHashString(snapshot.ExprString) || snapshot.ExprVersion != billingexpr.ExprVersion(snapshot.ExprString) || snapshot.ModelName == "" || snapshot.QuotaPerUnit <= 0 || math.IsNaN(snapshot.QuotaPerUnit) || math.IsInf(snapshot.QuotaPerUnit, 0) || snapshot.GroupRatio < 0 || math.IsNaN(snapshot.GroupRatio) || math.IsInf(snapshot.GroupRatio, 0) {
		return fmt.Errorf("mixed video billing snapshot is invalid or unsupported")
	}
	if err := billing_setting.ValidateMixedVideoBillingFacts(snapshot.ExprString, snapshot.UsageFacts); err != nil {
		return err
	}
	result, err := billingexpr.ComputeTieredQuotaWithRequest(snapshot, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: snapshot.UsageFacts})
	if err != nil {
		return err
	}
	if result.Clamp != nil || result.ActualQuotaAfterGroup != snapshot.EstimatedQuotaAfterGroup {
		return fmt.Errorf("mixed video billing reservation does not match the frozen request price")
	}
	return nil
}
