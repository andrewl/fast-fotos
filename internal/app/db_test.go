package app

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

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
			FocalLength: &focal, FlashFired: &flash, Objects: []string{"cat"}, DominantColors: []string{"red"}, MediaType: "photo", DayOfYear: 65, Thumbnail: []byte{1}},
		{Path: "b.jpg", TakenAt: base.Add(30 * time.Minute), Objects: []string{"dog"}, MediaType: "photo", DayOfYear: 65},
	} {
		rec.LocationSource = map[bool]string{true: "extracted"}[rec.Location != ""]
		rec.IndexedAt = base
		if err := db.UpsertPhoto(ctx, rec); err != nil {
			t.Fatalf("upsert %d: %v", i, err)
		}
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
	if labels, err := db.Labels(ctx); err != nil || len(labels) != 2 {
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
