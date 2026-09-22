package billing_setting

// Built-in token prices use actual USD per million tokens. Keep new model
// defaults here instead of splitting them across the legacy ratio tables.
var builtinBillingExpr = map[string]string{
	// https://developers.openai.com/api/docs/pricing (Standard, 2026-09-09).
	// The Images API reports image output in output_tokens, normalized to c.
	"gpt-image-2":            `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	"gpt-image-2.5-sunburst": `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	"gpt-image-2.5-flare":    `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`,
	// https://docs.x.ai/developers/pricing (video generation, 2026-09-21).
	// Task usage expressions return the request's USD cost directly.
	"grok-imagine-video-1.5":            `u("billing_source") == "upstream" ? tier("upstream", u("upstream_cost_credit")) : u("resolution") == "1080p" ? tier("1080p", u("seconds") * 0.25 + u("input_images") * 0.01) : u("resolution") == "720p" ? tier("720p", u("seconds") * 0.14 + u("input_images") * 0.01) : tier("480p", u("seconds") * 0.08 + u("input_images") * 0.01)`,
	"grok-imagine-video-1.5-preview":    `u("billing_source") == "upstream" ? tier("upstream", u("upstream_cost_credit")) : u("resolution") == "1080p" ? tier("1080p", u("seconds") * 0.25 + u("input_images") * 0.01) : u("resolution") == "720p" ? tier("720p", u("seconds") * 0.14 + u("input_images") * 0.01) : tier("480p", u("seconds") * 0.08 + u("input_images") * 0.01)`,
	"grok-imagine-video-1.5-2026-05-30": `u("billing_source") == "upstream" ? tier("upstream", u("upstream_cost_credit")) : u("resolution") == "1080p" ? tier("1080p", u("seconds") * 0.25 + u("input_images") * 0.01) : u("resolution") == "720p" ? tier("720p", u("seconds") * 0.14 + u("input_images") * 0.01) : tier("480p", u("seconds") * 0.08 + u("input_images") * 0.01)`,
	"grok-imagine-video":                `u("billing_source") == "upstream" ? tier("upstream", u("upstream_cost_credit")) : u("resolution") == "720p" ? tier("720p", u("seconds") * 0.07 + u("input_images") * 0.002 + u("input_video_seconds") * 0.01) : tier("480p", u("seconds") * 0.05 + u("input_images") * 0.002 + u("input_video_seconds") * 0.01)`,
	// https://developers.openai.com/api/docs/models/gpt-6-astra
	// Standard pricing; the long-context rates apply to the whole request.
	// Do not infer service-tier discounts from incoming request parameters:
	// channels filter service_tier by default, so it may not reach the upstream.
	"gpt-6-astra": `len <= 272000 ? tier("standard", p * 10 + c * 50 + cr * 1 + cc * 12.5) : tier("long_context", p * 20 + c * 75 + cr * 2 + cc * 25)`,
}
