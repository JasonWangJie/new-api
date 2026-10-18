package service

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
)

var (
	videoFailureURLPattern        = regexp.MustCompile(`(?i)\b(?:https?|ftp|s3|gs|file|blob|data):[^\s<>"']+`)
	videoFailureCredentialPattern = regexp.MustCompile(`(?i)(\b(?:token|refresh[_ -]?token|signature|sig|passwd|password|api[_ -]?key|access[_ -]?token|secret|authorization)\b["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*"|'(?:\\.|[^'\\])*'|[^\s,;"'}]+)`)
	videoFailureAuthHeaderPattern = regexp.MustCompile(`(?i)(\bauthorization\b["']?\s*[:=]\s*["']?)(?:basic|bearer|digest|token)\s+[^\r\n,;"'}]+`)
	videoFailureRequestPattern    = regexp.MustCompile(`(?i)(?:^|[;:\r\n])\s*(?:request[_ -]?(?:body|payload)|payload)\b["']?\s*[:=]\s*`)
	videoFailureContentPattern    = regexp.MustCompile(`\bcontent\[[0-9]{1,10}\]`)
	videoFailureRatioPattern      = regexp.MustCompile(`(?is)aspect ratio of .+?\s+is\s+([0-9]{1,8}(?:\.[0-9]{1,32})?)\.\s*Required\s+([0-9]{1,8}(?:\.[0-9]{1,32})?)\s*[-–—]\s*([0-9]{1,8}(?:\.[0-9]{1,32})?)`)
	videoFailureDimensionsPattern = regexp.MustCompile(`(?is)expected the (width|height) to be (at most|at least) ([0-9]{1,8})px.*?received (?:a )?([0-9]{1,8})x([0-9]{1,8})px`)
)

// VideoFailureNotice describes a failed video task without changing its stored
// status, accounting, retry eligibility or provider response.
type VideoFailureNotice struct {
	Summary    string
	Suggestion string
	Detail     string
}

// VideoTaskFailureNotice keeps a bounded, sanitized diagnostic alongside a
// plain-language explanation. Unknown errors do not imply a content category,
// a retry policy or a successful refund.
func VideoTaskFailureNotice(raw string) VideoFailureNotice {
	// The original diagnostic may have been shortened for public display after
	// classification. Restore the explanation itself on a second presentation;
	// classifying its shortened detail could lose the actual cause.
	if summary, suffix, rendered := strings.Cut(raw, "\n处理建议："); rendered {
		suggestion, detail, hasDetail := strings.Cut(suffix, "\n原始错误：")
		if hasDetail || summary == "视频任务失败，未提供具体错误原因。" {
			return sanitizeVideoFailureNotice(VideoFailureNotice{Summary: summary, Suggestion: suggestion, Detail: detail})
		}
	}
	// Classify the complete sanitized diagnostic before bounding the public
	// detail, so a concrete cause after a long prefix is still explained.
	raw = sanitizeVideoFailureDiagnostic(raw, -1)
	notice := VideoFailureNotice{
		Summary:    "视频任务失败，未提供可识别的进一步原因。",
		Suggestion: "请保留任务号和原始错误，联系管理员核查；当前信息不足以确定具体原因。",
		Detail:     raw,
	}
	lower := strings.ToLower(raw)
	location := ""
	if content := videoFailureContentPattern.FindString(raw); content != "" {
		location = "（上游位置 " + content + "，不等同于参考素材序号）"
	}
	imageInput := strings.Contains(lower, "input image")
	videoInput := strings.Contains(lower, "input video")
	person := strings.Contains(lower, "may contain real person") || strings.Contains(lower, "contains real person")
	image := strings.Contains(lower, "image") || strings.Contains(raw, "图片")
	material := "素材"
	if image {
		material = "图片"
	} else if strings.Contains(lower, "video") || strings.Contains(raw, "视频") {
		material = "视频"
	}
	switch {
	case isMissingVideoFailureReason(raw):
		notice.Summary = "视频任务失败，未提供具体错误原因。"
		notice.Suggestion = "请保留任务号并联系管理员检查任务记录；当前没有更多可确认的失败原因。"
	case strings.Contains(lower, "generated video could not be saved locally") || lower == "storage_failed":
		notice.Summary = "视频结果未能保存到本地。"
		notice.Suggestion = "请联系管理员检查本地存储，并优先恢复现有视频结果的保存；不要重复发起视频生成。"
	case strings.Contains(lower, "billing settlement failed") || lower == "billing_failed":
		notice.Summary = "视频任务结算失败。"
		notice.Suggestion = "请保留任务号并联系管理员核查结算记录；此错误不能用于判断上游是否已生成视频。"
	case imageInput && person:
		notice.Summary = "上游审核认为输入图片" + location + "可能包含真人，已拒绝生成视频。"
		notice.Suggestion = "请确认素材符合上游规则，并更换允许使用的输入图片。"
	case videoInput && person:
		notice.Summary = "上游审核认为输入视频" + location + "可能包含真人，已拒绝生成视频。"
		notice.Suggestion = "请确认素材符合上游规则，并更换允许使用的输入视频。"
	case strings.Contains(lower, "output audio") && strings.Contains(lower, "copyright"):
		notice.Summary = "上游审核认为生成的音频可能涉及版权限制，已拦截输出；未提供具体音频片段。"
		notice.Suggestion = "请检查音频参考素材和提示词是否符合上游版权规则，并使用允许使用的音频素材。"
	case strings.Contains(lower, "output video") && strings.Contains(lower, "sensitive"):
		notice.Summary = "上游审核认为生成的视频可能包含敏感内容，已拦截输出；未提供具体敏感类别或画面位置。"
		notice.Suggestion = "请检查提示词和参考素材是否符合上游内容规则，调整后再提交。"
	case strings.Contains(raw, "内容安全审核") || strings.Contains(lower, "content policy") || strings.Contains(lower, "content moderation") || strings.Contains(lower, "safety filter"):
		notice.Summary = "上游内容安全审核未通过，任务被拦截；未提供具体触发内容。"
		notice.Suggestion = "请检查提示词和参考素材是否符合上游内容规则。"
	case videoFailureRatioPattern.MatchString(raw):
		match := videoFailureRatioPattern.FindStringSubmatch(raw)
		bounds := match[2] + "–" + match[3]
		notice.Summary = "输入" + material + "的宽高比不符合上游要求的 " + bounds + "。"
		if ratio, err := strconv.ParseFloat(match[1], 64); err == nil {
			notice.Summary = fmt.Sprintf("输入%s的宽高比为 %.3f，超出上游要求的 %s。", material, ratio, bounds)
		}
		notice.Suggestion = "请裁剪" + material + "或补边，使宽÷高处于 " + bounds + "，再提交。"
	case videoFailureDimensionsPattern.MatchString(raw):
		match := videoFailureDimensionsPattern.FindStringSubmatch(raw)
		dimension, relation, action := "宽度", "超过上游上限", "缩小"
		if strings.EqualFold(match[1], "height") {
			dimension = "高度"
		}
		if strings.EqualFold(match[2], "at least") {
			relation, action = "低于上游下限", "增大"
		}
		notice.Summary = "输入" + material + "尺寸为 " + match[4] + "×" + match[5] + " 像素，" + dimension + relation + " " + match[3] + " 像素。"
		notice.Suggestion = "请" + action + material + "，使" + dimension + "满足 " + match[3] + " 像素的限制后再提交。"
	case image && (strings.Contains(lower, "download") || strings.Contains(lower, "fetch") || strings.Contains(lower, "accessible")):
		notice.Summary = "上游无法获取输入图片，视频生成失败。"
		notice.Suggestion = "请检查图片链接能否访问、是否过期或需要登录，必要时重新上传图片。"
	case image && (strings.Contains(lower, "format") || strings.Contains(lower, "decode") || strings.Contains(lower, "corrupt")):
		notice.Summary = "输入图片格式或文件内容不符合上游要求。"
		notice.Suggestion = "请检查图片是否能正常打开，并按上游支持的格式重新导出、上传图片。"
	case strings.Contains(lower, "aspect ratio") || strings.Contains(lower, "invalid parameter") || strings.Contains(lower, "unsupported parameter") || strings.Contains(lower, "invalid_argument"):
		notice.Summary = "视频生成参数不符合上游要求。"
		notice.Suggestion = "请根据原始错误检查图片比例、时长、分辨率等参数，按上游支持的取值调整。"
	case strings.Contains(lower, "rate limit") || strings.Contains(lower, "too many requests") || strings.Contains(lower, "concurrency") || strings.Contains(raw, "限流"):
		notice.Summary = "请求频率或并发数超过上游限制，本次视频任务未能完成。"
		notice.Suggestion = "请降低请求频率或并发，稍后再手动提交。"
	case strings.Contains(lower, "busy") || strings.Contains(lower, "overloaded") || strings.Contains(lower, "service unavailable") || strings.Contains(raw, "系统繁忙"):
		notice.Summary = "上游服务当前繁忙，未能完成视频任务。"
		notice.Suggestion = "请稍后手动重试；若持续出现，请联系管理员检查上游服务。"
	case strings.Contains(lower, "timeout") || strings.Contains(lower, "timed out") || strings.Contains(lower, "deadline exceeded") || strings.Contains(raw, "超时"):
		notice.Summary = "视频任务处理或等待响应超时，未能完成。"
		notice.Suggestion = "请保留任务号，联系管理员确认任务状态和上游处理结果后再决定是否重新提交。"
	}
	return sanitizeVideoFailureNotice(notice)
}

func isMissingVideoFailureReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "", "video generation failed", "task failed", "video generation failed; upstream did not provide an error reason":
		return true
	default:
		return false
	}
}

// sanitizeVideoFailureNotice protects both fresh classifications and restored
// notices. Every field is sanitized, and the complete message stays bounded.
func sanitizeVideoFailureNotice(notice VideoFailureNotice) VideoFailureNotice {
	notice.Summary = sanitizeVideoFailureDiagnostic(notice.Summary, 256)
	notice.Suggestion = sanitizeVideoFailureDiagnostic(notice.Suggestion, 256)
	prefix := notice.Summary + "\n处理建议：" + notice.Suggestion + "\n原始错误："
	remaining := max(0, common.LocalLogContentLimit-len([]rune(prefix)))
	notice.Detail = sanitizeVideoFailureDiagnostic(notice.Detail, remaining)
	return notice
}

func sanitizeVideoFailureDiagnostic(raw string, limit int) string {
	raw = strings.TrimSpace(raw)
	// Structured provider envelopes may follow a text prefix. Keep diagnostic
	// fields only; never return an echoed request body from those envelopes.
	for depth := range 6 {
		previous := raw
		for index, character := range raw {
			if character != '{' && (character != '[' || index > 0 && !strings.ContainsAny(raw[index-1:index], " \t\r\n:=")) {
				continue
			}
			if character == '[' && index > 0 {
				following := strings.TrimSpace(raw[index+1:])
				if following == "" || !strings.ContainsAny(following[:1], `{"[]`) {
					continue
				}
			}
			var envelope any
			diagnostic := ""
			if depth < 5 && common.DecodeJson(strings.NewReader(raw[index:]), &envelope) == nil {
				diagnostic = asyncImageErrorMessage(envelope, 0)
			}
			raw = strings.TrimSpace(raw[:index] + " " + diagnostic)
			break
		}
		if raw == previous {
			break
		}
	}
	if request := videoFailureRequestPattern.FindStringIndex(raw); request != nil {
		raw = strings.TrimSpace(raw[:request[0]])
	}
	if strings.HasPrefix(raw, "/") && !strings.ContainsAny(raw, " \t\r\n") {
		return ""
	}
	if parsed, err := url.Parse(raw); err == nil && (parsed.Host != "" || parsed.Scheme != "") {
		if parsed.Host != "" {
			return ""
		}
		switch strings.ToLower(parsed.Scheme) {
		case "http", "https", "data", "blob", "file", "ftp", "s3", "gs":
			return ""
		}
	}
	raw = videoFailureURLPattern.ReplaceAllString(raw, "[链接已隐藏]")
	raw = videoFailureAuthHeaderPattern.ReplaceAllString(raw, "${1}***")
	raw = videoFailureCredentialPattern.ReplaceAllString(raw, "${1}***")
	if limit < 0 {
		limit = len([]rune(raw))
	}
	return SanitizeAsyncImageText(raw, limit)
}

// VideoTaskFailureReason provides the same explanation to API error.message,
// plugin fail_reason, and task-history callers.
func VideoTaskFailureReason(raw string) string {
	notice := VideoTaskFailureNotice(raw)
	reason := notice.Summary + "\n处理建议：" + notice.Suggestion
	if notice.Detail != "" {
		reason += "\n原始错误：" + notice.Detail
	}
	return reason
}
