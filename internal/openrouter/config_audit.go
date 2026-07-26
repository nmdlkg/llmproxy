package openrouter

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func init() {
	config.RegisterModelPricingAuditor(func(cfg *config.Config) {
		if cfg == nil || !cfg.Tenancy.Enabled {
			return
		}
		defaultCatalog.logUnpricedModels(builtinLocalModelNames(), cfg)
	})
}

func builtinLocalModelNames() []string {
	groups := [][]*registry.ModelInfo{
		registry.GetClaudeModels(),
		registry.GetGeminiModels(),
		registry.GetGeminiVertexModels(),
		registry.GetAIStudioModels(),
		registry.GetCodexFreeModels(),
		registry.GetCodexTeamModels(),
		registry.GetCodexPlusModels(),
		registry.GetCodexProModels(),
		registry.GetKimiModels(),
		registry.GetAntigravityModels(),
		registry.GetXAIModels(),
	}
	names := make([]string, 0)
	for _, models := range groups {
		for _, model := range models {
			if model != nil {
				names = append(names, model.ID)
			}
		}
	}
	return uniqueSortedIDs(names)
}
