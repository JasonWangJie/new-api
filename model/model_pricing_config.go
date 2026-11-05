package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PricingValues is one model's configuration, keyed by the existing option
// names. A missing key inherits the engine's default; an explicit zero is free.
type PricingValues map[string]any

type ModelPricingChange struct {
	ModelName       string        `json:"model_name"`
	ExpectedVersion string        `json:"expected_version"`
	Pricing         PricingValues `json:"pricing"`
	Reset           bool          `json:"reset,omitempty"`
}

type ModelPricingEntry struct {
	ModelPricingDescription
	MixedVideo     ModelPricingMixedVideo               `json:"mixed_video"`
	PluginVariants []ModelPricingPluginVariant          `json:"plugin_variants,omitempty"`
	ModelName      string                               `json:"model_name"`
	Version        string                               `json:"version"`
	Configured     PricingValues                        `json:"configured"`
	UsageSchema    map[string]jsplugin.UsageFieldSchema `json:"usage_schema,omitempty"`
}

type ModelPricingMixedVideo struct {
	Eligible    bool                                 `json:"eligible"`
	Valid       bool                                 `json:"valid"`
	Expression  string                               `json:"expression"`
	Source      string                               `json:"source"`
	UsageSchema map[string]jsplugin.UsageFieldSchema `json:"usage_schema"`
}

type ModelPricingPluginVariant struct {
	PluginKey     string                               `json:"plugin_key"`
	PluginName    string                               `json:"plugin_name"`
	Icon          string                               `json:"icon,omitempty"`
	UsageSchema   map[string]jsplugin.UsageFieldSchema `json:"usage_schema"`
	UsageExamples []jsplugin.UsageExample              `json:"usage_examples,omitempty"`
	Configured    string                               `json:"configured"`
	Effective     string                               `json:"effective"`
	Compatible    bool                                 `json:"compatible"`
	Stale         bool                                 `json:"stale,omitempty"`
}

type ModelPricingSnapshot struct {
	Entries      []ModelPricingEntry `json:"entries"`
	Options      map[string]string   `json:"options"`
	EmptyVersion string              `json:"empty_version"`
}

var ErrModelPricingConflict = errors.New("model pricing changed; reload before saving")

// Lock order is stable across instances. Creating missing option rows inside
// the transaction also serializes the first write to an unconfigured database.
var modelPricingOptionKeys = []string{
	"AudioCompletionRatio", "AudioRatio", "CacheRatio", "CompletionRatio",
	"CreateCacheRatio", "ImageRatio", "ModelPrice", "ModelRatio",
	"billing_setting.billing_expr", "billing_setting.billing_mode", billing_setting.MixedVideoBillingExprOption, billing_setting.PluginBillingExprOption,
}

var modelPricingMutationMu sync.Mutex

func IsModelPricingOption(key string) bool {
	return slices.Contains(modelPricingOptionKeys, key)
}

func ModelPricingVersion(values PricingValues) string {
	encoded, _ := common.Marshal(values)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func defaultPricingMaps() map[string]map[string]any {
	result := make(map[string]map[string]any, len(modelPricingOptionKeys))
	for _, key := range modelPricingOptionKeys {
		result[key] = make(map[string]any)
	}
	for key, values := range ratio_setting.GetDefaultPricingMaps() {
		for name, value := range values {
			result[key][name] = value
		}
	}
	return result
}

func readModelPricingMaps(db *gorm.DB) (map[string]map[string]any, map[string]bool, []string, error) {
	var rows []Option
	if err := db.Where(map[string]any{"key": modelPricingOptionKeys}).Find(&rows).Error; err != nil {
		return nil, nil, nil, err
	}
	values := defaultPricingMaps()
	existing := make(map[string]bool)
	counts := make(map[string]int)
	for _, row := range rows {
		var entries map[string]any
		if err := common.UnmarshalJsonStr(row.Value, &entries); err != nil {
			return nil, nil, nil, fmt.Errorf("%s: %w", row.Key, err)
		}
		if entries == nil {
			return nil, nil, nil, fmt.Errorf("%s must be a JSON object", row.Key)
		}
		values[row.Key] = entries
		existing[row.Key] = true
		counts[row.Key]++
	}
	var duplicated []string
	for _, key := range modelPricingOptionKeys {
		if counts[key] > 1 {
			duplicated = append(duplicated, key)
		}
	}
	return values, existing, duplicated, nil
}

func modelPricingValues(values map[string]map[string]any, name string) PricingValues {
	result := make(PricingValues)
	for _, key := range modelPricingOptionKeys {
		if key == billing_setting.PluginBillingExprOption {
			variants := make(map[string]any)
			for variant, expression := range values[key] {
				if plugin, model, ok := billing_setting.SplitPluginBillingExprKey(variant); ok && model == name {
					variants[plugin] = expression
				}
			}
			if len(variants) > 0 {
				result[key] = variants
			}
			continue
		}
		if value, exists := values[key][name]; exists {
			result[key] = value
		}
	}
	return result
}

// replaceModelPricing writes one complete model draft into the option maps.
// Plugin expressions are grouped in the draft and flattened only in storage.
func replaceModelPricing(values map[string]map[string]any, name string, draft PricingValues) {
	for _, key := range modelPricingOptionKeys {
		if key == billing_setting.PluginBillingExprOption {
			for variant := range values[key] {
				if _, model, ok := billing_setting.SplitPluginBillingExprKey(variant); ok && model == name {
					delete(values[key], variant)
				}
			}
			variants, _ := draft[key].(map[string]any)
			for plugin, expr := range variants {
				values[key][billing_setting.PluginBillingExprKey(plugin, name)] = expr
			}
			continue
		}
		delete(values[key], name)
		if value, exists := draft[key]; exists {
			values[key][name] = value
		}
	}
}

func effectiveModelPricing(values map[string]map[string]any, name string) PricingValues {
	result := modelPricingValues(values, name)
	// Legacy wildcard aliases are resolved by the same normalization as relay.
	alias := ratio_setting.FormatMatchingModelName(name)
	for _, key := range []string{"ModelPrice", "ModelRatio", "CompletionRatio", "AudioRatio", "AudioCompletionRatio"} {
		delete(result, key)
		if value, exists := values[key][alias]; exists {
			result[key] = value
		}
	}
	mode, _ := result["billing_setting.billing_mode"].(string)
	if mode == "" {
		_, hasPrice := result["ModelPrice"]
		_, hasRatio := result["ModelRatio"]
		if _, builtin := billing_setting.GetBuiltinBillingExpr(name); builtin && !hasPrice && !hasRatio {
			mode = "tiered_expr"
		}
	}
	if mode == "tiered_expr" {
		result["billing_setting.billing_mode"] = mode
		if _, exists := result["billing_setting.billing_expr"]; !exists {
			if expression, ok := billing_setting.GetBuiltinBillingExpr(name); ok {
				result["billing_setting.billing_expr"] = expression
			}
		}
		return result
	}
	if _, exists := result["ModelPrice"]; exists {
		return result
	}
	if _, exists := result["ModelRatio"]; !exists && operation_setting.SelfUseModeEnabled {
		result["ModelRatio"] = float64(37.5)
	}
	// Completion ratios include engine-enforced model defaults. Expose their
	// effective value without persisting them into the editable configuration.
	var configuredCompletion *float64
	if ratio, exists := result["CompletionRatio"].(float64); exists {
		configuredCompletion = &ratio
	}
	result["CompletionRatio"] = ratio_setting.ResolveCompletionRatio(name, configuredCompletion).Ratio
	for key, fallback := range map[string]float64{
		"CacheRatio":       ratio_setting.DefaultCacheRatio,
		"CreateCacheRatio": ratio_setting.DefaultCreateCacheRatio,
		"ImageRatio":       ratio_setting.DefaultImageRatio,
	} {
		if _, exists := result[key]; !exists {
			result[key] = fallback
		}
	}
	return result
}

// PreviewModelPricing resolves a complete editable draft using the same defaults
// as the saved-price display and conversion. It has no write side effects.
func PreviewModelPricing(name string, draft PricingValues) (PricingValues, error) {
	if draft == nil {
		return nil, errors.New("pricing draft is required")
	}
	values, _, _, err := readModelPricingMaps(DB)
	if err != nil {
		return nil, err
	}
	if err := validateModelPricing(name, draft, modelPricingValues(values, name)); err != nil {
		return nil, err
	}
	replaceModelPricing(values, name, draft)
	return effectiveModelPricing(values, name), nil
}

func GetModelPricingSnapshot(names []string) (*ModelPricingSnapshot, error) {
	values, _, _, err := readModelPricingMaps(DB)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		nameSet := make(map[string]bool)
		for key, entries := range values {
			for name := range entries {
				if key == billing_setting.PluginBillingExprOption {
					_, model, ok := billing_setting.SplitPluginBillingExprKey(name)
					if !ok {
						continue
					}
					name = model
				}
				nameSet[name] = true
			}
		}
		for name := range billing_setting.GetBuiltinBillingExprCopy() {
			nameSet[name] = true
		}
		for name := range nameSet {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	result := &ModelPricingSnapshot{Entries: make([]ModelPricingEntry, 0, len(names)), Options: make(map[string]string), EmptyVersion: ModelPricingVersion(PricingValues{})}
	eligibleMixed := make(map[string]bool)
	if DB.Migrator().HasTable(&ImageGroupPolicy{}) {
		var policies []ImageGroupPolicy
		if err := DB.Where("platform = ?", "mixed").Find(&policies).Error; err != nil {
			return nil, err
		}
		for _, policy := range policies {
			var catalog []struct {
				ID        string `json:"id"`
				MediaType string `json:"media_type"`
			}
			if err := common.UnmarshalJsonStr(policy.Models, &catalog); err != nil {
				return nil, err
			}
			for _, capability := range catalog {
				if capability.MediaType == "video" {
					eligibleMixed[capability.ID] = true
				}
			}
		}
	}
	generation := jsplugin.DefaultRegistry.Generation()
	for _, name := range names {
		configured := modelPricingValues(values, name)
		entry := ModelPricingEntry{ModelName: name, Version: ModelPricingVersion(configured), Configured: configured,
			ModelPricingDescription: ModelPricingDescription{Effective: effectiveModelPricing(values, name)}}
		entry.MixedVideo = ModelPricingMixedVideo{Eligible: eligibleMixed[name], Source: "unconfigured", UsageSchema: billing_setting.MixedVideoUsageSchema()}
		if expression, ok := configured[billing_setting.MixedVideoBillingExprOption].(string); ok {
			entry.MixedVideo.Expression, entry.MixedVideo.Source = expression, "override"
		} else if entry.Effective["billing_setting.billing_mode"] == billing_setting.BillingModeTieredExpr {
			if expression, ok := entry.Effective["billing_setting.billing_expr"].(string); ok {
				entry.MixedVideo.Expression, entry.MixedVideo.Source = expression, "model"
			}
		}
		entry.MixedVideo.Valid = (entry.MixedVideo.Eligible || entry.MixedVideo.Source == "override") && entry.MixedVideo.Source != "unconfigured" && billing_setting.ValidateMixedVideoBillingExpr(entry.MixedVideo.Expression) == nil
		entry.CacheWriteMode = ResolveCacheWriteMode(name, configured)
		entry.BillingDetails = ResolveLegacyBillingDetails(name, entry.Effective, configured)
		if plugin, ok := generation.GetByModel(name); ok {
			entry.UsageSchema, _ = plugin.Meta.UsageForModel(name)
		} else if target, ok := ResolveTaskModelAlias(generation, name); ok {
			if plugin, ok := generation.Get(target.PluginKey); ok {
				entry.UsageSchema, _ = plugin.Meta.UsageForModel(target.Declared)
			}
		}
		plugins := generation.PluginsByModel(name)
		configuredVariants, _ := configured[billing_setting.PluginBillingExprOption].(map[string]any)
		if len(plugins) >= 2 || len(configuredVariants) > 0 {
			keys := make(map[string]bool, len(plugins)+len(configuredVariants))
			for _, plugin := range plugins {
				keys[plugin.Meta.Key] = true
			}
			for key := range configuredVariants {
				keys[key] = true
			}
			for _, key := range slices.Sorted(maps.Keys(keys)) {
				configuredValue, overridden := configuredVariants[key]
				configuredExpr, _ := configuredValue.(string)
				plugin, exists := generation.Get(key)
				if !exists || !slices.Contains(plugin.Meta.Models, name) {
					variant := ModelPricingPluginVariant{
						PluginKey: key, PluginName: key, Configured: configuredExpr,
						UsageSchema: map[string]jsplugin.UsageFieldSchema{}, Stale: true,
					}
					if exists {
						variant.PluginName, variant.Icon = plugin.Meta.Name, plugin.Meta.Icon
					}
					entry.PluginVariants = append(entry.PluginVariants, variant)
					continue
				}
				schema, examples := plugin.Meta.UsageForModel(name)
				if schema == nil {
					schema = map[string]jsplugin.UsageFieldSchema{}
				}
				expression := configuredExpr
				if !overridden && entry.Effective["billing_setting.billing_mode"] == billing_setting.BillingModeTieredExpr {
					expression, _ = entry.Effective["billing_setting.billing_expr"].(string)
				}
				entry.PluginVariants = append(entry.PluginVariants, ModelPricingPluginVariant{
					PluginKey: plugin.Meta.Key, PluginName: plugin.Meta.Name, Icon: plugin.Meta.Icon,
					UsageSchema: schema, UsageExamples: examples, Configured: configuredExpr, Effective: expression,
					Compatible: billing_setting.TaskExprCompatible(expression, schema),
				})
			}
		}
		result.Entries = append(result.Entries, entry)
	}
	// Preserve the existing settings editor's full-map interface. Built-in
	// expressions are display defaults only; per-model writes do not persist them.
	for name, expression := range billing_setting.GetBuiltinBillingExprCopy() {
		effective := effectiveModelPricing(values, name)
		if effective["billing_setting.billing_mode"] != "tiered_expr" {
			continue
		}
		if _, ok := values["billing_setting.billing_mode"][name]; !ok {
			values["billing_setting.billing_mode"][name] = "tiered_expr"
		}
		if _, ok := values["billing_setting.billing_expr"][name]; !ok {
			values["billing_setting.billing_expr"][name] = expression
		}
	}
	for key, entries := range values {
		encoded, err := common.Marshal(entries)
		if err != nil {
			return nil, err
		}
		result.Options[key] = string(encoded)
	}
	return result, nil
}

func ValidateModelPricing(name string, values PricingValues) error {
	variants := make(map[string]any)
	for key, expression := range billing_setting.GetPluginBillingExprCopy() {
		if plugin, model, ok := billing_setting.SplitPluginBillingExprKey(key); ok && model == name {
			variants[plugin] = expression
		}
	}
	previous := PricingValues{billing_setting.PluginBillingExprOption: variants}
	if expression, ok := billing_setting.GetBillingExpr(name); ok {
		previous["billing_setting.billing_expr"] = expression
	}
	return validateModelPricing(name, values, previous)
}

// Writes pass the locked database snapshot here, so allowing an unchanged stale
// override cannot bypass validation through an out-of-date process-local cache.
func validateModelPricing(name string, values, previous PricingValues) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("model name is required")
	}
	currentMixedValue, currentMixedExists := values[billing_setting.MixedVideoBillingExprOption]
	previousMixedValue, previousMixedExists := previous[billing_setting.MixedVideoBillingExprOption]
	currentMixed, _ := common.Marshal(currentMixedValue)
	previousMixed, _ := common.Marshal(previousMixedValue)
	mixedChanged := currentMixedExists != previousMixedExists || string(currentMixed) != string(previousMixed)
	if currentMixedExists && mixedChanged {
		expression, ok := currentMixedValue.(string)
		if !ok {
			return fmt.Errorf("model %s: mixed video billing expression must be a string", name)
		}
		if err := billing_setting.ValidateMixedVideoBillingExpr(expression); err != nil {
			return fmt.Errorf("model %s: mixed video billing: %w", name, err)
		}
	}
	standard, oldStandard := maps.Clone(values), maps.Clone(previous)
	delete(standard, billing_setting.MixedVideoBillingExprOption)
	delete(oldStandard, billing_setting.MixedVideoBillingExprOption)
	currentJSON, _ := common.Marshal(standard)
	previousJSON, _ := common.Marshal(oldStandard)
	if string(currentJSON) == string(previousJSON) && mixedChanged {
		return nil
	}
	generation := jsplugin.DefaultRegistry.Generation()
	previousVariants, _ := previous[billing_setting.PluginBillingExprOption].(map[string]any)
	variants := map[string]any{}
	if value, exists := values[billing_setting.PluginBillingExprOption]; exists {
		var ok bool
		variants, ok = value.(map[string]any)
		if !ok || variants == nil {
			return errors.New("plugin billing expressions must be a plugin-to-expression object")
		}
		for key, value := range variants {
			expression, ok := value.(string)
			if !ok || strings.TrimSpace(expression) == "" {
				return fmt.Errorf("model %s: plugin %s: billing expression is required", name, key)
			}
			plugin, exists := generation.Get(key)
			if !exists || !slices.Contains(plugin.Meta.Models, name) {
				if previousVariants[key] == expression {
					continue
				}
				return fmt.Errorf("model %s: plugin %s does not declare this model", name, key)
			}
			schema, _ := plugin.Meta.UsageForModel(name)
			if err := billing_setting.SmokeTestTaskExpr(expression, schema); err != nil {
				return fmt.Errorf("model %s: plugin %s: %w", name, key, err)
			}
		}
	}
	for key, value := range values {
		if key == billing_setting.PluginBillingExprOption || key == billing_setting.MixedVideoBillingExprOption {
			continue
		}
		if !IsModelPricingOption(key) {
			return fmt.Errorf("unsupported pricing field: %s", key)
		}
		if key == "billing_setting.billing_mode" {
			if value != "ratio" && value != "tiered_expr" {
				return errors.New("invalid billing mode")
			}
			continue
		}
		if key == "billing_setting.billing_expr" {
			expression, ok := value.(string)
			if !ok || strings.TrimSpace(expression) == "" {
				return errors.New("billing expression is required")
			}
			// Even a model expression currently shadowed by every provider must
			// compile; only its schema-specific smoke tests can be skipped.
			if _, err := billingexpr.CompileFromCache(expression); err != nil {
				return fmt.Errorf("model %s: %w", name, err)
			}
			var err error
			if plugins := generation.PluginsByModel(name); len(plugins) > 0 {
				for _, plugin := range plugins {
					if _, overridden := variants[plugin.Meta.Key]; overridden {
						continue
					}
					schema, _ := plugin.Meta.UsageForModel(name)
					if err = billing_setting.SmokeTestTaskExpr(expression, schema); err != nil {
						return fmt.Errorf("model %s: plugin %s: %w", name, plugin.Meta.Key, err)
					}
				}
			} else if target, resolved := ResolveTaskModelAlias(generation, name); resolved {
				if plugin, ok := generation.Get(target.PluginKey); ok {
					schema, _ := plugin.Meta.UsageForModel(target.Declared)
					err = billing_setting.SmokeTestTaskExpr(expression, schema)
				} else {
					err = billing_setting.SmokeTestExpr(expression)
				}
			} else if previous[key] != expression || len(billingexpr.UsedUsageKeys(expression)) == 0 {
				err = billing_setting.SmokeTestExpr(expression)
			}
			// With no remaining plugin, an unchanged stored usage expression has
			// no schema to test. Preserve it so removing stale overrides or saving
			// other model prices does not become impossible.
			if err != nil {
				return fmt.Errorf("model %s: %w", name, err)
			}
			continue
		}
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
			return fmt.Errorf("%s must be a finite, non-negative number", key)
		}
	}
	if values["billing_setting.billing_mode"] == "tiered_expr" {
		if _, exists := values["billing_setting.billing_expr"]; !exists {
			if _, builtin := billing_setting.GetBuiltinBillingExpr(name); !builtin {
				return errors.New("billing expression is required")
			}
		}
	}
	return nil
}

type MixedVideoPricingValidator func(tx *gorm.DB, resolve func(string) (string, bool)) error

func validateMixedVideoPricingDraft(tx *gorm.DB, values, previous map[string]map[string]any, validators []MixedVideoPricingValidator) error {
	resolve := func(prices map[string]map[string]any, name string) (string, bool) {
		if expression, ok := prices[billing_setting.MixedVideoBillingExprOption][name].(string); ok {
			return expression, true
		}
		effective := effectiveModelPricing(prices, name)
		if effective["billing_setting.billing_mode"] != billing_setting.BillingModeTieredExpr {
			return "", false
		}
		expression, ok := effective["billing_setting.billing_expr"].(string)
		return expression, ok
	}
	for _, validate := range validators {
		if err := validate(tx, func(name string) (string, bool) {
			expression, ok := resolve(values, name)
			oldExpression, oldOK := resolve(previous, name)
			if expression == oldExpression && ok == oldOK {
				return "", false
			}
			return expression, ok
		}); err != nil {
			return err
		}
	}
	return nil
}

func UpdateModelPricing(changes []ModelPricingChange, validators ...MixedVideoPricingValidator) error {
	if len(changes) == 0 {
		return errors.New("select model pricing changes before saving")
	}
	seen := make(map[string]bool)
	for _, change := range changes {
		if seen[change.ModelName] {
			return errors.New("duplicate model pricing change")
		}
		seen[change.ModelName] = true
		if change.ExpectedVersion == "" {
			return ErrModelPricingConflict
		}
	}
	return mutateModelPricingOptions(func(tx *gorm.DB, values map[string]map[string]any) error {
		previousValues := make(map[string]map[string]any, len(values))
		for key, entries := range values {
			previousValues[key] = maps.Clone(entries)
		}
		defaults := defaultPricingMaps()
		for _, change := range changes {
			previous := modelPricingValues(values, change.ModelName)
			if ModelPricingVersion(previous) != change.ExpectedVersion {
				return fmt.Errorf("%w: %s", ErrModelPricingConflict, change.ModelName)
			}
			pricing := change.Pricing
			if change.Reset {
				pricing = modelPricingValues(defaults, change.ModelName)
			}
			if err := validateModelPricing(change.ModelName, pricing, previous); err != nil {
				return err
			}
			replaceModelPricing(values, change.ModelName, pricing)
		}
		return validateMixedVideoPricingDraft(tx, values, previousValues, validators)
	})
}

// UpdateModelPricingOptions keeps legacy single-option callers on the same
// locking, validation and transaction path as the model-level API.
func UpdateModelPricingOptions(updates map[string]string, validators ...MixedVideoPricingValidator) error {
	return mutateModelPricingOptions(func(tx *gorm.DB, values map[string]map[string]any) error {
		previous := maps.Clone(values)
		names := make(map[string]bool)
		for key, raw := range updates {
			if !IsModelPricingOption(key) {
				return fmt.Errorf("unsupported pricing field: %s", key)
			}
			var entries map[string]any
			if err := common.UnmarshalJsonStr(raw, &entries); err != nil {
				return err
			}
			if entries == nil {
				return fmt.Errorf("%s must be a JSON object", key)
			}
			for _, entriesForKey := range []map[string]any{values[key], entries} {
				for name := range entriesForKey {
					if key == billing_setting.PluginBillingExprOption {
						_, model, ok := billing_setting.SplitPluginBillingExprKey(name)
						if !ok {
							return fmt.Errorf("invalid plugin billing expression key: %s", name)
						}
						name = model
					}
					names[name] = true
				}
			}
			values[key] = entries
		}
		for name := range names {
			if len(updates) == 1 {
				if _, mixedOnly := updates[billing_setting.MixedVideoBillingExprOption]; mixedOnly {
					oldValue, oldExists := previous[billing_setting.MixedVideoBillingExprOption][name]
					currentValue, currentExists := values[billing_setting.MixedVideoBillingExprOption][name]
					old, _ := common.Marshal(oldValue)
					current, _ := common.Marshal(currentValue)
					if oldExists == currentExists && string(old) == string(current) {
						continue
					}
					if expression, exists := values[billing_setting.MixedVideoBillingExprOption][name]; exists {
						text, ok := expression.(string)
						if !ok {
							return fmt.Errorf("model %s: mixed video billing expression must be a string", name)
						}
						if err := billing_setting.ValidateMixedVideoBillingExpr(text); err != nil {
							return fmt.Errorf("model %s: mixed video billing: %w", name, err)
						}
					}
					continue
				}
			}
			if err := validateModelPricing(name, modelPricingValues(values, name), modelPricingValues(previous, name)); err != nil {
				return err
			}
		}
		return validateMixedVideoPricingDraft(tx, values, previous, validators)
	})
}

func mutateModelPricingOptions(mutate func(*gorm.DB, map[string]map[string]any) error) error {
	modelPricingMutationMu.Lock()
	defer modelPricingMutationMu.Unlock()
	var committed map[string]map[string]any
	err := DB.Transaction(func(tx *gorm.DB) error {
		values, existing, duplicated, err := readModelPricingMaps(lockForUpdate(tx))
		if err != nil {
			return err
		}
		if len(duplicated) > 0 {
			common.SysError("options table has duplicate pricing keys [" + strings.Join(duplicated, ", ") + "]; the table is missing a primary key")
		}
		defaults := defaultPricingMaps()
		for _, key := range modelPricingOptionKeys {
			if existing[key] {
				continue
			}
			encoded, err := common.Marshal(defaults[key])
			if err != nil {
				return err
			}
			row := Option{Key: key, Value: string(encoded)}
			if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
				return err
			}
		}
		if err := mutate(tx, values); err != nil {
			return err
		}
		for _, key := range modelPricingOptionKeys {
			encoded, err := common.Marshal(values[key])
			if err != nil {
				return err
			}
			if err := tx.Model(&Option{}).Where(map[string]any{"key": key}).Update("value", string(encoded)).Error; err != nil {
				return err
			}
		}
		committed = values
		return nil
	})
	if err != nil {
		return err
	}
	for _, key := range modelPricingOptionKeys {
		encoded, _ := common.Marshal(committed[key])
		if err := updateOptionMap(key, string(encoded)); err != nil {
			return err
		}
	}
	RefreshPricing()
	ratio_setting.InvalidateExposedDataCache()
	return nil
}
