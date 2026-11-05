package app

import (
	"bytes"
	"context"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/andrewl/fast-fotos/internal/colors"
	"github.com/rwcarlsen/goexif/exif"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/tiff"
	_ "golang.org/x/image/webp"
)

const defaultThumbnailMaxDimension = 640

var photoExtensions = map[string]bool{
	".avif": true, ".gif": true, ".heic": true, ".jpeg": true, ".jpg": true,
	".png": true, ".tif": true, ".tiff": true, ".webp": true,
}

var rawExtensions = map[string]bool{
	".3fr": true, ".arw": true, ".cr2": true, ".cr3": true, ".dcr": true,
	".dng": true, ".erf": true, ".iiq": true, ".kdc": true, ".mef": true,
	".mos": true, ".mrw": true, ".nef": true, ".nrw": true, ".orf": true,
	".pef": true, ".raf": true, ".raw": true, ".rw2": true, ".rwl": true,
	".srw": true, ".x3f": true,
}

// reindexPhotos clears existing photo index state and performs a complete re-index of all photo paths.
func (s *Server) reindexPhotos(ctx context.Context) (int, error) {
	if err := s.db.ClearIndexState(ctx); err != nil {
		return 0, fmt.Errorf("clear index state: %w", err)
	}
	paths, err := photoPaths(s.config.PhotoRoot)
	if err != nil {
		return 0, err
	}
	existing := make([]string, 0, len(paths))
	for _, fullPath := range paths {
		relativePath, err := filepath.Rel(s.config.PhotoRoot, fullPath)
		if err != nil {
			return 0, err
		}
		existing = append(existing, relativePath)
	}
	removed, err := s.db.DeleteMissingPhotos(ctx, existing)
	if err != nil {
		return 0, fmt.Errorf("remove deleted photos: %w", err)
	}
	slog.Info("removed records for deleted files", "count", removed)
	return s.indexPhotoPaths(ctx, nil, time.Now().UTC())
}

// indexPhotos performs an incremental index of new or updated photos since the last index timestamp.
func (s *Server) indexPhotos(ctx context.Context) (int, error) {
	lastIndexedAt, err := s.db.LastIndexedAt(ctx)
	if err != nil {
		return 0, err
	}
	return s.indexPhotoPaths(ctx, lastIndexedAt, time.Now().UTC())
}

// indexPhotoPaths scans photo files and indexes them concurrently using worker goroutines.
func (s *Server) indexPhotoPaths(ctx context.Context, modifiedSince *time.Time, indexStartedAt time.Time) (int, error) {
	paths, err := photoPathsModifiedSince(s.config.PhotoRoot, modifiedSince)
	if err != nil {
		return 0, err
	}
	s.setIndexTotal(len(paths))

	type result struct {
		err error
	}
	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan string)
	results := make(chan result, s.config.IndexWorkers)
	var workers sync.WaitGroup
	for range s.config.IndexWorkers {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for fullPath := range jobs {
				if err := s.indexPhoto(workerCtx, fullPath, indexStartedAt); err != nil {
					cancel()
					results <- result{err: err}
					return
				}
				s.incrementIndexProgress()
				results <- result{}
			}
		}()
	}
	go func() {
		defer close(results)
		for _, fullPath := range paths {
			select {
			case jobs <- fullPath:
			case <-workerCtx.Done():
				close(jobs)
				workers.Wait()
				return
			}
		}
		close(jobs)
		workers.Wait()
	}()

	count := 0
	var firstErr error
	for result := range results {
		if result.err != nil && firstErr == nil {
			firstErr = result.err
		}
		if result.err == nil {
			count++
		}
	}
	if firstErr != nil {
		return count, firstErr
	}
	if err := ctx.Err(); err != nil {
		return count, err
	}
	if err := s.db.DeriveNearbyLocations(ctx); err != nil {
		return count, fmt.Errorf("derive nearby locations: %w", err)
	}
	// Golden hour needs a location, so it is computed after locations are derived.
	if _, err := s.db.UpdateGoldenHour(ctx); err != nil {
		return count, fmt.Errorf("update golden hour: %w", err)
	}
	if err := s.db.RecordIndexedAt(ctx, indexStartedAt); err != nil {
		return count, err
	}
	return count, nil
}

// indexPhoto extracts metadata, creates thumbnails, extracts colors, reverse geocodes, and saves a photo record to the database.
func (s *Server) indexPhoto(ctx context.Context, fullPath string, indexStartedAt time.Time) error {

	//we want to use defer here because some broken jpegs can panic the decoder
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("panic while indexing %s: %v", fullPath, r)
		}
	}()

	relativePath, err := filepath.Rel(s.config.PhotoRoot, fullPath)
	if err != nil {
		return err
	}
	slog.Info("indexing %s", relativePath, relativePath)
	isVideo := videoExtensions[strings.ToLower(filepath.Ext(fullPath))]

	rawRelativePath := ""
	var takenAt time.Time
	var dayOfYear int
	var latitude, longitude, focalLength, durationSeconds *float64
	var cameraModel string
	var flashFired *bool
	var thumbnail []byte
	mediaType := "photo"

	if isVideo {
		mediaType = "video"
		takenAt, latitude, longitude, durationSeconds, err = readVideoMetadata(ctx, fullPath)
		if err != nil {
		}
		if err == nil {
			dayOfYear = takenAt.YearDay()
		}
		thumbnail, err = createVideoThumbnail(ctx, fullPath, s.config.ThumbnailMaxDimension)
		if err != nil {
			slog.Warn("create video thumbnail for %s: %v", relativePath, err)
		}
	} else {
		rawPath, err := associatedRawPath(fullPath)
		if err == nil {
			if rawPath != "" {
				rawRelativePath, _ = filepath.Rel(s.config.PhotoRoot, rawPath)
				if err != nil {
					slog.Warn("resolve raw file for %s: %v", relativePath, err)
				}
			}
		}
		takenAt, latitude, longitude, cameraModel, focalLength, flashFired, err = readMetadata(fullPath)
		if err != nil {
			slog.Warn("read metadata for %s: %v", relativePath, err)
		} else {
			dayOfYear = takenAt.YearDay()
		}
		thumbnail, err = createThumbnail(fullPath, s.config.ResizeFilter, s.config.ThumbnailMaxDimension)
		if err != nil {
			slog.Warn("create thumbnail for %s: %v", relativePath, err)
		}
	}
	location := ""
	settlement, region, country := "", "", ""
	if latitude != nil && longitude != nil && s.geocoder != nil {
		resolved, err := s.geocoder.ReverseGeocode(ctx, *latitude, *longitude)
		if err != nil {
			slog.Warn("reverse geocode for %s: %v", relativePath, err)
		}
		location = resolved.String()
		settlement, region, country = resolved.Settlement, resolved.Region, resolved.Country
	}
	var dominantColors []string
	if thumbnail != nil {
		dominantColors = colors.ExtractDominantColors(thumbnail, 3)
	}
	if dominantColors == nil {
		dominantColors = []string{}
	}
	locationSource := ""
	if location != "" {
		locationSource = "extracted"
	}
	return s.db.UpsertPhoto(ctx, PhotoRecord{
		Path: relativePath, RawPath: rawRelativePath, TakenAt: takenAt, Latitude: latitude, Longitude: longitude,
		Thumbnail: thumbnail, Location: location, LocationSource: locationSource,
		Settlement: settlement, Region: region, Country: country,
		CameraModel: cameraModel, FocalLength: focalLength, FlashFired: flashFired,
		DominantColors: dominantColors, MediaType: mediaType,
		DurationSeconds: durationSeconds, IndexedAt: indexStartedAt, DayOfYear: dayOfYear,
	})
}

// photoPaths traverses root directory and returns all supported photo file paths.
func photoPaths(root string) ([]string, error) {
	return photoPathsModifiedSince(root, nil)
}

// photoPathsModifiedSince returns supported photo file paths in root modified after modifiedSince timestamp.
func photoPathsModifiedSince(root string, modifiedSince *time.Time) ([]string, error) {
	var paths []string
	err := filepath.WalkDir(root, func(fullPath string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		ext := strings.ToLower(filepath.Ext(entry.Name()))
		if entry.IsDir() || !(photoExtensions[ext] || videoExtensions[ext]) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if modifiedSince == nil || info.ModTime().After(*modifiedSince) {
			paths = append(paths, fullPath)
		} else if rawPath, err := associatedRawPath(fullPath); err != nil {
			return err
		} else if rawPath != "" {
			rawInfo, err := os.Stat(rawPath)
			if err != nil {
				return err
			}
			if rawInfo.ModTime().After(*modifiedSince) {
				paths = append(paths, fullPath)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("find photos: %w", err)
	}
	return paths, nil
}

// associatedRawPath checks the image directory for a raw photo file matching the image base filename.
func associatedRawPath(imagePath string) (string, error) {
	base := strings.TrimSuffix(filepath.Base(imagePath), filepath.Ext(imagePath))
	entries, err := os.ReadDir(filepath.Dir(imagePath))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if entry.IsDir() || strings.TrimSuffix(entry.Name(), filepath.Ext(entry.Name())) != base {
			continue
		}
		if rawExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
			return filepath.Join(filepath.Dir(imagePath), entry.Name()), nil
		}
	}
	return "", nil
}

// readMetadata extracts EXIF header information (timestamp, GPS, camera model, focal length, flash status) from an image file.
func readMetadata(path string) (time.Time, *float64, *float64, string, *float64, *bool, error) {
	//default taken at is 0000-01-01 00:00:00 UTC
	takenAt := time.Time{}

	file, err := os.Open(path)
	if err != nil {
		return time.Time{}, nil, nil, "", nil, nil, err
	}
	defer file.Close()
	metadata, err := exif.Decode(file)
	if err != nil {
		return takenAt, nil, nil, "", nil, nil, nil
	}

	if timestamp, err := metadata.DateTime(); err == nil {
		takenAt = timestamp
	}
	latitude, longitude, err := metadata.LatLong()
	var cameraModel string
	if tag, tagErr := metadata.Get(exif.Model); tagErr == nil {
		if value, valueErr := tag.StringVal(); valueErr == nil {
			cameraModel = strings.TrimSpace(value)
		}
	}
	var focalLength *float64
	if tag, tagErr := metadata.Get(exif.FocalLength); tagErr == nil {
		if numerator, denominator, ratErr := tag.Rat2(0); ratErr == nil && denominator != 0 {
			value := float64(numerator) / float64(denominator)
			focalLength = &value
		}
	}
	var flashFired *bool
	if tag, tagErr := metadata.Get(exif.Flash); tagErr == nil {
		if value, intErr := tag.Int(0); intErr == nil {
			fired := value&1 != 0
			flashFired = &fired
		}
	}
	if err == nil {
		if latitude == 0 && longitude == 0 {
			return takenAt, nil, nil, cameraModel, focalLength, flashFired, nil
		}
		return takenAt, &latitude, &longitude, cameraModel, focalLength, flashFired, nil
	}

	return takenAt, nil, nil, cameraModel, focalLength, flashFired, nil
}

// createThumbnail generates a JPEG thumbnail byte buffer resized to fit within maxDimension.
func createThumbnail(path, filterName string, maxDimension int) ([]byte, error) {
	if _, err := resizeFilter(filterName); err != nil {
		return nil, err
	}
	if thumbnail, err := embeddedJPEGThumbnail(path); err != nil {
		return nil, err
	} else if thumbnail != nil {
		return thumbnail, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	source, _, err := image.Decode(file)
	if err != nil {
		// Index formats which are browser-viewable but unsupported by Go's decoders
		// without discarding an existing thumbnail.
		return nil, nil
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil, fmt.Errorf("image has invalid dimensions %dx%d", width, height)
	}
	if width > maxDimension || height > maxDimension {
		scale := float64(maxDimension) / float64(max(width, height))
		width = int(float64(width)*scale + 0.5)
		height = int(float64(height)*scale + 0.5)
	}

	thumbnail := image.NewRGBA(image.Rect(0, 0, width, height))
	filter, _ := resizeFilter(filterName)
	filter.Scale(thumbnail, thumbnail.Bounds(), source, bounds, draw.Over, nil)
	var output bytes.Buffer
	if err := jpeg.Encode(&output, thumbnail, &jpeg.Options{Quality: 85}); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

// embeddedJPEGThumbnail attempts to extract a pre-rendered JPEG thumbnail directly from EXIF metadata.
func embeddedJPEGThumbnail(path string) ([]byte, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg":
	default:
		return nil, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	metadata, err := exif.Decode(file)
	if err != nil {
		return nil, nil
	}
	thumbnail, err := metadata.JpegThumbnail()
	if err != nil || len(thumbnail) == 0 {
		return nil, nil
	}
	if _, format, err := image.DecodeConfig(bytes.NewReader(thumbnail)); err != nil || format != "jpeg" {
		return nil, nil
	}
	return thumbnail, nil
}

// resizeFilter maps a filter name string to an image scaling Interpolator.
func resizeFilter(name string) (draw.Interpolator, error) {
	switch name {
	case "", "bilinear":
		return draw.BiLinear, nil
	case "approx-bilinear":
		return draw.ApproxBiLinear, nil
	case "catmull-rom":
		return draw.CatmullRom, nil
	default:
		return nil, fmt.Errorf("unknown resize filter %q", name)
	}
}

// detectObjects runs object detection on photos that have not been processed yet, until none remain or ctx is cancelled.
func (s *Server) detectObjects(ctx context.Context) {
	total, err := s.db.PendingDetectionCount(ctx)
	if err != nil {
		if ctx.Err() == nil {
			slog.Error("count photos pending detection", "error", err)
		}
		return
	}
	s.updateDetectionProgress(0, total)

	var afterID int64
	detected := 0
	for {
		batch, err := s.db.NextPendingDetections(ctx, afterID, 50)
		if err != nil {
			if ctx.Err() == nil {
				slog.Error("list photos pending detection", "error", err)
			}
			return
		}
		if len(batch) == 0 {
			if detected > 0 {
				slog.Info("object detection finished", "count", detected)
			}
			return
		}
		for _, p := range batch {
			if ctx.Err() != nil {
				slog.Info("object detection interrupted", "count", detected)
				return
			}
			afterID = p.ID
			labels := []string{}
			if len(p.Thumbnail) > 0 {
				found, err := s.detector.Detect(ctx, p.Thumbnail)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					slog.Warn("object detection failed", "path", p.Path, "error", err)
					continue
				}
				if found != nil {
					labels = found
				}
			}
			if err := s.db.SetDetectedObjects(ctx, p.ID, labels); err != nil {
				if ctx.Err() == nil {
					slog.Error("save detected objects", "path", p.Path, "error", err)
				}
				return
			}
			detected++
			s.updateDetectionProgress(detected, total)
		}
	}
}
