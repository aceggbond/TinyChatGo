//go:build windows

package gui

import (
	"bytes"
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"hfsgo/internal/server"
)

type HWND uintptr
type appState struct {
	hwnd, list, logs, status, port                              HWND
	title, subtitle, shareTitle, shareHint, logTitle, portLabel HWND
	password, upload, download, manage                          HWND
	start, addFile, addDir, remove, rename, open                HWND
	copyURL, clearLog, splitter                                 HWND
	srv                                                         *server.Server
	log                                                         *safeBuffer
	running                                                     bool
	transitioning                                               bool
	stateMu                                                     sync.Mutex
	stateErr                                                    string
	stateRunning                                                bool
	clientW, clientH, splitY                                    int
	config                                                      string
}
type safeBuffer struct {
	mu   sync.Mutex
	b    bytes.Buffer
	hwnd HWND
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	n, e := b.b.Write(p)
	if b.b.Len() > 100000 {
		x := append([]byte(nil), b.b.Bytes()[b.b.Len()-70000:]...)
		b.b.Reset()
		b.b.Write(x)
	}
	b.mu.Unlock()
	if b.hwnd != 0 {
		postMessage.Call(uintptr(b.hwnd), wmLog, 0, 0)
	}
	return n, e
}
func (b *safeBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }
func (b *safeBuffer) Clear()         { b.mu.Lock(); b.b.Reset(); b.mu.Unlock() }

var app *appState
var (
	user32           = syscall.NewLazyDLL("user32.dll")
	kernel32         = syscall.NewLazyDLL("kernel32.dll")
	shell32          = syscall.NewLazyDLL("shell32.dll")
	comdlg32         = syscall.NewLazyDLL("comdlg32.dll")
	comctl32         = syscall.NewLazyDLL("comctl32.dll")
	registerClass    = user32.NewProc("RegisterClassExW")
	createWindow     = user32.NewProc("CreateWindowExW")
	defWindowProc    = user32.NewProc("DefWindowProcW")
	destroyWindow    = user32.NewProc("DestroyWindow")
	showWindow       = user32.NewProc("ShowWindow")
	updateWindow     = user32.NewProc("UpdateWindow")
	getMessage       = user32.NewProc("GetMessageW")
	translateMessage = user32.NewProc("TranslateMessage")
	dispatchMessage  = user32.NewProc("DispatchMessageW")
	postQuit         = user32.NewProc("PostQuitMessage")
	sendMessage      = user32.NewProc("SendMessageW")
	postMessage      = user32.NewProc("PostMessageW")
	setWindowText    = user32.NewProc("SetWindowTextW")
	getWindowText    = user32.NewProc("GetWindowTextW")
	getWindowTextLen = user32.NewProc("GetWindowTextLengthW")
	moveWindow       = user32.NewProc("MoveWindow")
	messageBox       = user32.NewProc("MessageBoxW")
	setFocus         = user32.NewProc("SetFocus")
	setForeground    = user32.NewProc("SetForegroundWindow")
	dragAccept       = shell32.NewProc("DragAcceptFiles")
	dragQuery        = shell32.NewProc("DragQueryFileW")
	dragFinish       = shell32.NewProc("DragFinish")
	browseFolder     = shell32.NewProc("SHBrowseForFolderW")
	pathFromID       = shell32.NewProc("SHGetPathFromIDListW")
	coTaskFree       = syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree")
	getOpenFile      = comdlg32.NewProc("GetOpenFileNameW")
	getModule        = kernel32.NewProc("GetModuleHandleW")
	loadCursor       = user32.NewProc("LoadCursorW")
	loadIcon         = user32.NewProc("LoadIconW")
	shellNotify      = shell32.NewProc("Shell_NotifyIconW")
	createFont       = syscall.NewLazyDLL("gdi32.dll").NewProc("CreateFontW")
	createBrush      = syscall.NewLazyDLL("gdi32.dll").NewProc("CreateSolidBrush")
	setTextColor     = syscall.NewLazyDLL("gdi32.dll").NewProc("SetTextColor")
	setBkColor       = syscall.NewLazyDLL("gdi32.dll").NewProc("SetBkColor")
	setBkMode        = syscall.NewLazyDLL("gdi32.dll").NewProc("SetBkMode")
	createPopupMenu  = user32.NewProc("CreatePopupMenu")
	appendMenu       = user32.NewProc("AppendMenuW")
	trackPopupMenu   = user32.NewProc("TrackPopupMenu")
	destroyMenu      = user32.NewProc("DestroyMenu")
	getCursorPos     = user32.NewProc("GetCursorPos")
	screenToClient   = user32.NewProc("ScreenToClient")
	enableWindow     = user32.NewProc("EnableWindow")
	initCommon       = comctl32.NewProc("InitCommonControlsEx")
	setCapture       = user32.NewProc("SetCapture")
	releaseCapture   = user32.NewProc("ReleaseCapture")
)

var fontUI, fontTitle, fontSmall, fontMono, brushBackground, brushWhite, brushSplitter uintptr
var splitterDragging bool

const (
	wmCreate           = 1
	wmDestroy          = 2
	wmSize             = 5
	wmCommand          = 0x111
	wmCtlColorEdit     = 0x0133
	wmCtlColorList     = 0x0134
	wmCtlColorStatic   = 0x0138
	wmContextMenu      = 0x007B
	wmDropFiles        = 0x233
	wmClose            = 0x10
	wmLog              = 0x8001
	wmTray             = 0x8002
	wmServerState      = 0x8003
	wmLButtonDown      = 0x0201
	wmLButtonUp        = 0x0202
	wmMouseMove        = 0x0200
	wsOverlappedWindow = 0x00CF0000
	wsVisible          = 0x10000000
	wsChild            = 0x40000000
	wsBorder           = 0x00800000
	wsVScroll          = 0x00200000
	wsTabStop          = 0x00010000
	esMultiLine        = 0x0004
	esReadOnly         = 0x0800
	esAutoVScroll      = 0x0040
	lbsNotify          = 1
	bsPushButton       = 0
	bsFlat             = 0x8000
	bsAutoCheckBox     = 3
	esPassword         = 0x20
	swShow             = 5
	lbAddString        = 0x180
	lbResetContent     = 0x184
	lbGetCurSel        = 0x188
	lbErr              = -1
	emSetSel           = 0xB1
	emScrollCaret      = 0xB7
	wmSetFont          = 0x30
	lbSetItemHeight    = 0x1A0
	lbSetCurSel        = 0x0186
	lbItemFromPoint    = 0x01A9
	bnClicked          = 0
	lbnDblClk          = 2
	lvmDeleteAll       = 0x1009
	lvmInsertItem      = 0x104D
	lvmSetItem         = 0x104C
	lvmInsertColumn    = 0x1061
	lvmSetExtended     = 0x1036
	bmGetCheck         = 0x00F0
	bmSetCheck         = 0x00F1
)
const (
	idStart      = 1001
	idAddFile    = 1002
	idAddDir     = 1003
	idRemove     = 1004
	idRename     = 1005
	idOpen       = 1006
	idPassword   = 1007
	idUpload     = 1008
	idDownload   = 1009
	idManage     = 1010
	idCopyURL    = 1011
	idClearLog   = 1012
	idMenuOpen   = 1101
	idMenuCopy   = 1102
	idMenuRename = 1103
	idMenuRemove = 1104
	idTrayShow   = 1201
	idTrayOpen   = 1202
	idTrayExit   = 1203
)

type point struct{ X, Y int32 }
type msg struct {
	Hwnd           HWND
	Message        uint32
	WParam, LParam uintptr
	Time           uint32
	Pt             point
	Private        uint32
}
type wndClass struct {
	Size                                                            uint32
	Style                                                           uint32
	WndProc                                                         uintptr
	ClsExtra, WndExtra                                              int32
	Instance, Icon, Cursor, Background, MenuName, ClassName, IconSm uintptr
}
type openFileName struct {
	Size                                   uint32
	Owner, Instance                        uintptr
	Filter, CustomFilter                   uintptr
	MaxCustomFilter, FilterIndex           uint32
	File                                   uint64
	MaxFile                                uint32
	FileTitle                              uint64
	MaxFileTitle                           uint32
	InitialDir, Title                      uintptr
	Flags, FileOffset, FileExtension       uint32
	DefExt, CustomData, Hook, TemplateName uintptr
	Reserved                               uintptr
	Reserved2, FlagsEx                     uint32
}
type browseInfo struct {
	Owner       uintptr
	Root        uintptr
	DisplayName uintptr
	Title       uintptr
	Flags       uint32
	Callback    uintptr
	LParam      uintptr
	Image       int32
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
type lvColumn struct {
	Mask                                                               uint32
	Fmt, Width                                                         int32
	Text                                                               uintptr
	TextMax, SubItem, Image, Order, MinWidth, DefaultWidth, IdealWidth int32
}
type lvItem struct {
	Mask                     uint32
	Item, SubItem            int32
	State, StateMask         uint32
	Text                     uintptr
	TextMax, Image           int32
	Param                    uintptr
	Indent, GroupID, Columns uint32
	ColumnPtr, FormatPtr     uintptr
	Group                    int32
}
type initCommonControls struct{ Size, Classes uint32 }

func Run() error {
	icc := initCommonControls{Size: uint32(unsafe.Sizeof(initCommonControls{})), Classes: 1}
	initCommon.Call(uintptr(unsafe.Pointer(&icc)))
	exe, _ := os.Executable()
	cfg := filepath.Join(filepath.Dir(exe), "hfsgo.json")
	buf := &safeBuffer{}
	app = &appState{log: buf, config: cfg}
	app.srv = server.New(buf)
	_ = app.srv.Load(cfg)
	inst, _, _ := getModule.Call(0)
	cur, _, _ := loadCursor.Call(0, 32512)
	resizeCursor, _, _ := loadCursor.Call(0, 32645)
	brushBackground, _, _ = createBrush.Call(rgb(245, 247, 250))
	brushWhite, _, _ = createBrush.Call(rgb(255, 255, 255))
	brushSplitter, _, _ = createBrush.Call(rgb(190, 200, 212))
	fontUI = makeFont("Segoe UI", 17, 400)
	fontTitle = makeFont("Segoe UI", 29, 600)
	fontSmall = makeFont("Segoe UI", 15, 400)
	fontMono = makeFont("Consolas", 15, 400)
	cls := utf16("HFSGoMain")
	splitClass := utf16("HFSGoSplitter")
	swc := wndClass{Size: uint32(unsafe.Sizeof(wndClass{})), WndProc: syscall.NewCallback(splitterProc), Instance: inst, Cursor: resizeCursor, Background: brushSplitter, ClassName: uintptr(unsafe.Pointer(&splitClass[0]))}
	if r, _, e := registerClass.Call(uintptr(unsafe.Pointer(&swc))); r == 0 {
		return e
	}
	wc := wndClass{Size: uint32(unsafe.Sizeof(wndClass{})), WndProc: syscall.NewCallback(wndProc), Instance: inst, Cursor: cur, Background: brushBackground, ClassName: uintptr(unsafe.Pointer(&cls[0]))}
	if r, _, e := registerClass.Call(uintptr(unsafe.Pointer(&wc))); r == 0 {
		return e
	}
	title := utf16("HFS Go - HTTP 文件服务器")
	h, _, e := createWindow.Call(0, uintptr(unsafe.Pointer(&cls[0])), uintptr(unsafe.Pointer(&title[0])), wsOverlappedWindow|wsVisible, 100, 60, 1020, 760, 0, 0, inst, 0)
	if h == 0 {
		return e
	}
	app.hwnd = HWND(h)
	buf.hwnd = HWND(h)
	dragAccept.Call(h, 1)
	refreshList()
	showWindow.Call(h, swShow)
	updateWindow.Call(h)
	var m msg
	for {
		r, _, _ := getMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(r) <= 0 {
			break
		}
		translateMessage.Call(uintptr(unsafe.Pointer(&m)))
		dispatchMessage.Call(uintptr(unsafe.Pointer(&m)))
	}
	return nil
}

func wndProc(hwnd uintptr, m uint32, w, l uintptr) uintptr {
	switch m {
	case wmCreate:
		createControls(HWND(hwnd))
		return 0
	case wmSize:
		if w != 1 {
			layout(int(int16(l&0xffff)), int(int16((l>>16)&0xffff)))
		}
		return 0
	case wmDropFiles:
		addDropped(w)
		return 0
	case wmCommand:
		if uint16(w) == 0 && uint16(w>>16) == lbnDblClk {
			confirmRemoveSelected()
			return 0
		}
		if uint16(w) == idPassword || uint16(w) == idUpload || uint16(w) == idDownload || uint16(w) == idManage {
			updateAccess()
		}
		if uint16(w>>16) == bnClicked {
			switch uint16(w) {
			case idStart:
				toggle()
			case idAddFile:
				addFileDialog()
			case idAddDir:
				addFolderDialog()
			case idRemove:
				removeSelected()
			case idRename:
				renameSelected()
			case idOpen:
				openBrowser()
			case idCopyURL:
				copyAddress()
			case idClearLog:
				app.log.Clear()
				refreshLog()
			case idMenuOpen:
				openSelected()
			case idMenuCopy:
				copySelectedAddress()
			case idMenuRename:
				renameSelected()
			case idMenuRemove:
				confirmRemoveSelected()
			}
		}
		return 0
	case wmContextMenu:
		if HWND(w) == app.list {
			showShareMenu(l)
			return 0
		}
	case wmLog:
		refreshLog()
		return 0
	case wmServerState:
		finishTransition()
		return 0
	case wmTray:
		if l == 0x203 {
			restoreFromTray()
		} else if l == 0x205 {
			showTrayMenu()
		}
		return 0
	case wmCtlColorStatic:
		setBkMode.Call(w, 1)
		if HWND(l) == app.status {
			setTextColor.Call(w, rgb(25, 103, 210))
		} else if HWND(l) == app.subtitle || HWND(l) == app.shareHint {
			setTextColor.Call(w, rgb(101, 113, 133))
		} else {
			setTextColor.Call(w, rgb(31, 42, 58))
		}
		return brushBackground
	case wmCtlColorEdit, wmCtlColorList:
		setTextColor.Call(w, rgb(31, 42, 58))
		setBkColor.Call(w, rgb(255, 255, 255))
		return brushWhite
	case wmClose:
		minimizeToTray()
		return 0
	case wmDestroy:
		postQuit.Call(0)
		return 0
	}
	r, _, _ := defWindowProc.Call(hwnd, uintptr(m), w, l)
	return r
}

func splitterProc(hwnd uintptr, m uint32, w, l uintptr) uintptr {
	switch m {
	case wmLButtonDown:
		splitterDragging = true
		setCapture.Call(hwnd)
		return 0
	case wmMouseMove:
		if splitterDragging && app != nil {
			var p point
			getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
			screenToClient.Call(uintptr(app.hwnd), uintptr(unsafe.Pointer(&p)))
			y := int(p.Y)
			if y < 420 {
				y = 420
			}
			if y > app.clientH-135 {
				y = app.clientH - 135
			}
			app.splitY = y
			layout(app.clientW, app.clientH)
		}
		return 0
	case wmLButtonUp:
		splitterDragging = false
		releaseCapture.Call()
		return 0
	}
	r, _, _ := defWindowProc.Call(hwnd, uintptr(m), w, l)
	return r
}

func createControls(parent HWND) {
	app.title = control(parent, "STATIC", "HFS Go", wsVisible|wsChild, 30, 20, 280, 38, 0)
	app.subtitle = control(parent, "STATIC", "轻量、安全的局域网文件分享", wsVisible|wsChild, 30, 60, 360, 24, 0)
	app.status = control(parent, "STATIC", "●  服务未启动", wsVisible|wsChild, 30, 94, 400, 25, 0)
	app.password = control(parent, "EDIT", "", wsVisible|wsChild|wsBorder|wsTabStop|esPassword, 514, 91, 145, 28, idPassword)
	control(parent, "STATIC", "访问密码", wsVisible|wsChild, 445, 94, 65, 22, 0)
	app.upload = control(parent, "BUTTON", "允许上传", wsVisible|wsChild|wsTabStop|bsAutoCheckBox, 680, 92, 100, 26, idUpload)
	app.download = control(parent, "BUTTON", "允许下载", wsVisible|wsChild|wsTabStop|bsAutoCheckBox, 790, 92, 100, 26, idDownload)
	app.manage = control(parent, "BUTTON", "网页管理", wsVisible|wsChild|wsTabStop|bsAutoCheckBox, 895, 92, 95, 26, idManage)
	sendMessage.Call(uintptr(app.download), bmSetCheck, 1, 0)
	app.portLabel = control(parent, "STATIC", "端口", wsVisible|wsChild, 678, 35, 42, 22, 0)
	app.port = control(parent, "EDIT", "1122", wsVisible|wsChild|wsBorder|wsTabStop, 725, 29, 78, 31, 0)
	app.start = control(parent, "BUTTON", "启动服务", wsVisible|wsChild|wsTabStop|bsPushButton|bsFlat, 814, 27, 155, 36, idStart)
	app.shareTitle = control(parent, "STATIC", "分享内容", wsVisible|wsChild, 30, 139, 120, 27, 0)
	app.shareHint = control(parent, "STATIC", "将文件或文件夹拖到下方区域", wsVisible|wsChild, 132, 142, 280, 22, 0)
	app.list = control(parent, "LISTBOX", "", wsVisible|wsChild|wsBorder|wsVScroll|lbsNotify, 30, 175, 939, 280, 0)
	app.addFile = control(parent, "BUTTON", "+  添加文件", wsVisible|wsChild|wsTabStop|bsFlat, 30, 467, 116, 34, idAddFile)
	app.addDir = control(parent, "BUTTON", "+  添加文件夹", wsVisible|wsChild|wsTabStop|bsFlat, 154, 467, 130, 34, idAddDir)
	app.rename = control(parent, "BUTTON", "重命名", wsVisible|wsChild|wsTabStop|bsFlat, 292, 467, 100, 34, idRename)
	app.remove = control(parent, "BUTTON", "移除", wsVisible|wsChild|wsTabStop|bsFlat, 400, 467, 90, 34, idRemove)
	app.copyURL = control(parent, "BUTTON", "复制地址", wsVisible|wsChild|wsTabStop|bsFlat, 700, 467, 100, 34, idCopyURL)
	app.open = control(parent, "BUTTON", "在浏览器中打开", wsVisible|wsChild|wsTabStop|bsFlat, 810, 467, 159, 34, idOpen)
	app.logTitle = control(parent, "STATIC", "访问日志", wsVisible|wsChild, 30, 521, 120, 27, 0)
	app.clearLog = control(parent, "BUTTON", "清空日志", wsVisible|wsChild|wsTabStop|bsFlat, 869, 516, 100, 32, idClearLog)
	app.splitter = control(parent, "HFSGoSplitter", "", wsVisible|wsChild, 30, 548, 939, 6, 0)
	app.logs = control(parent, "SysListView32", "", wsVisible|wsChild|wsBorder|wsVScroll|1|4, 30, 556, 939, 150, 0)
	for _, h := range []HWND{app.subtitle, app.status, app.portLabel, app.port, app.password, app.upload, app.download, app.manage, app.start, app.shareTitle, app.shareHint, app.list, app.addFile, app.addDir, app.remove, app.rename, app.copyURL, app.open, app.logTitle, app.clearLog} {
		applyFont(h, fontUI)
	}
	applyFont(app.title, fontTitle)
	applyFont(app.subtitle, fontSmall)
	applyFont(app.shareHint, fontSmall)
	applyFont(app.logs, fontUI)
	initLogTable()
	sendMessage.Call(uintptr(app.list), lbSetItemHeight, 0, 30)
	updateAccess()
}
func control(p HWND, class, text string, style uint32, x, y, w, h, id int) HWND {
	c, t := utf16(class), utf16(text)
	r, _, _ := createWindow.Call(0, uintptr(unsafe.Pointer(&c[0])), uintptr(unsafe.Pointer(&t[0])), uintptr(style), uintptr(x), uintptr(y), uintptr(w), uintptr(h), uintptr(p), uintptr(id), 0, 0)
	return HWND(r)
}
func layout(w, h int) {
	if app == nil || app.list == 0 {
		return
	}
	app.clientW, app.clientH = w, h
	right := w - 30
	move(app.port, right-244, 29, 78, 31)
	move(app.start, right-155, 27, 155, 36)
	if app.splitY == 0 {
		app.splitY = 229 + (h-265)*55/100
	}
	if app.splitY < 420 {
		app.splitY = 420
	}
	if app.splitY > h-135 {
		app.splitY = h - 135
	}
	listH := app.splitY - 229
	move(app.list, 30, 175, w-60, listH)
	y := app.splitY - 42
	move(app.addFile, 30, y, 116, 34)
	move(app.addDir, 154, y, 130, 34)
	move(app.rename, 292, y, 100, 34)
	move(app.remove, 400, y, 90, 34)
	move(app.open, right-159, y, 159, 34)
	move(app.copyURL, right-267, y, 100, 34)
	move(app.logTitle, 30, app.splitY+11, 120, 27)
	move(app.clearLog, right-100, app.splitY+6, 100, 32)
	move(app.splitter, 30, app.splitY, w-60, 6)
	move(app.logs, 30, app.splitY+45, w-60, h-app.splitY-60)
}
func move(h HWND, x, y, w, hh int) {
	moveWindow.Call(uintptr(h), uintptr(x), uintptr(y), uintptr(w), uintptr(hh), 1)
}
func refreshList() {
	if app.list == 0 {
		return
	}
	sendMessage.Call(uintptr(app.list), lbResetContent, 0, 0)
	shares := app.srv.Shares()
	if len(shares) == 0 {
		addList("     暂无分享内容 — 可将文件或文件夹拖到这里")
	}
	for _, x := range shares {
		st, _ := os.Stat(x.Path)
		kind := "文件"
		if st != nil && st.IsDir() {
			kind = "目录"
		}
		addList(fmt.Sprintf("   %-4s   %s      %s", kind, x.Name, x.Path))
	}
}
func addList(s string) {
	u := utf16(s)
	sendMessage.Call(uintptr(app.list), lbAddString, 0, uintptr(unsafe.Pointer(&u[0])))
}
func selected() int {
	r, _, _ := sendMessage.Call(uintptr(app.list), lbGetCurSel, 0, 0)
	if int32(r) == lbErr {
		return -1
	}
	return int(r)
}
func addPath(p string) {
	if e := app.srv.Add(p); e != nil {
		alert(e.Error())
	}
	refreshList()
	_ = app.srv.Save(app.config)
}
func addDropped(h uintptr) {
	n, _, _ := dragQuery.Call(h, 0xffffffff, 0, 0)
	for i := uintptr(0); i < n; i++ {
		size, _, _ := dragQuery.Call(h, i, 0, 0)
		b := make([]uint16, size+1)
		dragQuery.Call(h, i, uintptr(unsafe.Pointer(&b[0])), size+1)
		addPath(syscall.UTF16ToString(b))
	}
	dragFinish.Call(h)
}
func removeSelected() {
	i := selected()
	if i >= 0 {
		app.srv.Remove(i)
		refreshList()
		_ = app.srv.Save(app.config)
	}
}

func confirmRemoveSelected() {
	if selected() < 0 || selected() >= len(app.srv.Shares()) {
		return
	}
	title, message := utf16("确认移除"), utf16("仅从分享列表移除，不会删除磁盘上的文件。确定继续吗？")
	r, _, _ := messageBox.Call(uintptr(app.hwnd), uintptr(unsafe.Pointer(&message[0])), uintptr(unsafe.Pointer(&title[0])), 0x34)
	if r == 6 {
		removeSelected()
	}
}

func showShareMenu(position uintptr) {
	var p point
	if position == ^uintptr(0) {
		getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	} else {
		p.X, p.Y = int32(int16(position)), int32(int16(position>>16))
	}
	client := p
	screenToClient.Call(uintptr(app.list), uintptr(unsafe.Pointer(&client)))
	item, _, _ := sendMessage.Call(uintptr(app.list), lbItemFromPoint, 0, uintptr(uint16(client.X))|uintptr(uint16(client.Y))<<16)
	if uint16(item>>16) != 0 || int(uint16(item)) >= len(app.srv.Shares()) {
		return
	}
	sendMessage.Call(uintptr(app.list), lbSetCurSel, uintptr(uint16(item)), 0)
	menu, _, _ := createPopupMenu.Call()
	defer destroyMenu.Call(menu)
	for _, x := range []struct {
		id   uintptr
		text string
	}{{idMenuOpen, "在浏览器中打开"}, {idMenuCopy, "复制分享地址"}, {idMenuRename, "重命名"}, {idMenuRemove, "从分享列表移除"}} {
		u := utf16(x.text)
		appendMenu.Call(menu, 0, x.id, uintptr(unsafe.Pointer(&u[0])))
	}
	cmd, _, _ := trackPopupMenu.Call(menu, 0x100|2, uintptr(p.X), uintptr(p.Y), 0, uintptr(app.hwnd), 0)
	switch cmd {
	case idMenuOpen:
		openSelected()
	case idMenuCopy:
		copySelectedAddress()
	case idMenuRename:
		renameSelected()
	case idMenuRemove:
		confirmRemoveSelected()
	}
}

func selectedAddress() string {
	i := selected()
	shares := app.srv.Shares()
	if i < 0 || i >= len(shares) {
		return ""
	}
	return serverAddress(getText(app.port)) + "/" + url.PathEscape(shares[i].Name)
}

func openSelected() {
	address := selectedAddress()
	if address == "" {
		return
	}
	if !app.running {
		alert("请先启动服务")
		return
	}
	_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", address).Start()
}

func copySelectedAddress() {
	address := selectedAddress()
	if address == "" {
		return
	}
	copyText(address)
}
func renameSelected() {
	i := selected()
	if i < 0 {
		return
	}
	items := app.srv.Shares()
	name := prompt("虚拟重命名", "输入浏览器中显示的名称：", items[i].Name)
	if name != "" {
		if e := app.srv.Rename(i, name); e != nil {
			alert(e.Error())
		}
		refreshList()
		_ = app.srv.Save(app.config)
	}
}
func toggle() {
	updateAccess()
	if app.transitioning {
		return
	}
	p := getText(app.port)
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		alert("端口必须是 1 到 65535")
		return
	}
	app.transitioning = true
	enableWindow.Call(uintptr(app.start), 0)
	if app.running {
		setText(app.status, "●  正在停止服务…")
		go func() {
			err := app.srv.Stop()
			app.stateMu.Lock()
			app.stateErr, app.stateRunning = errorText(err), false
			app.stateMu.Unlock()
			postMessage.Call(uintptr(app.hwnd), wmServerState, 0, 0)
		}()
		return
	}
	setText(app.status, "●  正在检查端口并启动…")
	go func() {
		_, err := app.srv.Start(":" + p)
		app.stateMu.Lock()
		app.stateErr, app.stateRunning = errorText(err), err == nil
		app.stateMu.Unlock()
		postMessage.Call(uintptr(app.hwnd), wmServerState, 0, 0)
	}()
}

func errorText(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func finishTransition() {
	app.stateMu.Lock()
	message, running := app.stateErr, app.stateRunning
	app.stateMu.Unlock()
	app.transitioning, app.running = false, running
	enableWindow.Call(uintptr(app.start), 1)
	if message != "" {
		setText(app.start, "启动服务")
		setText(app.status, "●  服务未启动")
		alert("无法启动服务。端口可能已被其他程序占用。\n\n详细信息：" + message)
		return
	}
	if running {
		setText(app.start, "停止服务")
		setText(app.status, "●  正在运行    "+serverAddress(getText(app.port)))
	} else {
		setText(app.start, "启动服务")
		setText(app.status, "●  服务未启动")
	}
}
func openBrowser() {
	p := getText(app.port)
	_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", "http://127.0.0.1:"+p).Start()
}

func serverAddress(port string) string {
	for _, iface := range mustInterfaces() {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			ip, _, _ := net.ParseCIDR(addr.String())
			if ip != nil && ip.To4() != nil && !ip.IsLoopback() {
				return "http://" + ip.String() + ":" + port
			}
		}
	}
	return "http://127.0.0.1:" + port
}

func mustInterfaces() []net.Interface { x, _ := net.Interfaces(); return x }

func copyAddress() {
	address := serverAddress(getText(app.port))
	copyText(address)
}

func copyText(address string) {
	cmd := exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", "Set-Clipboard -Value '"+ps(address)+"'")
	if err := cmd.Run(); err != nil {
		alert("复制地址失败: " + err.Error())
		return
	}
	setText(app.status, "✓  已复制    "+address)
}
func refreshLogLegacy() {
	s := app.log.String()
	display := strings.ReplaceAll(strings.ReplaceAll(s, "\\r\\n", "\\n"), "\\n", "\\r\\n")
	setText(app.logs, display)
	sendMessage.Call(uintptr(app.logs), emSetSel, uintptr(len([]rune(display))), uintptr(len([]rune(display))))
	sendMessage.Call(uintptr(app.logs), emScrollCaret, 0, 0)
}
func refreshLog() {
	sendMessage.Call(uintptr(app.logs), lvmDeleteAll, 0, 0)
	lines := strings.Split(app.log.String(), "\n")
	start := 0
	if len(lines) > 500 {
		start = len(lines) - 500
	}
	row := 0
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		cols := []string{"", "", "", line, "", ""}
		if len(fields) >= 7 && (fields[3] == "GET" || fields[3] == "POST" || fields[3] == "HEAD" || fields[3] == "PUT" || fields[3] == "DELETE") {
			cols = []string{fields[0] + " " + fields[1], fields[2], fields[3], fields[4], fields[5], fields[6]}
		} else if len(fields) >= 3 && strings.Contains(fields[0], "/") && strings.Contains(fields[1], ":") {
			cols[0], cols[3] = fields[0]+" "+fields[1], strings.Join(fields[2:], " ")
		}
		insertLogRow(row, cols)
		row++
	}
}

func initLogTable() {
	sendMessage.Call(uintptr(app.logs), lvmSetExtended, 0, 0x21)
	for i, col := range []struct {
		name  string
		width int32
	}{{"时间", 145}, {"客户端", 145}, {"方法", 65}, {"路径 / 消息", 360}, {"状态", 65}, {"耗时", 75}} {
		u := utf16(col.name)
		c := lvColumn{Mask: 1 | 2 | 4, Width: col.width, Text: uintptr(unsafe.Pointer(&u[0])), SubItem: int32(i)}
		sendMessage.Call(uintptr(app.logs), lvmInsertColumn, uintptr(i), uintptr(unsafe.Pointer(&c)))
	}
}

func insertLogRow(row int, cols []string) {
	for col, value := range cols {
		u := utf16(value)
		item := lvItem{Mask: 1, Item: int32(row), SubItem: int32(col), Text: uintptr(unsafe.Pointer(&u[0]))}
		message := uintptr(lvmSetItem)
		if col == 0 {
			message = lvmInsertItem
		}
		sendMessage.Call(uintptr(app.logs), message, 0, uintptr(unsafe.Pointer(&item)))
	}
}
func setText(h HWND, s string) {
	u := utf16(s)
	setWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&u[0])))
}
func getText(h HWND) string {
	n, _, _ := getWindowTextLen.Call(uintptr(h))
	b := make([]uint16, n+1)
	getWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&b[0])), n+1)
	return syscall.UTF16ToString(b)
}
func alert(s string) {
	t, m := utf16("HFS Go"), utf16(s)
	messageBox.Call(uintptr(app.hwnd), uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), 0x10)
}
func utf16(s string) []uint16 { return syscall.StringToUTF16(s) }

func rgb(r, g, b uintptr) uintptr { return r | g<<8 | b<<16 }

func makeFont(face string, size, weight int) uintptr {
	name := utf16(face)
	h, _, _ := createFont.Call(uintptr(uint32(int32(-size))), 0, 0, 0, uintptr(weight), 0, 0, 0, 1, 0, 0, 5, 0, uintptr(unsafe.Pointer(&name[0])))
	return h
}

func applyFont(hwnd HWND, font uintptr) {
	sendMessage.Call(uintptr(hwnd), wmSetFont, font, 1)
}

func checked(hwnd HWND) bool {
	r, _, _ := sendMessage.Call(uintptr(hwnd), bmGetCheck, 0, 0)
	return r == 1
}

func updateAccess() {
	if app == nil || app.password == 0 {
		return
	}
	app.srv.SetAccess(getText(app.password), checked(app.upload), checked(app.download), checked(app.manage))
}

func trayData() notifyIconData {
	icon, _, _ := loadIcon.Call(0, 32512)
	n := notifyIconData{Hwnd: uintptr(app.hwnd), UID: 1, Flags: 1 | 2 | 4, Callback: wmTray, Icon: icon}
	n.Size = uint32(unsafe.Sizeof(n))
	copy(n.Tip[:], utf16("HFS Go - 双击显示，右击打开菜单"))
	return n
}

func minimizeToTray() {
	n := trayData()
	shellNotify.Call(0, uintptr(unsafe.Pointer(&n)))
	showWindow.Call(uintptr(app.hwnd), 0)
}

func restoreFromTray() {
	removeTray()
	showWindow.Call(uintptr(app.hwnd), 9)
	setFocus.Call(uintptr(app.hwnd))
}

func showTrayMenu() {
	var p point
	getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	menu, _, _ := createPopupMenu.Call()
	defer destroyMenu.Call(menu)
	for _, x := range []struct {
		id   uintptr
		text string
	}{{idTrayShow, "显示主窗口"}, {idTrayOpen, "在浏览器中打开"}, {idTrayExit, "退出"}} {
		u := utf16(x.text)
		appendMenu.Call(menu, 0, x.id, uintptr(unsafe.Pointer(&u[0])))
	}
	setForeground.Call(uintptr(app.hwnd))
	cmd, _, _ := trackPopupMenu.Call(menu, 0x100|2, uintptr(p.X), uintptr(p.Y), 0, uintptr(app.hwnd), 0)
	switch cmd {
	case idTrayShow:
		restoreFromTray()
	case idTrayOpen:
		if app.running {
			openBrowser()
		} else {
			restoreFromTray()
			alert("服务尚未启动")
		}
	case idTrayExit:
		exitApplication()
	}
}

func exitApplication() {
	removeTray()
	_ = app.srv.Stop()
	_ = app.srv.Save(app.config)
	destroyWindow.Call(uintptr(app.hwnd))
}

func removeTray() {
	if app == nil || app.hwnd == 0 {
		return
	}
	n := trayData()
	shellNotify.Call(2, uintptr(unsafe.Pointer(&n)))
}

func addFileDialog() {
	buf := make([]uint16, 32768)
	filter := utf16("所有文件\x00*.*\x00\x00")
	title := utf16("选择要分享的文件")
	o := openFileName{Size: uint32(unsafe.Sizeof(openFileName{})), Owner: uintptr(app.hwnd), Filter: uintptr(unsafe.Pointer(&filter[0])), File: uint64(uintptr(unsafe.Pointer(&buf[0]))), MaxFile: uint32(len(buf)), Title: uintptr(unsafe.Pointer(&title[0])), Flags: 0x1000 | 0x800}
	if r, _, _ := getOpenFile.Call(uintptr(unsafe.Pointer(&o))); r != 0 {
		addPath(syscall.UTF16ToString(buf))
	}
}
func addFolderDialog() {
	display := make([]uint16, 260)
	title := utf16("选择要分享的文件夹")
	bi := browseInfo{Owner: uintptr(app.hwnd), DisplayName: uintptr(unsafe.Pointer(&display[0])), Title: uintptr(unsafe.Pointer(&title[0])), Flags: 0x40 | 1}
	pid, _, _ := browseFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pid == 0 {
		return
	}
	defer coTaskFree.Call(pid)
	buf := make([]uint16, 32768)
	if ok, _, _ := pathFromID.Call(pid, uintptr(unsafe.Pointer(&buf[0]))); ok != 0 {
		addPath(syscall.UTF16ToString(buf))
	}
}

// A tiny modal-like input implemented with a temporary native window.
func prompt(title, label, initial string) string {
	// The Win32 API has no stock input box. PowerShell's VisualBasic InputBox is
	// available on supported Windows systems and preserves Unicode correctly.
	script := fmt.Sprintf("Add-Type -AssemblyName Microsoft.VisualBasic; [Microsoft.VisualBasic.Interaction]::InputBox('%s','%s','%s')", ps(label), ps(title), ps(initial))
	c := exec.Command("powershell.exe", "-NoProfile", "-WindowStyle", "Hidden", "-Command", script)
	out, e := c.Output()
	if e != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
func ps(s string) string { return strings.ReplaceAll(s, "'", "''") }
