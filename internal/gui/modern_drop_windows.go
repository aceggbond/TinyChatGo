//go:build windows

package gui

import (
	"errors"
	"fmt"
	"math"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	modernWMEraseBackground = 0x0014
	modernWMPaint           = 0x000F
	modernSWHide            = 0
	modernSWPNoActivate     = 0x0010
	modernSWPShowWindow     = 0x0040
	modernWSExTransparent   = 0x00000020
	modernWSExNoActivate    = 0x08000000
)

type modernPaintStruct struct {
	DC                 uintptr
	Erase              int32
	Paint              rect
	Restore, IncUpdate int32
	Reserved           [32]byte
}

var (
	modernDropClassOnce sync.Once
	modernDropClassErr  error
	modernDropWndProcCB = syscall.NewCallback(modernDropWndProc)
	modernBeginPaint    = user32.NewProc("BeginPaint")
	modernEndPaint      = user32.NewProc("EndPaint")
	modernSetWindowPos  = user32.NewProc("SetWindowPos")
)

func registerModernDropClass() error {
	modernDropClassOnce.Do(func() {
		module, _, _ := getModule.Call(0)
		cursor, _, _ := loadCursor.Call(0, 32512)
		className := utf16("HFSGoModernDropTarget")
		class := wndClass{
			Size:      uint32(unsafe.Sizeof(wndClass{})),
			WndProc:   modernDropWndProcCB,
			Instance:  module,
			Cursor:    cursor,
			ClassName: uintptr(unsafe.Pointer(&className[0])),
		}
		result, _, callErr := registerClass.Call(uintptr(unsafe.Pointer(&class)))
		if result == 0 && !errors.Is(callErr, syscall.Errno(1410)) {
			modernDropClassErr = fmt.Errorf("注册拖拽窗口失败：%w", callErr)
		}
	})
	return modernDropClassErr
}

func (m *modernController) createNativeDropWindow(parent HWND) error {
	if parent == 0 {
		return errors.New("主窗口尚未准备好")
	}
	if err := registerModernDropClass(); err != nil {
		return err
	}
	module, _, _ := getModule.Call(0)
	className := utf16("HFSGoModernDropTarget")
	windowName := utf16("")
	window, _, callErr := createWindow.Call(
		modernWSExTransparent|modernWSExNoActivate,
		uintptr(unsafe.Pointer(&className[0])),
		uintptr(unsafe.Pointer(&windowName[0])),
		wsChild,
		0, 0, 1, 1,
		uintptr(parent), 0, module, 0,
	)
	if window == 0 {
		return fmt.Errorf("创建拖拽窗口失败：%w", callErr)
	}
	m.mu.Lock()
	m.dropWindow = HWND(window)
	m.mu.Unlock()
	dragAccept.Call(window, 1)
	return nil
}

// setNativeDropZone positions the transparent Win32 drop target over the blue
// drop area rendered by WebView2. DOM coordinates are CSS pixels, while child
// window coordinates are physical pixels, so devicePixelRatio is applied.
func (m *modernController) setNativeDropZone(x, y, width, height, scale float64) {
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale <= 0 {
		scale = 1
	}
	m.mu.RLock()
	window := m.dropWindow
	view := m.view
	m.mu.RUnlock()
	if window == 0 || view == nil {
		return
	}
	left := int32(math.Round(x * scale))
	top := int32(math.Round(y * scale))
	w := int32(math.Round(width * scale))
	h := int32(math.Round(height * scale))
	view.Dispatch(func() {
		if w < 8 || h < 8 {
			showWindow.Call(uintptr(window), modernSWHide)
			return
		}
		modernSetWindowPos.Call(uintptr(window), 0, uintptr(left), uintptr(top), uintptr(w), uintptr(h), modernSWPNoActivate|modernSWPShowWindow)
	})
}

func modernDropWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmDropFiles:
		_, _, paths := consumeDroppedPaths(wParam)
		handleModernShareDrop(paths)
		return 0
	case modernWMEraseBackground:
		return 1
	case modernWMPaint:
		var paint modernPaintStruct
		modernBeginPaint.Call(hwnd, uintptr(unsafe.Pointer(&paint)))
		modernEndPaint.Call(hwnd, uintptr(unsafe.Pointer(&paint)))
		return 0
	}
	result, _, _ := defWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
}

func handleModernShareDrop(paths []string) {
	if modern == nil || len(paths) == 0 {
		return
	}
	page, selectedChat := modern.dropTarget()
	if page == "files" {
		if _, err := modern.addPaths(paths); err != nil {
			_, _ = fmt.Fprintf(modern.log, "%s 拖拽添加失败：%v\n", time.Now().Format("2006/01/02 15:04:05"), err)
			modern.view.Eval("toast('拖拽添加失败，请查看操作记录',true)")
			return
		}
		modern.view.Eval(fmt.Sprintf("toast('已通过拖拽添加 %d 项')", len(paths)))
		return
	}
	if page != "chat" || selectedChat == "" {
		return
	}
	for index, path := range paths {
		if err := modern.srv.SendChatAttachmentPath(selectedChat, path); err != nil {
			_, _ = fmt.Fprintf(modern.log, "%s 拖拽发送失败：%v\n", time.Now().Format("2006/01/02 15:04:05"), err)
			modern.view.Eval("toast('拖拽发送失败，请检查文件大小和聊天状态',true)")
			return
		}
		if index == len(paths)-1 {
			modernRefresh()
		}
	}
	modern.view.Eval(fmt.Sprintf("toast('已发送 %d 个附件')", len(paths)))
}
