package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	googleoauthmock "github.com/meoyawn/mock-servers/google-oauth-mock"
)

const readHeaderTimeout = 5 * time.Second

func main() {
	config := googleoauthmock.DefaultConfig()
	handler, err := googleoauthmock.New(config)
	if err != nil {
		slog.Error("configure Google OAuth mock", "error", err)
		os.Exit(1)
	}
	address := envOrDefault("GOOGLE_OAUTH_MOCK_ADDR", "127.0.0.1:8081")
	server := &http.Server{Addr: address, Handler: handler, ReadHeaderTimeout: readHeaderTimeout}
	slog.Info("Google OAuth mock listening", "address", address)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error(
			"Google OAuth mock stopped",
			"error",
			fmt.Errorf("serve on %q: %w", address, err),
		)
		os.Exit(1)
	}
}

func envOrDefault(name string, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
