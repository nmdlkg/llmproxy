//go:build cgo && (linux || darwin || freebsd)

package pluginhost

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// TestNativeTenancySchedulerSmoke loads a real custom-plugins/tenancy-scheduler
// build through the host loader and exercises register, pick, usage, quiesce,
// and shutdown. Build the library first and point the variable at it:
//
//	(cd custom-plugins/tenancy-scheduler && go build -buildmode=c-shared -o /tmp/tenancy-scheduler.so .)
//	TENANCY_SCHEDULER_PLUGIN=/tmp/tenancy-scheduler.so go test ./internal/pluginhost -run TestNativeTenancySchedulerSmoke
func TestNativeTenancySchedulerSmoke(t *testing.T) {
	path := os.Getenv("TENANCY_SCHEDULER_PLUGIN")
	if path == "" {
		t.Skip("TENANCY_SCHEDULER_PLUGIN is not set")
	}
	host := New()
	client, errOpen := dynamicLibraryLoader{}.Open(pluginFile{ID: "tenancy-scheduler", Path: path}, host)
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	defer client.Shutdown()

	ctx := context.Background()
	statePath := filepath.Join(t.TempDir(), "state.json")
	configYAML := []byte("enabled: true\npriority: 100\nmode: optimizer\nstate-path: " + statePath + "\n")
	plugin, errRegister := registerRPCPlugin(ctx, host, "tenancy-scheduler", client, pluginabi.MethodPluginRegister, configYAML)
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	if !validPlugin(plugin) {
		t.Fatalf("native plugin registration rejected by host: metadata = %+v", plugin.Metadata)
	}
	if plugin.Capabilities.Scheduler == nil || plugin.Capabilities.UsagePlugin == nil || plugin.Capabilities.SchedulerAcrossPriorities {
		t.Fatalf("capabilities = %+v", plugin.Capabilities)
	}

	headers := http.Header{}
	headers.Set("x-codex-primary-used-percent", "10")
	headers.Set("x-codex-primary-window-minutes", "300")
	headers.Set("x-codex-primary-reset-after-seconds", "3600")
	headers.Set("x-codex-secondary-used-percent", "100")
	headers.Set("x-codex-secondary-window-minutes", "10080")
	headers.Set("x-codex-secondary-reset-after-seconds", "300000")
	request := pluginapi.SchedulerPickRequest{Provider: "codex", Model: "gpt-5", Candidates: []pluginapi.SchedulerAuthCandidate{
		{ID: "exhausted", Provider: "codex"},
		{ID: "fresh", Provider: "codex"},
	}}
	if _, errPick := plugin.Capabilities.Scheduler.Pick(ctx, request); errPick != nil {
		t.Fatal(errPick)
	}
	plugin.Capabilities.UsagePlugin.HandleUsage(ctx, pluginapi.UsageRecord{
		AuthID: "exhausted", Provider: "codex", Model: "gpt-5", Generate: true, RequestedAt: time.Now(), ResponseHeaders: headers,
	})
	for i := 0; i < 3; i++ {
		response, errPick := plugin.Capabilities.Scheduler.Pick(ctx, request)
		if errPick != nil || !response.Handled || response.AuthID != "fresh" {
			t.Fatalf("pick = %+v, err = %v", response, errPick)
		}
	}
	if _, errQuiesce := callPlugin[rpcEmptyResponse](ctx, client, pluginabi.MethodPluginQuiesce, rpcEmptyResponse{}); errQuiesce != nil {
		t.Fatal(errQuiesce)
	}
	if _, errStat := os.Stat(statePath); errStat != nil {
		t.Fatalf("quiesce did not checkpoint state: %v", errStat)
	}
}
