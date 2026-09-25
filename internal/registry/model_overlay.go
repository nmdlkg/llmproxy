package registry

import (
	_ "embed"
	"encoding/json"
	"reflect"
	"strings"

	log "github.com/sirupsen/logrus"
)

// Fork seam: fork-only model catalog entries. models.json is owned by upstream
// and refreshed from router-for-me/models (in CI and by the remote updater), so
// fork entries live in a separate overlay that every catalog load path merges:
// the embedded catalog (including --local-model) and each remote refresh.
//
// Precedence: upstream entries are authoritative. An overlay entry whose ID
// matches an entry in the same section case-insensitively is skipped and
// logged at debug level. Remaining overlay entries are appended to the end of
// their section in overlay order.

//go:embed models/fork_models_overlay.json
var forkModelsOverlayJSON []byte

// applyForkModelOverlay merges the embedded fork overlay into a parsed catalog
// before validation. A malformed overlay is logged and ignored so the upstream
// catalog always loads.
func applyForkModelOverlay(data *staticModelsJSON, source string) {
	if data == nil {
		return
	}
	overlay, errOverlay := parseForkModelOverlay(forkModelsOverlayJSON)
	if errOverlay != nil {
		log.WithError(errOverlay).Error("registry: ignoring invalid fork model overlay")
		return
	}
	mergeModelOverlay(data, overlay, source)
}

// parseForkModelOverlay decodes the overlay into fresh ModelInfo values so no
// pointer is shared between catalog loads.
func parseForkModelOverlay(raw []byte) (map[string][]*ModelInfo, error) {
	overlay := make(map[string][]*ModelInfo)
	if len(raw) == 0 {
		return overlay, nil
	}
	if errDecode := json.Unmarshal(raw, &overlay); errDecode != nil {
		return nil, errDecode
	}
	return overlay, nil
}

func mergeModelOverlay(data *staticModelsJSON, overlay map[string][]*ModelInfo, source string) {
	sections := catalogSections(data)
	for sectionName, entries := range overlay {
		section, ok := sections[sectionName]
		if !ok {
			log.WithField("section", sectionName).Warn("registry: fork model overlay references an unknown catalog section")
			continue
		}
		existing := make(map[string]struct{}, len(*section)+len(entries))
		for _, model := range *section {
			if model != nil {
				existing[strings.ToLower(strings.TrimSpace(model.ID))] = struct{}{}
			}
		}
		for _, model := range entries {
			if model == nil {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(model.ID))
			if key == "" {
				continue
			}
			if _, shadowed := existing[key]; shadowed {
				log.WithFields(log.Fields{
					"source":  source,
					"section": sectionName,
					"model":   model.ID,
				}).Debug("registry: fork model overlay entry shadowed by upstream catalog")
				continue
			}
			existing[key] = struct{}{}
			*section = append(*section, model)
		}
	}
}

// catalogSections maps each JSON section name of staticModelsJSON to its slice,
// so sections upstream adds later are supported without fork edits.
func catalogSections(data *staticModelsJSON) map[string]*[]*ModelInfo {
	sections := make(map[string]*[]*ModelInfo)
	value := reflect.ValueOf(data).Elem()
	valueType := value.Type()
	for i := 0; i < valueType.NumField(); i++ {
		field := valueType.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "" || name == "-" {
			continue
		}
		if models, ok := value.Field(i).Addr().Interface().(*[]*ModelInfo); ok {
			sections[name] = models
		}
	}
	return sections
}
