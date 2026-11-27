package app

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestParseTemplates tests parsing all embedded template files.
func TestRemoveSelectedGalleryPhoto(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.SelectPhotos(ctx, []int64{19}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	request := httptest.NewRequest(http.MethodPost, "/galleries/0/remove", strings.NewReader("ids=19"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.removeSelectedGalleryPhotos(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("remove selected status = %d", response.Code)
	}
	selected, err := db.SelectedPhotoIDs(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if selected[19] {
		t.Fatal("photo 19 remained selected")
	}
}

func TestSelectedGalleryRoutes(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	latitude, longitude := 51.5, -0.12
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "selected.jpg", TakenAt: now, IndexedAt: now, MediaType: "photo", Latitude: &latitude, Longitude: &longitude}); err != nil {
		t.Fatal(err)
	}
	if err := db.SelectPhotos(ctx, []int64{1}); err != nil {
		t.Fatal(err)
	}
	galleryID, err := db.CreateGallery(ctx, "ordinary")
	if err != nil || galleryID != 1 {
		t.Fatalf("ordinary gallery = %d, %v; want ID 1", galleryID, err)
	}
	if _, err := db.AddPhotosToGallery(ctx, galleryID, []int64{1}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	routes := server.Routes()
	defer server.dlnaCancel()

	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/selected", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "selected.jpg") ||
		!strings.Contains(response.Body.String(), `href="/selected/photo/1"`) {
		t.Fatalf("selected page = %d, body %s", response.Code, response.Body.String())
	}
	for _, path := range []string{"/selected/photo/1", "/galleries/1"} {
		response = httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "selected.jpg") {
			t.Errorf("%s = %d, body %s", path, response.Code, response.Body.String())
		}
	}
	response = httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/selected/photo/1", nil))
	if !strings.Contains(response.Body.String(), `href="/selected"`) || !strings.Contains(response.Body.String(), `href="/selected/photo/1/map"`) {
		t.Fatalf("selected photo lost its navigation context: %s", response.Body.String())
	}
	for _, path := range []string{"/selected/map", "/selected/photo/1/map"} {
		response = httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), `id="photo-map"`) {
			t.Errorf("%s = %d, body %s", path, response.Code, response.Body.String())
		}
	}
	for _, path := range []string{"/galleries", "/galleries/1", "/search"} {
		response = httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		body := response.Body.String()
		if response.Code != http.StatusOK || strings.Contains(body, `href="/galleries/0"`) || strings.Contains(body, `<option value="0">`) {
			t.Errorf("%s exposes selected gallery: %d, body %s", path, response.Code, body)
		}
	}
	for path, location := range map[string]string{
		"/galleries/0": "/selected", "/gallery/0/photo/1/map": "/selected/photo/1/map",
	} {
		response = httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != location {
			t.Errorf("%s redirect = %d, %q; want %q", path, response.Code, response.Header().Get("Location"), location)
		}
	}
	response = httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodDelete, "/galleries/0", nil))
	if response.Code != http.StatusForbidden {
		t.Fatalf("delete selected status = %d, want 403", response.Code)
	}
	response = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/galleries/0/remove", strings.NewReader("ids=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	routes.ServeHTTP(response, request)
	selected, err := db.SelectedPhotoIDs(ctx)
	if response.Code != http.StatusNoContent || err != nil || len(selected) != 0 {
		t.Fatalf("remove selected = %d, %v, %v", response.Code, selected, err)
	}
	photos, err := db.PhotosForGallery(ctx, galleryID)
	if err != nil || len(photos) != 1 {
		t.Fatalf("ordinary gallery after unselect = %+v, %v", photos, err)
	}
	response = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/gallery", strings.NewReader("gallery_id=0&ids=1"))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	routes.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent || response.Header().Get("HX-Redirect") != "/selected" {
		t.Fatalf("add to selected = %d, redirect %q", response.Code, response.Header().Get("HX-Redirect"))
	}
}

func TestSelectedAndGalleryListWhenEmpty(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	routes := server.Routes()
	defer server.dlnaCancel()
	for path, message := range map[string]string{
		"/selected":  "No photos found",
		"/galleries": "No galleries yet.",
	} {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), message) ||
			strings.Contains(response.Body.String(), `href="/galleries/0"`) {
			t.Errorf("%s = %d, body %s", path, response.Code, response.Body.String())
		}
	}
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/selected/photo/99", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("unknown selected photo = %d, want 404", response.Code)
	}
}

func TestChangeSelectionIgnoresEmptyIDs(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := &Server{db: db}
	request := httptest.NewRequest(http.MethodPost, "/selection", strings.NewReader("ids="))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response := httptest.NewRecorder()
	server.changeSelection(response, request, db.SelectPhotos)
	if response.Code != http.StatusNoContent {
		t.Fatalf("empty selection status = %d, want 204", response.Code)
	}
}

func TestGalleryPageIncludesSelectionState(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "a.jpg", TakenAt: base, MediaType: "photo", IndexedAt: base}); err != nil {
		t.Fatal(err)
	}
	galleryID, err := db.CreateGallery(ctx, "Test gallery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddPhotosToGallery(ctx, galleryID, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if err := db.SelectPhotos(ctx, []int64{1}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/galleries/"+strconv.FormatInt(galleryID, 10), nil)
	response := httptest.NewRecorder()
	server.galleryPage(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("gallery status = %d", response.Code)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`value="1" checked`)) {
		t.Fatalf("gallery response did not reflect selected photo")
	}
}

func TestSidebarAndSearchPageRendering(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "a.jpg", TakenAt: base, MediaType: "photo", IndexedAt: base}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateGallery(ctx, "Test gallery"); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		path string
		want []string
	}{
		{name: "timeline", path: "/timeline/2026/11", want: []string{"<aside>", ">Timeline<", "/timeline/"}},
		{name: "search", path: "/search", want: []string{"<aside", "Saved searches", "data-search-form", "name=\"from\""}},
		{name: "galleries", path: "/galleries", want: []string{"<aside>", ">Galleries<"}},
	}
	for _, test := range tests {
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		response := httptest.NewRecorder()
		switch test.name {
		case "timeline":
			server.timeline(response, request)
		case "search":
			server.search(response, request)
		case "galleries":
			server.galleryPage(response, request)
		}
		body := response.Body.String()
		t.Logf("%s response status=%d body=%s", test.name, response.Code, body)
		for _, expected := range test.want {
			if !bytes.Contains(response.Body.Bytes(), []byte(expected)) {
				t.Errorf("%s response missing %q", test.name, expected)
			}
		}
		if body == "" {
			t.Errorf("%s returned empty response", test.name)
		}
	}
}

func TestMapPointsForPhotoIDs(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	lat1, lng1 := 51.5, -0.12
	lat2, lng2 := 48.8, 2.35
	for _, record := range []PhotoRecord{
		{Path: "a.jpg", TakenAt: base, Latitude: &lat1, Longitude: &lng1, MediaType: "photo", IndexedAt: base},
		{Path: "b.jpg", TakenAt: base.Add(time.Hour), Latitude: &lat2, Longitude: &lng2, MediaType: "photo", IndexedAt: base},
	} {
		if err := db.UpsertPhoto(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	server := &Server{db: db}
	request := httptest.NewRequest(http.MethodGet, "/api/map-points?photoIds=1&minLat=-90&maxLat=90&minLng=-180&maxLng=180&zoom=10", nil)
	response := httptest.NewRecorder()
	server.mapPoints(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("map points status = %d", response.Code)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte(`"photoId":1`)) {
		t.Fatalf("map points response = %s", response.Body.String())
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`"photoId":2`)) {
		t.Fatalf("map points included photo outside collection: %s", response.Body.String())
	}

	zeroRequest := httptest.NewRequest(http.MethodGet,
		"/api/map-points?photoId=0&minLat=-90&maxLat=90&minLng=-180&maxLng=180&zoom=10", nil)
	zeroResponse := httptest.NewRecorder()
	server.mapPoints(zeroResponse, zeroRequest)
	if zeroResponse.Code != http.StatusOK {
		t.Fatalf("photoId=0 map points status = %d, want 200", zeroResponse.Code)
	}
}

func TestParseTemplates(t *testing.T) {
	templates, err := parseTemplates()
	if err != nil {
		t.Fatalf("parse templates: %v", err)
	}
	for _, name := range []string{"home", "gallery", "locations", "collections", "collection"} {
		if templates[name] == nil {
			t.Errorf("template %q was not created", name)
		}
	}
}

// TestValidMonth tests month string format validation logic.
func TestValidMonth(t *testing.T) {
	for _, test := range []struct {
		value string
		valid bool
	}{
		{"2026-08", true},
		{"2026-8", false},
		{"2026-13", false},
		{"not-a-month", false},
	} {
		if actual := validMonth(test.value); actual != test.valid {
			t.Errorf("validMonth(%q) = %t, want %t", test.value, actual, test.valid)
		}
	}
}

func TestSelectWeeklyPhotosPrefersGoldenHourAndVariedSubjects(t *testing.T) {
	photos := make([]Photo, 12)
	for i := range photos {
		photos[i] = Photo{
			ID:         int64(i + 1),
			Objects:    []string{"subject-" + strconv.Itoa(i)},
			GoldenHour: i < 5,
		}
	}

	selected := selectWeeklyPhotos(photos, 10)
	if len(selected) != 10 {
		t.Fatalf("selected %d photos, want 10", len(selected))
	}
	goldenHourCount := 0
	subjects := make(map[string]bool)
	for _, photo := range selected {
		if photo.GoldenHour {
			goldenHourCount++
		}
		for _, subject := range photo.Objects {
			subjects[subject] = true
		}
	}
	if goldenHourCount == 0 {
		t.Error("selected no golden-hour photos")
	}
	if len(subjects) < 8 {
		t.Errorf("selected only %d distinct subjects, want at least 8", len(subjects))
	}
}

func TestSelectWeeklyPhotosIsDeterministic(t *testing.T) {
	photos := []Photo{
		{ID: 3, Objects: []string{"cat"}},
		{ID: 1, Objects: []string{"cat"}},
		{ID: 2, Objects: []string{"dog"}},
	}

	selected := selectWeeklyPhotos(photos, 3)
	reversed := selectWeeklyPhotos([]Photo{photos[2], photos[1], photos[0]}, 3)
	for i := range selected {
		if selected[i].ID != reversed[i].ID {
			t.Fatalf("selection changed with input order: %v != %v", selected, reversed)
		}
	}
}

func TestCollectionPhotoMapURLPreservesCollection(t *testing.T) {
	lat, lng := 34.146019, -118.159211
	photo := Photo{ID: 19, Latitude: &lat, Longitude: &lng}
	if got := mapURLForCollectionPhoto("/timeline/2026/06", photo); got != "/timeline/2026/06/photo/19/map" {
		t.Fatalf("timeline map URL = %q", got)
	}
	if got := mapURLForCollectionPhoto("/galleries/6", photo); got != "/galleries/6/photo/19/map" {
		t.Fatalf("gallery map URL = %q", got)
	}
	if got := mapURLForCollectionPhoto("/locations/minLat=34.1,minLng=-118.2,maxLat=34.2,maxLng=-118.1", photo); got != "/locations/minLat=34.1,minLng=-118.2,maxLat=34.2,maxLng=-118.1/photo/19/map" {
		t.Fatalf("locations map URL = %q", got)
	}
}

func TestLocationsPhotoSuffix(t *testing.T) {
	path := "/locations/minLat=34.145970,minLng=-118.159326,maxLat=34.146068,maxLng=-118.159096/photo/1"
	collectionPath, photoID, hasPhoto, mapView, ok := splitCollectionPhotoMapPath(path)
	if !ok || collectionPath != "/locations/minLat=34.145970,minLng=-118.159326,maxLat=34.146068,maxLng=-118.159096" ||
		photoID != 1 || !hasPhoto || mapView {
		t.Fatalf("locations photo suffix parsed as (%q, %d, %t, %t, %t)",
			collectionPath, photoID, hasPhoto, mapView, ok)
	}

	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	lat, lng := 34.146019, -118.159211
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := db.UpsertPhoto(ctx, PhotoRecord{
		Path: "photo-18.jpg", TakenAt: base, Latitude: &lat, Longitude: &lng,
		MediaType: "photo", IndexedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, path, nil)
	response := httptest.NewRecorder()
	server.locations(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("locations photo status = %d, want 200", response.Code)
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("photo-18.jpg")) {
		t.Fatalf("locations photo response did not display photo 1")
	}

	mapRequest := httptest.NewRequest(http.MethodGet, path+"/map", nil)
	mapResponse := httptest.NewRecorder()
	server.locations(mapResponse, mapRequest)
	if mapResponse.Code != http.StatusOK {
		t.Fatalf("locations photo map status = %d, want 200", mapResponse.Code)
	}
	if !bytes.Contains(mapResponse.Body.Bytes(), []byte(`data-photo-id="1"`)) {
		t.Fatalf("locations photo map did not limit map to photo 1")
	}
}

func TestLocationsPlaceRedirect(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	lat1, lng1 := 34.145, -118.16
	lat2, lng2 := 34.15, -118.15
	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for i, record := range []PhotoRecord{
		{Path: "pasadena-a.jpg", TakenAt: base, Latitude: &lat1, Longitude: &lng1, Settlement: "Pasadena", MediaType: "photo", IndexedAt: base},
		{Path: "pasadena-b.jpg", TakenAt: base.Add(time.Hour), Latitude: &lat2, Longitude: &lng2, Settlement: "Pasadena", MediaType: "photo", IndexedAt: base},
	} {
		record.IndexedAt = base
		if err := db.UpsertPhoto(ctx, record); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
	}

	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/locations/place=Pasadena", nil)
	response := httptest.NewRecorder()
	server.locations(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("place status = %d, want 303", response.Code)
	}
	location := response.Header().Get("Location")
	if !strings.HasPrefix(location, "/locations/minLat=") ||
		!strings.Contains(location, "minLat=") ||
		!strings.Contains(location, "maxLng=") ||
		strings.Contains(location, "place=") ||
		strings.Contains(location, "mapLat=") ||
		strings.Contains(location, "mapLng=") ||
		strings.Contains(location, "mapZoom=") {
		t.Fatalf("place redirect = %q", location)
	}

	resolvedRequest := httptest.NewRequest(http.MethodGet,
		"/locations/place=Pasadena,minLat=34.145,minLng=-118.16,maxLat=34.15,maxLng=-118.15,mapZoom=22", nil)
	resolvedResponse := httptest.NewRecorder()
	server.locations(resolvedResponse, resolvedRequest)
	if resolvedResponse.Code != http.StatusOK {
		t.Fatalf("resolved place status = %d, want 200", resolvedResponse.Code)
	}
}

func TestImagePathDoesNotAppendRepeatedPhotoSegments(t *testing.T) {
	returnURL := "/search/from=2012-10-01?from=2012-10-01&to="
	if got := imagePath(18, returnURL); got != "/search/from=2012-10-01/photo/18" {
		t.Fatalf("imagePath() = %q", got)
	}
	if got := imagePath(18, "/search/from=2012-10-01/photo/18"); got != "/search/from=2012-10-01/photo/18" {
		t.Fatalf("imagePath() repeated detail = %q", got)
	}
}

func TestSearchTitle(t *testing.T) {
	query := url.Values{
		"from":     {"2026-10-10"},
		"color":    {"red"},
		"camera":   {"DSC-RX100M2"},
		"location": {"region:Ain"},
	}
	title := searchTitle(query)
	if title != "from 2026-10-10, color red, camera DSC-RX100M2, location Ain" {
		t.Fatalf("searchTitle() = %q", title)
	}
}

func TestLocationsCollectionURL(t *testing.T) {
	query, err := parseLocationsCollectionCriteria("place=Pasadena,minLat=34.1,minLng=-118.2,maxLat=34.2,maxLng=-118.1")
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("place") != "Pasadena" || query.Get("minLat") != "34.1" ||
		query.Get("maxLng") != "-118.1" {
		t.Fatalf("locations criteria = %v", query)
	}

	canonical := canonicalLocationsURL(url.Values{
		"place":  {"Pasadena"},
		"minLat": {"34.1"},
		"minLng": {"-118.2"},
		"maxLat": {"34.2"},
		"maxLng": {"-118.1"},
	})
	if canonical != "/locations/place=Pasadena,minLat=34.1,minLng=-118.2,maxLat=34.2,maxLng=-118.1" {
		t.Fatalf("canonical locations URL = %q", canonical)
	}
}

func TestParseSearchCollectionCriteria(t *testing.T) {
	query, err := parseSearchCollectionCriteria("from=20261010,to=20261109,color=red")
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("from") != "2026-10-10" || query.Get("to") != "2026-11-09" || query.Get("color") != "red" {
		t.Fatalf("parsed search criteria = %v", query)
	}

	query, err = parseSearchCollectionCriteria("from%3D2026-10-10%26color%3Dred")
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("from") != "2026-10-10" || query.Get("color") != "red" {
		t.Fatalf("parsed encoded search criteria = %v", query)
	}

	query, err = parseSearchCollectionCriteria("camera=DSC-RX100M2,location=region%3AAin")
	if err != nil {
		t.Fatal(err)
	}
	if query.Get("camera") != "DSC-RX100M2" || query.Get("location") != "region:Ain" {
		t.Fatalf("parsed region criteria = %v", query)
	}
}

func TestRandomCollectionRedirect(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "random.jpg", TakenAt: base, MediaType: "photo", IndexedAt: base}); err != nil {
		t.Fatal(err)
	}

	server := &Server{db: db}
	request := httptest.NewRequest(http.MethodGet, "/random", nil)
	response := httptest.NewRecorder()
	server.randomCollection(response, request)
	if response.Code != http.StatusSeeOther {
		t.Fatalf("random status = %d, want %d", response.Code, http.StatusSeeOther)
	}
	if location := response.Header().Get("Location"); location != "/random/photo/1" {
		t.Fatalf("random location = %q, want /random/photo/1", location)
	}
}

func TestSplitCollectionPhotoMapPath(t *testing.T) {
	tests := []struct {
		path       string
		collection string
		photoID    int64
		hasPhoto   bool
		mapView    bool
		valid      bool
	}{
		{"/timeline/2026/11/map", "/timeline/2026/11", 0, false, true, true},
		{"/timeline/2026/11/photo/143/map", "/timeline/2026/11", 143, true, true, true},
		{"/timeline/2026/06/photo/19/map", "/timeline/2026/06", 19, true, true, true},
		{"/galleries/6/map", "/galleries/6", 0, false, true, true},
		{"/search/color=red/photo/18/map", "/search/color=red", 18, true, true, true},
	}
	for _, test := range tests {
		collection, photoID, hasPhoto, mapView, valid := splitCollectionPhotoMapPath(test.path)
		if collection != test.collection || photoID != test.photoID || hasPhoto != test.hasPhoto ||
			mapView != test.mapView || valid != test.valid {
			t.Errorf("splitCollectionPhotoMapPath(%q) = (%q, %d, %t, %t, %t), want (%q, %d, %t, %t, %t)",
				test.path, collection, photoID, hasPhoto, mapView, valid,
				test.collection, test.photoID, test.hasPhoto, test.mapView, test.valid)
		}
	}
}

func TestSplitCollectionPhotoPath(t *testing.T) {
	tests := []struct {
		path       string
		collection string
		photoID    int64
		hasPhoto   bool
		valid      bool
	}{
		{"/timeline/2026/11", "/timeline/2026/11", 0, false, true},
		{"/timeline/2026/11/photo/143", "/timeline/2026/11", 143, true, true},
		{"/this-week/photo/7", "/this-week", 7, true, true},
		{"/galleries/6/photo/12", "/galleries/6", 12, true, true},
		{"/galleries/6/photo/not-an-id", "", 0, false, false},
	}
	for _, test := range tests {
		collection, photoID, hasPhoto, valid := splitCollectionPhotoPath(test.path)
		if collection != test.collection || photoID != test.photoID || hasPhoto != test.hasPhoto || valid != test.valid {
			t.Errorf("splitCollectionPhotoPath(%q) = (%q, %d, %t, %t), want (%q, %d, %t, %t)",
				test.path, collection, photoID, hasPhoto, valid,
				test.collection, test.photoID, test.hasPhoto, test.valid)
		}
	}
}

func TestPhotoInCollection(t *testing.T) {
	photos := []Photo{{ID: 10}, {ID: 20}, {ID: 30}}
	if !photoInCollection(photos, 20) {
		t.Error("expected photo 20 to be in the collection")
	}
	if photoInCollection(photos, 21) {
		t.Error("did not expect photo 21 to be in the collection")
	}
}

func TestImageGalleryContextPhotosForThisWeek(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	server := &Server{db: db}
	returnURL, collectionName, photos, err := server.imageGalleryContextPhotos(ctx, "/this-week/", "selection")
	if err != nil {
		t.Fatalf("imageGalleryContextPhotos() error = %v", err)
	}
	if returnURL != "/this-week/" || collectionName != "This Week" || len(photos) != 0 {
		t.Errorf("imageGalleryContextPhotos() = (%q, %q, %d photos), want (%q, %q, 0 photos)",
			returnURL, collectionName, len(photos), "/this-week/", "This Week")
	}
}

// TestIndexProgress tests starting, tracking progress of, finishing, and canceling indexing state.
func TestIndexProgress(t *testing.T) {
	server := &Server{}
	ctx, started := server.beginIndexing(context.Background())
	if !started {
		t.Fatal("beginIndexing() did not start")
	}
	server.setIndexTotal(3)
	server.incrementIndexProgress()

	progress := server.currentIndexProgress()
	if !progress.Active || progress.Processed != 1 || progress.Total != 3 || progress.Percentage != 33 {
		t.Errorf("progress = %+v, want active 1/3 (33%%)", progress)
	}

	server.finishIndexing(1, nil)
	if server.currentIndexProgress().Active {
		t.Error("indexing should be finished")
	}
	if server.currentIndexProgress().Message != "1 photos indexed" {
		t.Errorf("indexing message = %q, want %q", server.currentIndexProgress().Message, "1 photos indexed")
	}

	ctx, started = server.beginIndexing(context.Background())
	if !started {
		t.Fatal("beginIndexing() did not restart")
	}
	server.indexCancel()
	if err := ctx.Err(); err != context.Canceled {
		t.Errorf("index context error = %v, want %v", err, context.Canceled)
	}
	server.finishIndexing(0, context.Canceled)
}

// TestPhotoTakenAt tests formatting timestamps into localized string representations.
func TestPhotoTakenAt(t *testing.T) {
	takenAt := time.Date(2026, time.August, 29, 20, 2, 0, 0, time.UTC)
	if actual := photoTakenAt(takenAt); actual != "29 Aug 2026, 20:02" {
		t.Errorf("photoTakenAt() = %q, want %q", actual, "29 Aug 2026, 20:02")
	}
}

// TestAddDownloadFilesIncludesRawOnlyWhenRequested tests archiving selected photo files with or without raw files.
func TestAddDownloadFilesIncludesRawOnlyWhenRequested(t *testing.T) {
	root := t.TempDir()
	for name, contents := range map[string]string{
		"photos/a.jpg":  "image",
		"photos/a.xmp":  "xmp",
		"photos/a.json": "json",
		"photos/a.RW2":  "raw",
	} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, test := range []struct {
		name       string
		includeRaw bool
		want       map[string]string
	}{
		{
			name: "without raw",
			want: map[string]string{"photos/a.jpg": "image", "photos/a.xmp": "xmp", "photos/a.json": "json"},
		},
		{
			name:       "with raw",
			includeRaw: true,
			want:       map[string]string{"photos/a.jpg": "image", "photos/a.xmp": "xmp", "photos/a.json": "json", "photos/a.RW2": "raw"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			archive := zip.NewWriter(&output)
			server := &Server{config: Config{PhotoRoot: root}}
			if err := server.addDownloadFiles(archive, "photos/a.jpg", "photos/a.RW2", test.includeRaw); err != nil {
				t.Fatal(err)
			}
			if err := archive.Close(); err != nil {
				t.Fatal(err)
			}
			reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
			if err != nil {
				t.Fatal(err)
			}
			files := make(map[string]string, len(reader.File))
			for _, file := range reader.File {
				contents, err := file.Open()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(contents)
				closeErr := contents.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				files[file.Name] = string(data)
			}
			if len(files) != len(test.want) {
				t.Fatalf("archive contains %d files, want %d: %v", len(files), len(test.want), files)
			}
			for name, want := range test.want {
				if files[name] != want {
					t.Errorf("archive entry %q = %q, want %q", name, files[name], want)
				}
			}
		})
	}
}

// TestAddFileToArchiveRejectsPathOutsideRoot verifies path traversal protection when adding files to zip archive.
func TestAddFileToArchiveRejectsPathOutsideRoot(t *testing.T) {
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	err := addFileToArchive(archive, t.TempDir(), "../outside.jpg")
	if err == nil {
		t.Fatal("addFileToArchive() accepted a path outside the root")
	}
	_ = archive.Close()
}

func TestDeleteCollectionKeepsSelected(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	server := &Server{db: db}
	id, err := db.CreateCollection(ctx, "Holiday")
	if err != nil {
		t.Fatal(err)
	}
	selectedID, err := db.selectedCollectionID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []int64{id, selectedID} {
		request := httptest.NewRequest(http.MethodDelete, "/galleries/"+strconv.FormatInt(target, 10), nil)
		server.deleteCollection(httptest.NewRecorder(), request)
	}
	collections, err := db.Collections(ctx)
	if err != nil || len(collections) != 1 || collections[0].Name != SelectedCollectionName {
		t.Fatalf("collections = %+v, %v; want only the selected collection", collections, err)
	}
}

func TestSelectWeeklyPhotosPrefersPeopleAnimalsAndQuality(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	base := time.Date(2024, 3, 5, 12, 0, 0, 0, time.UTC)
	photos := []Photo{
		{ID: 1, TakenAt: base, Objects: []string{"chair"}, Sharpness: f(250), Exposure: f(0.5), Contrast: f(0.7), Brisque: f(20)},
		{ID: 2, TakenAt: base.Add(time.Hour), Objects: []string{"person"}, Sharpness: f(250), Exposure: f(0.5), Contrast: f(0.7), Brisque: f(20)},
		{ID: 3, TakenAt: base.Add(2 * time.Hour), Objects: []string{"dog"}, Sharpness: f(250), Exposure: f(0.5), Contrast: f(0.7), Brisque: f(20)},
		{ID: 4, TakenAt: base.Add(3 * time.Hour), Objects: []string{"person"}, Sharpness: f(5), Exposure: f(0.5), Contrast: f(0.7), Brisque: f(20)},
	}
	got := selectWeeklyPhotos(photos, 2)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 3 {
		t.Fatalf("selected %+v, want photos 2 and 3 in time order", got)
	}
	// Poor photos are still used when needed to fill the limit.
	if all := selectWeeklyPhotos(photos, 10); len(all) != 4 {
		t.Fatalf("selected %d photos, want 4", len(all))
	}
}

func TestDownloadGalleryZipIncludesRawFiles(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	for name, contents := range map[string]string{"a.jpg": "image", "a.RW2": "raw", "b.jpg": "image2"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(contents), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	for _, p := range []PhotoRecord{{Path: "a.jpg", RawPath: "a.RW2"}, {Path: "b.jpg"}} {
		p.TakenAt, p.IndexedAt, p.MediaType = now, now, "photo"
		if err := db.UpsertPhoto(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	id, err := db.CreateGallery(ctx, "My trip/2024")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddPhotosToGallery(ctx, id, []int64{1, 2}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, config: Config{PhotoRoot: root}}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	routes := server.Routes()
	defer server.dlnaCancel()

	for raw, want := range map[string]int{"true": 3, "false": 2} {
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/galleries/"+strconv.FormatInt(id, 10)+"/download?raw="+raw, nil))
		if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Disposition"), "fast-fotos-My-trip-2024.zip") {
			t.Fatalf("raw=%s: status %d header %q", raw, response.Code, response.Header().Get("Content-Disposition"))
		}
		archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
		if err != nil || len(archive.File) != want {
			t.Fatalf("raw=%s: %d files, %v; want %d", raw, len(archive.File), err, want)
		}
	}
	response := httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/galleries/999/download", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing gallery status = %d", response.Code)
	}
}
