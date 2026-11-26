package main

import (
	"io"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestConfigureLoggerIncludesSource(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	previousStdout, previousLogger := os.Stdout, slog.Default()
	os.Stdout = writer
	defer func() {
		os.Stdout = previousStdout
		slog.SetDefault(previousLogger)
	}()
	configureLogger(slog.LevelDebug)
	_, _, line, _ := runtime.Caller(0)
	slog.Debug("test source")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(output), "main_test.go:"+strconv.Itoa(line+1)) ||
		!strings.Contains(string(output), ".TestConfigureLoggerIncludesSource") {
		t.Fatalf("log missing caller function or line: %s", output)
	}
}
