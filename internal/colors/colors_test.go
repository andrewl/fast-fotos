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

func solidColors(t *testing.T, fill func(x, y int) color.RGBA) []string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for y := 0; y < 100; y++ {
		for x := 0; x < 100; x++ {
			img.SetRGBA(x, y, fill(x, y))
		}
	}
	return ExtractDominantColorsFromImage(img, 3)
}

// TestGreyRequiresNeutralDominantCluster verifies Grey is only reported for large, truly neutral areas.
func TestGreyRequiresNeutralDominantCluster(t *testing.T) {
	grey := color.RGBA{R: 128, G: 128, B: 128, A: 255}
	if got := solidColors(t, func(x, y int) color.RGBA { return grey }); len(got) != 1 || got[0] != "Grey" {
		t.Errorf("neutral grey image = %v, want [Grey]", got)
	}
	// Muted brownish grey is not neutral enough.
	muted := color.RGBA{R: 140, G: 125, B: 110, A: 255}
	for _, c := range solidColors(t, func(x, y int) color.RGBA { return muted }) {
		if c == "Grey" {
			t.Errorf("muted color tagged Grey")
		}
	}
	// Small neutral area (20%) inside a red image is not dominant enough.
	for _, c := range solidColors(t, func(x, y int) color.RGBA {
		if x < 20 {
			return grey
		}
		return color.RGBA{R: 220, G: 20, B: 20, A: 255}
	}) {
		if c == "Grey" {
			t.Errorf("minor grey area tagged Grey")
		}
	}
}
