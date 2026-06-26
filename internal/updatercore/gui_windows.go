//go:build windows

package updatercore

import (
	"sync"
	"syscall"
	"unsafe"
)

// Windows 11 Light Theme — 匹配 updater_gui.html 参考设计
const (
	clrWindowBg      = 0x00F3F3F3 // #F3F3F3 窗口背景 (BGR)
	clrSurfaceBg     = 0x00FFFFFF // #FFFFFF 分组框/控件背景 (BGR)
	clrSurfaceAltBg  = 0x00FFFFFF // #FFFFFF 输入框背景 (BGR)
	clrSurfaceBorder = 0x00B0B0B0 // #B0B0B0 边框色 (BGR)
	clrTextPrimary   = 0x00000000 // #000000 主文字 (BGR)
	clrTextSecondary = 0x0063554B // #4B5563 次要文字 (BGR)
	clrTextMuted     = 0x008C8C8C // #8C8C8C 弱化文字 (BGR)
	clrProgressBar   = 0x00C06700 // #0067C0 强调蓝 (BGR)
	clrProgressBg    = 0x00E6E6E6 // #E6E6E6 进度条底色 (BGR)
	clrErrorBg       = 0x00E9E7FD // #FDE7E9 错误提示背景 (BGR)
	clrErrorText     = 0x001C1CA8 // #A81C20 错误提示文字 (BGR)
)

// Win32 message constants
const (
	WM_CREATE           = 0x0001
	WM_DESTROY          = 0x0002
	WM_SIZE             = 0x0005
	WM_PAINT            = 0x000F
	WM_CLOSE            = 0x0010
	WM_COMMAND          = 0x0111
	WM_ERASEBKGND       = 0x0014
	WM_WINDOWPOSCHANGED = 0x0047
	WM_NCPAINT          = 0x0085
	WM_CTLCOLORSTATIC   = 0x0138
	WM_CTLCOLOREDIT     = 0x0133
	WM_CTLCOLORBTN      = 0x0135
	WM_GETMINMAXINFO    = 0x0024
	WM_DPICHANGED       = 0x02E0
	WM_PRINTCLIENT      = 0x0318
	WM_APP              = 0x8000
	WM_SETICON          = 0x0080
)

// Custom messages for worker -> UI communication
const (
	wmAppProgress  = WM_APP + 1
	wmAppInfo      = WM_APP + 2
	wmAppError     = WM_APP + 3
	wmAppPlan      = WM_APP + 4
	wmAppLocked    = WM_APP + 5
	wmAppDone      = WM_APP + 6
	wmAppVersion   = WM_APP + 7
	wmAppStartWork = WM_APP + 8
)

// System commands and styles
const (
	SC_CLOSE           = 0xF060
	wsOverlappedWindow = 0x00CF0000
	// 固定大小窗口：保留标题栏、关闭/最小化按钮，禁止拖拽缩放
	wsFixedWindow   = 0x00CEF000 // wsOverlappedWindow 去掉 WS_SIZEBOX(0x40000)，同时去掉 WS_MAXIMIZEBOX
	wsChild         = 0x40000000
	wsVisible       = 0x10000000
	wsVScroll       = 0x00200000
	wsBorder        = 0x00800000
	ssLeft          = 0x00000000
	ssCenter        = 0x00000001
	ssNotify        = 0x00000100
	esMultiline     = 0x00000004
	esReadOnly      = 0x0800
	esAutoVScroll   = 0x0040
	pbsSmooth       = 0x00000001
	pbmSetRange32   = 0x0406
	pbmSetPos       = 0x0402
	pbmSetBarColor  = 0x0409
	pbmSetBkColor   = 0x2001
	bmSetStyle      = 0x00F4
	swShow          = 5
	swHide          = 0
	csHRedraw       = 0x0002
	csVRedraw       = 0x0001
	colorWindow     = 5
	colorBtnFace    = 15
	idcArrow        = 32512
	idiApplication  = 32512
	ICON_SMALL      = 0
	ICON_BIG        = 1
	IMAGE_ICON      = 1
	LR_SHARED       = 0x00008000
	bsGroupbox      = 0x0007
	bsDefPushbutton = 0x0001
	bsPushbutton    = 0x0000
)

// Edit control messages
const (
	wmGetTextLength = 0x000E
	emSetSel        = 0x00B1
	emReplaceSel    = 0x00C2
)

// Control IDs
const (
	idcStart        = 1001
	idcCancel       = 1002
	idcKillProc     = 1003
	idcCloseBtn     = 1004
	idcRecheck      = 1005
	idcVersionLbl   = 1010
	idcLatestLbl    = 1011
	idcCountsLbl    = 1012
	idcSizeLbl      = 1013
	idcFileLbl      = 1014
	idcSpeedLbl     = 1015
	idcEtaLbl       = 1016
	idcErrorEdit    = 1017
	idcProgress     = 1018
	idcStatusLbl    = 1019
	idcVerGroupBox  = 1020
	idcProgGroupBox = 1021
)

// Font and DPI related constants — 匹配参考设计 15px/500weight 主文本, 14px/600weight 标题按钮
const (
	WM_SETFONT             = 0x0030
	defaultFontSize        = 12 // 参考基准 (lfHeight ≈ -16)
	fontWeightNormal       = 400
	fontWeightMedium       = 500 // 主文本: info-row font-weight: 500
	fontWeightSemibold     = 600 // 标题/按钮: font-weight: 600
	ffDontCare             = 0x00
	defaultGuiFont         = 0x11
	spiGetNonClientMetrics = 0x0029
	transparentBkMode      = 1 // TRANSPARENT — 避免静态文字后出现填充块
	opaqueBkMode           = 2 // OPAQUE — 仅用于编辑框等需要实底的控件
	defaultCharset         = 1 // DEFAULT_CHARSET
)

// Dialog layout metrics shared by window creation and control layout.
const (
	dialogWindowWidth  = 620
	dialogWindowHeight = 760
	dialogMinWidth     = 620
	dialogMinHeight    = 760
)

// Win32 types
type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

type point struct {
	X, Y int32
}

type minMaxInfo struct {
	PtReserved     point
	PtMaxSize      point
	PtMaxPosition  point
	PtMinTrackSize point
	PtMaxTrackSize point
}

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// logFont is used by CreateFontW via SystemParametersInfoW(SPI_GETNONCLIENTMETRICS).
type logFont struct {
	LfHeight         int32
	LfWidth          int32
	LfEscapement     int32
	LfOrientation    int32
	LfWeight         int32
	LfItalic         byte
	LfUnderline      byte
	LfStrikeOut      byte
	LfCharSet        byte
	LfOutPrecision   byte
	LfClipPrecision  byte
	LfQuality        byte
	LfPitchAndFamily byte
	LfFaceName       [32]uint16
}

// Win32 API procedures
var (
	user32                    = syscall.NewLazyDLL("user32.dll")
	kernel32                  = syscall.NewLazyDLL("kernel32.dll")
	gdi32                     = syscall.NewLazyDLL("gdi32.dll")
	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procShowWindow            = user32.NewProc("ShowWindow")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostMessageW          = user32.NewProc("PostMessageW")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procEnableWindow          = user32.NewProc("EnableWindow")
	procSetWindowTextW        = user32.NewProc("SetWindowTextW")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procLoadCursorW           = user32.NewProc("LoadCursorW")
	procLoadIconW             = user32.NewProc("LoadIconW")
	procLoadImageW            = user32.NewProc("LoadImageW")
	procUpdateWindow          = user32.NewProc("UpdateWindow")
	procGetDpiForWindow       = user32.NewProc("GetDpiForWindow")
	procSetWindowPos          = user32.NewProc("SetWindowPos")
	procMessageBoxW           = user32.NewProc("MessageBoxW")
	procSetProcessDPIAware    = user32.NewProc("SetProcessDPIAware")
	procCreateFontW           = gdi32.NewProc("CreateFontW")
	procDeleteObject          = gdi32.NewProc("DeleteObject")
	procSystemParametersInfoW = user32.NewProc("SystemParametersInfoW")
	procSetWindowTheme        = syscall.NewLazyDLL("uxtheme.dll").NewProc("SetWindowTheme")
	procCreateSolidBrush      = gdi32.NewProc("CreateSolidBrush")
	procSetBkMode             = gdi32.NewProc("SetBkMode")
	procSetTextColor          = gdi32.NewProc("SetTextColor")
	procSetBkColor            = gdi32.NewProc("SetBkColor")
	procInvalidateRect        = user32.NewProc("InvalidateRect")
	procFillRect              = user32.NewProc("FillRect")
)

// Global state for window proc (single-instance application)
var (
	globalDialogUI    *DialogUI
	globalDialogMu    sync.Mutex
	dialogProcCB      = syscall.NewCallback(dialogProc)
	registerClassOnce sync.Once
	classNamePtr      = syscall.StringToUTF16Ptr("UgeminiUpdaterDialog")
	windowBrush       = mustCreateSolidBrush(clrWindowBg)
	surfaceBrush      = mustCreateSolidBrush(clrSurfaceBg)
	surfaceAltBrush   = mustCreateSolidBrush(clrSurfaceAltBg)
	errorBrush        = mustCreateSolidBrush(clrErrorBg)
)

// dialogProc is the window procedure called by Windows.
func dialogProc(hwnd uintptr, msg uint32, wParam uintptr, lParam uintptr) uintptr {
	globalDialogMu.Lock()
	ui := globalDialogUI
	globalDialogMu.Unlock()
	if ui == nil {
		ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
		return ret
	}
	return ui.handleMessage(hwnd, msg, wParam, lParam)
}

// Win32 helper functions

func registerDialogClass() {
	registerClassOnce.Do(func() {
		procSetProcessDPIAware.Call()

		hInst, _, _ := procGetModuleHandleW.Call(0)
		hCursor, _, _ := procLoadCursorW.Call(0, uintptr(idcArrow))
		hIcon, _, _ := procLoadIconW.Call(0, uintptr(idiApplication))
		wc := wndClassEx{
			CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
			Style:         csHRedraw | csVRedraw,
			LpfnWndProc:   dialogProcCB,
			HInstance:     hInst,
			HIcon:         hIcon,
			HCursor:       hCursor,
			HbrBackground: windowBrush,
			LpszClassName: classNamePtr,
		}
		procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))
	})
}

func createModernFont() uintptr {
	return createUIFont(fontWeightMedium, -16)
}

func createUIFont(weight int32, height int32) uintptr {
	var lf logFont
	procSystemParametersInfoW.Call(uintptr(spiGetNonClientMetrics), unsafe.Sizeof(lf), uintptr(unsafe.Pointer(&lf)), 0)
	lf.LfHeight = height
	lf.LfWeight = weight
	lf.LfCharSet = byte(defaultCharset)
	faceName, _ := syscall.UTF16FromString("Segoe UI")
	for i, r := range faceName {
		if i >= len(lf.LfFaceName) {
			break
		}
		lf.LfFaceName[i] = r
		if r == 0 {
			break
		}
	}
	font, _, _ := procCreateFontW.Call(
		uintptr(lf.LfHeight),
		uintptr(lf.LfWidth),
		uintptr(lf.LfEscapement),
		uintptr(lf.LfOrientation),
		uintptr(lf.LfWeight),
		uintptr(lf.LfItalic),
		uintptr(lf.LfUnderline),
		uintptr(lf.LfStrikeOut),
		uintptr(lf.LfCharSet),
		uintptr(lf.LfOutPrecision),
		uintptr(lf.LfClipPrecision),
		uintptr(lf.LfQuality),
		uintptr(lf.LfPitchAndFamily),
		uintptr(unsafe.Pointer(&lf.LfFaceName[0])),
	)
	return font
}

func applyFont(hwnd uintptr, font uintptr) {
	procSendMessageW.Call(hwnd, uintptr(WM_SETFONT), font, 1)
}

func applyExplorerTheme(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	subApp, _ := syscall.UTF16PtrFromString("Explorer")
	procSetWindowTheme.Call(hwnd, uintptr(unsafe.Pointer(subApp)), 0)
}

func setButtonStyle(hwnd uintptr, style uintptr) {
	if hwnd == 0 {
		return
	}
	procSendMessageW.Call(hwnd, uintptr(bmSetStyle), style, 1)
}

func styleProgressBar(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	// 移除视觉主题，使 PBM_SETBARCOLOR / PBM_SETBKCOLOR 生效
	// 否则 Win11 主题会强制使用系统蓝色，忽略自定义颜色
	emptyStr, _ := syscall.UTF16PtrFromString("")
	procSetWindowTheme.Call(hwnd, uintptr(unsafe.Pointer(emptyStr)), uintptr(unsafe.Pointer(emptyStr)))
	procSendMessageW.Call(hwnd, uintptr(pbmSetBkColor), 0, uintptr(clrProgressBg))
	procSendMessageW.Call(hwnd, uintptr(pbmSetBarColor), 0, uintptr(clrProgressBar))
}

func styleGroupBox(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	applyExplorerTheme(hwnd)
}

func styleButton(hwnd uintptr, primary bool) {
	if hwnd == 0 {
		return
	}
	applyExplorerTheme(hwnd)
	if primary {
		setButtonStyle(hwnd, bsDefPushbutton)
		return
	}
	setButtonStyle(hwnd, bsPushbutton)
}

func createMainWindow(title string) uintptr {
	registerDialogClass()
	hInst, _, _ := procGetModuleHandleW.Call(0)
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classNamePtr)),
		uintptr(unsafe.Pointer(titlePtr)),
		uintptr(wsFixedWindow),
		120, 120, dialogWindowWidth, dialogWindowHeight,
		0, 0, hInst, 0,
	)
	// 从 exe 资源加载自定义图标并设置到窗口标题栏和任务栏
	if hwnd != 0 {
		hIcon, _, _ := procLoadImageW.Call(hInst, 1, IMAGE_ICON, 0, 0, LR_SHARED)
		if hIcon != 0 {
			procSendMessageW.Call(hwnd, WM_SETICON, ICON_BIG, hIcon)
			procSendMessageW.Call(hwnd, WM_SETICON, ICON_SMALL, hIcon)
		}
	}
	return hwnd
}

func createControl(parent uintptr, class string, style uintptr, id int, x, y, w, h int32) uintptr {
	classPtr, _ := syscall.UTF16PtrFromString(class)
	hInst, _, _ := procGetModuleHandleW.Call(0)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(classPtr)),
		0,
		uintptr(wsChild|wsVisible|style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), hInst, 0,
	)
	return hwnd
}

func showCtrl(hwnd uintptr, show int) {
	procShowWindow.Call(hwnd, uintptr(show))
}

func enableCtrl(hwnd uintptr, enable bool) {
	var v uintptr
	if enable {
		v = 1
	}
	procEnableWindow.Call(hwnd, v)
}

func setCtrlText(hwnd uintptr, text string) {
	ptr, _ := syscall.UTF16PtrFromString(text)
	procSetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(ptr)))
}

func postMsg(hwnd uintptr, msg uint32) {
	procPostMessageW.Call(hwnd, uintptr(msg), 0, 0)
}

func sendMsg(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	ret, _, _ := procSendMessageW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func mustCreateSolidBrush(color uintptr) uintptr {
	brush, _, _ := procCreateSolidBrush.Call(color)
	return brush
}

// setupStaticTextColor uses TRANSPARENT text background so glyphs blend like the HTML mock.
func setupStaticTextColor(hdc uintptr, textColor uintptr) {
	procSetBkMode.Call(hdc, transparentBkMode)
	procSetTextColor.Call(hdc, textColor)
}

// setupOpaqueTextColor is for controls that should paint a solid text background, like EDIT.
func setupOpaqueTextColor(hdc uintptr, textColor uintptr, bkColor uintptr) {
	procSetBkMode.Call(hdc, opaqueBkMode)
	procSetBkColor.Call(hdc, bkColor)
	procSetTextColor.Call(hdc, textColor)
}

var destroyWin = func(hwnd uintptr) {
	procDestroyWindow.Call(hwnd)
}

var postQuit = func(code int32) {
	procPostQuitMessage.Call(uintptr(code))
}

func defWinProc(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(msg), wParam, lParam)
	return ret
}

func getMessage(m *msg) int32 {
	ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(m)), 0, 0, 0)
	return int32(ret)
}

func translateMsg(m *msg) {
	procTranslateMessage.Call(uintptr(unsafe.Pointer(m)))
}

func dispatchMsg(m *msg) {
	procDispatchMessageW.Call(uintptr(unsafe.Pointer(m)))
}
