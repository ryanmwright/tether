package tray

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"sync"

	"github.com/ryanmwright/tether/internal/api"
)

// Icon colors, matching the TUI's state colors.
var iconColors = map[api.State]color.NRGBA{
	api.StateUp:       {0x2e, 0xa0, 0x43, 0xff}, // green
	api.StateDegraded: {0xd9, 0x9a, 0x06, 0xff}, // amber
	api.StateError:    {0xd0, 0x31, 0x2d, 0xff}, // red
	api.StateDown:     {0x8a, 0x8f, 0x98, 0xff}, // gray
}

var (
	iconMu    sync.Mutex
	iconCache = map[api.State][]byte{}
)

// Icon returns a PNG for an overall state: two linked rings (a tether) in
// the state's color.
func Icon(state api.State) []byte {
	iconMu.Lock()
	defer iconMu.Unlock()
	if b, ok := iconCache[state]; ok {
		return b
	}
	c, ok := iconColors[state]
	if !ok {
		c = iconColors[api.StateDown]
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
	var buf bytes.Buffer
	png.Encode(&buf, img)
	iconCache[state] = buf.Bytes()
	return iconCache[state]
}
