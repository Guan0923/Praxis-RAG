// Command rag-service starts the standalone Praxis-RAG HTTP service: REST API,
// MCP Streamable HTTP endpoint, and Console static assets.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "praxis-rag: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if _, err := config.LoadStartupConfig(); err != nil {
		return err
	}
	serviceSettings, err := config.ServiceSettingsFromEnv()
	if err != nil {
		return err
	}
	ragConfig, err := config.RagConfigFromEnv()
	if err != nil {
		return err
	}
	stateSettings, err := config.StateSettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	service, err := runtime.New(ragConfig, stateSettings, serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	logger := newLogger()
	api := httpapi.New(serviceSettings, service, nil, logger)

	address := net.JoinHostPort(serviceSettings.Host, strconv.Itoa(serviceSettings.Port))
	httpServer := &http.Server{
		Addr:              address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	logger.Info("Praxis-RAG service starting",
		"version", version.Version,
		"address", address,
		"backend", string(ragConfig.Backend),
		"console", api.WebRoot(),
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveError := make(chan error, 1)
	go func() {
		serveError <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	logger.Info("Praxis-RAG service stopping")
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return err
	}
	if err := service.Close(shutdownContext); err != nil {
		return err
	}
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if isTruthy(os.Getenv("RAG_DEBUG_TRACE")) {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
