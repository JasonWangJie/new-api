package billing_setting

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/expr-lang/expr/ast"
	"github.com/expr-lang/expr/parser"
)

// Adding optional host facts preserves version 1 snapshots whose expressions
// do not reference them. Never infer missing input facts for historical tasks.
const MixedVideoBillingSchemaVersion = 1

// MixedVideoUsageSchema describes request facts owned by the host. Provider
// meters never define or replace these quantities, including after recovery.
func MixedVideoUsageSchema() map[string]jsplugin.UsageFieldSchema {
	return map[string]jsplugin.UsageFieldSchema{
		"requests":   {Type: "number", Unit: "count", Description: jsplugin.LocalizedText{"en": "Video operation unit price", "zh": "视频操作单价"}},
		"seconds":    {Type: "number", Unit: "second", Description: jsplugin.LocalizedText{"en": "Video duration unit price", "zh": "视频时长单价"}},
		"resolution": {Enum: []string{"480p", "720p", "1080p", "2k", "4k"}, Description: jsplugin.LocalizedText{"en": "Output video resolution", "zh": "输出视频分辨率"}},
		"action":     {Enum: []string{"generate", "edit", "extend"}, Description: jsplugin.LocalizedText{"en": "Video operation", "zh": "视频操作"}},
		"video_input": {
			Enum:        []string{"none", "video"},
			Description: jsplugin.LocalizedText{"en": "Reference video input", "zh": "参考视频输入"},
			EnumLabels: map[string]jsplugin.LocalizedText{
				"none":  {"en": "No reference video", "zh": "无参考视频"},
				"video": {"en": "With reference video", "zh": "有参考视频"},
			},
		},
	}
}

func GetMixedVideoBillingExpr(model string) (string, bool) {
	expression, ok := billingSetting.MixedVideoBillingExpr[model]
	return expression, ok && strings.TrimSpace(expression) != ""
}

func GetMixedVideoBillingExprCopy() map[string]string {
	expressions := maps.Clone(billingSetting.MixedVideoBillingExpr)
	if expressions == nil {
		expressions = make(map[string]string)
	}
	return expressions
}

// ValidateMixedVideoBillingExpr rejects provider meters and request probes.
// Only literal host fact keys can affect the public price of a mixed task.
func ValidateMixedVideoBillingExpr(expression string) error {
	if err := validateMixedVideoBillingContract(expression); err != nil {
		return err
	}
	schema := MixedVideoUsageSchema()
	for _, seconds := range []float64{0, 1, relaycommon.MaxTaskDurationSeconds} {
		for _, resolution := range schema["resolution"].Enum {
			for _, action := range schema["action"].Enum {
				for _, videoInput := range schema["video_input"].Enum {
					facts := map[string]any{"requests": float64(1), "seconds": seconds, "resolution": resolution, "action": action, "video_input": videoInput}
					cost, _, err := billingexpr.RunExprWithRequest(expression, billingexpr.TokenParams{}, billingexpr.RequestInput{Usage: facts})
					if err != nil {
						return err
					}
					if math.IsNaN(cost) || math.IsInf(cost, 0) || cost < 0 {
						return fmt.Errorf("mixed video billing cost must be finite and non-negative")
					}
				}
			}
		}
	}
	return nil
}

func validateMixedVideoBillingContract(expression string) error {
	if strings.TrimSpace(expression) == "" {
		return fmt.Errorf("mixed video billing expression is required")
	}
	_, body := billingexpr.ParseExprVersion(expression)
	tree, err := parser.Parse(body)
	if err != nil {
		return err
	}
	schema := MixedVideoUsageSchema()
	var invalid error
	ast.Find(tree.Node, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.IdentifierNode:
			switch value.Value {
			case "u", "tier", "max", "min", "ceil", "floor", "abs", "has":
			default:
				invalid = fmt.Errorf("mixed video billing does not support %q", value.Value)
			}
		case *ast.CallNode:
			callee, ok := value.Callee.(*ast.IdentifierNode)
			if !ok {
				invalid = fmt.Errorf("mixed video billing requires named functions")
				break
			}
			if callee.Value == "u" {
				if len(value.Arguments) != 1 {
					invalid = fmt.Errorf("mixed video billing requires literal usage keys")
					break
				}
				literal, ok := value.Arguments[0].(*ast.StringNode)
				if !ok {
					invalid = fmt.Errorf("mixed video billing requires literal usage keys")
					break
				}
				if _, declared := schema[literal.Value]; !declared {
					invalid = fmt.Errorf("mixed video billing fact %q is unavailable", literal.Value)
				}
			}
		}
		return invalid != nil
	})
	if invalid != nil {
		return invalid
	}
	if _, err := billingexpr.CompileFromCache(expression); err != nil {
		return err
	}
	return nil
}

// ValidateMixedVideoBillingFacts permits absent optional facts only when the
// expression does not reference them. It never supplies provider defaults.
func ValidateMixedVideoBillingFacts(expression string, facts map[string]any) error {
	if err := validateMixedVideoBillingContract(expression); err != nil {
		return err
	}
	schema := MixedVideoUsageSchema()
	for key, value := range facts {
		field, declared := schema[key]
		if !declared {
			return fmt.Errorf("mixed video billing fact %q is unavailable", key)
		}
		if len(field.Enum) > 0 {
			text, ok := value.(string)
			if !ok || !slices.Contains(field.Enum, text) {
				return fmt.Errorf("invalid mixed video billing fact %q", key)
			}
			continue
		}
		number, ok := value.(float64)
		if !ok || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 || number != math.Trunc(number) || key == "requests" && number != 1 || key == "seconds" && number > relaycommon.MaxTaskDurationSeconds {
			return fmt.Errorf("invalid mixed video billing fact %q", key)
		}
	}
	if facts["requests"] != float64(1) {
		return fmt.Errorf("mixed video billing requires one request")
	}
	if _, present := facts["action"]; !present {
		return fmt.Errorf("mixed video billing action is unavailable")
	}
	for key := range billingexpr.UsedUsageKeys(expression) {
		if _, present := facts[key]; !present {
			return fmt.Errorf("mixed video billing fact %q cannot be determined from this request", key)
		}
	}
	return nil
}
