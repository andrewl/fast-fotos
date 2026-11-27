package app

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/andrewl/fast-fotos/internal/geocode"
	"github.com/andrewl/fast-fotos/internal/objects"
)

//go:embed templates/*.html
var templateFiles embed.FS

//go:embed static/*
var staticFiles embed.FS

// Server represents the web application server instance, managing database connections, templates, geocoding, and object detection.
type Server struct {
	config          Config
	db              *DB
	templates       map[string]*template.Template
	indexProgressMu sync.RWMutex
	indexProgress   IndexProgress
	indexCancel     context.CancelFunc
	geocoder        geocode.Provider
	detector        objects.Detector
	detectMu        sync.Mutex
	detectCancel    context.CancelFunc
	detectDone      chan struct{}
	detectProgress  DetectionProgress
	closing         bool
	dlnaCancel      context.CancelFunc
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
	Active     bool              `json:"active"`
	Processed  int               `json:"processed"`
	Total      int               `json:"total"`
	Percentage int               `json:"percentage"`
	Message    string            `json:"message"`
	Detection  DetectionProgress `json:"detection"`
}

type DetectionProgress struct {
	Active     bool   `json:"active"`
	Processed  int    `json:"processed"`
	Total      int    `json:"total"`
	Percentage int    `json:"percentage"`
	Message    string `json:"message"`
}

type Photo struct {
	ID              int64
	Path            string
	RawPath         string
	TakenAt         time.Time
	Latitude        *float64
	Longitude       *float64
	Location        string
	Settlement      string
	Region          string
	Country         string
	CameraModel     sql.NullString
	FocalLength     *float64
	FlashFired      *bool
	Objects         []string
	DominantColors  []string
	MediaType       string
	DurationSeconds *float64
	DayOfYear       int
	GoldenHour      bool
	Sharpness       *float64
	Exposure        *float64
	Contrast        *float64
	Brisque         *float64
}

// IsVideo reports whether the photo record represents a video file rather than a still image.
func (p Photo) IsVideo() bool {
	return p.MediaType == "video"
}

// Month represents a calendar month grouping key and photo count.
type Month struct {
	Key   string
	Count int
}

// Gallery represents a user-created gallery.
type Gallery struct {
	ID          int64
	Name        string
	Description string
	Count       int
}

// Collection is kept for source compatibility; new code should use Gallery.
type Collection = Gallery

// PageData contains all contextual view data passed to HTML templates for rendering.
type PageData struct {
	Page               string
	Photos             []Photo
	Photo              *Photo
	Months             []Month
	Collections        []Collection
	Galleries          []Gallery
	SelectedMonth      string
	Collection         *Collection
	PhotoRoot          string
	Locations          []LocationGroup
	SelectedLocation   string
	SelectedSettlement string
	SelectedRegion     string
	MapBounds          *MapBounds
	MapPhotoID         int64
	MapURL             string
	MapPhotoIDs        []int64
	MapPhotoIDsCSV     string
	MapReturnURL       string
	Search             SearchData
	SavedSearches      []SavedSearch
	ReturnURL          string
	CollectionName     string
	PreviousPhotoID    int64
	NextPhotoID        int64
	OtherCollections   []Collection
	PhotoCollections   map[int64]bool
	Selected           map[int64]bool
	Events             []Event
	Event              *Event
	ShowHiddenEvents   bool
}

// SearchData holds filter parameters and dropdown values for photo search options.
type SearchData struct {
	DateFrom, DateTo, Label, CameraModel, FocalLength, Flash, Location, Color string
	CameraModels                                                              []string
	Locations                                                                 []SearchLocation
	Labels                                                                    []string
	FocalLengths                                                              []string
	Colors                                                                    []string
	Sharpness, Exposure, Contrast, Quality                                    string
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

// NewServer opens the SQLite database, runs migrations, parses templates, and returns an initialized Server.
func NewServer(ctx context.Context, config Config) (*Server, error) {
	db, err := OpenDB(ctx, config.DatabasePath)
	if err != nil {
		slog.Error("open database", "error", err)
		return nil, err
	}
	server := &Server{config: config, db: db}
	if config.GeoNamesDir != "" {
		server.geocoder, err = geocode.LoadGeoNamesProvider(config.GeoNamesDir)
		if err != nil {
			slog.Error("load GeoNames data", "error", err)
			db.Close()
			return nil, fmt.Errorf("load GeoNames data: %w", err)
		}
	}
	if config.ObjectModelDir != "" {
		server.detector, err = objects.LoadONNXDetector(config.ObjectModelDir, config.ONNXIntraOpThreads, config.ONNXInterOpThreads)
		if err != nil {
			slog.Error("load object detector", "error", err)
			db.Close()
			return nil, fmt.Errorf("load object detector: %w", err)
		}
	}
	server.templates, err = parseTemplates()
	if err != nil {
		slog.Error("parse templates", "error", err)
		db.Close()
		return nil, fmt.Errorf("parse templates: %w", err)
	}
	server.startDetection()
	return server, nil
}

// startDetection launches background object detection and quality assessment unless already running or indexing is active.
func (s *Server) startDetection() {
	s.indexProgressMu.RLock()
	indexing := s.indexCancel != nil
	s.indexProgressMu.RUnlock()
	s.detectMu.Lock()
	defer s.detectMu.Unlock()
	if indexing || s.closing || s.detectCancel != nil {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	s.detectCancel, s.detectDone = cancel, done
	s.detectProgress = DetectionProgress{Active: true, Message: "Analysing photos: preparing..."}
	go func() {
		defer close(done)
		if err := s.db.RebuildEvents(ctx); err != nil && ctx.Err() == nil {
			slog.Error("rebuild events", "error", err)
		}
		if s.detector != nil {
			s.detectObjects(ctx)
		}
		s.assessQuality(ctx)
		s.detectMu.Lock()
		if s.detectDone == done {
			s.detectCancel, s.detectDone = nil, nil
			s.detectProgress.Active = false
		}
		s.detectMu.Unlock()
		cancel()
	}()
}

// stopDetection cancels any running object detection and waits for it to stop.
func (s *Server) stopDetection() {
	s.detectMu.Lock()
	cancel, done := s.detectCancel, s.detectDone
	s.detectMu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
}

// Close closes underlying database pools and releases object detector resources.
func (s *Server) Close() {
	s.detectMu.Lock()
	s.closing = true
	s.detectMu.Unlock()
	s.stopDetection()
	if s.dlnaCancel != nil {
		s.dlnaCancel()
		s.dlnaCancel = nil
	}
	if detector, ok := s.detector.(interface{ Close() error }); ok {
		_ = detector.Close()
	}
	s.db.Close()
}

// Routes constructs and returns an http.Handler with all application endpoints configured.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()
	s.startDLNADiscovery()
	mux.HandleFunc("GET /dlna/description.xml", s.dlnaDescription)
	mux.HandleFunc("GET /dlna/contentdirectory.xml", s.dlnaContentDirectoryDescription)
	mux.HandleFunc("GET /dlna/resource/", s.dlnaResource)
	mux.HandleFunc("GET /dlna/thumbnail/", s.dlnaThumbnail)
	mux.HandleFunc("POST /dlna/control", s.dlnaControl)
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServerFS(mustSub(staticFiles, "static"))))
	mux.HandleFunc("GET /", s.home)
	mux.HandleFunc("GET /timeline/", s.timeline)
	mux.HandleFunc("GET /search", s.search)
	mux.HandleFunc("GET /search/", s.search)
	mux.HandleFunc("GET /maintenance", s.maintenance)
	mux.HandleFunc("GET /months/", s.month)
	mux.HandleFunc("GET /this-week/", s.thisweek)
	mux.HandleFunc("GET /this-week", s.thisweek)
	mux.HandleFunc("GET /events", s.eventsPage)
	mux.HandleFunc("GET /events/", s.eventsPage)
	mux.HandleFunc("POST /events/{id}/name", s.updateEventName)
	mux.HandleFunc("POST /events/{id}/hide", s.setEventHidden)
	mux.HandleFunc("POST /events/{id}/restore", s.setEventHidden)
	mux.HandleFunc("GET /locations", s.locations)
	mux.HandleFunc("GET /locations/", s.locations)
	mux.HandleFunc("GET /cluster", s.cluster)
	mux.HandleFunc("GET /api/map-points", s.mapPoints)
	mux.HandleFunc("GET /api/map-cluster", s.mapCluster)
	mux.HandleFunc("GET /gallery/", s.galleryPage)
	mux.HandleFunc("GET /galleries", s.galleryPage)
	mux.HandleFunc("GET /galleries/", s.galleryPage)
	mux.HandleFunc("POST /galleries/{id}/description", s.updateGalleryDescription)
	mux.HandleFunc("GET /selected", s.selected)
	mux.HandleFunc("GET /selected/", s.selected)
	mux.HandleFunc("POST /gallery", s.addToGallery)
	mux.HandleFunc("DELETE /gallery/", s.removeFromGallery)
	mux.HandleFunc("DELETE /galleries/", s.deleteGallery)
	mux.HandleFunc("POST /selection", s.selectPhotos)
	mux.HandleFunc("DELETE /selection", s.unselectPhotos)
	mux.HandleFunc("POST /selection/clear", s.clearSelection)
	mux.HandleFunc("POST /galleries/0/remove", s.removeSelectedGalleryPhotos)
	mux.HandleFunc("POST /saved-searches", s.saveSearch)
	mux.HandleFunc("DELETE /saved-searches/", s.deleteSavedSearch)
	mux.HandleFunc("GET /similar/", s.similarCollection)
	mux.HandleFunc("GET /near-to/", s.nearCollection)
	mux.HandleFunc("GET /random", s.randomCollection)
	mux.HandleFunc("GET /random/", s.randomCollection)
	mux.HandleFunc("POST /index", s.index)
	mux.HandleFunc("POST /reindex", s.reindex)
	mux.HandleFunc("POST /stop-indexing", s.stopIndexing)
	mux.HandleFunc("GET /index-progress", s.indexProgressStatus)
	mux.HandleFunc("GET /thumbnails/", s.thumbnailFile)
	mux.HandleFunc("GET /photos/", s.photoFile)
	mux.HandleFunc("GET /raw/", s.rawFile)
	mux.HandleFunc("GET /download/", s.download)
	mux.HandleFunc("GET /galleries/{id}/download", s.downloadGallery)
	return mux
}

// selected renders the selection, its photo details, or its map.
// Inputs: an HTTP response writer and request; output: HTML or an HTTP error.
func (s *Server) selected(w http.ResponseWriter, r *http.Request) {
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok || collectionPath != "/selected" {
		http.NotFound(w, r)
		return
	}
	photos, err := s.db.PhotosForCollection(r.Context(), SelectedCollectionID)
	if err != nil {
		slog.Error("Failed to load selected photos", "error", err)
		http.Error(w, "Could not load selected photos", http.StatusInternalServerError)
		return
	}
	if mapView {
		if hasPhoto {
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, "/selected", photos)
		return
	}
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, "/selected", "Selected photos", photos)
		return
	}
	galleries, err := s.db.Collections(r.Context())
	if err != nil {
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "selected", PageData{Page: "selected", Photos: photos, Collections: galleries, ReturnURL: "/selected"})
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

// home renders the root homepage showing the latest month's photo timeline.
func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	months, err := s.db.Months(r.Context())
	if err != nil {
		slog.Error("Failed to load months", "error", err)
		http.Error(w, "Could not load photo library", http.StatusInternalServerError)
		return
	}

	var photos []Photo
	selectedMonth := ""
	if len(months) > 0 {
		selectedMonth = months[0].Key
		photos, err = s.db.PhotosForMonth(r.Context(), months[0].Key)
		if err != nil {
			slog.Error("Failed to load photos for month", "month", months[0].Key, "error", err)
			http.Error(w, "Could not load photos", http.StatusInternalServerError)
			return
		}
	}
	s.render(w, "home", PageData{Page: "home", Months: months, Photos: photos, SelectedMonth: selectedMonth, PhotoRoot: s.config.PhotoRoot, ReturnURL: "/"})
}

// timeline renders the photo gallery for a specific month (e.g. /timeline/2026-08).
func (s *Server) timeline(w http.ResponseWriter, r *http.Request) {
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	month := strings.TrimPrefix(collectionPath, "/timeline/")
	if len(month) == 7 && month[4] == '/' {
		month = month[:4] + "-" + month[5:]
	}
	if !validMonth(month) {
		http.NotFound(w, r)
		return
	}
	months, err := s.db.Months(r.Context())
	if err != nil {
		slog.Error("Failed to load months", "error", err)
		http.Error(w, "Could not load photo library", http.StatusInternalServerError)
		return
	}
	photos, err := s.db.PhotosForMonth(r.Context(), month)
	if err != nil {
		slog.Error("Failed to load photos for month", "month", month, "error", err)
		http.Error(w, "Could not load photos", http.StatusInternalServerError)
		return
	}
	months, err = s.db.Months(r.Context())
	if err != nil {
		slog.Error("Failed to load months", "error", err)
		http.Error(w, "Could not load photo library", http.StatusInternalServerError)
		return
	}
	if mapView {
		if hasPhoto {
			if !photoInCollection(photos, photoID) {
				http.NotFound(w, r)
				return
			}
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, collectionPath, photos)
		return
	}
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, collectionPath, monthLabel(month), photos)
		return
	}
	s.render(w, "home", PageData{Page: "home", Months: months, Photos: photos, SelectedMonth: month, PhotoRoot: s.config.PhotoRoot, ReturnURL: r.URL.Path})
}

// parseSearchCollectionCriteria decodes path-encoded search filters into query values.
// Input: comma-separated or URL-encoded criteria; outputs: parsed filters or an error.
func parseSearchCollectionCriteria(criteria string) (url.Values, error) {
	decoded, err := url.PathUnescape(criteria)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if strings.Contains(decoded, ",") && !strings.Contains(decoded, "&") {
		for _, part := range strings.Split(decoded, ",") {
			key, value, found := strings.Cut(part, "=")
			if !found {
				return nil, errors.New("invalid search criteria")
			}
			query.Set(key, value)
		}
	} else {
		query, err = url.ParseQuery(decoded)
		if err != nil {
			return nil, err
		}
	}
	for _, key := range []string{"from", "to"} {
		value := query.Get(key)
		if len(value) == 8 {
			query.Set(key, value[:4]+"-"+value[4:6]+"-"+value[6:])
		}
	}
	return query, nil
}

// parseLocationsCollectionCriteria decodes path-encoded location filters into query values.
// Input: comma-separated or URL-encoded criteria; outputs: parsed filters or an error.
func parseLocationsCollectionCriteria(criteria string) (url.Values, error) {
	decoded, err := url.PathUnescape(criteria)
	if err != nil {
		return nil, err
	}
	query := url.Values{}
	if strings.Contains(decoded, ",") && !strings.Contains(decoded, "&") {
		for _, part := range strings.Split(decoded, ",") {
			key, value, found := strings.Cut(part, "=")
			if !found {
				return nil, errors.New("invalid locations criteria")
			}
			query.Set(key, value)
		}
	} else {
		query, err = url.ParseQuery(decoded)
		if err != nil {
			return nil, err
		}
	}
	return query, nil
}

// canonicalLocationsURL serializes supported location filters into a stable locations path.
// Input: location query values; output: a canonical /locations URL.
func canonicalLocationsURL(query url.Values) string {
	keys := []string{"place", "settlement", "region", "minLat", "minLng", "maxLat", "maxLng", "mapLat", "mapLng", "mapZoom", "photoId", "photoIds"}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(query.Get(key))
		if value != "" {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}
	if len(parts) == 0 {
		return "/locations"
	}
	return "/locations/" + strings.Join(parts, ",")
}

// canonicalSearchURL serializes supported search filters into a stable search path.
// Input: search query values; output: a canonical /search URL.
func canonicalSearchURL(query url.Values) string {
	keys := []string{"from", "to", "label", "color", "camera", "focal", "flash", "location", "sharpness", "exposure", "contrast", "quality"}
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		value := strings.TrimSpace(query.Get(key))
		if value != "" {
			parts = append(parts, url.QueryEscape(key)+"="+url.QueryEscape(value))
		}
	}
	if len(parts) == 0 {
		return "/search"
	}
	return "/search/" + strings.Join(parts, ",")
}

// searchTitle derives a concise human-readable title from canonical search criteria.
func searchTitle(query url.Values) string {
	labels := make([]string, 0, 8)
	if value := strings.TrimSpace(query.Get("from")); value != "" {
		labels = append(labels, "from "+value)
	}
	if value := strings.TrimSpace(query.Get("to")); value != "" {
		labels = append(labels, "to "+value)
	}
	if value := strings.TrimSpace(query.Get("label")); value != "" {
		labels = append(labels, "label "+value)
	}
	if value := strings.TrimSpace(query.Get("color")); value != "" {
		labels = append(labels, "color "+value)
	}
	if value := strings.TrimSpace(query.Get("camera")); value != "" {
		labels = append(labels, "camera "+value)
	}
	if value := strings.TrimSpace(query.Get("focal")); value != "" {
		labels = append(labels, "focal "+value+" mm")
	}
	if value := strings.TrimSpace(query.Get("flash")); value != "" {
		if value == "yes" {
			labels = append(labels, "flash fired")
		} else {
			labels = append(labels, "flash not fired")
		}
	}
	for _, key := range []string{"sharpness", "exposure", "contrast", "quality"} {
		if value := strings.TrimSpace(query.Get(key)); value != "" {
			labels = append(labels, key+" "+value)
		}
	}
	if value := strings.TrimSpace(query.Get("location")); value != "" {
		value = strings.TrimPrefix(value, "settlement:")
		value = strings.TrimPrefix(value, "region:")
		labels = append(labels, "location "+value)
	}
	if len(labels) == 0 {
		return "All photos"
	}
	return strings.Join(labels, ", ")
}

// search renders the photo search form and filtered search result thumbnail grid.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	var collectionPath string
	var photoID int64
	var hasPhoto bool
	var mapView bool
	if r.URL.Path == "/search" && len(query) > 0 {
		http.Redirect(w, r, canonicalSearchURL(query), http.StatusSeeOther)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/search/") {
		var ok bool
		collectionPath, photoID, hasPhoto, mapView, ok = splitCollectionPhotoMapPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		criteria := strings.TrimPrefix(collectionPath, "/search/")
		parsedQuery, err := parseSearchCollectionCriteria(criteria)
		if err != nil {
			slog.Error("Failed to parse search criteria", "criteria", criteria, "error", err)
			http.NotFound(w, r)
			return
		}
		for _, key := range []string{"from", "to", "label", "color", "camera", "focal", "flash", "location", "sharpness", "exposure", "contrast", "quality"} {
			if parsedQuery.Get(key) == "" && r.URL.Query().Get(key) != "" {
				parsedQuery.Set(key, r.URL.Query().Get(key))
			}
		}
		query = parsedQuery
	}
	search := SearchData{
		DateFrom: query.Get("from"), DateTo: query.Get("to"), Label: query.Get("label"),
		CameraModel: query.Get("camera"), FocalLength: query.Get("focal"), Flash: query.Get("flash"),
		Location: query.Get("location"), Color: query.Get("color"),
		Sharpness: query.Get("sharpness"), Exposure: query.Get("exposure"), Contrast: query.Get("contrast"), Quality: query.Get("quality"),
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

	for column, value := range map[string]string{
		"sharpness_score": search.Sharpness, "exposure_score": search.Exposure,
		"contrast_score": search.Contrast, "brisque_score": search.Quality,
	} {
		if !validQualityBand(column, value) {
			http.Error(w, "Invalid quality filter", http.StatusBadRequest)
			return
		}
	}

	collectionQuery := url.Values{}
	for key, value := range map[string]string{
		"from": search.DateFrom, "to": search.DateTo, "label": search.Label,
		"color": search.Color, "camera": search.CameraModel, "focal": search.FocalLength,
		"flash": search.Flash, "location": search.Location,
		"sharpness": search.Sharpness, "exposure": search.Exposure,
		"contrast": search.Contrast, "quality": search.Quality,
	} {
		if value != "" {
			collectionQuery.Set(key, value)
		}
	}
	if strings.HasPrefix(r.URL.Path, "/search") {
		collectionPath = canonicalSearchURL(collectionQuery)
	}

	photos := []Photo{}
	//we must have at lease one search criteria to avoid returning the entire photo library
	if search.DateFrom == "" && search.DateTo == "" && search.Label == "" && search.CameraModel == "" && search.FocalLength == "" && search.Flash == "" && search.Location == "" && search.Color == "" &&
		search.Sharpness == "" && search.Exposure == "" && search.Contrast == "" && search.Quality == "" {
		slog.Info("No search criteria provided")
	} else {
		var err error
		photos, err = s.db.SearchPhotos(r.Context(), search)
		if err != nil {
			slog.Error("Failed to search photos", "error", err)
			http.Error(w, "Could not search photos", http.StatusInternalServerError)
			return
		}
	}
	if mapView {
		if hasPhoto {
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, collectionPath, photos)
		return
	}
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, collectionPath, "Search results", photos)
		return
	}
	var err error
	if search.CameraModels, err = s.db.CameraModels(r.Context()); err != nil {
		slog.Error("Failed to load camera models", "error", err)
		http.Error(w, "Could not load camera models", http.StatusInternalServerError)
		return
	}
	if search.Locations, err = s.db.SearchLocations(r.Context()); err != nil {
		slog.Error("Failed to load locations", "error", err)
		http.Error(w, "Could not load locations", http.StatusInternalServerError)
		return
	}
	if search.FocalLengths, err = s.db.FocalLengths(r.Context()); err != nil {
		slog.Error("Failed to load focal lengths", "error", err)
		http.Error(w, "Could not load focal lengths", http.StatusInternalServerError)
		return
	}
	if search.Labels, err = s.db.Labels(r.Context()); err != nil {
		slog.Error("Failed to load labels", "error", err)
		http.Error(w, "Could not load labels", http.StatusInternalServerError)
		return
	}
	if search.Colors, err = s.db.Colors(r.Context()); err != nil {
		slog.Error("Failed to load colors", "error", err)
		http.Error(w, "Could not load colors", http.StatusInternalServerError)
		return
	}

	galleries, err := s.db.Collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	if collectionPath == "" {
		collectionPath = "/search"
	}
	savedSearches, err := s.db.SavedSearches(r.Context())
	if err != nil {
		slog.Error("Failed to load saved searches", "error", err)
		http.Error(w, "Could not load saved searches", http.StatusInternalServerError)
		return
	}
	s.render(w, "search", PageData{
		Page: "search", Photos: photos, Collections: galleries, Search: search,
		SavedSearches: savedSearches, ReturnURL: collectionPath,
	})
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
	if strings.Contains(galleryURL.Path, "/photo/") {
		collectionPath, _, _, validCollectionPath := splitCollectionPhotoPath(galleryURL.Path)
		if validCollectionPath {
			galleryURL.Path = collectionPath
		}
	}
	switch {
	case galleryURL.Path == "/":
		slog.Info("Loading timeline", "query", galleryURL.Query())
		months, err := s.db.Months(ctx)
		if err != nil || len(months) == 0 {
			return "/", "Timeline", nil, err
		}
		photos, err := s.db.PhotosForMonth(ctx, months[0].Key)
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
		photos, err := s.db.PhotosForMonth(ctx, month)
		return galleryURL.RequestURI(), monthLabel(month), photos, err
	case galleryURL.Path == "/cluster":
		slog.Info("Loading map cluster", "query", galleryURL.Query())
		photos, err := s.clusterPhotos(ctx, galleryURL.Query())
		return galleryURL.RequestURI(), "Map cluster", photos, err
	case galleryURL.Path == "/selected":
		photos, err := s.db.PhotosForCollection(ctx, SelectedCollectionID)
		return "/selected", "Selected photos", photos, err
	case strings.HasPrefix(galleryURL.Path, "/events/"):
		id, err := strconv.ParseInt(strings.TrimPrefix(galleryURL.Path, "/events/"), 10, 64)
		if err != nil || id < 1 {
			return "", "", nil, errors.New("invalid event")
		}
		event, err := s.db.EventByID(ctx, id)
		if err != nil {
			return "", "", nil, err
		}
		photos, err := s.db.PhotosForEvent(ctx, id)
		return galleryURL.Path, event.Name, photos, err
	case strings.HasPrefix(galleryURL.Path, "/galleries/"):
		slog.Info("Loading collection", "collectionId", galleryURL.Path)
		galleryPath := strings.TrimPrefix(galleryURL.Path, "/galleries/")
		collectionID, err := strconv.ParseInt(galleryPath, 10, 64)
		if err != nil || collectionID < 0 {
			slog.Error("Invalid collection ID", "collectionId", galleryURL.Path, "error", err)
			return "", "", nil, errors.New("invalid collection")
		}
		photos, err := s.db.PhotosForCollection(ctx, collectionID)
		name, err := s.db.CollectionName(ctx, collectionID)
		if err != nil {
			slog.Error("Failed to load collection name", "collectionId", collectionID, "error", err)
		}
		return galleryURL.Path, name, photos, err
	case galleryURL.Path == "/this-week" || galleryURL.Path == "/this-week/":
		photos, err := s.photosForThisWeek(ctx)
		return galleryURL.Path, "This Week", photos, err
	case galleryURL.Path == "/search" || strings.HasPrefix(galleryURL.Path, "/search/"):
		slog.Info("Loading search results", "query", galleryURL.Query())
		query := galleryURL.Query()
		if strings.HasPrefix(galleryURL.Path, "/search/") {
			criteria := strings.TrimPrefix(galleryURL.Path, "/search/")
			var err error
			query, err = parseSearchCollectionCriteria(criteria)
			if err != nil {
				return "", "", nil, err
			}
		}
		search := SearchData{
			DateFrom: query.Get("from"), DateTo: query.Get("to"), Label: query.Get("label"),
			CameraModel: query.Get("camera"), FocalLength: query.Get("focal"), Flash: query.Get("flash"),
			Location: query.Get("location"), Color: query.Get("color"),
			Sharpness: query.Get("sharpness"), Exposure: query.Get("exposure"), Contrast: query.Get("contrast"), Quality: query.Get("quality"),
		}
		if strings.HasPrefix(search.Location, "settlement:") {
			search.Settlement = strings.TrimPrefix(search.Location, "settlement:")
		} else if strings.HasPrefix(search.Location, "region:") {
			search.Region = strings.TrimPrefix(search.Location, "region:")
		} else if search.Location != "" {
			return "", "", nil, errors.New("invalid location filter")
		}
		photos, err := s.db.SearchPhotos(ctx, search)
		return galleryURL.RequestURI(), "Search results", photos, err
	default:
		slog.Error("Unsupported collection URL", "url", galleryURL.Path)
		return "", "", nil, errors.New("unsupported collection URL")
	}
}

// splitCollectionPhotoPath separates a collection URL from an optional trailing /photo/<id>.
func splitCollectionPhotoPath(path string) (collectionPath string, photoID int64, hasPhoto bool, ok bool) {
	const marker = "/photo/"
	markerIndex := strings.LastIndex(path, marker)
	if markerIndex < 0 {
		return strings.TrimSuffix(path, "/"), 0, false, path != ""
	}
	collectionPath = strings.TrimSuffix(path[:markerIndex], "/")
	idText := path[markerIndex+len(marker):]
	if collectionPath == "" || idText == "" || strings.Contains(idText, "/") {
		return "", 0, false, false
	}
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 {
		return "", 0, false, false
	}
	return collectionPath, id, true, true
}

// splitCollectionPhotoMapPath handles the optional /map suffix on collection and photo URLs.
func splitCollectionPhotoMapPath(path string) (collectionPath string, photoID int64, hasPhoto, mapView, ok bool) {
	mapView = strings.HasSuffix(strings.TrimSuffix(path, "/"), "/map")
	if mapView {
		path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), "/map")
	}
	collectionPath, photoID, hasPhoto, ok = splitCollectionPhotoPath(path)
	if ok {
		return collectionPath, photoID, hasPhoto, mapView, true
	}
	if mapView {
		collectionPath = strings.TrimSuffix(path, "/")
		if collectionPath != "" {
			return collectionPath, 0, false, true, true
		}
	}
	return "", 0, false, false, false
}

// mapURLForPhoto returns a photo map URL when the photo has valid coordinates.
// Input: a photo record; output: its map path or an empty string when it is ungeotagged.
func mapURLForPhoto(photo Photo) string {
	if photo.Latitude == nil || photo.Longitude == nil || (*photo.Latitude == 0 && *photo.Longitude == 0) {
		return ""
	}
	return fmt.Sprintf("/photo/%d/map", photo.ID)
}

// mapURLForCollectionPhoto builds a map path that preserves the photo's collection context.
// Inputs: a collection return URL and photo record; output: a map path or an empty string.
func mapURLForCollectionPhoto(returnURL string, photo Photo) string {
	if photo.Latitude == nil || photo.Longitude == nil || (*photo.Latitude == 0 && *photo.Longitude == 0) {
		return ""
	}
	if parsed, err := url.Parse(returnURL); err == nil {
		returnURL = parsed.Path
	}
	if collectionPath, _, hasPhoto, _, valid := splitCollectionPhotoMapPath(returnURL); valid && !hasPhoto {
		return strings.TrimSuffix(collectionPath, "/") + fmt.Sprintf("/photo/%d/map", photo.ID)
	}
	return mapURLForPhoto(photo)
}

// photoInCollection reports whether a photo ID occurs in a collection's photo list.
// Inputs: photo records and an ID; output: whether the ID is present.
func photoInCollection(photos []Photo, id int64) bool {
	for _, photo := range photos {
		if photo.ID == id {
			return true
		}
	}
	return false
}

// renderCollectionMap renders a map limited to geotagged photos in a collection.
func (s *Server) renderCollectionMap(w http.ResponseWriter, r *http.Request, returnURL string, photos []Photo) {
	ids := make([]int64, 0, len(photos))
	var minLat, minLng, maxLat, maxLng *float64
	for _, photo := range photos {
		if photo.Latitude == nil || photo.Longitude == nil || (*photo.Latitude == 0 && *photo.Longitude == 0) {
			continue
		}
		ids = append(ids, photo.ID)
		lat, lng := *photo.Latitude, *photo.Longitude
		if minLat == nil || lat < *minLat {
			minLat = &lat
		}
		if maxLat == nil || lat > *maxLat {
			maxLat = &lat
		}
		if minLng == nil || lng < *minLng {
			minLng = &lng
		}
		if maxLng == nil || lng > *maxLng {
			maxLng = &lng
		}
	}
	if len(ids) == 0 {
		http.Error(w, "Collection has no geotagged photos", http.StatusNotFound)
		return
	}
	bounds := &MapBounds{
		MinLatitude: *minLat, MinLongitude: *minLng,
		MaxLatitude: *maxLat, MaxLongitude: *maxLng,
	}
	locationGroups, _ := s.db.LocationGroups(r.Context())
	idText := make([]string, len(ids))
	for i, id := range ids {
		idText[i] = strconv.FormatInt(id, 10)
	}
	s.render(w, "locations", PageData{
		Page: "locations", Locations: locationGroups, MapBounds: bounds,
		MapPhotoIDs: ids, MapPhotoIDsCSV: strings.Join(idText, ","),
		MapReturnURL: returnURL,
	})
}

// renderCollectionPhoto verifies membership before displaying a photo and derives navigation from the collection.
func (s *Server) renderCollectionPhoto(w http.ResponseWriter, r *http.Request, id int64, returnURL, collectionName string, photos []Photo) {
	if !photoInCollection(photos, id) {
		http.NotFound(w, r)
		return
	}
	photo, err := s.db.PhotoByID(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	previousID, nextID := adjacentPhotoIDs(photos, id)
	galleries, err := s.db.Collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	photoCollections, err := s.db.CollectionIDsForPhoto(r.Context(), id)
	if err != nil {
		slog.Error("Failed to load photo collections", "id", id, "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	var otherCollections []Collection
	for _, gallery := range galleries {
		if gallery.Name != SelectedCollectionName {
			otherCollections = append(otherCollections, gallery)
		}
	}
	selected, err := s.db.SelectedPhotoIDs(r.Context())
	if err != nil {
		slog.Error("Failed to load selected photos", "error", err)
		http.Error(w, "Could not load selected photos", http.StatusInternalServerError)
		return
	}
	s.render(w, "image", PageData{
		Page: "image", Photo: &photo, Collections: galleries, OtherCollections: otherCollections,
		PhotoCollections: photoCollections, ReturnURL: returnURL, CollectionName: collectionName,
		PreviousPhotoID: previousID, NextPhotoID: nextID,
		MapURL:   mapURLForCollectionPhoto(returnURL, photo),
		Selected: selected,
	})
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
	galleries, err := s.db.Collections(r.Context())
	if err != nil {
		slog.Error("Failed to load galleries", "error", err)
		http.Error(w, "Could not load galleries", http.StatusInternalServerError)
		return
	}
	s.render(w, "maintenance", PageData{Page: "maintenance", Collections: galleries})
}

// month renders the photo grid HTML template partial for a given month.
func (s *Server) month(w http.ResponseWriter, r *http.Request) {
	month := strings.TrimPrefix(r.URL.Path, "/months/")
	if !validMonth(month) {
		slog.Error("Invalid month format", "month", month)
		http.Error(w, "Invalid month", http.StatusBadRequest)
		return
	}
	photos, err := s.db.PhotosForMonth(r.Context(), month)
	if err != nil {
		slog.Error("Failed to load photos for month", "month", month, "error", err)
		http.Error(w, "Could not load photos", http.StatusInternalServerError)
		return
	}
	months, err := s.db.Months(r.Context())
	if err != nil {
		slog.Error("Failed to load months", "error", err)
		http.Error(w, "Could not load photo library", http.StatusInternalServerError)
		return
	}
	s.render(w, "home", PageData{Page: "home", Photos: photos, SelectedMonth: month, Months: months, ReturnURL: r.URL.Path})
}

// thiswweek renders the photo grid HTML template partial for this week
func (s *Server) thisweek(w http.ResponseWriter, r *http.Request) {
	photos, err := s.photosForThisWeek(r.Context())
	if err != nil {
		slog.Error("Failed to load photos for this week", "error", err)
		http.Error(w, "Could not load photos", http.StatusInternalServerError)
		return
	}
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok || (collectionPath != "/this-week" && collectionPath != "/this-week/") {
		http.NotFound(w, r)
		return
	}
	if mapView {
		if hasPhoto {
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, "/this-week", photos)
		return
	}
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, "/this-week", "This Week", photos)
		return
	}
	s.render(w, "thisweek", PageData{Page: "thisweek", Photos: photos, ReturnURL: r.URL.Path})
}

// locations renders the interactive Leaflet map and location selection sidebar.
func (s *Server) locations(w http.ResponseWriter, r *http.Request) {
	locationGroups, err := s.db.LocationGroups(r.Context())
	if err != nil {
		slog.Error("Failed to load locations", "error", err)
		http.Error(w, "Could not load locations", http.StatusInternalServerError)
		return
	}

	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}

	mapPhotoID := int64(0)
	var bounds *MapBounds
	criteria := r.URL.Query()
	if strings.HasPrefix(collectionPath, "/locations/") {
		encoded := strings.TrimPrefix(collectionPath, "/locations/")
		criteria, err = parseLocationsCollectionCriteria(encoded)
		if err != nil {
			http.NotFound(w, r)
			return
		}
	}

	selectedSettlement := criteria.Get("settlement")
	selectedRegion := criteria.Get("region")
	if mapPhotoID == 0 {
		minLat, hasMinLat := parseOptionalFloat(criteria.Get("minLat"))
		minLng, hasMinLng := parseOptionalFloat(criteria.Get("minLng"))
		maxLat, hasMaxLat := parseOptionalFloat(criteria.Get("maxLat"))
		maxLng, hasMaxLng := parseOptionalFloat(criteria.Get("maxLng"))
		if hasMinLat && hasMinLng && hasMaxLat && hasMaxLng &&
			minLat >= -90 && maxLat <= 90 && minLng >= -180 && maxLng <= 180 && minLat <= maxLat && minLng <= maxLng {
			bounds = &MapBounds{MinLatitude: minLat, MinLongitude: minLng, MaxLatitude: maxLat, MaxLongitude: maxLng}
		} else if criteria.Get("minLat") != "" || criteria.Get("minLng") != "" || criteria.Get("maxLat") != "" || criteria.Get("maxLng") != "" {
			http.Error(w, "Invalid map bounds", http.StatusBadRequest)
			return
		}
	}
	if place := criteria.Get("place"); place != "" {
		kind, placeBounds, err := s.db.PlaceBounds(r.Context(), place)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			slog.Error("Failed to load place bounds", "place", place, "error", err)
			http.Error(w, "Could not load place bounds", http.StatusInternalServerError)
			return
		}
		if kind == "settlement" {
			selectedSettlement = place
		} else {
			selectedRegion = place
		}
		resolvedMinLat, hasResolvedMinLat := parseOptionalFloat(criteria.Get("minLat"))
		resolvedMinLng, hasResolvedMinLng := parseOptionalFloat(criteria.Get("minLng"))
		resolvedMaxLat, hasResolvedMaxLat := parseOptionalFloat(criteria.Get("maxLat"))
		resolvedMaxLng, hasResolvedMaxLng := parseOptionalFloat(criteria.Get("maxLng"))
		if hasResolvedMinLat && hasResolvedMinLng && hasResolvedMaxLat && hasResolvedMaxLng {
			bounds = &MapBounds{
				MinLatitude: resolvedMinLat, MinLongitude: resolvedMinLng,
				MaxLatitude: resolvedMaxLat, MaxLongitude: resolvedMaxLng,
			}
		} else {
			redirectQuery := url.Values{
				"minLat": {fmt.Sprintf("%.6f", placeBounds.MinLatitude)},
				"minLng": {fmt.Sprintf("%.6f", placeBounds.MinLongitude)},
				"maxLat": {fmt.Sprintf("%.6f", placeBounds.MaxLatitude)},
				"maxLng": {fmt.Sprintf("%.6f", placeBounds.MaxLongitude)},
			}
			http.Redirect(w, r, canonicalLocationsURL(redirectQuery), http.StatusSeeOther)
			return
		}
	}
	if mapPhotoID == 0 {
		minLat, hasMinLat := parseOptionalFloat(criteria.Get("minLat"))
		minLng, hasMinLng := parseOptionalFloat(criteria.Get("minLng"))
		maxLat, hasMaxLat := parseOptionalFloat(criteria.Get("maxLat"))
		maxLng, hasMaxLng := parseOptionalFloat(criteria.Get("maxLng"))
		if hasMinLat && hasMinLng && hasMaxLat && hasMaxLng &&
			minLat >= -90 && maxLat <= 90 && minLng >= -180 && maxLng <= 180 && minLat <= maxLat && minLng <= maxLng {
			bounds = &MapBounds{MinLatitude: minLat, MinLongitude: minLng, MaxLatitude: maxLat, MaxLongitude: maxLng}
		} else if criteria.Get("minLat") != "" || criteria.Get("minLng") != "" || criteria.Get("maxLat") != "" || criteria.Get("maxLng") != "" {
			http.Error(w, "Invalid map bounds", http.StatusBadRequest)
			return
		}
	}
	if mapPhotoID == 0 && bounds == nil && (selectedSettlement != "" || selectedRegion != "") {
		bounds, err = s.db.LocationBounds(r.Context(), selectedSettlement, selectedRegion)
		if err != nil {
			slog.Error("Failed to load location bounds", "settlement", selectedSettlement, "region", selectedRegion, "error", err)
			http.Error(w, "Could not load location bounds", http.StatusInternalServerError)
			return
		}
	}
	if hasPhoto {
		if bounds == nil {
			http.NotFound(w, r)
			return
		}
		photos, err := s.db.PhotosInBounds(r.Context(),
			bounds.MinLatitude, bounds.MaxLatitude, bounds.MinLongitude, bounds.MaxLongitude)
		if err != nil {
			http.Error(w, "Could not load location collection", http.StatusInternalServerError)
			return
		}
		if !photoInCollection(photos, photoID) {
			http.NotFound(w, r)
			return
		}
		if mapView {
			mapPhotoID = photoID
			photo, err := s.db.PhotoByID(r.Context(), photoID)
			if err != nil || photo.Latitude == nil || photo.Longitude == nil ||
				(*photo.Latitude == 0 && *photo.Longitude == 0) {
				http.NotFound(w, r)
				return
			}
			const padding = 0.001
			bounds = &MapBounds{
				MinLatitude:  *photo.Latitude - padding,
				MaxLatitude:  *photo.Latitude + padding,
				MinLongitude: *photo.Longitude - padding,
				MaxLongitude: *photo.Longitude + padding,
			}
			s.render(w, "locations", PageData{
				Page: "locations", Locations: locationGroups, MapBounds: bounds,
				MapPhotoID: mapPhotoID, MapReturnURL: collectionPath,
			})
			return
		}
		s.renderCollectionPhoto(w, r, photoID, collectionPath, "Locations", photos)
		return
	}
	s.render(w, "locations", PageData{Page: "locations", Locations: locationGroups,
		SelectedLocation: selectedSettlement + selectedRegion, SelectedSettlement: selectedSettlement,
		SelectedRegion: selectedRegion, MapBounds: bounds, MapPhotoID: mapPhotoID, MapReturnURL: r.URL.RequestURI()})
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

// mapPoints responds with JSON spatial cluster points aggregated by map grid cells for the current zoom level.
func (s *Server) mapPoints(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	if photoIDsText := query.Get("photoIds"); photoIDsText != "" {
		photoIDs, err := parseIDList(photoIDsText)
		if err != nil {
			http.Error(w, "Invalid photo IDs", http.StatusBadRequest)
			return
		}
		points, err := s.db.MapPointsForPhotos(r.Context(), photoIDs)
		if err != nil {
			http.Error(w, "Could not load map locations", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(points); err != nil {
			http.Error(w, "Could not encode map locations", http.StatusInternalServerError)
		}
		return
	}
	if photoIDText := query.Get("photoId"); photoIDText != "" {
		photoID, err := strconv.ParseInt(photoIDText, 10, 64)
		if err != nil {
			http.Error(w, "Invalid photo ID", http.StatusBadRequest)
			return
		}
		if photoID > 0 {
			points, err := s.db.MapPointForPhoto(r.Context(), photoID)
			if err != nil {
				http.Error(w, "Could not load map location", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(points); err != nil {
				http.Error(w, "Could not encode map location", http.StatusInternalServerError)
			}
			return
		}
	}
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
	points, err := s.db.MapPoints(r.Context(), minLatitude, maxLatitude, minLongitude, maxLongitude, cellSize, settlement, region)
	if err != nil {
		slog.Error("Failed to load map points", "minLat", minLatitude, "maxLat", maxLatitude,
			"minLng", minLongitude, "maxLng", maxLongitude, "zoom", zoom, "settlement", settlement, "region", region, "error", err)
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
	if photoIDsText := query.Get("photoIds"); photoIDsText != "" {
		photoIDs, err := parseIDList(photoIDsText)
		if err != nil {
			http.Error(w, "Invalid photo IDs", http.StatusBadRequest)
			return
		}
		photos, err := s.db.ClusterPointsForPhotos(r.Context(), photoIDs)
		if err != nil {
			http.Error(w, "Could not load map photos", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(photos); err != nil {
			http.Error(w, "Could not encode map photos", http.StatusInternalServerError)
		}
		return
	}
	if photoIDText := query.Get("photoId"); photoIDText != "" {
		photoID, err := strconv.ParseInt(photoIDText, 10, 64)
		if err != nil {
			http.Error(w, "Invalid photo ID", http.StatusBadRequest)
			return
		}
		if photoID > 0 {
			photos, err := s.db.ClusterPointForPhoto(r.Context(), photoID)
			if err != nil {
				http.Error(w, "Could not load map photo", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(photos); err != nil {
				http.Error(w, "Could not encode map photo", http.StatusInternalServerError)
			}
			return
		}
	}
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
	photos, err := s.db.ClusterPoints(r.Context(), minLatitude, maxLatitude, minLongitude, maxLongitude)
	if err != nil {
		slog.Error("Failed to load cluster photos", "minLat", minLatitude, "maxLat", maxLatitude,
			"minLng", minLongitude, "maxLng", maxLongitude, "error", err)
		http.Error(w, "Could not load cluster photos", http.StatusInternalServerError)
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
	if photoIDText := query.Get("photoId"); photoIDText != "" {
		photoID, err := strconv.ParseInt(photoIDText, 10, 64)
		if err != nil || photoID < 1 {
			return nil, errors.New("invalid photo ID")
		}
		photo, err := s.db.PhotoByID(ctx, photoID)
		if err != nil {
			return nil, err
		}
		if photo.Latitude == nil || photo.Longitude == nil || (*photo.Latitude == 0 && *photo.Longitude == 0) {
			return nil, sql.ErrNoRows
		}
		return []Photo{photo}, nil
	}
	minLatitude, maxLatitude, minLongitude, maxLongitude, err := clusterBounds(query)
	if err != nil {
		return nil, err
	}
	return s.db.PhotosInBounds(ctx, minLatitude, maxLatitude, minLongitude, maxLongitude)
}

// clusterBounds parses bounding box coordinate query parameters minLat, maxLat, minLng, maxLng.
func parseOptionalFloat(value string) (float64, bool) {
	if value == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(value, 64)
	return parsed, err == nil
}

// clusterBounds validates and extracts the latitude and longitude bounds in query.
// Input: map query values; outputs: minimum/maximum coordinates in latitude-longitude order or an error.
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

// updateGalleryDescription saves the optional text displayed on a gallery page.
func (s *Server) updateGalleryDescription(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 256<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Invalid gallery text", http.StatusBadRequest)
		return
	}
	values, ok := r.PostForm["description"]
	if !ok || len(values) != 1 || !utf8.ValidString(values[0]) || utf8.RuneCountInString(values[0]) > 10000 {
		http.Error(w, "Invalid gallery text", http.StatusBadRequest)
		return
	}
	if err := s.db.UpdateGalleryDescription(r.Context(), id, values[0]); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.NotFound(w, r)
		} else {
			slog.Error("Failed to update gallery text", "gallery_id", id, "error", err)
			http.Error(w, "Could not save gallery text", http.StatusInternalServerError)
		}
		return
	}
	http.Redirect(w, r, fmt.Sprintf("/galleries/%d", id), http.StatusSeeOther)
}

// galleryPage renders a user-created gallery and optional photo detail.
func (s *Server) galleryPage(w http.ResponseWriter, r *http.Request) {
	collections, err := s.db.Collections(r.Context())
	if err != nil {
		slog.Error("Failed to load collections", "error", err)
		http.Error(w, "Could not load collections", http.StatusInternalServerError)
		return
	}

	if len(collections) == 0 {
		s.render(w, "collection", PageData{Collections: collections, Collection: nil, Photos: nil, ReturnURL: r.URL.Path})
		return
	}

	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if collectionPath == "/galleries" || collectionPath == "/galleries/" {
		s.render(w, "collection", PageData{
			Page: "galleries", Collections: collections, Collection: nil,
			Photos: nil, ReturnURL: r.URL.Path,
		})
		return
	}
	prefix := "/galleries/"
	if strings.HasPrefix(collectionPath, "/gallery/") {
		prefix = "/gallery/"
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(collectionPath, prefix), 10, 64)
	if err != nil || id < 0 {
		slog.Error("Invalid collection ID", "error", err)
		slog.Info("Redirecting to first collection", "collectionID", collections[0].ID)
		http.Redirect(w, r, fmt.Sprintf("/galleries/%d", collections[0].ID), http.StatusSeeOther)
		return
	}
	if id == SelectedCollectionID {
		target := "/selected"
		if hasPhoto {
			target += fmt.Sprintf("/photo/%d", photoID)
		}
		if mapView {
			target += "/map"
		}
		http.Redirect(w, r, target, http.StatusSeeOther)
		return
	}
	collection, err := s.db.CollectionByID(r.Context(), id)
	if err != nil {
		slog.Error("Failed to load collection", "collection_id", id, "error", err)
		http.NotFound(w, r)
		return
	}
	photos, err := s.db.PhotosForCollection(r.Context(), id)
	if err != nil {
		slog.Error("Failed to load collection photos", "collection_id", id, "error", err)
		http.Error(w, "Could not load collection", http.StatusInternalServerError)
		return
	}
	if mapView {
		if hasPhoto {
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, fmt.Sprintf("/galleries/%d", id), photos)
		return
	}
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, fmt.Sprintf("/galleries/%d", id), collection.Name, photos)
		return
	}
	selected, err := s.db.SelectedPhotoIDs(r.Context())
	if err != nil {
		slog.Error("Failed to load selected photos", "error", err)
		http.Error(w, "Could not load selected photos", http.StatusInternalServerError)
		return
	}
	s.render(w, "collection", PageData{Page: "galleries", Collections: collections, Collection: &collection, Photos: photos, Selected: selected, ReturnURL: r.URL.Path})
}

// similarCollection renders a conceptual collection of photos similar to a target photo.
func (s *Server) similarCollection(w http.ResponseWriter, r *http.Request) {
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	targetText := strings.TrimPrefix(collectionPath, "/similar/")
	targetID, err := strconv.ParseInt(targetText, 10, 64)
	if err != nil || targetID < 1 {
		http.Error(w, "Invalid photo ID", http.StatusBadRequest)
		return
	}
	if _, err := s.db.PhotoByID(r.Context(), targetID); err != nil {
		http.NotFound(w, r)
		return
	}
	photos, err := s.db.SimilarPhotos(r.Context(), targetID, 100)
	if err != nil {
		slog.Error("Failed to load similar photos", "photo_id", targetID, "error", err)
		http.Error(w, "Could not load similar photos", http.StatusInternalServerError)
		return
	}
	if mapView {
		if hasPhoto {
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, fmt.Sprintf("/similar/%d", targetID), photos)
		return
	}
	collectionURL := fmt.Sprintf("/similar/%d", targetID)
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, collectionURL, "Similar to", photos)
		return
	}
	s.render(w, "gallery-full", PageData{Page: "gallery", Photos: photos, ReturnURL: collectionURL, CollectionName: "Similar to"})
}

// randomCollection redirects to a random photo or renders that photo within a three-photo random collection.
func (s *Server) randomCollection(w http.ResponseWriter, r *http.Request) {
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if collectionPath == "/random" || collectionPath == "/random/" {
		if hasPhoto {
			if _, err := s.db.PhotoByID(r.Context(), photoID); err != nil {
				http.NotFound(w, r)
				return
			}
			targetID := photoID
			photos, err := s.db.RandomCollectionPhotos(r.Context(), targetID)
			if err != nil {
				slog.Error("Failed to load random collection", "photo_id", targetID, "error", err)
				http.Error(w, "Could not load random collection", http.StatusInternalServerError)
				return
			}
			if mapView {
				s.renderCollectionMap(w, r, fmt.Sprintf("/random/%d", targetID), photos)
				return
			}
			s.renderCollectionPhoto(w, r, targetID, "/random/"+strconv.FormatInt(targetID, 10),
				"Random", photos)
			return
		}
		targetID, err := s.db.RandomPhotoID(r.Context())
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.NotFound(w, r)
				return
			}
			slog.Error("Failed to choose random photo", "error", err)
			http.Error(w, "Could not choose a random photo", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, fmt.Sprintf("/random/photo/%d", targetID), http.StatusSeeOther)
		return
	}
	http.NotFound(w, r)
}

// nearCollection renders a conceptual collection of photos within 50 km of a target photo.
func (s *Server) nearCollection(w http.ResponseWriter, r *http.Request) {
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	targetText := strings.TrimPrefix(collectionPath, "/near-to/")
	targetID, err := strconv.ParseInt(targetText, 10, 64)
	if err != nil || targetID < 1 {
		http.Error(w, "Invalid photo ID", http.StatusBadRequest)
		return
	}
	if _, err := s.db.PhotoByID(r.Context(), targetID); err != nil {
		http.NotFound(w, r)
		return
	}
	photos, err := s.db.NearPhotos(r.Context(), targetID, 100)
	if err != nil {
		slog.Error("Failed to load nearby photos", "photo_id", targetID, "error", err)
		http.Error(w, "Could not load nearby photos", http.StatusInternalServerError)
		return
	}
	if mapView {
		if hasPhoto {
			filtered := make([]Photo, 0, 1)
			for _, photo := range photos {
				if photo.ID == photoID {
					filtered = append(filtered, photo)
					break
				}
			}
			photos = filtered
		}
		s.renderCollectionMap(w, r, fmt.Sprintf("/near-to/%d", targetID), photos)
		return
	}
	collectionURL := fmt.Sprintf("/near-to/%d", targetID)
	if hasPhoto {
		s.renderCollectionPhoto(w, r, photoID, collectionURL, "Near to", photos)
		return
	}
	s.render(w, "gallery-full", PageData{Page: "gallery", Photos: photos, ReturnURL: collectionURL, CollectionName: "Near to"})
}

// addToCollection is retained as a compatibility wrapper for older callers.
func (s *Server) addToCollection(w http.ResponseWriter, r *http.Request) {
	s.addToGallery(w, r)
}

// addToGallery adds selected photo IDs to an existing or newly created custom gallery.
func (s *Server) addToGallery(w http.ResponseWriter, r *http.Request) {
	ids := r.FormValue("ids")
	if ids == "" {
		slog.Error("No photo IDs provided for gallery addition")
		http.Error(w, "Select at least one photo", http.StatusBadRequest)
		return
	}
	galleryIDInput := r.FormValue("gallery_id")
	if galleryIDInput == "" {
		galleryIDInput = r.FormValue("collection_id")
	}
	galleryID, err := s.selectCollection(r.Context(), galleryIDInput, r.FormValue("name"))
	if err != nil {
		slog.Error("Failed to select or create gallery", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	photoIDs, err := parseIDList(ids)
	if err != nil {
		slog.Error("Invalid photo IDs provided for gallery addition", "ids", ids, "error", err)
		http.Error(w, "Invalid photo selection", http.StatusBadRequest)
		return
	}
	added, err := s.db.AddPhotosToCollection(r.Context(), galleryID, photoIDs)
	if err != nil {
		slog.Error("Failed to add photos to gallery", "collection_id", galleryID, "error", err)
		http.Error(w, "Could not add photos to gallery", http.StatusInternalServerError)
		return
	}
	if added == 0 {
		slog.Error("No selected photos could be added to the gallery", "collection_id", galleryID)
		http.Error(w, "No selected photos could be added to the gallery", http.StatusBadRequest)
		return
	}
	redirect := fmt.Sprintf("/galleries/%d", galleryID)
	if galleryID == SelectedCollectionID {
		redirect = "/selected"
	}
	w.Header().Set("HX-Redirect", redirect)
	w.WriteHeader(http.StatusNoContent)
}

// removeFromCollection preserves the legacy endpoint handler by forwarding to removeFromGallery.
// Inputs: the HTTP response writer and request; output: the gallery-removal HTTP response.
func (s *Server) removeFromCollection(w http.ResponseWriter, r *http.Request) {
	s.removeFromGallery(w, r)
}

// removeFromGallery removes submitted photo IDs from the selected gallery.
// Inputs: the HTTP response writer and a request containing gallery_id and ids; output: an HTTP status/redirect response.
func (s *Server) removeFromGallery(w http.ResponseWriter, r *http.Request) {
	ids := r.FormValue("ids")
	if ids == "" {
		slog.Error("No photo IDs provided for gallery deletion")
		http.Error(w, "Select at least one photo to remove from a gallery", http.StatusBadRequest)
		return
	}
	galleryIDInput := r.FormValue("gallery_id")
	if galleryIDInput == "" {
		galleryIDInput = r.FormValue("collection_id")
	}
	galleryID, err := s.selectCollection(r.Context(), galleryIDInput, "")
	if err != nil {
		slog.Error("Failed to select gallery to delete item from", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	photoIDs, err := parseIDList(ids)
	if err != nil {
		slog.Error("Invalid photo IDs provided for gallery deletion", "ids", ids, "error", err)
		http.Error(w, "Invalid photo selection", http.StatusBadRequest)
		return
	}
	err = s.db.RemovePhotosFromCollection(r.Context(), galleryID, photoIDs)
	if err != nil {
		slog.Error("Failed to remove photos from gallery", "collection_id", galleryID, "error", err)
		http.Error(w, "Could not remove photos from gallery", http.StatusInternalServerError)
		return
	}
	redirect := fmt.Sprintf("/galleries/%d", galleryID)
	if galleryID == SelectedCollectionID {
		redirect = "/selected"
	}
	w.Header().Set("HX-Redirect", redirect)
	w.WriteHeader(http.StatusNoContent)
}

// addToCollection adds selected photo IDs to an existing or newly created custom gallery.
func (s *Server) deleteCollection(w http.ResponseWriter, r *http.Request) {
	s.deleteGallery(w, r)
}

// deleteGallery deletes the gallery identified by the request path unless it is protected.
// Inputs: the HTTP response writer and request; output: an HTTP status and redirect response.
func (s *Server) deleteGallery(w http.ResponseWriter, r *http.Request) {
	collectionID := strings.TrimPrefix(r.URL.Path, "/galleries/")
	collectionID = strings.TrimPrefix(collectionID, "/")
	galleryID, err := s.selectCollection(r.Context(), collectionID, "")
	if err != nil {
		slog.Error("Failed to select collection to remove", "error", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.db.DeleteCollection(r.Context(), galleryID); err != nil {
		if errors.Is(err, ErrProtectedCollection) {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		slog.Error("Failed to delete collection", "collection_id", galleryID, "error", err)
		http.Error(w, "Failed to delete collection", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/galleries")
	w.WriteHeader(http.StatusNoContent)
}

// selectPhotos adds photo IDs to the selected collection.
func (s *Server) selectPhotos(w http.ResponseWriter, r *http.Request) {
	s.changeSelection(w, r, s.db.SelectPhotos)
}

// unselectPhotos removes photo IDs from the selected collection.
func (s *Server) unselectPhotos(w http.ResponseWriter, r *http.Request) {
	s.changeSelection(w, r, s.db.UnselectPhotos)
}

// changeSelection parses submitted photo IDs and applies the supplied selection update.
// Inputs: response writer, request, and database update callback; output: an HTTP status response.
func (s *Server) changeSelection(w http.ResponseWriter, r *http.Request, change func(context.Context, []int64) error) {
	ids := strings.TrimSpace(r.FormValue("ids"))
	if ids == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	photoIDs, err := parseIDList(ids)
	if err != nil {
		slog.Error("Invalid photo IDs provided for selection", "error", err)
		http.Error(w, "Invalid photo selection", http.StatusBadRequest)
		return
	}
	if err := change(r.Context(), photoIDs); err != nil {
		slog.Error("Failed to update selection", "error", err)
		http.Error(w, "Could not update selection", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clearSelection empties the selected collection.
func (s *Server) removeSelectedGalleryPhotos(w http.ResponseWriter, r *http.Request) {
	ids := strings.TrimSpace(r.FormValue("ids"))
	if ids == "" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	photoIDs, err := parseIDList(ids)
	if err != nil {
		http.Error(w, "Invalid photo selection", http.StatusBadRequest)
		return
	}
	if err := s.db.UnselectPhotos(r.Context(), photoIDs); err != nil {
		slog.Error("Failed to remove photos from selected gallery", "error", err)
		http.Error(w, "Could not remove photos from selected gallery", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// clearSelection removes every photo from the current user's persisted selection.
// Inputs: the HTTP response writer and request; output: an HTTP status response.
func (s *Server) clearSelection(w http.ResponseWriter, r *http.Request) {
	if err := s.db.ClearSelection(r.Context()); err != nil {
		slog.Error("Failed to clear selection", "error", err)
		http.Error(w, "Could not clear selection", http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// saveSearch persists the criteria represented by a canonical search URL.
func (s *Server) saveSearch(w http.ResponseWriter, r *http.Request) {
	searchURL := strings.TrimSpace(r.FormValue("url"))
	if !strings.HasPrefix(searchURL, "/search") {
		http.Error(w, "Invalid search URL", http.StatusBadRequest)
		return
	}
	parsed, err := url.Parse(searchURL)
	if err != nil || parsed.IsAbs() || parsed.Host != "" {
		http.Error(w, "Invalid search URL", http.StatusBadRequest)
		return
	}

	var title string
	if strings.HasPrefix(parsed.Path, "/search/") {
		criteria := strings.TrimPrefix(parsed.Path, "/search/")
		query, err := parseSearchCollectionCriteria(criteria)
		if err != nil {
			http.Error(w, "Invalid search URL", http.StatusBadRequest)
			return
		}
		title = searchTitle(query)
	} else {
		title = searchTitle(parsed.Query())
	}

	if _, err := s.db.SaveSearch(r.Context(), parsed.Path, title); err != nil {
		slog.Error("Failed to save search", "url", parsed.Path, "error", err)
		http.Error(w, "Could not save search", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", parsed.Path)
	w.WriteHeader(http.StatusNoContent)
}

// deleteSavedSearch removes saved search criteria by ID.
func (s *Server) deleteSavedSearch(w http.ResponseWriter, r *http.Request) {
	idText := strings.TrimPrefix(r.URL.Path, "/saved-searches/")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id < 1 {
		http.Error(w, "Invalid saved search ID", http.StatusBadRequest)
		return
	}
	if err := s.db.DeleteSavedSearch(r.Context(), id); err != nil {
		slog.Error("Failed to delete saved search", "id", id, "error", err)
		http.Error(w, "Could not delete saved search", http.StatusInternalServerError)
		return
	}
	w.Header().Set("HX-Redirect", "/search")
	w.WriteHeader(http.StatusNoContent)
}

// selectCollection validates an existing gallery ID or creates a new named gallery record.
func (s *Server) selectCollection(ctx context.Context, existingID, name string) (int64, error) {
	if existingID != "" {
		id, err := strconv.ParseInt(existingID, 10, 64)
		if err != nil || id < 0 {
			slog.Error("Invalid collection ID provided", "collection_id", existingID, "error", err)
			return 0, fmt.Errorf("invalid collection")
		}
		found, err := s.db.CollectionExists(ctx, id)
		if err != nil {
			slog.Error("Failed to look up collection existence", "collection_id", id, "error", err)
			return 0, fmt.Errorf("look up collections: %w", err)
		}
		if !found {
			slog.Error("Collection does not exist", "collection_id", id)
			return 0, fmt.Errorf("collection does not exist")
		}
		return id, nil
	}
	name = strings.TrimSpace(name)
	if name == "" {
		slog.Error("No collection name provided for new collection creation")
		return 0, fmt.Errorf("enter a new collection name or choose an existing collection")
	}
	if len(name) > 200 {
		slog.Error("Collection name exceeds maximum length of 200 characters", "collection_name_length", len(name))
		return 0, fmt.Errorf("collection name must be 200 characters or fewer")
	}
	id, err := s.db.CreateCollection(ctx, name)
	if err != nil {
		slog.Error("Failed to create new collection", "collection_name", name, "error", err)
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
		// Indexing takes priority: stop detection before it starts.
		s.stopDetection()
		count, err := index(ctx)
		s.finishIndexing(count, err)
		s.startDetection()
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
	progress := s.indexProgress
	s.indexProgressMu.RUnlock()
	s.detectMu.Lock()
	progress.Detection = s.detectProgress
	s.detectMu.Unlock()
	return progress
}

// updateDetectionProgress stores object-detection counts and their computed percentage.
// Inputs: processed and total photo counts; output: none.
func (s *Server) updateDetectionProgress(processed, total int) {
	s.detectMu.Lock()
	s.detectProgress.Processed = processed
	s.detectProgress.Total = total
	s.detectProgress.Percentage = percentage(processed, total)
	s.detectProgress.Message = "Object detection in progress..."
	s.detectMu.Unlock()
}

// updateQualityProgress stores quality-assessment counts and their computed percentage.
func (s *Server) updateQualityProgress(processed, total int) {
	s.detectMu.Lock()
	s.detectProgress.Processed = processed
	s.detectProgress.Total = total
	s.detectProgress.Percentage = percentage(processed, total)
	s.detectProgress.Message = "Quality assessment in progress..."
	s.detectMu.Unlock()
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
	path, err := s.db.PhotoPath(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, filepath.Join(s.config.PhotoRoot, path))
}

// rawFile serves raw camera photo files as attachment downloads.
func (s *Server) rawFile(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/raw/")
	rawPath, err := s.db.PhotoRawPath(r.Context(), id)
	if err != nil || rawPath == "" {
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
	thumbnail, path, err := s.db.PhotoThumbnail(r.Context(), id)
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
	photoIDs, err := parseIDList(r.URL.Query().Get("ids"))
	if err != nil {
		slog.Error("Download request rejected because of invalid photo ids: ", "ids", r.URL.Query().Get("ids"), "err", err)
		http.Error(w, "Invalid photo ids", http.StatusBadRequest)
		return
	}

	filename := fmt.Sprintf("fast-fotos-selection-%s.zip", time.Now().Format("2006-01-02-150405"))
	s.writeZip(w, r, photoIDs, filename)
}

// downloadGallery streams a zip of every photo in a gallery, adding raw files when raw=true.
// Inputs: a /galleries/{id}/download request; output: a zip attachment or an HTTP error.
func (s *Server) downloadGallery(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id < 0 {
		http.NotFound(w, r)
		return
	}
	name := "selected"
	if id != SelectedCollectionID {
		collection, err := s.db.CollectionByID(r.Context(), id)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		name = collection.Name
	}
	photos, err := s.db.PhotosForCollection(r.Context(), id)
	if err != nil {
		http.Error(w, "Could not load gallery", http.StatusInternalServerError)
		return
	}
	if len(photos) == 0 {
		http.Error(w, "Gallery has no photos", http.StatusNotFound)
		return
	}
	ids := make([]int64, len(photos))
	for i, photo := range photos {
		ids[i] = photo.ID
	}
	s.writeZip(w, r, ids, "fast-fotos-"+safeFilename(name)+".zip")
}

// safeFilename reduces a name to characters that are safe in a Content-Disposition filename.
func safeFilename(name string) string {
	cleaned := strings.Map(func(r rune) rune {
		if r < 128 && (r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
			return r
		}
		return '-'
	}, strings.TrimSpace(name))
	if strings.Trim(cleaned, "-") == "" {
		return "gallery"
	}
	return cleaned
}

// writeZip sends a single file directly, or streams multiple files as a zip attachment.
func (s *Server) writeZip(w http.ResponseWriter, r *http.Request, photoIDs []int64, filename string) {
	includeRaw := r.URL.Query().Get("raw") == "true"
	files, err := s.db.PhotoFilePaths(r.Context(), photoIDs)
	if err != nil {
		slog.Error("Download request rejected because of database query error: ", "err", err)
		http.Error(w, "Invalid selection", http.StatusBadRequest)
		return
	}
	type downloadItem struct {
		path      string
		processed bool
	}
	var items []downloadItem
	seen := make(map[string]bool)
	for _, file := range files {
		base := strings.TrimSuffix(file.Path, filepath.Ext(file.Path))
		paths := []string{file.Path, base + ".xmp", base + ".json"}
		if includeRaw && file.RawPath != "" {
			paths = append(paths, file.RawPath)
		}
		for i, path := range paths {
			fullPath, relative, err := downloadPath(s.config.PhotoRoot, path)
			if err != nil {
				http.Error(w, "Invalid file path", http.StatusBadRequest)
				return
			}
			if seen[relative] {
				continue
			}
			info, err := os.Stat(fullPath)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil || !info.Mode().IsRegular() {
				slog.Error("download file unavailable", "path", path, "error", err)
				http.Error(w, "Could not read download", http.StatusInternalServerError)
				return
			}
			seen[relative] = true
			items = append(items, downloadItem{path: path, processed: i == 0 && r.URL.Query().Get("processed") == "true"})
		}
	}
	if len(items) == 0 {
		http.Error(w, "No files available to download", http.StatusNotFound)
		return
	}
	if len(items) == 1 {
		item := items[0]
		name, file, data, err := prepareDownloadFile(r.Context(), s.config.PhotoRoot, item.path, item.processed)
		if err != nil || file == nil {
			slog.Error("download file unavailable", "path", item.path, "error", err)
			http.Error(w, "Could not read download", http.StatusInternalServerError)
			return
		}
		defer file.Close()
		base := filepath.Base(name)
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": base}))
		if contentType := mime.TypeByExtension(filepath.Ext(base)); contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		var contents io.ReadSeeker = file
		if data != nil {
			contents = bytes.NewReader(data)
		}
		http.ServeContent(w, r, base, time.Time{}, contents)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
	archive := zip.NewWriter(w)
	defer archive.Close()
	for _, item := range items {
		if err := addFileToArchiveWithProcessing(r.Context(), archive, s.config.PhotoRoot, item.path, item.processed); err != nil {
			slog.Error("Download request failed while adding files to archive: ", "err", err)
			return
		}
	}
}

// parseIDList parses a comma-separated list of positive integer IDs.
func parseIDList(value string) ([]int64, error) {
	var ids []int64
	for _, part := range strings.Split(value, ",") {
		id, err := strconv.ParseInt(strings.TrimSpace(part), 10, 64)
		if err != nil || id < 1 {
			return nil, fmt.Errorf("invalid photo id %q", part)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// addDownloadFiles adds an image, its sidecar files (.xmp, .json), and optional raw file to a zip archive.
func (s *Server) addDownloadFiles(archive *zip.Writer, photoPath, rawPath string, includeRaw bool) error {
	return s.addDownloadFilesWithProcessing(context.Background(), archive, photoPath, rawPath, includeRaw, false)
}

func (s *Server) addDownloadFilesWithProcessing(ctx context.Context, archive *zip.Writer, photoPath, rawPath string, includeRaw, processed bool) error {
	base := strings.TrimSuffix(photoPath, filepath.Ext(photoPath))
	for i, extension := range []string{filepath.Ext(photoPath), ".xmp", ".json"} {
		path := base + extension
		if err := addFileToArchiveWithProcessing(ctx, archive, s.config.PhotoRoot, path, i == 0 && processed); err != nil {
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
	return addFileToArchiveWithProcessing(context.Background(), archive, root, relativePath, false)
}

func downloadPath(root, relativePath string) (string, string, error) {
	root, err := filepath.Abs(root)
	if err != nil {
		return "", "", err
	}
	fullPath, err := filepath.Abs(filepath.Join(root, relativePath))
	if err != nil {
		return "", "", err
	}
	relative, err := filepath.Rel(root, fullPath)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", "", fmt.Errorf("file path escapes photo root: %q", relativePath)
	}
	return fullPath, relative, nil
}

func prepareDownloadFile(ctx context.Context, root, relativePath string, processed bool) (string, *os.File, []byte, error) {
	fullPath, relative, err := downloadPath(root, relativePath)
	if err != nil {
		return "", nil, nil, err
	}
	file, err := os.Open(fullPath)
	if os.IsNotExist(err) {
		return "", nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, err
	}
	ext := strings.ToLower(filepath.Ext(relativePath))
	if processed && (ext == ".jpg" || ext == ".jpeg" || ext == ".png") {
		info, statErr := file.Stat()
		if statErr != nil {
			file.Close()
			return "", nil, nil, statErr
		}
		if info.Size() <= 128<<20 {
			data, readErr := io.ReadAll(file)
			if readErr != nil {
				file.Close()
				return "", nil, nil, readErr
			}
			enhanced, enhanceErr := enhancePhoto(ctx, data)
			if ctx.Err() != nil {
				file.Close()
				return "", nil, nil, ctx.Err()
			}
			if enhanceErr == nil {
				ext := filepath.Ext(relative)
				return strings.TrimSuffix(relative, ext) + "-processed" + ext, file, enhanced, nil
			}
			slog.Warn("could not process photo; including original", "path", relativePath, "error", enhanceErr)
			if _, err := file.Seek(0, io.SeekStart); err != nil {
				file.Close()
				return "", nil, nil, err
			}
		} else {
			slog.Warn("photo too large to process; including original", "path", relativePath)
		}
	}
	return relative, file, nil, nil
}

func addFileToArchiveWithProcessing(ctx context.Context, archive *zip.Writer, root, relativePath string, processed bool) error {
	name, file, data, err := prepareDownloadFile(ctx, root, relativePath, processed)
	if err != nil || file == nil {
		return err
	}
	defer file.Close()
	entry, err := archive.Create(name)
	if err != nil {
		return err
	}
	if data != nil {
		_, err = entry.Write(data)
		return err
	}
	_, err = io.Copy(entry, file)
	return err
}

// photosForThisWeek loads photos captured within three days of today in any year.
func (s *Server) photosForThisWeek(ctx context.Context) ([]Photo, error) {

	//get the day of year for the current date
	dayOfYear := time.Now().YearDay()

	//the week will consist of 3 days before the curreny dayOfYear and 3 days after
	//but need to take into account days at the start and end of the year
	startOfWeek := (dayOfYear - 3) % 365
	endOfWeek := (dayOfYear + 3) % 365

	//get all the photo ids where day_of_year field is bettween startOfWeek and endOfWeek
	//and put them in an array
	photoIDs, err := s.db.PhotoIDsForDayRange(ctx, startOfWeek, endOfWeek)
	if err != nil {
		slog.Error("Error querying photos for this week: ", "err", err)
		return nil, err
	}
	photos, err := s.db.PhotosByIDs(ctx, photoIDs)
	if err != nil {
		return nil, err
	}
	return selectWeeklyPhotos(photos, weeklyPhotoLimit), nil
}

// weeklyPhotoLimit is the maximum number of photos in the this-week collection.
const weeklyPhotoLimit = 100

var weeklyAnimals = map[string]bool{
	"bird": true, "cat": true, "dog": true, "horse": true, "sheep": true,
	"cow": true, "elephant": true, "bear": true, "zebra": true, "giraffe": true,
}

// weeklyQuality returns a 0-1 technical quality estimate from whichever scores exist (0.5 if none).
func weeklyQuality(p Photo) float64 {
	sum, n := 0.0, 0
	if p.Sharpness != nil {
		sum += math.Min(1, math.Log1p(math.Max(0, *p.Sharpness))/math.Log1p(300))
		n++
	}
	if p.Exposure != nil {
		sum += math.Max(0, 1-math.Abs(*p.Exposure-0.5)*2)
		n++
	}
	if p.Contrast != nil {
		sum += math.Min(1, math.Max(0, *p.Contrast))
		n++
	}
	if p.Brisque != nil {
		sum += math.Min(1, math.Max(0, (100-*p.Brisque)/100))
		n++
	}
	if n == 0 {
		return 0.5
	}
	return sum / float64(n)
}

// weeklyPoorQuality reports whether a photo is clearly blurry, badly exposed or low quality.
func weeklyPoorQuality(p Photo) bool {
	return qualityBandName("sharpness_score", p.Sharpness) == "blurry" ||
		qualityBandName("exposure_score", p.Exposure) != "" && qualityBandName("exposure_score", p.Exposure) != "good" ||
		qualityBandName("brisque_score", p.Brisque) == "poor"
}

// selectWeeklyPhotos chooses up to limit photos, favoring people and animals, technical quality,
// golden-hour shots and subject variety. Clearly poor photos are dropped while enough others remain.
// Inputs: candidate photos and a maximum count; output: the chosen photos in chronological order.
func selectWeeklyPhotos(photos []Photo, limit int) []Photo {
	if limit <= 0 || len(photos) == 0 {
		return nil
	}
	candidates := append([]Photo(nil), photos...)
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].TakenAt.Equal(candidates[j].TakenAt) {
			return candidates[i].ID < candidates[j].ID
		}
		return candidates[i].TakenAt.Before(candidates[j].TakenAt)
	})
	good := make([]Photo, 0, len(candidates))
	for _, photo := range candidates {
		if !weeklyPoorQuality(photo) {
			good = append(good, photo)
		}
	}
	if len(good) >= limit {
		candidates = good
	}

	selected := make([]Photo, 0, min(limit, len(candidates)))
	used := make([]bool, len(candidates))
	subjects := make(map[string]bool)
	for len(selected) < limit && len(selected) < len(candidates) {
		bestIndex, bestScore := -1, math.Inf(-1)
		for i, photo := range candidates {
			if used[i] {
				continue
			}
			score := 2 * weeklyQuality(photo)
			if photo.GoldenHour {
				score += 0.5
			}
			seenInPhoto := make(map[string]bool)
			for _, subject := range photo.Objects {
				subject = strings.ToLower(strings.TrimSpace(subject))
				if subject == "" || seenInPhoto[subject] {
					continue
				}
				seenInPhoto[subject] = true
				switch {
				case subject == "person" && subjects[subject]:
					score += 1.5
				case subject == "person":
					score += 3
				case weeklyAnimals[subject] && subjects[subject]:
					score += 1
				case weeklyAnimals[subject]:
					score += 2
				case !subjects[subject]:
					score++
				}
			}
			for _, other := range selected {
				if d := photo.TakenAt.Sub(other.TakenAt); d < 5*time.Minute && d > -5*time.Minute {
					score -= 0.5
				}
			}
			if score > bestScore {
				bestIndex, bestScore = i, score
			}
		}

		photo := candidates[bestIndex]
		used[bestIndex] = true
		selected = append(selected, photo)
		for _, subject := range photo.Objects {
			subject = strings.ToLower(strings.TrimSpace(subject))
			if subject != "" {
				subjects[subject] = true
			}
		}
	}
	sort.Slice(selected, func(i, j int) bool {
		if selected[i].TakenAt.Equal(selected[j].TakenAt) {
			return selected[i].ID < selected[j].ID
		}
		return selected[i].TakenAt.Before(selected[j].TakenAt)
	})
	return selected
}

// render executes a named HTML template with PageData context and writes the output response.
func (s *Server) render(w http.ResponseWriter, name string, data PageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	galleries := make([]Collection, 0, len(data.Collections))
	for _, gallery := range data.Collections {
		if gallery.ID != SelectedCollectionID {
			galleries = append(galleries, gallery)
		}
	}
	data.Collections = galleries
	if data.Selected == nil {
		selected, err := s.db.SelectedPhotoIDs(context.Background())
		if err != nil {
			slog.Error("load selected photos", "error", err)
		}
		data.Selected = selected
	}
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
		"qualityBand":      qualityBandName,
		"deref":            func(v *float64) float64 { return *v },
		"imagePath":        imagePath,
		"join":             strings.Join,
		"urlquery":         url.QueryEscape,
	}
	pages := map[string][]string{
		"home":         {"templates/base.html", "templates/home.html", "templates/gallery.html"},
		"image":        {"templates/base.html", "templates/image.html", "templates/gallery.html"},
		"search":       {"templates/base.html", "templates/search.html", "templates/gallery.html"},
		"maintenance":  {"templates/base.html", "templates/maintenance.html"},
		"gallery":      {"templates/gallery.html"},
		"collections":  {"templates/base.html", "templates/collections.html"},
		"collection":   {"templates/base.html", "templates/collection.html", "templates/gallery.html"},
		"selected":     {"templates/base.html", "templates/selected.html", "templates/gallery.html"},
		"gallery-full": {"templates/base.html", "templates/gallery_full.html", "templates/gallery.html"},
		"locations":    {"templates/base.html", "templates/locations.html"},
		"cluster":      {"templates/base.html", "templates/cluster.html", "templates/gallery.html"},
		"thisweek":     {"templates/base.html", "templates/thisweek.html", "templates/gallery.html"},
		"events":       {"templates/base.html", "templates/events.html", "templates/gallery.html"},
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
		return "/photo/" + strconv.FormatInt(id, 10)
	}
	if parsed, err := url.Parse(returnURL); err == nil {
		returnURL = parsed.Path
	} else if queryIndex := strings.Index(returnURL, "?"); queryIndex >= 0 {
		returnURL = returnURL[:queryIndex]
	}
	if collectionPath, _, hasPhoto, valid := splitCollectionPhotoPath(returnURL); valid && hasPhoto {
		returnURL = collectionPath
	}
	return strings.TrimSuffix(returnURL, "/") + "/photo/" + strconv.FormatInt(id, 10)
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
