package api

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/otelusage"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const otelUsagePluginName = "otelusage"

var (
	otelEnvironmentFallbackWarning sync.Once
	otelReloadNotice               sync.Once
	otelReloadNoticeArmed          atomic.Bool
)

type otelUsageSink struct {
	mu     sync.RWMutex
	plugin *otelusage.Plugin
}

func (s *otelUsageSink) HandleUsage(ctx context.Context, record coreusage.Record) {
	if s == nil {
		return
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.plugin != nil {
		s.plugin.HandleUsage(ctx, record)
	}
}

func (s *otelUsageSink) detach() *otelusage.Plugin {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	plugin := s.plugin
	s.plugin = nil
	s.mu.Unlock()
	return plugin
}

func otelUsageOptions(cfg *config.Config) (options otelusage.Options, usedEnvironmentFallback bool, err error) {
	if cfg != nil && cfg.OTel != nil {
		options, err = otelUsageOptionsFromConfig(cfg.OTel)
		return options, false, err
	}

	enabledValue, enabledSet := os.LookupEnv("LLMPROXY_OTEL_ENABLED")
	options, err = otelUsageOptionsFromEnvironment()
	return options, enabledSet && strings.TrimSpace(enabledValue) != "", err
}

func otelUsageOptionsFromConfig(otelConfig *config.OTelConfig) (otelusage.Options, error) {
	if otelConfig == nil || !otelConfig.Enabled {
		return otelusage.Options{}, nil
	}

	exportInterval := otelusage.DefaultExportInterval
	intervalValue := strings.TrimSpace(otelConfig.ExportInterval)
	if intervalValue != "" {
		interval, errInterval := time.ParseDuration(intervalValue)
		if errInterval != nil {
			return otelusage.Options{}, fmt.Errorf("parse otel.export-interval: %w", errInterval)
		}
		if interval <= 0 {
			return otelusage.Options{}, fmt.Errorf("otel.export-interval must be positive")
		}
		exportInterval = interval
	}

	hostName, errHostName := os.Hostname()
	if errHostName != nil {
		return otelusage.Options{}, fmt.Errorf("resolve host name for OpenTelemetry resource: %w", errHostName)
	}

	serviceVersion := strings.TrimSpace(otelConfig.ServiceVersion)
	if serviceVersion == "" {
		serviceVersion = strings.TrimSpace(buildinfo.Version)
	}
	var resourceAttributes map[string]string
	if len(otelConfig.ResourceAttributes) > 0 {
		resourceAttributes = make(map[string]string, len(otelConfig.ResourceAttributes))
		for key, value := range otelConfig.ResourceAttributes {
			resourceAttributes[key] = value
		}
	}

	return otelusage.Options{
		Enabled:            true,
		Endpoint:           strings.TrimSpace(otelConfig.Endpoint),
		ExportInterval:     exportInterval,
		ServiceName:        strings.TrimSpace(otelConfig.ServiceName),
		ServiceVersion:     serviceVersion,
		Environment:        strings.TrimSpace(otelConfig.Environment),
		HostName:           hostName,
		ResourceAttributes: resourceAttributes,
	}, nil
}

func otelUsageOptionsFromEnvironment() (otelusage.Options, error) {
	enabledValue := strings.TrimSpace(os.Getenv("LLMPROXY_OTEL_ENABLED"))
	if enabledValue == "" {
		return otelusage.Options{}, nil
	}
	enabled, errEnabled := strconv.ParseBool(enabledValue)
	if errEnabled != nil {
		return otelusage.Options{}, fmt.Errorf("parse LLMPROXY_OTEL_ENABLED: %w", errEnabled)
	}
	if !enabled {
		return otelusage.Options{}, nil
	}

	endpoint := strings.TrimSpace(os.Getenv("LLMPROXY_OTEL_ENDPOINT"))
	if endpoint == "" {
		return otelusage.Options{}, fmt.Errorf("LLMPROXY_OTEL_ENDPOINT is required when OpenTelemetry usage export is enabled")
	}

	exportInterval := otelusage.DefaultExportInterval
	if intervalValue := strings.TrimSpace(os.Getenv("LLMPROXY_OTEL_INTERVAL_SECONDS")); intervalValue != "" {
		seconds, errSeconds := strconv.ParseInt(intervalValue, 10, 64)
		if errSeconds != nil {
			return otelusage.Options{}, fmt.Errorf("parse LLMPROXY_OTEL_INTERVAL_SECONDS: %w", errSeconds)
		}
		if seconds <= 0 || seconds > math.MaxInt64/int64(time.Second) {
			return otelusage.Options{}, fmt.Errorf("LLMPROXY_OTEL_INTERVAL_SECONDS must be a positive duration in whole seconds")
		}
		exportInterval = time.Duration(seconds) * time.Second
	}

	hostName, errHostName := os.Hostname()
	if errHostName != nil {
		return otelusage.Options{}, fmt.Errorf("resolve host name for OpenTelemetry resource: %w", errHostName)
	}

	return otelusage.Options{
		Enabled:        true,
		Endpoint:       endpoint,
		ExportInterval: exportInterval,
		ServiceName:    otelusage.DefaultServiceName,
		ServiceVersion: buildinfo.Version,
		Environment:    strings.TrimSpace(os.Getenv("LLMPROXY_OTEL_ENVIRONMENT")),
		HostName:       hostName,
	}, nil
}

func warnOTelEnvironmentFallback() {
	otelEnvironmentFallbackWarning.Do(func() {
		log.Warn("LLMPROXY_OTEL_* is deprecated; configure the otel section instead")
	})
}

func armOTelReloadNotice() {
	otelReloadNoticeArmed.Store(true)
}

func noteOTelConfigReload() {
	if !otelReloadNoticeArmed.Load() {
		return
	}
	otelReloadNotice.Do(func() {
		log.Info("OpenTelemetry usage export is startup-only; otel configuration changes require a restart")
	})
}

func tenancyUsageEmailResolver(service *tenancy.Service) otelusage.UserEmailResolver {
	if service == nil || service.Store() == nil {
		return nil
	}
	resolveUser := tenancy.ResolveUsageUser(service.Store())
	return func(ctx context.Context, record coreusage.Record) (string, bool) {
		user, errResolve := resolveUser(ctx, record)
		if errResolve != nil {
			if !errors.Is(errResolve, tenancy.ErrNotFound) {
				log.WithError(errResolve).Debug("otel usage: user attribution failed")
			}
			return "", false
		}
		if user == nil {
			return "", false
		}
		email := strings.TrimSpace(user.Email)
		return email, email != ""
	}
}

func registerOTelUsage(
	ctx context.Context,
	options otelusage.Options,
	resolver otelusage.UserEmailResolver,
	register func(string, coreusage.Plugin),
) (*otelUsageSink, error) {
	if !options.Enabled {
		return nil, nil
	}
	if register == nil {
		return nil, fmt.Errorf("otel usage: plugin registrar is required")
	}
	plugin, errPlugin := otelusage.New(ctx, options, resolver)
	if errPlugin != nil {
		return nil, errPlugin
	}
	if plugin == nil {
		return nil, nil
	}
	sink := &otelUsageSink{plugin: plugin}
	register(otelUsagePluginName, sink)
	return sink, nil
}
