package main

import (
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

const iconSize = 256

func main() {
	source, err := os.Open("logo.png")
	if err != nil {
		panic(err)
	}
	defer source.Close()
	input, err := png.Decode(source)
	if err != nil {
		panic(err)
	}
	icon := downsample(input, iconSize, iconSize)
	pngFile, err := os.CreateTemp("", "hfs-go-icon-*.png")
	if err != nil {
		panic(err)
	}
	pngName := pngFile.Name()
	defer os.Remove(pngName)
	if err = png.Encode(pngFile, icon); err != nil {
		panic(err)
	}
	if err = pngFile.Close(); err != nil {
		panic(err)
	}
	pngData, err := os.ReadFile(pngName)
	if err != nil {
		panic(err)
	}
	output, err := os.Create("hfs-go.ico")
	if err != nil {
		panic(err)
	}
	defer output.Close()
	header := []byte{
		0, 0, // reserved
		1, 0, // icon
		1, 0, // one image
		0, 0, // 256 × 256
		0, 0, // palette and reserved
		1, 0, // color planes
		32, 0, // bits per pixel
	}
	if _, err = output.Write(header); err != nil {
		panic(err)
	}
	if err = binary.Write(output, binary.LittleEndian, uint32(len(pngData))); err != nil {
		panic(err)
	}
	if err = binary.Write(output, binary.LittleEndian, uint32(22)); err != nil {
		panic(err)
	}
	if _, err = output.Write(pngData); err != nil {
		panic(err)
	}
	fmt.Println("generated hfs-go.ico")
}

func downsample(source image.Image, width, height int) *image.NRGBA {
	bounds := source.Bounds()
	target := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		y0 := bounds.Min.Y + y*bounds.Dy()/height
		y1 := bounds.Min.Y + (y+1)*bounds.Dy()/height
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < width; x++ {
			x0 := bounds.Min.X + x*bounds.Dx()/width
			x1 := bounds.Min.X + (x+1)*bounds.Dx()/width
			if x1 <= x0 {
				x1 = x0 + 1
			}
			var red, green, blue, alpha uint64
			var count uint64
			for sampleY := y0; sampleY < y1; sampleY++ {
				for sampleX := x0; sampleX < x1; sampleX++ {
					r, g, b, a := source.At(sampleX, sampleY).RGBA()
					red += uint64(r)
					green += uint64(g)
					blue += uint64(b)
					alpha += uint64(a)
					count++
				}
			}
			target.SetNRGBA(x, y, color.NRGBA{
				R: uint8(red / count >> 8),
				G: uint8(green / count >> 8),
				B: uint8(blue / count >> 8),
				A: uint8(alpha / count >> 8),
			})
		}
	}
	return target
}
