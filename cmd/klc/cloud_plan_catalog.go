//go:build cloud

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/davesade/kilolock/cloud/controlbilling"
)

type stripePlanCatalog map[string]controlbilling.CatalogPlan

func loadStripePlanCatalogFromEnv() (stripePlanCatalog, string, error) {
	rawJSON := strings.TrimSpace(os.Getenv("KL_STRIPE_PLANS_JSON"))
	defaultPlan := strings.TrimSpace(os.Getenv("KL_STRIPE_DEFAULT_PLAN"))
	if rawJSON != "" {
		var rows []struct {
			BillingPlan             string `json:"billing_plan"`
			PriceID                 string `json:"price_id"`
			DisplayName             string `json:"display_name"`
			ContactOnly             bool   `json:"contact_only"`
			MaxEnvironments         int    `json:"max_environments"`
			MaxStateResources       int    `json:"max_state_resources"`
			MaxEnvironmentResources int    `json:"max_environment_resources"`
		}
		if err := json.Unmarshal([]byte(rawJSON), &rows); err != nil {
			return nil, "", fmt.Errorf("parse KL_STRIPE_PLANS_JSON: %w", err)
		}
		catalog := make(stripePlanCatalog, len(rows))
		for _, row := range rows {
			planKey := strings.TrimSpace(row.BillingPlan)
			priceID := strings.TrimSpace(row.PriceID)
			if planKey == "" {
				return nil, "", fmt.Errorf("each stripe plan needs billing_plan")
			}
			if !row.ContactOnly && planKey != controlbilling.StarterPlan().BillingPlan && priceID == "" {
				return nil, "", fmt.Errorf("plan %q needs price_id unless it is free or contact_only", planKey)
			}
			catalog[planKey] = controlbilling.CatalogPlan{
				EntitlementPlan: controlbilling.EntitlementPlan{
					BillingPlan:             planKey,
					MaxEnvironments:         row.MaxEnvironments,
					MaxStateResources:       row.MaxStateResources,
					MaxEnvironmentResources: row.MaxEnvironmentResources,
				},
				PriceID:     priceID,
				DisplayName: strings.TrimSpace(row.DisplayName),
				ContactOnly: row.ContactOnly,
			}
		}
		if defaultPlan == "" && len(rows) == 1 {
			defaultPlan = strings.TrimSpace(rows[0].BillingPlan)
		}
		if defaultPlan != "" {
			if _, ok := catalog[defaultPlan]; !ok {
				return nil, "", fmt.Errorf("default stripe plan %q is not in KL_STRIPE_PLANS_JSON", defaultPlan)
			}
		}
		return catalog, defaultPlan, nil
	}

	legacyPriceID := strings.TrimSpace(os.Getenv("KL_STRIPE_PRICE_ID"))
	defaults := make(stripePlanCatalog)
	for _, plan := range controlbilling.DefaultCatalogPlans() {
		defaults[plan.BillingPlan] = plan
	}
	if legacyPriceID != "" {
		pro := defaults[controlbilling.ProPlan().BillingPlan]
		pro.PriceID = legacyPriceID
		defaults[pro.BillingPlan] = pro
	}
	if defaultPlan == "" {
		defaultPlan = controlbilling.StarterPlan().BillingPlan
	}
	return defaults, defaultPlan, nil
}

func (c stripePlanCatalog) checkoutPlan(planKey, defaultPlan string) (controlbilling.CatalogPlan, error) {
	planKey = strings.TrimSpace(planKey)
	if planKey == "" {
		planKey = strings.TrimSpace(defaultPlan)
	}
	if planKey == "" && len(c) == 1 {
		for _, plan := range c {
			return plan, nil
		}
	}
	plan, ok := c[planKey]
	if !ok {
		return controlbilling.CatalogPlan{}, fmt.Errorf("unknown billing plan %q", planKey)
	}
	return plan, nil
}

func (c stripePlanCatalog) planForPriceID(priceID string) (controlbilling.CatalogPlan, bool) {
	priceID = strings.TrimSpace(priceID)
	if priceID == "" {
		return controlbilling.CatalogPlan{}, false
	}
	for _, plan := range c {
		if strings.TrimSpace(plan.PriceID) == priceID {
			return plan, true
		}
	}
	return controlbilling.CatalogPlan{}, false
}
