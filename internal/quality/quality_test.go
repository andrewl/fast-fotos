package quality

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"math/rand"
	"testing"
)

func encode(t *testing.T, f func(x, y int) uint8) []byte {
	img := image.NewGray(image.Rect(0, 0, 128, 128))
	for y := 0; y < 128; y++ {
		for x := 0; x < 128; x++ {
			img.SetGray(x, y, color.Gray{f(x, y)})
		}
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestAnalyze(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	flat, err := Analyze(encode(t, func(x, y int) uint8 { return 128 }))
	if err != nil {
		t.Fatal(err)
	}
	noisy, err := Analyze(encode(t, func(x, y int) uint8 { return uint8(rng.Intn(256)) }))
	if err != nil {
		t.Fatal(err)
	}
	if flat.Sharpness >= noisy.Sharpness {
		t.Errorf("sharpness flat %v should be below noisy %v", flat.Sharpness, noisy.Sharpness)
	}
	if flat.Exposure < 0.49 || flat.Exposure > 0.51 {
		t.Errorf("flat exposure = %v, want ~0.5", flat.Exposure)
	}
	if flat.Contrast != 0 || noisy.Contrast < 0.8 {
		t.Errorf("contrast flat=%v noisy=%v", flat.Contrast, noisy.Contrast)
	}
	t.Logf("flat=%+v noisy=%+v", flat, noisy)
}

func TestAnalyzeInvalid(t *testing.T) {
	if _, err := Analyze([]byte("nope")); err == nil {
		t.Fatal("expected error")
	}
}
