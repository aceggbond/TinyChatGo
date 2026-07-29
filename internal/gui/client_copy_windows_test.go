//go:build windows

package gui

import (
	"image"
	"image/color"
	"testing"
)

func TestEncodeClipboardDIBV5RoundTrip(t *testing.T) {
	source := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	source.SetNRGBA(0, 0, color.NRGBA{R: 240, G: 30, B: 20, A: 255})
	source.SetNRGBA(1, 0, color.NRGBA{R: 10, G: 210, B: 40, A: 220})
	source.SetNRGBA(0, 1, color.NRGBA{R: 50, G: 60, B: 230, A: 180})
	source.SetNRGBA(1, 1, color.NRGBA{R: 90, G: 100, B: 110, A: 255})

	data, err := encodeClipboardDIBV5(source)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeClipboardDIB(data)
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			want := source.NRGBAAt(x, y)
			got := color.NRGBAModel.Convert(decoded.At(x, y)).(color.NRGBA)
			if got != want {
				t.Fatalf("pixel (%d,%d) = %#v, want %#v", x, y, got, want)
			}
		}
	}
}
