package app

import (
	"bytes"
	"context"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestScaleDLNAImageTo4K(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wide.jpg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	imageData := image.NewRGBA(image.Rect(0, 0, 7680, 4320))
	if err := jpeg.Encode(file, imageData, nil); err != nil {
		file.Close()
		t.Fatal(err)
	}
	file.Close()

	resized, mime, changed, err := scaleDLNAImage(path)
	if err != nil {
		t.Fatal(err)
	}
	if !changed || mime != "image/jpeg" {
		t.Fatalf("resize changed/mime = %t/%q", changed, mime)
	}
	config, _, err := image.DecodeConfig(bytes.NewReader(resized))
	if err != nil {
		t.Fatal(err)
	}
	if config.Width != 3840 || config.Height != 2160 {
		t.Fatalf("resized dimensions = %dx%d, want 3840x2160", config.Width, config.Height)
	}
}

func TestDLNABrowseFoldersAndResources(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	db, err := OpenDB(ctx, filepath.Join(root, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	base := time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC)
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), []byte("jpeg"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertPhoto(ctx, PhotoRecord{
		Path: "photo.jpg", TakenAt: base, MediaType: "photo",
		Thumbnail: []byte("thumbnail"), IndexedAt: base,
	}); err != nil {
		t.Fatal(err)
	}
	galleryID, err := db.CreateGallery(ctx, "Trips")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddPhotosToGallery(ctx, galleryID, []int64{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.SaveSearch(ctx, "/search/color=red", "Red photos"); err != nil {
		t.Fatal(err)
	}

	server := &Server{db: db, config: Config{PhotoRoot: root, ListenAddress: ":8090"}}

	description := httptest.NewRecorder()
	server.dlnaDescription(description, httptest.NewRequest(http.MethodGet, "/dlna/description.xml", nil))
	if description.Code != http.StatusOK || !strings.Contains(description.Body.String(), "Fast Fotos") {
		t.Fatalf("DLNA description response = %d %q", description.Code, description.Body.String())
	}

	didl, total, err := server.dlnaBrowse(ctx, "0", "BrowseDirectChildren", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, title := range []string{"Timeline", "This Week", "Saved Searches", "Galleries"} {
		if !strings.Contains(didl, title) {
			t.Errorf("root DIDL missing %q: %s", title, didl)
		}
	}
	if total != 4 {
		t.Fatalf("root total = %d, want 4", total)
	}

	didl, total, err = server.dlnaBrowse(ctx, "timeline/2026-06", "BrowseDirectChildren", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || !strings.Contains(didl, "photo.jpg") {
		t.Fatalf("timeline DIDL total=%d: %s", total, didl)
	}

	didl, _, err = server.dlnaBrowse(ctx, "gallery/"+itoa(galleryID), "BrowseDirectChildren", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(didl, "photo.jpg") {
		t.Fatalf("gallery DIDL missing photo: %s", didl)
	}

	didl, _, err = server.dlnaBrowse(ctx, "search/1", "BrowseDirectChildren", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if didl == "" {
		t.Fatal("saved search DIDL is empty")
	}

	thumbnail := httptest.NewRecorder()
	server.dlnaThumbnail(thumbnail, httptest.NewRequest(http.MethodGet, "/dlna/thumbnail/1", nil))
	if thumbnail.Code != http.StatusOK || thumbnail.Body.String() != "thumbnail" {
		t.Fatalf("thumbnail status/body = %d/%q", thumbnail.Code, thumbnail.Body.String())
	}

	resource := httptest.NewRecorder()
	server.dlnaResource(resource, httptest.NewRequest(http.MethodGet, "/dlna/resource/1", nil))
	if resource.Code != http.StatusOK || resource.Body.String() != "jpeg" {
		t.Fatalf("resource status/body = %d/%q", resource.Code, resource.Body.String())
	}
}

func itoa(value int64) string {
	return strconv.FormatInt(value, 10)
}

func TestDLNAFriendlyName(t *testing.T) {
	if got := dlnaFriendlyName(""); got != "Fast Fotos" {
		t.Fatalf("default name = %q", got)
	}
	if got := dlnaFriendlyName("A&B"); got != "Fast Fotos - A&amp;B" {
		t.Fatalf("escaped name = %q", got)
	}
}
