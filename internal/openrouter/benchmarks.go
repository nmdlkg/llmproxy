package openrouter

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// BenchmarkSource identifies an OpenRouter benchmark feed.
type BenchmarkSource string

const (
	BenchmarkArtificialAnalysis BenchmarkSource = "artificial-analysis"
	BenchmarkDesignArena        BenchmarkSource = "design-arena"
)

// TaskType selects an artificial-analysis capability index.
type TaskType string

const (
	TaskIntelligence TaskType = "intelligence"
	TaskCoding       TaskType = "coding"
	TaskAgentic      TaskType = "agentic"
)

// ModelQuality is the normalized benchmark record for one OpenRouter model.
// Every numeric field explicitly preserves unknown versus a measured zero.
type ModelQuality struct {
	OpenRouterID        string     `json:"openrouter_id"`
	DisplayName         string     `json:"display_name,omitempty"`
	IntelligenceIndex   KnownValue `json:"intelligence_index"`
	CodingIndex         KnownValue `json:"coding_index"`
	AgenticIndex        KnownValue `json:"agentic_index"`
	Elo                 KnownValue `json:"elo"`
	WinRate             KnownValue `json:"win_rate"`
	AvgGenerationTimeMS KnownValue `json:"avg_generation_time_ms"`
	Arena               string     `json:"arena,omitempty"`
	Category            string     `json:"category,omitempty"`
}

// RankingMode controls whether RankModels compares the raw benchmark score or
// score divided by the model's known prompt-plus-completion token cost.
type RankingMode string

const (
	RankByRawScore       RankingMode = "raw"
	RankByScorePerDollar RankingMode = "score-per-dollar"
)

// RankOptions configures local-model ranking.
type RankOptions struct {
	Mode     RankingMode
	Source   BenchmarkSource
	ModelMap map[string]string
}

// RankedModel reports the score and mapping used for one ranked candidate.
type RankedModel struct {
	LocalModel   string       `json:"local_model"`
	Mapping      ModelMapping `json:"mapping"`
	RawScore     KnownValue   `json:"raw_score"`
	RankingScore KnownValue   `json:"ranking_score"`
}

// RankModels returns local candidate names ordered best-first. For
// artificial-analysis, task selects the corresponding index. For design-arena,
// Elo is the ranking score and task is ignored. Score-per-dollar divides the raw
// score by prompt plus completion cost (one input plus one output token);
// pricing must be known for both fields. Unknown mappings, benchmark
// scores, or required prices sort last deterministically.
func RankModels(task TaskType, candidates []string, snapshot Snapshot, options RankOptions) []RankedModel {
	mappings := ResolveModels(candidates, snapshot.ModelIDs(), options.ModelMap)
	ranked := make([]RankedModel, 0, len(mappings))
	for _, mapping := range mappings {
		entry := RankedModel{LocalModel: mapping.LocalModel, Mapping: mapping}
		if mapping.Resolved {
			if quality, ok := snapshot.Quality[mapping.OpenRouterModel]; ok {
				entry.RawScore = benchmarkScore(quality, task, options.Source)
				entry.RankingScore = entry.RawScore
				if options.Mode == RankByScorePerDollar {
					entry.RankingScore = scorePerDollar(entry.RawScore, snapshot.Pricing[mapping.OpenRouterModel])
				}
			}
		}
		ranked = append(ranked, entry)
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		left := ranked[i].RankingScore
		right := ranked[j].RankingScore
		if left.Known != right.Known {
			return left.Known
		}
		if left.Known && left.Value != right.Value {
			return left.Value > right.Value
		}
		leftName := strings.ToLower(ranked[i].LocalModel)
		rightName := strings.ToLower(ranked[j].LocalModel)
		if leftName != rightName {
			return leftName < rightName
		}
		return ranked[i].LocalModel < ranked[j].LocalModel
	})
	return ranked
}

func benchmarkScore(quality ModelQuality, task TaskType, source BenchmarkSource) KnownValue {
	if source == BenchmarkDesignArena {
		return quality.Elo
	}
	switch task {
	case TaskCoding:
		return quality.CodingIndex
	case TaskAgentic:
		return quality.AgenticIndex
	case TaskIntelligence:
		return quality.IntelligenceIndex
	default:
		return KnownValue{}
	}
}

func scorePerDollar(score KnownValue, pricing ModelPricing) KnownValue {
	if !score.Known ||
		!pricing.PromptNanoUSDPerToken.Known ||
		!pricing.CompletionNanoUSDPerToken.Known {
		return KnownValue{}
	}
	prompt := pricing.PromptNanoUSDPerToken.NanoUSD
	completion := pricing.CompletionNanoUSDPerToken.NanoUSD
	if prompt < 0 || completion < 0 || prompt > math.MaxInt64-completion {
		return KnownValue{}
	}
	cost := prompt + completion
	if cost == 0 {
		switch {
		case score.Value > 0:
			return KnownValue{Value: math.MaxFloat64, Known: true}
		case score.Value < 0:
			return KnownValue{Value: -math.MaxFloat64, Known: true}
		default:
			return KnownValue{Value: 0, Known: true}
		}
	}
	// Pricing remains exact int64 nano-USD. Only the dimensionless ranking score
	// is floating-point.
	return KnownValue{Value: score.Value * 1_000_000_000 / float64(cost), Known: true}
}

func parseBenchmarkResponse(data []byte) (map[string]ModelQuality, error) {
	rows, errRows := decodeDataRows(data)
	if errRows != nil {
		return nil, fmt.Errorf("decode benchmark response: %w", errRows)
	}

	quality := make(map[string]ModelQuality, len(rows))
	for _, row := range rows {
		var raw map[string]json.RawMessage
		if errRow := json.Unmarshal(row, &raw); errRow != nil {
			continue
		}
		id, ok := parseJSONString(raw["model_permaslug"])
		if !ok || strings.TrimSpace(id) == "" {
			continue
		}
		displayName, _ := parseJSONString(raw["display_name"])
		arena, _ := parseJSONString(raw["arena"])
		category, _ := parseJSONString(raw["category"])
		incoming := ModelQuality{
			OpenRouterID:        id,
			DisplayName:         displayName,
			IntelligenceIndex:   parseOptionalNumber(raw["intelligence_index"]),
			CodingIndex:         parseOptionalNumber(raw["coding_index"]),
			AgenticIndex:        parseOptionalNumber(raw["agentic_index"]),
			Elo:                 parseOptionalNumber(raw["elo"]),
			WinRate:             parseOptionalNumber(raw["win_rate"]),
			AvgGenerationTimeMS: parseOptionalNumber(raw["avg_generation_time_ms"]),
			Arena:               arena,
			Category:            category,
		}
		quality[id] = mergeQuality(quality[id], incoming)
	}

	if len(quality) == 0 {
		return nil, fmt.Errorf("benchmark response contains no identifiable models")
	}
	return quality, nil
}

func parseOptionalNumber(raw json.RawMessage) KnownValue {
	if len(raw) == 0 || string(raw) == "null" {
		return KnownValue{}
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		var encoded string
		if errString := json.Unmarshal(raw, &encoded); errString != nil {
			return KnownValue{}
		}
		parsed, errParse := strconv.ParseFloat(strings.TrimSpace(encoded), 64)
		if errParse != nil {
			return KnownValue{}
		}
		value = parsed
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return KnownValue{}
	}
	return KnownValue{Value: value, Known: true}
}

func mergeQuality(current, incoming ModelQuality) ModelQuality {
	if current.OpenRouterID == "" {
		return incoming
	}
	if current.DisplayName == "" {
		current.DisplayName = incoming.DisplayName
	}
	current.IntelligenceIndex = preferKnown(current.IntelligenceIndex, incoming.IntelligenceIndex)
	current.CodingIndex = preferKnown(current.CodingIndex, incoming.CodingIndex)
	current.AgenticIndex = preferKnown(current.AgenticIndex, incoming.AgenticIndex)
	current.Elo = preferKnown(current.Elo, incoming.Elo)
	current.WinRate = preferKnown(current.WinRate, incoming.WinRate)
	current.AvgGenerationTimeMS = preferKnown(current.AvgGenerationTimeMS, incoming.AvgGenerationTimeMS)
	if current.Arena == "" {
		current.Arena = incoming.Arena
	}
	if current.Category == "" {
		current.Category = incoming.Category
	}
	return current
}

func preferKnown(current, incoming KnownValue) KnownValue {
	if current.Known {
		return current
	}
	return incoming
}
