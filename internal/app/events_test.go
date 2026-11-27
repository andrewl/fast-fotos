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
	photos := []eventPhoto{
		located(1, 0, 51.38, -2.36, "Bath", "Somerset"),
		{id: 2, taken: base.Add(time.Hour)}, // Unlocated but bracketed by reliable photos.
		located(3, 2, 51.39, -2.36, "Bath", "Somerset"),
		located(4, 3, 51.40, -2.36, "Bristol", "Somerset"),
		located(5, 4, 52.5, -1.9, "Birmingham", "West Midlands"),
		located(6, 5, 52.5, -1.9, "Birmingham", "West Midlands"),
		located(7, 6, 52.5, -1.9, "Birmingham", "West Midlands"),
		{id: 8, taken: base.Add(7 * time.Hour)}, // Outside both events.
	}
	groups := groupEvents(photos)
	if len(groups) != 2 || len(groups[0].photos) != 4 || len(groups[1].photos) != 3 {
		t.Fatalf("unexpected groups: %+v", groups)
	}
	if groups[0].name != "Bath" || groups[1].name != "Birmingham" {
		t.Fatalf("names: %s, %s", groups[0].name, groups[1].name)
	}
	photos[0].settlement = "Frome"
	groups = groupEvents(photos)
	if groups[0].name != "Somerset" {
		t.Fatalf("region fallback = %q", groups[0].name)
	}
	photos[0].source, photos[2].source, photos[3].source = "derived", "derived", "derived"
	groups = groupEvents(photos)
	if len(groups) != 1 || groups[0].name != "Birmingham" {
		t.Fatalf("inferred locations should not anchor an event: %+v", groups)
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
	for i := 1; i <= 3; i++ {
		add(i)
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
	if err != nil || len(events) != 1 || events[0].Name != "Bath" || events[0].Count != 3 {
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
	if response := post(path+"/name", url.Values{"name": {"Holiday <test>"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("rename: %d", response.Code)
	}
	if body := get(path).Body.String(); !strings.Contains(body, "Holiday &lt;test&gt;") || strings.Contains(body, "Holiday <test>") {
		t.Fatalf("unsafe or missing event name: %s", body)
	}
	if response := post(path+"/hide", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("hide: %d", response.Code)
	}
	add(4)
	if err := db.RebuildEvents(ctx); err != nil {
		t.Fatal(err)
	}
	events, err = db.Events(ctx, true)
	if err != nil || len(events) != 1 || events[0].ID != id || events[0].Name != "Holiday <test>" || events[0].Count != 4 {
		t.Fatalf("edited event after rebuild: %+v, %v", events, err)
	}
	if body := get("/events").Body.String(); strings.Contains(body, "Holiday") {
		t.Fatal("hidden event appears in visible list")
	}
	if body := get("/events?hidden=1").Body.String(); !strings.Contains(body, "Holiday &lt;test&gt;") {
		t.Fatal("hidden event missing from hidden list")
	}
	if response := post(path+"/restore", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("restore: %d", response.Code)
	}
	if response := post(path+"/name", url.Values{"name": {""}}); response.Code != http.StatusSeeOther {
		t.Fatalf("reset name: %d", response.Code)
	}
	if event, err := db.EventByID(ctx, id); err != nil || event.Name != "Bath" || event.Hidden {
		t.Fatalf("restored event: %+v, %v", event, err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := db.RebuildEvents(cancelled); err == nil {
		t.Fatal("cancelled rebuild should fail")
	}
	if event, err := db.EventByID(ctx, id); err != nil || event.Count != 4 {
		t.Fatalf("cancelled rebuild modified events: %+v, %v", event, err)
	}
}
