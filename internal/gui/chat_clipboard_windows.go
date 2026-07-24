//go:build windows

package gui

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

var (
	clipboardUser32            = syscall.NewLazyDLL("user32.dll")
	clipboardKernel32          = syscall.NewLazyDLL("kernel32.dll")
	isClipboardFormatAvailable = clipboardUser32.NewProc("IsClipboardFormatAvailable")
	openClipboard              = clipboardUser32.NewProc("OpenClipboard")
	closeClipboard             = clipboardUser32.NewProc("CloseClipboard")
	getClipboardData           = clipboardUser32.NewProc("GetClipboardData")
	globalLock                 = clipboardKernel32.NewProc("GlobalLock")
	globalUnlock               = clipboardKernel32.NewProc("GlobalUnlock")
	globalSize                 = clipboardKernel32.NewProc("GlobalSize")
	moveMemory                 = clipboardKernel32.NewProc("RtlMoveMemory")
)

const (
	clipboardFormatBitmap = 2
	clipboardFormatDIB    = 8
	clipboardFormatDIBV5  = 17
	bitmapCompressionRGB  = 0
	bitmapCompressionBits = 3
	maxChatImageInput     = 32 << 20
	maxChatImagePixels    = 64 * 1024 * 1024
	maxClipboardDIBBytes  = 256 << 20
)

func clipboardContainsImage() bool {
	for _, format := range []uintptr{clipboardFormatDIBV5, clipboardFormatDIB, clipboardFormatBitmap} {
		if available, _, _ := isClipboardFormatAvailable.Call(format); available != 0 {
			return true
		}
	}
	return false
}

func readClipboardChatImage() ([]byte, string, error) {
	source, err := readClipboardDIB()
	if err != nil {
		return nil, "", err
	}
	return encodeChatImage(source)
}

func readDroppedChatImage(imagePath string) ([]byte, string, error) {
	switch strings.ToLower(filepath.Ext(imagePath)) {
	case ".png", ".jpg", ".jpeg":
	default:
		return nil, "", errors.New("聊天拖拽仅支持 PNG 或 JPEG 图片")
	}
	info, err := os.Stat(imagePath)
	if err != nil || !info.Mode().IsRegular() {
		return nil, "", errors.New("无法读取拖入的图片")
	}
	if info.Size() > maxChatImageInput {
		return nil, "", errors.New("原始图片不能超过 32 MiB")
	}
	file, err := os.Open(imagePath)
	if err != nil {
		return nil, "", errors.New("无法读取拖入的图片")
	}
	defer file.Close()
	config, format, err := image.DecodeConfig(file)
	if err != nil || format != "png" && format != "jpeg" {
		return nil, "", errors.New("拖入的文件不是有效 PNG/JPEG 图片")
	}
	if err = validateChatImageDimensions(config.Width, config.Height); err != nil {
		return nil, "", err
	}
	if _, err = file.Seek(0, 0); err != nil {
		return nil, "", errors.New("无法重新读取拖入的图片")
	}
	source, format, err := image.Decode(file)
	if err != nil || format != "png" && format != "jpeg" {
		return nil, "", errors.New("无法解码拖入的图片")
	}
	return encodeChatImage(source)
}

func readClipboardDIB() (image.Image, error) {
	opened := false
	for attempt := 0; attempt < 10; attempt++ {
		if result, _, _ := openClipboard.Call(0); result != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return nil, errors.New("剪贴板正被其他程序占用，请重试")
	}
	defer closeClipboard.Call()

	format := uintptr(clipboardFormatDIBV5)
	handle, _, _ := getClipboardData.Call(format)
	if handle == 0 {
		format = clipboardFormatDIB
		handle, _, _ = getClipboardData.Call(format)
	}
	if handle == 0 {
		return nil, errors.New("剪贴板图片格式暂不支持，请复制为普通位图后重试")
	}
	size, _, _ := globalSize.Call(handle)
	if size < 40 || size > maxClipboardDIBBytes {
		return nil, errors.New("剪贴板图片数据大小无效")
	}
	pointer, _, _ := globalLock.Call(handle)
	if pointer == 0 {
		return nil, errors.New("无法读取剪贴板图片")
	}
	data := make([]byte, int(size))
	moveMemory.Call(uintptr(unsafe.Pointer(&data[0])), pointer, size)
	globalUnlock.Call(handle)
	return decodeClipboardDIB(data)
}

func decodeClipboardDIB(data []byte) (image.Image, error) {
	if len(data) < 40 {
		return nil, errors.New("剪贴板位图头无效")
	}
	headerSize := int(binary.LittleEndian.Uint32(data[0:4]))
	width := int(int32(binary.LittleEndian.Uint32(data[4:8])))
	signedHeight := int32(binary.LittleEndian.Uint32(data[8:12]))
	height := int(signedHeight)
	if height < 0 {
		height = -height
	}
	if headerSize < 40 || headerSize > len(data) || width <= 0 || height <= 0 {
		return nil, errors.New("剪贴板位图尺寸无效")
	}
	if err := validateChatImageDimensions(width, height); err != nil {
		return nil, err
	}
	if binary.LittleEndian.Uint16(data[12:14]) != 1 {
		return nil, errors.New("剪贴板位图平面数无效")
	}
	bitCount := int(binary.LittleEndian.Uint16(data[14:16]))
	compression := binary.LittleEndian.Uint32(data[16:20])
	if bitCount != 24 && bitCount != 32 {
		return nil, fmt.Errorf("暂不支持 %d 位剪贴板图片", bitCount)
	}
	if compression != bitmapCompressionRGB && compression != bitmapCompressionBits {
		return nil, errors.New("暂不支持压缩的剪贴板位图")
	}
	pixelOffset := headerSize
	var redMask, greenMask, blueMask, alphaMask uint32
	if compression == bitmapCompressionBits {
		if headerSize >= 56 {
			redMask = binary.LittleEndian.Uint32(data[40:44])
			greenMask = binary.LittleEndian.Uint32(data[44:48])
			blueMask = binary.LittleEndian.Uint32(data[48:52])
			alphaMask = binary.LittleEndian.Uint32(data[52:56])
		} else {
			maskBytes := 12
			if headerSize+maskBytes > len(data) {
				return nil, errors.New("剪贴板位图颜色掩码无效")
			}
			redMask = binary.LittleEndian.Uint32(data[headerSize : headerSize+4])
			greenMask = binary.LittleEndian.Uint32(data[headerSize+4 : headerSize+8])
			blueMask = binary.LittleEndian.Uint32(data[headerSize+8 : headerSize+12])
			pixelOffset += maskBytes
		}
		if redMask == 0 || greenMask == 0 || blueMask == 0 {
			return nil, errors.New("剪贴板位图颜色掩码无效")
		}
	}
	rowStride := ((width*bitCount + 31) / 32) * 4
	required := int64(pixelOffset) + int64(rowStride)*int64(height)
	if required > int64(len(data)) {
		return nil, errors.New("剪贴板位图像素数据不完整")
	}
	result := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sourceY := y
		if signedHeight > 0 {
			sourceY = height - 1 - y
		}
		row := data[pixelOffset+sourceY*rowStride:]
		for x := 0; x < width; x++ {
			index := result.PixOffset(x, y)
			if bitCount == 24 {
				source := x * 3
				result.Pix[index] = row[source+2]
				result.Pix[index+1] = row[source+1]
				result.Pix[index+2] = row[source]
				result.Pix[index+3] = 0xff
				continue
			}
			pixel := binary.LittleEndian.Uint32(row[x*4 : x*4+4])
			if compression == bitmapCompressionBits {
				result.Pix[index] = maskedChannel(pixel, redMask)
				result.Pix[index+1] = maskedChannel(pixel, greenMask)
				result.Pix[index+2] = maskedChannel(pixel, blueMask)
				if alphaMask != 0 {
					result.Pix[index+3] = maskedChannel(pixel, alphaMask)
				} else {
					result.Pix[index+3] = 0xff
				}
			} else {
				result.Pix[index] = byte(pixel >> 16)
				result.Pix[index+1] = byte(pixel >> 8)
				result.Pix[index+2] = byte(pixel)
				result.Pix[index+3] = 0xff
			}
		}
	}
	return result, nil
}

func maskedChannel(pixel, mask uint32) uint8 {
	if mask == 0 {
		return 0
	}
	shift := bits.TrailingZeros32(mask)
	value := (pixel & mask) >> shift
	maximum := mask >> shift
	return uint8((uint64(value)*255 + uint64(maximum)/2) / uint64(maximum))
}

func validateChatImageDimensions(width, height int) error {
	if width <= 0 || height <= 0 || width > 16384 || height > 16384 ||
		int64(width)*int64(height) > maxChatImagePixels {
		return errors.New("图片尺寸过大，最多支持 6400 万像素")
	}
	return nil
}

func encodeChatImage(source image.Image) ([]byte, string, error) {
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if err := validateChatImageDimensions(width, height); err != nil {
		return nil, "", err
	}
	targetWidth, targetHeight := fitImageWithin(width, height, 1280)
	for {
		resized := resizeChatImage(source, targetWidth, targetHeight)
		for _, quality := range []int{82, 72, 62, 52, 42} {
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, resized, &jpeg.Options{Quality: quality}); err != nil {
				return nil, "", errors.New("图片压缩失败")
			}
			if encoded.Len() <= 512<<10 {
				return encoded.Bytes(), "image/jpeg", nil
			}
		}
		if targetWidth <= 320 && targetHeight <= 320 {
			break
		}
		targetWidth = maxInt(1, int(float64(targetWidth)*0.8))
		targetHeight = maxInt(1, int(float64(targetHeight)*0.8))
	}
	return nil, "", errors.New("图片压缩后仍超过 512 KB")
}

func fitImageWithin(width, height, maxSide int) (int, int) {
	if width <= maxSide && height <= maxSide {
		return width, height
	}
	scale := math.Min(float64(maxSide)/float64(width), float64(maxSide)/float64(height))
	return maxInt(1, int(float64(width)*scale+0.5)), maxInt(1, int(float64(height)*scale+0.5))
}

func resizeChatImage(source image.Image, width, height int) *image.RGBA {
	bounds := source.Bounds()
	sourceWidth, sourceHeight := bounds.Dx(), bounds.Dy()
	target := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		sourceY := (float64(y)+0.5)*float64(sourceHeight)/float64(height) - 0.5
		y0, y1, vertical := interpolationPoints(sourceY, sourceHeight)
		for x := 0; x < width; x++ {
			sourceX := (float64(x)+0.5)*float64(sourceWidth)/float64(width) - 0.5
			x0, x1, horizontal := interpolationPoints(sourceX, sourceWidth)
			r00, g00, b00, a00 := source.At(bounds.Min.X+x0, bounds.Min.Y+y0).RGBA()
			r10, g10, b10, a10 := source.At(bounds.Min.X+x1, bounds.Min.Y+y0).RGBA()
			r01, g01, b01, a01 := source.At(bounds.Min.X+x0, bounds.Min.Y+y1).RGBA()
			r11, g11, b11, a11 := source.At(bounds.Min.X+x1, bounds.Min.Y+y1).RGBA()
			red := bilinearChannel(r00, r10, r01, r11, horizontal, vertical)
			green := bilinearChannel(g00, g10, g01, g11, horizontal, vertical)
			blue := bilinearChannel(b00, b10, b01, b11, horizontal, vertical)
			alpha := bilinearChannel(a00, a10, a01, a11, horizontal, vertical)
			index := target.PixOffset(x, y)
			target.Pix[index] = uint8(minUint32(0xffff, red+0xffff-alpha) >> 8)
			target.Pix[index+1] = uint8(minUint32(0xffff, green+0xffff-alpha) >> 8)
			target.Pix[index+2] = uint8(minUint32(0xffff, blue+0xffff-alpha) >> 8)
			target.Pix[index+3] = 0xff
		}
	}
	return target
}

func interpolationPoints(position float64, length int) (int, int, float64) {
	if position <= 0 {
		return 0, 0, 0
	}
	if position >= float64(length-1) {
		last := length - 1
		return last, last, 0
	}
	left := int(math.Floor(position))
	return left, left + 1, position - float64(left)
}

func bilinearChannel(topLeft, topRight, bottomLeft, bottomRight uint32, horizontal, vertical float64) uint32 {
	top := float64(topLeft)*(1-horizontal) + float64(topRight)*horizontal
	bottom := float64(bottomLeft)*(1-horizontal) + float64(bottomRight)*horizontal
	return uint32(top*(1-vertical) + bottom*vertical + 0.5)
}

func minUint32(left, right uint32) uint32 {
	if left < right {
		return left
	}
	return right
}
