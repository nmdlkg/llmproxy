package openrouter

import "testing"

func TestRankModelsUnknownScoresSortLastDeterministically(t *testing.T) {
	snapshot := Snapshot{
		Pricing: map[string]ModelPricing{
			"vendor/model-a": {
				OpenRouterID:              "vendor/model-a",
				PromptNanoUSDPerToken:     KnownNanoUSD{NanoUSD: 1, Known: true},
				CompletionNanoUSDPerToken: KnownNanoUSD{NanoUSD: 1, Known: true},
			},
			"vendor/model-b": {
				OpenRouterID:              "vendor/model-b",
				PromptNanoUSDPerToken:     KnownNanoUSD{NanoUSD: 10, Known: true},
				CompletionNanoUSDPerToken: KnownNanoUSD{NanoUSD: 10, Known: true},
			},
			"vendor/model-c": {OpenRouterID: "vendor/model-c"},
		},
		Quality: map[string]ModelQuality{
			"vendor/model-a": {
				OpenRouterID: "vendor/model-a",
				CodingIndex:  KnownValue{Value: 90, Known: true},
			},
			"vendor/model-b": {
				OpenRouterID: "vendor/model-b",
				CodingIndex:  KnownValue{Value: 100, Known: true},
			},
			"vendor/model-c": {OpenRouterID: "vendor/model-c"},
		},
	}
	candidates := []string{"model-c", "model-a", "model-b", "no-match"}

	raw := RankModels(TaskCoding, candidates, snapshot, RankOptions{
		Mode:   RankByRawScore,
		Source: BenchmarkArtificialAnalysis,
	})
	assertRankOrder(t, raw, []string{"model-b", "model-a", "model-c", "no-match"})

	perDollar := RankModels(TaskCoding, candidates, snapshot, RankOptions{
		Mode:   RankByScorePerDollar,
		Source: BenchmarkArtificialAnalysis,
	})
	assertRankOrder(t, perDollar, []string{"model-a", "model-b", "model-c", "no-match"})
	if perDollar[2].RankingScore.Known || perDollar[3].RankingScore.Known {
		t.Fatalf("unknown entries unexpectedly known: %+v %+v", perDollar[2], perDollar[3])
	}
}

func assertRankOrder(t *testing.T, got []RankedModel, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].LocalModel != want[i] {
			t.Fatalf("rank[%d] = %q, want %q; all=%v", i, got[i].LocalModel, want[i], rankedNames(got))
		}
	}
}

func rankedNames(ranked []RankedModel) []string {
	names := make([]string, 0, len(ranked))
	for _, entry := range ranked {
		names = append(names, entry.LocalModel)
	}
	return names
}
