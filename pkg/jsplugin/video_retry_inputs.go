package jsplugin

import (
	"fmt"
	"slices"
	"strings"
)

// VideoRetryInput declares URL slots in the incoming openai_video JSON body.
// A wildcard traverses arrays only. The host never searches arbitrary strings.
type VideoRetryInput struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

func ValidateVideoRetryInputs(inputs []VideoRetryInput) error {
	if len(inputs) > 64 {
		return fmt.Errorf("videoRetryInputs exceeds 64 selectors")
	}
	seen := map[string]bool{}
	for _, input := range inputs {
		if !strings.HasPrefix(input.Path, "/") || len(input.Path) > 256 || strings.Count(input.Path, "/") > 12 || seen[input.Path] || !slices.Contains([]string{"image", "video", "audio", "media"}, input.Kind) {
			return fmt.Errorf("invalid videoRetryInputs selector %q", input.Path)
		}
		for part := range strings.SplitSeq(input.Path[1:], "/") {
			if part == "" || strings.ContainsAny(part, "~\\") || strings.Contains(part, "*") && part != "*" {
				return fmt.Errorf("invalid videoRetryInputs path %q", input.Path)
			}
		}
		seen[input.Path] = true
	}
	return nil
}
