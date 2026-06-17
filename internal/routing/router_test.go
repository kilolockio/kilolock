package routing

import (
	"context"
	"errors"
	"testing"

	"github.com/kilolockio/kilolock/pkg/auth"
	"github.com/kilolockio/kilolock/pkg/store"
)

type lookupFunc func(ctx context.Context, id string) (store.EnvironmentRow, error)

func (f lookupFunc) GetEnvironmentByID(ctx context.Context, id string) (store.EnvironmentRow, error) {
	return f(ctx, id)
}

func TestRouterStoreFor_FailsClosedOnInactiveLifecycle(t *testing.T) {
	router := NewRouter(nil, lookupFunc(func(_ context.Context, _ string) (store.EnvironmentRow, error) {
		return store.EnvironmentRow{
			Slug:            "prod",
			LifecycleStatus: store.LifecycleStatusSuspended,
		}, nil
	}), nil)

	ctx := auth.WithPrincipal(context.Background(), auth.Principal{EnvironmentID: "env-1"})
	_, err := router.StoreFor(ctx)
	if err == nil {
		t.Fatalf("expected error for suspended environment")
	}
	if !errors.Is(err, ErrEnvironmentUnavailable) {
		t.Fatalf("expected ErrEnvironmentUnavailable, got %v", err)
	}
}

func TestRouterStoreFor_DefaultPoolWhenNoEnvironmentInContext(t *testing.T) {
	router := NewRouter(nil, lookupFunc(func(_ context.Context, _ string) (store.EnvironmentRow, error) {
		t.Fatal("lookup should not be called when no environment is present")
		return store.EnvironmentRow{}, nil
	}), nil)

	st, err := router.StoreFor(context.Background())
	if err != nil {
		t.Fatalf("StoreFor: %v", err)
	}
	if st == nil {
		t.Fatalf("expected store")
	}
}
