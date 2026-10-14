package main

import (
	"bytes"
	"encoding/binary"
)

// icon draws a 32×32 .ico: a filled circle of one colour on a
// transparent ground, with a lighter ring — enough to tell idle,
// syncing, held and paused apart in the tray.
func icon(r, g, b uint8) []byte {
	const n = 32
	pixels := make([]byte, 0, n*n*4)
	for y := n - 1; y >= 0; y-- { // bottom-up
		for x := 0; x < n; x++ {
			dx, dy := float64(x)-15.5, float64(y)-15.5
			d := dx*dx + dy*dy
			switch {
			case d <= 12*12:
				pixels = append(pixels, b, g, r, 255)
			case d <= 15*15:
				pixels = append(pixels, lighten(b), lighten(g), lighten(r), 255)
			default:
				pixels = append(pixels, 0, 0, 0, 0)
			}
		}
	}
	mask := make([]byte, n*4) // AND mask, 32 rows of 4 bytes, all drawn by alpha
	var img bytes.Buffer
	w := func(v any) { binary.Write(&img, binary.LittleEndian, v) }
	w(uint32(40))   // BITMAPINFOHEADER size
	w(int32(n))     // width
	w(int32(2 * n)) // height: the image and the mask
	w(uint16(1))    // planes
	w(uint16(32))   // bits per pixel
	w(uint32(0))    // no compression
	w(uint32(len(pixels) + len(mask)))
	w(int32(0))
	w(int32(0))
	w(uint32(0))
	w(uint32(0))
	img.Write(pixels)
	img.Write(mask)

	var ico bytes.Buffer
	v := func(x any) { binary.Write(&ico, binary.LittleEndian, x) }
	v(uint16(0)) // reserved
	v(uint16(1)) // an icon
	v(uint16(1)) // one image
	ico.Write([]byte{n, n, 0, 0})
	v(uint16(1))  // planes
	v(uint16(32)) // bits per pixel
	v(uint32(img.Len()))
	v(uint32(6 + 16))
	ico.Write(img.Bytes())
	return ico.Bytes()
}

func lighten(c uint8) uint8 { return c + (255-c)/2 }

var (
	iconIdle    = icon(0x2e, 0x9e, 0x4f) // green: up to date
	iconBusy    = icon(0x1f, 0x6f, 0xd1) // blue: syncing
	iconProblem = icon(0xd9, 0x8c, 0x1f) // amber: held, or failing
	iconPaused  = icon(0x8a, 0x8a, 0x8a) // grey: paused
)
