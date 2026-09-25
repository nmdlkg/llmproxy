package api

import (
	"context"
	"errors"
	"fmt"

	managementHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	userHandlers "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/user"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/tenancy"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/userpanelasset"
	sdkAuth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

// forkRuntime owns every fork-specific server dependency: the tenancy service,
// OTel usage export, the tenant user API handler, and the user-panel asset and
// its update supervisor. Server keeps a single field and a few call sites so
// upstream lifecycle edits in server.go and server_reload.go do not conflict
// with fork code.
//
// All methods are nil-safe so tests that build a bare Server keep working.
type forkRuntime struct {
	tenancyService *tenancy.Service
	tenancyInitErr error

	otelUsageSink    *otelUsageSink
	otelUsageInitErr error

	user *userHandlers.Handler

	// userPanelAsset and userPanelSupervisor are instance-owned. The supervisor
	// performs background update work; request handlers only read the configured
	// local development file or verified release cache.
	userPanelAsset      *userpanelasset.Manager
	userPanelSupervisor *userpanelasset.Supervisor
}

// newForkRuntime initializes the tenancy service and OTel usage export. It must
// run before the access providers are applied because tenant authentication
// reads the tenancy store. Initialization errors are reported by startErr.
func newForkRuntime(cfg *config.Config, configFilePath string, authManager *auth.Manager, options forkOptionConfig) *forkRuntime {
	f := &forkRuntime{}

	// The panel is maintained in the separate CLIProxyAPI-User-Panel repository.
	// The backend serves its configured local build or verified release cache;
	// it no longer embeds a frontend copy that can drift from that repository.
	f.userPanelAsset = userpanelasset.NewManager(configFilePath)
	f.userPanelSupervisor = userpanelasset.NewSupervisor(f.userPanelAsset)
	f.userPanelSupervisor.SetConfig(cfg)

	if options.tenancyEnabled {
		tenancyService, errTenancy := tenancy.NewService(cfg.Tenancy, cfg.AuthDir, authManager)
		if errTenancy != nil {
			f.tenancyInitErr = errTenancy
			log.WithError(errTenancy).Error("failed to initialize tenancy service")
		} else {
			f.tenancyService = tenancyService
		}
	}
	if options.otelUsageEnabled {
		f.initOTelUsage(cfg)
		// Server construction has already performed its startup SDK config
		// conversion, so later conversions are reloads.
		armOTelReloadNotice()
	}
	return f
}

func (f *forkRuntime) initOTelUsage(cfg *config.Config) {
	otelOptions, usedEnvironmentFallback, errOptions := otelUsageOptions(cfg)
	if usedEnvironmentFallback {
		warnOTelEnvironmentFallback()
	}
	if errOptions != nil {
		f.otelUsageInitErr = errOptions
		log.WithError(errOptions).Error("failed to configure OpenTelemetry usage export")
		return
	}
	otelSink, errOTelUsage := registerOTelUsage(
		context.Background(),
		otelOptions,
		tenancyUsageEmailResolver(f.tenancyService),
		coreusage.RegisterNamedPlugin,
	)
	if errOTelUsage != nil {
		f.otelUsageInitErr = errOTelUsage
		log.WithError(errOTelUsage).Error("failed to initialize OpenTelemetry usage export")
		return
	}
	f.otelUsageSink = otelSink
}

// attachManagement creates the tenant user API handler once the management
// handler exists, because tenant OAuth flows reuse its persistence path.
func (f *forkRuntime) attachManagement(cfg *config.Config, authManager *auth.Manager, mgmt *managementHandlers.Handler) {
	if f == nil {
		return
	}
	f.user = userHandlers.NewHandler(
		cfg,
		authManager,
		f.tenancyService,
		sdkAuth.GetTokenStore(),
		mgmt,
	)
	f.user.SetUserPanelAsset(f.userPanelAsset)
}

// startErr reports initialization failures that must prevent serving traffic.
func (f *forkRuntime) startErr() error {
	if f == nil {
		return nil
	}
	if f.tenancyInitErr != nil {
		return fmt.Errorf("initialize tenancy: %w", f.tenancyInitErr)
	}
	if f.otelUsageInitErr != nil {
		return fmt.Errorf("initialize OpenTelemetry usage export: %w", f.otelUsageInitErr)
	}
	return nil
}

// start launches background fork work for the lifetime of one Server.Start call
// and returns the matching stop function.
func (f *forkRuntime) start(ctx context.Context) func() {
	if f == nil || f.userPanelSupervisor == nil {
		return func() {}
	}
	f.userPanelSupervisor.Start(ctx)
	return f.userPanelSupervisor.Stop
}

// stop releases fork resources after the HTTP server is closed. It always runs.
// When the HTTP shutdown already failed, that error takes precedence and fork
// errors are logged instead of returned.
func (f *forkRuntime) stop(ctx context.Context, errHTTPShutdown error) error {
	if f == nil {
		return nil
	}
	if f.userPanelSupervisor != nil {
		f.userPanelSupervisor.Stop()
	}
	var stopErrors []error
	if f.otelUsageSink != nil {
		if otelPlugin := f.otelUsageSink.detach(); otelPlugin != nil {
			if errShutdownOTel := otelPlugin.Shutdown(ctx); errShutdownOTel != nil {
				stopErrors = append(stopErrors, errShutdownOTel)
			}
		}
	}
	if f.tenancyService != nil {
		if errCloseTenancy := f.tenancyService.Close(); errCloseTenancy != nil {
			stopErrors = append(stopErrors, errCloseTenancy)
		}
	}
	errFork := errors.Join(stopErrors...)
	if errFork != nil && errHTTPShutdown != nil {
		log.WithError(errFork).Error("failed to release fork server resources")
		return nil
	}
	return errFork
}

// reload applies a hot-reloaded configuration to fork components.
func (f *forkRuntime) reload(cfg *config.Config) {
	if f == nil || cfg == nil {
		return
	}
	noteOTelConfigReload()
	if f.tenancyService != nil {
		if err := f.tenancyService.SetConfig(cfg.Tenancy); err != nil {
			log.Errorf("failed to reconfigure tenancy quota: %v", err)
		}
	}
	if f.userPanelSupervisor != nil {
		f.userPanelSupervisor.SetConfig(cfg)
	}
	if f.user != nil {
		f.user.SetConfig(cfg)
	}
}

// tenancyStore returns the tenant store used by access providers, or nil when
// tenancy is disabled or failed to initialize.
func (f *forkRuntime) tenancyStore() tenancy.Store {
	if f == nil || f.tenancyService == nil {
		return nil
	}
	return f.tenancyService.Store()
}

// tenancy returns the tenancy service, or nil when it is not running.
func (f *forkRuntime) tenancy() *tenancy.Service {
	if f == nil {
		return nil
	}
	return f.tenancyService
}
