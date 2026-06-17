package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kilolockio/kilolock/internal/portalauth"
	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/config"
	"github.com/kilolockio/kilolock/pkg/provision"
	"github.com/kilolockio/kilolock/pkg/store"
)

type server struct {
	control            *store.Store
	cfg                config.Config
	logger             *slog.Logger
	apiToken           string
	portalServiceToken string
	httpClient         *http.Client
	cloud              *cloudFeatures
}

func newServer(control *store.Store, cfg config.Config, logger *slog.Logger, apiToken string) *server {
	return &server{
		control:            control,
		cfg:                cfg,
		logger:             logger,
		apiToken:           apiToken,
		portalServiceToken: strings.TrimSpace(os.Getenv("KL_PORTAL_SERVICE_TOKEN")),
		httpClient:         &http.Client{Timeout: 20 * time.Second},
		cloud:              newCloudFeatures(),
	}
}

func (s *server) listenAndServe(addr string) error {
	return http.ListenAndServe(addr, s.routes())
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/", s.withRequestID(muxHandler(s)))
	return mux
}

func muxHandler(s *server) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	})
	mux.HandleFunc("/bootstrap/status", s.handleBootstrapStatus)
	mux.HandleFunc("/bootstrap/init", s.handleBootstrapInit)
	s.registerCloudRoutes(mux)
	mux.HandleFunc("/", s.handleUI)
	mux.HandleFunc("/portal", s.handlePortalUI)
	mux.HandleFunc("/portal/", s.handlePortalUI)
	mux.HandleFunc("/portal/api", s.handlePortalAPI)
	mux.HandleFunc("/portal/api/", s.handlePortalAPI)
	mux.Handle("/api/", s.withPublicAPIGuard(s.withAuth(http.HandlerFunc(s.handleAPI))))
	return mux
}

func (s *server) withRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get("X-Request-ID"))
		if id == "" {
			id = fmt.Sprintf("req-%d", time.Now().UnixNano())
		}
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

func (s *server) withPublicAPIGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.publicAPIDisabled() && !s.isPortalServiceRequest(r) && bearerToken(r) == "" {
			writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) publicAPIDisabled() bool {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("KL_CONTROL_PUBLIC_API")))
	switch mode {
	case "enabled":
		return false
	case "disabled":
		return true
	case "":
		return strings.EqualFold(strings.TrimSpace(s.cfg.ResolvedInitMode()), "prod")
	default:
		return true
	}
}

func (s *server) withAuth(next http.Handler) http.Handler {
	if s.apiToken == "" {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if bearerToken(r) == "" && !s.isPortalServiceRequest(r) {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized"})
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) isPortalServiceRequest(r *http.Request) bool {
	if strings.TrimSpace(s.portalServiceToken) == "" {
		return false
	}
	got := strings.TrimSpace(r.Header.Get("X-Kl-Portal-Service-Token"))
	return got != "" && got == s.portalServiceToken
}

func (s *server) handleAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api")
	permission, guarded := requiredPermissionForRoute(r.Method, path)
	if !guarded {
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}

	if s.handleCloudAPI(w, r, path, permission) {
		return
	}

	switch {
	case r.Method == http.MethodGet && path == "/tenants":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiTenantsList(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && !strings.Contains(strings.TrimPrefix(path, "/tenants/"), "/"):
		tenant := strings.TrimPrefix(path, "/tenants/")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiTenantGet(w, r, tenant)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/members"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/members")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiTenantMembersList(w, r, tenant)
	case r.Method == http.MethodPost && path == "/tenants":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiTenantCreate(w, r)
	case r.Method == http.MethodPost && path == "/tenants/lifecycle":
		s.apiTenantLifecycleSet(w, r)
	case r.Method == http.MethodPost && path == "/tenants/delete":
		s.apiTenantDelete(w, r)
	case r.Method == http.MethodPost && path == "/tenants/entitlements":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiTenantEntitlementsSet(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/environments")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiEnvironmentsList(w, r, tenant)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/environments")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiEnvironmentCreate(w, r, tenant)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments/rename"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/environments/rename")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiEnvironmentRename(w, r, tenant)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments/lifecycle"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/environments/lifecycle")
		s.apiEnvironmentLifecycleSet(w, r, tenant)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments/delete"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/environments/delete")
		s.apiEnvironmentDelete(w, r, tenant)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/tokens"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/tokens")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiTokensList(w, r, tenant)
	case r.Method == http.MethodGet && path == "/environments/access":
		s.apiEnvironmentPATAccessList(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/tokens"):
		tenant := strings.TrimSuffix(strings.TrimPrefix(path, "/tenants/"), "/tokens")
		if !s.requirePermission(w, r, permission, tenant, "") {
			return
		}
		s.apiTokenCreate(w, r, tenant)
	case r.Method == http.MethodPost && path == "/tokens/lifecycle":
		s.apiTokenLifecycleSet(w, r)
	case r.Method == http.MethodPost && path == "/retention/purge":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiRetentionPurge(w, r)
	case r.Method == http.MethodPost && path == "/admin/environment/validate-routing":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiEnvironmentValidateRouting(w, r)
	case r.Method == http.MethodPost && path == "/admin/environment/upgrade-dedicated":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiEnvironmentUpgradeDedicated(w, r)
	case r.Method == http.MethodGet && path == "/rbac/grants":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiRBACGrantsList(w, r)
	case r.Method == http.MethodGet && path == "/operators/tokens":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiControlOperatorTokensList(w, r)
	case r.Method == http.MethodGet && path == "/platform/iac-versions":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiPlatformIACVersions(w, r)
	case r.Method == http.MethodPost && path == "/rbac/grants":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiRBACGrantCreate(w, r)
	case r.Method == http.MethodPost && path == "/operators/tokens":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiControlOperatorTokenCreate(w, r)
	case r.Method == http.MethodPost && path == "/rbac/grants/revoke":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiRBACGrantRevoke(w, r)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/states/"):
		parts := strings.Split(strings.TrimPrefix(path, "/states/"), "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path must be /api/states/{tenant}/{environment}"})
			return
		}
		if !s.requirePermission(w, r, permission, parts[0], parts[1]) {
			return
		}
		s.apiStatesList(w, r, parts[0], parts[1])
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/delete"):
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/states/"), "/delete"), "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path must be /api/states/{tenant}/{environment}/delete"})
			return
		}
		if !s.requirePermission(w, r, permission, parts[0], parts[1]) {
			return
		}
		s.apiStateDelete(w, r, parts[0], parts[1])
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/destroy"):
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/states/"), "/destroy"), "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path must be /api/states/{tenant}/{environment}/destroy"})
			return
		}
		if !s.requirePermission(w, r, permission, parts[0], parts[1]) {
			return
		}
		s.apiStateDestroy(w, r, parts[0], parts[1])
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/lifecycle"):
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/states/"), "/lifecycle"), "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path must be /api/states/{tenant}/{environment}/lifecycle"})
			return
		}
		if !s.requirePermission(w, r, permission, parts[0], parts[1]) {
			return
		}
		s.apiStateLifecycleSet(w, r, parts[0], parts[1])
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/config"):
		parts := strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/states/"), "/config"), "/")
		if len(parts) != 2 {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "path must be /api/states/{tenant}/{environment}/config"})
			return
		}
		if !s.requirePermission(w, r, permission, parts[0], parts[1]) {
			return
		}
		s.apiStateConfigSet(w, r, parts[0], parts[1])
	case r.Method == http.MethodGet && path == "/ownership-transfers":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiOwnershipTransfersList(w, r)
	case r.Method == http.MethodPost && path == "/ownership-transfers":
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiOwnershipTransferCreate(w, r)
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/accept"):
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiOwnershipTransferResolve(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/ownership-transfers/"), "/accept"), "accept")
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/reject"):
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiOwnershipTransferResolve(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/ownership-transfers/"), "/reject"), "reject")
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/cancel"):
		if !s.requirePermission(w, r, permission, "*", "") {
			return
		}
		s.apiOwnershipTransferResolve(w, r, strings.TrimSuffix(strings.TrimPrefix(path, "/ownership-transfers/"), "/cancel"), "cancel")
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}

func requiredPermissionForRoute(method, path string) (string, bool) {
	if p, ok := cloudPermissionForRoute(method, path); ok {
		return p, true
	}
	switch {
	case method == http.MethodGet && path == "/tenants":
		return "tenant.read", true
	case method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && !strings.Contains(strings.TrimPrefix(path, "/tenants/"), "/"):
		return "tenant.read", true
	case method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/members"):
		return "tenant.read", true
	case method == http.MethodPost && path == "/tenants":
		return "tenant.create", true
	case method == http.MethodPost && path == "/tenants/lifecycle":
		return "tenant.lifecycle.update", true
	case method == http.MethodPost && path == "/tenants/delete":
		return "tenant.lifecycle.update", true
	case method == http.MethodPost && path == "/tenants/entitlements":
		return "tenant.entitlements.update", true
	case method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments"):
		return "environment.read", true
	case method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments"):
		return "environment.create", true
	case method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments/rename"):
		return "environment.create", true
	case method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments/lifecycle"):
		return "environment.lifecycle.update", true
	case method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/environments/delete"):
		return "environment.lifecycle.update", true
	case method == http.MethodGet && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/tokens"):
		return "token.read", true
	case method == http.MethodGet && path == "/environments/access":
		return "environment.read", true
	case method == http.MethodPost && strings.HasPrefix(path, "/tenants/") && strings.HasSuffix(path, "/tokens"):
		return "token.create", true
	case method == http.MethodPost && path == "/tokens/lifecycle":
		return "token.lifecycle.update", true
	case method == http.MethodPost && path == "/retention/purge":
		return "retention.purge", true
	case method == http.MethodPost && path == "/admin/environment/validate-routing":
		return "environment.read", true
	case method == http.MethodPost && path == "/admin/environment/upgrade-dedicated":
		return "environment.create", true
	case method == http.MethodGet && path == "/rbac/grants":
		return "rbac.manage", true
	case method == http.MethodGet && path == "/operators/tokens":
		return "rbac.manage", true
	case method == http.MethodGet && path == "/platform/iac-versions":
		return "rbac.manage", true
	case method == http.MethodPost && path == "/rbac/grants":
		return "rbac.manage", true
	case method == http.MethodPost && path == "/operators/tokens":
		return "rbac.manage", true
	case method == http.MethodPost && path == "/rbac/grants/revoke":
		return "rbac.manage", true
	case method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/config"):
		return "state.config.update", true
	case method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/delete"):
		return "state.delete", true
	case method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/destroy"):
		return "state.delete", true
	case method == http.MethodPost && strings.HasPrefix(path, "/states/") && strings.HasSuffix(path, "/lifecycle"):
		return "state.delete", true
	case method == http.MethodGet && strings.HasPrefix(path, "/states/"):
		return "tenant.read", true
	case method == http.MethodGet && path == "/ownership-transfers":
		return "tenant.read", true
	case method == http.MethodPost && path == "/ownership-transfers":
		return "environment.transfer.update", true
	case method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/accept"):
		return "environment.transfer.update", true
	case method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/reject"):
		return "environment.transfer.update", true
	case method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/cancel"):
		return "environment.transfer.update", true
	default:
		return "", false
	}
}

func bearerToken(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("Authorization"))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(strings.ToLower(raw), "bearer ") {
		return strings.TrimSpace(raw[7:])
	}
	return strings.TrimSpace(strings.TrimPrefix(raw, "Bearer"))
}

func (s *server) requirePermission(w http.ResponseWriter, r *http.Request, permission, targetTenant, targetEnv string) bool {
	if s.apiToken == "" {
		return true
	}
	// Legacy static control token remains superuser for bootstrap/self-hosted.
	secret := bearerToken(r)
	if s.apiToken != "" && secret == s.apiToken {
		return true
	}
	// Portal service token is an internal boundary between portal and control.
	// It is deliberately narrow: only a small allowlist of customer-facing
	// permissions is granted, and only for tenant/environment-scoped routes.
	if s.isPortalServiceRequest(r) {
		if portalAllowsPermission(permission) && (targetTenant != "*" || permission == "environment.transfer.update") {
			return true
		}
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
		return false
	}
	p, ok, err := s.control.AuthorizeControlAPIToken(r.Context(), secret, permission)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return false
	}
	if !ok {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: missing permission " + permission})
		return false
	}
	scoped, err := s.control.AuthorizeControlPrincipalScope(r.Context(), p, permission, targetTenant, targetEnv)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return false
	}
	if !scoped {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: scope mismatch for requested tenant/environment"})
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), controlPrincipalKey{}, p))
	return true
}

type controlPrincipalKey struct{}

func portalAllowsPermission(permission string) bool {
	switch strings.TrimSpace(permission) {
	case "tenant.read",
		"environment.read",
		"environment.create",
		"environment.lifecycle.update",
		"environment.transfer.update",
		"state.delete",
		"token.read",
		"token.create",
		"token.lifecycle.update",
		"tenant.billing.checkout":
		return true
	default:
		return false
	}
}

func (s *server) handleBootstrapStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	status, err := s.control.GetSystemInitStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"initialized":    status.Initialized,
		"init_mode":      status.InitMode,
		"initialized_by": status.InitializedBy,
		"initialized_at": status.InitializedAt,
	})
}

func (s *server) handleBootstrapInit(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	status, err := s.control.GetSystemInitStatus(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if status.Initialized {
		at := ""
		if status.InitializedAt != nil {
			at = status.InitializedAt.UTC().Format(time.RFC3339)
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":          "already initialized",
			"initialized_by": status.InitializedBy,
			"initialized_at": at,
			"init_mode":      status.InitMode,
		})
		return
	}
	var in struct {
		Tenant    string `json:"tenant"`
		Name      string `json:"tenant_name"`
		TokenName string `json:"token_name"`
		Token     string `json:"token"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tenant := strings.TrimSpace(in.Tenant)
	if tenant == "" {
		tenant = "operator"
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		name = "Operator"
	}
	tokenName := strings.TrimSpace(in.TokenName)
	if tokenName == "" {
		tokenName = "operator-bootstrap"
	}
	secret := strings.TrimSpace(in.Token)
	if secret == "" {
		generated, _, _, err := auth.NewAPIToken()
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		secret = generated
	}
	if err := s.control.BootstrapTenantToken(r.Context(), tenant, name, tokenName, secret); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.control.EnsureAPITokenRoleByName(r.Context(), tenant, "default", tokenName, "platform_admin", "control-init"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.control.MarkSystemInitialized(r.Context(), "prod", "control-init"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant":       tenant,
		"tenant_name":  name,
		"token_name":   tokenName,
		"token_secret": secret,
	})
}

func (s *server) apiRBACGrantsList(w http.ResponseWriter, r *http.Request) {
	includeRevoked := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_revoked")), "true")
	rows, err := s.control.ListRBACGrants(r.Context(), includeRevoked)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"grants": rows})
}

func (s *server) apiControlOperatorTokensList(w http.ResponseWriter, r *http.Request) {
	includeInactive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_inactive")), "true")
	rows, err := s.control.ListControlOperatorTokens(r.Context(), includeInactive)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": rows})
}

func (s *server) apiPlatformIACVersions(w http.ResponseWriter, r *http.Request) {
	limit := parsePositiveInt(r.URL.Query().Get("limit"), 50)
	rows, err := s.control.ListIACVersionUsage(r.Context(), limit)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"versions": rows})
}

func (s *server) apiRBACGrantCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SubjectKind string `json:"subject_kind"`
		SubjectID   string `json:"subject_id"`
		RoleKey     string `json:"role_key"`
		ScopeKind   string `json:"scope_kind"`
		ScopeRef    string `json:"scope_ref"`
		GrantedBy   string `json:"granted_by"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.control.EnsurePrincipalRole(r.Context(), in.SubjectKind, in.SubjectID, in.RoleKey, in.ScopeKind, in.ScopeRef, in.GrantedBy); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiControlOperatorTokenCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name      string `json:"name"`
		RoleKey   string `json:"role_key"`
		ScopeKind string `json:"scope_kind"`
		ScopeRef  string `json:"scope_ref"`
		GrantedBy string `json:"granted_by"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	row, secret, err := s.control.CreateControlOperatorToken(r.Context(), in.Name, in.RoleKey, in.ScopeKind, in.ScopeRef, firstNonEmptyControlActor(in.GrantedBy, controlActorFromContext(r.Context())))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":        row,
		"token_secret": secret,
	})
}

func (s *server) apiRBACGrantRevoke(w http.ResponseWriter, r *http.Request) {
	var in struct {
		GrantID   string `json:"grant_id"`
		RevokedBy string `json:"revoked_by"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if err := s.control.RevokePrincipalRoleByID(r.Context(), in.GrantID, in.RevokedBy); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiRetentionPurge(w http.ResponseWriter, r *http.Request) {
	var in struct {
		OlderThanHours int    `json:"older_than_hours"`
		Tenant         string `json:"tenant"`
		Actor          string `json:"actor"`
		Reason         string `json:"reason"`
		Apply          bool   `json:"apply"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if in.OlderThanHours <= 0 {
		in.OlderThanHours = 24 * 30
	}
	actor := firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context()))
	if err := validateRetentionPurgeApply(in.Apply, in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	cutoff := time.Now().UTC().Add(-time.Duration(in.OlderThanHours) * time.Hour)
	candidates, err := s.control.ListArchivedTenantPurgeCandidates(r.Context(), cutoff, in.Tenant)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	out := map[string]any{
		"cutoff":     cutoff.Format(time.RFC3339),
		"apply":      in.Apply,
		"candidates": candidates,
	}
	if !in.Apply {
		for _, c := range candidates {
			_ = s.control.RecordRetentionPurgeAudit(r.Context(), store.RetentionPurgeAuditEvent{
				TenantSlug: c.TenantSlug,
				CutoffAt:   cutoff,
				Actor:      actor,
				Reason:     in.Reason,
				ApplyMode:  false,
				Status:     "dry_run",
			})
		}
		out["dry_run"] = true
		writeJSON(w, http.StatusOK, out)
		return
	}
	results := make([]store.ArchivedTenantPurgeResult, 0, len(candidates))
	for _, c := range candidates {
		res, err := s.control.PurgeArchivedTenant(r.Context(), c.TenantSlug, cutoff)
		if err != nil {
			_ = s.control.RecordRetentionPurgeAudit(r.Context(), store.RetentionPurgeAuditEvent{
				TenantSlug:   c.TenantSlug,
				CutoffAt:     cutoff,
				Actor:        actor,
				Reason:       in.Reason,
				ApplyMode:    true,
				Status:       "failed",
				ErrorMessage: err.Error(),
			})
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error(), "tenant": c.TenantSlug})
			return
		}
		_ = s.control.RecordRetentionPurgeAudit(r.Context(), store.RetentionPurgeAuditEvent{
			TenantSlug:          c.TenantSlug,
			CutoffAt:            cutoff,
			Actor:               actor,
			Reason:              in.Reason,
			ApplyMode:           true,
			Status:              "applied",
			DeletedTenants:      res.DeletedTenants,
			DeletedEnvironments: res.DeletedEnvironments,
			DeletedAPITokens:    res.DeletedAPITokens,
		})
		results = append(results, res)
	}
	out["results"] = results
	out["purged"] = len(results)
	writeJSON(w, http.StatusOK, out)
}

func (s *server) apiTenantsList(w http.ResponseWriter, r *http.Request) {
	includeInactive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_inactive")), "true")
	if includeInactive && !s.requireGlobalScopeForIncludeInactive(w, r, "tenant.read") {
		return
	}
	filter := parseListFilter(r.URL.Query())
	var (
		rows []store.TenantRow
		err  error
	)
	if includeInactive {
		rows, err = s.control.ListTenantsAll(r.Context())
	} else {
		rows, err = s.control.ListTenants(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	filtered := filterTenants(rows, filter)
	page, total := paginateAny(filtered, filter.Offset, filter.Limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"tenants": page,
		"meta": map[string]any{
			"total":  total,
			"limit":  filter.Limit,
			"offset": filter.Offset,
		},
	})
}

func (s *server) apiTenantCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug      string `json:"slug"`
		Name      string `json:"name"`
		Provision bool   `json:"provision"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	row, err := s.control.CreateTenantWithDefaultEnvironment(
		r.Context(), in.Slug, in.Name, s.cfg.ResolvedAutoDefaultEnvironment(),
	)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.Provision {
		env, err := s.control.GetEnvironmentByTenantSlug(r.Context(), row.Slug, "default")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if err := s.provisionEnvironment(r.Context(), row, env); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *server) apiEnvironmentsList(w http.ResponseWriter, r *http.Request, tenant string) {
	includeInactive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_inactive")), "true")
	if includeInactive && !s.requireGlobalScopeForIncludeInactive(w, r, "environment.read") {
		return
	}
	filter := parseListFilter(r.URL.Query())
	var (
		rows []store.EnvironmentRow
		err  error
	)
	if includeInactive {
		rows, err = s.control.ListEnvironmentsAll(r.Context(), tenant)
	} else {
		rows, err = s.control.ListEnvironments(r.Context(), tenant)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	filtered := filterEnvironments(rows, filter)
	page, total := paginateAny(filtered, filter.Offset, filter.Limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"environments": page,
		"meta": map[string]any{
			"total":  total,
			"limit":  filter.Limit,
			"offset": filter.Offset,
		},
	})
}

func (s *server) apiEnvironmentCreate(w http.ResponseWriter, r *http.Request, tenant string) {
	var in struct {
		Slug      string `json:"slug"`
		Instance  string `json:"instance"`
		Tier      string `json:"tier"`
		Provision bool   `json:"provision"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tier := store.EnvironmentTier(strings.TrimSpace(in.Tier))
	if tier == "" {
		tier = store.EnvironmentTierSharedHost
	}
	row, err := s.control.CreateEnvironment(r.Context(), tenant, in.Slug, tier, in.Instance)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if in.Provision && tier == store.EnvironmentTierSharedHost {
		trow, err := s.control.GetTenantBySelector(r.Context(), tenant)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if err := s.provisionEnvironment(r.Context(), trow, row); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		row, _ = s.control.GetEnvironmentByID(r.Context(), row.ID)
	}
	writeJSON(w, http.StatusCreated, row)
}

func (s *server) apiEnvironmentValidateRouting(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Ping bool `json:"ping"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	envs, err := s.control.ListAllEnvironments(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	var errs []string
	seen := map[string]struct{}{}
	toPing := make([]string, 0)
	for _, env := range envs {
		key := strings.TrimSpace(env.DatabaseInstanceKey)
		if key == "" {
			key = "shared"
		}
		if key != "shared" {
			if strings.TrimSpace(s.cfg.DataPlaneInstanceURLs[key]) == "" {
				errs = append(errs, fmt.Sprintf("%s/%s uses instance=%q but KL_DATA_PLANE_DSN_%s is not set", env.TenantSlug, env.Slug, key, strings.ToUpper(strings.ReplaceAll(key, "-", "_"))))
			}
			if strings.TrimSpace(s.cfg.DataPlaneInstanceAdminURLs[key]) == "" {
				errs = append(errs, fmt.Sprintf("%s/%s uses instance=%q but KL_DATA_PLANE_ADMIN_DSN_%s is not set", env.TenantSlug, env.Slug, key, strings.ToUpper(strings.ReplaceAll(key, "-", "_"))))
			}
		}
		if strings.TrimSpace(env.DatabaseDSN) != "" {
			if _, ok := seen[env.DatabaseDSN]; !ok {
				seen[env.DatabaseDSN] = struct{}{}
				toPing = append(toPing, env.DatabaseDSN)
			}
		}
	}
	if in.Ping {
		for _, dsn := range toPing {
			p, err := pgxpool.New(r.Context(), dsn)
			if err != nil {
				errs = append(errs, fmt.Sprintf("ping environment database_dsn failed: %v", err))
				continue
			}
			if err := p.Ping(r.Context()); err != nil {
				errs = append(errs, fmt.Sprintf("ping environment database_dsn failed: %v", err))
			}
			p.Close()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                len(errs) == 0,
		"environment_count": len(envs),
		"errors":            errs,
		"ping_enabled":      in.Ping,
	})
}

func (s *server) apiEnvironmentUpgradeDedicated(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Tenant string `json:"tenant"`
		Slug   string `json:"slug"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	row, err := s.control.RequestDedicatedUpgrade(r.Context(), strings.TrimSpace(in.Tenant), strings.TrimSpace(in.Slug))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *server) apiEnvironmentRename(w http.ResponseWriter, r *http.Request, tenant string) {
	var in struct {
		Environment string `json:"environment"`
		NewSlug     string `json:"new_slug"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	row, err := s.control.RenameEnvironment(r.Context(), tenant, in.Environment, in.NewSlug, in.Actor, in.Reason)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *server) apiTokensList(w http.ResponseWriter, r *http.Request, tenant string) {
	includeInactive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_inactive")), "true")
	if includeInactive && !s.requireGlobalScopeForIncludeInactive(w, r, "token.read") {
		return
	}
	filter := parseListFilter(r.URL.Query())
	var (
		rows []store.APITokenRow
		err  error
	)
	if includeInactive {
		rows, err = s.control.ListAPITokensAll(r.Context(), tenant)
	} else {
		rows, err = s.control.ListAPITokens(r.Context(), tenant)
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	filtered := filterTokens(rows, filter)
	page, total := paginateAny(filtered, filter.Offset, filter.Limit)
	writeJSON(w, http.StatusOK, map[string]any{
		"tokens": page,
		"meta": map[string]any{
			"total":  total,
			"limit":  filter.Limit,
			"offset": filter.Offset,
		},
	})
}

func (s *server) apiTenantLifecycleSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug   string `json:"slug"`
		Status string `json:"status"`
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.requirePermission(w, r, "tenant.lifecycle.update", in.Slug, "") {
		return
	}
	status, err := store.ParseLifecycleStatus(in.Status)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.control.SetTenantLifecycleStatusAudit(r.Context(), in.Slug, status, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiTenantDelete(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug   string `json:"slug"`
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.requirePermission(w, r, "tenant.lifecycle.update", in.Slug, "") {
		return
	}
	if err := s.control.DeleteTenantAudit(r.Context(), in.Slug, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": true})
}

func (s *server) apiEnvironmentLifecycleSet(w http.ResponseWriter, r *http.Request, tenant string) {
	var in struct {
		Environment string `json:"environment"`
		Status      string `json:"status"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.requirePermission(w, r, "environment.lifecycle.update", tenant, in.Environment) {
		return
	}
	status, err := store.ParseLifecycleStatus(in.Status)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.control.SetEnvironmentLifecycleStatusAudit(r.Context(), tenant, in.Environment, status, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiEnvironmentDelete(w http.ResponseWriter, r *http.Request, tenant string) {
	var in struct {
		Environment string `json:"environment"`
		Actor       string `json:"actor"`
		Reason      string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.requirePermission(w, r, "environment.lifecycle.update", tenant, in.Environment) {
		return
	}
	if err := s.control.DeleteEnvironmentAudit(r.Context(), tenant, in.Environment, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "archived": true})
}

func (s *server) apiTokenLifecycleSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TokenID string `json:"token_id"`
		Status  string `json:"status"`
		Actor   string `json:"actor"`
		Reason  string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	row, err := s.control.GetAPITokenByID(r.Context(), in.TokenID)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.requirePermission(w, r, "token.lifecycle.update", row.TenantSlug, row.EnvSlug) {
		return
	}
	if strings.EqualFold(strings.TrimSpace(in.Status), string(store.LifecycleStatusArchived)) || strings.EqualFold(strings.TrimSpace(in.Status), "deleted") {
		if err := s.control.DeleteAPITokenAudit(r.Context(), in.TokenID, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
		return
	}
	status, err := store.ParseLifecycleStatus(in.Status)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if err := s.control.SetAPITokenLifecycleStatusAudit(r.Context(), in.TokenID, status, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiStateDelete(w http.ResponseWriter, r *http.Request, tenant, environment string) {
	var in struct {
		State  string `json:"state"`
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.State = strings.TrimSpace(in.State)
	if in.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state is required"})
		return
	}
	env, err := s.control.GetEnvironmentByTenantSlug(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	resolvedState, ok := resolveEnvironmentScopedStateName(env, in.State)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state does not belong to the selected environment"})
		return
	}
	stateCtx, iso, cleanup, err := s.openEnvironmentStateStore(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	defer cleanup()
	if err := iso.SetStateLifecycleStatusAudit(stateCtx, resolvedState, store.LifecycleStatusArchived, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSONForStateConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiStateDestroy(w http.ResponseWriter, r *http.Request, tenant, environment string) {
	var in struct {
		State  string `json:"state"`
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.State = strings.TrimSpace(in.State)
	if in.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state is required"})
		return
	}
	env, err := s.control.GetEnvironmentByTenantSlug(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	resolvedState, ok := resolveEnvironmentScopedStateName(env, in.State)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state does not belong to the selected environment"})
		return
	}
	stateCtx, iso, cleanup, err := s.openEnvironmentStateStore(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	defer cleanup()
	if err := iso.DeleteState(stateCtx, resolvedState, "", firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context()))); err != nil {
		writeJSONForStateConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
}

func (s *server) apiStateLifecycleSet(w http.ResponseWriter, r *http.Request, tenant, environment string) {
	var in struct {
		State  string `json:"state"`
		Status string `json:"status"`
		Actor  string `json:"actor"`
		Reason string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.State = strings.TrimSpace(in.State)
	if in.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state is required"})
		return
	}
	env, err := s.control.GetEnvironmentByTenantSlug(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	resolvedState, ok := resolveEnvironmentScopedStateName(env, in.State)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state does not belong to the selected environment"})
		return
	}
	status, err := store.ParseLifecycleStatus(in.Status)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	stateCtx, iso, cleanup, err := s.openEnvironmentStateStore(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	defer cleanup()
	if err := iso.SetStateLifecycleStatusAudit(stateCtx, resolvedState, status, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context())), in.Reason); err != nil {
		writeJSONForStateConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *server) apiTokenCreate(w http.ResponseWriter, r *http.Request, tenant string) {
	var in struct {
		Environment string `json:"environment"`
		Name        string `json:"name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.Environment = strings.TrimSpace(in.Environment)
	if !s.cfg.ResolvedAutoDefaultEnvironment() && in.Environment == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "environment is required when auto default environment is disabled"})
		return
	}
	row, secret, err := s.control.CreateAPIToken(r.Context(), tenant, in.Environment, in.Name)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"token":        row,
		"token_secret": secret,
	})
}

func (s *server) apiStatesList(w http.ResponseWriter, r *http.Request, tenant, environment string) {
	envRow, err := s.control.GetEnvironmentByTenantSlug(r.Context(), tenant, environment)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	stateCtx, iso, cleanup, err := s.openEnvironmentStateStore(r.Context(), tenant, environment)
	if err != nil {
		var status int
		switch {
		case errors.Is(err, errEnvironmentStateAccess):
			status = http.StatusConflict
		case errors.Is(err, errEnvironmentNoStateDSN):
			writeJSON(w, http.StatusOK, map[string]any{"states": []any{}, "warning": "environment has no database_dsn yet"})
			return
		default:
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	defer cleanup()
	includeInactive := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_inactive")), "true")
	var rows []store.StateInfo
	if includeInactive {
		rows, err = iso.ListStatesAll(stateCtx)
	} else {
		rows, err = iso.ListStates(stateCtx)
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "3D000" {
			writeJSON(w, http.StatusOK, map[string]any{
				"states":  []any{},
				"warning": "environment database is not provisioned yet",
			})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	rows = filterStatesForEnvironment(rows, envRow)
	writeJSON(w, http.StatusOK, map[string]any{"states": rows})
}

func (s *server) apiStateConfigSet(w http.ResponseWriter, r *http.Request, tenant, environment string) {
	var in struct {
		State           string `json:"state"`
		ExclusiveLocks  *bool  `json:"exclusive_locks"`
		CoexistenceMode string `json:"coexistence_mode"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	in.State = strings.TrimSpace(in.State)
	if in.State == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "state is required"})
		return
	}
	mode := store.StateCoexistenceMode(strings.TrimSpace(in.CoexistenceMode))
	if in.ExclusiveLocks == nil && mode == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "at least one of exclusive_locks or coexistence_mode is required"})
		return
	}
	if mode != "" && !mode.Valid() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": fmt.Sprintf("invalid coexistence_mode %q", mode)})
		return
	}

	stateCtx, iso, cleanup, err := s.openEnvironmentStateStore(r.Context(), tenant, environment)
	if err != nil {
		var status int
		switch {
		case errors.Is(err, errEnvironmentStateAccess):
			status = http.StatusConflict
		default:
			status = http.StatusBadRequest
		}
		writeJSON(w, status, map[string]any{"error": err.Error()})
		return
	}
	defer cleanup()

	if in.ExclusiveLocks != nil {
		if err := iso.SetStateExclusiveLocks(stateCtx, in.State, *in.ExclusiveLocks); err != nil {
			writeJSONForStateConfigError(w, err)
			return
		}
	}
	if mode != "" {
		if err := iso.SetStateCoexistenceMode(stateCtx, in.State, mode); err != nil {
			writeJSONForStateConfigError(w, err)
			return
		}
	}

	exclusiveLocks, err := iso.GetStateExclusiveLocks(stateCtx, in.State)
	if err != nil {
		writeJSONForStateConfigError(w, err)
		return
	}
	coexistenceMode, err := iso.GetStateCoexistenceMode(stateCtx, in.State)
	if err != nil {
		writeJSONForStateConfigError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state": in.State,
		"config": map[string]any{
			"exclusive_locks":  exclusiveLocks,
			"coexistence_mode": coexistenceMode,
		},
	})
}

func (s *server) apiOwnershipTransfersList(w http.ResponseWriter, r *http.Request) {
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	rows, err := s.control.ListOwnershipTransferProposals(r.Context(), tenant, status)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"transfers": rows})
}

func (s *server) apiOwnershipTransferCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		SourceTenant     string `json:"source_tenant"`
		EnvironmentSlug  string `json:"environment_slug"`
		TargetTenantSlug string `json:"target_tenant_slug"`
		Actor            string `json:"actor"`
		Reason           string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	proposal, err := s.control.CreateEnvironmentOwnershipTransferProposalByOperator(r.Context(), in.SourceTenant, in.EnvironmentSlug, in.TargetTenantSlug, in.Actor, in.Reason)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"proposal": proposal})
}

func (s *server) apiOwnershipTransferResolve(w http.ResponseWriter, r *http.Request, proposalID, action string) {
	var in struct {
		Actor         string `json:"actor"`
		TargetNewSlug string `json:"target_new_slug"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	var (
		proposal store.OwnershipTransferProposal
		err      error
	)
	switch action {
	case "accept":
		proposal, err = s.acceptOwnershipTransferAndMoveStates(r.Context(), proposalID, "", in.Actor, in.TargetNewSlug, true)
	case "reject":
		proposal, err = s.control.RejectOwnershipTransferProposalByOperator(r.Context(), proposalID, in.Actor)
	case "cancel":
		proposal, err = s.control.CancelOwnershipTransferProposalByOperator(r.Context(), proposalID, in.Actor)
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unsupported action"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal})
}

func (s *server) acceptOwnershipTransferAndMoveStates(ctx context.Context, proposalID, accountID, actor, targetNewSlug string, operator bool) (store.OwnershipTransferProposal, error) {
	proposal, err := s.control.GetOwnershipTransferProposal(ctx, proposalID)
	if err != nil {
		return store.OwnershipTransferProposal{}, err
	}
	if proposal.ResourceType != "environment" {
		return store.OwnershipTransferProposal{}, fmt.Errorf("unsupported resource type %q", proposal.ResourceType)
	}
	sourceEnv, err := s.control.GetEnvironmentByTenantSlug(ctx, proposal.CurrentOwnerRef, proposal.ResourceName)
	if err != nil {
		return store.OwnershipTransferProposal{}, err
	}
	targetTenant, err := s.control.GetTenantBySlug(ctx, proposal.TargetOwnerRef)
	if err != nil {
		return store.OwnershipTransferProposal{}, err
	}
	stateCtx, stateStore, cleanup, err := s.openStateStoreForEnvironmentRow(ctx, sourceEnv)
	if err != nil && !errors.Is(err, errEnvironmentNoStateDSN) {
		return store.OwnershipTransferProposal{}, err
	}
	if cleanup != nil {
		defer cleanup()
	}

	if operator {
		proposal, err = s.control.AcceptOwnershipTransferProposalByOperatorWithTarget(ctx, proposalID, actor, targetNewSlug)
	} else {
		proposal, err = s.control.AcceptOwnershipTransferProposalWithTarget(ctx, proposalID, accountID, actor, targetNewSlug)
	}
	if err != nil {
		return store.OwnershipTransferProposal{}, err
	}
	if stateStore != nil {
		if err := stateStore.MoveEnvironmentStateNamespace(stateCtx, sourceEnv.TenantID, targetTenant.ID, sourceEnv.WorkspaceID, targetTenant.WorkspaceID, sourceEnv.EnvPublicID); err != nil {
			return store.OwnershipTransferProposal{}, err
		}
	}
	return proposal, nil
}

var errEnvironmentStateAccess = errors.New("environment state access")
var errEnvironmentNoStateDSN = errors.New("environment has no database_dsn yet")

func (s *server) openEnvironmentStateStore(ctx context.Context, tenant, environment string) (context.Context, *store.Store, func(), error) {
	env, err := s.control.GetEnvironmentByTenantSlug(ctx, tenant, environment)
	if err != nil {
		return nil, nil, nil, err
	}
	return s.openStateStoreForEnvironmentRow(ctx, env)
}

func (s *server) openStateStoreForEnvironmentRow(ctx context.Context, env store.EnvironmentRow) (context.Context, *store.Store, func(), error) {
	if err := validateEnvironmentStateAccess(env); err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", errEnvironmentStateAccess, err)
	}
	dsn := strings.TrimSpace(env.DatabaseDSN)
	if dsn == "" {
		baseURL := strings.TrimSpace(s.cfg.ResolvedDataPlaneURLForInstance(strings.TrimSpace(env.DatabaseInstanceKey)))
		// Match backend routing behavior: when environment.database_dsn is empty,
		// requests go to the instance base data-plane DB (shared/unified mode).
		dsn = baseURL
	}
	if dsn == "" {
		return nil, nil, nil, errEnvironmentNoStateDSN
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		cancel()
		return nil, nil, nil, err
	}
	cleanup := func() {
		pool.Close()
		cancel()
	}
	if strings.TrimSpace(env.DatabaseDSN) == "" {
		stateCtx := auth.WithPrincipal(ctx, auth.Principal{
			TenantID:            env.TenantID,
			WorkspaceID:         env.WorkspaceID,
			TenantSlug:          env.TenantSlug,
			EnvironmentID:       env.ID,
			EnvironmentPublicID: env.EnvPublicID,
			EnvironmentSlug:     env.Slug,
			DatabaseInstanceKey: env.DatabaseInstanceKey,
			Source:              "control-api",
		})
		return stateCtx, store.New(pool), cleanup, nil
	}
	return ctx, store.NewIsolated(pool), cleanup, nil
}

func filterStatesForEnvironment(rows []store.StateInfo, envRow store.EnvironmentRow) []store.StateInfo {
	if strings.TrimSpace(envRow.DatabaseDSN) != "" {
		return rows
	}
	prefix := strings.TrimSpace(envRow.WorkspaceID)
	env := strings.TrimSpace(envRow.EnvPublicID)
	if prefix == "" || env == "" {
		return rows
	}
	prefix = prefix + "/" + env + "/"
	filtered := make([]store.StateInfo, 0, len(rows))
	for _, row := range rows {
		if strings.HasPrefix(strings.TrimSpace(row.Name), prefix) {
			filtered = append(filtered, row)
		}
	}
	return filtered
}

func resolveEnvironmentScopedStateName(env store.EnvironmentRow, name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", false
	}
	prefix := strings.TrimSpace(env.WorkspaceID) + "/" + strings.TrimSpace(env.EnvPublicID) + "/"
	if strings.TrimSpace(env.DatabaseDSN) != "" {
		if prefix != "//" && strings.HasPrefix(name, prefix) {
			return strings.TrimPrefix(name, prefix), true
		}
		return name, true
	}
	if prefix == "//" {
		return name, true
	}
	if strings.HasPrefix(name, prefix) {
		return name, true
	}
	return prefix + name, true
}

func writeJSONForStateConfigError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrStateNotFound):
		writeJSON(w, http.StatusNotFound, map[string]any{"error": err.Error()})
	case errors.As(err, new(*store.StateNotActiveError)):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
	}
}

func (s *server) apiTenantEntitlementsSet(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Slug                    string `json:"slug"`
		BillingPlan             string `json:"billing_plan"`
		MaxEnvironments         int    `json:"max_environments"`
		MaxStateResources       int    `json:"max_state_resources"`
		MaxEnvironmentResources int    `json:"max_environment_resources"`
		Actor                   string `json:"actor"`
		Reason                  string `json:"reason"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	actor := firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context()))
	if err := s.control.SetTenantEntitlements(r.Context(), in.Slug, in.BillingPlan, in.MaxEnvironments, in.MaxStateResources, in.MaxEnvironmentResources, actor, in.Reason); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	row, err := s.control.GetTenantBySlug(r.Context(), in.Slug)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *server) apiTenantGet(w http.ResponseWriter, r *http.Request, tenant string) {
	row, err := s.control.GetTenantBySelector(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *server) apiTenantMembersList(w http.ResponseWriter, r *http.Request, tenant string) {
	trow, err := s.control.GetTenantBySelector(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	rows, err := s.control.ListPortalUsersByTenant(r.Context(), trow.Slug)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"workspace": trow,
		"members":   rows,
	})
}

func (s *server) apiEnvironmentPATAccessList(w http.ResponseWriter, r *http.Request) {
	ref := strings.TrimSpace(r.URL.Query().Get("env_id"))
	if ref == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "env_id is required"})
		return
	}
	env, err := s.control.GetEnvironmentBySelector(r.Context(), ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	if !s.requirePermission(w, r, "environment.read", env.TenantSlug, env.Slug) {
		return
	}
	rows, err := s.control.ListPortalEnvironmentPATAccess(r.Context(), ref)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"environment": env,
		"access":      rows,
	})
}

func (s *server) provisionEnvironment(ctx context.Context, tenant store.TenantRow, env store.EnvironmentRow) error {
	instanceKey := strings.TrimSpace(env.DatabaseInstanceKey)
	adminURL := strings.TrimSpace(s.cfg.ResolvedDataPlaneAdminURLForInstance(instanceKey))
	baseURL := strings.TrimSpace(s.cfg.ResolvedDataPlaneURLForInstance(instanceKey))
	if adminURL == "" || baseURL == "" {
		return fmt.Errorf("missing data-plane DSN/admin DSN for instance %q", instanceKey)
	}
	dsn, err := provision.SharedHostEnvironment(ctx, provision.SharedHostConfig{
		AdminDSN: adminURL,
		BaseDSN:  baseURL,
		Logger:   s.logger,
	}, tenant, env)
	if err != nil {
		return err
	}
	return s.control.SetEnvironmentDSN(ctx, env.ID, dsn)
}

func decodeJSON(w http.ResponseWriter, r *http.Request, out any) bool {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	if err := json.Unmarshal(b, out); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return false
	}
	return true
}

func firstNonEmptyControlActor(values ...string) string {
	for _, v := range values {
		v = strings.TrimSpace(v)
		if v != "" {
			return v
		}
	}
	return ""
}

func controlActorFromContext(ctx context.Context) string {
	p, _ := ctx.Value(controlPrincipalKey{}).(auth.Principal)
	if strings.TrimSpace(p.Email) != "" {
		return strings.TrimSpace(p.Email)
	}
	if strings.TrimSpace(p.TenantSlug) != "" && strings.TrimSpace(p.EnvironmentSlug) != "" {
		return strings.TrimSpace(p.TenantSlug) + "/" + strings.TrimSpace(p.EnvironmentSlug)
	}
	return ""
}

func validateRetentionPurgeApply(apply bool, reason string) error {
	if !apply {
		return nil
	}
	if strings.TrimSpace(reason) == "" {
		return fmt.Errorf("reason is required when apply=true for retention purge")
	}
	return nil
}

func validateEnvironmentStateAccess(env store.EnvironmentRow) error {
	if env.LifecycleStatus != store.LifecycleStatusActive {
		return fmt.Errorf("environment %s/%s is %s", env.TenantSlug, env.Slug, env.LifecycleStatus)
	}
	return nil
}

func (s *server) requireGlobalScopeForIncludeInactive(w http.ResponseWriter, r *http.Request, permission string) bool {
	p, ok := r.Context().Value(controlPrincipalKey{}).(auth.Principal)
	if !ok {
		// Static control token path (or no scoped principal in context) keeps
		// legacy operator behavior.
		return true
	}
	allowed, err := s.control.AuthorizeControlPrincipalScope(r.Context(), p, permission, "*", "")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return false
	}
	if !allowed {
		writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden: include_inactive requires global scope"})
		return false
	}
	return true
}

type listFilter struct {
	Query     string
	Lifecycle string
	Tier      string
	Limit     int
	Offset    int
}

func parseListFilter(q url.Values) listFilter {
	limit := parsePositiveInt(q.Get("limit"), 200)
	if limit > 1000 {
		limit = 1000
	}
	return listFilter{
		Query:     strings.ToLower(strings.TrimSpace(q.Get("q"))),
		Lifecycle: strings.ToLower(strings.TrimSpace(q.Get("lifecycle"))),
		Tier:      strings.ToLower(strings.TrimSpace(q.Get("tier"))),
		Limit:     limit,
		Offset:    parsePositiveInt(q.Get("offset"), 0),
	}
}

func parsePositiveInt(raw string, def int) int {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return def
	}
	return n
}

func containsAnyFold(q string, parts ...string) bool {
	if q == "" {
		return true
	}
	for _, p := range parts {
		if strings.Contains(strings.ToLower(p), q) {
			return true
		}
	}
	return false
}

func filterTenants(in []store.TenantRow, f listFilter) []store.TenantRow {
	out := make([]store.TenantRow, 0, len(in))
	for _, r := range in {
		if f.Lifecycle != "" && strings.ToLower(string(r.LifecycleStatus)) != f.Lifecycle {
			continue
		}
		if !containsAnyFold(f.Query, r.Slug, r.Name, r.ID) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func filterEnvironments(in []store.EnvironmentRow, f listFilter) []store.EnvironmentRow {
	out := make([]store.EnvironmentRow, 0, len(in))
	for _, r := range in {
		if f.Lifecycle != "" && strings.ToLower(string(r.LifecycleStatus)) != f.Lifecycle {
			continue
		}
		if f.Tier != "" && strings.ToLower(string(r.Tier)) != f.Tier {
			continue
		}
		if !containsAnyFold(f.Query, r.Slug, r.TenantSlug, r.ID, r.DatabaseName) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func filterTokens(in []store.APITokenRow, f listFilter) []store.APITokenRow {
	out := make([]store.APITokenRow, 0, len(in))
	for _, r := range in {
		if f.Lifecycle != "" && strings.ToLower(string(r.LifecycleStatus)) != f.Lifecycle {
			continue
		}
		if !containsAnyFold(f.Query, r.Name, r.TenantSlug, r.EnvSlug, r.ID, r.TokenPrefix) {
			continue
		}
		out = append(out, r)
	}
	return out
}

func paginateAny[T any](in []T, offset, limit int) ([]T, int) {
	total := len(in)
	if offset >= total {
		return []T{}, total
	}
	end := total
	if limit > 0 && offset+limit < end {
		end = offset + limit
	}
	return in[offset:end], total
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *server) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, controlUI)
}

func (s *server) handlePortalUI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	if r.URL.Path != "/portal" && r.URL.Path != "/portal/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = io.WriteString(w, portalUI)
}

func portalCookie(r *http.Request) string {
	c, err := r.Cookie("kl_portal_session")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(c.Value)
}

func setPortalSessionCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     "kl_portal_session",
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	})
}

func clearPortalSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: "kl_portal_session", Value: "", Path: "/", MaxAge: -1, HttpOnly: true, SameSite: http.SameSiteLaxMode})
}

func redirectPortalAuthError(w http.ResponseWriter, r *http.Request, msg string) {
	target := "/portal?auth_error=" + url.QueryEscape(strings.TrimSpace(msg))
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *server) requirePortalUser(w http.ResponseWriter, r *http.Request) (store.PortalUser, bool) {
	token := portalCookie(r)
	if token == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return store.PortalUser{}, false
	}
	u, err := s.control.GetPortalSessionUser(r.Context(), token)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthenticated"})
		return store.PortalUser{}, false
	}
	return u, true
}

func (s *server) handlePortalAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/portal/api")
	switch {
	case r.Method == http.MethodGet && path == "/auth/config":
		writeJSON(w, http.StatusOK, portalauth.FromRuntimeConfig(s.cfg))
	case r.Method == http.MethodPost && path == "/signup":
		if !s.cfg.ResolvedPortalPasswordAuthEnabled() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "password signup is disabled"})
			return
		}
		var in struct {
			Email    string `json:"email"`
			Company  string `json:"company"`
			Plan     string `json:"plan"`
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		user, err := s.control.CreatePortalAccount(r.Context(), in.Email, in.Company, in.Plan, in.Password)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		token, err := s.control.CreatePortalSession(r.Context(), user.ID, 24*time.Hour)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		setPortalSessionCookie(w, token)
		writeJSON(w, http.StatusOK, map[string]any{"user": user})
	case r.Method == http.MethodPost && path == "/login":
		if !s.cfg.ResolvedPortalPasswordAuthEnabled() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "password login is disabled"})
			return
		}
		var in struct {
			Email    string `json:"email"`
			Password string `json:"password"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		user, err := s.control.AuthenticatePortalUser(r.Context(), in.Email, in.Password)
		if err != nil {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "invalid credentials"})
			return
		}
		token, err := s.control.CreatePortalSession(r.Context(), user.ID, 24*time.Hour)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		setPortalSessionCookie(w, token)
		writeJSON(w, http.StatusOK, map[string]any{"user": user})
	case r.Method == http.MethodGet && path == "/oidc/login":
		if !s.cfg.ResolvedPortalOIDCEnabled() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "oidc login is disabled"})
			return
		}
		if s.cfg.ResolvedPortalOIDCGoogleEnabled() {
			redirectURL, err := portalauth.BeginGoogleOIDC(w, r, s.cfg, s.httpClient)
			if err != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
				return
			}
			http.Redirect(w, r, redirectURL, http.StatusFound)
			return
		}
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "oidc login requires POST in trusted-header mode"})
	case r.Method == http.MethodPost && path == "/oidc/login":
		if !s.cfg.ResolvedPortalOIDCEnabled() {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "oidc login is disabled"})
			return
		}
		if !s.cfg.ResolvedPortalOIDCTrustedHeaderEnabled() {
			writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "trusted-header oidc login is disabled"})
			return
		}
		email := portalauth.TrustedEmail(r, s.cfg.ResolvedPortalTrustedEmailHeaders())
		if email == "" {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "missing trusted identity header"})
			return
		}
		user, err := s.control.FindOrCreatePortalOIDCAccount(r.Context(), email, "", "starter", s.cfg.ResolvedPortalOIDCProviderLabel())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		token, err := s.control.CreatePortalSession(r.Context(), user.ID, 24*time.Hour)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		setPortalSessionCookie(w, token)
		writeJSON(w, http.StatusOK, map[string]any{"user": user})
	case r.Method == http.MethodGet && path == "/oidc/callback":
		if !s.cfg.ResolvedPortalOIDCGoogleEnabled() {
			redirectPortalAuthError(w, r, "Direct Google login is disabled")
			return
		}
		email, err := portalauth.CompleteGoogleOIDC(r.Context(), r, s.cfg, s.httpClient)
		portalauth.ClearOIDCCookies(w)
		if err != nil {
			redirectPortalAuthError(w, r, err.Error())
			return
		}
		user, err := s.control.FindOrCreatePortalOIDCAccount(r.Context(), email, "", "starter", s.cfg.ResolvedPortalOIDCProviderLabel())
		if err != nil {
			redirectPortalAuthError(w, r, err.Error())
			return
		}
		token, err := s.control.CreatePortalSession(r.Context(), user.ID, 24*time.Hour)
		if err != nil {
			redirectPortalAuthError(w, r, err.Error())
			return
		}
		setPortalSessionCookie(w, token)
		http.Redirect(w, r, "/portal", http.StatusFound)
	case r.Method == http.MethodPost && path == "/logout":
		if tok := portalCookie(r); tok != "" {
			_ = s.control.DeletePortalSession(r.Context(), tok)
		}
		clearPortalSessionCookie(w)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && path == "/me":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		invites, err := s.control.ListPortalInvitationsByEmail(r.Context(), u.Email)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		transfers, err := s.control.ListOwnershipTransferProposalsByAccount(r.Context(), u.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusOK, map[string]any{"user": u, "memberships": u.Memberships, "invitations": invites, "transfers": transfers, "environments": []any{}, "tokens": []any{}})
			return
		}
		tenant, err := s.control.GetTenantBySlug(r.Context(), u.TenantSlug)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		envs, err := s.control.ListEnvironmentsAll(r.Context(), u.TenantSlug)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		toks, err := s.control.ListAPITokensAll(r.Context(), u.TenantSlug)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"user": u, "tenant": tenant, "memberships": u.Memberships, "invitations": invites, "transfers": transfers, "environments": envs, "tokens": toks})
	case r.Method == http.MethodGet && path == "/ownership-transfers":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		transfers, err := s.control.ListOwnershipTransferProposalsByAccount(r.Context(), u.ID)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"transfers": transfers})
	case r.Method == http.MethodPost && path == "/ownership-transfers":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		if strings.ToLower(strings.TrimSpace(u.Role)) != "owner" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner role required"})
			return
		}
		var in struct {
			EnvironmentSlug  string `json:"environment_slug"`
			TargetTenantSlug string `json:"target_tenant_slug"`
			Reason           string `json:"reason"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		proposal, err := s.control.CreateEnvironmentOwnershipTransferProposal(r.Context(), u.TenantSlug, in.EnvironmentSlug, in.TargetTenantSlug, u.ID, u.Email, in.Reason)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"proposal": proposal})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/accept"):
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.ToLower(strings.TrimSpace(u.Role)) != "owner" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner role required"})
			return
		}
		var in struct {
			TargetNewSlug string `json:"target_new_slug"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		proposalID := strings.TrimSuffix(strings.TrimPrefix(path, "/ownership-transfers/"), "/accept")
		proposal, err := s.acceptOwnershipTransferAndMoveStates(r.Context(), proposalID, u.ID, u.Email, in.TargetNewSlug, false)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		_ = s.control.SetPortalSessionActiveTenant(r.Context(), portalCookie(r), proposal.TargetOwnerRef)
		writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/reject"):
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.ToLower(strings.TrimSpace(u.Role)) != "owner" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner role required"})
			return
		}
		proposalID := strings.TrimSuffix(strings.TrimPrefix(path, "/ownership-transfers/"), "/reject")
		proposal, err := s.control.RejectOwnershipTransferProposal(r.Context(), proposalID, u.ID, u.Email)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/ownership-transfers/") && strings.HasSuffix(path, "/cancel"):
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.ToLower(strings.TrimSpace(u.Role)) != "owner" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner role required"})
			return
		}
		proposalID := strings.TrimSuffix(strings.TrimPrefix(path, "/ownership-transfers/"), "/cancel")
		proposal, err := s.control.CancelOwnershipTransferProposal(r.Context(), proposalID, u.ID, u.Email)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"proposal": proposal})
	case r.Method == http.MethodPost && path == "/active-tenant":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		_ = u
		var in struct {
			TenantSlug string `json:"tenant_slug"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if err := s.control.SetPortalSessionActiveTenant(r.Context(), portalCookie(r), in.TenantSlug); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		updated, err := s.control.GetPortalSessionUser(r.Context(), portalCookie(r))
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"user": updated})
	case r.Method == http.MethodPost && path == "/tenants":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		var in struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		slug := strings.TrimSpace(in.Slug)
		name := strings.TrimSpace(in.Name)
		if name == "" {
			name = slug
		}
		tenant, membership, err := s.control.CreateTenantForPortalAccount(r.Context(), u.ID, slug, name, u.Email, s.cfg.ResolvedAutoDefaultEnvironment())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		_ = s.control.SetPortalSessionActiveTenant(r.Context(), portalCookie(r), tenant.Slug)
		writeJSON(w, http.StatusCreated, map[string]any{"tenant": tenant, "membership": membership})
	case r.Method == http.MethodPost && path == "/personal-workspace":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		tenant, membership, err := s.control.CreatePersonalWorkspaceForPortalAccount(r.Context(), u.ID, u.Email, u.Email, s.cfg.ResolvedAutoDefaultEnvironment())
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		_ = s.control.SetPortalSessionActiveTenant(r.Context(), portalCookie(r), tenant.Slug)
		writeJSON(w, http.StatusCreated, map[string]any{"tenant": tenant, "membership": membership})
	case r.Method == http.MethodGet && path == "/invites":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		invites, err := s.control.ListPortalInvitationsByEmail(r.Context(), u.Email)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"invites": invites})
	case r.Method == http.MethodPost && path == "/invites":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		role := strings.ToLower(strings.TrimSpace(u.Role))
		if role != "owner" && role != "tenant_admin" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "tenant admin required"})
			return
		}
		var in struct {
			Email string `json:"email"`
			Role  string `json:"role"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		invite, err := s.control.CreateTenantInvitation(r.Context(), u.TenantSlug, in.Email, in.Role, u.Email)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"invite": invite})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/invites/") && strings.HasSuffix(path, "/accept"):
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		inviteID := strings.TrimSuffix(strings.TrimPrefix(path, "/invites/"), "/accept")
		membership, err := s.control.AcceptTenantInvitation(r.Context(), inviteID, u.ID, u.Email)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		_ = s.control.SetPortalSessionActiveTenant(r.Context(), portalCookie(r), membership.TenantSlug)
		writeJSON(w, http.StatusOK, map[string]any{"membership": membership})
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/invites/") && strings.HasSuffix(path, "/reject"):
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		inviteID := strings.TrimSuffix(strings.TrimPrefix(path, "/invites/"), "/reject")
		if err := s.control.RejectTenantInvitation(r.Context(), inviteID, u.Email); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodGet && path == "/states":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		envSlug := strings.TrimSpace(r.URL.Query().Get("environment"))
		if envSlug == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "environment is required"})
			return
		}
		s.apiStatesList(w, r, u.TenantSlug, envSlug)
	case r.Method == http.MethodPost && path == "/states/delete":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		role := strings.ToLower(strings.TrimSpace(u.Role))
		if role != "owner" && role != "tenant_admin" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner or tenant admin required"})
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		var in struct {
			Environment string `json:"environment"`
			State       string `json:"state"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		reqBody, _ := json.Marshal(map[string]any{"state": in.State, "actor": u.Email, "reason": "portal action"})
		req, _ := http.NewRequest(http.MethodPost, "/api/states/"+u.TenantSlug+"/"+in.Environment+"/delete", bytes.NewReader(reqBody))
		req.Header = r.Header.Clone()
		req = req.WithContext(r.Context())
		s.apiStateDelete(w, req, u.TenantSlug, in.Environment)
	case r.Method == http.MethodPost && path == "/environments":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		var in struct {
			Slug string `json:"slug"`
			Tier string `json:"tier"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		tier := store.EnvironmentTier(strings.TrimSpace(in.Tier))
		if tier == "" {
			tier = store.EnvironmentTierSharedHost
		}
		row, err := s.control.CreateEnvironment(r.Context(), u.TenantSlug, in.Slug, tier, "")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, row)
	case r.Method == http.MethodPost && path == "/environments/rename":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		if strings.ToLower(strings.TrimSpace(u.Role)) != "owner" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "owner role required"})
			return
		}
		var in struct {
			Environment string `json:"environment"`
			NewSlug     string `json:"new_slug"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		row, err := s.control.RenameEnvironment(r.Context(), u.TenantSlug, in.Environment, in.NewSlug, u.Email, "portal rename")
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, row)
	case r.Method == http.MethodPost && path == "/environments/lifecycle":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		var in struct {
			Environment string `json:"environment"`
			Status      string `json:"status"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		st, err := store.ParseLifecycleStatus(in.Status)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.control.SetEnvironmentLifecycleStatusAudit(r.Context(), u.TenantSlug, in.Environment, st, u.Email, "portal action"); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodPost && path == "/tokens":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		var in struct {
			Environment string `json:"environment"`
			Name        string `json:"name"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		in.Environment = strings.TrimSpace(in.Environment)
		if !s.cfg.ResolvedAutoDefaultEnvironment() && in.Environment == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "environment is required when auto default environment is disabled"})
			return
		}
		row, secret, err := s.control.CreateAPIToken(r.Context(), u.TenantSlug, in.Environment, in.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{"token": row, "token_secret": secret})
	case r.Method == http.MethodPost && path == "/tokens/lifecycle":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		if strings.TrimSpace(u.TenantSlug) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "no active tenant selected"})
			return
		}
		var in struct {
			TokenID string `json:"token_id"`
			Status  string `json:"status"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		tok, err := s.control.GetAPITokenByID(r.Context(), in.TokenID)
		if err != nil || tok.TenantSlug != u.TenantSlug {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "forbidden"})
			return
		}
		if strings.EqualFold(strings.TrimSpace(in.Status), string(store.LifecycleStatusArchived)) || strings.EqualFold(strings.TrimSpace(in.Status), "deleted") {
			if err := s.control.DeleteAPITokenAudit(r.Context(), in.TokenID, u.Email, "portal action"); err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted": true})
			return
		}
		st, err := store.ParseLifecycleStatus(in.Status)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		if err := s.control.SetAPITokenLifecycleStatusAudit(r.Context(), in.TokenID, st, u.Email, "portal action"); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case r.Method == http.MethodPost && path == "/billing/checkout":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		role := strings.ToLower(strings.TrimSpace(u.Role))
		if role != "owner" && role != "billing_admin" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "billing admin required"})
			return
		}
		var in struct {
			BillingPlan string `json:"billing_plan"`
		}
		if !decodeJSON(w, r, &in) {
			return
		}
		if strings.EqualFold(strings.TrimSpace(in.BillingPlan), "enterprise") {
			out, err := s.billingCheckoutSessionPayload(r.Context(), u.TenantSlug, "enterprise", u.Email, u.Company, u.Email)
			if err != nil {
				writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, http.StatusOK, out)
			return
		}
		tenant, err := s.control.GetTenantBySlug(r.Context(), u.TenantSlug)
		if err == nil && strings.TrimSpace(tenant.StripeCustomerID) != "" {
			out, err := s.billingPortalSessionPayload(r.Context(), u.TenantSlug)
			if err == nil {
				writeJSON(w, http.StatusOK, out)
				return
			}
		}
		out, err := s.billingCheckoutSessionPayload(r.Context(), u.TenantSlug, strings.TrimSpace(in.BillingPlan), u.Email, u.Company, u.Email)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
	case r.Method == http.MethodGet && path == "/billing/plans":
		u, ok := s.requirePortalUser(w, r)
		if !ok {
			return
		}
		role := strings.ToLower(strings.TrimSpace(u.Role))
		if role != "owner" && role != "billing_admin" {
			writeJSON(w, http.StatusForbidden, map[string]any{"error": "billing admin required"})
			return
		}
		writeJSON(w, http.StatusOK, s.billingPlansPayload())
	default:
		writeJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
	}
}
