package objects

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/png"
	"os"
	"testing"
)

// TestImageInputPreservesAspectRatioWithPadding tests that image scaling pads borders with black while preserving aspect ratio.
func TestImageInputPreservesAspectRatioWithPadding(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 1))
	source.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	source.SetRGBA(1, 0, color.RGBA{B: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatalf("encode source: %v", err)
	}

	input, err := ssdImageInput(encoded.Bytes())
	if err != nil {
		t.Fatalf("ssdImageInput: %v", err)
	}
	pixel := func(x, y int) (uint8, uint8, uint8) {
		offset := (y*inputSizeSSD + x) * 3
		return input[offset], input[offset+1], input[offset+2]
	}
	topR, topG, topB := pixel(200, 0)
	if topR != 0 || topG != 0 || topB != 0 {
		t.Error("top padding should be black")
	}
	redR, redG, redB := pixel(100, 200)
	blueR, blueG, blueB := pixel(299, 200)
	if redR < 200 || redG > 40 || redB > 40 {
		t.Errorf("left image content = (%d, %d, %d), want red", redR, redG, redB)
	}
	if blueB < 200 || blueR > 40 || blueG > 40 {
		t.Errorf("right image content = (%d, %d, %d), want blue", blueR, blueG, blueB)
	}
}

// TestYOLOv8ImageInput tests that YOLO input processing returns planar float32 image data of expected length.
func TestYOLOv8ImageInput(t *testing.T) {
	source := image.NewRGBA(image.Rect(0, 0, 2, 1))
	source.SetRGBA(0, 0, color.RGBA{R: 255, A: 255})
	source.SetRGBA(1, 0, color.RGBA{B: 255, A: 255})
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, source); err != nil {
		t.Fatalf("encode source: %v", err)
	}

	input, err := yoloImageInput(encoded.Bytes())
	if err != nil {
		t.Fatalf("yoloImageInput: %v", err)
	}
	planeSize := inputSizeYOLOv8 * inputSizeYOLOv8
	if len(input) != 3*planeSize {
		t.Fatalf("input length = %d, want %d", len(input), 3*planeSize)
	}
}

// TestInspectYOLOv11 tests object detection using a local YOLOv11 model if available.
func TestInspectYOLOv11(t *testing.T) {
	dir := "/tmp/test-yolo11"
	if _, err := os.Stat(dir + "/yolo11s.onnx"); err != nil {
		t.Skip("yolo11s.onnx not present")
	}
	detector, err := LoadONNXDetector(dir)
	if err != nil {
		t.Fatalf("LoadONNXDetector: %v", err)
	}
	defer detector.Close()

	testImages := []string{
		"../../test-images/05/IMG_20260525_093818333.jpg",
		"../../test-images/07/IMG_20250725_115152035.jpg",
		"../../test-images/07/IMG_20260725_154452995.jpg",
		"../../test-images/07/IMG_20260727_143828613.jpg",
	}
	for _, imgPath := range testImages {
		data, err := os.ReadFile(imgPath)
		if err != nil {
			t.Logf("Skip %s: %v", imgPath, err)
			continue
		}
		labels, err := detector.Detect(context.Background(), data)
		if err != nil {
			t.Fatalf("Detect for %s: %v", imgPath, err)
		}
		t.Logf("%s labels: %v", imgPath, labels)
	}
}
