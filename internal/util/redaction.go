package util

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const logRedactedValue = "[REDACTED]"

var (
	logCredentialPattern = regexp.MustCompile(`(?i)\bcp_u_[A-Za-z0-9_-]+`)
	logBearerPattern     = regexp.MustCompile(`(?i)\bBearer\s+[A-Za-z0-9._~+/=-]+`)
	logSensitiveField    = regexp.MustCompile(`(?i)(["']?(?:authorization|cookie|set-cookie|api[_-]?key|key[_-]?hash|access[_-]?token|refresh[_-]?token|token|secret|password)["']?\s*[:=]\s*)(["']?)([^"'\s,}\]]+)(["']?)`)
)

// RedactSensitiveLogText removes credential-shaped values and named secret
// fields before a payload is written to a request log.
func RedactSensitiveLogText(value string) string {
	value = logCredentialPattern.ReplaceAllString(value, logRedactedValue)
	value = logBearerPattern.ReplaceAllString(value, "Bearer "+logRedactedValue)
	return logSensitiveField.ReplaceAllString(value, "${1}${2}"+logRedactedValue+"${4}")
}

// RedactSensitiveLogBody preserves ordinary JSON fields while replacing values
// whose names identify credentials. Non-JSON bodies still have common token
// forms removed.
func RedactSensitiveLogBody(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if errDecode := decoder.Decode(&value); errDecode == nil {
		if !containsSensitiveLogField(value) && !logCredentialPattern.Match(body) && !logBearerPattern.Match(body) {
			return body
		}
		value = redactSensitiveLogJSONValue(value)
		if encoded, errMarshal := json.Marshal(value); errMarshal == nil {
			return []byte(RedactSensitiveLogText(string(encoded)))
		}
	}
	return []byte(RedactSensitiveLogText(string(body)))
}

func redactSensitiveLogJSONValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveLogField(key) {
				typed[key] = logRedactedValue
				continue
			}
			typed[key] = redactSensitiveLogJSONValue(child)
		}
	case []any:
		for index := range typed {
			typed[index] = redactSensitiveLogJSONValue(typed[index])
		}
	}
	return value
}

func isSensitiveLogField(key string) bool {
	normalized := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(key), "[]"))
	if normalized == "authorization" || normalized == "cookie" || normalized == "set-cookie" || normalized == "password" {
		return true
	}
	return strings.Contains(normalized, "api-key") ||
		strings.Contains(normalized, "api_key") ||
		strings.Contains(normalized, "apikey") ||
		strings.Contains(normalized, "key-hash") ||
		strings.Contains(normalized, "key_hash") ||
		strings.Contains(normalized, "token") ||
		strings.Contains(normalized, "secret")
}

func containsSensitiveLogField(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if isSensitiveLogField(key) || containsSensitiveLogField(child) {
				return true
			}
		}
	case []any:
		for _, child := range typed {
			if containsSensitiveLogField(child) {
				return true
			}
		}
	}
	return false
}
