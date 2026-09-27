package optimizer

import (
	"math"
	"testing"
)

func TestNormalizeRejectsNonFiniteFloatFields(t *testing.T) {
	fields := []struct {
		name string
		set  func(*Config, float64)
	}{
		{"epsilon", func(c *Config, v float64) { c.Epsilon = v }},
		{"request-value", func(c *Config, v float64) { c.RequestValue = v }},
		{"lambda-latency", func(c *Config, v float64) { c.LambdaLatency = v }},
		{"lambda-failure", func(c *Config, v float64) { c.LambdaFailure = v }},
		{"perturb-requests", func(c *Config, v float64) { c.PerturbRequests = v }},
		{"prior-requests-per-window", func(c *Config, v float64) { c.PriorRequestsPerWindow = v }},
		{"demand-cv", func(c *Config, v float64) { c.DemandCV = v }},
		{"consumption-cv", func(c *Config, v float64) { c.ConsumptionCV = v }},
		{"drift-per-hour", func(c *Config, v float64) { c.DriftPerHour = v }},
		{"terminal-value", func(c *Config, v float64) { c.TerminalValue = v }},
		{"max-work", func(c *Config, v float64) { c.MaxWork = v }},
	}
	for _, field := range fields {
		for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			t.Run(field.name, func(t *testing.T) {
				cfg := Defaults()
				field.set(&cfg, value)
				if _, errNormalize := cfg.Normalize(); errNormalize == nil {
					t.Fatalf("Normalize accepted %v", value)
				}
			})
		}
	}
}
