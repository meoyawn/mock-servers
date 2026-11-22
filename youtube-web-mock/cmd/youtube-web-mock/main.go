package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	youtubewebmock "github.com/meoyawn/mock-servers/youtube-web-mock"
)

const shutdownTimeout = 10 * time.Second

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	mock, err := youtubewebmock.Start(ctx, youtubewebmock.Config{
		Address: os.Getenv(
			"YOUTUBE_WEB_MOCK_ADDRESS",
		),
		CAFile:           os.Getenv("YOUTUBE_WEB_MOCK_CA_FILE"),
		FixtureDirectory: os.Getenv("YOUTUBE_WEB_MOCK_FIXTURE_DIRECTORY"),
	})
	if err != nil {
		fatal(err)
	}
	<-ctx.Done()
	closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), shutdownTimeout)
	defer cancel()
	if err := mock.Close(closeCtx); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	_, _ = fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
