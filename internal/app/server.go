package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"database/sql"
	"log/slog"

	"github.com/andrewl/fast-fotos/internal/geocode"
	"github.com/andrewl/fast-fotos/internal/objects"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*
var staticFiles embed.FS

// Server represents the web application server instance, managing database connections, templates, geocoding, and object detection.
type Server struct {
	config          Config
	pool            *pgxpool.Pool
	templates       map[string]*template.Template
	indexProgressMu sync.RWMutex
	indexProgress   IndexProgress
	indexCancel     context.CancelFunc
	geocoder        geocode.Provider
	detector        objects.Detector
}

// LocationGroup represents a named geographical area (settlement or region) and its photo count.
type LocationGroup struct {
	Name  string
	Kind  string
	Count int
}

// MapBounds represents the bounding box coordinates for map views.
type MapBounds struct {
	MinLatitude  float64
	MinLongitude float64
	MaxLatitude  float64
	MaxLongitude float64
}

// IndexProgress tracks background photo indexing state and completion percentage.
type IndexProgress struct {
	Active     bool   `json:"active"`
	Processed  int    `json:"processed"`
	Total      int    `json:"total"`
	Percentage int    `json:"percentage"`
	Message    string `json:"message"`
}

type Photo struct {
	ID             int64
	Path           string
	RawPath        string
	TakenAt        time.Time
	Latitude       *float64
	Longitude      *float64
	Location       string
	Settlement     string
	Region         string
	Country        string
	CameraModel    sql.NullString
	FocalLength    *float64
	FlashFired     *bool
	Objects        []string
	DominantColors []string
}

// Month represents a calendar month grouping key and photo count.
type Month struct {
	Key   string
	Count int
}

// Collection represents a custom photo collection.
type Collection struct {
	ID    int64
	Name  string
	Count int
}

// PageData contains all contextual view data passed to HTML templates for rendering.
type PageData struct {
	Page               string
	Photos             []Photo
	Photo              *Photo
	Months             []Month
	Collections          []Collection
	SelectedMonth      string
	Collection            *Collection
	PhotoRoot          string
	Locations          []LocationGroup
	SelectedLocation   string
	SelectedSettlement string
	SelectedRegion     string
	MapBounds          *MapBounds
	MapReturnURL       string
	Search             SearchData
	ReturnURL          string
	CollectionName     string
	PreviousPhotoID    int64
	NextPhotoID        int64
}

// SearchData holds filter parameters and dropdown values for photo search options.
type SearchData struct {
	DateFrom, DateTo, Label, CameraModel, FocalLength, Flash, Location, Color string
	CameraModels                                                              []string
	Locations                                                                 []SearchLocation
	Labels                                                                    []string
	FocalLengths                                                              []string
	Colors                                                                    []string
	Settlement                                                                string
	Region                                                                    string
}

// SearchLocation represents a searchable geographical location option.
type SearchLocation struct {
	Value      string
	Label      string
	Settlement string
	Region     string
}

// mapPoint represents a clustered map marker point with coordinate boundaries and photo metadata.
type mapPoint struct {
	Latitude     float64 `json:"latitude"`
	Longitude    float64 `json:"longitude"`
	Count        int     `json:"count"`
	PhotoID      int64   `json:"photoId"`
	MinLatitude  float64 `json:"minLatitude"`
	MinLongitude float64 `json:"minLongitude"`
	MaxLatitude  float64 `json:"maxLatitude"`
	MaxLongitude float64 `json:"maxLongitude"`
	Path         string  `json:"path,omitempty"`
	TakenAt      string  `json:"takenAt,omitempty"`
	Location     string  `json:"location,omitempty"`
}

// NewServer connects to PostgreSQL, runs database migrations, parses templates, and returns an initialized Server.
func NewServer(ctx context.Context, config Config) (*Server, error) {
	pool, err := pgxpool.New(ctx, config.DatabaseURL)
	if err != nil {
		slog.Error("connect to database", "error", err)
		return nil, fmt.Errorf("connect to database: %w", err)
	}
	server := &Server{config: config, pool: pool}
	if config.GeoNamesDir != "" {
		server.geocoder, err = geocode.LoadGeoNamesProvider(config.GeoNamesDir)
		if err != nil {
			slog.Error("load GeoNames data", "error", err)
			pool.Close()
			return nil, fmt.Errorf("load GeoNames data: %w", err)
		}
	}
	if config.ObjectModelDir != "" {
		server.detector, err = objects.LoadONNXDetector(config.ObjectModelDir)
		if err != nil {
			slog.Error("load object detector", "error", err)
			pool.Close()
			return nil, fmt.Errorf("load object detector: %w", err)
		}
	}
	if err := server.migrate(ctx); err != nil {
		slog.Error("run database migrations", "error", err)
		pool.Close()
		return nil, err
	}
	server.templates, err = parseTemplates()
	if err != nil {
		slog.Error("parse templates", "error", err)
		pool.Close()
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	return server, nil
}

// Close closes underlying database pools and releases object detector resources.
func (s *Server) Close() {
	if detector, ok := s.detector.(interface{ Close() error }); ok {
		_ = detector.Close()
	}
	s.pool.Close()
}

// Routes constructs and returns an http.Handler with all application endpoints configured.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(mustSub(staticFiles, "static"))))
	mux.HandleFunc("GET /", s.home)
	mux.HandleFunc("GET /timeline/", s.timeline)
	mux.HandleFunc("GET /image/", s.image)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /search/", s.search)
	mux.HandleFunc("GET /maintenance", s.maintenance)
	mux.HandleFunc("GET /months/", s.month)
	mux.HandleFunc("GET /locations", s.locations)
	mux.HandleFunc("GET /cluster", s.cluster)
	mux.HandleFunc("GET /api/map-points", s.mapPoints)
	mux.HandleFunc("GET /api/map-cluster", s.mapCluster)
	mux.HandleFunc("GET /collections", s.allCollectionsPage)
	mux.HandleFunc("GET /collection/", s.collectionPage)
	mux.HandleFunc("POST /collections", s.addToCollection)
	mux.HandleFunc("POST /index", s.index)
	mux.HandleFunc("POST /reindex", s.reindex)
	mux.HandleFunc("POST /stop-indexing", s.stopIndexing)
	mux.HandleFunc("GET /index-progress", s.indexProgressStatus)
	mux.HandleFunc("GET /thumbnails/", s.thumbnailFile)
	mux.HandleFunc("GET /photos/", s.photoFile)
	mux.HandleFunc("GET /raw/", s.rawFile)
	mux.HandleFunc("GET /download/", s.download)
	return mux
}

// mustSub returns an embedded sub-filesystem or panics on error.
func mustSub(files embed.FS, directory string) fs.FS {
	sub, err := fs.Sub(files, directory)
	if err != nil {
		slog.Error("create sub filesystem", "error", err)
		panic(err)
	}
	return sub
}

// migrate creates and updates application database tables, columns, and indexes.
func (s *Server) migrate(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS photos (
			id BIGSERIAL PRIMARY KEY,
			path TEXT UNIQUE NOT NULL,
			raw_path TEXT,
			taken_at TIMESTAMPTZ NOT NULL,
			latitude DOUBLE PRECISION,
			longitude DOUBLE PRECISION,
			thumbnail BYTEA,
			location TEXT,
			location_source TEXT,
			settlement TEXT,
			region TEXT,
			country TEXT,
			camera_model TEXT,
			focal_length DOUBLE PRECISION,
			flash_fired BOOLEAN,
			objects TEXT[] NOT NULL DEFAULT '{}',
			dominant_colors TEXT[] NOT NULL DEFAULT '{}',
			indexed_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS thumbnail BYTEA;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS raw_path TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS location TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS location_source TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS settlement TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS region TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS country TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS camera_model TEXT;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS focal_length DOUBLE PRECISION;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS flash_fired BOOLEAN;
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS objects TEXT[] NOT NULL DEFAULT '{}';
		ALTER TABLE photos ADD COLUMN IF NOT EXISTS dominant_colors TEXT[] NOT NULL DEFAULT '{}';
		CREATE TABLE IF NOT EXISTS index_state (
			id BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
			last_indexed_at TIMESTAMPTZ NOT NULL
		);
		CREATE TABLE IF NOT EXISTS collections (
			id BIGSERIAL PRIMARY KEY,
			name TEXT UNIQUE NOT NULL CHECK (length(trim(name)) > 0),
			created_at TIMESTAMPTZ NOT NULL DEFAULT now()
		);
		CREATE TABLE IF NOT EXISTS collection_photos (
			collection_id BIGINT NOT NULL REFERENCES collections(id) ON DELETE CASCADE,
			photo_id BIGINT NOT NULL REFERENCES photos(id) ON DELETE CASCADE,
			PRIMARY KEY (collection_id, photo_id)
		);
		CREATE INDEX IF NOT EXISTS photos_taken_at_idx ON photos (taken_at);
		CREATE INDEX IF NOT EXISTS photos_location_idx ON photos (latitude, longitude)
			WHERE latitude IS NOT NULL AND longitude IS NOT NULL;`)
	if err != nil {
		slog.Error("migrate database", "error", err)
		return fmt.Errorf("migrate database: %w", err)
	}
	return nil
}

// lastIndexedAt queries the timestamp of the last completed photo indexing operation.
func (s *Server) lastIndexedAt(ctx context.Context) (*time.Time, error) {
	var lastIndexedAt time.Time
	err := s.pool.QueryRow(ctx, "SELECT last_indexed_at FROM index_state WHERE id = TRUE").Scan(&lastIndexedAt)
	if err == nil {
		return &lastIndexedAt, nil
	}
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	return nil, fmt.Errorf("get last indexed time: %w", err)
}

// recordIndexedAt updates or inserts the timestamp of the latest successful indexing run.
func (s *Server) recordIndexedAt(ctx context.Context, indexedAt time.Time) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO index_state (id, last_indexed_at) VALUES (TRUE, $1)
		ON CONFLICT (id) DO UPDATE SET last_indexed_at = EXCLUDED.last_indexed_at`, indexedAt)
	if err != nil {
		slog.Error("record indexed time", "error", err)
		return fmt.Errorf("record indexed time: %w", err)
	}
	return nil
}

// home renders the root homepage showing the latest month's photo timeline.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	months, err := s.months(r.Context())
	if err != nil {
		slog.Error("Failed to load months", "error", err)
		http.Error(w, "Could not load photo library", http.StatusInternalServerError)
		return
	}

	var photos []Photo
	selectedMonth := ""
	if len(months) > 0 {
		selectedMonth = months[0].Key
		photos, err = s.photosForMonth(r.Context(), months[0].Key)
		if err != nil {
			slog.Error("Failed to load photos for month", "month", months[0].Key, "error", err)
			http.Error(w, "Could not load photos", http.StatusInternalServerError)
			return
		}
	}
	galleries, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "home", PageData{Months: months, Collections: galleries, Photos: photos, SelectedMonth: selectedMonth, PhotoRoot: s.config.PhotoRoot, ReturnURL: "/"})
}

// timeline renders the photo gallery for a specific month (e.g. /timeline/2026-08).
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	month := strings.TrimPrefix(r.URL.Path, "/timeline/")
	if len(month) == 7 && month[4] == '/' {
		month = month[:4] + "-" + month[5:]
	}
	if !validMonth(month) {
		http.NotFound(w, r)
		return
	}
	months, err := s.months(r.Context())
	if err != nil {
		slog.Error("Failed to load months", "error", err)
		http.Error(w, "Could not load photo library", http.StatusInternalServerError)
		return
	}
	photos, err := s.photosForMonth(r.Context(), month)
	if err != nil {
		slog.Error("Failed to load photos for month", "month", month, "error", err)
		http.Error(w, "Could not load photos", http.StatusInternalServerError)
		return
	}
	galleries, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "home", PageData{Page: "home", Months: months, Collections: galleries, Photos: photos, SelectedMonth: month, PhotoRoot: s.config.PhotoRoot, ReturnURL: r.URL.Path})
}

// image renders the detail page for a single photo including EXIF metadata and navigation links.
func (s *Server) image(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/image/"), 10, 64)
	if err != nil || id < 1 {
		slog.Error("Invalid photo ID", "id", r.URL.Path, "error", err)
		http.NotFound(w, r)
		return
	}
	photo, err := s.photoByID(r.Context(), id)
	if err != nil {
		slog.Error("Failed to load photo", "id", id, "error", err)
		http.NotFound(w, r)
		return
	}
	from := r.URL.Query().Get("from")
	if from == "" {
		from = timelinePath(photo.TakenAt.Format("2006-01"))
	}
	returnURL, collectionName, collection, err := s.imageGalleryContextPhotos(r.Context(), from, s.selectionToken(w, r))
	if err != nil {
		slog.Error("Failed to load image collection", "from", from, "error", err)
		http.NotFound(w, r)
		return
	}
	previousID, nextID := adjacentPhotoIDs(collection, id)
	galleries, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "image", PageData{Page: "image", Photo: &photo, Collections: galleries, ReturnURL: returnURL, CollectionName: collectionName, PreviousPhotoID: previousID, NextPhotoID: nextID})
}

// search renders the photo search form and filtered search result thumbnail grid.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if strings.HasPrefix(r.URL.Path, "/search/") {
		criteria := strings.TrimPrefix(r.URL.Path, "/search/")
		decoded, err := url.PathUnescape(criteria)
		if err != nil {
			slog.Error("Failed to decode search criteria", "criteria", criteria, "error", err)
			http.NotFound(w, r)
			return
		}

		query, err = url.ParseQuery(decoded)
		if err != nil {
			slog.Error("Failed to parse search criteria", "criteria", decoded, "error", err)
			http.NotFound(w, r)
			return
		}
	}
	search := SearchData{
		DateFrom: query.Get("from"), DateTo: query.Get("to"), Label: query.Get("label"),
		CameraModel: query.Get("camera"), FocalLength: query.Get("focal"), Flash: query.Get("flash"),
		Location: query.Get("location"), Color: query.Get("color"),
	}

	if strings.HasPrefix(search.Location, "settlement:") {
		search.Settlement = strings.TrimPrefix(search.Location, "settlement:")
	} else if strings.HasPrefix(search.Location, "region:") {
		search.Region = strings.TrimPrefix(search.Location, "region:")
	} else if search.Location != "" {
		slog.Error("Invalid location filter", "location", search.Location)
		http.Error(w, "Invalid location filter", http.StatusBadRequest)
		return
	}
	for _, value := range []string{search.DateFrom, search.DateTo} {
		if value != "" {
			if _, err := time.Parse("2006-01-02", value); err != nil {
				slog.Error("Invalid search date", "date", value, "error", err)
				http.Error(w, "Invalid search date", http.StatusBadRequest)
				return
			}
		}
	}
	if search.FocalLength != "" {
		focalLength, err := strconv.ParseFloat(search.FocalLength, 64)
		if err != nil || focalLength < 0 {
			slog.Error("Invalid focal length", "focal_length", search.FocalLength, "error", err)
			http.Error(w, "Invalid focal length", http.StatusBadRequest)
			return
		}
	}
	if search.Flash != "" && search.Flash != "yes" && search.Flash != "no" {
		slog.Error("Invalid flash filter", "flash", search.Flash)
		http.Error(w, "Invalid flash filter", http.StatusBadRequest)
		return
	}

	photos := []Photo{}
	//we must have at lease one search criteria to avoid returning the entire photo library
	if search.DateFrom == "" && search.DateTo == "" && search.Label == "" && search.CameraModel == "" && search.FocalLength == "" && search.Flash == "" && search.Location == "" && search.Color == "" {
		slog.Info("No search criteria provided")
	} else {
		var err error
		photos, err = s.searchPhotos(r.Context(), search)
		if err != nil {
			slog.Error("Failed to search photos", "error", err)
			http.Error(w, "Could not search photos", http.StatusInternalServerError)
			return
		}
	}
	var err error
	if search.CameraModels, err = s.distinctValues(r.Context(), `SELECT DISTINCT camera_model FROM photos WHERE camera_model IS NOT NULL AND camera_model <> '' ORDER BY camera_model`); err != nil {
		slog.Error("Failed to load camera models", "error", err)
		http.Error(w, "Could not load camera models", http.StatusInternalServerError)
		return
	}
	if search.Locations, err = s.searchLocations(r.Context()); err != nil {
		slog.Error("Failed to load locations", "error", err)
		http.Error(w, "Could not load locations", http.StatusInternalServerError)
		return
	}
	if search.FocalLengths, err = s.distinctValues(r.Context(), `SELECT DISTINCT focal_length::text FROM photos WHERE focal_length IS NOT NULL ORDER BY focal_length`); err != nil {
		slog.Error("Failed to load focal lengths", "error", err)
		http.Error(w, "Could not load focal lengths", http.StatusInternalServerError)
		return
	}
	if search.Labels, err = s.distinctValues(r.Context(), `SELECT DISTINCT unnest(objects) FROM photos ORDER BY 1`); err != nil {
		slog.Error("Failed to load labels", "error", err)
		http.Error(w, "Could not load labels", http.StatusInternalServerError)
		return
	}
	if search.Colors, err = s.distinctValues(r.Context(), `SELECT DISTINCT unnest(dominant_colors) FROM photos ORDER BY 1`); err != nil {
		slog.Error("Failed to load colors", "error", err)
		http.Error(w, "Could not load colors", http.StatusInternalServerError)
		return
	}

	galleries, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "search", PageData{Page: "search", Photos: photos, Collections: galleries, Search: search, ReturnURL: r.URL.RequestURI()})
}

// selectionToken reads or sets an HTTP-only session cookie for managing photo selections.
func (s *Server) selectionToken(w http.ResponseWriter, r *http.Request) string {
	const cookieName = "fast-fotos-selection"
	if cookie, err := r.Cookie(cookieName); err == nil && cookie.Value != "" {
		return cookie.Value
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		slog.Error("Failed to generate selection token", "error", err)
		http.Error(w, "Could not create selection session", http.StatusInternalServerError)
		return ""
	}
	token := fmt.Sprintf("%x", tokenBytes)
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: token, Path: "/", HttpOnly: true, SameSite: http.SameSiteLaxMode})
	return token
}

// imageGalleryContextPhotos loads context photos and display labels for navigating between photos in a gallery.
func (s *Server) imageGalleryContextPhotos(ctx context.Context, from, selectionToken string) (string, string, []Photo, error) {
	if from == "" {
		slog.Info("No gallery URL provided, defaulting to timeline")
		return "/", "Timeline", nil, nil
	}
	galleryURL, err := url.Parse(from)
	if err != nil || galleryURL.IsAbs() || galleryURL.Host != "" {
		slog.Error("Invalid collection URL", "url", from, "error", err)
		return "", "", nil, errors.New("invalid collection URL")
	}
	switch {
	case galleryURL.Path == "/":
		slog.Info("Loading timeline", "query", galleryURL.Query())
		months, err := s.months(ctx)
		if err != nil || len(months) == 0 {
			return "/", "Timeline", nil, err
		}
		photos, err := s.photosForMonth(ctx, months[0].Key)
		return "/", monthLabel(months[0].Key), photos, err
	case strings.HasPrefix(galleryURL.Path, "/timeline/"):
		slog.Info("Loading timeline month", "month", galleryURL.Path)
		month := strings.TrimPrefix(galleryURL.Path, "/timeline/")
		if len(month) == 7 && month[4] == '/' {
			month = month[:4] + "-" + month[5:]
		}
		if !validMonth(month) {
			return "", "", nil, errors.New("invalid timeline month")
		}
		photos, err := s.photosForMonth(ctx, month)
		return galleryURL.RequestURI(), monthLabel(month), photos, err
	case galleryURL.Path == "/cluster":
		slog.Info("Loading map cluster", "query", galleryURL.Query())
		photos, err := s.clusterPhotos(ctx, galleryURL.Query())
		return galleryURL.RequestURI(), "Map cluster", photos, err
	case strings.HasPrefix(galleryURL.Path, "/collections/"):
		slog.Info("Loading collection", "collectionId", galleryURL.Path)
		collectionID, err := strconv.ParseInt(strings.TrimPrefix(galleryURL.Path, "/collections/"), 10, 64)
		if err != nil || collectionID < 1 {
			slog.Error("Invalid collection ID", "collectionId", galleryURL.Path, "error", err)
			return "", "", nil, errors.New("invalid collection")
		}
		photos, err := s.photosForCollection(ctx, collectionID)
		var name string
		err = s.pool.QueryRow(ctx, `SELECT name from collections WHERE id = $1`, collectionID).Scan(&name)
		if err != nil {
			slog.Error("Failed to load collection name", "collectionId", collectionID, "error", err)
		}
		return galleryURL.Path, name, photos, err
	case galleryURL.Path == "/search" || strings.HasPrefix(galleryURL.Path, "/search/"):
		slog.Info("Loading search results", "query", galleryURL.Query())
		query := galleryURL.Query()
		if strings.HasPrefix(galleryURL.Path, "/search/") {
			criteria := strings.TrimPrefix(galleryURL.Path, "/search/")
			decoded, err := url.PathUnescape(criteria)
			if err != nil {
				return "", "", nil, err
			}
			query, err = url.ParseQuery(decoded)
			if err != nil {
				return "", "", nil, err
			}
		}
		search := SearchData{
			DateFrom: query.Get("from"), DateTo: query.Get("to"), Label: query.Get("label"),
			CameraModel: query.Get("camera"), FocalLength: query.Get("focal"), Flash: query.Get("flash"),
			Location: query.Get("location"),
		}
		if strings.HasPrefix(search.Location, "settlement:") {
			search.Settlement = strings.TrimPrefix(search.Location, "settlement:")
		} else if strings.HasPrefix(search.Location, "region:") {
			search.Region = strings.TrimPrefix(search.Location, "region:")
		} else if search.Location != "" {
			return "", "", nil, errors.New("invalid location filter")
		}
		photos, err := s.searchPhotos(ctx, search)
		return galleryURL.RequestURI(), "Search results", photos, err
	default:
		slog.Error("Unsupported collection URL", "url", galleryURL.Path)
		return "", "", nil, errors.New("unsupported collection URL")
	}
}

// adjacentPhotoIDs returns the IDs of the photos directly preceding and following currentID in a slice.
func adjacentPhotoIDs(photos []Photo, currentID int64) (int64, int64) {
	for index, photo := range photos {
		if photo.ID != currentID {
			continue
		}
		var previousID, nextID int64
		if index > 0 {
			previousID = photos[index-1].ID
		}
		if index+1 < len(photos) {
			nextID = photos[index+1].ID
		}
		return previousID, nextID
	}
	return 0, 0
}

// maintenance renders the system management page for indexing control and status monitoring.
func (s *Server) maintenance(w http.ResponseWriter, r *http.Request) {
	galleries, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "maintenance", PageData{Page: "maintenance", Collections: galleries})
}

// distinctValues executes a SQL query returning a slice of distinct string column values.
func (s *Server) distinctValues(ctx context.Context, query string) ([]string, error) {
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return nil, err
	}

	defer rows.Close()
	var values []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

// searchLocations loads distinct settlement and region filter choices for the search view.
func (s *Server) searchLocations(ctx context.Context) ([]SearchLocation, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT 'settlement:' || settlement, settlement,
		concat_ws(', ', settlement, region, country), settlement, region
		FROM photos WHERE settlement IS NOT NULL AND settlement <> ''
		UNION
		SELECT DISTINCT 'region:' || region, region, concat_ws(', ', region, country), '', region
		FROM photos WHERE region IS NOT NULL AND region <> ''
	ORDER BY 3`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var locations []SearchLocation
	for rows.Next() {
		var location SearchLocation
		var rawValue string
		if err := rows.Scan(&location.Value, &rawValue, &location.Label, &location.Settlement, &location.Region); err != nil {
			return nil, err
		}
		locations = append(locations, location)
	}
	return locations, rows.Err()
}

// searchPhotos queries the photos table with dynamic filters defined in SearchData.
func (s *Server) searchPhotos(ctx context.Context, search SearchData) ([]Photo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, path, COALESCE(raw_path, ''), taken_at, latitude, longitude, COALESCE(location, ''),
		camera_model, focal_length, flash_fired, objects, dominant_colors
		FROM photos
		WHERE ($1 = '' OR taken_at >= $1::date)
		AND ($2 = '' OR taken_at < ($2::date + INTERVAL '1 day'))
		AND ($3 = '' OR $3 = ANY(objects))
		AND ($4 = '' OR camera_model = $4)
		AND ($5 = '' OR focal_length = NULLIF($5, '')::double precision)
		AND ($6 = '' OR ($6 = 'yes' AND flash_fired = TRUE) OR ($6 = 'no' AND flash_fired = FALSE))
		AND ($7 = '' OR settlement = $7)
		AND ($8 = '' OR region = $8)
		AND ($9 = '' OR $9 = ANY(dominant_colors))
		ORDER BY taken_at ASC, id ASC`,
		search.DateFrom, search.DateTo, search.Label, search.CameraModel,
		search.FocalLength, search.Flash, search.Settlement, search.Region, search.Color)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPhotos(rows)
}

// month renders the photo grid HTML template partial for a given month.
func (s *Server) month(w http.ResponseWriter, r *http.Request) {
	month := strings.TrimPrefix(r.URL.Path, "/months/")
	if !validMonth(month) {
		slog.Error("Invalid month format", "month", month)
		http.Error(w, "Invalid month", http.StatusBadRequest)
		return
	}
	photos, err := s.photosForMonth(r.Context(), month)
	if err != nil {
		slog.Error("Failed to load photos for month", "month", month, "error", err)
		http.Error(w, "Could not load photos", http.StatusInternalServerError)
		return
	}
	s.render(w, "gallery", PageData{Photos: photos, SelectedMonth: month})
}

// locations renders the interactive Leaflet map and location selection sidebar.
func (s *Server) locations(w http.ResponseWriter, r *http.Request) {
	locationGroups, err := s.locationGroups(r.Context())
	if err != nil {
		slog.Error("Failed to load locations", "error", err)
		http.Error(w, "Could not load locations", http.StatusInternalServerError)
		return
	}

	selectedSettlement := r.URL.Query().Get("settlement")
	selectedRegion := r.URL.Query().Get("region")
	var bounds *MapBounds
	if selectedSettlement != "" || selectedRegion != "" {
		bounds, err = s.locationBounds(r.Context(), selectedSettlement, selectedRegion)
		if err != nil {
			slog.Error("Failed to load location bounds", "settlement", selectedSettlement, "region", selectedRegion, "error", err)
			http.Error(w, "Could not load location bounds", http.StatusInternalServerError)
			return
		}
	}
	s.render(w, "locations", PageData{Page: "locations", Locations: locationGroups,
		SelectedLocation: selectedSettlement + selectedRegion, SelectedSettlement: selectedSettlement,
		SelectedRegion: selectedRegion, MapBounds: bounds, MapReturnURL: r.URL.RequestURI()})
}

// cluster renders the grid view for photos contained within a specific map cluster bounding box.
func (s *Server) cluster(w http.ResponseWriter, r *http.Request) {
	photos, err := s.clusterPhotos(r.Context(), r.URL.Query())
	if err != nil {
		slog.Error("Failed to load cluster photos", "error", err)
		http.Error(w, "Invalid cluster", http.StatusBadRequest)
		return
	}
	mapReturnURL := r.URL.Query().Get("map")
	if parsed, err := url.Parse(mapReturnURL); err != nil || parsed.IsAbs() || parsed.Host != "" || !strings.HasPrefix(parsed.Path, "/locations") {
		mapReturnURL = "/locations"
	}
	s.render(w, "cluster", PageData{Page: "cluster", Photos: photos, ReturnURL: r.URL.RequestURI(), MapReturnURL: mapReturnURL})
}

// locationGroups queries all distinct settlement and region names with photo counts.
func (s *Server) locationGroups(ctx context.Context) ([]LocationGroup, error) {
	rows, err := s.pool.Query(ctx, `SELECT 'settlement', settlement, COUNT(*)::int FROM photos
		WHERE settlement IS NOT NULL AND settlement <> '' GROUP BY settlement
		UNION ALL
		SELECT 'region', region, COUNT(*)::int FROM photos
		WHERE region IS NOT NULL AND region <> '' GROUP BY region
	ORDER BY 2`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []LocationGroup
	for rows.Next() {
		var group LocationGroup
		if err := rows.Scan(&group.Kind, &group.Name, &group.Count); err != nil {
			return nil, err
		}
		groups = append(groups, group)
	}
	return groups, rows.Err()
}

// locationBounds calculates geographical bounding box coordinates for photos matching settlement or region filters.
func (s *Server) locationBounds(ctx context.Context, settlement, region string) (*MapBounds, error) {
	var bounds MapBounds
	err := s.pool.QueryRow(ctx, `SELECT MIN(latitude), MIN(longitude), MAX(latitude), MAX(longitude)
		FROM photos WHERE ($1 = '' OR settlement = $1) AND ($2 = '' OR region = $2)
		AND latitude IS NOT NULL AND longitude IS NOT NULL`, settlement, region).
		Scan(&bounds.MinLatitude, &bounds.MinLongitude, &bounds.MaxLatitude, &bounds.MaxLongitude)
	if err != nil {
		return nil, err
	}
	return &bounds, nil
}

// mapPoints responds with JSON spatial cluster points aggregated by map grid cells for the current zoom level.
func (s *Server) mapPoints(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	minLatitude, err := strconv.ParseFloat(query.Get("minLat"), 64)
	if err != nil {
		slog.Error("Invalid minimum latitude", "error", err)
		http.Error(w, "Invalid minimum latitude", http.StatusBadRequest)
		return
	}
	maxLatitude, err := strconv.ParseFloat(query.Get("maxLat"), 64)
	if err != nil {
		slog.Error("Invalid maximum latitude", "error", err)
		http.Error(w, "Invalid maximum latitude", http.StatusBadRequest)
		return
	}
	minLongitude, err := strconv.ParseFloat(query.Get("minLng"), 64)
	if err != nil {
		slog.Error("Invalid minimum longitude", "error", err)
		http.Error(w, "Invalid minimum longitude", http.StatusBadRequest)
		return
	}
	maxLongitude, err := strconv.ParseFloat(query.Get("maxLng"), 64)
	if err != nil {
		slog.Error("Invalid maximum longitude", "error", err)
		http.Error(w, "Invalid maximum longitude", http.StatusBadRequest)
		return
	}
	zoom, err := strconv.Atoi(query.Get("zoom"))
	if err != nil || zoom < 0 || zoom > 22 || minLatitude < -90 || maxLatitude > 90 ||
	minLongitude < -180 || maxLongitude > 180 || minLatitude >= maxLatitude || minLongitude >= maxLongitude {

		slog.Error("Invalid map bounds or zoom level", "minLat", minLatitude, "maxLat", maxLatitude,
			"minLng", minLongitude, "maxLng", maxLongitude, "zoom", zoom, "error", err)
		http.Error(w, "Invalid map bounds", http.StatusBadRequest)
		return
	}
	cellSize := 360 / math.Pow(2, float64(zoom+2))
	settlement := query.Get("settlement")
	region := query.Get("region")
	rows, err := s.pool.Query(r.Context(), `SELECT AVG(latitude), AVG(longitude), COUNT(*)::int, MIN(id),
		MIN(latitude), MIN(longitude), MAX(latitude), MAX(longitude)
		FROM photos
		WHERE latitude BETWEEN $1 AND $2 AND longitude BETWEEN $3 AND $4
		AND ($6 = '' OR settlement = $6) AND ($7 = '' OR region = $7)
		GROUP BY floor((latitude + 90) / $5::double precision), floor((longitude + 180) / $5::double precision)
		ORDER BY MIN(id) LIMIT 10000`, minLatitude, maxLatitude, minLongitude, maxLongitude, cellSize, settlement, region)
	if err != nil {
		slog.Error("Failed to load map points", "minLat", minLatitude, "maxLat", maxLatitude,
			"minLng", minLongitude, "maxLng", maxLongitude, "zoom", zoom, "settlement", settlement, "region", region, "error", err)
		http.Error(w, "Could not load map points", http.StatusInternalServerError)
		return
	}

	defer rows.Close()
	points := make([]mapPoint, 0)
	for rows.Next() {
		var point mapPoint
		if err := rows.Scan(&point.Latitude, &point.Longitude, &point.Count, &point.PhotoID,
			&point.MinLatitude, &point.MinLongitude, &point.MaxLatitude, &point.MaxLongitude); err != nil {
			slog.Error("Failed to scan map point", "error", err)
			http.Error(w, "Could not load map points", http.StatusInternalServerError)
			return
		}
		points = append(points, point)
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating map points", "error", err)
		http.Error(w, "Could not load map points", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=30")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(points); err != nil {
		slog.Error("Failed to encode map points", "error", err)
		http.Error(w, "Could not encode map points", http.StatusInternalServerError)
	}
}

// mapCluster responds with JSON metadata for all photos located within specified cluster map bounds.
func (s *Server) mapCluster(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	minLatitude, err := strconv.ParseFloat(query.Get("minLat"), 64)
	if err != nil || minLatitude < -90 || minLatitude > 90 {
		slog.Error("Invalid cluster minimum latitude", "error", err)
		http.Error(w, "Invalid cluster minimum latitude", http.StatusBadRequest)
		return
	}
	maxLatitude, err := strconv.ParseFloat(query.Get("maxLat"), 64)
	if err != nil || maxLatitude < -90 || maxLatitude > 90 || minLatitude > maxLatitude {
		slog.Error("Invalid cluster maximum latitude", "error", err)
		http.Error(w, "Invalid cluster maximum latitude", http.StatusBadRequest)
		return
	}
	minLongitude, err := strconv.ParseFloat(query.Get("minLng"), 64)
	if err != nil || minLongitude < -180 || minLongitude > 180 {
		slog.Error("Invalid cluster minimum longitude", "error", err)
		http.Error(w, "Invalid cluster minimum longitude", http.StatusBadRequest)
		return
	}
	maxLongitude, err := strconv.ParseFloat(query.Get("maxLng"), 64)
	if err != nil || maxLongitude < -180 || maxLongitude > 180 || minLongitude > maxLongitude {
		slog.Error("Invalid cluster maximum longitude", "error", err)
		http.Error(w, "Invalid cluster maximum longitude", http.StatusBadRequest)
		return
	}
	rows, err := s.pool.Query(r.Context(), `SELECT id, latitude, longitude, path,
		to_char(taken_at, 'YYYY-MM-DD"T"HH24:MI:SSZ'), COALESCE(location, '')
		FROM photos
		WHERE latitude BETWEEN $1 AND $2 AND longitude BETWEEN $3 AND $4
		ORDER BY taken_at ASC, id ASC LIMIT 10000`, minLatitude, maxLatitude, minLongitude, maxLongitude)
	if err != nil {
		slog.Error("Failed to load cluster photos", "minLat", minLatitude, "maxLat", maxLatitude,
			"minLng", minLongitude, "maxLng", maxLongitude, "error", err)
		http.Error(w, "Could not load cluster photos", http.StatusInternalServerError)
		return
	}

	defer rows.Close()
	photos := make([]mapPoint, 0)
	for rows.Next() {
		var photo mapPoint
		if err := rows.Scan(&photo.PhotoID, &photo.Latitude, &photo.Longitude, &photo.Path, &photo.TakenAt, &photo.Location); err != nil {
			slog.Error("Failed to scan cluster photo", "error", err)
			http.Error(w, "Could not read cluster photos", http.StatusInternalServerError)
			return
		}
		photos = append(photos, photo)
	}
	if err := rows.Err(); err != nil {
		slog.Error("Error iterating cluster photos", "error", err)
		http.Error(w, "Could not read cluster photos", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Cache-Control", "private, max-age=30")
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(photos); err != nil {
		slog.Error("Failed to encode cluster photos", "error", err)
		http.Error(w, "Could not encode cluster photos", http.StatusInternalServerError)
	}
}

// clusterPhotos queries full Photo records lying within specified map cluster bounding box coordinates.
func (s *Server) clusterPhotos(ctx context.Context, query url.Values) ([]Photo, error) {
	minLatitude, maxLatitude, minLongitude, maxLongitude, err := clusterBounds(query)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id, path, COALESCE(raw_path, ''), taken_at, latitude, longitude, COALESCE(location, ''),
		camera_model, focal_length, flash_fired, objects, dominant_colors
		FROM photos
		WHERE latitude BETWEEN $1 AND $2 AND longitude BETWEEN $3 AND $4
		ORDER BY taken_at ASC, id ASC LIMIT 10000`, minLatitude, maxLatitude, minLongitude, maxLongitude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPhotos(rows)
}

// clusterBounds parses bounding box coordinate query parameters minLat, maxLat, minLng, maxLng.
func clusterBounds(query url.Values) (float64, float64, float64, float64, error) {
	minLatitude, err := strconv.ParseFloat(query.Get("minLat"), 64)
	if err != nil || minLatitude < -90 || minLatitude > 90 {
		return 0, 0, 0, 0, errors.New("invalid cluster minimum latitude")
	}
	maxLatitude, err := strconv.ParseFloat(query.Get("maxLat"), 64)
	if err != nil || maxLatitude < -90 || maxLatitude > 90 || minLatitude > maxLatitude {
		return 0, 0, 0, 0, errors.New("invalid cluster maximum latitude")
	}
	minLongitude, err := strconv.ParseFloat(query.Get("minLng"), 64)
	if err != nil || minLongitude < -180 || minLongitude > 180 {
		return 0, 0, 0, 0, errors.New("invalid cluster minimum longitude")
	}
	maxLongitude, err := strconv.ParseFloat(query.Get("maxLng"), 64)
	if err != nil || maxLongitude < -180 || maxLongitude > 180 || minLongitude > maxLongitude {
		return 0, 0, 0, 0, errors.New("invalid cluster maximum longitude")
	}
	return minLatitude, maxLatitude, minLongitude, maxLongitude, nil
}

// galleriesPage renders the custom galleries list page or redirects to the first gallery.
func (s *Server) allCollectionsPage(w http.ResponseWriter, r *http.Request) {
	collections, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load collections", "error", err)
		http.Error(w, "Could not load collections", http.StatusInternalServerError)
		return
	}
	if len(collections) > 0 {
		slog.Info("Redirecting to first collection", "collectionID", collections[0].ID)
		http.Redirect(w, r, fmt.Sprintf("/collection/%d", collections[0].ID), http.StatusSeeOther)
		return
	}
	s.render(w, "collections", PageData{Collections: collections})
}

// gallery renders the detail grid page for a collection
func (s *Server) collectionPage(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, "/collection/"), 10, 64)
	if err != nil || id < 1 {
		slog.Error("Invalid collection ID", "error", err)
		http.NotFound(w, r)
		return
	}
	var collection Collection 
	if err := s.pool.QueryRow(r.Context(), `SELECT g.id, g.name, COUNT(gp.photo_id)::int
		from collections g LEFT JOIN collection_photos gp ON gp.collection_id = g.id
		WHERE g.id = $1 GROUP BY g.id`, id).Scan(&collection.ID, &collection.Name, &collection.Count); err != nil {
		slog.Error("Failed to load collection", "collection_id", id, "error", err)
		http.NotFound(w, r)
		return
	}
	photos, err := s.photosForCollection(r.Context(), id)
	if err != nil {
		slog.Error("Failed to load collection photos", "collection_id", id, "error", err)
		http.Error(w, "Could not load collection", http.StatusInternalServerError)
		return
	}
	collections, err := s.collections(r.Context())
	if err != nil {
		slog.Error("Failed to load collections", "error", err)
		http.Error(w, "Could not load collections", http.StatusInternalServerError)
		return
	}
	s.render(w, "collection", PageData{Collections: collections, Collection: &collection, Photos: photos, ReturnURL: r.URL.Path})
}

// addToCollection adds selected photo IDs to an existing or newly created custom gallery.
func (s *Server) addToCollection(w http.ResponseWriter, r *http.Request) {
	ids := r.FormValue("ids")
	if ids == "" {
		slog.Error("No photo IDs provided for gallery addition")
		http.Error(w, "Select at least one photo", http.StatusBadRequest)
		return
	}
	galleryID, err := s.selectCollection(r.Context(), r.FormValue("collection_id"), r.FormValue("name"))
	if err != nil {
		slog.Error("Failed to select or create gallery", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	tag, err := s.pool.Exec(r.Context(), `INSERT INTO collection_photos (collection_id, photo_id)
		SELECT $1, id FROM photos WHERE id = ANY(string_to_array($2, ',')::bigint[])
		ON CONFLICT DO NOTHING`, galleryID, ids)
	if err != nil {
		slog.Error("Failed to add photos to gallery", "collection_id", galleryID, "error", err)
		http.Error(w, "Could not add photos to gallery", http.StatusInternalServerError)
		return
	}
	if tag.RowsAffected() == 0 {
		slog.Error("No selected photos could be added to the gallery", "collection_id", galleryID)
		http.Error(w, "No selected photos could be added to the gallery", http.StatusBadRequest)
		return
	}
	w.Header().Set("HX-Redirect", fmt.Sprintf("/collections/%d", galleryID))
	w.WriteHeader(http.StatusNoContent)
}

// selectCollection validates an existing gallery ID or creates a new named gallery record.
func (s *Server) selectCollection(ctx context.Context, existingID, name string) (int64, error) {
	if existingID != "" {
		id, err := strconv.ParseInt(existingID, 10, 64)
		if err != nil || id < 1 {
			return 0, fmt.Errorf("invalid collection")
		}
		var found bool
		if err := s.pool.QueryRow(ctx, "SELECT EXISTS(SELECT 1 from collections WHERE id = $1)", id).Scan(&found); err != nil {
			return 0, fmt.Errorf("look up collections: %w", err)
		}
		if !found {
			return 0, fmt.Errorf("collection does not exist")
		}
		return id, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, fmt.Errorf("enter a new collection name or choose an existing collection")
	}
	if len(name) > 200 {
		return 0, fmt.Errorf("collection name must be 200 characters or fewer")
	}
	var id int64
	if err := s.pool.QueryRow(ctx, "INSERT INTO collections (name) VALUES ($1) RETURNING id", name).Scan(&id); err != nil {
		return 0, fmt.Errorf("create collection: %w", err)
	}
	return id, nil
}

// index triggers an incremental photo indexing HTTP endpoint request.
func (s *Server) index(w http.ResponseWriter, r *http.Request) {
	if !s.startIndexing(s.indexPhotos) {
		slog.Error("Indexing request rejected because another indexing operation is already in progress")
		http.Error(w, "Indexing is already in progress", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// reindex triggers a complete photo re-index HTTP endpoint request.
func (s *Server) reindex(w http.ResponseWriter, r *http.Request) {
	if !s.startIndexing(s.reindexPhotos) {
		slog.Error("Reindexing request rejected because another indexing operation is already in progress")
		http.Error(w, "Indexing is already in progress", http.StatusConflict)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// startIndexing launches a background indexing task if another run is not actively processing.
func (s *Server) startIndexing(index func(context.Context) (int, error)) bool {
	ctx, started := s.beginIndexing(context.Background())
	if !started {
		return false
	}
	go func() {
		count, err := index(ctx)
		s.finishIndexing(count, err)
	}()
	slog.Info("Started background indexing operation")
	return true
}

// stopIndexing cancels any currently active background indexing operation context.
func (s *Server) stopIndexing(w http.ResponseWriter, _ *http.Request) {
	s.indexProgressMu.Lock()
	defer s.indexProgressMu.Unlock()
	if s.indexCancel == nil {
		slog.Error("Stop indexing request rejected because no indexing operation is currently in progress")
		http.Error(w, "No indexing is in progress", http.StatusConflict)
		return
	}
	slog.Info("Cancelling background indexing operation")
	s.indexCancel()
	w.WriteHeader(http.StatusNoContent)
}

// indexProgressStatus responds with a JSON representation of current indexing progress.
func (s *Server) indexProgressStatus(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.currentIndexProgress()); err != nil {
		http.Error(w, "Could not encode indexing progress", http.StatusInternalServerError)
	}
}

// beginIndexing thread-safely checks and initializes indexing context and status.
func (s *Server) beginIndexing(parent context.Context) (context.Context, bool) {
	s.indexProgressMu.Lock()
	defer s.indexProgressMu.Unlock()
	if s.indexCancel != nil {
		return nil, false
	}
	ctx, cancel := context.WithCancel(parent)
	s.indexCancel = cancel
	s.indexProgress = IndexProgress{Active: true, Message: "Indexing: preparing files..."}
	return ctx, true
}

// setIndexTotal sets the total count of photo paths to be indexed.
func (s *Server) setIndexTotal(total int) {
	s.indexProgressMu.Lock()
	defer s.indexProgressMu.Unlock()
	s.indexProgress.Total = total
	s.indexProgress.Percentage = percentage(s.indexProgress.Processed, total)
}

// incrementIndexProgress increments the count of processed photo items.
func (s *Server) incrementIndexProgress() {
	s.indexProgressMu.Lock()
	defer s.indexProgressMu.Unlock()
	s.indexProgress.Processed++
	s.indexProgress.Percentage = percentage(s.indexProgress.Processed, s.indexProgress.Total)
}

// finishIndexing updates state upon completion or error of the indexing process.
func (s *Server) finishIndexing(count int, err error) {
	s.indexProgressMu.Lock()
	defer s.indexProgressMu.Unlock()
	s.indexCancel = nil
	s.indexProgress.Active = false
	switch {
	case errors.Is(err, context.Canceled):
		s.indexProgress.Message = "Indexing stopped"
	case err != nil:
		s.indexProgress.Message = fmt.Sprintf("Indexing failed: %v", err)
	default:
		s.indexProgress.Message = fmt.Sprintf("%d photos indexed", count)
	}
}

// currentIndexProgress returns a copy of current IndexProgress.
func (s *Server) currentIndexProgress() IndexProgress {
	s.indexProgressMu.RLock()
	defer s.indexProgressMu.RUnlock()
	return s.indexProgress
}

// percentage calculates integer percentage processed of total.
func percentage(processed, total int) int {
	if total == 0 {
		return 0
	}
	return processed * 100 / total
}

// photoFile serves full-size photo image files from disk.
func (s *Server) photoFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/photos/")
	var path string
	err := s.pool.QueryRow(r.Context(), "SELECT path FROM photos WHERE id = $1", id).Scan(&path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.config.PhotoRoot, path))
}

// rawFile serves raw camera photo files as attachment downloads.
func (s *Server) rawFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/raw/")
	var rawPath string
	if err := s.pool.QueryRow(r.Context(), "SELECT raw_path FROM photos WHERE id = $1", id).Scan(&rawPath); err != nil || rawPath == "" {
		http.NotFound(w, r)
		return
	}
	fullPath := filepath.Join(s.config.PhotoRoot, rawPath)

	root, err := filepath.Abs(s.config.PhotoRoot)
	if err != nil {
		slog.Error("Could not resolve photo directory: ", "err", err)
		http.Error(w, "Could not resolve photo directory", http.StatusInternalServerError)
		return
	}
	fullPath, err = filepath.Abs(fullPath)
	if err != nil {
		slog.Error("Could not resolve raw file: ", "err", err)
		http.Error(w, "Could not resolve raw file", http.StatusInternalServerError)
		return
	}
	relative, err := filepath.Rel(root, fullPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		http.NotFound(w, r)
		return
	}
	if _, err := os.Stat(fullPath); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(filepath.Base(rawPath)))
	http.ServeFile(w, r, fullPath)
}

// thumbnailFile serves cached 400px JPEG thumbnails for photos.
func (s *Server) thumbnailFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/thumbnails/")
	var thumbnail []byte
	var path string
	err := s.pool.QueryRow(r.Context(), "SELECT thumbnail, path FROM photos WHERE id = $1", id).Scan(&thumbnail, &path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if len(thumbnail) == 0 {
		http.ServeFile(w, r, filepath.Join(s.config.PhotoRoot, path))
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	http.ServeContent(w, r, path+".jpg", time.Time{}, bytes.NewReader(thumbnail))
}



// download streams a zip archive containing photos from a specific collection and optional raw files.
// the photo ids are passed in the query string as a comma-separated list, e.g., /download/abc123?ids=1,2,3
func (s *Server) download(w http.ResponseWriter, r *http.Request) {

	//get the ids from the ids query parameter, e.g., /download/abc123?ids=1,2,3 would have ids "1,2,3"
	//and confirm each entry is an integer, otherwise return a 400 error
	photoIDs := strings.Split(r.URL.Query().Get("ids"), ",")

	if len(photoIDs) == 0 {
		slog.Error("Download request rejected because no photo ids were provided","ids", r.URL.Query().Get("ids"))
		http.Error(w, "No photo ids provided", http.StatusBadRequest)
		return
	}

	for _, id := range photoIDs {
		if _, err := strconv.Atoi(id); err != nil {
			slog.Error("Download request rejected because of invalid photo id: ", "id", id, "err", err)
			http.Error(w, "Invalid photo id: "+id, http.StatusBadRequest)
			return
		}
	}


	//if the query string contains raw=true, include the raw files in the zip archive
	includeRaw := r.URL.Query().Get("raw") == "true"

	//get the path and rawpath of all the photos in the collection
	rows, err := s.pool.Query(r.Context(), "SELECT path, COALESCE(raw_path, '') FROM photos where id = ANY($1)", photoIDs)

	if err != nil {
		slog.Error("Download request rejected because of database query error: ", "err", err)
		http.Error(w, "Invalid selection", http.StatusBadRequest)
		return
	}
	defer rows.Close()
	w.Header().Set("Content-Type", "application/zip")

	//create a filename with the date-time of the request, e.g., fast-fotos-selection-2023-08-15-150405.zip
	filename := fmt.Sprintf("fast-fotos-selection-%s.zip", time.Now().Format("2006-01-02-150405"))

	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	archive := zip.NewWriter(w)
	defer archive.Close()
	for rows.Next() {
		var path, rawPath string
		if err := rows.Scan(&path, &rawPath); err != nil {
			return
		}
		if err := s.addDownloadFiles(archive, path, rawPath, includeRaw); err != nil {
			slog.Error("Download request failed while adding files to archive: ", "err", err)
			http.Error(w, "Could not create download", http.StatusInternalServerError)
			return
		}
	}
}

// addDownloadFiles adds an image, its sidecar files (.xmp, .json), and optional raw file to a zip archive.
func (s *Server) addDownloadFiles(archive *zip.Writer, photoPath, rawPath string, includeRaw bool) error {
	base := strings.TrimSuffix(photoPath, filepath.Ext(photoPath))
	for _, extension := range []string{filepath.Ext(photoPath), ".xmp", ".json"} {
		path := base + extension
		if err := addFileToArchive(archive, s.config.PhotoRoot, path); err != nil {
			return err
		}
	}
	if includeRaw && rawPath != "" {
		if err := addFileToArchive(archive, s.config.PhotoRoot, rawPath); err != nil {
			return err
		}
	}
	return nil
}

// addFileToArchive reads a relative file path under root and writes its content into a zip archive entry.
func addFileToArchive(archive *zip.Writer, root, relativePath string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	fullPath, err := filepath.Abs(filepath.Join(root, relativePath))
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(root, fullPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return fmt.Errorf("file path escapes photo root: %q", relativePath)
	}
	file, err := os.Open(fullPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	entry, err := archive.Create(relative)
	if err != nil {
		return err
	}
	_, err = io.Copy(entry, file)
	return err
}

// months queries all distinct photo capture months with total photo counts.
func (s *Server) months(ctx context.Context) ([]Month, error) {
	rows, err := s.pool.Query(ctx, `SELECT to_char(date_trunc('month', taken_at), 'YYYY-MM'), COUNT(*)::int
		FROM photos GROUP BY 1 ORDER BY 1 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var months []Month
	for rows.Next() {
		var month Month
		if err := rows.Scan(&month.Key, &month.Count); err != nil {
			return nil, err
		}
		months = append(months, month)
	}
	return months, rows.Err()
}

// photosForMonth queries all photos captured within a specific calendar month key (YYYY-MM).
func (s *Server) photosForMonth(ctx context.Context, month string) ([]Photo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, path, COALESCE(raw_path, ''), taken_at, latitude, longitude, COALESCE(location, ''), camera_model, focal_length, flash_fired, objects, dominant_colors FROM photos
		WHERE taken_at >= $1::date AND taken_at < ($1::date + INTERVAL '1 month') ORDER BY taken_at ASC`, month+"-01")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectPhotos(rows)
}

// galleries queries all user-created galleries and their photo counts.
func (s *Server) collections(ctx context.Context) ([]Collection, error) {
	rows, err := s.pool.Query(ctx, `SELECT g.id, g.name, COUNT(gp.photo_id)::int
		from collections g LEFT JOIN collection_photos gp ON gp.collection_id = g.id
		GROUP BY g.id ORDER BY g.name ASC`)
	if err != nil {
		slog.Error("Error querying collections: ", "err", err)
		return nil, err
	}
	defer rows.Close()
	var collections []Collection
	for rows.Next() {
		var collection Collection 
		if err := rows.Scan(&collection.ID, &collection.Name, &collection.Count); err != nil {
			return nil, err
		}
		collections = append(collections, collection)
	}
	return collections, rows.Err()
}

// photosForCollection queries all photos assigned to a specific custom gallery ID.
func (s *Server) photosForCollection(ctx context.Context, galleryID int64) ([]Photo, error) {
	rows, err := s.pool.Query(ctx, `SELECT p.id, p.path, COALESCE(p.raw_path, ''), p.taken_at, p.latitude, p.longitude, COALESCE(p.location, ''), p.camera_model, p.focal_length, p.flash_fired, p.objects, p.dominant_colors FROM photos p
		JOIN collection_photos gp ON gp.photo_id = p.id WHERE gp.collection_id = $1 ORDER BY p.taken_at ASC`, galleryID)
	if err != nil {
		slog.Error("Error querying photos for collection: ", "err", err)
		return nil, err
	}

	defer rows.Close()
	return collectPhotos(rows)
}

// photoByID loads a single Photo record by its database primary key ID.
func (s *Server) photoByID(ctx context.Context, id int64) (Photo, error) {
	rows, err := s.pool.Query(ctx, `SELECT id, path, COALESCE(raw_path, ''), taken_at, latitude, longitude, COALESCE(location, ''), camera_model, focal_length, flash_fired, objects, dominant_colors
		FROM photos WHERE id = $1`, id)
	if err != nil {
		slog.Error("Error querying photo by ID: ", "err", err)
		return Photo{}, err
	}
	defer rows.Close()
	photos, err := collectPhotos(rows)
	if err != nil || len(photos) != 1 {
		if err != nil {
			slog.Error("Error collecting photo by ID: ", "err", err)
			return Photo{}, err
		}
		return Photo{}, pgx.ErrNoRows
	}
	return photos[0], nil
}

// photoRows abstracts pgx database row iteration for photo queries.
type photoRows interface {
	Next() bool
	Scan(...any) error
	Err() error
}

// collectPhotos scans database query result rows into a slice of Photo structs.
func collectPhotos(rows photoRows) ([]Photo, error) {
	var photos []Photo
	for rows.Next() {
		var photo Photo
		if err := rows.Scan(&photo.ID, &photo.Path, &photo.RawPath, &photo.TakenAt, &photo.Latitude, &photo.Longitude, &photo.Location, &photo.CameraModel, &photo.FocalLength, &photo.FlashFired, &photo.Objects, &photo.DominantColors); err != nil {
			slog.Error("Error scanning photo row: ", "err", err)
			return nil, err
		}
		photos = append(photos, photo)
	}
	return photos, rows.Err()
}

// render executes a named HTML template with PageData context and writes the output response.
func (s *Server) render(w http.ResponseWriter, name string, data PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	page, ok := s.templates[name]
	if !ok {
		slog.Error("template not found", "name", name)
		http.Error(w, "Unknown page", http.StatusInternalServerError)
		return
	}
	if err := page.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("template execution failed", "name", name, "error", err)
		http.Error(w, "Could not render page", http.StatusInternalServerError)
	}
}

// parseTemplates compiles all embedded HTML templates with helper functions.
func parseTemplates() (map[string]*template.Template, error) {
	funcs := template.FuncMap{
		"monthLabel":       monthLabel,
		"timelinePath":     timelinePath,
		"photoTakenAt":     photoTakenAt,
		"photoFocalLength": photoFocalLength,
		"photoFlash":       photoFlash,
		"imagePath":        imagePath,
		"join":             strings.Join,
		"urlquery":         url.QueryEscape,
	}
	pages := map[string][]string{
		"home":           {"templates/base.html", "templates/home.html", "templates/gallery.html"},
		"image":          {"templates/base.html", "templates/image.html", "templates/gallery.html"},
		"search":         {"templates/base.html", "templates/search.html", "templates/gallery.html"},
		"maintenance":    {"templates/base.html", "templates/maintenance.html"},
		"gallery":        {"templates/gallery.html"},
		"locations":      {"templates/base.html", "templates/locations.html"},
		"cluster":        {"templates/base.html", "templates/cluster.html", "templates/gallery.html"},
		"collections":    {"templates/base.html", "templates/collections.html"},
		"collection": {"templates/base.html", "templates/collection.html", "templates/gallery.html"},
	}
	templates := make(map[string]*template.Template, len(pages))
	for name, files := range pages {
		page, err := template.New(name).Funcs(funcs).ParseFS(templateFiles, files...)
		if err != nil {
			slog.Error("Error parsing template", "name", name, "error", err)
			return nil, fmt.Errorf("parse %s template: %w", name, err)
		}
		templates[name] = page
	}
	return templates, nil
}

// validMonth verifies if a month string follows the YYYY-MM format.
func validMonth(month string) bool {
	_, err := time.Parse("2006-01", month)
	return err == nil && len(month) == len("2006-01")
}

// timelinePath formats a YYYY-MM string into a timeline URI path.
func timelinePath(month string) string {
	if len(month) == 7 && month[4] == '-' {
		return "/timeline/" + month[:4] + "/" + month[5:]
	}
	return "/timeline/" + month
}

// imagePath constructs a URL path to a photo's detail page with an optional return context URL.
func imagePath(id int64, returnURL string) string {
	if returnURL == "" {
		return "/image/" + strconv.FormatInt(id, 10)
	}
	return "/image/" + strconv.FormatInt(id, 10) + "?from=" + url.QueryEscape(returnURL)
}

// monthLabel formats a YYYY-MM string into a human-readable month name and year (e.g. "August 2026").
func monthLabel(month string) string {
	parsed, err := time.Parse("2006-01", month)
	if err != nil {
		slog.Error("Error parsing month string: ", "month", month, "err", err)
		return month
	}
	return parsed.Format("January 2006")
}

// photoTakenAt formats a time.Time timestamp into a display string for image metadata.
func photoTakenAt(takenAt time.Time) string {
	return takenAt.Format("2 Jan 2006, 15:04")
}

// photoFocalLength formats a focal length float value into a millimetre string representation.
func photoFocalLength(focalLength *float64) string {
	if focalLength == nil {
		return ""
	}
	return fmt.Sprintf("%.1f mm", *focalLength)
}

// photoFlash formats a flash boolean pointer into a human-readable flash status string.
func photoFlash(flashFired *bool) string {
	if flashFired == nil {
		return ""
	}
	if *flashFired {
		return "Flash fired"
	}
	return "Flash did not fire"
}
