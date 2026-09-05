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

const thumbnailMaxDimension = 400

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
	if _, err := s.pool.Exec(ctx, "DELETE FROM index_state"); err != nil {
		return 0, fmt.Errorf("clear index state: %w", err)
	}
	reindexStartedAt := time.Now().UTC()
	count, err := s.indexPhotoPaths(ctx, nil, reindexStartedAt)
	if err != nil {
		return count, err
	}
	if _, err := s.pool.Exec(ctx, "DELETE FROM photos WHERE indexed_at < $1", reindexStartedAt); err != nil {
		return count, fmt.Errorf("remove stale photos: %w", err)
	}
	return count, nil
}

// indexPhotos performs an incremental index of new or updated photos since the last index timestamp.
func (s *Server) indexPhotos(ctx context.Context) (int, error) {
	lastIndexedAt, err := s.lastIndexedAt(ctx)
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
	if err := s.deriveNearbyLocations(ctx); err != nil {
		return count, fmt.Errorf("derive nearby locations: %w", err)
	}
	if err := s.recordIndexedAt(ctx, indexStartedAt); err != nil {
		return count, err
	}
	return count, nil
}

// indexPhoto extracts metadata, creates thumbnails, detects objects/colors, reverse geocodes, and saves a photo record to the database.
func (s *Server) indexPhoto(ctx context.Context, fullPath string, indexStartedAt time.Time) error {
	relativePath, err := filepath.Rel(s.config.PhotoRoot, fullPath)
	if err != nil {
		return err
	}
	rawPath, err := associatedRawPath(fullPath)
	if err != nil {
		return fmt.Errorf("find raw file for %s: %w", relativePath, err)
	}
	rawRelativePath := ""
	if rawPath != "" {
		rawRelativePath, err = filepath.Rel(s.config.PhotoRoot, rawPath)
		if err != nil {
			return fmt.Errorf("resolve raw file for %s: %w", relativePath, err)
		}
	}
	takenAt, latitude, longitude, cameraModel, focalLength, flashFired, err := readMetadata(fullPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", relativePath, err)
	}
	thumbnail, err := createThumbnail(fullPath, s.config.ResizeFilter)
	if err != nil {
		return fmt.Errorf("create thumbnail for %s: %w", relativePath, err)
	}
	location := ""
	settlement, region, country := "", "", ""
	if latitude != nil && longitude != nil && s.geocoder != nil {
		resolved, err := s.geocoder.ReverseGeocode(ctx, *latitude, *longitude)
		if err != nil {
			return fmt.Errorf("reverse geocode %s: %w", relativePath, err)
		}
		location = resolved.String()
		settlement, region, country = resolved.Settlement, resolved.Region, resolved.Country
	}
	var labels []string
	if s.detector != nil && thumbnail != nil {
		labels, err = s.detector.Detect(ctx, thumbnail)
		if err != nil {
			return fmt.Errorf("detect objects in %s: %w", relativePath, err)
		}
	}
	if labels == nil {
		labels = []string{}
	}
	var dominantColors []string
	if thumbnail != nil {
		dominantColors = colors.ExtractDominantColors(thumbnail, 3)
	}
	if dominantColors == nil {
		dominantColors = []string{}
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var photoID int64
	locationSource := ""
	if location != "" {
		locationSource = "extracted"
	}
	err = tx.QueryRow(ctx, `INSERT INTO photos (path, raw_path, taken_at, latitude, longitude, thumbnail, location, location_source, settlement, region, country, camera_model, focal_length, flash_fired, objects, dominant_colors, indexed_at)
		VALUES ($1, NULLIF($2, ''), $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, ''), NULLIF($10, ''), NULLIF($11, ''), NULLIF($12, ''), $13, $14, $15, $16, $17) ON CONFLICT (path) DO UPDATE
		SET taken_at = EXCLUDED.taken_at, latitude = EXCLUDED.latitude, longitude = EXCLUDED.longitude,
		raw_path = EXCLUDED.raw_path, thumbnail = COALESCE(EXCLUDED.thumbnail, photos.thumbnail), location = EXCLUDED.location, location_source = EXCLUDED.location_source,
			settlement = EXCLUDED.settlement, region = EXCLUDED.region, country = EXCLUDED.country,
			camera_model = EXCLUDED.camera_model, focal_length = EXCLUDED.focal_length,
			flash_fired = EXCLUDED.flash_fired, objects = EXCLUDED.objects, dominant_colors = EXCLUDED.dominant_colors, indexed_at = EXCLUDED.indexed_at
		RETURNING id`,
		relativePath, rawRelativePath, takenAt, latitude, longitude, thumbnail, location, locationSource, settlement, region, country,
		cameraModel, focalLength, flashFired, labels, dominantColors, indexStartedAt).Scan(&photoID)
	if err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return err
	}
	return nil
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
		if entry.IsDir() || !photoExtensions[strings.ToLower(filepath.Ext(entry.Name()))] {
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

// deriveNearbyLocations populates location data for un-geotagged photos taken within 1 hour of a geotagged photo.
func (s *Server) deriveNearbyLocations(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		WITH ranked_sources AS (
			SELECT target.id AS target_id,
				source.latitude, source.longitude, source.location,
				source.settlement, source.region, source.country,
				ROW_NUMBER() OVER (
					PARTITION BY target.id
					ORDER BY ABS(EXTRACT(EPOCH FROM (source.taken_at - target.taken_at))), source.id
				) AS rank
			FROM photos AS target
			JOIN photos AS source
				ON source.id <> target.id
				AND source.location IS NOT NULL AND source.location <> ''
				AND source.location_source = 'extracted'
				AND source.latitude IS NOT NULL AND source.longitude IS NOT NULL
				AND source.latitude <> 0 AND source.longitude <> 0
				AND source.taken_at BETWEEN target.taken_at - INTERVAL '1 hour'
					AND target.taken_at + INTERVAL '1 hour'
			WHERE (target.location IS NULL OR target.location = ''
					OR target.latitude IS NULL OR target.longitude IS NULL
					OR target.latitude = 0 OR target.longitude = 0)
				AND target.location_source IS DISTINCT FROM 'extracted'
		)
		UPDATE photos AS target
		SET latitude = source.latitude, longitude = source.longitude,
			location = source.location, location_source = 'derived',
			settlement = source.settlement, region = source.region, country = source.country
		FROM ranked_sources AS source
		WHERE source.rank = 1 AND source.target_id = target.id`)
	return err
}

// createThumbnail generates a JPEG thumbnail byte buffer resized to fit within thumbnailMaxDimension.
func createThumbnail(path, filterName string) ([]byte, error) {
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
	if width > thumbnailMaxDimension || height > thumbnailMaxDimension {
		scale := float64(thumbnailMaxDimension) / float64(max(width, height))
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
