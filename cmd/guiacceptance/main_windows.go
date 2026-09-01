//go:build windows && amd64

package main

import (
	"errors"
	"flag"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"
)

const (
	updaterWindowClass = "UgeminiUpdaterDialog"

	inputMouse    = 0
	inputKeyboard = 1

	keyeventfKeyUp = 0x0002

	mouseeventfLeftDown = 0x0002
	mouseeventfLeftUp   = 0x0004

	vkEscape = 0x1B

	swRestore = 9

	bsGroupBox = 0x00000007

	cfUnicodeText = 13
	biRGB         = 0
	dibRGBColors  = 0

	srcCopy    = 0x00CC0020
	captureBLT = 0x40000000
)

var (
	user32   = syscall.NewLazyDLL("user32.dll")
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")
	dwmapi   = syscall.NewLazyDLL("dwmapi.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procEnumChildWindows         = user32.NewProc("EnumChildWindows")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowTextLengthW     = user32.NewProc("GetWindowTextLengthW")
	procGetWindowTextW           = user32.NewProc("GetWindowTextW")
	procIsWindowVisible          = user32.NewProc("IsWindowVisible")
	procIsWindowEnabled          = user32.NewProc("IsWindowEnabled")
	procGetWindowRect            = user32.NewProc("GetWindowRect")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procSetForegroundWindow      = user32.NewProc("SetForegroundWindow")
	procGetForegroundWindow      = user32.NewProc("GetForegroundWindow")
	procBringWindowToTop         = user32.NewProc("BringWindowToTop")
	procSetActiveWindow          = user32.NewProc("SetActiveWindow")
	procSetFocus                 = user32.NewProc("SetFocus")
	procAttachThreadInput        = user32.NewProc("AttachThreadInput")
	procShowWindow               = user32.NewProc("ShowWindow")
	procSetCursorPos             = user32.NewProc("SetCursorPos")
	procSendInput                = user32.NewProc("SendInput")
	procGetDlgCtrlID             = user32.NewProc("GetDlgCtrlID")
	procGetWindowLongW           = user32.NewProc("GetWindowLongW")
	procSetProcessDPIAware       = user32.NewProc("SetProcessDPIAware")
	procGetDC                    = user32.NewProc("GetDC")
	procReleaseDC                = user32.NewProc("ReleaseDC")
	procOpenClipboard            = user32.NewProc("OpenClipboard")
	procCloseClipboard           = user32.NewProc("CloseClipboard")
	procGetClipboardData         = user32.NewProc("GetClipboardData")

	procGlobalLock         = kernel32.NewProc("GlobalLock")
	procGlobalUnlock       = kernel32.NewProc("GlobalUnlock")
	procGlobalSize         = kernel32.NewProc("GlobalSize")
	procGetCurrentThreadID = kernel32.NewProc("GetCurrentThreadId")

	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procCreateCompatibleBitmap = gdi32.NewProc("CreateCompatibleBitmap")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procBitBlt                 = gdi32.NewProc("BitBlt")
	procGetDIBits              = gdi32.NewProc("GetDIBits")

	procDwmFlush = dwmapi.NewProc("DwmFlush")
)

type rect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

func (r rect) width() int32  { return r.Right - r.Left }
func (r rect) height() int32 { return r.Bottom - r.Top }

type control struct {
	HWND    uintptr
	ID      int32
	Class   string
	Text    string
	Rect    rect
	Style   uint32
	Visible bool
	Enabled bool
}

type keyboardInput struct {
	Vk        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

type mouseInput struct {
	Dx        int32
	Dy        int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	ExtraInfo uintptr
}

// INPUT is 40 bytes on windows/amd64: DWORD type + compiler padding + 32-byte union.
// uintptr keeps the union 8-byte aligned, matching Win64 INPUT/MOUSEINPUT ABI.
type input struct {
	Type uint32
	Data [4]uintptr
}

type bitmapInfoHeader struct {
	Size          uint32
	Width         int32
	Height        int32
	Planes        uint16
	BitCount      uint16
	Compression   uint32
	SizeImage     uint32
	XPelsPerMeter int32
	YPelsPerMeter int32
	ClrUsed       uint32
	ClrImportant  uint32
}

type rgbQuad struct {
	Blue     byte
	Green    byte
	Red      byte
	Reserved byte
}

type bitmapInfo struct {
	Header bitmapInfoHeader
	Colors [1]rgbQuad
}

func main() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "guiacceptance:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		pid              = flag.Uint("pid", 0, "Updater.exe PID; omit when exactly one updater window exists")
		wait             = flag.Duration("wait", 10*time.Second, "how long to wait for the updater window")
		settle           = flag.Duration("settle", 250*time.Millisecond, "render/input settle delay")
		list             = flag.Bool("list", false, "list visible and hidden child controls with rectangles")
		checkOverlap     = flag.Bool("check-overlap", false, "fail when visible non-group-box buttons overlap")
		screenshot       = flag.String("screenshot", "", "capture the actual on-screen updater window to this PNG path")
		clickText        = flag.String("click", "", "real-mouse click a visible control by caption text")
		focusClass       = flag.String("focus-class", "", "real-mouse click a visible control by Win32 class name")
		index            = flag.Int("index", 0, "zero-based match index for -focus-class")
		key              = flag.String("key", "", "real keyboard key: R, S, A, Esc")
		repeat           = flag.Int("repeat", 1, "number of full key presses for -key")
		autoRepeat       = flag.Int("auto-repeat", 0, "additional key-down events before key-up, emulating typematic repeat")
		interval         = flag.Duration("interval", 80*time.Millisecond, "delay between repeated key events")
		clipboard        = flag.Bool("clipboard", false, "print current CF_UNICODETEXT clipboard text")
		expectClipboard  = flag.String("expect-clipboard", "", "pipe-separated substrings that must exist in CF_UNICODETEXT")
		expectVisible    = flag.String("expect-visible", "", "fail unless a visible child control contains this text")
		expectNotVisible = flag.String("expect-not-visible", "", "fail if any visible child control contains this text")
	)
	flag.Parse()

	if *index < 0 {
		return errors.New("-index must be >= 0")
	}
	if *repeat < 1 {
		return errors.New("-repeat must be >= 1")
	}
	if *autoRepeat < 0 {
		return errors.New("-auto-repeat must be >= 0")
	}
	if *autoRepeat > 0 && *repeat != 1 {
		return errors.New("-auto-repeat cannot be combined with -repeat other than its default value 1")
	}

	procSetProcessDPIAware.Call()

	hwnd, err := waitForUpdaterWindow(uint32(*pid), *wait)
	if err != nil {
		return err
	}

	controls, err := enumControls(hwnd)
	if err != nil {
		return err
	}

	if *expectVisible != "" && !visibleControlContains(controls, *expectVisible) {
		return fmt.Errorf("visible control containing %q not found", *expectVisible)
	}
	if *expectNotVisible != "" && visibleControlContains(controls, *expectNotVisible) {
		return fmt.Errorf("unexpected visible control containing %q", *expectNotVisible)
	}

	if *list {
		printControls(hwnd, controls)
	}

	var overlaps [][2]control
	if *checkOverlap {
		overlaps = findButtonOverlaps(controls)
		if len(overlaps) == 0 {
			fmt.Println("button-overlap: PASS")
		} else {
			fmt.Printf("button-overlap: FAIL (%d collision(s))\n", len(overlaps))
			for _, pair := range overlaps {
				fmt.Printf("  %q id=%d %s overlaps %q id=%d %s\n",
					pair[0].Text, pair[0].ID, formatRect(pair[0].Rect),
					pair[1].Text, pair[1].ID, formatRect(pair[1].Rect))
			}
		}
	}

	needsForeground := *clickText != "" || *focusClass != "" || *key != ""
	if needsForeground {
		if err := ensureForeground(hwnd, 3*time.Second); err != nil {
			return err
		}
	}

	if *focusClass != "" {
		match, err := findControlByClass(controls, *focusClass, *index)
		if err != nil {
			return err
		}
		if err := realClick(match.Rect); err != nil {
			return fmt.Errorf("focus-class %q: %w", *focusClass, err)
		}
		time.Sleep(*settle)
	}

	if *clickText != "" {
		match, err := findControlByText(controls, *clickText)
		if err != nil {
			return err
		}
		if err := realClick(match.Rect); err != nil {
			return fmt.Errorf("click %q: %w", *clickText, err)
		}
		time.Sleep(*settle)
	}

	if *key != "" {
		vk, err := parseVirtualKey(*key)
		if err != nil {
			return err
		}
		if *autoRepeat > 0 {
			if err := realAutoRepeat(vk, *autoRepeat, *interval); err != nil {
				return err
			}
		} else {
			for i := 0; i < *repeat; i++ {
				if err := realKeyPress(vk); err != nil {
					return err
				}
				if i+1 < *repeat {
					time.Sleep(*interval)
				}
			}
		}
		time.Sleep(*settle)
	}

	if *screenshot != "" {
		foreground, _, _ := procGetForegroundWindow.Call()
		if foreground != hwnd {
			fmt.Fprintln(os.Stderr, "guiacceptance: warning: updater is not foreground; screenshot contains the actual current desktop pixels and may include occluding windows")
		}
		procDwmFlush.Call()
		time.Sleep(*settle)
		if err := captureWindowScreen(hwnd, *screenshot); err != nil {
			return err
		}
		fmt.Println("screenshot:", *screenshot)
	}

	if *clipboard || strings.TrimSpace(*expectClipboard) != "" {
		text, err := readClipboardText(2 * time.Second)
		if err != nil {
			return err
		}
		if strings.TrimSpace(*expectClipboard) != "" {
			for _, needle := range strings.Split(*expectClipboard, "|") {
				needle = strings.TrimSpace(needle)
				if needle != "" && !strings.Contains(text, needle) {
					return fmt.Errorf("clipboard missing %q", needle)
				}
			}
			fmt.Println("clipboard-expect: PASS")
		}
		if *clipboard {
			fmt.Println(text)
		}
	}

	if len(overlaps) > 0 {
		return fmt.Errorf("visible button overlap detected")
	}
	return nil
}

func waitForUpdaterWindow(pid uint32, timeout time.Duration) (uintptr, error) {
	deadline := time.Now().Add(timeout)
	for {
		matches, err := findUpdaterWindows(pid)
		if err != nil {
			return 0, err
		}
		if len(matches) == 1 {
			return matches[0], nil
		}
		if len(matches) > 1 {
			return 0, fmt.Errorf("found %d updater windows; specify -pid", len(matches))
		}
		if time.Now().After(deadline) {
			if pid != 0 {
				return 0, fmt.Errorf("updater window class %q for pid %d not found within %s", updaterWindowClass, pid, timeout)
			}
			return 0, fmt.Errorf("updater window class %q not found within %s", updaterWindowClass, timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func findUpdaterWindows(pid uint32) ([]uintptr, error) {
	var matches []uintptr
	var pidTitleMatches []uintptr
	callback := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		if visible == 0 {
			return 1
		}
		windowPIDValue := windowPID(hwnd)
		if pid != 0 && windowPIDValue != pid {
			return 1
		}
		if strings.EqualFold(windowClass(hwnd), updaterWindowClass) {
			matches = append(matches, hwnd)
			return 1
		}
		if pid != 0 && strings.TrimSpace(windowText(hwnd)) == "更新器" {
			pidTitleMatches = append(pidTitleMatches, hwnd)
		}
		return 1
	})
	r, _, callErr := procEnumWindows.Call(callback, 0)
	if r == 0 && callErr != syscall.Errno(0) {
		return nil, fmt.Errorf("EnumWindows: %w", callErr)
	}
	if len(matches) == 0 && pid != 0 {
		return pidTitleMatches, nil
	}
	return matches, nil
}

func enumControls(parent uintptr) ([]control, error) {
	var controls []control
	callback := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var r rect
		procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
		visible, _, _ := procIsWindowVisible.Call(hwnd)
		enabled, _, _ := procIsWindowEnabled.Call(hwnd)
		id, _, _ := procGetDlgCtrlID.Call(hwnd)
		style, _, _ := procGetWindowLongW.Call(hwnd, ^uintptr(15)) // GWL_STYLE == -16
		controls = append(controls, control{
			HWND:    hwnd,
			ID:      int32(id),
			Class:   windowClass(hwnd),
			Text:    windowText(hwnd),
			Rect:    r,
			Style:   uint32(style),
			Visible: visible != 0,
			Enabled: enabled != 0,
		})
		return 1
	})
	r, _, callErr := procEnumChildWindows.Call(parent, callback, 0)
	if r == 0 && callErr != syscall.Errno(0) {
		return nil, fmt.Errorf("EnumChildWindows: %w", callErr)
	}
	return controls, nil
}

func windowClass(hwnd uintptr) string {
	buf := make([]uint16, 256)
	n, _, _ := procGetClassNameW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func windowText(hwnd uintptr) string {
	length, _, _ := procGetWindowTextLengthW.Call(hwnd)
	if length == 0 {
		return ""
	}
	buf := make([]uint16, int(length)+1)
	n, _, _ := procGetWindowTextW.Call(hwnd, uintptr(unsafe.Pointer(&buf[0])), uintptr(len(buf)))
	if n == 0 {
		return ""
	}
	return syscall.UTF16ToString(buf[:n])
}

func windowPID(hwnd uintptr) uint32 {
	var pid uint32
	procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
	return pid
}

func ensureForeground(hwnd uintptr, timeout time.Duration) error {
	procShowWindow.Call(hwnd, swRestore)
	deadline := time.Now().Add(timeout)
	clickedTitleBar := false
	for time.Now().Before(deadline) {
		fg, _, _ := procGetForegroundWindow.Call()
		if fg == hwnd {
			return nil
		}

		activateWindow(hwnd, fg)
		time.Sleep(75 * time.Millisecond)
		fg, _, _ = procGetForegroundWindow.Call()
		if fg == hwnd {
			return nil
		}

		if !clickedTitleBar {
			var r rect
			ok, _, _ := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
			if ok != 0 && r.width() > 80 && r.height() > 40 {
				titleBar := rect{Left: r.Left + 40, Top: r.Top + 8, Right: r.Right - 80, Bottom: r.Top + 30}
				if titleBar.width() > 0 {
					_ = realClick(titleBar)
					clickedTitleBar = true
					time.Sleep(125 * time.Millisecond)
				}
			}
		}
	}
	return errors.New("Updater.exe could not become the foreground window after AttachThreadInput and real title-bar click")
}

func activateWindow(hwnd, foreground uintptr) {
	currentThread, _, _ := procGetCurrentThreadID.Call()
	targetThread, _, _ := procGetWindowThreadProcessID.Call(hwnd, 0)
	var foregroundThread uintptr
	if foreground != 0 {
		foregroundThread, _, _ = procGetWindowThreadProcessID.Call(foreground, 0)
	}

	attachedTarget := false
	attachedForeground := false
	if currentThread != 0 && targetThread != 0 && currentThread != targetThread {
		attached, _, _ := procAttachThreadInput.Call(currentThread, targetThread, 1)
		attachedTarget = attached != 0
	}
	if currentThread != 0 && foregroundThread != 0 && currentThread != foregroundThread && foregroundThread != targetThread {
		attached, _, _ := procAttachThreadInput.Call(currentThread, foregroundThread, 1)
		attachedForeground = attached != 0
	}

	procBringWindowToTop.Call(hwnd)
	procSetForegroundWindow.Call(hwnd)
	procSetActiveWindow.Call(hwnd)
	procSetFocus.Call(hwnd)

	if attachedForeground {
		procAttachThreadInput.Call(currentThread, foregroundThread, 0)
	}
	if attachedTarget {
		procAttachThreadInput.Call(currentThread, targetThread, 0)
	}
}

func visibleControlContains(controls []control, query string) bool {
	query = strings.TrimSpace(query)
	if query == "" {
		return false
	}
	for _, c := range controls {
		if c.Visible && strings.Contains(c.Text, query) {
			return true
		}
	}
	return false
}

func findControlByText(controls []control, query string) (control, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return control{}, errors.New("empty control text")
	}
	var exact, contains []control
	for _, c := range controls {
		if !c.Visible {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(c.Text), query) {
			exact = append(exact, c)
		} else if strings.Contains(strings.ToLower(c.Text), strings.ToLower(query)) {
			contains = append(contains, c)
		}
	}
	matches := exact
	if len(matches) == 0 {
		matches = contains
	}
	if len(matches) == 0 {
		return control{}, fmt.Errorf("visible control with text %q not found", query)
	}
	if len(matches) > 1 {
		return control{}, fmt.Errorf("control text %q matched %d visible controls; use the full caption", query, len(matches))
	}
	return matches[0], nil
}

func findControlByClass(controls []control, class string, index int) (control, error) {
	var matches []control
	for _, c := range controls {
		if c.Visible && strings.EqualFold(c.Class, class) {
			matches = append(matches, c)
		}
	}
	if index >= len(matches) {
		return control{}, fmt.Errorf("visible class %q index %d not found; matches=%d", class, index, len(matches))
	}
	return matches[index], nil
}

func realClick(r rect) error {
	if r.width() <= 0 || r.height() <= 0 {
		return fmt.Errorf("invalid target rectangle %s", formatRect(r))
	}
	x := r.Left + r.width()/2
	y := r.Top + r.height()/2
	ok, _, callErr := procSetCursorPos.Call(uintptr(int64(x)), uintptr(int64(y)))
	if ok == 0 {
		return fmt.Errorf("SetCursorPos(%d,%d): %w", x, y, callErr)
	}

	inputs := make([]input, 2)
	inputs[0].Type = inputMouse
	inputs[1].Type = inputMouse
	(*mouseInput)(unsafe.Pointer(&inputs[0].Data[0])).Flags = mouseeventfLeftDown
	(*mouseInput)(unsafe.Pointer(&inputs[1].Data[0])).Flags = mouseeventfLeftUp
	return sendInputs(inputs)
}

func realKeyPress(vk uint16) error {
	inputs := make([]input, 2)
	inputs[0].Type = inputKeyboard
	inputs[1].Type = inputKeyboard
	(*keyboardInput)(unsafe.Pointer(&inputs[0].Data[0])).Vk = vk
	up := (*keyboardInput)(unsafe.Pointer(&inputs[1].Data[0]))
	up.Vk = vk
	up.Flags = keyeventfKeyUp
	return sendInputs(inputs)
}

func realAutoRepeat(vk uint16, additionalDowns int, interval time.Duration) error {
	if additionalDowns < 1 {
		return realKeyPress(vk)
	}
	down := input{Type: inputKeyboard}
	(*keyboardInput)(unsafe.Pointer(&down.Data[0])).Vk = vk
	if err := sendInputs([]input{down}); err != nil {
		return err
	}
	for i := 0; i < additionalDowns; i++ {
		time.Sleep(interval)
		if err := sendInputs([]input{down}); err != nil {
			return err
		}
	}
	up := input{Type: inputKeyboard}
	ki := (*keyboardInput)(unsafe.Pointer(&up.Data[0]))
	ki.Vk = vk
	ki.Flags = keyeventfKeyUp
	return sendInputs([]input{up})
}

func sendInputs(inputs []input) error {
	if len(inputs) == 0 {
		return nil
	}
	sent, _, callErr := procSendInput.Call(
		uintptr(len(inputs)),
		uintptr(unsafe.Pointer(&inputs[0])),
		unsafe.Sizeof(inputs[0]),
	)
	if sent != uintptr(len(inputs)) {
		return fmt.Errorf("SendInput sent %d/%d event(s): %w", sent, len(inputs), callErr)
	}
	return nil
}

func parseVirtualKey(value string) (uint16, error) {
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "R":
		return 'R', nil
	case "S":
		return 'S', nil
	case "A":
		return 'A', nil
	case "ESC", "ESCAPE":
		return vkEscape, nil
	default:
		return 0, fmt.Errorf("unsupported key %q; use R, S, A, or Esc", value)
	}
}

func printControls(parent uintptr, controls []control) {
	var parentRect rect
	procGetWindowRect.Call(parent, uintptr(unsafe.Pointer(&parentRect)))
	fmt.Printf("window hwnd=0x%X pid=%d class=%q title=%q rect=%s\n", parent, windowPID(parent), windowClass(parent), windowText(parent), formatRect(parentRect))
	for _, c := range controls {
		rel := rect{
			Left: c.Rect.Left - parentRect.Left, Top: c.Rect.Top - parentRect.Top,
			Right: c.Rect.Right - parentRect.Left, Bottom: c.Rect.Bottom - parentRect.Top,
		}
		fmt.Printf("id=%d hwnd=0x%X class=%q visible=%t enabled=%t rect=%s rel=%s text=%q\n",
			c.ID, c.HWND, c.Class, c.Visible, c.Enabled, formatRect(c.Rect), formatRect(rel), c.Text)
	}
}

func findButtonOverlaps(controls []control) [][2]control {
	var buttons []control
	for _, c := range controls {
		if !c.Visible || !strings.EqualFold(c.Class, "Button") || c.Rect.width() <= 0 || c.Rect.height() <= 0 {
			continue
		}
		if c.Style&0xF == bsGroupBox {
			continue
		}
		buttons = append(buttons, c)
	}
	var overlaps [][2]control
	for i := 0; i < len(buttons); i++ {
		for j := i + 1; j < len(buttons); j++ {
			if rectsOverlap(buttons[i].Rect, buttons[j].Rect) {
				overlaps = append(overlaps, [2]control{buttons[i], buttons[j]})
			}
		}
	}
	return overlaps
}

func rectsOverlap(a, b rect) bool {
	left := max32(a.Left, b.Left)
	top := max32(a.Top, b.Top)
	right := min32(a.Right, b.Right)
	bottom := min32(a.Bottom, b.Bottom)
	return right > left && bottom > top
}

func max32(a, b int32) int32 {
	if a > b {
		return a
	}
	return b
}

func min32(a, b int32) int32 {
	if a < b {
		return a
	}
	return b
}

func formatRect(r rect) string {
	return fmt.Sprintf("[%d,%d]-[%d,%d] %dx%d", r.Left, r.Top, r.Right, r.Bottom, r.width(), r.height())
}

func captureWindowScreen(hwnd uintptr, output string) error {
	var r rect
	ok, _, callErr := procGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(&r)))
	if ok == 0 {
		return fmt.Errorf("GetWindowRect: %w", callErr)
	}
	width, height := r.width(), r.height()
	if width <= 0 || height <= 0 {
		return fmt.Errorf("invalid window rectangle %s", formatRect(r))
	}

	desktopDC, _, callErr := procGetDC.Call(0)
	if desktopDC == 0 {
		return fmt.Errorf("GetDC(desktop): %w", callErr)
	}
	defer procReleaseDC.Call(0, desktopDC)

	memoryDC, _, callErr := procCreateCompatibleDC.Call(desktopDC)
	if memoryDC == 0 {
		return fmt.Errorf("CreateCompatibleDC: %w", callErr)
	}
	defer procDeleteDC.Call(memoryDC)

	bitmap, _, callErr := procCreateCompatibleBitmap.Call(desktopDC, uintptr(width), uintptr(height))
	if bitmap == 0 {
		return fmt.Errorf("CreateCompatibleBitmap: %w", callErr)
	}
	defer procDeleteObject.Call(bitmap)

	old, _, callErr := procSelectObject.Call(memoryDC, bitmap)
	if old == 0 {
		return fmt.Errorf("SelectObject: %w", callErr)
	}

	copied, _, callErr := procBitBlt.Call(
		memoryDC, 0, 0, uintptr(width), uintptr(height),
		desktopDC, uintptr(int64(r.Left)), uintptr(int64(r.Top)),
		uintptr(srcCopy|captureBLT),
	)
	procSelectObject.Call(memoryDC, old)
	if copied == 0 {
		return fmt.Errorf("BitBlt: %w", callErr)
	}

	pixels := make([]byte, int(width)*int(height)*4)
	info := bitmapInfo{Header: bitmapInfoHeader{
		Size:        uint32(unsafe.Sizeof(bitmapInfoHeader{})),
		Width:       width,
		Height:      -height,
		Planes:      1,
		BitCount:    32,
		Compression: biRGB,
	}}
	scanLines, _, callErr := procGetDIBits.Call(
		memoryDC, bitmap, 0, uintptr(height), uintptr(unsafe.Pointer(&pixels[0])),
		uintptr(unsafe.Pointer(&info)), dibRGBColors,
	)
	if scanLines != uintptr(height) {
		return fmt.Errorf("GetDIBits returned %d/%d scan lines: %w", scanLines, height, callErr)
	}

	img := image.NewRGBA(image.Rect(0, 0, int(width), int(height)))
	for y := 0; y < int(height); y++ {
		for x := 0; x < int(width); x++ {
			src := (y*int(width) + x) * 4
			dst := y*img.Stride + x*4
			img.Pix[dst+0] = pixels[src+2]
			img.Pix[dst+1] = pixels[src+1]
			img.Pix[dst+2] = pixels[src+0]
			img.Pix[dst+3] = 0xFF
		}
	}

	if dir := filepath.Dir(output); dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.Create(output)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := png.Encode(file, img); err != nil {
		return err
	}
	return file.Sync()
}

func readClipboardText(timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for {
		ok, _, _ := procOpenClipboard.Call(0)
		if ok != 0 {
			break
		}
		if time.Now().After(deadline) {
			return "", errors.New("OpenClipboard timed out")
		}
		time.Sleep(25 * time.Millisecond)
	}
	defer procCloseClipboard.Call()

	handle, _, callErr := procGetClipboardData.Call(cfUnicodeText)
	if handle == 0 {
		return "", fmt.Errorf("GetClipboardData(CF_UNICODETEXT): %w", callErr)
	}
	size, _, _ := procGlobalSize.Call(handle)
	if size < 2 {
		return "", errors.New("clipboard Unicode text handle is empty")
	}
	ptr, _, callErr := procGlobalLock.Call(handle)
	if ptr == 0 {
		return "", fmt.Errorf("GlobalLock: %w", callErr)
	}
	defer procGlobalUnlock.Call(handle)

	units := unsafe.Slice((*uint16)(unsafe.Pointer(ptr)), int(size/2))
	end := 0
	for end < len(units) && units[end] != 0 {
		end++
	}
	return string(utf16.Decode(units[:end])), nil
}
