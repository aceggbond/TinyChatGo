//go:build windows && client

package gui

import (
	"strings"
	"syscall"
	"unsafe"
)

type HWND uintptr

type point struct {
	X int32
	Y int32
}

type notifyIconData struct {
	Size                 uint32
	Hwnd                 uintptr
	UID, Flags, Callback uint32
	Icon                 uintptr
	Tip                  [128]uint16
	State, StateMask     uint32
	Info                 [256]uint16
	Timeout              uint32
	InfoTitle            [64]uint16
	InfoFlags            uint32
	GUID                 [16]byte
	BalloonIcon          uintptr
}

const (
	modernIconResourceID = 1

	wmDestroy     = 2
	wmSize        = 5
	wmContextMenu = 0x007B
	wmSysCommand  = 0x0112
	wmSysKeyDown  = 0x0104
	wmClose       = 0x10
	wmTray        = 0x8002
	scClose       = 0xF060
	vkF4          = 0x73

	nimAdd             = 0
	nimModify          = 1
	nimDelete          = 2
	nimSetVersion      = 4
	nifMessage         = 0x01
	nifIcon            = 0x02
	nifTip             = 0x04
	nifInfo            = 0x10
	nifShowTip         = 0x80
	niifUser           = 0x04
	niifNoSound        = 0x10
	niifLargeIcon      = 0x20
	notifyIconVersion4 = 4
)

var (
	user32           = syscall.NewLazyDLL("user32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	shell32          = syscall.NewLazyDLL("shell32.dll")
	defWindowProc    = user32.NewProc("DefWindowProcW")
	destroyWindow    = user32.NewProc("DestroyWindow")
	showWindow       = user32.NewProc("ShowWindow")
	isWindowVisible  = user32.NewProc("IsWindowVisible")
	isIconic         = user32.NewProc("IsIconic")
	getForeground    = user32.NewProc("GetForegroundWindow")
	postMessage      = user32.NewProc("PostMessageW")
	messageBox       = user32.NewProc("MessageBoxW")
	messageBeep      = user32.NewProc("MessageBeep")
	setFocus         = user32.NewProc("SetFocus")
	setForeground    = user32.NewProc("SetForegroundWindow")
	registerMessage  = user32.NewProc("RegisterWindowMessageW")
	setWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	callWindowProc   = user32.NewProc("CallWindowProcW")
	getModule        = kernel32.NewProc("GetModuleHandleW")
	loadIcon         = user32.NewProc("LoadIconW")
	shellNotify      = shell32.NewProc("Shell_NotifyIconW")
	createPopupMenu  = user32.NewProc("CreatePopupMenu")
	appendMenu       = user32.NewProc("AppendMenuW")
	trackPopupMenu   = user32.NewProc("TrackPopupMenu")
	destroyMenu      = user32.NewProc("DestroyMenu")
	getCursorPos     = user32.NewProc("GetCursorPos")
)

var taskbarCreatedMessage uint32

func utf16(value string) []uint16 {
	return syscall.StringToUTF16(strings.ReplaceAll(value, "\x00", "�"))
}

func copyUTF16(destination []uint16, value string) {
	encoded := utf16(value)
	if len(encoded) > len(destination) {
		encoded = encoded[:len(destination)]
		if len(encoded) > 0 {
			encoded[len(encoded)-1] = 0
		}
	}
	copy(destination, encoded)
}

func registerNotifyIcon(data notifyIconData) bool {
	result, _, _ := shellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	if result == 0 {
		return false
	}
	version := data
	version.Timeout = notifyIconVersion4
	shellNotify.Call(nimSetVersion, uintptr(unsafe.Pointer(&version)))
	return true
}

func trayCallbackEvent(lParam uintptr) uintptr {
	return lParam & 0xffff
}

func trayNotificationData(base notifyIconData, title, body string, infoFlags uint32, balloonIcon uintptr) notifyIconData {
	base.Flags = nifInfo | nifIcon | nifMessage | nifTip | nifShowTip
	base.InfoFlags = infoFlags
	base.BalloonIcon = balloonIcon
	copyUTF16(base.InfoTitle[:], title)
	copyUTF16(base.Info[:], body)
	return base
}

func showNotifyIconNotification(data notifyIconData) bool {
	result, _, _ := shellNotify.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	return result != 0
}
