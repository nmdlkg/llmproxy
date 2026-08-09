package autoroute

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/constant"
	"github.com/tidwall/gjson"
)

// ExtractPrompt returns the last user-authored text suitable for classification.
func ExtractPrompt(entryProtocol string, rawJSON []byte, maxChars int) string {
	if len(rawJSON) == 0 || !gjson.ValidBytes(rawJSON) {
		return ""
	}

	var text string
	switch strings.ToLower(strings.TrimSpace(entryProtocol)) {
	case constant.Gemini, constant.GeminiInteractions:
		text = extractGeminiPrompt(rawJSON)
	default:
		text = extractMessagesPrompt(rawJSON)
	}
	return truncatePrompt(text, maxChars)
}

func extractMessagesPrompt(rawJSON []byte) string {
	messages := gjson.GetBytes(rawJSON, "messages")
	if !messages.IsArray() {
		return ""
	}
	items := messages.Array()
	for i := len(items) - 1; i >= 0; i-- {
		if !strings.EqualFold(strings.TrimSpace(items[i].Get("role").String()), "user") {
			continue
		}
		return joinTextContent(items[i].Get("content"))
	}
	return ""
}

func extractGeminiPrompt(rawJSON []byte) string {
	contents := gjson.GetBytes(rawJSON, "contents")
	if !contents.IsArray() {
		return ""
	}
	items := contents.Array()
	if len(items) == 0 {
		return ""
	}
	parts := items[len(items)-1].Get("parts")
	if !parts.IsArray() {
		return ""
	}
	texts := make([]string, 0, len(parts.Array()))
	for _, part := range parts.Array() {
		if text := strings.TrimSpace(part.Get("text").String()); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func joinTextContent(content gjson.Result) string {
	if content.Type == gjson.String {
		return strings.TrimSpace(content.String())
	}
	if !content.IsArray() {
		return ""
	}
	parts := content.Array()
	texts := make([]string, 0, len(parts))
	for _, part := range parts {
		if part.Type == gjson.String {
			if text := strings.TrimSpace(part.String()); text != "" {
				texts = append(texts, text)
			}
			continue
		}
		if text := strings.TrimSpace(part.Get("text").String()); text != "" {
			texts = append(texts, text)
		}
	}
	return strings.Join(texts, "\n")
}

func truncatePrompt(text string, maxChars int) string {
	if maxChars <= 0 {
		return text
	}
	runes := []rune(text)
	if len(runes) <= maxChars {
		return text
	}
	return string(runes[:maxChars])
}
