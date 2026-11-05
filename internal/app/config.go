package app

import (
	"errors"
	"log/slog"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Config holds configuration parameters for running the application.
type Config struct {
	DatabasePath          string
	PhotoRoot             string
	ListenAddress         string
	LogLevel              slog.Level
	IndexWorkers          int
	ThumbnailMaxDimension int
	ResizeFilter          string
	GeoNamesDir           string
	ObjectModelDir        string
	// ONNX thread counts; 0 uses the ONNX Runtime default.
	ONNXIntraOpThreads int
	ONNXInterOpThreads int
}

// ConfigFromEnvironment loads and validates application configuration settings from environment variables.
func ConfigFromEnvironment() (Config, error) {
	config := Config{
		DatabasePath:          os.Getenv("DATABASE_PATH"),
		PhotoRoot:             os.Getenv("PHOTO_ROOT"),
		ListenAddress:         os.Getenv("LISTEN_ADDR"),
		LogLevel:              slog.LevelInfo,
		IndexWorkers:          defaultIndexWorkers(),
		ThumbnailMaxDimension: defaultThumbnailMaxDimension,
		ResizeFilter:          "bilinear",
		GeoNamesDir:           os.Getenv("GEONAMES_DIR"),
		ObjectModelDir:        os.Getenv("OBJECT_MODEL_DIR"),
	}
	if config.DatabasePath == "" {
		config.DatabasePath = "fast-fotos.db"
	}
	if config.PhotoRoot == "" {
		return Config{}, errors.New("PHOTO_ROOT is required")
	}
	if config.ListenAddress == "" {
		config.ListenAddress = ":8090"
	}
	if value := os.Getenv("LOG_LEVEL"); value != "" {
		switch strings.ToLower(value) {
		case "debug":
			config.LogLevel = slog.LevelDebug
		case "info":
			config.LogLevel = slog.LevelInfo
		case "warn":
			config.LogLevel = slog.LevelWarn
		case "error":
			config.LogLevel = slog.LevelError
		default:
			return Config{}, errors.New("LOG_LEVEL must be debug, info, warn, or error")
		}
	}
	if value := os.Getenv("INDEX_WORKER"); value != "" {
		workers, err := strconv.Atoi(value)
		if err != nil || workers < 1 {
			return Config{}, errors.New("INDEX_WORKER must be a positive integer")
		}
		config.IndexWorkers = workers
	}
	if value := os.Getenv("THUMBNAIL_MAX_DIMENSION"); value != "" {
		dimension, err := strconv.Atoi(value)
		if err != nil || dimension < 1 {
			return Config{}, errors.New("THUMBNAIL_MAX_DIMENSION must be a positive integer")
		}
		config.ThumbnailMaxDimension = dimension
	}
	if value := os.Getenv("RESIZE_FILTER"); value != "" {
		config.ResizeFilter = strings.ToLower(value)
	}
	if config.ResizeFilter != "bilinear" && config.ResizeFilter != "approx-bilinear" && config.ResizeFilter != "catmull-rom" {
		return Config{}, errors.New("RESIZE_FILTER must be bilinear, approx-bilinear, or catmull-rom")
	}
	var err error
	if config.ONNXIntraOpThreads, err = threadsFromEnvironment("ONNX_INTRA_OP_THREADS"); err != nil {
		return Config{}, err
	}
	if config.ONNXInterOpThreads, err = threadsFromEnvironment("ONNX_INTER_OP_THREADS"); err != nil {
		return Config{}, err
	}
	return config, nil
}

// threadsFromEnvironment reads a non-negative thread count; unset means 0 (ONNX Runtime default).
func threadsFromEnvironment(name string) (int, error) {
	value := os.Getenv(name)
	if value == "" {
		return 0, nil
	}
	threads, err := strconv.Atoi(value)
	if err != nil || threads < 0 {
		return 0, errors.New(name + " must be a non-negative integer")
	}
	return threads, nil
}

// defaultIndexWorkers calculates the default number of concurrent indexing worker goroutines.
func defaultIndexWorkers() int {
	return min(4, max(1, runtime.GOMAXPROCS(0)))
}
