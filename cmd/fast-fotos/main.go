package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	_ "net/http/pprof"
	"os"
	"path/filepath"
	"time"

	"github.com/andrewl/fast-fotos/internal/app"
)

// main initializes configuration, creates the application server, and starts the HTTP listener.
func main() {
	configureLogger(slog.LevelInfo)
	config, err := app.ConfigFromArgs(os.Args[1:])
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

// configureLogger sets the process-wide slog logger to write to stdout at level.
// Input: the minimum log level to emit; output: none.
func configureLogger(level slog.Level) {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{
		Level:     level,
		AddSource: true,
		ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.SourceKey {
				if source, ok := attr.Value.Any().(*slog.Source); ok {
					return slog.String(slog.SourceKey, fmt.Sprintf("%s:%d %s", filepath.Base(source.File), source.Line, source.Function))
				}
			}
			return attr
		},
	})))
}
