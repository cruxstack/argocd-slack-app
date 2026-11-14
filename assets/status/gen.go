//go:build ignore

// Generates the status icons: go run assets/status/gen.go assets/status
package main

import (
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

const size, ss = 128, 4 // output px, supersampling

type seg struct{ x1, y1, x2, y2 float64 }

func distSeg(px, py float64, s seg) float64 {
	dx, dy := s.x2-s.x1, s.y2-s.y1
	t := ((px-s.x1)*dx + (py-s.y1)*dy) / (dx*dx + dy*dy)
	t = math.Max(0, math.Min(1, t))
	cx, cy := s.x1+t*dx, s.y1+t*dy
	return math.Hypot(px-cx, py-cy)
}

// draw renders a filled circle in bg with white strokes (in unit coords 0..1).
func draw(path string, bg color.RGBA, strokes []seg, dots [][2]float64, ring bool) {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	n := size * ss
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			var r, g, b, a float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					u := (float64(x*ss+sx) + 0.5) / float64(n)
					v := (float64(y*ss+sy) + 0.5) / float64(n)
					d := math.Hypot(u-0.5, v-0.5)
					if d > 0.5 {
						continue
					}
					white := false
					for _, s := range strokes {
						if distSeg(u, v, s) < 0.055 {
							white = true
						}
					}
					for _, p := range dots {
						if math.Hypot(u-p[0], v-p[1]) < 0.06 {
							white = true
						}
					}
					if ring && d > 0.22 && d < 0.32 {
						ang := math.Atan2(v-0.5, u-0.5)
						if ang < -1.75 || ang > -0.35 { // open ring, like a spinner
							white = true
						}
					}
					if white {
						r, g, b = r+255, g+255, b+255
					} else {
						r, g, b = r+float64(bg.R), g+float64(bg.G), b+float64(bg.B)
					}
					a++
				}
			}
			if a == 0 {
				continue
			}
			k := float64(ss * ss)
			// premultiplied RGBA
			img.SetRGBA(x, y, color.RGBA{uint8(r / k), uint8(g / k), uint8(b / k), uint8(255 * a / k)})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		panic(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		panic(err)
	}
}

func main() {
	dir := os.Args[1]
	check := []seg{{0.29, 0.52, 0.44, 0.67}, {0.44, 0.67, 0.72, 0.36}}
	cross := []seg{{0.34, 0.34, 0.66, 0.66}, {0.66, 0.34, 0.34, 0.66}}
	bang := []seg{{0.5, 0.27, 0.5, 0.56}}
	minus := []seg{{0.32, 0.5, 0.68, 0.5}}
	draw(filepath.Join(dir, "ok.png"), color.RGBA{0x2B, 0xAC, 0x76, 255}, check, nil, false)
	draw(filepath.Join(dir, "fail.png"), color.RGBA{0xE0, 0x1E, 0x5A, 255}, cross, nil, false)
	draw(filepath.Join(dir, "warn.png"), color.RGBA{0xE8, 0xA2, 0x0C, 255}, bang, [][2]float64{{0.5, 0.71}}, false)
	draw(filepath.Join(dir, "run.png"), color.RGBA{0x1D, 0x9B, 0xD1, 255}, nil, nil, true)
	draw(filepath.Join(dir, "muted.png"), color.RGBA{0x8D, 0x8D, 0x8F, 255}, minus, nil, false)
}
