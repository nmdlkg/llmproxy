package user

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

// GetCapacityPlan exposes the AX-75 advisory capacity snapshot consumed by a
// planning UI. It is deliberately read-only and never mutates selector state.
func (h *Handler) GetCapacityPlan(c *gin.Context) {
	user, _ := currentUser(c)
	accounts, truncated := h.providerQuotaAccounts(c.Request.Context(), user.ID)
	type planRow struct {
		Provider               string     `json:"provider"`
		Credential             string     `json:"credential"`
		Window                 string     `json:"window"`
		RemainingPercent       *float64   `json:"remaining_percent,omitempty"`
		ResetAt                *time.Time `json:"reset_at,omitempty"`
		UsePriority            int        `json:"use_priority"`
		ExhaustionRisk         string     `json:"exhaustion_risk"`
		ModelExclusiveCapacity any        `json:"model_exclusive_capacity"`
	}
	rows := make([]planRow, 0)
	for _, account := range accounts {
		for _, window := range account.Windows {
			risk, priority := capacityRisk(window.RemainingPercent)
			rows = append(rows, planRow{Provider: account.Provider, Credential: redactedCredentialHandle(user.ID, account.Label), Window: window.Name, RemainingPercent: window.RemainingPercent, ResetAt: window.ResetAt, UsePriority: priority, ExhaustionRisk: risk, ModelExclusiveCapacity: nil})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Provider != rows[j].Provider {
			return rows[i].Provider < rows[j].Provider
		}
		if rows[i].Credential != rows[j].Credential {
			return rows[i].Credential < rows[j].Credential
		}
		return rows[i].Window < rows[j].Window
	})
	hashInput := strings.Builder{}
	for _, row := range rows {
		hashInput.WriteString(row.Provider)
		hashInput.WriteByte(0)
		hashInput.WriteString(row.Credential)
		hashInput.WriteByte(0)
		hashInput.WriteString(row.Window)
		hashInput.WriteByte(0)
		hashInput.WriteString(row.ExhaustionRisk)
	}
	sum := sha256.Sum256([]byte(hashInput.String()))
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"schema_version": 1, "advisory": true, "snapshot_id": "capacity-" + hex.EncodeToString(sum[:8]), "algorithm_version": "capacity-shadow-v1", "generated_at": time.Now().UTC(), "stale_threshold_seconds": int64(providerQuotaCacheTTL / time.Second), "rows": rows, "truncated": truncated, "limitations": []string{"provider quota may represent a shared account", "model-exclusive capacity is unknown unless the provider reports it"}})
}

func capacityRisk(remaining *float64) (string, int) {
	if remaining == nil {
		return "unknown", 0
	}
	if *remaining <= 5 {
		return "critical", 3
	}
	if *remaining <= 20 {
		return "high", 2
	}
	if *remaining <= 50 {
		return "medium", 1
	}
	return "low", 0
}
