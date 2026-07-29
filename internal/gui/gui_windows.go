//go:build windows

package gui

import (
	"bytes"
	"errors"
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
	"time"
	unicodeutf16 "unicode/utf16"
	"unsafe"

	"hfsgo/internal/server"
)

type HWND uintptr
type appState struct {
	hwnd, list, logs, status, port                                       HWND
	title, subtitle, shareTitle, shareHint, logTitle, portLabel          HWND
	address, addressLabel, password, upload, download, manage            HWND
	chatEnabled, chatGroup, start, addFile, addDir, remove, rename, open HWND
	copyURL, clearLog, chatView, chatList, chatDetails, chatHistory      HWND
	chatInput, chatSend, splitter                                        HWND
	srv                                                                  *server.Server
	log                                                                  *safeBuffer
	running                                                              bool
	transitioning                                                        bool
	stateMu                                                              sync.Mutex
	stateErr                                                             string
	stateRunning                                                         bool
	runningAddress, runningPort, statusAddress                           string
	clientW, clientH, splitY                                             int
	chatMode                                                             bool
	chatIDs                                                              []string
	chatSelected                                                         string
	accessHosts                                                          []string
	seenChatMessages                                                     map[string]struct{}
	seenGroupMode, seenModeKnown                                         bool
	chatProtocolVersion                                                  uint64
	chatImageMu                                                          sync.Mutex
	chatImageData                                                        []byte
	chatImageMime, chatImageTarget, chatImageErr                         string
	chatImageProtocolVersion                                             uint64
	chatImagePending                                                     bool
	visitorMu                                                            sync.Mutex
	pendingVisitors                                                      []server.ChatClientInfo
	visitorPostPending                                                   bool
	trayAdded                                                            bool
	notifyNewVisitor, notifyNewMessage                                   bool
	config                                                               string
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
	isWindowVisible  = user32.NewProc("IsWindowVisible")
	isIconic         = user32.NewProc("IsIconic")
	getForeground    = user32.NewProc("GetForegroundWindow")
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
	messageBeep      = user32.NewProc("MessageBeep")
	setFocus         = user32.NewProc("SetFocus")
	setCursor        = user32.NewProc("SetCursor")
	setForeground    = user32.NewProc("SetForegroundWindow")
	registerMessage  = user32.NewProc("RegisterWindowMessageW")
	setWindowLongPtr = user32.NewProc("SetWindowLongPtrW")
	callWindowProc   = user32.NewProc("CallWindowProcW")
	dragAccept       = shell32.NewProc("DragAcceptFiles")
	dragQuery        = shell32.NewProc("DragQueryFileW")
	dragQueryPoint   = shell32.NewProc("DragQueryPoint")
	dragFinish       = shell32.NewProc("DragFinish")
	browseFolder     = shell32.NewProc("SHBrowseForFolderW")
	pathFromID       = shell32.NewProc("SHGetPathFromIDListW")
	coTaskFree       = syscall.NewLazyDLL("ole32.dll").NewProc("CoTaskMemFree")
	getOpenFile      = comdlg32.NewProc("GetOpenFileNameW")
	getOpenFileError = comdlg32.NewProc("CommDlgExtendedError")
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

var fontUI, fontTitle, fontSmall, fontMono, brushBackground, brushWhite, brushSplitter, handCursor uintptr
var splitterDragging bool
var originalChatInputProc uintptr
var chatInputComposing bool
var chatInputCallback = syscall.NewCallback(chatInputWindowProc)
var taskbarCreatedMessage uint32

const (
	wmCreate           = 1
	wmDestroy          = 2
	wmSize             = 5
	wmKillFocus        = 8
	wmSetCursor        = 0x0020
	wmCommand          = 0x111
	wmSysCommand       = 0x0112
	wmCtlColorEdit     = 0x0133
	wmCtlColorList     = 0x0134
	wmCtlColorStatic   = 0x0138
	wmContextMenu      = 0x007B
	wmKeyDown          = 0x0100
	wmChar             = 0x0102
	wmIMEStart         = 0x010D
	wmIMEEnd           = 0x010E
	wmDropFiles        = 0x233
	wmClose            = 0x10
	wmLog              = 0x8001
	wmTray             = 0x8002
	wmServerState      = 0x8003
	wmChat             = 0x8004
	wmChatImage        = 0x8005
	wmVisitor          = 0x8006
	wmPaste            = 0x0302
	wmLButtonDown      = 0x0201
	wmLButtonUp        = 0x0202
	wmMouseMove        = 0x0200
	scClose            = 0xF060
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
	lbsExtendedSel     = 0x0800
	bsPushButton       = 0
	bsFlat             = 0x8000
	bsAutoCheckBox     = 3
	ssNotify           = 0x0100
	cbsDropDownList    = 3
	cbsHasStrings      = 0x0200
	esPassword         = 0x20
	swShow             = 5
	swMinimize         = 6
	lbAddString        = 0x180
	lbResetContent     = 0x184
	lbGetCurSel        = 0x188
	lbGetSel           = 0x0187
	lbSetSel           = 0x0185
	lbGetSelCount      = 0x0190
	lbGetSelItems      = 0x0191
	lbGetItemRect      = 0x0198
	lbSetAnchorIndex   = 0x019C
	lbSetCaretIndex    = 0x019E
	lbErr              = -1
	emSetSel           = 0xB1
	emScrollCaret      = 0xB7
	emSetCueBanner     = 0x1501
	wmSetFont          = 0x30
	lbSetItemHeight    = 0x1A0
	lbSetCurSel        = 0x0186
	lbItemFromPoint    = 0x01A9
	cbAddString        = 0x0143
	cbGetCurSel        = 0x0147
	cbResetContent     = 0x014B
	cbSetCurSel        = 0x014E
	cbSetDroppedWidth  = 0x0160
	bnClicked          = 0
	cbnSelChange       = 1
	lbnDblClk          = 2
	lbnSelChange       = 1
	stnClicked         = 0
	lvmDeleteAll       = 0x1009
	lvmInsertItem      = 0x104D
	lvmSetItem         = 0x104C
	lvmInsertColumn    = 0x1061
	lvmSetExtended     = 0x1036
	bmGetCheck         = 0x00F0
	bmSetCheck         = 0x00F1
	vkReturn           = 0x0D
	nimAdd             = 0
	nimModify          = 1
	nimDelete          = 2
	nimSetVersion      = 4
	nifMessage         = 0x01
	nifIcon            = 0x02
	nifTip             = 0x04
	nifInfo            = 0x10
	nifShowTip         = 0x80
	niifInfo           = 0x01
	niifUser           = 0x04
	niifNoSound        = 0x10
	niifLargeIcon      = 0x20
	notifyIconVersion4 = 4
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
	idChatEnable = 1013
	idChatView   = 1014
	idChatList   = 1015
	idChatSend   = 1016
	idChatInput  = 1017
	idAddress    = 1018
	idChatGroup  = 1019
	idStatus     = 1022
	idMenuOpen   = 1101
	idMenuCopy   = 1102
	idMenuRename = 1103
	idMenuRemove = 1104
	idTrayShow   = 1201
	idTrayOpen   = 1202
	idTrayExit   = 1203
)

type point struct{ X, Y int32 }
type rect struct{ Left, Top, Right, Bottom int32 }
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
	File                                   uintptr
	MaxFile                                uint32
	FileTitle                              uintptr
	MaxFileTitle                           uint32
	InitialDir, Title                      uintptr
	Flags                                  uint32
	FileOffset, FileExtension              uint16
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

type accessAddress struct {
	host  string
	label string
	ip    net.IP
}

func runLegacy() error {
	icc := initCommonControls{Size: uint32(unsafe.Sizeof(initCommonControls{})), Classes: 1}
	initCommon.Call(uintptr(unsafe.Pointer(&icc)))
	exe, _ := os.Executable()
	cfg := filepath.Join(filepath.Dir(exe), "hfsgo.json")
	buf := &safeBuffer{}
	app = &appState{log: buf, config: cfg, seenChatMessages: make(map[string]struct{})}
	app.srv = server.New(buf)
	_ = app.srv.Load(cfg)
	_ = loadRichEdit()
	taskbarCreated := utf16("TaskbarCreated")
	registered, _, _ := registerMessage.Call(uintptr(unsafe.Pointer(&taskbarCreated[0])))
	taskbarCreatedMessage = uint32(registered)
	inst, _, _ := getModule.Call(0)
	cur, _, _ := loadCursor.Call(0, 32512)
	resizeCursor, _, _ := loadCursor.Call(0, 32645)
	handCursor, _, _ = loadCursor.Call(0, 32649)
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
	title := utf16("LanChatGo - 局域网聊天与文件分享")
	h, _, e := createWindow.Call(0, uintptr(unsafe.Pointer(&cls[0])), uintptr(unsafe.Pointer(&title[0])), wsOverlappedWindow|wsVisible, 100, 60, 1020, 760, 0, 0, inst, 0)
	if h == 0 {
		return e
	}
	app.hwnd = HWND(h)
	buf.hwnd = HWND(h)
	rememberChatMessages(app.srv.ChatOverview())
	app.srv.SetChatNotifier(func() {
		if app != nil && app.hwnd != 0 {
			postMessage.Call(uintptr(app.hwnd), wmChat, 0, 0)
		}
	})
	app.srv.SetVisitorNotifier(func(info server.ChatClientInfo) {
		if app == nil || app.hwnd == 0 {
			return
		}
		app.visitorMu.Lock()
		if len(app.pendingVisitors) >= 32 {
			copy(app.pendingVisitors, app.pendingVisitors[len(app.pendingVisitors)-31:])
			app.pendingVisitors = app.pendingVisitors[:31]
		}
		app.pendingVisitors = append(app.pendingVisitors, info)
		if app.visitorPostPending {
			app.visitorMu.Unlock()
			return
		}
		app.visitorPostPending = true
		window := app.hwnd
		app.visitorMu.Unlock()
		time.AfterFunc(500*time.Millisecond, func() {
			posted, _, _ := postMessage.Call(uintptr(window), wmVisitor, 0, 0)
			if posted == 0 && app != nil {
				app.visitorMu.Lock()
				app.visitorPostPending = false
				app.visitorMu.Unlock()
			}
		})
	})
	dragAccept.Call(h, 1)
	ensureTray()
	refreshList()
	refreshChat()
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
	if taskbarCreatedMessage != 0 && m == taskbarCreatedMessage {
		if app != nil {
			app.trayAdded = false
			ensureTray()
		}
		return 0
	}
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
		handleDropped(w)
		return 0
	case wmCommand:
		if HWND(l) == app.status && uint16(w>>16) == stnClicked {
			copyStatusAddress()
			return 0
		}
		if HWND(l) == app.list {
			switch uint16(w >> 16) {
			case lbnDblClk:
				openSelected()
				return 0
			case lbnSelChange:
				updateShareActions()
				return 0
			}
		}
		if HWND(l) == app.chatList && uint16(w) == idChatList {
			switch uint16(w >> 16) {
			case lbnSelChange:
				selectChatConversation()
				return 0
			case lbnDblClk:
				dismissSelectedChatVisitor()
				return 0
			}
		}
		if uint16(w) == idAddress && uint16(w>>16) == cbnSelChange {
			refreshRunningAddress()
			return 0
		}
		if uint16(w) == idPassword || uint16(w) == idUpload || uint16(w) == idDownload || uint16(w) == idManage || uint16(w) == idChatEnable || uint16(w) == idChatGroup {
			updateAccess()
			if uint16(w) == idChatEnable || uint16(w) == idChatGroup {
				refreshChat()
			}
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
				confirmRemoveSelected()
			case idRename:
				renameSelected()
			case idOpen:
				openBrowser()
			case idCopyURL:
				copyAddress()
			case idClearLog:
				app.log.Clear()
				refreshLog()
			case idChatView:
				toggleChatView()
			case idChatSend:
				sendChatMessage()
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
	case wmSetCursor:
		if app != nil && HWND(w) == app.status && app.running && !app.transitioning && app.statusAddress != "" && handCursor != 0 {
			setCursor.Call(handCursor)
			return 1
		}
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
	case wmChat:
		handleChatUpdate()
		return 0
	case wmChatImage:
		finishChatImage()
		return 0
	case wmVisitor:
		handleVisitorNotifications()
		return 0
	case wmTray:
		event := trayCallbackEvent(l)
		if event == 0x203 || event == 0x405 {
			restoreFromTray()
		} else if event == 0x205 || event == wmContextMenu {
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
	case wmSysCommand:
		if isKeyboardCloseCommand(m, w, l) {
			exitApplication()
			return 0
		}
	case wmClose:
		minimizeToTray()
		return 0
	case wmDestroy:
		app.srv.SetChatNotifier(nil)
		app.srv.SetVisitorNotifier(nil)
		removeTray()
		postQuit.Call(0)
		return 0
	}
	r, _, _ := defWindowProc.Call(hwnd, uintptr(m), w, l)
	return r
}

func isKeyboardCloseCommand(message uint32, wParam, lParam uintptr) bool {
	return message == wmSysCommand && wParam&0xFFF0 == scClose && lParam == 0
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
	app.title = control(parent, "STATIC", "LanChatGo", wsVisible|wsChild, 30, 20, 280, 38, 0)
	app.subtitle = control(parent, "STATIC", "轻量、安全的局域网文件分享", wsVisible|wsChild, 30, 60, 360, 24, 0)
	app.status = control(parent, "STATIC", "●  服务未启动", wsVisible|wsChild|ssNotify, 30, 94, 400, 25, idStatus)
	app.addressLabel = control(parent, "STATIC", "访问地址", wsVisible|wsChild, 330, 35, 65, 22, 0)
	app.address = control(parent, "COMBOBOX", "", wsVisible|wsChild|wsVScroll|wsTabStop|cbsDropDownList|cbsHasStrings, 400, 29, 270, 240, idAddress)
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
	app.shareHint = control(parent, "STATIC", "将文件或文件夹拖到下方；Ctrl/Shift 可多选", wsVisible|wsChild, 132, 142, 390, 22, 0)
	app.list = control(parent, "LISTBOX", "", wsVisible|wsChild|wsBorder|wsVScroll|lbsNotify|lbsExtendedSel, 30, 175, 939, 280, 0)
	app.addFile = control(parent, "BUTTON", "+  添加文件", wsVisible|wsChild|wsTabStop|bsFlat, 30, 467, 116, 34, idAddFile)
	app.addDir = control(parent, "BUTTON", "+  添加文件夹", wsVisible|wsChild|wsTabStop|bsFlat, 154, 467, 130, 34, idAddDir)
	app.rename = control(parent, "BUTTON", "重命名", wsVisible|wsChild|wsTabStop|bsFlat, 292, 467, 100, 34, idRename)
	app.remove = control(parent, "BUTTON", "移除", wsVisible|wsChild|wsTabStop|bsFlat, 400, 467, 90, 34, idRemove)
	app.copyURL = control(parent, "BUTTON", "复制地址", wsVisible|wsChild|wsTabStop|bsFlat, 700, 467, 100, 34, idCopyURL)
	app.open = control(parent, "BUTTON", "在浏览器中打开", wsVisible|wsChild|wsTabStop|bsFlat, 810, 467, 159, 34, idOpen)
	app.logTitle = control(parent, "STATIC", "访问日志", wsVisible|wsChild, 30, 521, 120, 27, 0)
	app.chatEnabled = control(parent, "BUTTON", "允许聊天", wsVisible|wsChild|wsTabStop|bsAutoCheckBox, 150, 517, 105, 30, idChatEnable)
	app.chatGroup = control(parent, "BUTTON", "系统群", wsVisible|wsChild|wsTabStop|bsAutoCheckBox, 260, 517, 105, 30, idChatGroup)
	app.chatView = control(parent, "BUTTON", "在线聊天 (0)", wsVisible|wsChild|wsTabStop|bsFlat, 370, 516, 125, 32, idChatView)
	app.clearLog = control(parent, "BUTTON", "清空日志", wsVisible|wsChild|wsTabStop|bsFlat, 869, 516, 100, 32, idClearLog)
	app.splitter = control(parent, "HFSGoSplitter", "", wsVisible|wsChild, 30, 548, 939, 6, 0)
	app.logs = control(parent, "SysListView32", "", wsVisible|wsChild|wsBorder|wsVScroll|1|4, 30, 556, 939, 150, 0)
	app.chatList = control(parent, "LISTBOX", "", wsChild|wsBorder|wsVScroll|lbsNotify, 30, 556, 190, 150, idChatList)
	app.chatDetails = control(parent, "EDIT", "", wsChild|wsBorder|wsVScroll|esMultiLine|esReadOnly|esAutoVScroll, 228, 556, 741, 60, 0)
	historyClass := "RICHEDIT50W"
	if !loadRichEdit() {
		historyClass = "EDIT"
	}
	app.chatHistory = control(parent, historyClass, "", wsChild|wsBorder|wsVScroll|esMultiLine|esReadOnly|esAutoVScroll, 228, 622, 741, 42, 0)
	app.chatInput = control(parent, "EDIT", "", wsChild|wsBorder|wsTabStop, 228, 674, 645, 32, idChatInput)
	app.chatSend = control(parent, "BUTTON", "发送", wsChild|wsTabStop|bsPushButton|bsFlat, 881, 674, 88, 32, idChatSend)
	for _, h := range []HWND{app.subtitle, app.status, app.addressLabel, app.address, app.portLabel, app.port, app.password, app.upload, app.download, app.manage, app.start, app.shareTitle, app.shareHint, app.list, app.addFile, app.addDir, app.remove, app.rename, app.copyURL, app.open, app.logTitle, app.chatEnabled, app.chatGroup, app.chatView, app.clearLog, app.chatList, app.chatDetails, app.chatInput, app.chatSend} {
		applyFont(h, fontUI)
	}
	applyFont(app.title, fontTitle)
	applyFont(app.subtitle, fontSmall)
	applyFont(app.shareHint, fontSmall)
	applyFont(app.logs, fontUI)
	applyFont(app.chatDetails, fontSmall)
	applyFont(app.chatHistory, fontMono)
	sendMessage.Call(uintptr(app.chatHistory), emSetBackground, 0, rgb(246, 248, 251))
	inputHint := utf16("输入消息，Enter 发送；也可 Ctrl+V 或拖入图片")
	sendMessage.Call(uintptr(app.chatInput), emSetCueBanner, 1, uintptr(unsafe.Pointer(&inputHint[0])))
	initLogTable()
	sendMessage.Call(uintptr(app.list), lbSetItemHeight, 0, 30)
	sendMessage.Call(uintptr(app.chatList), lbSetItemHeight, 0, 28)
	sendMessage.Call(uintptr(app.chatInput), 0x00C5, 32768, 0)
	populateAccessAddresses()
	subclassChatInput()
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
	if w < 400 {
		w = 400
	}
	if h < 560 {
		h = 560
	}
	app.clientW, app.clientH = w, h
	right := w - 30
	addressWidth := right - 701
	if addressWidth < 80 {
		addressWidth = 80
	}
	move(app.addressLabel, 330, 35, 65, 22)
	move(app.address, 400, 29, addressWidth, 240)
	move(app.portLabel, right-291, 35, 42, 22)
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
	move(app.chatEnabled, 150, app.splitY+7, 105, 30)
	move(app.chatGroup, 260, app.splitY+7, 105, 30)
	move(app.chatView, 370, app.splitY+6, 125, 32)
	move(app.clearLog, right-100, app.splitY+6, 100, 32)
	move(app.splitter, 30, app.splitY, w-60, 6)
	panelY, panelH := app.splitY+45, h-app.splitY-60
	move(app.logs, 30, panelY, w-60, panelH)
	chatListWidth := 220
	contentX := 30 + chatListWidth + 8
	move(app.chatList, 30, panelY, chatListWidth, panelH)
	detailsH := 76
	historyH := panelH - detailsH - 46
	if historyH < 20 {
		detailsH = 20
		historyH = 20
	}
	move(app.chatDetails, contentX, panelY, w-contentX-30, detailsH)
	move(app.chatHistory, contentX, panelY+detailsH+6, w-contentX-30, historyH)
	bottomY := panelY + panelH - 32
	inputWidth := w - contentX - 118
	if inputWidth < 40 {
		inputWidth = 40
	}
	move(app.chatInput, contentX, bottomY, inputWidth, 32)
	move(app.chatSend, w-102, bottomY, 72, 32)
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
	updateShareActions()
}
func addList(s string) {
	u := utf16(s)
	sendMessage.Call(uintptr(app.list), lbAddString, 0, uintptr(unsafe.Pointer(&u[0])))
}

func selectedIndices() []int {
	if app == nil || app.list == 0 {
		return nil
	}
	countResult, _, _ := sendMessage.Call(uintptr(app.list), lbGetSelCount, 0, 0)
	count := int(int32(countResult))
	if count <= 0 {
		return nil
	}
	raw := make([]int32, count)
	copiedResult, _, _ := sendMessage.Call(
		uintptr(app.list),
		lbGetSelItems,
		uintptr(count),
		uintptr(unsafe.Pointer(&raw[0])),
	)
	copied := int(int32(copiedResult))
	if copied <= 0 {
		return nil
	}
	if copied > len(raw) {
		copied = len(raw)
	}
	shareCount := len(app.srv.Shares())
	indices := make([]int, 0, copied)
	for _, value := range raw[:copied] {
		index := int(value)
		if index >= 0 && index < shareCount {
			indices = append(indices, index)
		}
	}
	return indices
}

func selected() int {
	indices := selectedIndices()
	if len(indices) != 1 {
		return -1
	}
	return indices[0]
}

func updateShareActions() {
	if app == nil || app.remove == 0 {
		return
	}
	count := len(selectedIndices())
	enableWindow.Call(uintptr(app.remove), boolFlag(count > 0))
	enableWindow.Call(uintptr(app.rename), boolFlag(count == 1))
	if count > 1 {
		setText(app.remove, fmt.Sprintf("移除 (%d)", count))
	} else {
		setText(app.remove, "移除")
	}
}

func addPath(p string) {
	if e := app.srv.Add(p); e != nil {
		alert(e.Error())
	}
	refreshList()
	_ = app.srv.Save(app.config)
}
func handleDropped(h uintptr) {
	dropPoint, pointOK, paths := consumeDroppedPaths(h)
	if pointOK && chatDropTarget(dropPoint) {
		if len(paths) == 0 {
			return
		}
		imagePath := ""
		for _, candidate := range paths {
			switch strings.ToLower(filepath.Ext(candidate)) {
			case ".png", ".jpg", ".jpeg":
				imagePath = candidate
			}
			if imagePath != "" {
				break
			}
		}
		if imagePath == "" {
			alert("聊天拖拽仅支持 PNG 或 JPEG 图片")
			return
		}
		if len(paths) > 1 {
			alert("聊天区一次发送一张图片，本次将发送第一张支持的图片。")
		}
		sendDroppedChatImage(imagePath)
		return
	}
	for _, droppedPath := range paths {
		addPath(droppedPath)
	}
}

func consumeDroppedPaths(h uintptr) (point, bool, []string) {
	var dropPoint point
	pointOK, _, _ := dragQueryPoint.Call(h, uintptr(unsafe.Pointer(&dropPoint)))
	n, _, _ := dragQuery.Call(h, 0xffffffff, 0, 0)
	paths := make([]string, 0, n)
	for i := uintptr(0); i < n; i++ {
		size, _, _ := dragQuery.Call(h, i, 0, 0)
		b := make([]uint16, size+1)
		dragQuery.Call(h, i, uintptr(unsafe.Pointer(&b[0])), size+1)
		paths = append(paths, syscall.UTF16ToString(b))
	}
	dragFinish.Call(h)
	return dropPoint, pointOK != 0, paths
}

func chatDropTarget(dropPoint point) bool {
	if app == nil || !app.chatMode {
		return false
	}
	return int(dropPoint.X) >= 30 && int(dropPoint.Y) >= app.splitY+45
}
func removeShareIndices(indices []int) {
	if app.srv.RemoveMany(indices) > 0 {
		refreshList()
		_ = app.srv.Save(app.config)
	}
}

func confirmRemoveSelected() {
	indices := selectedIndices()
	if len(indices) == 0 {
		return
	}
	description := "确定从分享列表移除此项目吗？"
	if len(indices) > 1 {
		description = fmt.Sprintf("确定从分享列表移除选中的 %d 个项目吗？", len(indices))
	}
	title := utf16("确认移除")
	message := utf16(description + "\r\n\r\n此操作不会删除磁盘上的文件或文件夹。")
	r, _, _ := messageBox.Call(uintptr(app.hwnd), uintptr(unsafe.Pointer(&message[0])), uintptr(unsafe.Pointer(&title[0])), 0x34)
	if r == 6 {
		removeShareIndices(indices)
	}
}

func showShareMenu(position uintptr) {
	var p point
	if position == ^uintptr(0) {
		getCursorPos.Call(uintptr(unsafe.Pointer(&p)))
	} else {
		p.X, p.Y = int32(int16(position)), int32(int16(position>>16))
		client := p
		screenToClient.Call(uintptr(app.list), uintptr(unsafe.Pointer(&client)))
		item, _, _ := sendMessage.Call(uintptr(app.list), lbItemFromPoint, 0, uintptr(uint16(client.X))|uintptr(uint16(client.Y))<<16)
		index := int(uint16(item))
		if uint16(item>>16) != 0 || index >= len(app.srv.Shares()) {
			return
		}
		var bounds rect
		boundsResult, _, _ := sendMessage.Call(uintptr(app.list), lbGetItemRect, uintptr(index), uintptr(unsafe.Pointer(&bounds)))
		if int32(boundsResult) == lbErr ||
			client.X < bounds.Left || client.X >= bounds.Right ||
			client.Y < bounds.Top || client.Y >= bounds.Bottom {
			return
		}
		isSelected, _, _ := sendMessage.Call(uintptr(app.list), lbGetSel, uintptr(index), 0)
		if int32(isSelected) <= 0 {
			sendMessage.Call(uintptr(app.list), lbSetSel, 0, ^uintptr(0))
			sendMessage.Call(uintptr(app.list), lbSetSel, 1, uintptr(index))
			sendMessage.Call(uintptr(app.list), lbSetAnchorIndex, uintptr(index), 0)
		}
		sendMessage.Call(uintptr(app.list), lbSetCaretIndex, uintptr(index), 0)
		updateShareActions()
	}
	indices := selectedIndices()
	if len(indices) == 0 {
		return
	}
	menu, _, _ := createPopupMenu.Call()
	defer destroyMenu.Call(menu)
	type shareMenuItem struct {
		id   uintptr
		text string
	}
	items := make([]shareMenuItem, 0, 4)
	if len(indices) == 1 {
		items = append(items,
			shareMenuItem{idMenuOpen, "在浏览器中打开"},
			shareMenuItem{idMenuCopy, "复制分享地址"},
			shareMenuItem{idMenuRename, "重命名"},
		)
	}
	removeText := "从分享列表移除"
	if len(indices) > 1 {
		removeText = fmt.Sprintf("从分享列表移除（%d 项）", len(indices))
	}
	items = append(items, shareMenuItem{idMenuRemove, removeText})
	for _, x := range items {
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
	return advertisedServerAddress() + "/" + url.PathEscape(shares[i].Name)
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
	items := app.srv.Shares()
	if i < 0 || i >= len(items) {
		return
	}
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
	if app.running {
		app.transitioning = true
		app.statusAddress = ""
		enableWindow.Call(uintptr(app.start), 0)
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
	p := getText(app.port)
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		alert("端口必须是 1 到 65535")
		return
	}
	app.transitioning = true
	app.statusAddress = ""
	enableWindow.Call(uintptr(app.start), 0)
	app.runningPort = p
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
		app.runningAddress, app.runningPort, app.statusAddress = "", "", ""
		setText(app.start, "启动服务")
		setText(app.status, "●  服务未启动")
		alert("无法启动服务。端口可能已被其他程序占用。\n\n详细信息：" + message)
		return
	}
	if running {
		setText(app.start, "停止服务")
		refreshRunningAddress()
	} else {
		app.runningAddress, app.runningPort, app.statusAddress = "", "", ""
		setText(app.start, "启动服务")
		setText(app.status, "●  服务未启动")
	}
}
func openBrowser() {
	_ = exec.Command("rundll32.exe", "url.dll,FileProtocolHandler", advertisedServerAddress()).Start()
}

func serverAddress(port string) string {
	host := selectedAccessHost()
	if host == "" {
		host = "127.0.0.1"
	}
	return (&url.URL{Scheme: "http", Host: net.JoinHostPort(host, port)}).String()
}

func selectedAccessHost() string {
	if app == nil || app.address == 0 {
		return "127.0.0.1"
	}
	selected, _, _ := sendMessage.Call(uintptr(app.address), cbGetCurSel, 0, 0)
	index := int(int32(selected))
	if index < 0 || index >= len(app.accessHosts) {
		return "127.0.0.1"
	}
	return app.accessHosts[index]
}

func populateAccessAddresses() {
	if app == nil || app.address == 0 {
		return
	}
	addresses, selected := availableAccessAddresses()
	app.accessHosts = app.accessHosts[:0]
	sendMessage.Call(uintptr(app.address), cbResetContent, 0, 0)
	for _, address := range addresses {
		label := utf16(address.label)
		sendMessage.Call(uintptr(app.address), cbAddString, 0, uintptr(unsafe.Pointer(&label[0])))
		app.accessHosts = append(app.accessHosts, address.host)
	}
	if selected < 0 || selected >= len(app.accessHosts) {
		selected = 0
	}
	if len(app.accessHosts) > 0 {
		sendMessage.Call(uintptr(app.address), cbSetCurSel, uintptr(selected), 0)
	}
	sendMessage.Call(uintptr(app.address), cbSetDroppedWidth, 360, 0)
}

func availableAccessAddresses() ([]accessAddress, int) {
	var addresses []accessAddress
	seen := make(map[string]struct{})
	interfaces, _ := net.Interfaces()
	for _, iface := range interfaces {
		if iface.Flags&net.FlagUp == 0 {
			continue
		}
		assigned, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, address := range assigned {
			ip := ipFromAddress(address)
			if ip == nil || ip.IsUnspecified() || ip.IsMulticast() {
				continue
			}
			host := ip.String()
			if ip.To4() == nil && ip.IsLinkLocalUnicast() {
				host += "%" + iface.Name
			}
			if _, ok := seen[host]; ok {
				continue
			}
			seen[host] = struct{}{}
			addresses = append(addresses, accessAddress{
				host:  host,
				label: fmt.Sprintf("%s  (%s)", host, iface.Name),
				ip:    append(net.IP(nil), ip...),
			})
		}
	}
	if _, ok := seen["127.0.0.1"]; !ok {
		addresses = append(addresses, accessAddress{
			host:  "127.0.0.1",
			label: "127.0.0.1  (本机)",
			ip:    net.ParseIP("127.0.0.1"),
		})
	}
	return addresses, preferredAccessAddress(addresses)
}

func ipFromAddress(address net.Addr) net.IP {
	switch value := address.(type) {
	case *net.IPNet:
		return value.IP
	case *net.IPAddr:
		return value.IP
	}
	ip, _, err := net.ParseCIDR(address.String())
	if err != nil {
		return nil
	}
	return ip
}

func preferredAccessAddress(addresses []accessAddress) int {
	if outbound := outboundIPv4(); outbound != nil {
		for i := range addresses {
			if addresses[i].ip.Equal(outbound) {
				return i
			}
		}
	}
	for i := range addresses {
		if addresses[i].ip.To4() != nil && addresses[i].ip.IsPrivate() {
			return i
		}
	}
	for i := range addresses {
		if addresses[i].ip.To4() != nil && !addresses[i].ip.IsLoopback() && !addresses[i].ip.IsLinkLocalUnicast() {
			return i
		}
	}
	for i := range addresses {
		if addresses[i].ip.To4() == nil && addresses[i].ip.IsGlobalUnicast() && !addresses[i].ip.IsLoopback() {
			return i
		}
	}
	for i := range addresses {
		if addresses[i].ip.Equal(net.IPv4(127, 0, 0, 1)) {
			return i
		}
	}
	if len(addresses) > 0 {
		return 0
	}
	return -1
}

func outboundIPv4() net.IP {
	connection, err := net.Dial("udp4", "1.1.1.1:80")
	if err != nil {
		return nil
	}
	defer connection.Close()
	address, ok := connection.LocalAddr().(*net.UDPAddr)
	if !ok {
		return nil
	}
	return address.IP
}

func refreshRunningAddress() {
	if app == nil || !app.running || app.transitioning {
		return
	}
	port := app.runningPort
	if port == "" {
		port = getText(app.port)
	}
	app.runningAddress = serverAddress(port)
	app.statusAddress = app.runningAddress
	setText(app.status, "●  正在运行    "+app.runningAddress)
}

func advertisedServerAddress() string {
	if app != nil && app.running && app.runningAddress != "" {
		return app.runningAddress
	}
	return serverAddress(getText(app.port))
}

func copyStatusAddress() {
	if app == nil || !app.running || app.transitioning || app.statusAddress == "" {
		return
	}
	copyText(app.statusAddress)
}

func copyAddress() {
	copyText(advertisedServerAddress())
}

func copyText(address string) {
	if app == nil || app.hwnd == 0 {
		return
	}
	if err := setClipboardText(app.hwnd, address); err != nil {
		alert("复制地址失败: " + err.Error())
		return
	}
	if app.status == 0 {
		return
	}
	app.statusAddress = address
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

func toggleChatView() {
	app.chatMode = !app.chatMode
	setVisible(app.logs, !app.chatMode)
	setVisible(app.clearLog, !app.chatMode)
	for _, h := range []HWND{app.chatList, app.chatDetails, app.chatHistory, app.chatInput, app.chatSend} {
		setVisible(h, app.chatMode)
	}
	if app.chatMode {
		setText(app.logTitle, "聊天中心")
		setText(app.chatView, "查看日志")
		refreshChat()
	} else {
		setText(app.logTitle, "访问日志")
		setText(app.chatView, fmt.Sprintf("在线聊天 (%d)", app.srv.ChatOnlineCount()))
		refreshLog()
	}
}

func setVisible(h HWND, visible bool) {
	command := uintptr(0)
	if visible {
		command = swShow
	}
	showWindow.Call(uintptr(h), command)
}

func refreshChat() {
	if app == nil || app.chatView == 0 {
		return
	}
	if app.chatMode {
		refreshChatSnapshot(app.srv.ChatOverview())
		return
	}
	setText(app.chatView, fmt.Sprintf("在线聊天 (%d)", app.srv.ChatOnlineCount()))
}

func refreshChatSnapshot(items []server.ChatConversation) {
	if app == nil || app.chatView == 0 {
		return
	}
	if !app.chatMode {
		setText(app.chatView, fmt.Sprintf("在线聊天 (%d)", app.srv.ChatOnlineCount()))
		return
	}
	if i := selectedChatIndex(); i >= 0 && i < len(app.chatIDs) {
		app.chatSelected = app.chatIDs[i]
	}
	app.chatIDs = app.chatIDs[:0]
	sendMessage.Call(uintptr(app.chatList), lbResetContent, 0, 0)
	selectedIndex := -1
	for _, item := range items {
		status := "○"
		if item.Online {
			status = "●"
		}
		label := fmt.Sprintf(" %s  %s", status, item.Name)
		if item.ID != server.ChatGroupConversationID && strings.TrimSpace(item.Client.IP) != "" {
			label = fmt.Sprintf(" %s  %s · %s", status, item.Client.IP, item.Name)
		}
		if len(item.Messages) > 0 {
			label += fmt.Sprintf("  (%d)", len(item.Messages))
		}
		addChatList(label)
		app.chatIDs = append(app.chatIDs, item.ID)
		if item.ID == app.chatSelected {
			selectedIndex = len(app.chatIDs) - 1
		}
	}
	if selectedIndex < 0 && len(app.chatIDs) > 0 {
		selectedIndex = 0
		app.chatSelected = app.chatIDs[0]
	}
	if len(app.chatIDs) == 0 {
		app.chatSelected = ""
	}
	if selectedIndex >= 0 {
		sendMessage.Call(uintptr(app.chatList), lbSetCurSel, uintptr(selectedIndex), 0)
	}
	refreshChatHistory(items)
}

func handleChatUpdate() {
	if app == nil {
		return
	}
	items := app.srv.ChatOverview()
	notifyNewChatMessages(items)
	refreshChatSnapshot(items)
}

func rememberChatMessages(items []server.ChatConversation) {
	if app == nil {
		return
	}
	current := make(map[string]struct{})
	for _, conversation := range items {
		for _, message := range conversation.Messages {
			current[chatMessageKey(conversation.ID, message)] = struct{}{}
		}
	}
	app.seenChatMessages = current
	app.seenGroupMode = chatSnapshotIsGroup(items)
	app.seenModeKnown = true
}

func notifyNewChatMessages(items []server.ChatConversation) {
	groupMode := chatSnapshotIsGroup(items)
	if !app.seenModeKnown || app.seenGroupMode != groupMode {
		rememberChatMessages(items)
		return
	}
	current := make(map[string]struct{})
	newCount := 0
	latestSet := false
	latestNano := int64(0)
	latestName, latestSummary := "", ""
	for _, conversation := range items {
		for _, message := range conversation.Messages {
			key := chatMessageKey(conversation.ID, message)
			current[key] = struct{}{}
			_, seen := app.seenChatMessages[key]
			if seen || strings.EqualFold(message.Sender, "admin") {
				continue
			}
			newCount++
			name := message.Name
			if strings.TrimSpace(name) == "" {
				name = conversation.Name
			}
			nano := message.SentAt.UnixNano()
			if !latestSet || nano >= latestNano {
				latestSet = true
				latestNano = nano
				latestName = name
				latestSummary = chatMessageSummary(message)
			}
		}
	}
	app.seenChatMessages = current
	if !latestSet {
		return
	}
	if newCount > 1 {
		latestSummary += fmt.Sprintf("（另有 %d 条新消息）", newCount-1)
	}
	showChatNotification(latestName, latestSummary)
}

func chatSnapshotIsGroup(items []server.ChatConversation) bool {
	return len(items) == 1 && items[0].ID == server.ChatGroupConversationID
}

func chatMessageKey(conversationID string, message server.ChatMessage) string {
	if message.ID != "" {
		return conversationID + "\x1f" + message.ID
	}
	return fmt.Sprintf("%s\x1f%s\x1f%d\x1f%s\x1f%s",
		conversationID,
		message.Sender,
		message.SentAt.UnixNano(),
		message.Kind,
		message.Text,
	)
}

func chatMessageSummary(message server.ChatMessage) string {
	if strings.EqualFold(message.Kind, server.ChatMessageKindImage) {
		return "发来一张图片"
	}
	if strings.EqualFold(message.Kind, server.ChatMessageKindFile) {
		if strings.TrimSpace(message.FileName) != "" {
			return "发来文件：" + message.FileName
		}
		return "发来一个文件"
	}
	summary := strings.Join(strings.Fields(message.Text), " ")
	if summary == "" {
		return "发来一条消息"
	}
	runes := []rune(summary)
	if len(runes) > 100 {
		summary = string(runes[:100]) + "…"
	}
	return summary
}

func addChatList(value string) {
	u := utf16(sanitizeDisplay(value))
	sendMessage.Call(uintptr(app.chatList), lbAddString, 0, uintptr(unsafe.Pointer(&u[0])))
}

func selectedChatIndex() int {
	if app.chatList == 0 {
		return -1
	}
	r, _, _ := sendMessage.Call(uintptr(app.chatList), lbGetCurSel, 0, 0)
	if int32(r) == lbErr {
		return -1
	}
	return int(r)
}

func selectChatConversation() {
	i := selectedChatIndex()
	if i < 0 || i >= len(app.chatIDs) {
		return
	}
	app.chatSelected = app.chatIDs[i]
	refreshChatHistory(app.srv.ChatOverview())
}

func dismissSelectedChatVisitor() {
	index := selectedChatIndex()
	if index < 0 || index >= len(app.chatIDs) {
		return
	}
	clientID := app.chatIDs[index]
	if clientID == server.ChatGroupConversationID {
		alert("系统群会话不能移除；请先关闭“系统群”，再双击单个访客。")
		return
	}
	name := "此访客"
	for _, conversation := range app.srv.ChatOverview() {
		if conversation.ID == clientID {
			name = conversation.Name
			break
		}
	}
	title := utf16("移除访客")
	message := utf16(fmt.Sprintf(
		"确定从后台移除“%s”及其聊天记录吗？\r\n\r\n不会断开或封禁该访客；对方再次发送消息或重新连接后会重新出现。",
		name,
	))
	result, _, _ := messageBox.Call(
		uintptr(app.hwnd),
		uintptr(unsafe.Pointer(&message[0])),
		uintptr(unsafe.Pointer(&title[0])),
		0x134,
	)
	if result != 6 || !app.srv.RemoveChatVisitor(clientID) {
		return
	}
	app.chatProtocolVersion++
	app.chatSelected = ""
	setText(app.chatInput, "")
	items := app.srv.ChatOverview()
	notifyNewChatMessages(items)
	refreshChatSnapshot(items)
}

func refreshChatHistory(items []server.ChatConversation) {
	var selected *server.ChatConversation
	for i := range items {
		if items[i].ID == app.chatSelected {
			selected = &items[i]
			break
		}
	}
	if selected == nil {
		setText(app.chatDetails, "选择访客后可查看 IP、连接端口、浏览器和系统信息。")
		setChatHistoryConversation(nil)
		enableWindow.Call(uintptr(app.chatInput), 0)
		enableWindow.Call(uintptr(app.chatSend), 0)
		return
	}
	display := *selected
	if full, ok := app.srv.ChatConversationSnapshot(selected.ID); ok {
		display = full
	}
	setText(app.chatDetails, formatChatClientDetails(display))
	setChatHistoryConversation(&display)
	busy := chatImageBusy()
	canSend := display.Online && checked(app.chatEnabled) && !busy
	enableWindow.Call(uintptr(app.chatInput), boolFlag(canSend))
	enableWindow.Call(uintptr(app.chatSend), boolFlag(canSend))
	if busy {
		setText(app.chatSend, "处理中…")
	} else {
		setText(app.chatSend, "发送")
	}
}

func formatChatClientDetails(conversation server.ChatConversation) string {
	if conversation.ID == server.ChatGroupConversationID {
		return fmt.Sprintf("系统群会话\r\n当前在线访客：%d\r\n关闭“系统群”后可查看单个访客详情。\r\n操作：Enter 发送；Ctrl+V 或拖入 PNG/JPEG 图片", app.srv.ChatOnlineCount())
	}
	status := "离线"
	if conversation.Online {
		status = "在线"
	}
	ipAddress := strings.TrimSpace(conversation.Client.IP)
	if ipAddress == "" {
		ipAddress = "未知"
	}
	endpoint := ipAddress
	if conversation.Client.Port != "" {
		endpoint = net.JoinHostPort(ipAddress, conversation.Client.Port)
	}
	browser := strings.TrimSpace(conversation.Client.Browser)
	if browser == "" {
		browser = "未知"
	}
	system := strings.TrimSpace(conversation.Client.OS)
	if system == "" {
		system = "未知"
	}
	connected := "未知"
	if !conversation.Client.ConnectedAt.IsZero() {
		connected = conversation.Client.ConnectedAt.Local().Format("2006-01-02 15:04:05")
	}
	return fmt.Sprintf(
		"访客：%s    状态：%s\r\n连接：%s    最近连接：%s\r\n浏览器：%s    系统：%s\r\n操作：Enter 发送；Ctrl+V 或拖入 PNG/JPEG 图片",
		conversation.Name,
		status,
		endpoint,
		connected,
		browser,
		system,
	)
}

func sendChatMessage() {
	text := strings.TrimSpace(getText(app.chatInput))
	if text == "" {
		return
	}
	if app.chatSelected == "" {
		alert("请先选择一个在线访客")
		return
	}
	if err := app.srv.SendChatMessage(app.chatSelected, text); err != nil {
		alert("消息发送失败：" + err.Error())
		refreshChat()
		return
	}
	setText(app.chatInput, "")
	setFocus.Call(uintptr(app.chatInput))
	refreshChat()
}

func pasteChatImage() {
	if !clipboardContainsImage() {
		alert("剪贴板中没有可发送的图片")
		return
	}
	startChatImageTransfer(readClipboardChatImage)
}

func sendDroppedChatImage(imagePath string) {
	startChatImageTransfer(func() ([]byte, string, error) {
		return readDroppedChatImage(imagePath)
	})
}

func startChatImageTransfer(loader func() ([]byte, string, error)) {
	if app == nil || app.chatSelected == "" {
		alert("请先选择一个在线会话")
		return
	}
	if !checked(app.chatEnabled) {
		alert("聊天功能尚未启用")
		return
	}
	conversation, ok := app.srv.ChatConversationSnapshot(app.chatSelected)
	if !ok || !conversation.Online {
		alert("当前访客不在线，无法发送图片")
		return
	}
	app.chatImageMu.Lock()
	if app.chatImagePending {
		app.chatImageMu.Unlock()
		return
	}
	app.chatImagePending = true
	app.chatImageTarget = app.chatSelected
	app.chatImageProtocolVersion = app.chatProtocolVersion
	app.chatImageData = nil
	app.chatImageMime = ""
	app.chatImageErr = ""
	app.chatImageMu.Unlock()
	refreshChat()
	go func() {
		data, mimeType, err := loader()
		app.chatImageMu.Lock()
		app.chatImageData = data
		app.chatImageMime = mimeType
		if err != nil {
			app.chatImageErr = err.Error()
		}
		app.chatImageMu.Unlock()
		postMessage.Call(uintptr(app.hwnd), wmChatImage, 0, 0)
	}()
}

func finishChatImage() {
	if app == nil {
		return
	}
	app.chatImageMu.Lock()
	data := append([]byte(nil), app.chatImageData...)
	mimeType, target, message := app.chatImageMime, app.chatImageTarget, app.chatImageErr
	protocolVersion := app.chatImageProtocolVersion
	app.chatImageData = nil
	app.chatImageMime, app.chatImageTarget, app.chatImageErr = "", "", ""
	app.chatImagePending = false
	app.chatImageMu.Unlock()
	setText(app.chatSend, "发送")
	if message != "" {
		alert(message)
		refreshChat()
		return
	}
	if protocolVersion != app.chatProtocolVersion || app.srv.GroupChatEnabled() != (target == server.ChatGroupConversationID) {
		alert("会话已切换或被移除，本次图片发送已取消")
		refreshChat()
		return
	}
	if err := app.srv.SendChatImage(target, mimeType, data); err != nil {
		alert("图片发送失败：" + err.Error())
	}
	refreshChat()
}

func chatImageBusy() bool {
	if app == nil {
		return false
	}
	app.chatImageMu.Lock()
	defer app.chatImageMu.Unlock()
	return app.chatImagePending
}

func boolFlag(value bool) uintptr {
	if value {
		return 1
	}
	return 0
}

func subclassChatInput() {
	if app == nil || app.chatInput == 0 || originalChatInputProc != 0 {
		return
	}
	previous, _, _ := setWindowLongPtr.Call(uintptr(app.chatInput), ^uintptr(3), chatInputCallback)
	if previous != 0 {
		originalChatInputProc = previous
	}
}

func chatInputWindowProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case wmIMEStart:
		chatInputComposing = true
	case wmIMEEnd, wmKillFocus:
		chatInputComposing = false
	}
	if message == wmPaste && clipboardContainsImage() {
		pasteChatImage()
		return 0
	}
	if wParam == vkReturn && !chatInputComposing {
		switch message {
		case wmKeyDown:
			sendChatMessage()
			return 0
		case wmChar:
			return 0
		}
	}
	if originalChatInputProc != 0 {
		result, _, _ := callWindowProc.Call(originalChatInputProc, hwnd, uintptr(message), wParam, lParam)
		return result
	}
	result, _, _ := defWindowProc.Call(hwnd, uintptr(message), wParam, lParam)
	return result
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
	u := utf16(sanitizeDisplay(s))
	setWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&u[0])))
}
func getText(h HWND) string {
	n, _, _ := getWindowTextLen.Call(uintptr(h))
	b := make([]uint16, n+1)
	getWindowText.Call(uintptr(h), uintptr(unsafe.Pointer(&b[0])), n+1)
	return syscall.UTF16ToString(b)
}
func alert(s string) {
	t, m := utf16("LanChatGo"), utf16(s)
	messageBox.Call(uintptr(app.hwnd), uintptr(unsafe.Pointer(&m[0])), uintptr(unsafe.Pointer(&t[0])), 0x10)
}
func utf16(s string) []uint16 { return syscall.StringToUTF16(sanitizeDisplay(s)) }

func utf16WithNULs(s string) []uint16 { return unicodeutf16.Encode([]rune(s)) }

func sanitizeDisplay(s string) string { return strings.ReplaceAll(s, "\x00", "�") }

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
	app.srv.SetChatEnabled(checked(app.chatEnabled))
	groupEnabled := checked(app.chatGroup)
	if groupEnabled != app.srv.GroupChatEnabled() {
		app.chatProtocolVersion++
	}
	app.srv.SetGroupChatEnabled(groupEnabled)
}

func trayData() notifyIconData {
	instance, _, _ := getModule.Call(0)
	icon, _, _ := loadIcon.Call(instance, modernIconResourceID)
	if icon == 0 {
		icon, _, _ = loadIcon.Call(0, 32512)
	}
	n := notifyIconData{Hwnd: uintptr(app.hwnd), UID: 1, Flags: nifMessage | nifIcon | nifTip | nifShowTip, Callback: wmTray, Icon: icon}
	n.Size = uint32(unsafe.Sizeof(n))
	copyUTF16(n.Tip[:], "LanChatGo - 双击显示，右击打开菜单")
	return n
}

func minimizeToTray() {
	ensureTray()
	if app.trayAdded {
		showWindow.Call(uintptr(app.hwnd), 0)
		return
	}
	showWindow.Call(uintptr(app.hwnd), swMinimize)
}

func restoreFromTray() {
	if app == nil || app.hwnd == 0 {
		return
	}
	showWindow.Call(uintptr(app.hwnd), 9)
	setForeground.Call(uintptr(app.hwnd))
	setFocus.Call(uintptr(app.hwnd))
}

func ensureTray() {
	if app == nil || app.hwnd == 0 || app.trayAdded {
		return
	}
	n := trayData()
	app.trayAdded = registerNotifyIcon(n)
}

func showChatNotification(name, summary string) {
	if app == nil || !app.notifyNewMessage {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "访客"
	}
	showTrayNotification("新聊天消息 · "+name, summary)
}

func handleVisitorNotifications() {
	if app == nil {
		return
	}
	app.visitorMu.Lock()
	visitors := append([]server.ChatClientInfo(nil), app.pendingVisitors...)
	app.pendingVisitors = app.pendingVisitors[:0]
	app.visitorPostPending = false
	app.visitorMu.Unlock()
	if len(visitors) == 0 {
		return
	}
	if !app.notifyNewVisitor {
		return
	}
	latest := visitors[len(visitors)-1]
	address := strings.TrimSpace(latest.IP)
	if address == "" {
		address = "未知地址"
	}
	if latest.Port != "" {
		address = net.JoinHostPort(address, latest.Port)
	}
	details := []string{address}
	if latest.Browser != "" && latest.Browser != "未知" {
		details = append(details, latest.Browser)
	}
	if latest.OS != "" && latest.OS != "未知" {
		details = append(details, latest.OS)
	}
	summary := strings.Join(details, " · ")
	if len(visitors) > 1 {
		summary += fmt.Sprintf("（另有 %d 位新访客）", len(visitors)-1)
	}
	showTrayNotification("新用户访问", summary)
}

func showTrayNotification(title, summary string) {
	ensureTray()
	messageBeep.Call(0x40)
	if !app.trayAdded {
		return
	}
	n := trayNotificationData(trayData(), title, summary, niifInfo|niifNoSound, 0)
	if showNotifyIconNotification(n) {
		return
	}
	stale := trayData()
	shellNotify.Call(nimDelete, uintptr(unsafe.Pointer(&stale)))
	app.trayAdded = false
	ensureTray()
	if app.trayAdded {
		n = trayNotificationData(trayData(), title, summary, niifInfo|niifNoSound, 0)
		showNotifyIconNotification(n)
	}
}

func registerNotifyIcon(data notifyIconData) bool {
	added, _, _ := shellNotify.Call(nimAdd, uintptr(unsafe.Pointer(&data)))
	if added == 0 {
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
	base.Flags |= nifInfo
	base.InfoFlags = infoFlags
	base.BalloonIcon = balloonIcon
	base.Timeout = 10000
	title = strings.Join(strings.Fields(title), " ")
	body = strings.Join(strings.Fields(body), " ")
	if title == "" {
		title = "LanChatGo"
	}
	copyUTF16(base.InfoTitle[:], title)
	copyUTF16(base.Info[:], body)
	return base
}

func showNotifyIconNotification(data notifyIconData) bool {
	shown, _, _ := shellNotify.Call(nimModify, uintptr(unsafe.Pointer(&data)))
	return shown != 0
}

func copyUTF16(destination []uint16, value string) {
	if len(destination) == 0 {
		return
	}
	encoded := utf16(value)
	if len(encoded) > len(destination) {
		encoded = encoded[:len(destination)]
		encoded[len(encoded)-1] = 0
		if len(encoded) >= 2 && encoded[len(encoded)-2] >= 0xD800 && encoded[len(encoded)-2] <= 0xDBFF {
			encoded[len(encoded)-2] = 0
		}
	}
	copy(destination, encoded)
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
	postMessage.Call(uintptr(app.hwnd), 0, 0, 0)
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
	if app == nil || app.hwnd == 0 || !app.trayAdded {
		return
	}
	n := trayData()
	shellNotify.Call(2, uintptr(unsafe.Pointer(&n)))
	app.trayAdded = false
}

func addFileDialog() {
	paths, err := chooseFilePaths(app.hwnd)
	if err != nil {
		alert(err.Error())
		return
	}
	for _, selectedPath := range paths {
		addPath(selectedPath)
	}
}

func chooseFilePaths(owner HWND) ([]string, error) {
	buf := make([]uint16, 32768)
	filter := utf16WithNULs("所有文件\x00*.*\x00\x00")
	title := utf16("选择要分享的文件")
	o := openFileName{
		Size:    uint32(unsafe.Sizeof(openFileName{})),
		Owner:   uintptr(owner),
		Filter:  uintptr(unsafe.Pointer(&filter[0])),
		File:    uintptr(unsafe.Pointer(&buf[0])),
		MaxFile: uint32(len(buf)),
		Title:   uintptr(unsafe.Pointer(&title[0])),
		Flags:   0x00080000 | 0x00000200 | 0x00000800 | 0x00001000 | 0x00000008,
	}
	if result, _, _ := getOpenFile.Call(uintptr(unsafe.Pointer(&o))); result == 0 {
		code, _, _ := getOpenFileError.Call()
		if code != 0 {
			return nil, fmt.Errorf("选择文件失败（错误代码 %d）", code)
		}
		return nil, nil
	}
	return parseOpenFilePaths(buf), nil
}

func parseOpenFilePaths(buffer []uint16) []string {
	parts := make([]string, 0, 4)
	for start := 0; start < len(buffer) && buffer[start] != 0; {
		end := start
		for end < len(buffer) && buffer[end] != 0 {
			end++
		}
		parts = append(parts, syscall.UTF16ToString(buffer[start:end]))
		start = end + 1
	}
	if len(parts) <= 1 {
		return parts
	}
	directory := parts[0]
	paths := make([]string, 0, len(parts)-1)
	for _, name := range parts[1:] {
		paths = append(paths, filepath.Join(directory, name))
	}
	return paths
}
func addFolderDialog() {
	selectedPath, err := chooseFolderPath(app.hwnd)
	if err != nil {
		alert(err.Error())
		return
	}
	if selectedPath != "" {
		addPath(selectedPath)
	}
}

func chooseFolderPath(owner HWND) (string, error) {
	display := make([]uint16, 260)
	title := utf16("选择要分享的文件夹")
	bi := browseInfo{Owner: uintptr(owner), DisplayName: uintptr(unsafe.Pointer(&display[0])), Title: uintptr(unsafe.Pointer(&title[0])), Flags: 0x40 | 1}
	pid, _, _ := browseFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pid == 0 {
		return "", nil
	}
	defer coTaskFree.Call(pid)
	buf := make([]uint16, 32768)
	if ok, _, _ := pathFromID.Call(pid, uintptr(unsafe.Pointer(&buf[0]))); ok != 0 {
		return syscall.UTF16ToString(buf), nil
	}
	return "", errors.New("无法读取所选文件夹路径")
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
