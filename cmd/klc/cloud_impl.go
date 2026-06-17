//go:build cloud

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/davesade/kilolock/cloud/billing"
	"github.com/davesade/kilolock/cloud/controlbilling"
)

type cloudFeatures struct {
	stripe              billing.StripeClient
	stripeWebhookSecret string
	stripePlans         stripePlanCatalog
	stripeDefaultPlan   string
	stripeSuccessURL    string
	stripeCancelURL     string
	stripePortalReturn  string
}

func newCloudFeatures() *cloudFeatures {
	plans, defaultPlan, err := loadStripePlanCatalogFromEnv()
	if err != nil {
		panic(err)
	}
	return &cloudFeatures{
		stripe:              billing.StripeClient{SecretKey: strings.TrimSpace(os.Getenv("KL_STRIPE_SECRET_KEY"))},
		stripeWebhookSecret: strings.TrimSpace(os.Getenv("KL_STRIPE_WEBHOOK_SECRET")),
		stripePlans:         plans,
		stripeDefaultPlan:   defaultPlan,
		stripeSuccessURL:    strings.TrimSpace(os.Getenv("KL_STRIPE_SUCCESS_URL")),
		stripeCancelURL:     strings.TrimSpace(os.Getenv("KL_STRIPE_CANCEL_URL")),
		stripePortalReturn:  strings.TrimSpace(os.Getenv("KL_STRIPE_PORTAL_RETURN_URL")),
	}
}

func (s *server) registerCloudRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/public/signup", s.handlePublicSignup)
	mux.HandleFunc("/public/stripe/webhook", s.handleStripeWebhook)
}

func cloudPermissionForRoute(method, path string) (string, bool) {
	switch {
	case method == http.MethodGet && path == "/billing/plans":
		return "tenant.billing.checkout", true
	case method == http.MethodPost && path == "/billing/checkout-session":
		return "tenant.billing.checkout", true
	case method == http.MethodPost && path == "/billing/portal-session":
		return "tenant.billing.checkout", true
	case method == http.MethodGet && path == "/portal/users":
		return "rbac.manage", true
	case method == http.MethodPost && path == "/portal/users/role":
		return "rbac.manage", true
	default:
		return "", false
	}
}

func (s *server) handleCloudAPI(w http.ResponseWriter, r *http.Request, path string, permission string) bool {
	switch {
	case r.Method == http.MethodGet && path == "/billing/plans":
		s.apiBillingPlansList(w, r, permission)
		return true
	case r.Method == http.MethodPost && path == "/billing/checkout-session":
		s.apiBillingCheckoutSessionCreate(w, r, permission)
		return true
	case r.Method == http.MethodPost && path == "/billing/portal-session":
		s.apiBillingPortalSessionCreate(w, r, permission)
		return true
	case r.Method == http.MethodGet && path == "/portal/users":
		s.apiPortalUsersList(w, r, permission)
		return true
	case r.Method == http.MethodPost && path == "/portal/users/role":
		s.apiPortalUserRoleSet(w, r, permission)
		return true
	default:
		return false
	}
}

func (s *server) apiBillingPlansList(w http.ResponseWriter, r *http.Request, permission string) {
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant_slug"))
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenant_slug is required"})
		return
	}
	if !s.requirePermission(w, r, permission, tenant, "") {
		return
	}
	writeJSON(w, http.StatusOK, s.billingPlansPayload())
}

func (s *server) billingPlansPayload() map[string]any {
	type planView struct {
		BillingPlan             string `json:"billing_plan"`
		DisplayName             string `json:"display_name"`
		PriceID                 string `json:"price_id"`
		CheckoutEnabled         bool   `json:"checkout_enabled"`
		ContactOnly             bool   `json:"contact_only"`
		MaxEnvironments         int    `json:"max_environments"`
		MaxStateResources       int    `json:"max_state_resources"`
		MaxEnvironmentResources int    `json:"max_environment_resources"`
		Default                 bool   `json:"default"`
	}
	out := make([]planView, 0, len(s.cloud.stripePlans))
	for _, plan := range s.cloud.stripePlans {
		out = append(out, planView{
			BillingPlan:             plan.BillingPlan,
			DisplayName:             strings.TrimSpace(plan.DisplayName),
			PriceID:                 plan.PriceID,
			CheckoutEnabled:         strings.TrimSpace(plan.PriceID) != "",
			ContactOnly:             plan.ContactOnly,
			MaxEnvironments:         plan.MaxEnvironments,
			MaxStateResources:       plan.MaxStateResources,
			MaxEnvironmentResources: plan.MaxEnvironmentResources,
			Default:                 strings.TrimSpace(plan.BillingPlan) == strings.TrimSpace(s.cloud.stripeDefaultPlan),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Default != out[j].Default {
			return out[i].Default
		}
		return out[i].BillingPlan < out[j].BillingPlan
	})
	return map[string]any{"plans": out}
}

func (s *server) handlePublicSignup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	headerEmail, err := requirePublicSignupIdentity(r, s.cfg.ResolvedInitMode())
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": err.Error()})
		return
	}
	var in struct {
		Email      string `json:"email"`
		TenantSlug string `json:"tenant_slug"`
		TenantName string `json:"tenant_name"`
		TokenName  string `json:"token_name"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	email := strings.TrimSpace(in.Email)
	if headerEmail != "" {
		if email != "" && !strings.EqualFold(email, headerEmail) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email mismatch"})
			return
		}
		email = headerEmail
	}
	if email == "" || !strings.Contains(email, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "email is required"})
		return
	}
	slug := strings.TrimSpace(in.TenantSlug)
	if slug == "" {
		slug = slugFromEmail(email)
	}
	name := strings.TrimSpace(in.TenantName)
	if name == "" {
		name = slug
	}
	tokenName := strings.TrimSpace(in.TokenName)
	if tokenName == "" {
		tokenName = "signup"
	}

	row, err := s.control.CreateTenantWithDefaultEnvironment(r.Context(), slug, name, true)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	starter := controlbilling.StarterPlan()
	if err := s.control.SetTenantEntitlements(r.Context(), row.Slug, starter.BillingPlan, starter.MaxEnvironments, starter.MaxStateResources, starter.MaxEnvironmentResources, email, "public signup"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	_, secret, err := s.control.CreateAPIToken(r.Context(), row.Slug, "default", tokenName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"tenant":      row,
		"environment": "default",
		"token": map[string]any{
			"name":   tokenName,
			"secret": secret,
		},
	})
}

func (s *server) apiBillingCheckoutSessionCreate(w http.ResponseWriter, r *http.Request, permission string) {
	var in struct {
		TenantSlug  string `json:"tenant_slug"`
		BillingPlan string `json:"billing_plan"`
		Email       string `json:"email"`
		Company     string `json:"company"`
		Actor       string `json:"actor"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tenant := strings.TrimSpace(in.TenantSlug)
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenant_slug is required"})
		return
	}
	if !s.requirePermission(w, r, permission, tenant, "") {
		return
	}
	out, err := s.billingCheckoutSessionPayload(r.Context(), tenant, in.BillingPlan, in.Email, in.Company, firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context()), "portal"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) billingCheckoutSessionPayload(ctx context.Context, tenant, billingPlan, email, company, actor string) (map[string]any, error) {
	if s.cloud == nil || len(s.cloud.stripePlans) == 0 {
		return nil, fmt.Errorf("stripe plans not configured (set KL_STRIPE_PLANS_JSON or KL_STRIPE_PRICE_ID)")
	}
	if strings.TrimSpace(s.cloud.stripeSuccessURL) == "" || strings.TrimSpace(s.cloud.stripeCancelURL) == "" {
		return nil, fmt.Errorf("stripe redirect URLs not configured (set KL_STRIPE_SUCCESS_URL and KL_STRIPE_CANCEL_URL)")
	}
	plan, err := s.cloud.stripePlans.checkoutPlan(billingPlan, s.cloud.stripeDefaultPlan)
	if err != nil {
		return nil, err
	}
	if plan.ContactOnly {
		return map[string]any{
			"billing_plan": plan.BillingPlan,
			"message":      "Enterprise is sales-led right now. Please contact us for pricing and migration support.",
			"contact_only": true,
		}, nil
	}
	if strings.TrimSpace(plan.PriceID) == "" {
		if plan.BillingPlan != controlbilling.StarterPlan().BillingPlan {
			return nil, fmt.Errorf("billing plan %q is not connected to Stripe yet (set KL_STRIPE_PRICE_ID or KL_STRIPE_PLANS_JSON)", plan.BillingPlan)
		}
		return map[string]any{
			"billing_plan": plan.BillingPlan,
			"message":      "Starter is free and already active. Upgrade to Pro when you need more environments or more managed resources.",
		}, nil
	}
	cid, err := controlbilling.GetTenantStripeCustomerID(ctx, s.control, tenant)
	if err != nil {
		return nil, err
	}
	if cid == "" {
		cust, cerr := s.cloud.stripe.CreateCustomer(ctx, email, company, tenant)
		if cerr != nil {
			return nil, cerr
		}
		cid = cust.ID
		if err := controlbilling.SetTenantStripeCustomerIDAudit(ctx, s.control, tenant, cid, actor, "portal checkout"); err != nil {
			return nil, err
		}
	}

	sess, err := s.cloud.stripe.CreateCheckoutSessionSubscription(ctx, cid, plan.PriceID, s.cloud.stripeSuccessURL, s.cloud.stripeCancelURL, tenant, plan.BillingPlan)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": sess.URL, "id": sess.ID, "billing_plan": plan.BillingPlan}, nil
}

func (s *server) apiBillingPortalSessionCreate(w http.ResponseWriter, r *http.Request, permission string) {
	var in struct {
		TenantSlug string `json:"tenant_slug"`
		Actor      string `json:"actor"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	tenant := strings.TrimSpace(in.TenantSlug)
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenant_slug is required"})
		return
	}
	if !s.requirePermission(w, r, permission, tenant, "") {
		return
	}
	out, err := s.billingPortalSessionPayload(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *server) billingPortalSessionPayload(ctx context.Context, tenant string) (map[string]any, error) {
	returnURL := strings.TrimSpace(s.cloud.stripePortalReturn)
	if returnURL == "" {
		returnURL = strings.TrimSpace(s.cloud.stripeCancelURL)
	}
	if returnURL == "" {
		return nil, fmt.Errorf("stripe portal return url not configured (set KL_STRIPE_PORTAL_RETURN_URL or KL_STRIPE_CANCEL_URL)")
	}

	cid, err := controlbilling.GetTenantStripeCustomerID(ctx, s.control, tenant)
	if err != nil {
		return nil, err
	}
	if cid == "" {
		return nil, fmt.Errorf("tenant has no stripe customer yet; start checkout first")
	}
	sess, err := s.cloud.stripe.CreateBillingPortalSession(ctx, cid, returnURL)
	if err != nil {
		return nil, err
	}
	return map[string]any{"url": sess.URL, "id": sess.ID}, nil
}

func (s *server) handleStripeWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"error": "method not allowed"})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "read body failed"})
		return
	}
	sig := r.Header.Get("Stripe-Signature")
	if verr := billing.VerifyStripeWebhookSignature(sig, raw, s.cloud.stripeWebhookSecret, time.Now(), 5*time.Minute); verr != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": verr.Error()})
		return
	}

	var evt struct {
		ID      string `json:"id"`
		Type    string `json:"type"`
		Created int64  `json:"created"`
		Data    struct {
			Object json.RawMessage `json:"object"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &evt); err != nil || strings.TrimSpace(evt.ID) == "" || strings.TrimSpace(evt.Type) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid stripe event"})
		return
	}

	first, err := controlbilling.RecordStripeWebhookEvent(r.Context(), s.control, evt.ID, evt.Type)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"error": err.Error()})
		return
	}
	if !first {
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok", "duplicate": true})
		return
	}

	actor := "stripe"
	var evCreated *time.Time
	if evt.Created > 0 {
		tm := time.Unix(evt.Created, 0).UTC()
		evCreated = &tm
	}

	switch evt.Type {
	case "checkout.session.completed":
		var sess struct {
			Customer     string            `json:"customer"`
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(evt.Data.Object, &sess); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid checkout session payload"})
			return
		}
		tenant := strings.TrimSpace(sess.Metadata["tenant_slug"])
		if tenant != "" && strings.TrimSpace(sess.Customer) != "" {
			_ = controlbilling.SetTenantStripeCustomerIDAudit(r.Context(), s.control, tenant, sess.Customer, actor, "stripe checkout.session.completed")
			_ = controlbilling.ApplyStripeSubscriptionUpdate(r.Context(), s.control, controlbilling.StripeSubscriptionUpdate{
				TenantSlug:              tenant,
				CustomerID:              sess.Customer,
				SubscriptionID:          sess.Subscription,
				Status:                  "trialing",
				EventCreatedAt:          evCreated,
				MaxEnvironments:         -1,
				MaxStateResources:       -1,
				MaxEnvironmentResources: -1,
			}, actor)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	case "customer.subscription.created", "customer.subscription.updated", "customer.subscription.deleted":
		var sub struct {
			ID               string `json:"id"`
			Customer         string `json:"customer"`
			Status           string `json:"status"`
			CurrentPeriodEnd int64  `json:"current_period_end"`
			Items            struct {
				Data []struct {
					Price struct {
						ID string `json:"id"`
					} `json:"price"`
				} `json:"data"`
			} `json:"items"`
			Metadata map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(evt.Data.Object, &sub); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid subscription payload"})
			return
		}
		var priceID string
		if len(sub.Items.Data) > 0 {
			priceID = sub.Items.Data[0].Price.ID
		}
		var end *time.Time
		if sub.CurrentPeriodEnd > 0 {
			tm := time.Unix(sub.CurrentPeriodEnd, 0).UTC()
			end = &tm
		}
		u := controlbilling.StripeSubscriptionUpdate{
			TenantSlug:              strings.TrimSpace(sub.Metadata["tenant_slug"]),
			CustomerID:              sub.Customer,
			SubscriptionID:          sub.ID,
			Status:                  sub.Status,
			PriceID:                 priceID,
			CurrentPeriodEnd:        end,
			EventCreatedAt:          evCreated,
			MaxEnvironments:         -1,
			MaxStateResources:       -1,
			MaxEnvironmentResources: -1,
		}
		if plan, ok := s.cloud.stripePlans.planForPriceID(priceID); ok {
			u.BillingPlan = plan.BillingPlan
			u.MaxEnvironments = plan.MaxEnvironments
			u.MaxStateResources = plan.MaxStateResources
			u.MaxEnvironmentResources = plan.MaxEnvironmentResources
		}
		if err := controlbilling.ApplyStripeSubscriptionUpdate(r.Context(), s.control, u, actor); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	case "invoice.payment_succeeded", "invoice_payment.paid", "invoice.paid":
		var inv struct {
			ID           string            `json:"id"`
			Customer     string            `json:"customer"`
			Subscription string            `json:"subscription"`
			Metadata     map[string]string `json:"metadata"`
			Lines        struct {
				Data []struct {
					Period struct {
						End int64 `json:"end"`
					} `json:"period"`
					Price struct {
						ID string `json:"id"`
					} `json:"price"`
				} `json:"data"`
			} `json:"lines"`
		}
		if err := json.Unmarshal(evt.Data.Object, &inv); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid invoice payload"})
			return
		}
		var (
			priceID string
			end     *time.Time
		)
		if len(inv.Lines.Data) > 0 {
			priceID = strings.TrimSpace(inv.Lines.Data[0].Price.ID)
			if inv.Lines.Data[0].Period.End > 0 {
				tm := time.Unix(inv.Lines.Data[0].Period.End, 0).UTC()
				end = &tm
			}
		}
		u := controlbilling.StripeSubscriptionUpdate{
			TenantSlug:              strings.TrimSpace(inv.Metadata["tenant_slug"]),
			CustomerID:              inv.Customer,
			SubscriptionID:          inv.Subscription,
			Status:                  "active",
			PriceID:                 priceID,
			CurrentPeriodEnd:        end,
			EventCreatedAt:          evCreated,
			MaxEnvironments:         -1,
			MaxStateResources:       -1,
			MaxEnvironmentResources: -1,
		}
		if plan, ok := s.cloud.stripePlans.planForPriceID(priceID); ok {
			u.BillingPlan = plan.BillingPlan
			u.MaxEnvironments = plan.MaxEnvironments
			u.MaxStateResources = plan.MaxStateResources
			u.MaxEnvironmentResources = plan.MaxEnvironmentResources
		}
		if err := controlbilling.ApplyStripeSubscriptionUpdate(r.Context(), s.control, u, actor); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
		return
	default:
		writeJSON(w, http.StatusOK, map[string]any{"status": "ignored"})
		return
	}
}

func (s *server) apiPortalUsersList(w http.ResponseWriter, r *http.Request, permission string) {
	tenant := strings.TrimSpace(r.URL.Query().Get("tenant"))
	if tenant == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "tenant query param is required"})
		return
	}
	if !s.requirePermission(w, r, permission, "*", "") {
		return
	}
	rows, err := s.control.ListPortalUsersByTenant(r.Context(), tenant)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": rows})
}

func (s *server) apiPortalUserRoleSet(w http.ResponseWriter, r *http.Request, permission string) {
	var in struct {
		TenantSlug string `json:"tenant_slug"`
		UserID     string `json:"user_id"`
		Role       string `json:"role"`
		Actor      string `json:"actor"`
	}
	if !decodeJSON(w, r, &in) {
		return
	}
	if !s.requirePermission(w, r, permission, "*", "") {
		return
	}
	actor := firstNonEmptyControlActor(in.Actor, controlActorFromContext(r.Context()))
	if err := s.control.SetPortalUserRole(r.Context(), in.TenantSlug, in.UserID, in.Role, actor, "platform_admin"); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func requirePublicSignupIdentity(r *http.Request, initMode string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("KL_PUBLIC_SIGNUP_MODE")))
	if mode == "" {
		if strings.EqualFold(strings.TrimSpace(initMode), "prod") {
			mode = "trusted_header"
		} else {
			mode = "open"
		}
	}
	switch mode {
	case "open":
		return "", nil
	case "shared_secret":
		want := strings.TrimSpace(os.Getenv("KL_PUBLIC_SIGNUP_TOKEN"))
		got := strings.TrimSpace(r.Header.Get("X-Kl-Signup-Token"))
		if want == "" || got == "" || got != want {
			return "", fmt.Errorf("unauthorized")
		}
		email := strings.TrimSpace(r.Header.Get("X-Kl-Signup-Email"))
		if email == "" {
			return "", fmt.Errorf("missing signup email header")
		}
		return email, nil
	case "trusted_header":
		raw := strings.TrimSpace(r.Header.Get("X-Goog-Authenticated-User-Email"))
		if raw == "" {
			raw = strings.TrimSpace(r.Header.Get("X-Kl-Verified-Email"))
		}
		if raw == "" {
			return "", fmt.Errorf("unauthorized")
		}
		raw = strings.TrimPrefix(raw, "accounts.google.com:")
		raw = strings.TrimSpace(raw)
		if raw == "" || !strings.Contains(raw, "@") {
			return "", fmt.Errorf("unauthorized")
		}
		return raw, nil
	default:
		return "", fmt.Errorf("invalid KL_PUBLIC_SIGNUP_MODE %q", mode)
	}
}

func slugFromEmail(email string) string {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.IndexByte(email, '@')
	if at > 0 {
		email = email[:at]
	}
	var b strings.Builder
	b.Grow(len(email))
	lastDash := false
	for _, r := range email {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "tenant"
	}
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}
