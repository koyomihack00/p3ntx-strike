//go:build linux

// Command claudeworker serves Claude Code headless calls to the claudecli provider
// adapter. It is not built into the PentAGI image and nothing starts it; see
// examples/claude-worker for review-only deployment files.
package main

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"pentagi/pkg/claudeworker"

	"github.com/sirupsen/logrus"
)

func main() {
	logrus.SetFormatter(&logrus.JSONFormatter{})
	if err := run(); err != nil {
		logrus.WithError(err).Error("claude worker stopped")
		os.Exit(1)
	}
}

func run() error {
	if err := claudeworker.CheckNotRoot(os.Geteuid()); err != nil {
		return err
	}
	if err := claudeworker.CheckEnvironment(os.LookupEnv); err != nil {
		return err
	}
	settings, err := claudeworker.SettingsFromEnv(os.LookupEnv)
	if err != nil {
		return err
	}
	executor, err := claudeworker.NewExecutor(settings.Config)
	if err != nil {
		return err
	}
	token, err := claudeworker.ReadSecretFile(settings.TokenFile)
	if err != nil {
		return err
	}
	handler, err := claudeworker.NewServer(executor, token, logrus.StandardLogger())
	if err != nil {
		return err
	}
	ln, err := claudeworker.Listen(settings.Listen)
	if err != nil {
		return err
	}

	srv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logrus.WithField("listen", settings.Listen).Info("claude worker listening")
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
