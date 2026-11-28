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

func TestConfigFromArgs(t *testing.T) {
	t.Setenv("PHOTO_ROOT", "")
	t.Setenv("DATABASE_PATH", "")
	t.Setenv("INSTANCE_NAME", "Env")
	config, err := ConfigFromArgs([]string{"-name", "John's", "/photos"})
	if err != nil {
		t.Fatal(err)
	}
	if config.PhotoRoot != "/photos" || config.DatabasePath != "fast-fotos.db" || config.InstanceName != "John's" {
		t.Fatalf("unexpected config: %+v", config)
	}
	if config, err = ConfigFromArgs([]string{"/photos"}); err != nil || config.InstanceName != "Env" {
		t.Fatalf("environment instance name: %+v, %v", config, err)
	}
	t.Setenv("PHOTO_ROOT", "/env-photos")
	if config, err = ConfigFromArgs([]string{"/photos"}); err != nil || config.PhotoRoot != "/env-photos" {
		t.Fatalf("PHOTO_ROOT should win: %+v, %v", config, err)
	}
	t.Setenv("PHOTO_ROOT", "")
	for _, args := range [][]string{nil, {"a", "b"}, {"-bogus"}} {
		if _, err := ConfigFromArgs(args); err == nil {
			t.Fatalf("ConfigFromArgs(%v) succeeded, want an error", args)
		}
	}
}
