package colors

import (
	"bytes"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"sort"
)

// Lab represents a color in the CIE-LAB color space with Lightness (L), A (green-red), and B (blue-yellow) components.
type Lab struct {
	L, A, B float64
}

// namedColor maps a human-readable color label to its reference CIE-LAB value.
type namedColor struct {
	name string
	lab  Lab
}

var referenceColors = []namedColor{
	{name: "Black", lab: rgbToLab(0, 0, 0)},
	{name: "White", lab: rgbToLab(255, 255, 255)},
	{name: "Grey", lab: rgbToLab(128, 128, 128)},
	{name: "Red", lab: rgbToLab(255, 0, 0)},
	{name: "Orange", lab: rgbToLab(255, 128, 0)},
	{name: "Yellow", lab: rgbToLab(255, 255, 0)},
	{name: "Green", lab: rgbToLab(0, 180, 0)},
	{name: "Cyan", lab: rgbToLab(0, 255, 255)},
	{name: "Blue", lab: rgbToLab(0, 0, 255)},
	{name: "Purple", lab: rgbToLab(128, 0, 128)},
	{name: "Pink", lab: rgbToLab(255, 128, 192)},
	{name: "Brown", lab: rgbToLab(139, 69, 19)},
}

// rgbToLab converts sRGB color values (0-255) to the CIE-LAB color space using D65 illuminant.
func rgbToLab(r, g, b uint8) Lab {
	var rf, gf, bf float64 = float64(r) / 255.0, float64(g) / 255.0, float64(b) / 255.0

	rf = pivotRGB(rf)
	gf = pivotRGB(gf)
	bf = pivotRGB(bf)

	// Observer = 2°, Illuminant = D65
	x := (rf*0.4124564 + gf*0.3575761 + bf*0.1804375) / 0.95047
	y := (rf*0.2126729 + gf*0.7151522 + bf*0.0721750) / 1.00000
	z := (rf*0.0193339 + gf*0.1191920 + bf*0.9503041) / 1.08883

	x = pivotXYZ(x)
	y = pivotXYZ(y)
	z = pivotXYZ(z)

	l := 116.0*y - 16.0
	a := 500.0 * (x - y)
	bVal := 200.0 * (y - z)
	return Lab{L: l, A: a, B: bVal}
}

// pivotRGB linearizes non-linear sRGB values.
func pivotRGB(n float64) float64 {
	if n > 0.04045 {
		return math.Pow((n+0.055)/1.055, 2.4)
	}
	return n / 12.92
}

// pivotXYZ applies the non-linear transfer function for XYZ-to-LAB conversion.
func pivotXYZ(n float64) float64 {
	if n > 0.008856 {
		return math.Pow(n, 1.0/3.0)
	}
	return (7.787037 * n) + (16.0 / 116.0)
}

// labDistance computes the Euclidean perceptual color distance (Delta E) in LAB space.
func labDistance(c1, c2 Lab) float64 {
	dL := c1.L - c2.L
	dA := c1.A - c2.A
	dB := c1.B - c2.B
	return math.Sqrt(dL*dL + dA*dA + dB*dB)
}

// labToColorName maps a LAB color coordinate to the nearest reference named color.
func labToColorName(l Lab) string {
	closestName := referenceColors[0].name
	minDist := labDistance(l, referenceColors[0].lab)
	for i := 1; i < len(referenceColors); i++ {
		dist := labDistance(l, referenceColors[i].lab)
		if dist < minDist {
			minDist = dist
			closestName = referenceColors[i].name
		}
	}
	return closestName
}

// ExtractDominantColors decodes an image, samples its pixels into LAB space,
// runs K-Means clustering with k clusters, and returns the dominant color names.
func ExtractDominantColors(data []byte, k int) []string {
	if len(data) == 0 || k <= 0 {
		return nil
	}
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil
	}
	return ExtractDominantColorsFromImage(img, k)
}

// ExtractDominantColorsFromImage samples pixel values from an image.Image into LAB space,
// runs K-Means clustering, and returns up to k dominant color names sorted by cluster size.
func ExtractDominantColorsFromImage(img image.Image, k int) []string {
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w == 0 || h == 0 || k <= 0 {
		return nil
	}

	stepX := max(1, w/45)
	stepY := max(1, h/45)

	var points []Lab
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stepY {
		for x := bounds.Min.X; x < bounds.Max.X; x += stepX {
			r, g, b, _ := img.At(x, y).RGBA()
			points = append(points, rgbToLab(uint8(r>>8), uint8(g>>8), uint8(b>>8)))
		}
	}

	if len(points) == 0 {
		return nil
	}
	if k > len(points) {
		k = len(points)
	}

	centroids := make([]Lab, k)
	for i := 0; i < k; i++ {
		idx := (i * len(points)) / k
		centroids[i] = points[idx]
	}

	assignments := make([]int, len(points))
	counts := make([]int, k)

	for iter := 0; iter < 15; iter++ {
		changed := false
		for i, pt := range points {
			bestCluster := 0
			minDist := labDistance(pt, centroids[0])
			for c := 1; c < k; c++ {
				dist := labDistance(pt, centroids[c])
				if dist < minDist {
					minDist = dist
					bestCluster = c
				}
			}
			if assignments[i] != bestCluster {
				assignments[i] = bestCluster
				changed = true
			}
		}

		if !changed && iter > 0 {
			break
		}

		sumL := make([]float64, k)
		sumA := make([]float64, k)
		sumB := make([]float64, k)
		for i := 0; i < k; i++ {
			counts[i] = 0
		}

		for i, pt := range points {
			c := assignments[i]
			sumL[c] += pt.L
			sumA[c] += pt.A
			sumB[c] += pt.B
			counts[c]++
		}

		for c := 0; c < k; c++ {
			if counts[c] > 0 {
				centroids[c] = Lab{
					L: sumL[c] / float64(counts[c]),
					A: sumA[c] / float64(counts[c]),
					B: sumB[c] / float64(counts[c]),
				}
			}
		}
	}

	type clusterInfo struct {
		centroid Lab
		count    int
	}
	clusters := make([]clusterInfo, k)
	for c := 0; c < k; c++ {
		clusters[c] = clusterInfo{centroid: centroids[c], count: counts[c]}
	}

	sort.Slice(clusters, func(i, j int) bool {
		return clusters[i].count > clusters[j].count
	})

	var result []string
	seen := make(map[string]bool)
	threshold := len(points) / 10

	for _, cl := range clusters {
		if cl.count < threshold && len(result) > 0 {
			continue
		}
		name := labToColorName(cl.centroid)
		if !seen[name] {
			seen[name] = true
			result = append(result, name)
		}
	}

	return result
}
