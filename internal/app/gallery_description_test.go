package app

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
)

func TestGalleryDescriptionMigration(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE collections (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT UNIQUE NOT NULL,
		created_at TEXT NOT NULL DEFAULT '2024-01-01');
		INSERT INTO collections (id, name) VALUES (1, 'Existing')`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if gallery, err := db.CollectionByID(ctx, 1); err != nil || gallery.Description != "" {
		t.Fatalf("migrated gallery: %+v, %v", gallery, err)
	}
	if err := db.UpdateGalleryDescription(ctx, 1, "First\nSecond"); err != nil {
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
	if gallery, err := db.CollectionByID(ctx, 1); err != nil || gallery.Description != "First\nSecond" {
		t.Fatalf("reopened gallery: %+v, %v", gallery, err)
	}
}

func TestGalleryDescriptionRoutes(t *testing.T) {
	ctx := context.Background()
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.CreateGallery(ctx, "Holiday"); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db}
	server.templates, err = parseTemplates()
	if err != nil {
		t.Fatal(err)
	}
	routes := server.Routes()
	defer server.dlnaCancel()

	post := func(path string, form url.Values) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		response := httptest.NewRecorder()
		routes.ServeHTTP(response, req)
		return response
	}
	text := "First line\n<script>alert('x')</script>"
	response := post("/galleries/1/description", url.Values{"description": {text}})
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/galleries/1" {
		t.Fatalf("save: %d %q", response.Code, response.Header().Get("Location"))
	}
	response = httptest.NewRecorder()
	routes.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/galleries/1", nil))
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `class="gallery-description"`) ||
		!strings.Contains(body, "First line\n&lt;script&gt;") || strings.Contains(body, "<script>alert('x')</script>") ||
		!strings.Contains(body, `name="description"`) || !strings.Contains(body, ">edit</span>") ||
		strings.Index(body, "</h1>") > strings.Index(body, `class="gallery-description"`) {
		t.Fatalf("gallery page: %d %s", response.Code, body)
	}
	for _, path := range []string{"/galleries/0/description", "/galleries/999/description", "/galleries/invalid/description"} {
		if response := post(path, url.Values{"description": {"text"}}); response.Code != http.StatusNotFound {
			t.Errorf("%s: status %d", path, response.Code)
		}
	}
	for _, form := range []url.Values{{}, {"description": {strings.Repeat("x", 10001)}}} {
		if response := post("/galleries/1/description", form); response.Code != http.StatusBadRequest {
			t.Errorf("invalid form: status %d", response.Code)
		}
	}
	if response := post("/galleries/1/description", url.Values{"description": {""}}); response.Code != http.StatusSeeOther {
		t.Fatalf("clear: status %d", response.Code)
	}
	if gallery, err := db.CollectionByID(ctx, 1); err != nil || gallery.Description != "" {
		t.Fatalf("cleared gallery: %+v, %v", gallery, err)
	}
}
