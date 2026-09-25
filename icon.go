package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"net/http"
	"sync"
)

var (
	iconMu    sync.Mutex
	iconCache = map[int][]byte{}
	iconBlue  = color.RGBA{0x2f, 0x6f, 0xed, 0xff}
)

// drawIcon renders the logo (a white ring on a blue rounded square) at size×size.
// Generated in code so the repo carries no binary assets.
func drawIcon(size int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	radius := s * 0.22
	ringR, ringW := s*0.24, s*0.075
	cx, cy := s/2, s*0.47
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			fx, fy := float64(x)+0.5, float64(y)+0.5
			// rounded-square mask
			dx := math.Max(math.Max(radius-fx, fx-(s-radius)), 0)
			dy := math.Max(math.Max(radius-fy, fy-(s-radius)), 0)
			if dx*dx+dy*dy > radius*radius {
				continue
			}
			d := math.Hypot(fx-cx, fy-cy)
			if math.Abs(d-ringR) < ringW/2 {
				img.Set(x, y, color.White)
			} else {
				img.Set(x, y, iconBlue)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

func (a *App) handleIcon(w http.ResponseWriter, r *http.Request) {
	size := map[string]int{"/icon-32.png": 32, "/icon-192.png": 192, "/icon-512.png": 512}[r.URL.Path]
	if size == 0 {
		http.NotFound(w, r)
		return
	}
	iconMu.Lock()
	b, ok := iconCache[size]
	if !ok {
		b = drawIcon(size)
		iconCache[size] = b
	}
	iconMu.Unlock()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.Write(b)
}
