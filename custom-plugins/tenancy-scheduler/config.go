package main

import (
	"fmt"
	"strings"
	"time"

	"cliproxy-tenancy-scheduler/optimizer"
	"gopkg.in/yaml.v3"
)

// Scheduling modes. legacy keeps sharing filtering plus rotation; shadow
// executes legacy decisions while recording optimizer decisions; optimizer
// executes optimizer decisions.
const (
	modeLegacy    = "legacy"
	modeShadow    = "shadow"
	modeOptimizer = "optimizer"
)

type pluginConfig struct {
	Mode             string
	StatePath        string
	AcrossPriorities bool
	RecomputeEvery   time.Duration
	CheckpointEvery  time.Duration
	Optimizer        optimizer.Config
}

type rawConfig struct {
	Mode             string `yaml:"mode"`
	StatePath        string `yaml:"state-path"`
	AcrossPriorities bool   `yaml:"across-priorities"`
	Optimizer        struct {
		RecomputeInterval      string   `yaml:"recompute-interval"`
		CheckpointInterval     string   `yaml:"checkpoint-interval"`
		Epsilon                *float64 `yaml:"epsilon"`
		Scenarios              *int     `yaml:"scenarios"`
		Step                   string   `yaml:"step"`
		MaxHorizon             string   `yaml:"max-horizon"`
		StaleAfter             string   `yaml:"stale-after"`
		ReservationTTL         string   `yaml:"reservation-ttl"`
		ActiveAccountWindow    string   `yaml:"active-account-window"`
		AccountTTL             string   `yaml:"account-ttl"`
		RequestValue           *float64 `yaml:"request-value"`
		LambdaLatency          *float64 `yaml:"lambda-latency"`
		LambdaFailure          *float64 `yaml:"lambda-failure"`
		PerturbRequests        *float64 `yaml:"perturb-requests"`
		PriorRequestsPerWindow *float64 `yaml:"prior-requests-per-window"`
		DemandCV               *float64 `yaml:"demand-cv"`
		ConsumptionCV          *float64 `yaml:"consumption-cv"`
		DriftPerHour           *float64 `yaml:"drift-per-hour"`
		TerminalValue          *float64 `yaml:"terminal-value"`
		MaxWork                *float64 `yaml:"max-work"`
		Seed                   *uint64  `yaml:"seed"`
	} `yaml:"optimizer"`
}

func defaultPluginConfig() pluginConfig {
	return pluginConfig{
		Mode:            modeLegacy,
		RecomputeEvery:  time.Minute,
		CheckpointEvery: 5 * time.Minute,
		Optimizer:       optimizer.Defaults(),
	}
}

// parsePluginConfig parses plugins.configs.tenancy-scheduler. Unknown keys
// such as enabled and priority belong to the host and are ignored.
func parsePluginConfig(raw []byte) (pluginConfig, error) {
	cfg := defaultPluginConfig()
	var in rawConfig
	if len(strings.TrimSpace(string(raw))) > 0 {
		if errUnmarshal := yaml.Unmarshal(raw, &in); errUnmarshal != nil {
			return cfg, fmt.Errorf("tenancy-scheduler: parse config: %w", errUnmarshal)
		}
	}
	switch mode := strings.ToLower(strings.TrimSpace(in.Mode)); mode {
	case "":
	case modeLegacy, modeShadow, modeOptimizer:
		cfg.Mode = mode
	default:
		return cfg, fmt.Errorf("tenancy-scheduler: unknown mode %q", in.Mode)
	}
	cfg.StatePath = strings.TrimSpace(in.StatePath)
	cfg.AcrossPriorities = in.AcrossPriorities

	o := &cfg.Optimizer
	durations := []struct {
		text   string
		target *time.Duration
		name   string
	}{
		{in.Optimizer.RecomputeInterval, &cfg.RecomputeEvery, "recompute-interval"},
		{in.Optimizer.CheckpointInterval, &cfg.CheckpointEvery, "checkpoint-interval"},
		{in.Optimizer.Step, &o.Step, "step"},
		{in.Optimizer.MaxHorizon, &o.MaxHorizon, "max-horizon"},
		{in.Optimizer.StaleAfter, &o.StaleAfter, "stale-after"},
		{in.Optimizer.ReservationTTL, &o.ReservationTTL, "reservation-ttl"},
		{in.Optimizer.ActiveAccountWindow, &o.ActiveAccountWindow, "active-account-window"},
		{in.Optimizer.AccountTTL, &o.AccountTTL, "account-ttl"},
	}
	for _, item := range durations {
		if strings.TrimSpace(item.text) == "" {
			continue
		}
		value, errParse := time.ParseDuration(strings.TrimSpace(item.text))
		if errParse != nil || value <= 0 {
			return cfg, fmt.Errorf("tenancy-scheduler: optimizer.%s must be a positive duration", item.name)
		}
		*item.target = value
	}
	floats := []struct {
		value  *float64
		target *float64
	}{
		{in.Optimizer.Epsilon, &o.Epsilon},
		{in.Optimizer.RequestValue, &o.RequestValue},
		{in.Optimizer.LambdaLatency, &o.LambdaLatency},
		{in.Optimizer.LambdaFailure, &o.LambdaFailure},
		{in.Optimizer.PerturbRequests, &o.PerturbRequests},
		{in.Optimizer.PriorRequestsPerWindow, &o.PriorRequestsPerWindow},
		{in.Optimizer.DemandCV, &o.DemandCV},
		{in.Optimizer.ConsumptionCV, &o.ConsumptionCV},
		{in.Optimizer.DriftPerHour, &o.DriftPerHour},
		{in.Optimizer.TerminalValue, &o.TerminalValue},
		{in.Optimizer.MaxWork, &o.MaxWork},
	}
	for _, item := range floats {
		if item.value != nil {
			*item.target = *item.value
		}
	}
	if in.Optimizer.Scenarios != nil {
		o.Scenarios = *in.Optimizer.Scenarios
	}
	if in.Optimizer.Seed != nil {
		o.Seed = *in.Optimizer.Seed
	}
	normalized, errNormalize := o.Normalize()
	if errNormalize != nil {
		return cfg, fmt.Errorf("tenancy-scheduler: optimizer: %w", errNormalize)
	}
	cfg.Optimizer = normalized
	return cfg, nil
}
