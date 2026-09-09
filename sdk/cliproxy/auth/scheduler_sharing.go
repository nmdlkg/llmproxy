package auth

// schedulerSharingMetadata exposes only scalar sharing fields to scheduler
// plugins. Credential metadata may also contain tokens and must not be copied.
func schedulerSharingMetadata(src map[string]any) map[string]any {
	var out map[string]any
	for _, key := range []string{"owner_user_id", "shared"} {
		value, exists := src[key]
		if !exists {
			continue
		}
		switch value.(type) {
		case string, bool:
			if out == nil {
				out = make(map[string]any, 2)
			}
			out[key] = value
		}
	}
	return out
}
