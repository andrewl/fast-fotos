package app

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/andrewl/fast-fotos/internal/quality"
	"log/slog"
	"math"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSQLDebugLogging(t *testing.T) {
	var output bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelDebug, AddSource: true})))
	defer slog.SetDefault(previous)

	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if !strings.Contains(output.String(), `"sql":"CREATE TABLE IF NOT EXISTS photos`) ||
		!strings.Contains(output.String(), `"sql":"CREATE TABLE IF NOT EXISTS collections`) {
		t.Fatal("schema statements were not logged separately")
	}
	output.Reset()

	const create = `CREATE TEMP TABLE log_test (value TEXT)`
	const single = `SELECT ?`
	const list = `SELECT value FROM log_test`
	const insert = `INSERT INTO log_test (value) VALUES (?)`
	const count = `SELECT COUNT(*) FROM log_test`
	const invalid = `SELECT missing FROM log_test`
	_, _, createLine, _ := runtime.Caller(0)
	if _, err := db.sql.ExecContext(ctx, create); err != nil {
		t.Fatal(err)
	}
	var value string
	if err := db.sql.QueryRowContext(ctx, single, "sensitive-bound-value").Scan(&value); err != nil {
		t.Fatal(err)
	}
	rows, err := db.sql.QueryContext(ctx, list)
	if err != nil {
		t.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	tx, err := db.sql.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, insert, "first"); err != nil {
		t.Fatal(err)
	}
	var total int
	if err := tx.QueryRowContext(ctx, count).Scan(&total); err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.PrepareContext(ctx, insert)
	if err != nil {
		t.Fatal(err)
	}
	for _, arg := range []string{"second", "third"} {
		if _, err := stmt.ExecContext(ctx, arg); err != nil {
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, invalid); err == nil {
		t.Fatal("invalid statement unexpectedly succeeded")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	want := []string{create, single, list, insert, count, insert, insert, invalid}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != len(want) {
		t.Fatalf("got %d log entries, want %d: %s", len(lines), len(want), output.String())
	}
	for i, line := range lines {
		var entry struct {
			Level  string      `json:"level"`
			SQL    string      `json:"sql"`
			Source slog.Source `json:"source"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		if entry.Level != "DEBUG" || entry.SQL != want[i] {
			t.Errorf("log entry %d = %+v, want DEBUG and %q", i, entry, want[i])
		}
		if !strings.HasSuffix(entry.Source.Function, ".TestSQLDebugLogging") || filepath.Base(entry.Source.File) != "db_test.go" || entry.Source.Line == 0 {
			t.Errorf("log entry %d has incorrect caller: %+v", i, entry.Source)
		}
		if i == 0 && entry.Source.Line != createLine+1 {
			t.Errorf("first log source line = %d, want %d", entry.Source.Line, createLine+1)
		}
	}
	if strings.Contains(output.String(), "sensitive-bound-value") {
		t.Fatal("bound parameter value appeared in SQL logs")
	}

	output.Reset()
	if _, err := db.CollectionExists(ctx, 0); err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Source slog.Source `json:"source"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &entry); err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(entry.Source.Function, ".(*DB).CollectionExists") || filepath.Base(entry.Source.File) != "db.go" || entry.Source.Line == 0 {
		t.Fatalf("database method source = %+v", entry.Source)
	}
	output.Reset()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if err := db.sql.QueryRowContext(ctx, single, "ignored").Scan(&value); err != nil {
		t.Fatal(err)
	}
	if output.Len() != 0 {
		t.Fatalf("SQL was logged above debug level: %s", output.String())
	}
}

func TestSelectedCollectionMigration(t *testing.T) {
	for _, occupied := range []bool{false, true} {
		t.Run(strconv.FormatBool(occupied), func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "test.db")
			db, err := OpenDB(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			id, err := db.selectedCollectionID(ctx)
			if err != nil || id != 0 {
				t.Fatalf("new selected collection = %d, %v; want 0", id, err)
			}
			now := time.Now().UTC()
			for _, name := range []string{"selected.jpg", "other.jpg"} {
				if err := db.UpsertPhoto(ctx, PhotoRecord{Path: name, TakenAt: now, IndexedAt: now, MediaType: "photo"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := db.SelectPhotos(ctx, []int64{1}); err != nil {
				t.Fatal(err)
			}
			regularID, err := db.CreateGallery(ctx, "regular")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.AddPhotosToGallery(ctx, regularID, []int64{2}); err != nil {
				t.Fatal(err)
			}
			tx, err := db.sql.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, statement := range []string{
				`PRAGMA defer_foreign_keys = ON`,
				`UPDATE collections SET id = 9 WHERE id = 0`,
				`UPDATE collection_photos SET collection_id = 9 WHERE collection_id = 0`,
			} {
				if _, err := tx.ExecContext(ctx, statement); err != nil {
					t.Fatal(err)
				}
			}
			if occupied {
				if _, err := tx.ExecContext(ctx, `INSERT INTO collections (id, name) VALUES (0, 'occupied')`); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO collection_photos (collection_id, photo_id) VALUES (0, 2)`); err != nil {
					t.Fatal(err)
				}
			}
			if err := tx.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			db, err = OpenDB(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			id, err = db.selectedCollectionID(ctx)
			if err != nil || id != 0 {
				t.Fatalf("migrated selected collection = %d, %v; want 0", id, err)
			}
			photos, err := db.PhotosForGallery(ctx, id)
			if err != nil || len(photos) != 1 || photos[0].ID != 1 {
				t.Fatalf("selected photos = %+v, %v", photos, err)
			}
			photos, err = db.PhotosForGallery(ctx, regularID)
			if err != nil || len(photos) != 1 || photos[0].ID != 2 {
				t.Fatalf("regular gallery photos = %+v, %v", photos, err)
			}
			if occupied {
				var occupiedID int64
				if err := db.sql.QueryRowContext(ctx, `SELECT id FROM collections WHERE name = 'occupied'`).Scan(&occupiedID); err != nil {
					t.Fatal(err)
				}
				photos, err = db.PhotosForGallery(ctx, occupiedID)
				if occupiedID == 0 || err != nil || len(photos) != 1 || photos[0].ID != 2 {
					t.Fatalf("relocated gallery %d photos = %+v, %v", occupiedID, photos, err)
				}
			}
			if err := db.migrateSelectedCollection(ctx); err != nil {
				t.Fatalf("repeat migration: %v", err)
			}
		})
	}
}

func TestPlaceBounds(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	lat1, lng1 := 34.145, -118.16
	lat2, lng2 := 34.15, -118.15
	for _, record := range []PhotoRecord{
		{Path: "pasadena-a.jpg", TakenAt: base, Latitude: &lat1, Longitude: &lng1, Settlement: "Pasadena", MediaType: "photo", IndexedAt: base},
		{Path: "pasadena-b.jpg", TakenAt: base.Add(time.Hour), Latitude: &lat2, Longitude: &lng2, Settlement: "Pasadena", MediaType: "photo", IndexedAt: base},
	} {
		if err := db.UpsertPhoto(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	kind, bounds, err := db.PlaceBounds(ctx, "Pasadena")
	if err != nil {
		t.Fatal(err)
	}
	if kind != "settlement" {
		t.Fatalf("place kind = %q, want settlement", kind)
	}
	if bounds.MinLatitude != lat1 || bounds.MaxLatitude != lat2 ||
		bounds.MinLongitude != lng1 || bounds.MaxLongitude != lng2 {
		t.Fatalf("place bounds = %+v", bounds)
	}
}

func TestSavedSearchCRUD(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	id, err := db.SaveSearch(ctx, "/search/color=red", "color red")
	if err != nil {
		t.Fatal(err)
	}
	searches, err := db.SavedSearches(ctx)
	if err != nil || len(searches) != 1 {
		t.Fatalf("saved searches = %+v, %v", searches, err)
	}
	if searches[0].ID != id || searches[0].URL != "/search/color=red" || searches[0].Title != "color red" {
		t.Fatalf("saved search = %+v", searches[0])
	}
	if err := db.DeleteSavedSearch(ctx, id); err != nil {
		t.Fatal(err)
	}
	searches, err = db.SavedSearches(ctx)
	if err != nil || len(searches) != 0 {
		t.Fatalf("saved searches after delete = %+v, %v", searches, err)
	}
}

func TestRandomCollectionPhotos(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		record := PhotoRecord{
			Path:      "photo-" + string(rune('0'+i)) + ".jpg",
			TakenAt:   base.Add(time.Duration(i) * time.Hour),
			MediaType: "photo",
			IndexedAt: base,
		}
		if err := db.UpsertPhoto(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	photos, err := db.RandomCollectionPhotos(ctx, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 3 {
		t.Fatalf("random collection has %d photos, want 3", len(photos))
	}
	if photos[1].ID != 3 {
		t.Fatalf("middle random photo = %d, want target 3", photos[1].ID)
	}
	seen := map[int64]bool{}
	for _, photo := range photos {
		if seen[photo.ID] {
			t.Fatalf("random collection contains duplicate photo %d", photo.ID)
		}
		seen[photo.ID] = true
	}

	randomID, err := db.RandomPhotoID(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if randomID < 1 || randomID > 5 {
		t.Fatalf("random photo id = %d", randomID)
	}
}

func TestSimilarPhotosWeightsTagsTenTimesColors(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	records := []PhotoRecord{
		{Path: "target.jpg", TakenAt: base, Objects: []string{"cat", "forest"}, DominantColors: []string{"green"}, MediaType: "photo"},
		{Path: "tag-match.jpg", TakenAt: base.Add(time.Hour), Objects: []string{"cat"}, DominantColors: []string{"blue"}, MediaType: "photo"},
		{Path: "color-match.jpg", TakenAt: base.Add(2 * time.Hour), Objects: []string{"dog"}, DominantColors: []string{"green"}, MediaType: "photo"},
		{Path: "no-match.jpg", TakenAt: base.Add(3 * time.Hour), Objects: []string{"car"}, DominantColors: []string{"red"}, MediaType: "photo"},
	}
	for i, record := range records {
		record.IndexedAt = base
		if err := db.UpsertPhoto(ctx, record); err != nil {
			t.Fatal(err)
		}
		if err := db.SetDetectedObjects(ctx, int64(i+1), record.Objects); err != nil {
			t.Fatal(err)
		}
	}

	photos, err := db.SimilarPhotos(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 3 {
		t.Fatalf("similar photos = %d, want 3", len(photos))
	}
	if photos[0].Path != "tag-match.jpg" {
		t.Fatalf("top match = %q, want tag-match.jpg", photos[0].Path)
	}
	if photos[1].Path != "color-match.jpg" {
		t.Fatalf("second match = %q, want color-match.jpg", photos[1].Path)
	}
}

func TestDBRoundTrip(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	lat, lng, focal, flash := 51.5, -0.12, 50.0, true
	base := time.Date(2024, 3, 5, 10, 0, 0, 0, time.UTC)
	for i, rec := range []PhotoRecord{
		{Path: "a.jpg", TakenAt: base, Latitude: &lat, Longitude: &lng, Location: "London", Settlement: "London", Region: "England",
			FocalLength: &focal, FlashFired: &flash, DominantColors: []string{"red"}, MediaType: "photo", DayOfYear: 65, Thumbnail: []byte{1}},
		{Path: "b.jpg", TakenAt: base.Add(30 * time.Minute), MediaType: "photo", DayOfYear: 65},
	} {
		rec.LocationSource = map[bool]string{true: "extracted"}[rec.Location != ""]
		rec.IndexedAt = base
		if err := db.UpsertPhoto(ctx, rec); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE photos SET golden_hour = 1 WHERE path = 'a.jpg'`); err != nil {
		t.Fatal(err)
	}
	if photo, err := db.PhotoByID(ctx, 1); err != nil || !photo.GoldenHour {
		t.Fatalf("golden-hour photo: %+v %v", photo, err)
	}
	pending, err := db.NextPendingDetections(ctx, 0, 10)
	if err != nil || len(pending) != 2 {
		t.Fatalf("pending: %+v %v", pending, err)
	}
	if count, err := db.PendingDetectionCount(ctx); err != nil || count != 2 {
		t.Fatalf("pending detection count: %d %v", count, err)
	}
	if err := db.SetDetectedObjects(ctx, pending[0].ID, []string{"cat"}); err != nil {
		t.Fatal(err)
	}
	if pending, err = db.NextPendingDetections(ctx, 0, 10); err != nil || len(pending) != 1 {
		t.Fatalf("pending after detect: %+v %v", pending, err)
	}
	if err := db.DeriveNearbyLocations(ctx); err != nil {
		t.Fatal(err)
	}
	photo, err := db.PhotoByID(ctx, 2)
	if err != nil || photo.Location != "London" || photo.Latitude == nil {
		t.Fatalf("derived location: %+v %v", photo, err)
	}
	if months, err := db.Months(ctx); err != nil || len(months) != 1 || months[0].Key != "2024-03" || months[0].Count != 2 {
		t.Fatalf("months: %+v %v", months, err)
	}
	if got, err := db.PhotosForMonth(ctx, "2024-03"); err != nil || len(got) != 2 || !got[0].TakenAt.Equal(base) {
		t.Fatalf("month photos: %+v %v", got, err)
	}
	if got, err := db.SearchPhotos(ctx, SearchData{Label: "cat", DateFrom: "2024-03-05", DateTo: "2024-03-05", FocalLength: "50", Flash: "yes", Color: "red"}); err != nil || len(got) != 1 {
		t.Fatalf("search: %+v %v", got, err)
	}
	if err := db.SetQualityMetrics(ctx, 1, &quality.Metrics{Sharpness: 200, Exposure: 0.5, Contrast: 0.8, Brisque: 20}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetQualityMetrics(ctx, 2, &quality.Metrics{Sharpness: 10, Exposure: 0.1, Contrast: 0.2, Brisque: 60}); err != nil {
		t.Fatal(err)
	}
	if got, err := db.SearchPhotos(ctx, SearchData{Sharpness: "sharp", Exposure: "good", Contrast: "high", Quality: "good"}); err != nil || len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("quality search: %+v %v", got, err)
	}
	if got, err := db.SearchPhotos(ctx, SearchData{Sharpness: "blurry", Exposure: "under"}); err != nil || len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("quality search 2: %+v %v", got, err)
	}
	if labels, err := db.Labels(ctx); err != nil || len(labels) != 1 {
		t.Fatalf("labels: %v %v", labels, err)
	}
	if fl, err := db.FocalLengths(ctx); err != nil || len(fl) != 1 || fl[0] != "50" {
		t.Fatalf("focal: %v %v", fl, err)
	}
	if locs, err := db.SearchLocations(ctx); err != nil || len(locs) == 0 {
		t.Fatalf("locations: %v %v", locs, err)
	}
	if pts, err := db.MapPoints(ctx, -90, 90, -180, 180, 10, "", ""); err != nil || len(pts) != 1 || pts[0].Count != 2 {
		t.Fatalf("map points: %+v %v", pts, err)
	}
	if pts, err := db.ClusterPoints(ctx, -90, 90, -180, 180); err != nil || len(pts) != 2 || pts[0].TakenAt != "2024-03-05T10:00:00Z" {
		t.Fatalf("cluster: %+v %v", pts, err)
	}
	if _, err := db.LocationBounds(ctx, "London", ""); err != nil {
		t.Fatal(err)
	}
	if thumb, _, err := db.PhotoThumbnail(ctx, "1"); err != nil || len(thumb) != 1 {
		t.Fatalf("thumb: %v %v", thumb, err)
	}

	id, err := db.CreateCollection(ctx, "trip")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := db.AddPhotosToCollection(ctx, id, []int64{1, 2, 99}); err != nil || n != 2 {
		t.Fatalf("add: %d %v", n, err)
	}
	if n, _ := db.AddPhotosToCollection(ctx, id, []int64{1}); n != 0 {
		t.Fatalf("duplicate add should be ignored, got %d", n)
	}
	if err := db.RemovePhotosFromCollection(ctx, id, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if c, err := db.CollectionByID(ctx, id); err != nil || c.Count != 1 {
		t.Fatalf("collection: %+v %v", c, err)
	}
	if err := db.DeleteCollection(ctx, id); err != nil {
		t.Fatal(err)
	}

	if err := db.RecordIndexedAt(ctx, base); err != nil {
		t.Fatal(err)
	}
	if got, err := db.LastIndexedAt(ctx); err != nil || got == nil || !got.Equal(base) {
		t.Fatalf("last indexed: %v %v", got, err)
	}
	if err := db.DeleteStalePhotos(ctx, base.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if months, _ := db.Months(ctx); len(months) != 0 {
		t.Fatalf("stale photos not removed: %+v", months)
	}
}

func TestDeleteMissingPhotos(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	for _, p := range []string{"keep.jpg", "gone.jpg"} {
		if err := db.UpsertPhoto(ctx, PhotoRecord{Path: p, TakenAt: now, MediaType: "photo", IndexedAt: now}); err != nil {
			t.Fatal(err)
		}
	}
	removed, err := db.DeleteMissingPhotos(ctx, []string{"keep.jpg"})
	if err != nil || removed != 1 {
		t.Fatalf("removed %d, err %v", removed, err)
	}
}

func TestSelectedCollection(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "a.jpg", TakenAt: now, MediaType: "photo", IndexedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := db.SelectPhotos(ctx, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := db.SelectedPhotoIDs(ctx); !ids[1] {
		t.Fatalf("not selected: %v", ids)
	}
	id, _ := db.selectedCollectionID(ctx)
	if err := db.DeleteCollection(ctx, id); err != ErrProtectedCollection {
		t.Fatalf("want protected, got %v", err)
	}
	if err := db.ClearSelection(ctx); err != nil {
		t.Fatal(err)
	}
	if ids, _ := db.SelectedPhotoIDs(ctx); len(ids) != 0 {
		t.Fatalf("not cleared: %v", ids)
	}
}
func TestNearPhotosUsesMercatorDistance(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := OpenDB(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}

	base := time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)
	targetLat, targetLng := 51.5, -0.12
	nearLat, nearLng := 51.6, -0.12
	farLat, farLng := 52.0, -0.12
	for i, record := range []PhotoRecord{
		{Path: "target.jpg", TakenAt: base, Latitude: &targetLat, Longitude: &targetLng, MediaType: "photo"},
		{Path: "near.jpg", TakenAt: base.Add(time.Hour), Latitude: &nearLat, Longitude: &nearLng, MediaType: "photo"},
		{Path: "far.jpg", TakenAt: base.Add(2 * time.Hour), Latitude: &farLat, Longitude: &farLng, MediaType: "photo"},
	} {
		record.IndexedAt = base
		if err := db.UpsertPhoto(ctx, record); err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			continue
		}
	}

	photos, err := db.NearPhotos(ctx, 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(photos) != 1 || photos[0].Path != "near.jpg" {
		t.Fatalf("near photos = %+v, want only near.jpg", photos)
	}

	var x, y float64
	if err := db.sql.QueryRowContext(ctx, `SELECT x, y FROM photos WHERE id = 1`).Scan(&x, &y); err != nil {
		t.Fatal(err)
	}
	wantX, wantY := EPSG900913Project(targetLat, targetLng)
	if math.Abs(x-wantX) > 0.001 || math.Abs(y-wantY) > 0.001 {
		t.Fatalf("mercator coordinates = (%f, %f), want (%f, %f)", x, y, wantX, wantY)
	}
	if _, err := db.sql.ExecContext(ctx, `UPDATE photos SET x = NULL, y = NULL`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	// Existing records are backfilled on the next open.
	db, err = OpenDB(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	photos, err = db.NearPhotos(ctx, 1, 100)
	if err != nil || len(photos) != 1 {
		t.Fatalf("near photos after reopen = %d, %v", len(photos), err)
	}
}
