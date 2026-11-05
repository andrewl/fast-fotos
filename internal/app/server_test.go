package app

import (
	"archive/zip"
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// TestParseTemplates tests parsing all embedded template files.
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
		request := httptest.NewRequest(http.MethodDelete, "/collections/"+strconv.FormatInt(target, 10), nil)
		server.deleteCollection(httptest.NewRecorder(), request)
	}
	collections, err := db.Collections(ctx)
	if err != nil || len(collections) != 1 || collections[0].Name != SelectedCollectionName {
		t.Fatalf("collections = %+v, %v; want only the selected collection", collections, err)
	}
}
