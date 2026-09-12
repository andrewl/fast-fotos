package app

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// videoThumbnailMaxDimension bounds the largest side of a generated video thumbnail, matching photo thumbnails.
const videoThumbnailMaxDimension = thumbnailMaxDimension

var videoExtensions = map[string]bool{
	".3gp": true, ".avi": true, ".flv": true, ".m4v": true, ".mkv": true,
	".mov": true, ".mp4": true, ".mpeg": true, ".mpg": true, ".mts": true,
	".webm": true, ".wmv": true,
}

// iso6709Pattern matches ISO 6709 geographic coordinate strings such as "+40.6892-074.0445/" embedded in video metadata.
var iso6709Pattern = regexp.MustCompile(`^([+-]\d+(?:\.\d+)?)([+-]\d+(?:\.\d+)?)`)

// ffprobeFormat represents the subset of ffprobe's JSON output used to read video container metadata.
type ffprobeFormat struct {
	Format struct {
		Duration string            `json:"duration"`
		Tags     map[string]string `json:"tags"`
	} `json:"format"`
	Streams []struct {
		Tags map[string]string `json:"tags"`
	} `json:"streams"`
}

// readVideoMetadata extracts creation timestamp, GPS coordinates, and duration from a video file using ffprobe.
func readVideoMetadata(ctx context.Context, path string) (time.Time, *float64, *float64, *float64, error) {
	takenAt := time.Time{}

	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-print_format", "json", "-show_format", "-show_streams", path)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return takenAt, nil, nil, nil, fmt.Errorf("ffprobe %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}

	var probe ffprobeFormat
	if err := json.Unmarshal(stdout.Bytes(), &probe); err != nil {
		return takenAt, nil, nil, nil, fmt.Errorf("parse ffprobe output for %s: %w", path, err)
	}

	var duration *float64
	if probe.Format.Duration != "" {
		if seconds, err := strconv.ParseFloat(probe.Format.Duration, 64); err == nil {
			duration = &seconds
		}
	}

	tagSources := []map[string]string{probe.Format.Tags}
	for _, stream := range probe.Streams {
		tagSources = append(tagSources, stream.Tags)
	}

	for _, tags := range tagSources {
		if timestamp, ok := parseCreationTime(tags); ok && takenAt.IsZero() {
			takenAt = timestamp
		}
	}

	var latitude, longitude *float64
	for _, tags := range tagSources {
		if lat, lon, ok := parseLocationTags(tags); ok {
			latitude, longitude = &lat, &lon
			break
		}
	}

	return takenAt, latitude, longitude, duration, nil
}

// parseCreationTime searches tag values for a recognizable creation timestamp.
func parseCreationTime(tags map[string]string) (time.Time, bool) {
	for _, key := range []string{"creation_time", "com.apple.quicktime.creationdate"} {
		value, ok := tags[key]
		if !ok {
			continue
		}
		for _, layout := range []string{time.RFC3339, time.RFC3339Nano, "2006-01-02 15:04:05"} {
			if timestamp, err := time.Parse(layout, value); err == nil {
				return timestamp, true
			}
		}
	}
	return time.Time{}, false
}

// parseLocationTags searches tag values for an ISO 6709 location string and returns decoded latitude/longitude.
func parseLocationTags(tags map[string]string) (float64, float64, bool) {
	for _, key := range []string{"location", "com.apple.quicktime.location.ISO6709"} {
		value, ok := tags[key]
		if !ok {
			continue
		}
		if latitude, longitude, ok := parseISO6709(value); ok {
			return latitude, longitude, true
		}
	}
	return 0, 0, false
}

// parseISO6709 decodes an ISO 6709 coordinate prefix (e.g. "+40.6892-074.0445/") into latitude and longitude values.
func parseISO6709(value string) (float64, float64, bool) {
	matches := iso6709Pattern.FindStringSubmatch(value)
	if matches == nil {
		return 0, 0, false
	}
	latitude, err := strconv.ParseFloat(matches[1], 64)
	if err != nil {
		return 0, 0, false
	}
	longitude, err := strconv.ParseFloat(matches[2], 64)
	if err != nil {
		return 0, 0, false
	}
	return latitude, longitude, true
}

// createVideoThumbnail extracts a representative JPEG frame from a video file using ffmpeg, scaled to fit within videoThumbnailMaxDimension.
func createVideoThumbnail(ctx context.Context, path string) ([]byte, error) {
	scale := fmt.Sprintf("thumbnail,scale='min(%d,iw)':'min(%d,ih)':force_original_aspect_ratio=decrease", videoThumbnailMaxDimension, videoThumbnailMaxDimension)
	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-v", "error",
		"-i", path,
		"-vf", scale,
		"-frames:v", "1",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg thumbnail for %s: %w: %s", path, err, strings.TrimSpace(stderr.String()))
	}
	if stdout.Len() == 0 {
		return nil, nil
	}
	return stdout.Bytes(), nil
}
