package controller

import (
	"context"
	"maps"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
)

// addMixedVideoPricing describes default group policy routing. A token-specific
// platform mapping or explicit provider can select a different route.
func addMixedVideoPricing(ctx context.Context, pricing []model.Pricing, usableGroups map[string]string) error {
	var policies []model.ImageGroupPolicy
	if err := model.DB.WithContext(ctx).Where("platform = ?", "mixed").Find(&policies).Error; err != nil {
		return err
	}
	groupsByModel := make(map[string][]model.ImageGroupPolicy)
	for _, policy := range policies {
		if _, usable := usableGroups[policy.Group]; !usable {
			continue
		}
		var catalog []service.ImageModelCapability
		if err := common.UnmarshalJsonStr(policy.Models, &catalog); err != nil {
			return err
		}
		for _, capability := range catalog {
			if capability.MediaType == "video" {
				groupsByModel[capability.Id] = append(groupsByModel[capability.Id], policy)
			}
		}
	}
	if len(groupsByModel) == 0 {
		return nil
	}
	cfg, err := service.GetMediaRuntimeConfig(ctx)
	if err != nil {
		return err
	}
	overrides := billing_setting.GetMixedVideoBillingExprCopy()
	for i := range pricing {
		groupPolicies := groupsByModel[pricing[i].ModelName]
		if len(groupPolicies) == 0 {
			continue
		}
		mixed := &model.PricingMixedVideo{Groups: []string{}, EnabledGroups: []string{}, Source: "unconfigured", BillingUsageSchema: billing_setting.MixedVideoUsageSchema()}
		expression, configured := billing_setting.GetMixedVideoBillingExpr(pricing[i].ModelName)
		if configured && billing_setting.ValidateMixedVideoBillingExpr(expression) == nil {
			mixed.Source = "model"
			if _, overridden := overrides[pricing[i].ModelName]; overridden {
				mixed.Source = "override"
			}
			mixed.BillingMode = billing_setting.BillingModeTieredExpr
			mixed.BillingExpr = expression
		}
		for _, policy := range groupPolicies {
			group := policy.Group
			if !slices.Contains(mixed.Groups, group) {
				mixed.Groups = append(mixed.Groups, group)
			}
			if cfg.VideoAsyncEnabled && policy.Enabled && policy.AsyncEnabled && !slices.Contains(mixed.EnabledGroups, group) {
				mixed.EnabledGroups = append(mixed.EnabledGroups, group)
			}
		}
		slices.Sort(mixed.Groups)
		slices.Sort(mixed.EnabledGroups)
		pricing[i].MixedVideoPricing = mixed
	}
	return nil
}

func filterPricingByUsableGroups(pricing []model.Pricing, usableGroup map[string]string) []model.Pricing {
	if len(pricing) == 0 {
		return pricing
	}
	if len(usableGroup) == 0 {
		return []model.Pricing{}
	}

	filtered := make([]model.Pricing, 0, len(pricing))
	for _, item := range pricing {
		if common.StringsContains(item.EnableGroup, "all") {
			filtered = append(filtered, item)
			continue
		}
		for _, group := range item.EnableGroup {
			if _, ok := usableGroup[group]; ok {
				filtered = append(filtered, item)
				break
			}
		}
	}
	return filtered
}

func GetPricing(c *gin.Context) {
	pricing := model.GetPricing()
	userId, exists := c.Get("id")
	usableGroup := map[string]string{}
	groupRatio := map[string]float64{}
	maps.Copy(groupRatio, ratio_setting.GetGroupRatioCopy())
	var group string
	if exists {
		user, err := model.GetUserCache(userId.(int))
		if err == nil {
			group = user.Group
			for g := range groupRatio {
				ratio, ok := ratio_setting.GetGroupGroupRatio(group, g)
				if ok {
					groupRatio[g] = ratio
				}
			}
		}
	}

	usableGroup = service.GetUserUsableGroups(group)
	pricing = filterPricingByUsableGroups(pricing, usableGroup)
	if err := addMixedVideoPricing(c.Request.Context(), pricing, usableGroup); err != nil {
		common.ApiError(c, err)
		return
	}
	// check groupRatio contains usableGroup
	for group := range ratio_setting.GetGroupRatioCopy() {
		if _, ok := usableGroup[group]; !ok {
			delete(groupRatio, group)
		}
	}

	c.JSON(200, gin.H{
		"success":            true,
		"data":               pricing,
		"vendors":            model.GetVendors(),
		"group_ratio":        groupRatio,
		"usable_group":       usableGroup,
		"supported_endpoint": model.GetSupportedEndpointMap(),
		"auto_groups":        service.GetUserAutoGroup(group),
		"pricing_version":    "a42d372ccf0b5dd13ecf71203521f9d2",
	})
}

func ResetModelRatio(c *gin.Context) {
	defaultStr := ratio_setting.DefaultModelRatio2JSONString()
	err := model.UpdateOption("ModelRatio", defaultStr)
	if err != nil {
		c.JSON(200, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	err = ratio_setting.UpdateModelRatioByJSONString(defaultStr)
	if err != nil {
		c.JSON(200, gin.H{
			"success": false,
			"message": err.Error(),
		})
		return
	}
	c.JSON(200, gin.H{
		"success": true,
		"message": "重置模型倍率成功",
	})
}
