package user

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/userpanelasset"
)

type fakeUserPanelAsset struct {
	status    userpanelasset.Status
	refreshes int
	err       error
}

func (f *fakeUserPanelAsset) Status() userpanelasset.Status { return f.status }

func (f *fakeUserPanelAsset) Refresh(context.Context) error {
	f.refreshes++
	return f.err
}

func TestAdminUserPanelRoutesRequireAdminAndEmptyJSON(t *testing.T) {
	harness := newUserTestHarness(t, true)
	panel := &fakeUserPanelAsset{status: userpanelasset.Status{Source: userpanelasset.SourceUnavailable}}
	harness.engine = gin.New()
	harness.handler.SetUserPanelAsset(panel)
	harness.handler.RegisterRoutes(harness.engine, harness.testAuthMiddleware())

	if response := harness.request(t, "user-a", http.MethodGet, "/v0/user/admin/ui/status", nil); response.Code != http.StatusForbidden {
		t.Fatalf("regular user status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := harness.request(t, "admin", http.MethodGet, "/v0/user/admin/ui/status", nil); response.Code != http.StatusOK {
		t.Fatalf("admin status = %d, body = %s", response.Code, response.Body.String())
	}
	if response := harness.request(t, "admin", http.MethodPost, "/v0/user/admin/ui/refresh", []byte(`{"unexpected":true}`)); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid refresh body = %d, body = %s", response.Code, response.Body.String())
	}
	withoutBearer := httptest.NewRequest(http.MethodPost, "/v0/user/admin/ui/refresh", strings.NewReader(`{}`))
	withoutBearer.Header.Set("X-Test-User", "admin")
	withoutBearer.Header.Set("Content-Type", "application/json")
	withoutBearerResponse := httptest.NewRecorder()
	harness.engine.ServeHTTP(withoutBearerResponse, withoutBearer)
	if withoutBearerResponse.Code != http.StatusUnauthorized {
		t.Fatalf("refresh without Bearer = %d, body = %s", withoutBearerResponse.Code, withoutBearerResponse.Body.String())
	}
	if response := harness.request(t, "admin", http.MethodPost, "/v0/user/admin/ui/refresh", []byte(`{}`)); response.Code != http.StatusOK {
		t.Fatalf("empty refresh body = %d, body = %s", response.Code, response.Body.String())
	}
	if panel.refreshes != 1 {
		t.Fatalf("refresh calls = %d, want 1", panel.refreshes)
	}
}

func TestAdminUserPanelRefreshMapsUpdaterErrors(t *testing.T) {
	harness := newUserTestHarness(t, true)
	harness.engine = gin.New()
	harness.handler.SetUserPanelAsset(&fakeUserPanelAsset{err: errors.New("remote unavailable")})
	harness.handler.RegisterRoutes(harness.engine, harness.testAuthMiddleware())
	response := harness.request(t, "admin", http.MethodPost, "/v0/user/admin/ui/refresh", []byte(`{}`))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("updater error status = %d, body = %s", response.Code, response.Body.String())
	}
}
