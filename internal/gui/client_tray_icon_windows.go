//go:build windows

package gui

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"runtime"
	"syscall"
	"unsafe"
)

const clientTrayAlertIconSize = 32

var (
	clientIconUser32               = syscall.NewLazyDLL("user32.dll")
	clientCreateIconFromResourceEx = clientIconUser32.NewProc("CreateIconFromResourceEx")
	clientDestroyIcon              = clientIconUser32.NewProc("DestroyIcon")
)

func createClientTrayAlertIcon(logo []byte) uintptr {
	source, err := png.Decode(bytes.NewReader(logo))
	if err != nil {
		return 0
	}
	iconBits := buildClientTrayAlertIconDIB(source, clientTrayAlertIconSize)
	if len(iconBits) == 0 {
		return 0
	}
	icon, _, _ := clientCreateIconFromResourceEx.Call(
		uintptr(unsafe.Pointer(&iconBits[0])),
		uintptr(len(iconBits)),
		1,
		0x00030000,
		clientTrayAlertIconSize,
		clientTrayAlertIconSize,
		0,
	)
	runtime.KeepAlive(iconBits)
	return icon
}

func buildClientTrayAlertIconDIB(source image.Image, size int) []byte {
	if source == nil || size <= 0 {
		return nil
	}
	bounds := source.Bounds()
	if bounds.Empty() {
		return nil
	}

	const headerSize = 40
	colorStride := size * 4
	colorBytes := colorStride * size
	maskStride := ((size + 31) / 32) * 4
	maskOffset := headerSize + colorBytes
	result := make([]byte, maskOffset+maskStride*size)

	binary.LittleEndian.PutUint32(result[0:4], headerSize)
	binary.LittleEndian.PutUint32(result[4:8], uint32(size))
	binary.LittleEndian.PutUint32(result[8:12], uint32(size*2))
	binary.LittleEndian.PutUint16(result[12:14], 1)
	binary.LittleEndian.PutUint16(result[14:16], 32)
	binary.LittleEndian.PutUint32(result[20:24], uint32(colorBytes))

	for y := 0; y < size; y++ {
		sourceY := bounds.Min.Y + y*bounds.Dy()/size
		targetRow := size - 1 - y
		for x := 0; x < size; x++ {
			sourceX := bounds.Min.X + x*bounds.Dx()/size
			red16, green16, blue16, alpha16 := source.At(sourceX, sourceY).RGBA()
			alpha := uint8(alpha16 >> 8)
			if alpha == 0 {
				setClientTrayMaskBit(result[maskOffset:], maskStride, targetRow, x)
				continue
			}

			red, green, blue := clientTrayAlertColor(
				uint8(red16>>8),
				uint8(green16>>8),
				uint8(blue16>>8),
			)
			pixel := headerSize + targetRow*colorStride + x*4
			result[pixel] = premultiplyClientTrayColor(blue, alpha)
			result[pixel+1] = premultiplyClientTrayColor(green, alpha)
			result[pixel+2] = premultiplyClientTrayColor(red, alpha)
			result[pixel+3] = alpha
		}
	}
	return result
}

func clientTrayAlertColor(red, green, blue uint8) (uint8, uint8, uint8) {
	luminance := (uint32(red)*299 + uint32(green)*587 + uint32(blue)*114) / 1000
	return uint8(220 + luminance*35/255), uint8(24 + luminance*42/255), uint8(36 + luminance*34/255)
}

func premultiplyClientTrayColor(color, alpha uint8) uint8 {
	return uint8((uint16(color)*uint16(alpha) + 127) / 255)
}

func setClientTrayMaskBit(mask []byte, stride, row, x int) {
	offset := row*stride + x/8
	if offset >= 0 && offset < len(mask) {
		mask[offset] |= 0x80 >> uint(x%8)
	}
}
