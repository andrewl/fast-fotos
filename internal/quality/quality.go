// Package quality computes technical image quality metrics: sharpness, exposure, contrast and BRISQUE.
package quality

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"math"
	"sync"
)

// Metrics holds the quality scores for one image.
type Metrics struct {
	// Sharpness is the variance of the Laplacian of the 0-255 luma image; higher is sharper.
	Sharpness float64
	// Exposure is the mean luma in [0,1]; about 0.5 is well exposed, near 0 under- and near 1 over-exposed.
	Exposure float64
	// Contrast is the 5th-95th percentile luma spread in [0,1]; higher is more contrasty.
	Contrast float64
	// Brisque is the BRISQUE score, typically 0-100; lower is better quality.
	Brisque float64
}

// Analyze decodes an encoded image and computes all quality metrics.
func Analyze(data []byte) (Metrics, error) {
	img, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return Metrics{}, fmt.Errorf("decode image: %w", err)
	}
	gray := luma(img)
	if gray.w < 8 || gray.h < 8 {
		return Metrics{}, fmt.Errorf("image too small: %dx%d", gray.w, gray.h)
	}
	m := Metrics{Sharpness: laplacianVariance(gray)}
	m.Exposure, m.Contrast = histogramStats(gray)
	m.Brisque, err = brisque(gray)
	if err != nil {
		return Metrics{}, err
	}
	return m, nil
}

// plane is a single-channel float image with values in 0-255.
type plane struct {
	w, h int
	pix  []float64
}

func luma(img image.Image) plane {
	b := img.Bounds()
	p := plane{w: b.Dx(), h: b.Dy(), pix: make([]float64, b.Dx()*b.Dy())}
	for y := 0; y < p.h; y++ {
		for x := 0; x < p.w; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			p.pix[y*p.w+x] = (0.299*float64(r) + 0.587*float64(g) + 0.114*float64(bl)) / 257
		}
	}
	return p
}

// laplacianVariance returns the variance of the 3x3 Laplacian response over interior pixels.
func laplacianVariance(p plane) float64 {
	var sum, sumSq float64
	n := 0
	for y := 1; y < p.h-1; y++ {
		for x := 1; x < p.w-1; x++ {
			i := y*p.w + x
			v := p.pix[i-p.w] + p.pix[i+p.w] + p.pix[i-1] + p.pix[i+1] - 4*p.pix[i]
			sum += v
			sumSq += v * v
			n++
		}
	}
	mean := sum / float64(n)
	return sumSq/float64(n) - mean*mean
}

// histogramStats returns normalized mean luma and the 5th-95th percentile spread.
func histogramStats(p plane) (exposure, contrast float64) {
	var hist [256]int
	var sum float64
	for _, v := range p.pix {
		hist[int(math.Round(v))]++
		sum += v
	}
	total := len(p.pix)
	percentile := func(q float64) int {
		target := int(math.Ceil(q * float64(total)))
		acc := 0
		for i, c := range hist {
			acc += c
			if acc >= target {
				return i
			}
		}
		return 255
	}
	exposure = sum / float64(total) / 255
	contrast = float64(percentile(0.95)-percentile(0.05)) / 255
	return exposure, contrast
}

type svrModel struct {
	Gamma float64     `json:"gamma"`
	Rho   float64     `json:"rho"`
	Alpha []float64   `json:"alpha"`
	SV    [][]float64 `json:"sv"`
	Min   []float64   `json:"min"`
	Max   []float64   `json:"max"`
}

//go:embed brisque_model.json
var brisqueModelJSON []byte

var (
	modelOnce sync.Once
	model     svrModel
	modelErr  error
)

func loadModel() (*svrModel, error) {
	modelOnce.Do(func() {
		modelErr = json.Unmarshal(brisqueModelJSON, &model)
	})
	return &model, modelErr
}

// brisque computes the BRISQUE score using the LIVE-trained SVR model from OpenCV's quality module.
func brisque(p plane) (float64, error) {
	m, err := loadModel()
	if err != nil {
		return 0, fmt.Errorf("load BRISQUE model: %w", err)
	}
	features := make([]float64, 0, 36)
	cur := p
	for scale := 0; scale < 2; scale++ {
		features = append(features, scaleFeatures(mscn(cur))...)
		if scale == 0 {
			cur = halve(cur)
			if cur.w < 8 || cur.h < 8 {
				return 0, fmt.Errorf("image too small for BRISQUE")
			}
		}
	}
	for i, f := range features {
		span := m.Max[i] - m.Min[i]
		if span == 0 {
			features[i] = 0
			continue
		}
		features[i] = -1 + 2*(f-m.Min[i])/span
	}
	sum := 0.0
	for i, sv := range m.SV {
		d := 0.0
		for j, v := range sv {
			diff := features[j] - v
			d += diff * diff
		}
		sum += m.Alpha[i] * math.Exp(-m.Gamma*d)
	}
	score := sum - m.Rho
	if math.IsNaN(score) || math.IsInf(score, 0) {
		return 0, fmt.Errorf("BRISQUE produced a non-finite score")
	}
	return score, nil
}

// halve downsamples a plane by two using 2x2 averaging.
func halve(p plane) plane {
	w, h := p.w/2, p.h/2
	out := plane{w: w, h: h, pix: make([]float64, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := 2*y*p.w + 2*x
			out.pix[y*w+x] = (p.pix[i] + p.pix[i+1] + p.pix[i+p.w] + p.pix[i+p.w+1]) / 4
		}
	}
	return out
}

var gaussKernel = func() [7]float64 {
	var k [7]float64
	sigma := 7.0 / 6.0
	sum := 0.0
	for i := range k {
		d := float64(i - 3)
		k[i] = math.Exp(-d * d / (2 * sigma * sigma))
		sum += k[i]
	}
	for i := range k {
		k[i] /= sum
	}
	return k
}()

func reflect(i, n int) int {
	if i < 0 {
		i = -i
	}
	if i >= n {
		i = 2*(n-1) - i
	}
	return i
}

func blur(p plane) []float64 {
	tmp := make([]float64, len(p.pix))
	out := make([]float64, len(p.pix))
	for y := 0; y < p.h; y++ {
		for x := 0; x < p.w; x++ {
			s := 0.0
			for k, w := range gaussKernel {
				s += w * p.pix[y*p.w+reflect(x+k-3, p.w)]
			}
			tmp[y*p.w+x] = s
		}
	}
	for y := 0; y < p.h; y++ {
		for x := 0; x < p.w; x++ {
			s := 0.0
			for k, w := range gaussKernel {
				s += w * tmp[reflect(y+k-3, p.h)*p.w+x]
			}
			out[y*p.w+x] = s
		}
	}
	return out
}

// mscn returns the mean-subtracted contrast-normalized coefficients.
func mscn(p plane) plane {
	mu := blur(p)
	sq := plane{w: p.w, h: p.h, pix: make([]float64, len(p.pix))}
	for i, v := range p.pix {
		sq.pix[i] = v * v
	}
	muSq := blur(sq)
	out := plane{w: p.w, h: p.h, pix: make([]float64, len(p.pix))}
	for i, v := range p.pix {
		sigma := math.Sqrt(math.Abs(muSq[i] - mu[i]*mu[i]))
		out.pix[i] = (v - mu[i]) / (sigma + 1)
	}
	return out
}

// scaleFeatures returns the 18 BRISQUE features for one scale.
func scaleFeatures(s plane) []float64 {
	gamma, l, r := aggdFit(s.pix)
	feats := []float64{gamma, (l*l + r*r) / 2}
	shifts := [][2]int{{0, 1}, {1, 0}, {1, 1}, {-1, 1}}
	for _, sh := range shifts {
		pairs := make([]float64, 0, len(s.pix))
		for y := 0; y < s.h; y++ {
			y2 := y + sh[0]
			if y2 < 0 || y2 >= s.h {
				continue
			}
			for x := 0; x < s.w; x++ {
				x2 := x + sh[1]
				if x2 < 0 || x2 >= s.w {
					continue
				}
				pairs = append(pairs, s.pix[y*s.w+x]*s.pix[y2*s.w+x2])
			}
		}
		g, ls, rs := aggdFit(pairs)
		constant := math.Sqrt(gammaFn(1/g) / gammaFn(3/g))
		mean := (rs - ls) * (gammaFn(2/g) / gammaFn(1/g)) * constant
		feats = append(feats, g, mean, ls*ls, rs*rs)
	}
	return feats
}

func gammaFn(x float64) float64 { return math.Gamma(x) }

var (
	gammaGrid    []float64
	gammaRatios  []float64
	gammaGridOne sync.Once
)

func initGammaGrid() {
	for g := 0.2; g <= 10; g += 0.001 {
		gammaGrid = append(gammaGrid, g)
		gammaRatios = append(gammaRatios, math.Pow(gammaFn(2/g), 2)/(gammaFn(1/g)*gammaFn(3/g)))
	}
}

// aggdFit fits an asymmetric generalized Gaussian, returning shape and left/right standard deviations.
func aggdFit(values []float64) (gamma, left, right float64) {
	gammaGridOne.Do(initGammaGrid)
	var leftSq, rightSq, abs float64
	var nl, nr int
	for _, v := range values {
		switch {
		case v < 0:
			leftSq += v * v
			nl++
		case v > 0:
			rightSq += v * v
			nr++
		}
		abs += math.Abs(v)
	}
	if nl == 0 || nr == 0 {
		return 0.2, 0, 0
	}
	left = math.Sqrt(leftSq / float64(nl))
	right = math.Sqrt(rightSq / float64(nr))
	n := float64(len(values))
	gammaHat := left / right
	rHat := (abs / n) * (abs / n) / ((leftSq + rightSq) / n)
	rHatNorm := rHat * (math.Pow(gammaHat, 3) + 1) * (gammaHat + 1) / math.Pow(gammaHat*gammaHat+1, 2)
	best, bestDiff := 0, math.Inf(1)
	for i, r := range gammaRatios {
		if d := (r - rHatNorm) * (r - rHatNorm); d < bestDiff {
			best, bestDiff = i, d
		}
	}
	return gammaGrid[best], left, right
}
