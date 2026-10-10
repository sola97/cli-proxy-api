package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/pluginhost"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type v8QuotaFixture struct{}

func (v8QuotaFixture) Identifier() string { return "codex" }

func (v8QuotaFixture) DescribeQuota(context.Context, pluginapi.QuotaDescribeRequest) (pluginapi.QuotaDescribeResponse, error) {
	return pluginapi.QuotaDescribeResponse{SupportedProviders: []string{"codex"}, SupportsReset: true}, nil
}

func (v8QuotaFixture) FetchQuota(_ context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	return pluginapi.QuotaFetchResponse{
		Subscription: &pluginapi.QuotaSubscription{Plan: "plus"},
		Summary:      []pluginapi.QuotaMetric{{Key: "remaining", Label: req.AuthID, Value: 75}},
		Groups:       []pluginapi.QuotaGroup{{DisplayName: "5h", Buckets: []pluginapi.QuotaBucket{{RemainingFraction: 0.75}}}},
	}, nil
}

func (v8QuotaFixture) ResetQuota(context.Context, pluginapi.QuotaResetRequest) (pluginapi.QuotaResetResponse, error) {
	return pluginapi.QuotaResetResponse{Success: true}, nil
}

func TestManagementV8QuotaOperations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, tc := range []struct {
		name                      string
		enabled, home, authorized bool
		blockedStatus             int
	}{
		{"authorized", true, false, true, 0},
		{"missing key", true, false, false, http.StatusUnauthorized},
		{"disabled", false, false, true, http.StatusNotFound},
		{"home", true, true, true, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{AuthDir: t.TempDir()}
			cfg.RemoteManagement.SecretKey = "configured-management-key"
			cfg.Home.Enabled = tc.home
			manager := coreauth.NewManager(nil, nil, nil)
			auth := &coreauth.Auth{ID: "quota-auth", Provider: "codex", Status: coreauth.StatusActive}
			index := auth.EnsureIndex()
			if _, err := manager.Register(context.Background(), auth); err != nil {
				t.Fatal(err)
			}
			h := management.NewHandlerWithoutConfigFilePath(cfg, manager)
			h.SetLocalPassword("test-password")
			host := pluginhost.New()
			host.RegisterPluginForTest("quota-fixture", pluginapi.Plugin{Capabilities: pluginapi.Capabilities{QuotaProvider: v8QuotaFixture{}}})
			h.SetPluginHost(host)
			s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
			s.managementRoutesEnabled.Store(tc.enabled)
			s.registerManagementV8Routes()
			for _, operation := range []struct{ method, path string }{
				{http.MethodGet, "/quota/providers"},
				{http.MethodPost, "/quota/fetch"},
				{http.MethodPost, "/quota/reset"},
			} {
				req := httptest.NewRequest(operation.method, "/v8/management"+operation.path, strings.NewReader(`{"auth_index":"`+index+`"}`))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Content-Type", "application/json")
				if tc.authorized {
					req.Header.Set("Authorization", "Bearer test-password")
				}
				rec := httptest.NewRecorder()
				s.engine.ServeHTTP(rec, req)
				want := tc.blockedStatus
				if want == 0 {
					want = http.StatusOK
				}
				if rec.Code != want {
					t.Fatalf("%s: status=%d want=%d body=%s", operation.path, rec.Code, want, rec.Body.String())
				}
				if tc.blockedStatus != 0 {
					continue
				}
				switch operation.path {
				case "/quota/fetch":
					var quota pluginapi.QuotaFetchResponse
					if err := json.Unmarshal(rec.Body.Bytes(), &quota); err != nil {
						t.Fatal(err)
					}
					if quota.Subscription == nil || quota.Subscription.Plan != "plus" || len(quota.Groups) != 1 || quota.Groups[0].Buckets[0].RemainingFraction != 0.75 || len(quota.Summary) != 1 || quota.Summary[0].Label != auth.ID {
						t.Fatalf("unexpected quota response: %s", rec.Body.String())
					}
				case "/quota/providers":
					var providers struct {
						Providers []pluginhost.RegisteredQuotaProviderInfo `json:"providers"`
					}
					if err := json.Unmarshal(rec.Body.Bytes(), &providers); err != nil {
						t.Fatal(err)
					}
					if len(providers.Providers) != 1 || providers.Providers[0].Provider != "codex" {
						t.Fatalf("unexpected providers: %s", rec.Body.String())
					}
				case "/quota/reset":
					if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
						t.Fatalf("unexpected reset: %s", rec.Body.String())
					}
				}
			}
			if tc.blockedStatus != 0 {
				return
			}
			for _, path := range []string{"/quota/fetch", "/quota/reset"} {
				req := httptest.NewRequest(http.MethodPost, "/v8/management"+path, strings.NewReader(`{}`))
				req.RemoteAddr = "127.0.0.1:1234"
				req.Header.Set("Authorization", "Bearer test-password")
				rec := httptest.NewRecorder()
				s.engine.ServeHTTP(rec, req)
				if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "auth_index is required") {
					t.Fatalf("%s: %d %s", path, rec.Code, rec.Body.String())
				}
			}
		})
	}
}
