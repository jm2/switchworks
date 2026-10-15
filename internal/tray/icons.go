// SPDX-License-Identifier: GPL-3.0-or-later

package tray

import (
	"bytes"
	"embed"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"sync"
)

//go:embed icons/*.png
var iconFS embed.FS

// IconSizes are the sizes the tray icon is drawn at.
var IconSizes = []int{16, 20, 22, 24, 32, 48, 64}

var (
	dotOK   = color.NRGBA{0x3f, 0xb9, 0x50, 0xff}
	dotWarn = color.NRGBA{0xe3, 0xa0, 0x08, 0xff}
	ring    = color.NRGBA{0x16, 0x17, 0x1c, 0xff}
)

var iconCache sync.Map // key: [2]int{icon, size}

// IconImage returns the tray icon at one of IconSizes, with a status dot in
// the lower right corner: green when connected, amber when set up but not
// connected, none when nothing is set up.
func IconImage(ic Icon, size int) (*image.NRGBA, error) {
	key := [2]int{int(ic), size}
	if v, ok := iconCache.Load(key); ok {
		return v.(*image.NRGBA), nil
	}
	b, err := iconFS.ReadFile(fmt.Sprintf("icons/icon-%d.png", size))
	if err != nil {
		return nil, fmt.Errorf("no %dpx tray icon", size)
	}
	src, err := png.Decode(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	draw.Draw(img, img.Bounds(), src, src.Bounds().Min, draw.Src)
	var dot color.NRGBA
	switch ic {
	case IconOK:
		dot = dotOK
	case IconWarn:
		dot = dotWarn
	}
	if dot.A != 0 {
		drawDot(img, dot)
	}
	iconCache.Store(key, img)
	return img, nil
}

// drawDot paints a filled circle with a dark ring in the lower right quarter.
func drawDot(img *image.NRGBA, c color.NRGBA) {
	size := img.Bounds().Dx()
	r := float64(size) * 0.22
	if r < 3 {
		r = 3
	}
	cx, cy := float64(size)-r-0.5, float64(size)-r-0.5
	inner := r - max(1, float64(size)/16)
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			d2 := dx*dx + dy*dy
			switch {
			case d2 <= inner*inner:
				img.SetNRGBA(x, y, c)
			case d2 <= r*r:
				img.SetNRGBA(x, y, ring)
			}
		}
	}
}
