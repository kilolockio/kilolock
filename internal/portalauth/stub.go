package portalauth

import (
	"context"
	"fmt"
	"net/http"

	"github.com/davesade/kilolock/internal/config"
)

func FromRuntimeConfig(_ config.Config) map[string]any {
	return map[string]any{"enabled": false, "mode": "disabled"}
}

func BeginGoogleOIDC(http.ResponseWriter, *http.Request, config.Config, *http.Client) (string, error) {
	return "", fmt.Errorf("portal auth is not included in this OSS build")
}

func TrustedEmail(*http.Request, []string) string { return "" }

func CompleteGoogleOIDC(context.Context, *http.Request, config.Config, *http.Client) (string, error) {
	return "", fmt.Errorf("portal auth is not included in this OSS build")
}

func ClearOIDCCookies(http.ResponseWriter) {}
