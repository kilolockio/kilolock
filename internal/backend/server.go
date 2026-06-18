// Package backend implements the Terraform HTTP backend protocol over
// the Kilolock data store. See docs/protocol.md for the wire contract.
//
// The handler is intentionally minimal: there is no authentication, no
// rate limiting, and no per-state ACL. v0 is meant to run on a trusted
// network or behind a reverse proxy that supplies those concerns.
package backend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kilolockio/kilolock/internal/refresh"
	"github.com/kilolockio/kilolock/internal/routing"
	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/store"
)

func requestLogAttrs(ctx context.Context, state string) []any {
	p, _ := auth.FromContext(ctx)
	instance := p.DatabaseInstanceKey
	if strings.TrimSpace(instance) == "" {
		instance = "shared"
	}
	return []any{
		"state", state,
		"tenant_id", p.TenantID,
		"workspace_id", p.WorkspaceID,
		"tenant_slug", p.TenantSlug,
		"environment_id", p.EnvironmentID,
		"environment_public_id", p.EnvironmentPublicID,
		"environment_slug", p.EnvironmentSlug,
		"database_instance_key", instance,
	}
}

const (
	// maxStateBodyBytes caps the size of an incoming state payload.
	// Set deliberately high to accommodate large states; refuse anything
	// pathological.
	maxStateBodyBytes = 512 << 20 // 512 MiB

	// maxLockBodyBytes caps the LockInfo body size.
	maxLockBodyBytes = 64 << 10 // 64 KiB

	// sourceHTTPBackend tags state writes coming in through the HTTP
	// protocol in the audit trail.
	sourceHTTPBackend = "http_backend"
)

// StoreResolver returns the data-plane store for a request context.
type StoreResolver func(context.Context) (*store.Store, error)

// AvailabilityCheck reports whether state/admin routes should currently serve
// requests. It is intended for lightweight readiness gates such as "system init
// completed" without affecting unauthenticated health checks.
type AvailabilityCheck func(context.Context) (bool, error)

// Server is the HTTP handler implementing the Terraform HTTP backend
// protocol. Construct one and pass it to http.Server.
type Server struct {
	resolveStore      StoreResolver
	logger            *slog.Logger
	authenticator     auth.Authenticator
	routingStats      func() map[string]any
	availabilityCheck AvailabilityCheck
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusCapturingResponseWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCapturingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// New returns a Server backed by the given store and logger, using
// the self-hosted SingleTenantAuthenticator by default. Hosted-mode
// callers swap in a real Authenticator via WithAuthenticator before
// calling Handler.
func New(s *store.Store, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		resolveStore: func(_ context.Context) (*store.Store, error) {
			return s, nil
		},
		logger:        logger,
		authenticator: auth.SingleTenantAuthenticator{},
	}
}

// WithStoreResolver returns a copy that routes each request to a store via fn.
func (s *Server) WithStoreResolver(fn StoreResolver) *Server {
	if fn == nil {
		return s
	}
	c := *s
	c.resolveStore = fn
	return &c
}

// WithAuthenticator returns a copy of s configured to use the given
// Authenticator. Provided as a constructor option rather than a
// public field so the zero-value default ("everything is the
// self-hosted singleton") can never be silently retained by a
// hosted-mode caller that forgot to set this.
func (s *Server) WithAuthenticator(a auth.Authenticator) *Server {
	if a == nil {
		a = auth.SingleTenantAuthenticator{}
	}
	c := *s
	c.authenticator = a
	return &c
}

// WithRoutingStatsProvider returns a copy configured to expose runtime routing
// stats on the admin endpoint.
func (s *Server) WithRoutingStatsProvider(fn func() map[string]any) *Server {
	c := *s
	c.routingStats = fn
	return &c
}

// WithAvailabilityCheck returns a copy configured to gate state/admin routes
// until fn reports that the service is ready to serve authenticated traffic.
func (s *Server) WithAvailabilityCheck(fn AvailabilityCheck) *Server {
	c := *s
	c.availabilityCheck = fn
	return &c
}

// Handler returns an http.Handler with all Kilolock routes registered.
// The auth middleware wraps every state-scoped route so handlers can
// rely on auth.MustFromContext(r.Context()) returning a real
// Principal.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Health endpoint for orchestrators and dev convenience.
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// The backend protocol mounts every method at the same path,
	// behind the auth middleware. Health does NOT need auth — it's
	// the load-balancer probe.
	// Use a multi-segment wildcard so callers can use hierarchical
	// state identifiers like tenant/env/project without URL-encoding
	// path separators.
	mux.Handle("/states/{name...}", s.withAuth(http.HandlerFunc(s.handleState)))
	mux.Handle("/state-unlock/{name...}", s.withAuth(http.HandlerFunc(s.handleStateUnlockPost)))
	mux.Handle("/admin/routing/stats", s.withAuth(http.HandlerFunc(s.handleRoutingStats)))
	mux.Handle("/admin/query", s.withAuth(http.HandlerFunc(s.handleAdminQuery)))
	mux.Handle("/admin/provider-configs", s.withAuth(http.HandlerFunc(s.handleAdminProviderConfigs)))
	mux.Handle("/admin/provider-config", s.withAuth(http.HandlerFunc(s.handleAdminProviderConfig)))
	mux.Handle("/admin/provider-config/set", s.withAuth(http.HandlerFunc(s.handleAdminProviderConfigSet)))
	mux.Handle("/admin/provider-config/delete", s.withAuth(http.HandlerFunc(s.handleAdminProviderConfigDelete)))
	mux.Handle("/admin/states", s.withAuth(http.HandlerFunc(s.handleAdminStates)))
	mux.Handle("/admin/state/refresh", s.withAuth(http.HandlerFunc(s.handleAdminStateRefresh)))
	mux.Handle("/admin/states/{name}/history", s.withAuth(http.HandlerFunc(s.handleAdminStateHistory)))
	mux.Handle("/admin/states/{name}/status", s.withAuth(http.HandlerFunc(s.handleAdminStateStatus)))
	mux.Handle("/admin/states/{name}/diff", s.withAuth(http.HandlerFunc(s.handleAdminStateDiff)))
	mux.Handle("/admin/state/status", s.withAuth(http.HandlerFunc(s.handleAdminStateStatusQuery)))
	mux.Handle("/admin/state/current", s.withAuth(http.HandlerFunc(s.handleAdminStateCurrent)))
	mux.Handle("/admin/state/raw", s.withAuth(http.HandlerFunc(s.handleAdminStateRawAtSerial)))
	mux.Handle("/admin/state/version-id", s.withAuth(http.HandlerFunc(s.handleAdminStateVersionID)))
	mux.Handle("/admin/state/import", s.withAuth(http.HandlerFunc(s.handleAdminStateImport)))
	mux.Handle("/admin/state/tags", s.withAuth(http.HandlerFunc(s.handleAdminStateTags)))
	mux.Handle("/admin/state/tags/set", s.withAuth(http.HandlerFunc(s.handleAdminStateTagSet)))
	mux.Handle("/admin/state/tags/unset", s.withAuth(http.HandlerFunc(s.handleAdminStateTagUnset)))
	mux.Handle("/admin/state/lock-mode", s.withAuth(http.HandlerFunc(s.handleAdminStateLockMode)))
	mux.Handle("/admin/state/coexistence-mode", s.withAuth(http.HandlerFunc(s.handleAdminStateCoexistenceMode)))
	mux.Handle("/admin/state/resources", s.withAuth(http.HandlerFunc(s.handleAdminStateResources)))
	mux.Handle("/admin/state/resource", s.withAuth(http.HandlerFunc(s.handleAdminStateResource)))
	mux.Handle("/admin/state/resource-history", s.withAuth(http.HandlerFunc(s.handleAdminStateResourceHistory)))
	mux.Handle("/admin/state/rollback/preview", s.withAuth(http.HandlerFunc(s.handleAdminStateRollbackPreview)))
	mux.Handle("/admin/state/rollback/apply", s.withAuth(http.HandlerFunc(s.handleAdminStateRollbackApply)))
	mux.Handle("/admin/state/resource-rollback/preview", s.withAuth(http.HandlerFunc(s.handleAdminStateResourceRollbackPreview)))
	mux.Handle("/admin/state/resource-rollback/apply", s.withAuth(http.HandlerFunc(s.handleAdminStateResourceRollbackApply)))
	mux.Handle("/admin/quota/remaining", s.withAuth(http.HandlerFunc(s.handleAdminQuotaRemaining)))
	mux.Handle("/admin/quota/check", s.withAuth(http.HandlerFunc(s.handleAdminQuotaCheck)))
	mux.Handle("/admin/state/write-apply", s.withAuth(http.HandlerFunc(s.handleAdminStateWriteApply)))
	mux.Handle("/admin/apply-runs/begin", s.withAuth(http.HandlerFunc(s.handleAdminApplyRunBegin)))
	mux.Handle("/admin/apply-runs/{id}/status", s.withAuth(http.HandlerFunc(s.handleAdminApplyRunStatus)))
	mux.Handle("/admin/apply-runs/{id}/finish", s.withAuth(http.HandlerFunc(s.handleAdminApplyRunFinish)))
	mux.Handle("/admin/apply-runs/{id}/abort", s.withAuth(http.HandlerFunc(s.handleAdminApplyRunAbort)))
	mux.Handle("/admin/reservations/acquire", s.withAuth(http.HandlerFunc(s.handleAdminReservationsAcquire)))
	mux.Handle("/admin/reservations/{apply_id}/renew", s.withAuth(http.HandlerFunc(s.handleAdminReservationsRenew)))
	mux.Handle("/admin/reservations/{apply_id}/release", s.withAuth(http.HandlerFunc(s.handleAdminReservationsRelease)))

	return mux
}

func (s *Server) handleAdminStates(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	rows, err := st.ListStates(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"states": rows,
	})
}

func (s *Server) handleAdminQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		SQL       string `json:"sql"`
		TimeoutMS int64  `json:"timeout_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.SQL = strings.TrimSpace(in.SQL)
	if in.SQL == "" {
		writeJSONError(w, http.StatusBadRequest, "sql is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	var (
		cols []store.ColumnInfo
		rows [][]string
	)
	timeout := time.Duration(in.TimeoutMS) * time.Millisecond
	err = st.Query(r.Context(), in.SQL, timeout, func(got []store.ColumnInfo) error {
		cols = append([]store.ColumnInfo(nil), got...)
		return nil
	}, func(values []any) error {
		row := make([]string, len(values))
		for i, v := range values {
			row[i] = formatAdminQueryValue(v, cols[i].TypeOID)
		}
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "rows": rows})
}

func (s *Server) handleAdminProviderConfigs(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	rows, err := st.ListProviderConfigs(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"entries": rows})
}

func (s *Server) handleAdminProviderConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	source := strings.TrimSpace(r.URL.Query().Get("source"))
	alias := strings.TrimSpace(r.URL.Query().Get("alias"))
	if source == "" {
		writeJSONError(w, http.StatusBadRequest, "source is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	entry, err := st.GetProviderConfig(r.Context(), source, alias)
	if errors.Is(err, store.ErrConfigNotFound) {
		writeJSONError(w, http.StatusNotFound, "provider config not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleAdminProviderConfigSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		Source string         `json:"source"`
		Alias  string         `json:"alias"`
		Config map[string]any `json:"config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.Source = strings.TrimSpace(in.Source)
	in.Alias = strings.TrimSpace(in.Alias)
	if in.Source == "" {
		writeJSONError(w, http.StatusBadRequest, "source is required")
		return
	}
	if in.Config == nil {
		writeJSONError(w, http.StatusBadRequest, "config is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.PutProviderConfig(r.Context(), in.Source, in.Alias, in.Config); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	entry, err := st.GetProviderConfig(r.Context(), in.Source, in.Alias)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, entry)
}

func (s *Server) handleAdminProviderConfigDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		Source string `json:"source"`
		Alias  string `json:"alias"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.Source = strings.TrimSpace(in.Source)
	in.Alias = strings.TrimSpace(in.Alias)
	if in.Source == "" {
		writeJSONError(w, http.StatusBadRequest, "source is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	deleted, err := st.DeleteProviderConfig(r.Context(), in.Source, in.Alias)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": deleted})
}

func (s *Server) handleAdminStateRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		StateName   string   `json:"state_name"`
		Concurrency int      `json:"concurrency"`
		FailFast    bool     `json:"fail_fast"`
		DryRun      bool     `json:"dry_run"`
		Actor       string   `json:"actor"`
		SearchPaths []string `json:"search_paths"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	name := strings.TrimSpace(firstNonEmptyString(in.StateName, adminStateName(r)))
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	factory, err := refresh.NewProductionFactory(refresh.ProductionFactoryOptions{
		Store:       st,
		SearchPaths: in.SearchPaths,
		Logger:      s.logger,
	})
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, runErr := refresh.Run(r.Context(), st, factory, refresh.Options{
		StateName:   name,
		Concurrency: in.Concurrency,
		FailFast:    in.FailFast,
		DryRun:      in.DryRun,
		Actor:       in.Actor,
	})
	if runErr != nil && res == nil {
		writeJSONError(w, http.StatusBadRequest, runErr.Error())
		return
	}
	type refreshError struct {
		Address string `json:"address"`
		Error   string `json:"error"`
	}
	out := map[string]any{"run_error": ""}
	if runErr != nil {
		out["run_error"] = runErr.Error()
	}
	if res != nil {
		errs := make([]refreshError, 0, len(res.Errors))
		for _, item := range res.Errors {
			errs = append(errs, refreshError{Address: item.Address, Error: item.Err.Error()})
		}
		out["result"] = map[string]any{
			"run_id":            res.RunID,
			"state_name":        res.StateName,
			"status":            res.Status,
			"serial_before":     res.SerialBefore,
			"serial_after":      res.SerialAfter,
			"resources_checked": res.ResourcesChecked,
			"resources_changed": res.ResourcesChanged,
			"resources_failed":  res.ResourcesFailed,
			"errors":            errs,
			"started_at":        res.StartedAt,
			"finished_at":       res.FinishedAt,
			"dry_run":           res.DryRun,
			"changed_addresses": res.ChangedAddresses,
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func formatAdminQueryValue(v any, typeOID uint32) string {
	if v == nil {
		return ""
	}
	if typeOID == 114 || typeOID == 3802 {
		if b, ok := v.([]byte); ok {
			return string(b)
		}
	}
	switch x := v.(type) {
	case []byte:
		return string(x)
	case time.Time:
		return x.UTC().Format(time.RFC3339Nano)
	default:
		return fmt.Sprintf("%v", x)
	}
}

func (s *Server) handleAdminStateHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	limit, err := parseOptionalInt(r.URL.Query().Get("limit"), 20)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	offset, err := parseOptionalInt(r.URL.Query().Get("offset"), 0)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid offset")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	rows, err := st.ListVersions(r.Context(), name, limit, offset)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"state":    name,
		"versions": rows,
	})
}

func (s *Server) handleAdminStateStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	status, err := st.GetStateStatus(r.Context(), name)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}

func (s *Server) handleAdminStateStatusQuery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	status, err := st.GetStateStatus(r.Context(), name)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (s *Server) handleAdminStateDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	fromRef := strings.TrimSpace(r.URL.Query().Get("from"))
	if fromRef == "" {
		fromRef = "@1"
	}
	toRef := strings.TrimSpace(r.URL.Query().Get("to"))
	if toRef == "" {
		toRef = "current"
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	// Probe state existence explicitly to preserve clear operator errors.
	if _, _, err := st.GetVersionRaw(r.Context(), name, "current"); err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "state not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	fromInfo, _, err := st.GetVersionRaw(r.Context(), name, fromRef)
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "from version not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	toInfo, _, err := st.GetVersionRaw(r.Context(), name, toRef)
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "to version not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	rows, err := st.DiffVersionResources(r.Context(), fromInfo.ID, toInfo.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"state": name,
		"from":  fromInfo,
		"to":    toInfo,
		"rows":  rows,
	})
}

func adminStateName(r *http.Request) string {
	if r == nil {
		return ""
	}
	return strings.TrimSpace(firstNonEmptyString(
		r.Header.Get("X-Kilolock-State-Name"),
		r.URL.Query().Get("state_name"),
		r.URL.Query().Get("name"),
	))
}

func firstNonEmptyString(parts ...string) string {
	for _, part := range parts {
		if v := strings.TrimSpace(part); v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleAdminStateCurrent(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	ensureGenesis := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("ensure_genesis")), "true")
	var info *store.CurrentStateInfo
	if ensureGenesis {
		info, err = st.EnsureCurrentStateInfo(r.Context(), name)
	} else {
		info, err = st.GetCurrentStateInfo(r.Context(), name)
	}
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state_id":   info.StateID,
		"version_id": info.VersionID,
		"serial":     info.Serial,
		"raw_state":  string(info.Raw),
		"state_name": name,
	})
}

func (s *Server) handleAdminStateRawAtSerial(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	serial, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("serial")), 10, 64)
	if err != nil || serial <= 0 {
		writeJSONError(w, http.StatusBadRequest, "valid serial is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	raw, err := st.GetStateRawAtSerial(r.Context(), name, serial)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"raw_state": string(raw)})
}

func (s *Server) handleAdminStateVersionID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stateID := strings.TrimSpace(r.URL.Query().Get("state_id"))
	serial, err := strconv.ParseInt(strings.TrimSpace(r.URL.Query().Get("serial")), 10, 64)
	if stateID == "" || err != nil || serial <= 0 {
		writeJSONError(w, http.StatusBadRequest, "state_id and valid serial are required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	versionID, err := st.LookupStateVersionID(r.Context(), stateID, serial)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state version not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"version_id": versionID})
}

func (s *Server) handleAdminStateImport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		Name     string `json:"name"`
		RawState string `json:"raw_state"`
		Source   string `json:"source"`
		Actor    string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.Name = strings.TrimSpace(in.Name)
	in.Source = strings.TrimSpace(in.Source)
	if in.Name == "" || in.RawState == "" {
		writeJSONError(w, http.StatusBadRequest, "state name and raw_state are required")
		return
	}
	if !validAdminImportSource(in.Source) {
		writeJSONError(w, http.StatusBadRequest, "invalid source")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.WriteState(r.Context(), in.Name, "", []byte(in.RawState), in.Source, in.Actor); err != nil {
		switch {
		case errors.Is(err, store.ErrInvalidState):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, store.ErrSerialConflict), errors.Is(err, store.ErrStateLocked):
			writeJSONError(w, http.StatusConflict, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": in.Name})
}

func validAdminImportSource(s string) bool {
	switch s {
	case "import", "apply", "refresh", "unknown":
		return true
	default:
		return false
	}
}

func (s *Server) handleAdminStateTags(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	tags, err := st.ListTags(r.Context(), name)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": name, "tags": tags})
}

func (s *Server) handleAdminStateTagSet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		Tag         string `json:"tag"`
		VersionRef  string `json:"version_ref"`
		Description string `json:"description"`
		Actor       string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	row, err := st.SetTag(r.Context(), name, strings.TrimSpace(in.VersionRef), strings.TrimSpace(in.Tag), in.Description, in.Actor)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrStateNotFound), errors.Is(err, store.ErrTagReservedName):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, row)
}

func (s *Server) handleAdminStateTagUnset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		Tag   string `json:"tag"`
		Actor string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.UnsetTag(r.Context(), name, strings.TrimSpace(in.Tag), in.Actor); err != nil {
		switch {
		case errors.Is(err, store.ErrStateNotFound), errors.Is(err, store.ErrTagNotFound):
			writeJSONError(w, http.StatusBadRequest, err.Error())
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": name, "tag": strings.TrimSpace(in.Tag)})
}

func (s *Server) handleAdminStateLockMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	mode := strings.ToLower(strings.TrimSpace(in.Mode))
	var on bool
	switch mode {
	case "exclusive":
		on = true
	case "optimistic":
		on = false
	default:
		writeJSONError(w, http.StatusBadRequest, "mode must be optimistic or exclusive")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.SetStateExclusiveLocks(r.Context(), name, on); err != nil {
		switch {
		case errors.Is(err, store.ErrStateNotFound):
			writeJSONError(w, http.StatusNotFound, "state not found")
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": name, "mode": mode})
}

func (s *Server) handleAdminStateCoexistenceMode(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	mode := store.StateCoexistenceMode(strings.ToLower(strings.TrimSpace(in.Mode)))
	if !mode.Valid() {
		writeJSONError(w, http.StatusBadRequest, "mode must be warn or strict")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.SetStateCoexistenceMode(r.Context(), name, mode); err != nil {
		switch {
		case errors.Is(err, store.ErrStateNotFound):
			writeJSONError(w, http.StatusNotFound, "state not found")
		default:
			writeJSONError(w, http.StatusInternalServerError, "internal error")
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": name, "mode": string(mode)})
}

func (s *Server) handleAdminStateResources(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	addressGlob := strings.TrimSpace(r.URL.Query().Get("address_glob"))
	limit, err := parseOptionalInt(r.URL.Query().Get("limit"), 200)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	rows, err := st.ListCurrentResources(r.Context(), name, addressGlob, limit)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":        name,
		"address_glob": addressGlob,
		"resources":    rows,
	})
}

func (s *Server) handleAdminStateResource(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	address := strings.TrimSpace(r.URL.Query().Get("address"))
	if name == "" || address == "" {
		writeJSONError(w, http.StatusBadRequest, "state name and address are required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	row, err := st.GetCurrentResource(r.Context(), name, address)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "resource not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":    name,
		"resource": row,
	})
}

func (s *Server) handleAdminStateResourceHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	address := strings.TrimSpace(r.URL.Query().Get("address"))
	if name == "" || address == "" {
		writeJSONError(w, http.StatusBadRequest, "state name and address are required")
		return
	}
	limit, err := parseOptionalInt(r.URL.Query().Get("limit"), 50)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid limit")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	rows, err := st.ListResourceHistory(r.Context(), name, address, limit)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":   name,
		"address": address,
		"history": rows,
	})
}

func (s *Server) handleAdminStateRollbackPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		To string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.To = strings.TrimSpace(in.To)
	if in.To == "" {
		writeJSONError(w, http.StatusBadRequest, "target version reference is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	current, _, err := st.GetVersionRaw(r.Context(), name, "current")
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "state not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	target, _, err := st.GetVersionRaw(r.Context(), name, in.To)
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "target version not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target.ID == current.ID {
		writeJSONError(w, http.StatusBadRequest, "target resolves to the current version")
		return
	}
	diff, err := st.DiffVersionAddresses(r.Context(), current.ID, target.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"state":   name,
		"current": current,
		"target":  target,
		"diff":    diff,
	})
}

func (s *Server) handleAdminStateResourceRollbackPreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		Address string `json:"address"`
		To      string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if strings.TrimSpace(in.Address) == "" || strings.TrimSpace(in.To) == "" {
		writeJSONError(w, http.StatusBadRequest, "address and to are required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	preview, err := st.PreviewReplayResourceVersion(r.Context(), name, in.Address, in.To)
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state or version not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleAdminStateRollbackApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		To    string `json:"to"`
		Actor string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.To = strings.TrimSpace(in.To)
	if in.To == "" {
		writeJSONError(w, http.StatusBadRequest, "target version reference is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	current, _, err := st.GetVersionRaw(r.Context(), name, "current")
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "state not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	target, _, err := st.GetVersionRaw(r.Context(), name, in.To)
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "target version not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if target.ID == current.ID {
		writeJSONError(w, http.StatusBadRequest, "target resolves to the current version")
		return
	}
	diff, err := st.DiffVersionAddresses(r.Context(), current.ID, target.ID)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	newVersion, err := st.ReplayVersion(r.Context(), name, target.ID, in.Actor)
	if err != nil {
		if errors.Is(err, store.ErrStateNotFound) {
			writeJSONError(w, http.StatusNotFound, "target version not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"state":   name,
		"current": current,
		"target":  target,
		"diff":    diff,
		"version": newVersion,
	})
}

func (s *Server) handleAdminStateResourceRollbackApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		Address string `json:"address"`
		To      string `json:"to"`
		Actor   string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	if strings.TrimSpace(in.Address) == "" || strings.TrimSpace(in.To) == "" {
		writeJSONError(w, http.StatusBadRequest, "address and to are required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	version, preview, err := st.ReplayResourceVersion(r.Context(), name, in.Address, in.To, firstNonEmptyString(in.Actor, actorFromRequest(r)))
	if errors.Is(err, store.ErrStateNotFound) {
		writeJSONError(w, http.StatusNotFound, "state or version not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"preview": preview,
		"version": version,
	})
}

func (s *Server) handleAdminStateWriteApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := adminStateName(r)
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	var in struct {
		RawState string `json:"raw_state"`
		Source   string `json:"source"`
		Actor    string `json:"actor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.WriteStateForApply(r.Context(), name, []byte(in.RawState), firstNonEmptyString(in.Source, "apply"), in.Actor); err != nil {
		switch {
		case errors.Is(err, store.ErrStateNotFound):
			writeJSONError(w, http.StatusNotFound, "state not found")
		case errors.Is(err, store.ErrSerialConflict):
			writeJSONError(w, http.StatusConflict, err.Error())
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminQuotaRemaining(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	stateName := strings.TrimSpace(r.URL.Query().Get("state_name"))
	if stateName == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	preview, err := st.PreviewStateQuota(r.Context(), stateName, 0)
	if err != nil {
		if errors.Is(err, store.ErrTenantNotFound) {
			writeJSONError(w, http.StatusNotFound, "tenant not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleAdminQuotaCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		StateName            string `json:"state_name"`
		PlannedResourceDelta int    `json:"planned_resource_delta"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	in.StateName = strings.TrimSpace(in.StateName)
	if in.StateName == "" {
		writeJSONError(w, http.StatusBadRequest, "state name is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	preview, err := st.PreviewStateQuota(r.Context(), in.StateName, in.PlannedResourceDelta)
	if err != nil {
		if errors.Is(err, store.ErrTenantNotFound) {
			writeJSONError(w, http.StatusNotFound, "tenant not found")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, preview)
}

func (s *Server) handleAdminApplyRunBegin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		StateID       string          `json:"state_id"`
		FromVersionID string          `json:"from_version_id"`
		Actor         string          `json:"actor"`
		SourceSerial  int64           `json:"source_serial"`
		Info          json.RawMessage `json:"info"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	run, err := st.BeginApplyRun(r.Context(), in.StateID, in.FromVersionID, in.Actor, in.SourceSerial, in.Info)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, run)
}

func (s *Server) handleAdminApplyRunStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	status, err := st.GetApplyRunStatus(r.Context(), applyID)
	if errors.Is(err, store.ErrApplyRunNotFound) {
		writeJSONError(w, http.StatusNotFound, "apply run not found")
		return
	}
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": status})
}

func (s *Server) handleAdminApplyRunFinish(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	var in store.FinishApplyRunInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.FinishApplyRun(r.Context(), applyID, in); err != nil {
		switch {
		case errors.Is(err, store.ErrApplyRunNotFound):
			writeJSONError(w, http.StatusNotFound, "apply run not found")
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminApplyRunAbort(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	var in struct {
		Reason string `json:"reason"`
	}
	_ = json.NewDecoder(r.Body).Decode(&in)
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.AbortApplyRun(r.Context(), applyID, in.Reason); err != nil {
		switch {
		case errors.Is(err, store.ErrApplyRunNotFound):
			writeJSONError(w, http.StatusNotFound, "apply run not found")
		default:
			writeJSONError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminReservationsAcquire(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	var in struct {
		StateID     string              `json:"state_id"`
		ApplyID     string              `json:"apply_id"`
		Actor       string              `json:"actor"`
		Want        []store.Reservation `json:"want"`
		LeaseSecond int                 `json:"lease_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	err = st.AcquireReservations(r.Context(), in.StateID, in.ApplyID, in.Actor, in.Want, time.Duration(in.LeaseSecond)*time.Second)
	var conflict *store.ReservationConflictError
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case errors.As(err, &conflict):
		writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflicts": conflict.Conflicts})
	default:
		writeJSONError(w, http.StatusBadRequest, err.Error())
	}
}

func (s *Server) handleAdminReservationsRenew(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("apply_id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	var in struct {
		LeaseSecond int `json:"lease_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid json body")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	rows, err := st.RenewReservations(r.Context(), applyID, time.Duration(in.LeaseSecond)*time.Second)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"rows": rows})
}

func (s *Server) handleAdminReservationsRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	applyID := strings.TrimSpace(r.PathValue("apply_id"))
	if applyID == "" {
		writeJSONError(w, http.StatusBadRequest, "apply id is required")
		return
	}
	st, err := s.dataStore(r.Context())
	if err != nil {
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	if err := st.ReleaseReservations(r.Context(), applyID); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func parseOptionalInt(raw string, def int) (int, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, err
	}
	return n, nil
}

// withAuth runs the configured Authenticator and attaches the
// resulting Principal to the request context. On failure it returns
// 401 with the Authenticator's error message. The middleware is
// intentionally tiny: any real policy lives in the Authenticator
// implementation itself, not here.
func (s *Server) withAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.availabilityCheck != nil {
			ready, err := s.availabilityCheck(r.Context())
			if err != nil {
				s.logger.Error("availability check failed",
					"path", r.URL.Path, "method", r.Method, "err", err)
				writeJSONError(w, http.StatusServiceUnavailable, "service unavailable")
				return
			}
			if !ready {
				writeJSONError(w, http.StatusServiceUnavailable, "system is not initialized")
				return
			}
		}
		p, err := s.authenticator.Authenticate(r)
		if err != nil {
			s.logger.Warn("authentication failed",
				"path", r.URL.Path, "method", r.Method, "err", err)
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		ctx := auth.WithPrincipal(r.Context(), p)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// errResp is the JSON shape returned for non-protocol errors.
type errResp struct {
	Error string `json:"error"`
}

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

func (s *Server) handleRoutingStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	out := map[string]any{
		"ok": true,
	}
	if s.routingStats != nil {
		for k, v := range s.routingStats() {
			out[k] = v
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// handleState dispatches every method understood by the HTTP backend
// protocol. The path pattern ensures r.PathValue("name") is populated.
func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	start := time.Now()
	sw := &statusCapturingResponseWriter{ResponseWriter: w}
	defer s.logStateRequest(r.Context(), r, name, sw.status, time.Since(start))
	if name == "" {
		writeJSONError(sw, http.StatusBadRequest, "state name is required")
		return
	}
	if err := validateStateNameAgainstPrincipal(r.Context(), name); err != nil {
		writeJSONError(sw, http.StatusForbidden, err.Error())
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGet(sw, r, name)
	case http.MethodPost:
		s.handlePost(sw, r, name)
	case http.MethodDelete:
		s.handleDelete(sw, r, name)
	case "LOCK":
		s.handleLock(sw, r, name)
	case "UNLOCK":
		s.handleUnlock(sw, r, name)
	default:
		sw.Header().Set("Allow", "GET, POST, DELETE, LOCK, UNLOCK")
		writeJSONError(sw, http.StatusMethodNotAllowed,
			fmt.Sprintf("method %s not supported on %s", r.Method, r.URL.Path))
	}
}

func (s *Server) handleStateUnlockPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	start := time.Now()
	sw := &statusCapturingResponseWriter{ResponseWriter: w}
	defer s.logStateRequest(r.Context(), r, name, sw.status, time.Since(start))
	if name == "" {
		writeJSONError(sw, http.StatusBadRequest, "state name is required")
		return
	}
	if err := validateStateNameAgainstPrincipal(r.Context(), name); err != nil {
		writeJSONError(sw, http.StatusForbidden, err.Error())
		return
	}
	if r.Method != http.MethodPost {
		sw.Header().Set("Allow", "POST")
		writeJSONError(sw, http.StatusMethodNotAllowed,
			fmt.Sprintf("method %s not supported on %s", r.Method, r.URL.Path))
		return
	}
	s.handleUnlock(sw, r, name)
}

func (s *Server) logStateRequest(ctx context.Context, r *http.Request, state string, status int, duration time.Duration) {
	if s.logger == nil {
		return
	}
	if status == 0 {
		status = http.StatusOK
	}
	attrs := append(requestLogAttrs(ctx, state),
		"method", r.Method,
		"path", r.URL.Path,
		"query", r.URL.RawQuery,
		"status", status,
		"duration_ms", duration.Milliseconds(),
	)
	if cl := strings.TrimSpace(r.Header.Get("Content-Length")); cl != "" {
		attrs = append(attrs, "content_length", cl)
	}
	if ua := strings.TrimSpace(r.UserAgent()); ua != "" {
		attrs = append(attrs, "user_agent", ua)
	}
	switch {
	case status >= 500:
		s.logger.Error("state request failed", attrs...)
	case status >= 400:
		s.logger.Warn("state request rejected", attrs...)
	default:
		s.logger.Debug("state request", attrs...)
	}
}

func validateStateNameAgainstPrincipal(ctx context.Context, stateName string) error {
	p, ok := auth.FromContext(ctx)
	if !ok {
		return nil
	}
	workspaceID := strings.TrimSpace(p.WorkspaceID)
	environmentPublicID := strings.TrimSpace(p.EnvironmentPublicID)
	if workspaceID == "" || environmentPublicID == "" {
		return nil
	}
	parts := strings.Split(strings.Trim(strings.TrimSpace(stateName), "/"), "/")
	if len(parts) < 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return fmt.Errorf("state path must start with %q", workspaceID+"/"+environmentPublicID+"/…")
	}
	if parts[0] != workspaceID || parts[1] != environmentPublicID {
		return fmt.Errorf("state path uses the wrong workspace and/or environment for this credential")
	}
	return nil
}

func (s *Server) dataStore(ctx context.Context) (*store.Store, error) {
	if s.resolveStore == nil {
		return nil, fmt.Errorf("server has no store resolver")
	}
	return s.resolveStore(ctx)
}

func isEnvironmentUnavailableError(err error) bool {
	if errors.Is(err, routing.ErrEnvironmentUnavailable) {
		return true
	}
	if errors.Is(err, routing.ErrInstanceCircuitOpen) {
		return true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true
	}
	var sysErr *os.SyscallError
	if errors.As(err, &sysErr) {
		if errno, ok := sysErr.Err.(syscall.Errno); ok {
			switch errno {
			case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
				return true
			}
		}
	}
	if errno, ok := err.(syscall.Errno); ok {
		switch errno {
		case syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EPIPE, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH:
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "connection refused"),
		strings.Contains(msg, "no such host"),
		strings.Contains(msg, "server closed the connection"),
		strings.Contains(msg, "broken pipe"),
		strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "failed to receive message"),
		strings.Contains(msg, "terminating connection"),
		strings.Contains(msg, "instance circuit open"):
		return true
	default:
		return false
	}
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request, name string) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		s.logger.Error("resolve store failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	raw, err := st.GetCurrentState(r.Context(), name)
	switch {
	case errors.Is(err, store.ErrStateNotFound):
		http.NotFound(w, r)
		return
	case err != nil:
		s.logger.Error("get state failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		if isEnvironmentUnavailableError(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request, name string) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		s.logger.Error("resolve store failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	lockID := r.URL.Query().Get("ID")
	actor := actorFromRequest(r)

	body, err := readBody(r, maxStateBodyBytes)
	if err != nil {
		s.logger.Warn("state body too large or unreadable", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	if !json.Valid(body) {
		writeJSONError(w, http.StatusBadRequest, "state body must be valid JSON")
		return
	}

	err = st.WriteState(r.Context(), name, lockID, body, sourceHTTPBackend, actor)
	var conflict *store.WriteSetConflictError
	var tenantInactive *store.TenantNotActiveError
	var stateInactive *store.StateNotActiveError
	switch {
	case errors.Is(err, store.ErrInvalidState):
		s.logger.Warn("state body is not a valid Terraform state", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	case errors.Is(err, store.ErrSerialConflict):
		writeJSONError(w, http.StatusConflict, "state serial already stored; use a higher serial")
		return
	case errors.Is(err, store.ErrLineageMismatch):
		writeJSONError(w, http.StatusConflict,
			"state lineage mismatch: the POSTed state belongs to a different Terraform lineage than the stored one")
		return
	case errors.As(err, &conflict):
		// Optimistic-merge conflict: another operator committed
		// changes to addresses we also wanted to write. Terraform
		// itself doesn't have a structured handler for this beyond
		// "the apply failed and surfaced the body" — render a
		// shape that's machine-readable AND comprehensible at the
		// CLI:
		//
		//   conflicting_addresses → list the operator can grep
		//                           against their plan output
		//   latest_serial         → the serial they should refresh
		//                           against
		//
		// The HTTP status is 409 because terraform's HTTP backend
		// treats 409 as a retryable conflict and surfaces the
		// response body to the user (see Terraform's
		// http/client.go errFromStatusCode mapping).
		s.logger.Info("optimistic write-set conflict", append(
			requestLogAttrs(r.Context(), name),
			"addresses", conflict.Addresses,
			"latest_serial", conflict.LatestSerial,
		)...)
		writeJSONStatus(w, http.StatusConflict, writeSetConflictBody{
			Error:                "write-set conflict: another operator changed the same addresses while you were planning. Re-run `terraform refresh` and `terraform plan`, then apply again.",
			ConflictingAddresses: conflict.Addresses,
			LatestSerial:         conflict.LatestSerial,
		})
		return
	case errors.Is(err, store.ErrStateLocked):
		writeJSONError(w, http.StatusLocked, "state is locked; no lock id supplied")
		return
	case errors.Is(err, store.ErrLockMismatch):
		writeJSONError(w, http.StatusConflict, "lock id does not match current lock")
		return
	case errors.Is(err, store.ErrEntitlementExceeded):
		s.logger.Info("write state rejected by quota", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	case errors.As(err, &stateInactive):
		writeJSONError(w, http.StatusForbidden, err.Error())
		return
	case errors.As(err, &tenantInactive):
		writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
		return
	case err != nil:
		s.logger.Error("write state failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		if isEnvironmentUnavailableError(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// writeSetConflictBody is the structured 409 body the optimistic
// merge path returns. The CLI's wrapper (and any future SDK) can
// parse this directly; vanilla terraform just prints the JSON in
// its error message, which is still operator-readable enough to
// point at the conflicting addresses.
type writeSetConflictBody struct {
	Error                string   `json:"error"`
	ConflictingAddresses []string `json:"conflicting_addresses"`
	LatestSerial         int64    `json:"latest_serial"`
}

func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request, name string) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		s.logger.Error("resolve store failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	lockID := r.URL.Query().Get("ID")
	actor := actorFromRequest(r)

	err = st.DeleteState(r.Context(), name, lockID, actor)
	var tenantInactive *store.TenantNotActiveError
	switch {
	case errors.Is(err, store.ErrStateNotFound):
		http.NotFound(w, r)
		return
	case errors.Is(err, store.ErrStateLocked):
		writeJSONError(w, http.StatusLocked, "state is locked; no lock id supplied")
		return
	case errors.Is(err, store.ErrLockMismatch):
		writeJSONError(w, http.StatusConflict, "lock id does not match current lock")
		return
	case errors.As(err, &tenantInactive):
		writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
		return
	case err != nil:
		s.logger.Error("delete state failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		if isEnvironmentUnavailableError(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleLock(w http.ResponseWriter, r *http.Request, name string) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		s.logger.Error("resolve store failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	body, err := readBody(r, maxLockBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	var info store.LockInfo
	if jerr := json.Unmarshal(body, &info); jerr != nil {
		writeJSONError(w, http.StatusBadRequest, "lock body must be valid JSON")
		return
	}
	if info.ID == "" {
		writeJSONError(w, http.StatusBadRequest, "lock body must include ID")
		return
	}

	existing, err := st.AcquireLock(r.Context(), name, info)
	var tenantInactive *store.TenantNotActiveError
	switch {
	case errors.Is(err, store.ErrAlreadyLocked):
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusLocked)
		_ = json.NewEncoder(w).Encode(existing)
		return
	case errors.As(err, &tenantInactive):
		writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
		return
	case err != nil:
		s.logger.Error("acquire lock failed", append(requestLogAttrs(r.Context(), name), "err", err)...)
		if isEnvironmentUnavailableError(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleUnlock implements two related operations dispatched by the
// presence (or absence) of a request body:
//
//   - **Owner release**: body is a JSON LockInfo with a non-empty ID.
//     The lock is released only if the ID matches the one currently
//     held; otherwise a 409 is returned.
//
//   - **Force release**: body is empty (or JSON with an empty ID).
//     Terraform's `terraform force-unlock` against an http backend
//     reaches this path because the client never transmits the
//     user-supplied lock ID over the wire (see Terraform source:
//     internal/backend/remote-state/http/client.go). The semantics are
//     "release whatever is held" -- recorded as a distinct event
//     kind in the audit trail and idempotent on no-lock states.
func (s *Server) handleUnlock(w http.ResponseWriter, r *http.Request, name string) {
	st, err := s.dataStore(r.Context())
	if err != nil {
		s.logger.Error("resolve store failed", "err", err)
		writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
		return
	}
	body, err := readBody(r, maxLockBodyBytes)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	actor := actorFromRequest(r)

	force, lockID, perr := parseUnlockRequest(body, r.URL.Query().Get("ID"))
	if perr != nil {
		s.logger.Warn("invalid unlock payload",
			append(requestLogAttrs(r.Context(), name),
				"body_len", len(body),
				"body_preview", previewUnlockPayload(body),
				"err", perr,
			)...,
		)
		writeJSONError(w, http.StatusBadRequest, "unlock body must be valid JSON or a lock id string")
		return
	}

	if force {
		if ferr := st.ForceReleaseLock(r.Context(), name, actor); ferr != nil {
			s.logger.Error("force release lock failed", "state", name, "err", ferr)
			var tenantInactive *store.TenantNotActiveError
			if errors.As(ferr, &tenantInactive) {
				writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
				return
			}
			if isEnvironmentUnavailableError(ferr) {
				writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
				return
			}
			writeJSONError(w, http.StatusInternalServerError, "internal error")
			return
		}
		w.WriteHeader(http.StatusOK)
		return
	}

	err = st.ReleaseLock(r.Context(), name, lockID, actor)
	var tenantInactive *store.TenantNotActiveError
	switch {
	case errors.Is(err, store.ErrLockMismatch):
		writeJSONError(w, http.StatusConflict, "lock id does not match current lock")
		return
	case errors.As(err, &tenantInactive):
		writeJSONError(w, http.StatusForbidden, tenantInactive.Error())
		return
	case err != nil:
		s.logger.Error("release lock failed", "state", name, "err", err)
		if isEnvironmentUnavailableError(err) {
			writeJSONError(w, http.StatusServiceUnavailable, "environment unavailable")
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusOK)
}

func parseUnlockRequest(body []byte, queryID string) (force bool, lockID string, err error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		if q := strings.TrimSpace(queryID); q != "" {
			return false, q, nil
		}
		return true, "", nil
	}

	var info store.LockInfo
	if jerr := json.Unmarshal(trimmed, &info); jerr == nil {
		if strings.TrimSpace(info.ID) == "" {
			return true, "", nil
		}
		return false, strings.TrimSpace(info.ID), nil
	}

	var stringID string
	if jerr := json.Unmarshal(trimmed, &stringID); jerr == nil {
		if strings.TrimSpace(stringID) == "" {
			return true, "", nil
		}
		return false, strings.TrimSpace(stringID), nil
	}

	rawID := strings.TrimSpace(string(trimmed))
	if isLikelyLockID(rawID) {
		return false, rawID, nil
	}
	if q := strings.TrimSpace(queryID); q != "" {
		return false, q, nil
	}

	return false, "", fmt.Errorf("unsupported unlock payload shape")
}

func isLikelyLockID(s string) bool {
	if strings.TrimSpace(s) == "" || len(s) > 256 || strings.ContainsAny(s, "\r\n\t ") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("-_:.@/+", r):
		default:
			return false
		}
	}
	return true
}

func previewUnlockPayload(body []byte) string {
	trimmed := strings.TrimSpace(string(bytes.TrimSpace(body)))
	if trimmed == "" {
		return "<empty>"
	}
	if len(trimmed) > 160 {
		return trimmed[:160] + "..."
	}
	return trimmed
}

func readBody(r *http.Request, limit int64) ([]byte, error) {
	limited := http.MaxBytesReader(nil, r.Body, limit)
	defer r.Body.Close()
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	return body, nil
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(errResp{Error: msg})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// writeJSONStatus writes an arbitrary structured body with the given
// HTTP status. Used for typed conflict responses that carry more
// than just a string error.
func writeJSONStatus(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// actorFromRequest extracts a best-effort caller identifier for the audit
// trail. v0 does not authenticate; this is purely informational.
func actorFromRequest(r *http.Request) string {
	if u, _, ok := r.BasicAuth(); ok && u != "" {
		return u
	}
	if ua := r.Header.Get("User-Agent"); ua != "" {
		return strings.SplitN(ua, " ", 2)[0]
	}
	return "unknown"
}
