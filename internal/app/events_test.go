package app

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestGroupEvents(t *testing.T) {
	base := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	located := func(id int64, hours int, lat, lng float64, settlement, region string) eventPhoto {
		return eventPhoto{id: id, taken: base.Add(time.Duration(hours) * time.Hour),
			latitude: sql.NullFloat64{Float64: lat, Valid: true}, longitude: sql.NullFloat64{Float64: lng, Valid: true},
			settlement: settlement, region: region, country: "UK"}
	}
	var photos []eventPhoto
	// The most common settlement is treated as home and never forms an event.
	for i := 0; i < 10; i++ {
		photos = append(photos, located(int64(100+i), -400+i, 51.5, -0.5, "Staines", "Surrey"))
	}
	photos = append(photos, []eventPhoto{
		located(1, 0, 51.38, -2.36, "Bath", "Somerset"),
		{id: 2, taken: base.Add(time.Hour)}, // Unlocated but bracketed by reliable photos.
		located(3, 2, 51.39, -2.36, "Bath", "Somerset"),
		located(4, 3, 51.40, -2.36, "Bristol", "Somerset"),
		located(20, 3, 51.40, -2.36, "Bath", "Somerset"),
		located(5, 4, 52.5, -1.9, "Birmingham", "West Midlands"),
		located(6, 5, 52.5, -1.9, "Birmingham", "West Midlands"),
		located(7, 6, 52.5, -1.9, "Birmingham", "West Midlands"),
		located(21, 6, 52.5, -1.9, "Birmingham", "West Midlands"),
		located(22, 6, 52.5, -1.9, "Birmingham", "West Midlands"),
		{id: 8, taken: base.Add(7 * time.Hour)}, // Outside both events.
	}...)
	groups := groupEvents(photos)
	if len(groups) != 2 || len(groups[0].photos) != 5 || len(groups[1].photos) != 5 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
	if groups[0].name != "Bath" || groups[1].name != "Birmingham" {
		t.Fatalf("names: %s, %s", groups[0].name, groups[1].name)
	}
	photos[10].settlement = "Frome"
	groups = groupEvents(photos)
	if groups[0].name != "Somerset" {
		t.Fatalf("region fallback = %q", groups[0].name)
	}
	photos[10].source, photos[12].source, photos[13].source, photos[14].source = "derived", "derived", "derived", "derived"
	groups = groupEvents(photos)
	if len(groups) != 1 || groups[0].name != "Birmingham" {
		t.Fatalf("inferred locations should not anchor an event: %+v", groups)
	}
}

func TestGroupEventsDistanceAndHome(t *testing.T) {
	base := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	at := func(id int64, minutes int, lat, lng float64, settlement string) eventPhoto {
		return eventPhoto{id: id, taken: base.Add(time.Duration(minutes) * time.Minute),
			latitude: sql.NullFloat64{Float64: lat, Valid: true}, longitude: sql.NullFloat64{Float64: lng, Valid: true},
			settlement: settlement}
	}
	var photos []eventPhoto
	for i := 0; i < 5; i++ {
		photos = append(photos, at(int64(100+i), -10000-i, 51.43, -0.52, "Staines"))
	}
	// Holborn photos followed by home photos ~25 km away within the next two days.
	for i := 0; i < 5; i++ {
		photos = append(photos, at(int64(i+1), i, 51.514, -0.118, "Holborn"))
	}
	photos = append(photos, at(10, 2000, 51.43, -0.52, "Staines"), at(11, 2001, 51.43, -0.52, "Staines"))
	groups := groupEvents(photos)
	if len(groups) != 1 || len(groups[0].photos) != 5 || groups[0].name != "Holborn" {
		t.Fatalf("expected a single Holborn event without home photos: %+v", groups)
	}
}

func TestGroupEventsSameRegionIslands(t *testing.T) {
	base := time.Date(2025, 7, 21, 10, 0, 0, 0, time.UTC)
	at := func(id int64, day int, lat, lng float64, region string) eventPhoto {
		return eventPhoto{id: id, taken: base.Add(time.Duration(day) * 24 * time.Hour),
			latitude: sql.NullFloat64{Float64: lat, Valid: true}, longitude: sql.NullFloat64{Float64: lng, Valid: true},
			settlement: "", region: region}
	}
	photos := []eventPhoto{
		at(1, 0, 58.75, -2.93, "Orkney Islands"), at(2, 1, 59.02, -3.25, "Orkney Islands"),
		at(3, 2, 59.18, -2.77, "Orkney Islands"), at(4, 3, 58.98, -2.96, "Orkney Islands"),
		at(5, 4, 58.0, -3.87, "Highland"), // lone outlier
		at(6, 5, 59.0, -3.19, "Orkney Islands"),
	}
	groups := groupEvents(photos)
	if len(groups) != 1 || len(groups[0].photos) != 5 || groups[0].name != "Orkney Islands" {
		t.Fatalf("expected one Orkney event: %+v", groups)
	}
}

func TestGroupEventsTimeBoundaries(t *testing.T) {
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	var photos []eventPhoto
	for i := 0; i < 17; i++ {
		photos = append(photos, eventPhoto{id: int64(i + 1), taken: start.Add(time.Duration(i) * 24 * time.Hour),
			latitude: sql.NullFloat64{Float64: 51, Valid: true}, longitude: sql.NullFloat64{Float64: -1, Valid: true}})
	}
	groups := groupEvents(photos)
	if len(groups) != 1 || len(groups[0].photos) != 15 {
		t.Fatalf("event duration limit: %+v", groups)
	}
	// A gap longer than 48 hours splits otherwise nearby clusters.
	for i := 15; i < len(photos); i++ {
		photos[i].taken = photos[i].taken.Add(48 * time.Hour)
	}
	photos = append(photos, eventPhoto{id: 18, taken: start.Add(66 * 24 * time.Hour),
		latitude: sql.NullFloat64{Float64: 51, Valid: true}, longitude: sql.NullFloat64{Float64: -1, Valid: true}})
	if groups := groupEvents(photos); len(groups) != 1 || len(groups[0].photos) != 15 {
		t.Fatalf("gap limit: %+v", groups)
	}
}

func TestEventRebuildAndRoutes(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	base := time.Date(2025, 5, 1, 10, 0, 0, 0, time.UTC)
	lat, lng := 51.38, -2.36
	add := func(i int) {
		t.Helper()
		if err := db.UpsertPhoto(ctx, PhotoRecord{Path: fmt.Sprintf("%d.jpg", i), TakenAt: base.Add(time.Duration(i) * time.Hour),
			Latitude: &lat, Longitude: &lng, Settlement: "Bath", Region: "Somerset", Country: "UK", IndexedAt: base, MediaType: "photo"}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 5; i++ {
		add(i)
	}
	homeLat, homeLng := 51.43, -0.52
	for i := 0; i < 10; i++ {
		if err := db.UpsertPhoto(ctx, PhotoRecord{Path: fmt.Sprintf("home%d.jpg", i), TakenAt: base.Add(time.Duration(-500+i) * time.Hour),
			Latitude: &homeLat, Longitude: &homeLng, Settlement: "Staines", Region: "Surrey", Country: "UK", IndexedAt: base, MediaType: "photo"}); err != nil {
			t.Fatal(err)
		}
	}
	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	// The background phase also populates events for an existing library without an ONNX detector.
	server.startDetection()
	server.detectMu.Lock()
	done := server.detectDone
	server.detectMu.Unlock()
	if done != nil {
		<-done
	}
	events, err := db.Events(ctx, false)
	if err != nil || len(events) != 1 || events[0].Name != "Bath" || events[0].Count != 5 {
		t.Fatalf("initial events: %+v, %v", events, err)
	}
	id := events[0].ID
	routes := server.Routes()
	defer server.dlnaCancel()
	get := func(path string) *httptest.ResponseRecorder {
		t.Helper()
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		return response
	}
	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		return response
	}
	path := fmt.Sprintf("/events/%d", id)
	for _, target := range []string{"/events", path, path + "/photo/1", path + "/map"} {
		if response := get(target); response.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", target, response.Code, response.Body.String())
		}
	}
	if response := get(path + "/photo/999"); response.Code != http.StatusNotFound {
		t.Fatalf("non-member photo: %d", response.Code)
	}
	if response := post(path+"/name", nil); response.Code != http.StatusMethodNotAllowed && response.Code != http.StatusNotFound {
		t.Fatalf("event names must not be editable: %d", response.Code)
	}
	copied := post(path+"/gallery", nil)
	if copied.Code != http.StatusSeeOther || !strings.HasPrefix(copied.Header().Get("Location"), "/galleries/") {
		t.Fatalf("copy to gallery: %d %s", copied.Code, copied.Header().Get("Location"))
	}
	if body := get(copied.Header().Get("Location")).Body.String(); !strings.Contains(body, "Bath") {
		t.Fatalf("copied gallery missing: %s", body)
	}
	if response := post("/events/999/gallery", nil); response.Code != http.StatusNotFound {
		t.Fatalf("missing event copy: %d", response.Code)
	}
	if response := post(path+"/hide", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("hide: %d", response.Code)
	}
	add(6)
	if err := db.RebuildEvents(ctx); err != nil {
		t.Fatal(err)
	}
	events, err = db.Events(ctx, true)
	if err != nil || len(events) != 1 || events[0].ID != id || events[0].Name != "Bath" || events[0].Count != 6 {
		t.Fatalf("edited event after rebuild: %+v, %v", events, err)
	}
	if body := get("/events").Body.String(); strings.Contains(body, "Bath") {
		t.Fatal("hidden event appears in visible list")
	}
	if body := get("/events?hidden=1").Body.String(); !strings.Contains(body, "Bath") {
		t.Fatal("hidden event missing from hidden list")
	}
	if response := post(path+"/restore", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("restore: %d", response.Code)
	}
	if event, err := db.EventByID(ctx, id); err != nil || event.Name != "Bath" || event.Hidden {
		t.Fatalf("restored event: %+v, %v", event, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.RebuildEvents(cancelled); err == nil {
		t.Fatal("cancelled rebuild should fail")
	}
	if event, err := db.EventByID(ctx, id); err != nil || event.Count != 6 {
		t.Fatalf("cancelled rebuild modified events: %+v, %v", event, err)
	}
}
