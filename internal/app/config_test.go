package app

import (
	"log/slog"
	"testing"
)

func TestConfigLogLevel(t *testing.T) {
	t.Setenv("PHOTO_ROOT", t.TempDir())

	for _, test := range []struct {
		name      string
		value     string
		want      slog.Level
		wantError bool
	}{
		{name: "default", want: slog.LevelInfo},
		{name: "debug", value: "debug", want: slog.LevelDebug},
		{name: "case insensitive", value: "WARN", want: slog.LevelWarn},
		{name: "error", value: "error", want: slog.LevelError},
		{name: "invalid", value: "verbose", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("LOG_LEVEL", test.value)
			config, err := ConfigFromEnvironment()
			if test.wantError {
				if err == nil {
					t.Fatal("ConfigFromEnvironment() succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnvironment(): %v", err)
			}
			if config.LogLevel != test.want {
				t.Errorf("LogLevel = %s, want %s", config.LogLevel, test.want)
			}
		})
	}
}

func TestConfigThumbnailMaxDimension(t *testing.T) {
	t.Setenv("PHOTO_ROOT", t.TempDir())

	for _, test := range []struct {
		name      string
		value     string
		want      int
		wantError bool
	}{
		{name: "default", want: 640},
		{name: "configured", value: "720", want: 720},
		{name: "zero", value: "0", wantError: true},
		{name: "invalid", value: "large", wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("THUMBNAIL_MAX_DIMENSION", test.value)
			config, err := ConfigFromEnvironment()
			if test.wantError {
				if err == nil {
					t.Fatal("ConfigFromEnvironment() succeeded, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ConfigFromEnvironment(): %v", err)
			}
			if config.ThumbnailMaxDimension != test.want {
				t.Errorf("ThumbnailMaxDimension = %d, want %d", config.ThumbnailMaxDimension, test.want)
			}
		})
	}
}
