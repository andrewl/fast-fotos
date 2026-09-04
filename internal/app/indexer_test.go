package app

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCreateThumbnailResizesAndEncodesJPEG tests thumbnail creation and image dimension bounds scaling.
func TestCreateThumbnailResizesAndEncodesJPEG(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jpg")
	file, err := os.Create(path)
	if err != nil {
		t.Fatalf("create source image: %v", err)
	}

	source := image.NewRGBA(image.Rect(0, 0, 1600, 800))
	source.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	if err := jpeg.Encode(file, source, nil); err != nil {
		t.Fatalf("encode source image: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close source image: %v", err)
	}

	thumbnail, err := createThumbnail(path, "bilinear")
	if err != nil {
		t.Fatalf("create thumbnail: %v", err)
	}
	decoded, format, err := image.DecodeConfig(bytes.NewReader(thumbnail))
	if err != nil {
		t.Fatalf("decode thumbnail: %v", err)
	}
	if format != "jpeg" {
		t.Errorf("thumbnail format = %q, want jpeg", format)
	}
	if decoded.Width != thumbnailMaxDimension || decoded.Height != thumbnailMaxDimension/2 {
		t.Errorf("thumbnail dimensions = %dx%d, want %dx%d", decoded.Width, decoded.Height, thumbnailMaxDimension, thumbnailMaxDimension/2)
	}
}

// TestCreateThumbnailPrefersEmbeddedJPEGThumbnail verifies that embedded EXIF JPEG thumbnails are extracted when available.
func TestCreateThumbnailPrefersEmbeddedJPEGThumbnail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.jpg")
	embedded := jpegData(t, image.NewRGBA(image.Rect(0, 0, 3, 2)))
	if err := os.WriteFile(path, jpegWithEmbeddedThumbnail(embedded), 0o644); err != nil {
		t.Fatalf("write JPEG with embedded thumbnail: %v", err)
	}

	thumbnail, err := createThumbnail(path, "bilinear")
	if err != nil {
		t.Fatalf("create thumbnail: %v", err)
	}
	if !bytes.Equal(thumbnail, embedded) {
		t.Error("createThumbnail() did not preserve the embedded JPEG thumbnail")
	}
}

// jpegData renders an image into JPEG-encoded byte slice.
func jpegData(t *testing.T, source image.Image) []byte {
	t.Helper()
	var output bytes.Buffer
	if err := jpeg.Encode(&output, source, nil); err != nil {
		t.Fatalf("encode JPEG: %v", err)
	}
	return output.Bytes()
}

// jpegWithEmbeddedThumbnail constructs a mock JPEG with EXIF header containing a thumbnail byte payload.
func jpegWithEmbeddedThumbnail(thumbnail []byte) []byte {
	const tiffHeaderSize = 8
	const ifd0Offset = tiffHeaderSize
	const ifd1Offset = ifd0Offset + 2 + 4
	const thumbnailOffset = ifd1Offset + 2 + 2*12 + 4
	tiff := make([]byte, thumbnailOffset+len(thumbnail))
	copy(tiff, []byte{'I', 'I', 42, 0})
	binary.LittleEndian.PutUint32(tiff[4:], ifd0Offset)
	binary.LittleEndian.PutUint16(tiff[ifd1Offset:], 2)
	writeIFDEntry(tiff[ifd1Offset+2:], 0x0201, 4, 1, thumbnailOffset)
	writeIFDEntry(tiff[ifd1Offset+14:], 0x0202, 4, 1, len(thumbnail))
	copy(tiff[thumbnailOffset:], thumbnail)
	binary.LittleEndian.PutUint32(tiff[ifd0Offset+2:], ifd1Offset)

	app1 := append([]byte("Exif\x00\x00"), tiff...)
	result := []byte{0xff, 0xd8, 0xff, 0xe1, byte((len(app1) + 2) >> 8), byte(len(app1) + 2)}
	result = append(result, app1...)
	return append(result, 0xff, 0xd9)
}

// writeIFDEntry writes a single TIFF IFD entry structure into target byte slice.
func writeIFDEntry(destination []byte, tag, kind, count, value int) {
	binary.LittleEndian.PutUint16(destination, uint16(tag))
	binary.LittleEndian.PutUint16(destination[2:], uint16(kind))
	binary.LittleEndian.PutUint32(destination[4:], uint32(count))
	binary.LittleEndian.PutUint32(destination[8:], uint32(value))
}

// TestResizeFilter tests loading valid image interpolators and rejecting invalid filter names.
func TestResizeFilter(t *testing.T) {
	for _, name := range []string{"", "bilinear", "approx-bilinear", "catmull-rom"} {
		if filter, err := resizeFilter(name); err != nil || filter == nil {
			t.Errorf("resizeFilter(%q) = (%v, %v), want a filter", name, filter, err)
		}
	}
	if _, err := resizeFilter("invalid"); err == nil {
		t.Error("resizeFilter(invalid) should return an error")
	}
}

// TestAdjacentPhotoIDs tests resolving previous and next photo IDs in a slice of photos.
func TestAdjacentPhotoIDs(t *testing.T) {
	photos := []Photo{{ID: 10}, {ID: 20}, {ID: 30}}
	tests := []struct {
		currentID, previousID, nextID int64
	}{
		{10, 0, 20},
		{20, 10, 30},
		{30, 20, 0},
		{40, 0, 0},
	}
	for _, test := range tests {
		previousID, nextID := adjacentPhotoIDs(photos, test.currentID)
		if previousID != test.previousID || nextID != test.nextID {
			t.Errorf("adjacentPhotoIDs(%d) = (%d, %d), want (%d, %d)", test.currentID, previousID, nextID, test.previousID, test.nextID)
		}
	}
}

// TestPhotoPathsModifiedSinceIncludesOnlyNewerImages tests scanning photo paths modified after a timestamp.
func TestPhotoPathsModifiedSinceIncludesOnlyNewerImages(t *testing.T) {
	root := t.TempDir()
	oldPath := filepath.Join(root, "old.jpg")
	newPath := filepath.Join(root, "new.jpg")
	for _, path := range []string{oldPath, newPath} {
		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("create %q: %v", path, err)
		}
	}
	cutoff := time.Now().Add(-time.Hour)
	if err := os.Chtimes(oldPath, cutoff.Add(-time.Second), cutoff.Add(-time.Second)); err != nil {
		t.Fatalf("set old photo timestamp: %v", err)
	}
	if err := os.Chtimes(newPath, cutoff.Add(time.Second), cutoff.Add(time.Second)); err != nil {
		t.Fatalf("set new photo timestamp: %v", err)
	}

	paths, err := photoPathsModifiedSince(root, &cutoff)
	if err != nil {
		t.Fatalf("photo paths modified since: %v", err)
	}
	if len(paths) != 1 || paths[0] != newPath {
		t.Errorf("photo paths = %v, want [%q]", paths, newPath)
	}
}

// TestPhotoPathsIncludesOnlySupportedImages tests filtering directory files by supported image extensions.
func TestPhotoPathsIncludesOnlySupportedImages(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"one.jpg", "two.PNG", "notes.txt", "nested/three.webp"} {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("create directory for %q: %v", name, err)
		}

		if err := os.WriteFile(path, nil, 0o644); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	paths, err := photoPaths(root)
	if err != nil {
		t.Fatalf("photoPaths: %v", err)
	}
	if len(paths) != 3 {
		t.Fatalf("photo paths = %d, want 3", len(paths))
	}
}

// TestAssociatedRawPathMatchesBasename tests finding matching raw files with the same basename in a directory.
func TestAssociatedRawPathMatchesBasename(t *testing.T) {
	dir := t.TempDir()
	imagePath := filepath.Join(dir, "aabb.jpg")
	rawPath := filepath.Join(dir, "aabb.RW2")
	if err := os.WriteFile(imagePath, nil, 0o644); err != nil {
		t.Fatalf("create image: %v", err)
	}
	if err := os.WriteFile(rawPath, nil, 0o644); err != nil {
		t.Fatalf("create raw file: %v", err)
	}

	got, err := associatedRawPath(imagePath)
	if err != nil {
		t.Fatalf("associatedRawPath: %v", err)
	}
	if got != rawPath {
		t.Errorf("associatedRawPath() = %q, want %q", got, rawPath)
	}
}
