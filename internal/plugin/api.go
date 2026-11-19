package plugin

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/sketchain/cpa-quota/internal/ccbilling"
)

// Resource routes, relative to /v0/resource/plugins/cpa-quota/. CLIProxyAPI
// serves plugin resources for GET only and without its own authentication,
// so each route checks the plugin's bearer tokens itself.
const (
	routeQuota            = "quota"
	routeRebuild          = "admin/rebuild"
	routeRefreshPrices    = "admin/refresh-prices"
	routeCalibrationStart = "admin/calibrations/start"
	routeCalibrationStop  = "admin/calibrations/stop"
	routeCalibrationDrop  = "admin/calibrations/delete"
)

// resourceBasePath is CLIProxyAPI's prefix of plugin resources.
const resourceBasePath = "/v0/resource/plugins/"

// RegisterManagement declares the plugin's resource routes.
func (a *App) RegisterManagement(request ManagementRegistrationRequest) managementRegistration {
	a.mu.Lock()
	a.resourceBase = strings.TrimRight(strings.TrimSpace(request.ResourceBasePath), "/")
	a.mu.Unlock()
	return managementRegistration{Resources: []resourceRoute{
		{Path: routeQuota, Description: "Quota estimates of this instance's subscription accounts (Bearer api_token)."},
		{Path: routeRebuild, Description: "Rebuild the estimates from the stored history (Bearer admin_token)."},
		{Path: routeRefreshPrices, Description: "Fetch the ratio table now (Bearer admin_token)."},
		{Path: routeCalibrationStart, Description: "Start calibrating a model weight item (Bearer admin_token)."},
		{Path: routeCalibrationStop, Description: "End a calibration (Bearer admin_token)."},
		{Path: routeCalibrationDrop, Description: "Delete a calibration (Bearer admin_token)."},
	}}
}

// route is the request path relative to the plugin's resource prefix.
func (a *App) route(path string) string {
	a.mu.Lock()
	base := a.resourceBase
	a.mu.Unlock()
	path = strings.TrimRight(path, "/")
	if base != "" {
		if rest, ok := strings.CutPrefix(path, base+"/"); ok {
			return rest
		}
	}
	if rest, ok := strings.CutPrefix(path, resourceBasePath); ok {
		if _, route, found := strings.Cut(rest, "/"); found {
			return route
		}
	}
	return strings.TrimPrefix(path, "/")
}

// HandleManagement answers a resource request.
func (a *App) HandleManagement(request ManagementRequest) ManagementResponse {
	config, _, _, _, running := a.state()
	if !running {
		return problem(http.StatusServiceUnavailable, "not_configured", "cpa-quota is not configured")
	}
	if request.Method != "" && !strings.EqualFold(request.Method, http.MethodGet) {
		return problem(http.StatusMethodNotAllowed, "method_not_allowed", "only GET is supported")
	}
	route := a.route(request.Path)
	switch route {
	case routeQuota:
		if response, ok := authorize(request, config.APIToken, "api_token"); !ok {
			return response
		}
		return reply(http.StatusOK, a.Quota())
	case routeRebuild, routeRefreshPrices, routeCalibrationStart, routeCalibrationStop, routeCalibrationDrop:
		if response, ok := authorize(request, config.AdminToken, "admin_token"); !ok {
			return response
		}
		return a.admin(route, request)
	}
	return problem(http.StatusNotFound, "not_found", "unknown cpa-quota route")
}

// authorize checks the request's bearer token against token.
func authorize(request ManagementRequest, token, name string) (ManagementResponse, bool) {
	if token == "" {
		return problem(http.StatusUnauthorized, "unauthorized", name+" is not configured, so this route is disabled"), false
	}
	header := strings.TrimSpace(request.Headers.Get("Authorization"))
	scheme, given, found := strings.Cut(header, " ")
	if !found || !strings.EqualFold(scheme, "Bearer") || subtle.ConstantTimeCompare([]byte(strings.TrimSpace(given)), []byte(token)) != 1 {
		response := problem(http.StatusUnauthorized, "unauthorized", "a valid bearer token is required")
		response.Headers.Set("WWW-Authenticate", `Bearer realm="cpa-quota"`)
		return response, false
	}
	return ManagementResponse{}, true
}

// DatabaseStatus describes the database in the quota API.
type DatabaseStatus struct {
	// Status is ok or unavailable.
	Status        string     `json:"status"`
	LastError     string     `json:"last_error,omitempty"`
	FailedAt      *time.Time `json:"failed_at,omitempty"`
	PendingWrites int        `json:"pending_writes"`
	DroppedWrites int64      `json:"dropped_writes"`
	SchemaVersion int        `json:"schema_version"`
}

// EstimatesStatus describes the estimate snapshot.
type EstimatesStatus struct {
	// Loaded is false until a refresh with the database has produced an
	// estimate; until then every account is in cold start.
	Loaded      bool       `json:"loaded"`
	EstimatedAt *time.Time `json:"estimated_at"`
	LastError   string     `json:"last_error,omitempty"`
}

// QuotaResponse is the quota API's body.
type QuotaResponse struct {
	Plugin       string                   `json:"plugin"`
	Version      string                   `json:"version"`
	Instance     string                   `json:"instance"`
	GeneratedAt  time.Time                `json:"generated_at"`
	Subscription string                   `json:"subscription"`
	Tier         string                   `json:"tier"`
	PriceTable   PriceStatus              `json:"price_table"`
	Database     DatabaseStatus           `json:"database"`
	Estimates    EstimatesStatus          `json:"estimates"`
	Accounts     []ccbilling.AccountQuota `json:"accounts"`
}

// Quota builds the quota API's body. It refreshes the estimates when their
// interval has passed and no refresh runs, and otherwise reads memory only.
func (a *App) Quota() QuotaResponse {
	config, db, manager, prices, _ := a.state()
	manager.MaybeRefresh()
	settings := manager.Settings()
	tier := config.Tier
	if tier == "" {
		tier = settings.Providers[config.Provider()].Tiers[0].ID
	}
	response := QuotaResponse{Plugin: PluginID, Version: Version, Instance: config.Instance, GeneratedAt: a.Now().UTC(),
		Subscription: config.Subscription, Tier: tier, PriceTable: prices.status(config.PriceURL), Accounts: manager.Quotas()}
	status := db.Status()
	response.Database = DatabaseStatus{Status: "ok", LastError: status.LastError, PendingWrites: status.Pending, DroppedWrites: status.Dropped,
		SchemaVersion: status.SchemaVersion}
	if !status.Available {
		response.Database.Status = "unavailable"
	}
	if !status.FailedAt.IsZero() {
		at := status.FailedAt.UTC()
		response.Database.FailedAt = &at
	}
	view := manager.View()
	response.Estimates = EstimatesStatus{Loaded: view.Loaded, LastError: view.Error}
	if !view.At.IsZero() {
		at := view.At.UTC()
		response.Estimates.EstimatedAt = &at
	}
	return response
}

func (a *App) admin(route string, request ManagementRequest) ManagementResponse {
	config, _, manager, prices, _ := a.state()
	switch route {
	case routeRebuild:
		err := manager.Rebuild(seconds(config.RebuildBudgetSeconds))
		if err != nil {
			return failure(err)
		}
		a.logf("Estimates rebuilt from the stored history")
		quota := a.Quota()
		return reply(http.StatusOK, map[string]any{"ok": true, "estimates": quota.Estimates, "accounts": quota.Accounts})
	case routeRefreshPrices:
		err := a.refreshPrices(false)
		status := prices.status(config.PriceURL)
		if err != nil {
			return reply(http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error(), "price_table": status})
		}
		return reply(http.StatusOK, map[string]any{"ok": true, "price_table": status})
	case routeCalibrationStart:
		calibration, err := manager.StartCalibration(request.Query.Get("item"), request.Query.Get("auth_index"))
		if err != nil {
			return failure(err)
		}
		return reply(http.StatusOK, map[string]any{"ok": true, "calibration": calibration})
	case routeCalibrationStop, routeCalibrationDrop:
		id, err := strconv.ParseInt(request.Query.Get("id"), 10, 64)
		if err != nil || id <= 0 {
			return problem(http.StatusBadRequest, "invalid_request", "id must be a calibration ID")
		}
		if route == routeCalibrationStop {
			err = manager.StopCalibration(id)
		} else {
			err = manager.DeleteCalibration(id)
		}
		if err != nil {
			return failure(err)
		}
		return reply(http.StatusOK, map[string]any{"ok": true, "id": id})
	}
	return problem(http.StatusNotFound, "not_found", "unknown cpa-quota route")
}

// failure maps an operation error to a response.
func failure(err error) ManagementResponse {
	switch {
	case ccbilling.IsInvalid(err):
		return problem(http.StatusBadRequest, "invalid_request", err.Error())
	case ccbilling.IsCalibrationNotFound(err):
		return problem(http.StatusNotFound, "not_found", "calibration not found or already ended")
	case errUnavailable(err):
		return problem(http.StatusServiceUnavailable, "database_unavailable", err.Error())
	}
	return problem(http.StatusInternalServerError, "internal_error", err.Error())
}

func reply(status int, body any) ManagementResponse {
	raw, err := json.Marshal(body)
	if err != nil {
		return problem(http.StatusInternalServerError, "internal_error", err.Error())
	}
	return ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"},
		"Cache-Control": {"no-store"}}, Body: raw}
}

func problem(status int, code, message string) ManagementResponse {
	raw, _ := json.Marshal(map[string]any{"error": map[string]string{"code": code, "message": message}})
	return ManagementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"},
		"Cache-Control": {"no-store"}}, Body: raw}
}
