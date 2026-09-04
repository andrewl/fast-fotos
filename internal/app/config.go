package app

import (
	"errors"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Config holds configuration parameters for running the application.
type Config struct {
	DatabaseURL    string
	PhotoRoot      string
	ListenAddress  string
	IndexWorkers   int
	ResizeFilter   string
	GeoNamesDir    string
	ObjectModelDir string
}

// ConfigFromEnvironment loads and validates application configuration settings from environment variables.
func ConfigFromEnvironment() (Config, error) {
	config := Config{
		DatabaseURL:    os.Getenv("DATABASE_URL"),
		PhotoRoot:      os.Getenv("PHOTO_ROOT"),
		ListenAddress:  os.Getenv("LISTEN_ADDR"),
		IndexWorkers:   defaultIndexWorkers(),
		ResizeFilter:   "bilinear",
		GeoNamesDir:    os.Getenv("GEONAMES_DIR"),
		ObjectModelDir: os.Getenv("OBJECT_MODEL_DIR"),
	}
	if config.DatabaseURL == "" {
		return Config{}, errors.New("DATABASE_URL is required")
	}
	if config.PhotoRoot == "" {
		return Config{}, errors.New("PHOTO_ROOT is required")
	}
	if config.ListenAddress == "" {
		config.ListenAddress = ":8090"
	}
	if value := os.Getenv("INDEX_WORKER"); value != "" {
		workers, err := strconv.Atoi(value)
		if err != nil || workers < 1 {
			return Config{}, errors.New("INDEX_WORKER must be a positive integer")
		}
		config.IndexWorkers = workers
	}
	if value := os.Getenv("RESIZE_FILTER"); value != "" {
		config.ResizeFilter = strings.ToLower(value)
	}
	if config.ResizeFilter != "bilinear" && config.ResizeFilter != "approx-bilinear" && config.ResizeFilter != "catmull-rom" {
		return Config{}, errors.New("RESIZE_FILTER must be bilinear, approx-bilinear, or catmull-rom")
	}
	return config, nil
}

// defaultIndexWorkers calculates the default number of concurrent indexing worker goroutines.
func defaultIndexWorkers() int {
	return min(4, max(1, runtime.GOMAXPROCS(0)))
}
