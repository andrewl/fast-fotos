package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/draw"
	"image/jpeg"
	"image/png"
	"math"
)

// enhancePhoto preserves the JPEG orientation metadata rather than rotating the pixels.
func enhancePhoto(ctx context.Context, data []byte) ([]byte, error) {
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	if format != "jpeg" && format != "png" {
		return nil, fmt.Errorf("unsupported image format %q", format)
	}
	if config.Width <= 0 || config.Height <= 0 || int64(config.Width)*int64(config.Height) > 40_000_000 {
		return nil, fmt.Errorf("image too large to process")
	}
	src, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	img := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	draw.Draw(img, img.Bounds(), src, b.Min, draw.Src)
	adjustExposure(img)
	if err := sharpenAndVignette(ctx, img); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	if format == "png" {
		err = png.Encode(&out, img)
	} else {
		err = jpeg.Encode(&out, img, &jpeg.Options{Quality: 92})
	}
	if err != nil {
		return nil, err
	}
	if format == "jpeg" {
		return preserveJPEGMetadata(data, out.Bytes()), nil
	}
	return out.Bytes(), nil
}

// adjustExposure uses sampled luma percentiles and a bounded gamma on under/over-exposed images.
func adjustExposure(img *image.NRGBA) {
	var histogram [256]int
	var sum float64
	n := 0
	for y := 0; y < img.Rect.Dy(); y += max(1, img.Rect.Dy()/400) {
		for x := 0; x < img.Rect.Dx(); x += max(1, img.Rect.Dx()/400) {
			i := y*img.Stride + x*4
			if img.Pix[i+3] == 0 {
				continue
			}
			v := 0.299*float64(img.Pix[i]) + 0.587*float64(img.Pix[i+1]) + 0.114*float64(img.Pix[i+2])
			histogram[int(v+0.5)]++
			sum += v
			n++
		}
	}
	if n == 0 {
		return
	}
	mean := sum / float64(n) / 255
	if mean >= 0.35 && mean <= 0.65 {
		return
	}
	percentile := func(q float64) float64 {
		target, count := max(1, int(math.Ceil(q*float64(n)))), 0
		for i, v := range histogram {
			count += v
			if count >= target {
				return float64(i) / 255
			}
		}
		return 1
	}
	lo, hi := percentile(0.01), percentile(0.99)
	black, white := 0.0, 1.0
	if hi-lo < 0.8 && hi-lo > 0.05 {
		black, white = lo*0.25, 1-(1-hi)*0.25
	}
	// Compute gamma after the stretch, so the two adjustments don't overcorrect together.
	stretchedMean := min(0.999, max(0.001, (mean-black)/(white-black)))
	gamma := min(1.8, max(0.6, math.Log(0.47)/math.Log(stretchedMean)))
	var lut [256]byte
	for i := range lut {
		v := min(1, max(0, (float64(i)/255-black)/(white-black)))
		lut[i] = byte(math.Pow(v, gamma)*255 + 0.5)
	}
	for i := 0; i < len(img.Pix); i += 4 {
		img.Pix[i], img.Pix[i+1], img.Pix[i+2] = lut[img.Pix[i]], lut[img.Pix[i+1]], lut[img.Pix[i+2]]
	}
}

// sharpenAndVignette applies a separable Gaussian unsharp mask and a 10% corner vignette.
func sharpenAndVignette(ctx context.Context, img *image.NRGBA) error {
	w, h := img.Rect.Dx(), img.Rect.Dy()
	const radius, sigma, amount = 4, 1.2, 0.5
	var kernel [2*radius + 1]float32
	var total float32
	for i := range kernel {
		d := float64(i - radius)
		kernel[i] = float32(math.Exp(-d * d / (2 * sigma * sigma)))
		total += kernel[i]
	}
	for i := range kernel {
		kernel[i] /= total
	}
	blur := make([]float32, w*h*3)
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		for x := 0; x < w; x++ {
			for k, weight := range kernel {
				p := y*img.Stride + min(w-1, max(0, x+k-radius))*4
				for c := 0; c < 3; c++ {
					blur[(y*w+x)*3+c] += weight * float32(img.Pix[p+c])
				}
			}
		}
	}
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		dy := 0.0
		if h > 1 {
			dy = (2*float64(y)/float64(h-1) - 1)
		}
		for x := 0; x < w; x++ {
			dx := 0.0
			if w > 1 {
				dx = 2*float64(x)/float64(w-1) - 1
			}
			r2 := (dx*dx + dy*dy) / 2
			factor := float32(1 - 0.1*r2*math.Sqrt(r2))
			p := y*img.Stride + x*4
			for c := 0; c < 3; c++ {
				var smoothed float32
				for k, weight := range kernel {
					yy := min(h-1, max(0, y+k-radius))
					smoothed += weight * blur[(yy*w+x)*3+c]
				}
				value := float32(img.Pix[p+c])
				img.Pix[p+c] = byte(min(255, max(0, (value+amount*(value-smoothed))*factor)) + 0.5)
			}
		}
	}
	return nil
}

// Preserve EXIF, XMP, ICC profiles and comments, including the original orientation.
func preserveJPEGMetadata(original, encoded []byte) []byte {
	if len(original) < 2 || original[0] != 0xff || original[1] != 0xd8 {
		return encoded
	}
	var metadata bytes.Buffer
	for pos := 2; pos+4 <= len(original) && original[pos] == 0xff; {
		marker := original[pos+1]
		if marker == 0xda || marker == 0xd9 {
			break
		}
		length := int(binary.BigEndian.Uint16(original[pos+2 : pos+4]))
		if length < 2 || pos+2+length > len(original) {
			break
		}
		if marker == 0xe1 || marker == 0xe2 || marker == 0xed || marker == 0xfe {
			metadata.Write(original[pos : pos+2+length])
		}
		pos += 2 + length
	}
	if metadata.Len() == 0 {
		return encoded
	}
	result := make([]byte, 0, len(encoded)+metadata.Len())
	result = append(result, encoded[:2]...)
	result = append(result, metadata.Bytes()...)
	return append(result, encoded[2:]...)
}
