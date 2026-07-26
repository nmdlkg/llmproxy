package openrouter

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"
)

//go:embed snapshot.json
var embeddedSnapshotJSON []byte

// Snapshot is an immutable-by-convention catalog value. Catalog.Snapshot
// returns deep copies so callers cannot mutate the atomically published state.
type Snapshot struct {
	Pricing   map[string]ModelPricing `json:"pricing"`
	Quality   map[string]ModelQuality `json:"quality"`
	UpdatedAt time.Time               `json:"updated_at"`
	Source    string                  `json:"source"`
}

// ModelIDs returns the sorted union of OpenRouter IDs known to pricing or
// benchmark data.
func (s Snapshot) ModelIDs() []string {
	ids := make([]string, 0, len(s.Pricing)+len(s.Quality))
	for id := range s.Pricing {
		ids = append(ids, id)
	}
	for id := range s.Quality {
		ids = append(ids, id)
	}
	return uniqueSortedIDs(ids)
}

// PricingForLocalModel resolves a local model and returns its exact nano-USD price.
func (s Snapshot) PricingForLocalModel(localModel string, explicit map[string]string) (ModelPricing, ModelMapping, bool) {
	mapping := ResolveModels([]string{localModel}, s.ModelIDs(), explicit)[0]
	if !mapping.Resolved {
		return ModelPricing{}, mapping, false
	}
	pricing, ok := s.Pricing[mapping.OpenRouterModel]
	return pricing, mapping, ok
}

// QualityForLocalModel resolves a local model and returns its benchmark record.
func (s Snapshot) QualityForLocalModel(localModel string, explicit map[string]string) (ModelQuality, ModelMapping, bool) {
	mapping := ResolveModels([]string{localModel}, s.ModelIDs(), explicit)[0]
	if !mapping.Resolved {
		return ModelQuality{}, mapping, false
	}
	quality, ok := s.Quality[mapping.OpenRouterModel]
	return quality, mapping, ok
}

// Catalog owns an atomically replaceable OpenRouter snapshot.
type Catalog struct {
	current        atomic.Pointer[Snapshot]
	startOnce      sync.Once
	mappingLogOnce sync.Once
	pricingLogOnce sync.Once
}

// NewCatalog creates a catalog initialized from the embedded offline snapshot.
func NewCatalog() (*Catalog, error) {
	snapshot, errSnapshot := loadEmbeddedSnapshot()
	if errSnapshot != nil {
		return nil, errSnapshot
	}
	catalog := &Catalog{}
	catalog.replace(snapshot)
	return catalog, nil
}

// Snapshot returns an independent copy of the currently published catalog.
func (c *Catalog) Snapshot() Snapshot {
	if c == nil {
		return Snapshot{Pricing: map[string]ModelPricing{}, Quality: map[string]ModelQuality{}}
	}
	current := c.current.Load()
	if current == nil {
		return Snapshot{Pricing: map[string]ModelPricing{}, Quality: map[string]ModelQuality{}}
	}
	return cloneSnapshot(*current)
}

func (c *Catalog) replace(snapshot Snapshot) {
	cloned := cloneSnapshot(snapshot)
	c.current.Store(&cloned)
}

func cloneSnapshot(snapshot Snapshot) Snapshot {
	out := Snapshot{
		Pricing:   make(map[string]ModelPricing, len(snapshot.Pricing)),
		Quality:   make(map[string]ModelQuality, len(snapshot.Quality)),
		UpdatedAt: snapshot.UpdatedAt,
		Source:    snapshot.Source,
	}
	for id, pricing := range snapshot.Pricing {
		pricing.SupportedParameters = append([]string(nil), pricing.SupportedParameters...)
		out.Pricing[id] = pricing
	}
	for id, quality := range snapshot.Quality {
		out.Quality[id] = quality
	}
	return out
}

type embeddedSnapshot struct {
	GeneratedAt string          `json:"generated_at"`
	Models      json.RawMessage `json:"models"`
	Benchmarks  struct {
		ArtificialAnalysis json.RawMessage `json:"artificial-analysis"`
		DesignArena        json.RawMessage `json:"design-arena"`
	} `json:"benchmarks"`
}

func loadEmbeddedSnapshot() (Snapshot, error) {
	var document embeddedSnapshot
	if err := json.Unmarshal(embeddedSnapshotJSON, &document); err != nil {
		return Snapshot{}, fmt.Errorf("decode embedded OpenRouter snapshot: %w", err)
	}
	pricing, errPricing := parsePricingResponse(document.Models)
	if errPricing != nil {
		return Snapshot{}, fmt.Errorf("parse embedded OpenRouter pricing: %w", errPricing)
	}
	artificial, errArtificial := parseBenchmarkResponse(document.Benchmarks.ArtificialAnalysis)
	if errArtificial != nil {
		return Snapshot{}, fmt.Errorf("parse embedded artificial-analysis benchmarks: %w", errArtificial)
	}
	design, errDesign := parseBenchmarkResponse(document.Benchmarks.DesignArena)
	if errDesign != nil {
		return Snapshot{}, fmt.Errorf("parse embedded design-arena benchmarks: %w", errDesign)
	}

	quality := make(map[string]ModelQuality, len(artificial)+len(design))
	for id, record := range artificial {
		quality[id] = record
	}
	for id, record := range design {
		quality[id] = mergeQuality(quality[id], record)
	}

	updatedAt, errTime := time.Parse(time.RFC3339, document.GeneratedAt)
	if errTime != nil {
		return Snapshot{}, fmt.Errorf("parse embedded OpenRouter generated_at: %w", errTime)
	}
	return Snapshot{
		Pricing:   pricing,
		Quality:   quality,
		UpdatedAt: updatedAt,
		Source:    "embedded",
	}, nil
}

func newDefaultCatalog() *Catalog {
	catalog, errCatalog := NewCatalog()
	if errCatalog == nil {
		return catalog
	}
	log.WithError(errCatalog).Warn("openrouter: embedded snapshot is unavailable")
	empty := &Catalog{}
	empty.replace(Snapshot{
		Pricing: map[string]ModelPricing{},
		Quality: map[string]ModelQuality{},
		Source:  "empty",
	})
	return empty
}

var defaultCatalog = newDefaultCatalog()

// CurrentSnapshot returns a copy of the process-wide OpenRouter catalog.
func CurrentSnapshot() Snapshot {
	return defaultCatalog.Snapshot()
}

func sortedUnresolvedMappings(mappings []ModelMapping) []string {
	seen := make(map[string]struct{}, len(mappings))
	unresolved := make([]string, 0)
	for _, mapping := range mappings {
		if mapping.Resolved {
			continue
		}
		if _, ok := seen[mapping.LocalModel]; ok {
			continue
		}
		seen[mapping.LocalModel] = struct{}{}
		unresolved = append(unresolved, mapping.LocalModel)
	}
	sort.Strings(unresolved)
	return unresolved
}
