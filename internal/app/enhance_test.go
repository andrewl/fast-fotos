package app

import (
	"archive/zip"
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestAdjustExposure(t *testing.T) {
	for _, tt := range []struct {
		name    string
		input   uint8
		minimum uint8
		maximum uint8
	}{
		{"dark", 40, 41, 254},
		{"bright", 230, 1, 229},
		{"normal", 128, 128, 128},
	} {
		t.Run(tt.name, func(t *testing.T) {
			img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
			for i := 0; i < len(img.Pix); i += 4 {
				img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = tt.input, tt.input, tt.input, 255
			}
			adjustExposure(img)
			if got := img.Pix[0]; got < tt.minimum || got > tt.maximum {
				t.Fatalf("exposure result %d outside [%d,%d]", got, tt.minimum, tt.maximum)
			}
		})
	}
}

func TestSharpenAndVignette(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 101, 51))
	for y := 0; y < 51; y++ {
		for x := 0; x < 101; x++ {
			v := uint8(100)
			if x >= 50 {
				v = 160
			}
			img.SetNRGBA(x, y, color.NRGBA{R: v, G: v, B: v, A: 128})
		}
	}
	if err := sharpenAndVignette(context.Background(), img); err != nil {
		t.Fatal(err)
	}
	if img.NRGBAAt(49, 25).R >= 100 || img.NRGBAAt(50, 25).R <= 160 {
		t.Fatal("edge was not sharpened")
	}
	if got := img.NRGBAAt(0, 0); got.R != 90 || got.A != 128 {
		t.Fatalf("corner = %+v, want 10%% darkening and unchanged alpha", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sharpenAndVignette(ctx, img); err != context.Canceled {
		t.Fatalf("cancelled processing returned %v", err)
	}
}

func TestProcessedArchive(t *testing.T) {
	root := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 40, 40))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 40, 40, 40, 255
	}
	var jpegData, pngData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(&pngData, img); err != nil {
		t.Fatal(err)
	}
	files := map[string][]byte{
		"a.jpg": jpegData.Bytes(), "a.RW2": []byte("raw"), "a.xmp": []byte("sidecar"),
		"b.png": pngData.Bytes(), "c.heic": []byte("unsupported"),
	}
	for name, data := range files {
		if err := os.WriteFile(filepath.Join(root, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	server := &Server{config: Config{PhotoRoot: root}}
	for _, photo := range []struct{ path, raw string }{{"a.jpg", "a.RW2"}, {"b.png", ""}, {"c.heic", ""}} {
		if err := server.addDownloadFilesWithProcessing(context.Background(), archive, photo.path, photo.raw, true, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := zip.NewReader(bytes.NewReader(output.Bytes()), int64(output.Len()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][]byte{}
	for _, f := range reader.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		got[f.Name], err = io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(files) {
		t.Fatalf("archive contains %d files, want %d", len(got), len(files))
	}
	for _, name := range []string{"a-processed.jpg", "b-processed.png"} {
		result, _, err := image.Decode(bytes.NewReader(got[name]))
		if err != nil {
			t.Fatal(err)
		}
		r, _, _, _ := result.At(20, 20).RGBA()
		if r>>8 <= 40 {
			t.Errorf("%s was not brightened", name)
		}
	}
	for _, name := range []string{"a.RW2", "a.xmp", "c.heic"} {
		if !bytes.Equal(got[name], files[name]) {
			t.Errorf("%s was changed", name)
		}
	}
}

func TestPreserveJPEGMetadata(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	var jpegData bytes.Buffer
	if err := jpeg.Encode(&jpegData, img, nil); err != nil {
		t.Fatal(err)
	}
	metadata := []byte{0xff, 0xe1, 0x00, 0x08, 'E', 'x', 'i', 'f', 0, 0}
	input := append([]byte{0xff, 0xd8}, metadata...)
	input = append(input, jpegData.Bytes()[2:]...)
	out, err := enhancePhoto(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out[2:2+len(metadata)], metadata) {
		t.Fatal("JPEG metadata was not preserved")
	}
}

func TestProcessedDownloadRoutes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	img := image.NewNRGBA(image.Rect(0, 0, 20, 20))
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2], img.Pix[i+3] = 40, 40, 40, 255
	}

	var original bytes.Buffer
	if err := jpeg.Encode(&original, img, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "photo.jpg"), original.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "photo.raw"), []byte("raw"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "photo.jpg", RawPath: "photo.raw", TakenAt: now, IndexedAt: now, MediaType: "photo"}); err != nil {
		t.Fatal(err)
	}
	id, err := db.CreateGallery(ctx, "Test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.AddPhotosToGallery(ctx, id, []int64{1}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, config: Config{PhotoRoot: root}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /download/", server.download)
	mux.HandleFunc("GET /galleries/{id}/download", server.downloadGallery)
	for _, path := range []string{
		"/download/?ids=1&raw=true&processed=true",
		"/galleries/" + strconv.FormatInt(id, 10) + "/download?raw=true&processed=true",
		"/download/?ids=1&raw=true",
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, recorder.Code)
		}
		archive, err := zip.NewReader(bytes.NewReader(recorder.Body.Bytes()), int64(recorder.Body.Len()))
		if err != nil {
			t.Fatal(err)
		}
		if len(archive.File) != 2 {
			t.Fatalf("%s: got %d files", path, len(archive.File))
		}
		for _, file := range archive.File {
			r, err := file.Open()
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(r)
			r.Close()
			if err != nil {
				t.Fatal(err)
			}
			switch file.Name {
			case "photo.raw":
				if string(data) != "raw" {
					t.Fatal("raw file changed")
				}
			case "photo.jpg", "photo-processed.jpg":
				processed := strings.Contains(path, "processed=true")
				if (file.Name == "photo-processed.jpg") != processed || bytes.Equal(data, original.Bytes()) == processed {
					t.Fatalf("%s: incorrect image contents", path)
				}
			default:
				t.Fatalf("unexpected archive entry %s", file.Name)
			}
		}
	}
	for _, path := range []string{
		"/download/?ids=1&raw=false",
		"/download/?ids=1&raw=false&processed=true",
		"/galleries/" + strconv.FormatInt(id, 10) + "/download?raw=false&processed=true",
	} {
		recorder := httptest.NewRecorder()
		mux.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK || recorder.Header().Get("Content-Type") != "image/jpeg" {
			t.Fatalf("%s: status %d, type %s", path, recorder.Code, recorder.Header().Get("Content-Type"))
		}
		processed := strings.Contains(path, "processed=true")
		want := "photo.jpg"
		if processed {
			want = "photo-processed.jpg"
		}
		if !strings.Contains(recorder.Header().Get("Content-Disposition"), want) {
			t.Fatalf("%s: disposition %q", path, recorder.Header().Get("Content-Disposition"))
		}
		if bytes.Equal(recorder.Body.Bytes(), original.Bytes()) == processed {
			t.Fatalf("%s: incorrect direct file contents", path)
		}
		if _, _, err := image.Decode(bytes.NewReader(recorder.Body.Bytes())); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDownloadSingleFileFallbackAndSidecar(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "bad.jpg"), []byte("not an image"), 0600); err != nil {
		t.Fatal(err)
	}
	db, err := OpenDB(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	now := time.Now().UTC()
	if err := db.UpsertPhoto(ctx, PhotoRecord{Path: "bad.jpg", RawPath: "missing.raw", TakenAt: now, IndexedAt: now, MediaType: "photo"}); err != nil {
		t.Fatal(err)
	}
	server := &Server{db: db, config: Config{PhotoRoot: root}}
	request := httptest.NewRequest(http.MethodGet, "/download/?ids=1&raw=true&processed=true", nil)
	response := httptest.NewRecorder()
	server.download(response, request)
	if response.Code != http.StatusOK || response.Body.String() != "not an image" || !strings.Contains(response.Header().Get("Content-Disposition"), "bad.jpg") {
		t.Fatalf("fallback download: status %d, disposition %q, body %q", response.Code, response.Header().Get("Content-Disposition"), response.Body.String())
	}
	if err := os.WriteFile(filepath.Join(root, "bad.xmp"), []byte("sidecar"), 0600); err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	server.download(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Content-Type") != "application/zip" {
		t.Fatalf("sidecar download: status %d, type %s", response.Code, response.Header().Get("Content-Type"))
	}
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil || len(archive.File) != 2 {
		t.Fatalf("sidecar archive: %v", err)
	}
	if archive.File[0].Name != "bad.jpg" || archive.File[1].Name != "bad.xmp" {
		t.Fatalf("unexpected archive entries: %q, %q", archive.File[0].Name, archive.File[1].Name)
	}
}
