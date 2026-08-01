package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
)

var iconSizes = []int{16, 20, 24, 32, 40, 48, 64, 128, 256}

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

	images := make([][]byte, 0, len(iconSizes))
	for _, size := range iconSizes {
		var encoded bytes.Buffer
		if err = png.Encode(&encoded, downsample(input, size, size)); err != nil {
			panic(err)
		}
		images = append(images, encoded.Bytes())
	}

	output, err := os.Create("tinychatgo.ico")
	if err != nil {
		panic(err)
	}
	defer output.Close()
	if _, err = output.Write([]byte{0, 0, 1, 0}); err != nil {
		panic(err)
	}
	if err = binary.Write(output, binary.LittleEndian, uint16(len(images))); err != nil {
		panic(err)
	}

	offset := uint32(6 + 16*len(images))
	for index, size := range iconSizes {
		dimension := byte(size)
		if size == 256 {
			dimension = 0
		}
		entry := []byte{
			dimension, dimension,
			0, 0, // palette and reserved
			1, 0, // color planes
			32, 0, // bits per pixel
		}
		if _, err = output.Write(entry); err != nil {
			panic(err)
		}
		if err = binary.Write(output, binary.LittleEndian, uint32(len(images[index]))); err != nil {
			panic(err)
		}
		if err = binary.Write(output, binary.LittleEndian, offset); err != nil {
			panic(err)
		}
		offset += uint32(len(images[index]))
	}
	for _, data := range images {
		if _, err = output.Write(data); err != nil {
			panic(err)
		}
	}
	fmt.Printf("generated tinychatgo.ico from logo.png with %d sizes\n", len(images))
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
