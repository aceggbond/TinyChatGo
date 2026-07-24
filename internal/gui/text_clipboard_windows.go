//go:build windows

package gui

import (
	"errors"
	"runtime"
	"syscall"
	"time"
	"unsafe"
)

var (
	emptyClipboard   = clipboardUser32.NewProc("EmptyClipboard")
	setClipboardData = clipboardUser32.NewProc("SetClipboardData")
	globalAlloc      = clipboardKernel32.NewProc("GlobalAlloc")
	globalFree       = clipboardKernel32.NewProc("GlobalFree")
)

const (
	clipboardFormatUnicodeText = 13
	globalMemoryMoveable       = 0x0002
	globalMemoryZeroInit       = 0x0040
)

func encodeClipboardText(value string) ([]uint16, error) {
	encoded, err := syscall.UTF16FromString(value)
	if err != nil {
		return nil, errors.New("复制内容不能包含空字符")
	}
	return encoded, nil
}

func setClipboardText(owner HWND, value string) error {
	if owner == 0 {
		return errors.New("剪贴板窗口尚未准备好")
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	encoded, err := encodeClipboardText(value)
	if err != nil {
		return err
	}
	size := uintptr(len(encoded) * 2)
	handle, _, _ := globalAlloc.Call(globalMemoryMoveable|globalMemoryZeroInit, size)
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
	moveMemory.Call(pointer, uintptr(unsafe.Pointer(&encoded[0])), size)
	globalUnlock.Call(handle)
	runtime.KeepAlive(encoded)

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
	if result, _, _ := setClipboardData.Call(clipboardFormatUnicodeText, handle); result == 0 {
		return errors.New("无法设置剪贴板内容")
	}
	ownedByClipboard = true
	return nil
}
