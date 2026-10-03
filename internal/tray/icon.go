package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"
)

// Icon colors. Green is reserved for "everything connected".
var iconColors = map[Look]color.NRGBA{
	LookUp:      {0x2e, 0xa0, 0x43, 0xff}, // green
	LookPartial: {0x2f, 0x7d, 0xd6, 0xff}, // blue
	LookBusy:    {0xd9, 0x9a, 0x06, 0xff}, // amber
	LookError:   {0xd0, 0x31, 0x2d, 0xff}, // red
	LookIdle:    {0x8a, 0x8f, 0x98, 0xff}, // gray
}

var (
	iconMu    sync.Mutex
	iconCache = map[Look]*image.NRGBA{}
)

// Icon returns a PNG for a look: two linked rings (a tether) in its color.
func Icon(state Look) []byte {
	var buf bytes.Buffer
	png.Encode(&buf, iconImage(state))
	return buf.Bytes()
}

// IconPixmap is the icon for a look as a StatusNotifierItem shows it:
// ARGB32 pixels, not premultiplied, in network byte order.
func IconPixmap(state Look) (width, height int, argb []byte) {
	img := iconImage(state)
	b := img.Bounds()
	argb = make([]byte, 0, 4*b.Dx()*b.Dy())
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			c := img.NRGBAAt(x, y)
			argb = append(argb, c.A, c.R, c.G, c.B)
		}
	}
	return b.Dx(), b.Dy(), argb
}

func iconImage(state Look) *image.NRGBA {
	iconMu.Lock()
	defer iconMu.Unlock()
	if img, ok := iconCache[state]; ok {
		return img
	}
	c, ok := iconColors[state]
	if !ok {
		c = iconColors[LookIdle]
	}
	const size = 64
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	// Two overlapping rings, anti-aliased by coverage of the ring's edge.
	rings := [][2]float64{{22, 32}, {42, 32}}
	const radius, width = 15.0, 6.0
	for y := range size {
		for x := range size {
			cov := 0.0
			for _, r := range rings {
				d := math.Hypot(float64(x)+0.5-r[0], float64(y)+0.5-r[1])
				// Distance from the ring's center line, as coverage in [0,1].
				cov = math.Max(cov, math.Min(1, math.Max(0, width/2+0.5-math.Abs(d-radius))))
			}
			if cov > 0 {
				img.SetNRGBA(x, y, color.NRGBA{c.R, c.G, c.B, uint8(float64(c.A) * cov)})
			}
		}
	}
	iconCache[state] = img
	return img
}
