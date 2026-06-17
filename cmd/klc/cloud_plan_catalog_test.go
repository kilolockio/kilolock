//go:build cloud

package main

import "testing"

func TestLoadStripePlanCatalogFromEnv_LegacyFallback(t *testing.T) {
	t.Setenv("KL_STRIPE_PLANS_JSON", "")
	t.Setenv("KL_STRIPE_DEFAULT_PLAN", "")
	t.Setenv("KL_STRIPE_PRICE_ID", "price_pro")

	catalog, defaultPlan, err := loadStripePlanCatalogFromEnv()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if defaultPlan != "starter" {
		t.Fatalf("defaultPlan=%q want starter", defaultPlan)
	}
	if _, ok := catalog["starter"]; !ok {
		t.Fatalf("starter missing from catalog: %+v", catalog)
	}
	if _, ok := catalog["enterprise"]; !ok {
		t.Fatalf("enterprise missing from catalog: %+v", catalog)
	}
	plan, ok := catalog["pro"]
	if !ok {
		t.Fatalf("pro missing from catalog: %+v", catalog)
	}
	if plan.PriceID != "price_pro" || plan.MaxEnvironments != 3 {
		t.Fatalf("legacy plan=%+v", plan)
	}
}

func TestLoadStripePlanCatalogFromEnv_JSONCatalog(t *testing.T) {
	t.Setenv("KL_STRIPE_PRICE_ID", "")
	t.Setenv("KL_STRIPE_DEFAULT_PLAN", "pro")
	t.Setenv("KL_STRIPE_PLANS_JSON", `[{"billing_plan":"starter","display_name":"Starter","max_environments":1,"max_state_resources":100,"max_environment_resources":500},{"billing_plan":"pro","display_name":"Pro","price_id":"price_pro","max_environments":3,"max_state_resources":10000,"max_environment_resources":50000},{"billing_plan":"enterprise","display_name":"Enterprise","contact_only":true,"max_environments":0,"max_state_resources":0,"max_environment_resources":0}]`)

	catalog, defaultPlan, err := loadStripePlanCatalogFromEnv()
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	if defaultPlan != "pro" {
		t.Fatalf("defaultPlan=%q want pro", defaultPlan)
	}
	plan, err := catalog.checkoutPlan("", defaultPlan)
	if err != nil {
		t.Fatalf("checkoutPlan default: %v", err)
	}
	if plan.BillingPlan != "pro" || plan.PriceID != "price_pro" || plan.MaxEnvironments != 3 {
		t.Fatalf("default checkout plan=%+v", plan)
	}
	matched, ok := catalog.planForPriceID("price_pro")
	if !ok || matched.BillingPlan != "pro" {
		t.Fatalf("price lookup got ok=%v plan=%+v", ok, matched)
	}
	if starter, ok := catalog["starter"]; !ok || starter.PriceID != "" {
		t.Fatalf("starter free plan=%+v ok=%v", starter, ok)
	}
	if enterprise, ok := catalog["enterprise"]; !ok || !enterprise.ContactOnly {
		t.Fatalf("enterprise plan=%+v ok=%v", enterprise, ok)
	}
}
