package main

import (
	"context"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"time"

	"github.com/andrewl/fast-fotos/internal/app"
)

// main initializes configuration, creates the application server, and starts the HTTP listener.
func main() {
	configureLogger(slog.LevelInfo)
	config, err := app.ConfigFromEnvironment()
	if err != nil {
		slog.Error("load configuration", "error", err)
		os.Exit(1)
	}
	configureLogger(config.LogLevel)

	server, err := app.NewServer(context.Background(), config)
	if err != nil {
		slog.Error("create application server", "error", err)
		os.Exit(1)
	}
	defer server.Close()

	http.Handle("/", server.Routes())

	httpServer := &http.Server{
		Addr:              config.ListenAddress,
		Handler:           http.DefaultServeMux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	slog.Info("fast-fotos listening", "address", config.ListenAddress)
	if err := httpServer.ListenAndServe(); err != nil {
		slog.Error("HTTP server stopped", "error", err)
		os.Exit(1)
	}
}

func configureLogger(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})))
}
