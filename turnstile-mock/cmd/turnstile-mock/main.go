package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	turnstilemock "github.com/meoyawn/mock-servers/turnstile-mock"
)

const (
	defaultAddress  = ":8085"
	healthcheckURL  = "http://127.0.0.1:8085/healthz"
	requestTimeout  = 5 * time.Second
	shutdownTimeout = 10 * time.Second
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			_, _ = fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	if err := run(); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func healthcheck() error {
	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		healthcheckURL,
		nil,
	)
	if err != nil {
		return fmt.Errorf("build Turnstile mock healthcheck: %w", err)
	}
	client := &http.Client{Timeout: requestTimeout}
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("request Turnstile mock healthcheck: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return fmt.Errorf(
			"turnstile mock healthcheck status = %d, want %d",
			response.StatusCode,
			http.StatusNoContent,
		)
	}
	return nil
}

func run() error {
	addr := os.Getenv("TURNSTILE_MOCK_ADDR")
	if addr == "" {
		addr = defaultAddress
	}
	server, err := turnstilemock.NewServer(addr)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(
		context.Background(),
		syscall.SIGINT,
		syscall.SIGTERM,
	)
	defer stop()
	done := make(chan error, 1)
	go func() {
		done <- server.ListenAndServe()
	}()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve Turnstile mock: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(
			context.Background(),
			shutdownTimeout,
		)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shutdown Turnstile mock: %w", err)
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
