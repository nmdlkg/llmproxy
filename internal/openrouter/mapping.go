package openrouter

import (
	"regexp"
	"sort"
	"strings"
)

// MappingMethod describes how a local model name was associated with an
// OpenRouter model ID.
type MappingMethod string

const (
	MappingExplicit   MappingMethod = "explicit"
	MappingHeuristic  MappingMethod = "heuristic"
	MappingUnresolved MappingMethod = "unresolved"
)

// ModelMapping exposes the result of resolving one local model name. LocalModel
// is always returned exactly as supplied.
type ModelMapping struct {
	LocalModel      string        `json:"local_model"`
	OpenRouterModel string        `json:"openrouter_model,omitempty"`
	Method          MappingMethod `json:"method"`
	Resolved        bool          `json:"resolved"`
}

var (
	compactDateSuffix = regexp.MustCompile(`-\d{8}$`)
	dashedDateSuffix  = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)
)

// ResolveModels maps local model names against the currently known OpenRouter
// IDs. An explicit local-name entry is authoritative even when its target is
// absent, in which case the result remains unresolved and no heuristic runs.
//
// The heuristic only accepts a unique case-insensitive match after removing one
// vendor prefix and, if needed, one trailing YYYYMMDD or YYYY-MM-DD suffix.
// Ambiguous and fuzzy matches intentionally remain unresolved.
func ResolveModels(localModels, openRouterModelIDs []string, explicit map[string]string) []ModelMapping {
	knownIDs := uniqueSortedIDs(openRouterModelIDs)
	results := make([]ModelMapping, 0, len(localModels))
	for _, localModel := range localModels {
		results = append(results, resolveModel(localModel, knownIDs, explicit))
	}
	return results
}

func resolveModel(localModel string, knownIDs []string, explicit map[string]string) ModelMapping {
	if configured, ok := explicit[localModel]; ok {
		return resolveExplicitModel(localModel, configured, knownIDs)
	}
	if configured, ok := explicit[strings.ToLower(strings.TrimSpace(localModel))]; ok {
		return resolveExplicitModel(localModel, configured, knownIDs)
	}

	if match, ok := uniqueHeuristicMatch(localModel, knownIDs, false); ok {
		return ModelMapping{
			LocalModel:      localModel,
			OpenRouterModel: match,
			Method:          MappingHeuristic,
			Resolved:        true,
		}
	}
	if match, ok := uniqueHeuristicMatch(localModel, knownIDs, true); ok {
		return ModelMapping{
			LocalModel:      localModel,
			OpenRouterModel: match,
			Method:          MappingHeuristic,
			Resolved:        true,
		}
	}

	return ModelMapping{LocalModel: localModel, Method: MappingUnresolved}
}

func resolveExplicitModel(localModel, configured string, knownIDs []string) ModelMapping {
	canonical, resolved := uniqueCaseInsensitiveID(configured, knownIDs)
	if resolved {
		return ModelMapping{
			LocalModel:      localModel,
			OpenRouterModel: canonical,
			Method:          MappingExplicit,
			Resolved:        true,
		}
	}
	return ModelMapping{
		LocalModel:      localModel,
		OpenRouterModel: configured,
		Method:          MappingExplicit,
		Resolved:        false,
	}
}

func uniqueCaseInsensitiveID(target string, knownIDs []string) (string, bool) {
	var match string
	for _, id := range knownIDs {
		if !strings.EqualFold(strings.TrimSpace(target), id) {
			continue
		}
		if match != "" {
			return "", false
		}
		match = id
	}
	return match, match != ""
}

func uniqueHeuristicMatch(localModel string, knownIDs []string, stripDate bool) (string, bool) {
	localKey := heuristicKey(localModel, stripDate)
	if localKey == "" {
		return "", false
	}

	var match string
	for _, id := range knownIDs {
		if heuristicKey(id, stripDate) != localKey {
			continue
		}
		if match != "" {
			return "", false
		}
		match = id
	}
	return match, match != ""
}

func heuristicKey(model string, stripDate bool) string {
	model = strings.ToLower(strings.TrimSpace(model))
	if slash := strings.IndexByte(model, '/'); slash >= 0 {
		if slash == len(model)-1 || strings.Contains(model[slash+1:], "/") {
			return ""
		}
		model = model[slash+1:]
	}
	if stripDate {
		model = compactDateSuffix.ReplaceAllString(model, "")
		model = dashedDateSuffix.ReplaceAllString(model, "")
	}
	return model
}

func uniqueSortedIDs(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		key := strings.ToLower(id)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
