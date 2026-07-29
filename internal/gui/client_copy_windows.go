//go:build windows

package gui

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	_ "image/jpeg"
	_ "image/png"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

const maxClientClipboardImageBytes = 100 << 20

func decodeClipboardDataURL(value string) ([]byte, error) {
	comma := strings.IndexByte(value, ',')
	if comma < 0 || !strings.HasPrefix(value[:comma], "data:image/") ||
		!strings.Contains(value[:comma], ";base64") {
		return nil, errors.New("图片数据格式无效")
	}
	data, err := base64.StdEncoding.DecodeString(value[comma+1:])
	if err != nil || len(data) == 0 || len(data) > maxClientClipboardImageBytes {
		return nil, errors.New("图片数据无效或超过 100 MiB")
	}
	return data, nil
}

func setClipboardImage(owner HWND, encoded []byte) error {
	source, _, err := image.Decode(bytes.NewReader(encoded))
	if err != nil {
		return errors.New("无法解码要复制的图片")
	}
	data, err := encodeClipboardDIBV5(source)
	if err != nil {
		return err
	}
	return setClipboardBytes(owner, clipboardFormatDIBV5, data)
}

func encodeClipboardDIBV5(source image.Image) ([]byte, error) {
	bounds := source.Bounds()
	if err := validateChatImageDimensions(bounds.Dx(), bounds.Dy()); err != nil {
		return nil, err
	}
	const headerSize = 124
	rowSize := bounds.Dx() * 4
	data := make([]byte, headerSize+rowSize*bounds.Dy())
	binary.LittleEndian.PutUint32(data[0:4], headerSize)
	binary.LittleEndian.PutUint32(data[4:8], uint32(bounds.Dx()))
	binary.LittleEndian.PutUint32(data[8:12], uint32(int32(-bounds.Dy())))
	binary.LittleEndian.PutUint16(data[12:14], 1)
	binary.LittleEndian.PutUint16(data[14:16], 32)
	binary.LittleEndian.PutUint32(data[16:20], bitmapCompressionBits)
	binary.LittleEndian.PutUint32(data[20:24], uint32(rowSize*bounds.Dy()))
	binary.LittleEndian.PutUint32(data[40:44], 0x00ff0000)
	binary.LittleEndian.PutUint32(data[44:48], 0x0000ff00)
	binary.LittleEndian.PutUint32(data[48:52], 0x000000ff)
	binary.LittleEndian.PutUint32(data[52:56], 0xff000000)
	binary.LittleEndian.PutUint32(data[56:60], 0x73524742)
	for y := 0; y < bounds.Dy(); y++ {
		for x := 0; x < bounds.Dx(); x++ {
			pixel := color.NRGBAModel.Convert(source.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
			offset := headerSize + y*rowSize + x*4
			data[offset], data[offset+1], data[offset+2], data[offset+3] =
				pixel.B, pixel.G, pixel.R, pixel.A
		}
	}
	return data, nil
}

func setClipboardFiles(owner HWND, paths []string) error {
	if len(paths) == 0 {
		return errors.New("没有可复制的文件")
	}
	const dropFilesHeaderSize = 20
	utf16Paths := make([]uint16, 0, 256)
	for _, path := range paths {
		encoded, err := syscall.UTF16FromString(path)
		if err != nil {
			return errors.New("文件路径包含无效字符")
		}
		utf16Paths = append(utf16Paths, encoded...)
	}
	utf16Paths = append(utf16Paths, 0)
	data := make([]byte, dropFilesHeaderSize+len(utf16Paths)*2)
	binary.LittleEndian.PutUint32(data[0:4], dropFilesHeaderSize)
	binary.LittleEndian.PutUint32(data[16:20], 1)
	for index, value := range utf16Paths {
		binary.LittleEndian.PutUint16(data[dropFilesHeaderSize+index*2:], value)
	}
	const clipboardFormatFileDrop = 15
	return setClipboardBytes(owner, clipboardFormatFileDrop, data)
}

func setClipboardBytes(owner HWND, format uintptr, data []byte) error {
	if owner == 0 || len(data) == 0 {
		return errors.New("剪贴板数据尚未准备好")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	handle, _, _ := globalAlloc.Call(globalMemoryMoveable|globalMemoryZeroInit, uintptr(len(data)))
	if handle == 0 {
		return errors.New("无法分配剪贴板内存")
	}
	ownedByClipboard := false
	defer func() {
		if !ownedByClipboard {
			globalFree.Call(handle)
		}
	}()
	pointer, _, _ := globalLock.Call(handle)
	if pointer == 0 {
		return errors.New("无法写入剪贴板内存")
	}
	moveMemory.Call(pointer, uintptr(unsafe.Pointer(&data[0])), uintptr(len(data)))
	globalUnlock.Call(handle)
	runtime.KeepAlive(data)

	opened := false
	for attempt := 0; attempt < 10; attempt++ {
		if result, _, _ := openClipboard.Call(uintptr(owner)); result != 0 {
			opened = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !opened {
		return errors.New("剪贴板正被其他程序占用，请重试")
	}
	defer closeClipboard.Call()
	if result, _, _ := emptyClipboard.Call(); result == 0 {
		return errors.New("无法清空剪贴板")
	}
	if result, _, _ := setClipboardData.Call(format, handle); result == 0 {
		return errors.New("无法设置剪贴板内容")
	}
	ownedByClipboard = true
	return nil
}
