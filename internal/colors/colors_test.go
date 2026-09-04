package colors

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"testing"
)

// TestExtractDominantColors verifies that a solid color image produces the expected named dominant color.
func TestExtractDominantColors(t *testing.T) {
	// Create a red image
	img := image.NewRGBA(image.Rect(0, 0, 50, 50))
	for y := 0; y < 50; y++ {
		for x := 0; x < 50; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}

	dominant := ExtractDominantColors(buf.Bytes(), 3)
	if len(dominant) == 0 {
		t.Fatalf("expected dominant colors, got none")
	}
	if dominant[0] != "Red" {
		t.Errorf("expected dominant color Red, got %v", dominant[0])
	}
}

// TestExtractDominantColorsMulti verifies that a multi-colored image returns multiple dominant color names.
func TestExtractDominantColorsMulti(t *testing.T) {
	// Create half red, half blue image
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 50; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 220, G: 20, B: 20, A: 255})
		}
		for x := 50; x < 100; x++ {
			img.SetRGBA(x, y, color.RGBA{R: 20, G: 20, B: 220, A: 255})
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode: %v", err)
	}

	dominant := ExtractDominantColors(buf.Bytes(), 3)
	if len(dominant) < 2 {
		t.Fatalf("expected at least 2 dominant colors, got %v", dominant)
	}
	foundRed, foundBlue := false, false
	for _, c := range dominant {
		if c == "Red" {
			foundRed = true
		}
		if c == "Blue" {
			foundBlue = true
		}
	}
	if !foundRed || !foundBlue {
		t.Errorf("expected Red and Blue in dominant colors, got %v", dominant)
	}
}
