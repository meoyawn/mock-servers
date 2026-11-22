package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	koanfenv "github.com/knadh/koanf/providers/env/v2"
	"github.com/knadh/koanf/v2"
	youtubemock "github.com/meoyawn/mock-servers/youtube-data-api-v3"
)

const (
	idleTimeout            = 30 * time.Second
	readHeaderTimeout      = 5 * time.Second
	shutdownTimeout        = 5 * time.Second
	defaultProcessingPolls = 2
)

type config struct {
	Address         string
	BearerToken     string
	EmptyPlaylists  bool
	ProcessingPolls int
}

func main() {
	if err := run(); err != nil {
		slog.Error("YouTube mock stopped", "error", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mockConfig := youtubemock.DefaultConfig()
	mockConfig.BearerToken = cfg.BearerToken
	mockConfig.ProcessingPollsUntilSuccess = cfg.ProcessingPolls
	if cfg.EmptyPlaylists {
		mockConfig.Playlists = nil
	}
	handler, err := youtubemock.New(mockConfig)
	if err != nil {
		return fmt.Errorf("configure YouTube mock: %w", err)
	}
	httpServer := &http.Server{
		Addr: cfg.Address, Handler: handler,
		ReadHeaderTimeout: readHeaderTimeout, IdleTimeout: idleTimeout,
	}
	serverError := make(chan error, 1)
	go func() {
		slog.Info("YouTube mock listening", "address", cfg.Address)
		serverError <- httpServer.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
		defer cancel()
		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("shut down YouTube mock: %w", err)
		}
		return nil
	case err := <-serverError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return fmt.Errorf("serve YouTube mock on %q: %w", cfg.Address, err)
	}
}

func loadConfig() (config, error) {
	k := koanf.New(".")
	defaults := map[string]any{
		"server.address": "127.0.0.1:8090", "server.bearer_token": "test-token",
		"state.empty_playlists": false, "state.processing_polls": defaultProcessingPolls,
	}
	for key, value := range defaults {
		if err := k.Set(key, value); err != nil {
			return config{}, fmt.Errorf("set default YouTube mock config %q: %w", key, err)
		}
	}
	provider := koanfenv.Provider(".", koanfenv.Opt{
		Prefix: "YOUTUBE_MOCK_",
		TransformFunc: func(key string, value string) (string, any) {
			path := strings.ToLower(strings.TrimPrefix(key, "YOUTUBE_MOCK_"))
			path = strings.ReplaceAll(path, "__", ".")
			switch defaults[path].(type) {
			case bool:
				parsed, err := strconv.ParseBool(value)
				if err == nil {
					return path, parsed
				}
			case int:
				parsed, err := strconv.Atoi(value)
				if err == nil {
					return path, parsed
				}
			}
			return path, value
		},
	})
	if err := k.Load(provider, nil); err != nil {
		return config{}, fmt.Errorf("load YouTube mock environment: %w", err)
	}
	result := config{
		Address:     k.String("server.address"),
		BearerToken: k.String("server.bearer_token"),
		EmptyPlaylists: k.Bool(
			"state.empty_playlists",
		),
		ProcessingPolls: k.Int("state.processing_polls"),
	}
	if result.Address == "" || result.BearerToken == "" || result.ProcessingPolls < 1 {
		return config{}, fmt.Errorf(
			"invalid YouTube mock server/state configuration: address=%q, processing polls=%d",
			result.Address,
			result.ProcessingPolls,
		)
	}
	return result, nil
}
