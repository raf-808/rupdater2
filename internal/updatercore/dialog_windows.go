//go:build windows

package updatercore

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// DialogUI implements UI using a Win32 GUI window.
type DialogUI struct {
	AutoConfirm bool
	Debug       bool
	cancelFunc  context.CancelFunc
	hwnd        uintptr
	buttonY     int32
	resultChan  chan bool
	doneChan    chan error
	waitingFor  waitKind

	mu                sync.Mutex
	inUpdate          bool
	updateDone        bool
	result            error
	exitResult        error
	pendingProgress   *ProgressEvent
	pendingInfo       *string
	pendingError      *string
	pendingPlan       *Plan
	pendingLocked     *[]LockedFile
	pendingVersion    *versionInfo
	progressQueued    bool
	statusText        string
	statusError       bool
	lastUIUpdate      time.Time // UI 级别进度节流：上次实际刷新 Win32 控件的时间戳
	lastProgressPhase string    // 上次刷新时的阶段，用于检测阶段切换（节流跳过时不更新）

	workFunc        func(context.Context) error
	ctxFunc         func() context.Context
	font            uintptr
	fontMedium      uintptr
	postMessageFunc func(hwnd uintptr, msg uint32)
	setTextFunc     func(hwnd uintptr, text string)
	sendMessageFunc func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr

	// Control handles
	hVersionLbl   uintptr
	hLatestLbl    uintptr
	hCountsLbl    uintptr
	hSizeLbl      uintptr
	hFileLbl      uintptr
	hSpeedLbl     uintptr
	hEtaLbl       uintptr
	hErrorEdit    uintptr
	hProgress     uintptr
	hStatusLbl    uintptr
	hStart        uintptr
	hCancel       uintptr
	hKillProc     uintptr
	hCloseBtn     uintptr
	hRecheck      uintptr
	hStatusPanel  uintptr
	hVerGroupBox  uintptr
	hProgGroupBox uintptr

	// 错误状态追踪：用于 WM_CTLCOLORSTATIC 渲染红底红字 (匹配 status-alert)
	isErrorState bool
}

type dpiRect struct {
	Left   int32
	Top    int32
	Right  int32
	Bottom int32
}

type versionInfo struct {
	current string
	latest  string
}

type waitKind uint8

const (
	waitNone waitKind = iota
	waitPlan
	waitLocked
)

const (
	dialogMarginX   = 24
	dialogRowH      = 28
	dialogBtnH      = 36
	dialogLabelW    = 532
	dialogProgressH = 16
	dialogGroupPad  = 20
	dialogErrorH    = 140
	dialogBtnGap    = 10
	dialogCloseW    = 92
	dialogRecheckW  = 108
	dialogKillW     = 188
	dialogCancelW   = 88
	dialogStartW    = 116
)

type bottomButtonSpec struct {
	hwnd  uintptr
	width int32
}

// NewDialogUI creates a DialogUI without creating the window yet.
func NewDialogUI(autoConfirm bool) *DialogUI {
	return &DialogUI{
		AutoConfirm:     autoConfirm,
		resultChan:      make(chan bool, 1),
		doneChan:        make(chan error, 1),
		postMessageFunc: postMsg,
		setTextFunc:     setCtrlText,
		sendMessageFunc: sendMsg,
	}
}

// DefaultUI returns the default UI implementation.
func DefaultUI(autoConfirm, silent bool) UI {
	if silent {
		return NewConsoleUI(autoConfirm, silent)
	}
	return NewDialogUI(autoConfirm)
}

// SetCancel stores the cancel function for the "取消" button.
func (ui *DialogUI) SetCancel(cancel context.CancelFunc) {
	ui.mu.Lock()
	ui.cancelFunc = cancel
	ui.mu.Unlock()
}

func (ui *DialogUI) debugf(format string, args ...any) {
	debugLog(ui.Debug, format, args...)
}

// setStatusWithError 设置状态栏文字并标记是否为错误状态（控制红底红字渲染）
func (ui *DialogUI) setStatusWithError(text string, isError bool) {
	ui.debugf("setStatusWithError enter text=%q isError=%v", text, isError)
	ui.mu.Lock()
	if ui.statusText == text && ui.statusError == isError {
		ui.mu.Unlock()
		ui.debugf("setStatusWithError unchanged, skip")
		return
	}
	ui.statusText = text
	ui.statusError = isError
	ui.isErrorState = isError
	ui.mu.Unlock()
	ui.debugf("setStatusWithError before SetWindowText")
	ui.setTextFunc(ui.hStatusLbl, text)
	ui.debugf("setStatusWithError after SetWindowText")
	if ui.hStatusLbl != 0 {
		ui.debugf("setStatusWithError invalidate status label")
		procInvalidateRect.Call(ui.hStatusLbl, 0, 1)
	}
	if ui.hStatusPanel != 0 {
		ui.debugf("setStatusWithError invalidate status panel")
		procInvalidateRect.Call(ui.hStatusPanel, 0, 1)
	}
	ui.debugf("setStatusWithError exit")
}

// RunMessageLoop creates the window, runs the work in a goroutine,
// and runs the message loop on the calling thread.
func (ui *DialogUI) RunMessageLoop(work func(context.Context) error, ctx context.Context) error {
	// Windows GUI 消息队列是线程归属的，必须锁定 OS 线程以防止
	// Go 运行时将 goroutine 迁移到其他线程，导致 GetMessage 收不到
	// PostMessage 投递的消息（间歇性卡死的根因）。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	globalDialogMu.Lock()
	globalDialogUI = ui
	globalDialogMu.Unlock()

	// 保存 work 和 ctx 供"再次检查"复用
	ui.workFunc = work
	ui.ctxFunc = func() context.Context {
		ctx, cancel := context.WithCancel(ctx)
		ui.mu.Lock()
		ui.cancelFunc = cancel
		ui.mu.Unlock()
		return ctx
	}

	ui.hwnd = createMainWindow("更新器")
	if ui.hwnd == 0 {
		return fmt.Errorf("无法创建 GUI 窗口")
	}

	ui.rebuildFonts(ui.hwnd)

	showCtrl(ui.hwnd, swShow)
	ui.requestStartWork()

	var m msg
	for getMessage(&m) > 0 {
		if ui.Debug {
			switch m.Message {
			case wmAppProgress, wmAppPlan, wmAppDone, WM_PAINT, WM_NCPAINT, WM_SIZE, WM_WINDOWPOSCHANGED:
				ui.debugf("message loop dispatch msg=0x%X", m.Message)
			}
		}
		translateMsg(&m)
		dispatchMsg(&m)
		if ui.Debug {
			switch m.Message {
			case wmAppProgress, wmAppPlan, wmAppDone, WM_PAINT, WM_NCPAINT, WM_SIZE, WM_WINDOWPOSCHANGED:
				ui.debugf("message loop dispatched msg=0x%X", m.Message)
			}
		}
	}
	return ui.exitResult
}

func (ui *DialogUI) rebuildFonts(hwnd uintptr) {
	ui.font = createModernFont()
	ui.fontMedium = createUIFont(fontWeightSemibold, -16)
	if ui.font == 0 && ui.fontMedium == 0 {
		return
	}
	if ui.fontMedium == 0 {
		ui.fontMedium = ui.font
	}
	if ui.font != 0 {
		applyFont(ui.hVerGroupBox, ui.fontMedium)
		applyFont(ui.hProgGroupBox, ui.fontMedium)
		applyFont(ui.hStatusPanel, ui.fontMedium)
		applyFont(ui.hVersionLbl, ui.font)
		applyFont(ui.hLatestLbl, ui.font)
		applyFont(ui.hCountsLbl, ui.font)
		applyFont(ui.hSizeLbl, ui.font)
		applyFont(ui.hProgress, ui.font)
		applyFont(ui.hFileLbl, ui.font)
		applyFont(ui.hSpeedLbl, ui.font)
		applyFont(ui.hEtaLbl, ui.font)
		applyFont(ui.hErrorEdit, ui.font)
		applyFont(ui.hStatusLbl, ui.fontMedium)
		applyFont(ui.hStart, ui.font)
		applyFont(ui.hCancel, ui.font)
		applyFont(ui.hKillProc, ui.font)
		applyFont(ui.hRecheck, ui.font)
		applyFont(ui.hCloseBtn, ui.font)
	}
	if hwnd != 0 {
		procInvalidateRect.Call(hwnd, 0, 1)
	}
}

func (ui *DialogUI) startWork() {
	ui.mu.Lock()
	ui.inUpdate = true
	ui.updateDone = false
	ui.result = nil
	ui.exitResult = nil
	ui.lastUIUpdate = time.Time{} // 重置节流计时器，确保首次进度立即刷新
	ui.lastProgressPhase = ""     // 重置阶段追踪
	ui.mu.Unlock()

	// 重置 UI 状态（非错误状态）
	ui.setStatusWithError("正在检查更新...", false)
	ui.setTextFunc(ui.hVersionLbl, "当前版本：获取中...")
	ui.setTextFunc(ui.hLatestLbl, "最新版本：获取中...")
	ui.setTextFunc(ui.hCountsLbl, "")
	ui.setTextFunc(ui.hSizeLbl, "")
	ui.setTextFunc(ui.hFileLbl, "")
	ui.setTextFunc(ui.hSpeedLbl, "")
	ui.setTextFunc(ui.hEtaLbl, "")
	ui.setTextFunc(ui.hErrorEdit, "")
	ui.sendMessageFunc(ui.hProgress, pbmSetPos, 0, 0)

	// 隐藏底部所有按钮，避免遗留上一个状态的控件
	ui.resetBottomButtons()

	// 创建新的可取消 context
	ctx := context.Background()
	if ui.ctxFunc != nil {
		ctx = ui.ctxFunc()
	}

	go func() {
		result := ui.workFunc(ctx)
		debugLog(ui.Debug, "worker finished, result=%v", result)
		ui.mu.Lock()
		ui.result = result
		ui.exitResult = result
		ui.updateDone = true
		ui.inUpdate = false
		ui.mu.Unlock()
		debugLog(ui.Debug, "posting wmAppDone")
		ui.postMessageFunc(ui.hwnd, wmAppDone)
	}()
}

func (ui *DialogUI) requestStartWork() {
	if ui.hwnd == 0 {
		return
	}
	ui.postMessageFunc(ui.hwnd, wmAppStartWork)
}

// ConfirmPlan shows the plan and waits for user confirmation.
func (ui *DialogUI) ConfirmPlan(plan Plan) bool {
	ui.debugf("ConfirmPlan enter")
	ui.logVersionInfo(plan.CurrentVersion, plan.LatestVersion)
	fmt.Fprintf(os.Stdout, "更新计划：新增 %d，修改 %d，删除 %d，下载 %s\n", len(plan.Add), len(plan.Modify), len(plan.Delete), formatBytes(plan.DownloadSize))
	ui.debugf("ConfirmPlan before lock")
	ui.mu.Lock()
	p := plan
	ui.pendingPlan = &p
	ui.waitingFor = waitPlan
	ui.mu.Unlock()
	ui.debugf("ConfirmPlan after lock hwnd=%d", ui.hwnd)
	if ui.hwnd != 0 {
		ui.debugf("ConfirmPlan posting wmAppPlan")
		ui.postMessageFunc(ui.hwnd, wmAppPlan)
		ui.debugf("ConfirmPlan posted wmAppPlan")
	}
	if ui.AutoConfirm {
		ui.debugf("ConfirmPlan auto confirm")
		return true
	}
	ui.debugf("ConfirmPlan waiting on resultChan")
	return <-ui.resultChan
}

// ConfirmProcessTermination shows locked files and waits for user confirmation.
func (ui *DialogUI) ConfirmProcessTermination(files []LockedFile) bool {
	if len(files) > 0 {
		fmt.Fprintln(os.Stdout, "检测到以下文件被占用：")
		for _, file := range files {
			fmt.Fprintf(os.Stdout, "- %s（进程：%s，PID：%d）\n", file.Path, emptyAsUnknown(file.ProcessName), file.PID)
		}
	}
	if ui.AutoConfirm {
		return true
	}
	ui.mu.Lock()
	f := files
	ui.pendingLocked = &f
	ui.waitingFor = waitLocked
	ui.mu.Unlock()
	if ui.hwnd != 0 {
		ui.postMessageFunc(ui.hwnd, wmAppLocked)
	}
	return <-ui.resultChan
}

func (ui *DialogUI) logProgress(event ProgressEvent) {
	switch event.Phase {
	case "Stage":
		if event.TotalFiles > 0 {
			fmt.Fprintf(os.Stdout, "[下载] %s（%d/%d）\n", event.CurrentFile, event.CompletedFiles, event.TotalFiles)
			return
		}
		fmt.Fprintf(os.Stdout, "[下载] %s\n", event.CurrentFile)
	case "Backup":
		fmt.Fprintf(os.Stdout, "[备份] %s\n", event.CurrentFile)
	case "Switch":
		fmt.Fprintf(os.Stdout, "[切换] %s\n", event.CurrentFile)
	case "Recover":
		fmt.Fprintf(os.Stdout, "[恢复] %s\n", event.CurrentFile)
	case "Check":
		fmt.Fprintln(os.Stdout, "[检查] 正在检查远端版本与清单")
	case "Plan":
		if event.TotalFiles > 0 {
			fmt.Fprintf(os.Stdout, "[计划] %s（%d/%d）\n", event.CurrentFile, event.CompletedFiles, event.TotalFiles)
			return
		}
		fmt.Fprintln(os.Stdout, "[计划] 正在生成更新计划")
	case "OccupancyCheck":
		fmt.Fprintln(os.Stdout, "[占用] 正在检查文件占用状态")
	case "Commit":
		if strings.TrimSpace(event.CurrentFile) != "" {
			fmt.Fprintf(os.Stdout, "[提交] %s\n", event.CurrentFile)
			return
		}
		fmt.Fprintln(os.Stdout, "[提交] 正在提交更新结果")
	}
}

func (ui *DialogUI) logInfo(message string) {
	fmt.Fprintln(os.Stdout, message)
}

func (ui *DialogUI) logError(message string) {
	fmt.Fprintln(os.Stderr, message)
}

func (ui *DialogUI) logVersionInfo(current, latest string) {
	fmt.Fprintf(os.Stdout, "当前版本：%s  最新版本：%s\n", emptyAsUnknown(current), latest)
}

// Progress updates the progress display.
func (ui *DialogUI) Progress(event ProgressEvent) {
	ui.logProgress(event)
	ui.mu.Lock()
	ev := event
	ui.pendingProgress = &ev
	if ui.progressQueued {
		ui.mu.Unlock()
		return
	}
	ui.progressQueued = true
	ui.mu.Unlock()
	if ui.hwnd != 0 {
		ui.postMessageFunc(ui.hwnd, wmAppProgress)
	}
}

// Info shows an informational message.
func (ui *DialogUI) Info(message string) {
	ui.logInfo(message)
	ui.mu.Lock()
	m := message
	ui.pendingInfo = &m
	ui.mu.Unlock()
	if ui.hwnd != 0 {
		ui.postMessageFunc(ui.hwnd, wmAppInfo)
	}
}

// Error shows an error message.
func (ui *DialogUI) Error(message string) {
	ui.logError(message)
	ui.mu.Lock()
	m := message
	ui.pendingError = &m
	ui.mu.Unlock()
	if ui.hwnd != 0 {
		ui.postMessageFunc(ui.hwnd, wmAppError)
	}
}

// ShowVersionInfo displays current and latest version on the UI.
func (ui *DialogUI) ShowVersionInfo(current, latest string) {
	ui.logVersionInfo(current, latest)
	ui.mu.Lock()
	v := versionInfo{current: current, latest: latest}
	ui.pendingVersion = &v
	ui.mu.Unlock()
	if ui.hwnd != 0 {
		ui.postMessageFunc(ui.hwnd, wmAppVersion)
	}
}

// handleMessage is the window procedure dispatch.
func (ui *DialogUI) handleMessage(hwnd uintptr, msg uint32, wParam uintptr, lParam uintptr) uintptr {
	switch msg {
	case WM_CREATE:
		ui.createControls(hwnd)
		return 0

	case WM_PAINT:
		ui.debugf("handleMessage WM_PAINT")
	case WM_NCPAINT:
		ui.debugf("handleMessage WM_NCPAINT")
	case WM_SIZE:
		ui.debugf("handleMessage WM_SIZE")
	case WM_WINDOWPOSCHANGED:
		ui.debugf("handleMessage WM_WINDOWPOSCHANGED")

	case WM_COMMAND:
		cmdID := int(wParam & 0xFFFF)
		switch cmdID {
		case idcStart:
			ui.mu.Lock()
			ui.inUpdate = true
			ui.waitingFor = waitNone
			ui.mu.Unlock()
			enableCtrl(ui.hStart, false)
			enableCtrl(ui.hCancel, true)
			select {
			case ui.resultChan <- true:
			default:
			}
			return 0
		case idcCancel:
			ui.mu.Lock()
			cancel := ui.cancelFunc
			ui.waitingFor = waitNone
			ui.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			select {
			case ui.resultChan <- false:
			default:
			}
			return 0
		case idcKillProc:
			showCtrl(ui.hKillProc, swHide)
			ui.mu.Lock()
			ui.waitingFor = waitNone
			ui.mu.Unlock()
			select {
			case ui.resultChan <- true:
			default:
			}
			return 0
		case idcRecheck:
			// 启动新的检查流程
			ui.requestStartWork()
			return 0
		case idcCloseBtn:
			ui.mu.Lock()
			ui.waitingFor = waitNone
			ui.inUpdate = false
			ui.updateDone = true
			ui.mu.Unlock()
			destroyWin(hwnd)
			return 0
		}

	case WM_CLOSE:
		ui.mu.Lock()
		canClose := ui.updateDone || !ui.inUpdate
		waitingFor := ui.waitingFor
		if canClose {
			ui.waitingFor = waitNone
		}
		ui.mu.Unlock()
		if !canClose {
			if waitingFor != waitNone {
				select {
				case ui.resultChan <- false:
				default:
				}
			}
			return 0
		}
		ui.mu.Lock()
		ui.inUpdate = false
		ui.updateDone = true
		ui.waitingFor = waitNone
		ui.mu.Unlock()
		select {
		case ui.resultChan <- false:
		default:
		}
		destroyWin(hwnd)
		postQuit(0)
		return 0

	case WM_DESTROY:
		ui.mu.Lock()
		ui.waitingFor = waitNone
		ui.mu.Unlock()
		ui.hwnd = 0
		postQuit(0)
		return 0

	case WM_ERASEBKGND:
		return 1

	case WM_CTLCOLORSTATIC:
		ui.mu.Lock()
		isError := ui.isErrorState
		ui.mu.Unlock()
		hdc := wParam
		ctrl := lParam
		if ctrl == ui.hStatusPanel || ctrl == ui.hStatusLbl {
			if isError {
				setupStaticTextColor(hdc, clrErrorText)
				return errorBrush
			}
			setupStaticTextColor(hdc, clrTextPrimary)
			return surfaceBrush
		}
		switch ctrl {
		case ui.hVerGroupBox, ui.hProgGroupBox:
			setupStaticTextColor(hdc, clrTextPrimary)
			return surfaceBrush
		case ui.hVersionLbl, ui.hLatestLbl, ui.hCountsLbl, ui.hSizeLbl:
			setupStaticTextColor(hdc, clrTextPrimary)
			return surfaceBrush
		case ui.hFileLbl, ui.hSpeedLbl, ui.hEtaLbl:
			setupStaticTextColor(hdc, clrTextSecondary)
			return surfaceBrush
		default:
			setupStaticTextColor(hdc, clrTextPrimary)
			return windowBrush
		}

	case WM_CTLCOLOREDIT:
		hdc := wParam
		setupOpaqueTextColor(hdc, clrTextPrimary, clrSurfaceAltBg)
		return surfaceAltBrush

	case WM_CTLCOLORBTN:
		hdc := wParam
		setupStaticTextColor(hdc, clrTextPrimary)
		return surfaceBrush

	case WM_DPICHANGED:
		if lParam != 0 {
			rect := (*dpiRect)(unsafe.Pointer(lParam))
			procSetWindowPos.Call(hwnd, 0, uintptr(rect.Left), uintptr(rect.Top), uintptr(rect.Right-rect.Left), uintptr(rect.Bottom-rect.Top), 0x0027)
		}
		ui.rebuildFonts(hwnd)
		return 0

	case WM_GETMINMAXINFO:
		mmi := (*minMaxInfo)(unsafe.Pointer(lParam))
		mmi.PtMinTrackSize = point{X: dialogMinWidth, Y: dialogMinHeight}
		return 0

	case wmAppProgress:
		ui.mu.Lock()
		ev := ui.pendingProgress
		ui.pendingProgress = nil
		ui.progressQueued = false
		ui.mu.Unlock()
		if ev != nil {
			ui.debugf("received wmAppProgress phase=%s current=%q", ev.Phase, ev.CurrentFile)
			// UI 级别节流：避免高频进度更新导致消息队列 flooding 和 UI 线程饥饿。
			// 相邻两次 Win32 控件刷新至少间隔 progressThrottleInterval，
			// 但阶段切换和最后一个文件始终立即刷新。
			const progressThrottleInterval = 50 * time.Millisecond
			isPhaseChange := ev.Phase != ui.lastProgressPhase
			isFinal := ev.TotalFiles > 0 && ev.CompletedFiles >= ev.TotalFiles
			shouldUpdate := isPhaseChange || isFinal ||
				time.Since(ui.lastUIUpdate) >= progressThrottleInterval
			if shouldUpdate {
				ui.applyProgress(ev)
				ui.lastUIUpdate = time.Now()
				ui.lastProgressPhase = ev.Phase
			} else {
				ui.debugf("wmAppProgress throttled phase=%s", ev.Phase)
			}
			ui.debugf("finished wmAppProgress phase=%s current=%q", ev.Phase, ev.CurrentFile)
		}
		return 0

	case wmAppStartWork:
		ui.startWork()
		return 0

	case wmAppInfo:
		ui.mu.Lock()
		m := ui.pendingInfo
		ui.pendingInfo = nil
		ui.mu.Unlock()
		if m != nil {
			ui.setStatusWithError(*m, false)
		}
		return 0

	case wmAppError:
		ui.mu.Lock()
		m := ui.pendingError
		ui.pendingError = nil
		ui.mu.Unlock()
		if m != nil {
			ui.appendError(*m)
		}
		return 0

	case wmAppVersion:
		ui.mu.Lock()
		version := ui.pendingVersion
		ui.pendingVersion = nil
		ui.mu.Unlock()
		if version != nil {
			ui.setTextFunc(ui.hVersionLbl, "当前版本："+emptyAsUnknown(version.current))
			ui.setTextFunc(ui.hLatestLbl, "最新版本："+emptyAsUnknown(version.latest))
		}
		return 0

	case wmAppPlan:
		ui.mu.Lock()
		plan := ui.pendingPlan
		ui.pendingPlan = nil
		ui.mu.Unlock()
		if plan != nil {
			ui.debugf("received wmAppPlan")
			ui.showPlan(plan)
		}
		return 0

	case wmAppLocked:
		ui.mu.Lock()
		files := ui.pendingLocked
		ui.pendingLocked = nil
		ui.mu.Unlock()
		if files != nil {
			ui.showLocked(*files)
		}
		return 0

	case wmAppDone:
		ui.debugf("received wmAppDone")
		ui.handleDone()
		return 0
	}

	return defWinProc(hwnd, msg, wParam, lParam)
}

// createControls creates all child controls in the window.
// 布局参数匹配 updater_gui.html 参考设计 (580px内容区, 24px内边距, 20px间距)
func (ui *DialogUI) createControls(hwnd uintptr) {
	y := int32(24)

	ui.hVerGroupBox = createControl(hwnd, "BUTTON", bsGroupbox, idcVerGroupBox, dialogMarginX-12, y, dialogLabelW+24, 96)
	styleGroupBox(ui.hVerGroupBox)
	y += 28
	ui.hVersionLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcVersionLbl, dialogMarginX+dialogGroupPad, y, dialogLabelW-dialogGroupPad*2, dialogRowH)
	y += dialogRowH + 8
	ui.hLatestLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcLatestLbl, dialogMarginX+dialogGroupPad, y, dialogLabelW-dialogGroupPad*2, dialogRowH)
	y += dialogRowH + 20

	ui.hCountsLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcCountsLbl, dialogMarginX+dialogGroupPad, y, dialogLabelW-dialogGroupPad*2, dialogRowH)
	y += dialogRowH
	ui.hSizeLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcSizeLbl, dialogMarginX+dialogGroupPad, y, dialogLabelW-dialogGroupPad*2, dialogRowH)
	y += dialogRowH + 20

	progBoxY := y - 12
	ui.hProgGroupBox = createControl(hwnd, "BUTTON", bsGroupbox, idcProgGroupBox, dialogMarginX-12, progBoxY, dialogLabelW+24, 200)
	styleGroupBox(ui.hProgGroupBox)
	y += 22
	ui.hProgress = createControl(hwnd, "msctls_progress32", pbsSmooth, idcProgress, dialogMarginX+dialogGroupPad, y, dialogLabelW-dialogGroupPad*2, dialogProgressH)
	ui.sendMessageFunc(ui.hProgress, pbmSetRange32, 0, 100)
	styleProgressBar(ui.hProgress)
	y += dialogProgressH + 16

	ui.hFileLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcFileLbl, dialogMarginX+dialogGroupPad, y, dialogLabelW-dialogGroupPad*2, dialogRowH)
	y += dialogRowH + 6
	ui.hSpeedLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcSpeedLbl, dialogMarginX+dialogGroupPad, y, (dialogLabelW-dialogGroupPad*2)/2, dialogRowH)
	ui.hEtaLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcEtaLbl, dialogMarginX+dialogGroupPad+(dialogLabelW-dialogGroupPad*2)/2+8, y, (dialogLabelW-dialogGroupPad*2)/2, dialogRowH)
	y += dialogRowH + 16

	ui.hErrorEdit = createControl(hwnd, "EDIT", esMultiline|esReadOnly|esAutoVScroll|wsVScroll|wsBorder, idcErrorEdit, dialogMarginX, y, dialogLabelW, dialogErrorH)
	applyExplorerTheme(ui.hErrorEdit)
	y += dialogErrorH + 24

	progBoxH := y - progBoxY - 12
	procSetWindowPos.Call(ui.hProgGroupBox, 0, uintptr(dialogMarginX-12), uintptr(progBoxY), uintptr(dialogLabelW+24), uintptr(progBoxH), 0x0014)

	ui.hStatusPanel = createControl(hwnd, "BUTTON", bsGroupbox, 0, dialogMarginX-12, y-12, dialogLabelW+24, 56)
	styleGroupBox(ui.hStatusPanel)
	ui.setTextFunc(ui.hStatusPanel, "")
	ui.hStatusLbl = createControl(hwnd, "STATIC", ssLeft|ssNotify, idcStatusLbl, dialogMarginX+dialogGroupPad, y+4, dialogLabelW-dialogGroupPad*2, dialogRowH)
	y += dialogRowH + 24

	ui.buttonY = y
	ui.hCloseBtn = createControl(hwnd, "BUTTON", 0, idcCloseBtn, dialogMarginX, ui.buttonY, dialogCloseW, dialogBtnH)
	ui.setTextFunc(ui.hCloseBtn, "关闭")
	styleButton(ui.hCloseBtn, false)
	showCtrl(ui.hCloseBtn, swHide)
	enableCtrl(ui.hCloseBtn, false)

	ui.hRecheck = createControl(hwnd, "BUTTON", 0, idcRecheck, dialogMarginX, ui.buttonY, dialogRecheckW, dialogBtnH)
	ui.setTextFunc(ui.hRecheck, "再次检查")
	styleButton(ui.hRecheck, false)
	showCtrl(ui.hRecheck, swHide)
	enableCtrl(ui.hRecheck, false)

	ui.hKillProc = createControl(hwnd, "BUTTON", 0, idcKillProc, dialogMarginX, ui.buttonY, dialogKillW, dialogBtnH)
	ui.setTextFunc(ui.hKillProc, "关闭相关进程并继续")
	styleButton(ui.hKillProc, false)
	showCtrl(ui.hKillProc, swHide)

	ui.hCancel = createControl(hwnd, "BUTTON", 0, idcCancel, dialogMarginX, ui.buttonY, dialogCancelW, dialogBtnH)
	ui.setTextFunc(ui.hCancel, "取消")
	styleButton(ui.hCancel, false)
	showCtrl(ui.hCancel, swHide)

	ui.hStart = createControl(hwnd, "BUTTON", bsDefPushbutton, idcStart, dialogMarginX, ui.buttonY, dialogStartW, dialogBtnH)
	ui.setTextFunc(ui.hStart, "开始更新")
	styleButton(ui.hStart, true)
	enableCtrl(ui.hStart, false)
	showCtrl(ui.hStart, swHide)

	ui.rebuildFonts(hwnd)

	ui.setTextFunc(ui.hVerGroupBox, "版本信息")
	ui.setTextFunc(ui.hProgGroupBox, "更新进度")
	ui.setTextFunc(ui.hVersionLbl, "当前版本：获取中...")
	ui.setTextFunc(ui.hLatestLbl, "最新版本：获取中...")
	ui.setTextFunc(ui.hCountsLbl, "")
	ui.setTextFunc(ui.hSizeLbl, "")
	ui.setTextFunc(ui.hFileLbl, "当前文件：—")
	ui.setTextFunc(ui.hSpeedLbl, "速度：—")
	ui.setTextFunc(ui.hEtaLbl, "剩余：—")
	ui.setStatusWithError("正在检查更新...", false) // 初始状态：非错误
	ui.resetBottomButtons()
}

func (ui *DialogUI) layoutBottomButtons(buttons []bottomButtonSpec) {
	if len(buttons) == 0 {
		return
	}
	totalW := int32(0)
	for i, button := range buttons {
		totalW += button.width
		if i > 0 {
			totalW += dialogBtnGap
		}
	}
	x := int32(dialogMarginX + dialogLabelW - totalW)
	if x < dialogMarginX {
		x = dialogMarginX
	}
	for _, button := range buttons {
		ui.debugf("layoutBottomButtons move hwnd=%d x=%d y=%d w=%d h=%d", button.hwnd, x, ui.buttonY, button.width, dialogBtnH)
		procSetWindowPos.Call(button.hwnd, 0, uintptr(x), uintptr(ui.buttonY), uintptr(button.width), uintptr(dialogBtnH), 0x0014)
		x += button.width + dialogBtnGap
	}
	ui.debugf("layoutBottomButtons exit count=%d", len(buttons))
}

func (ui *DialogUI) resetBottomButtons() {
	buttons := []uintptr{
		ui.hCloseBtn,
		ui.hRecheck,
		ui.hKillProc,
		ui.hCancel,
		ui.hStart,
	}
	for _, hwnd := range buttons {
		ui.debugf("resetBottomButtons hide hwnd=%d", hwnd)
		showCtrl(hwnd, swHide)
		ui.debugf("resetBottomButtons disable hwnd=%d", hwnd)
		enableCtrl(hwnd, false)
	}
	ui.debugf("resetBottomButtons exit")
}

func (ui *DialogUI) showBottomButtons(buttons []bottomButtonSpec) {
	ui.debugf("showBottomButtons enter count=%d", len(buttons))
	ui.resetBottomButtons()
	ui.debugf("showBottomButtons after reset")
	ui.layoutBottomButtons(buttons)
	ui.debugf("showBottomButtons after layout")
	for _, button := range buttons {
		ui.debugf("showBottomButtons show hwnd=%d", button.hwnd)
		showCtrl(button.hwnd, swShow)
		ui.debugf("showBottomButtons enable hwnd=%d", button.hwnd)
		enableCtrl(button.hwnd, true)
	}
	ui.debugf("showBottomButtons exit")
}

// showPlan updates the labels with plan information and enables the start button.
func (ui *DialogUI) showPlan(plan *Plan) {
	ui.debugf("showPlan enter")
	ui.setTextFunc(ui.hVersionLbl, "当前版本："+emptyAsUnknown(plan.CurrentVersion))
	ui.setTextFunc(ui.hLatestLbl, "最新版本："+emptyAsUnknown(plan.LatestVersion))
	ui.setTextFunc(ui.hCountsLbl, fmt.Sprintf("新增：%d  修改：%d  删除：%d", len(plan.Add), len(plan.Modify), len(plan.Delete)))
	ui.setTextFunc(ui.hSizeLbl, "下载大小："+formatBytes(plan.DownloadSize))
	ui.setStatusWithError("已获取更新计划，请确认是否开始更新。", false)
	ui.showBottomButtons([]bottomButtonSpec{
		{hwnd: ui.hStart, width: dialogStartW},
		{hwnd: ui.hCancel, width: dialogCancelW},
	})
	if ui.AutoConfirm {
		enableCtrl(ui.hStart, false)
	}
	ui.debugf("showPlan exit")
}

// showLocked updates the labels with locked file information.
func (ui *DialogUI) showLocked(files []LockedFile) {
	var sb strings.Builder
	sb.WriteString("以下文件正在被占用：\r\n")
	for _, f := range files {
		sb.WriteString(fmt.Sprintf("- %s（进程：%s，PID：%d）\r\n", f.Path, emptyAsUnknown(f.ProcessName), f.PID))
	}
	sb.WriteString("\r\n请点击「关闭相关进程并继续」或「取消」。")
	ui.appendError(sb.String())
	ui.setStatusWithError("检测到文件占用，请先释放相关进程。", true)
	ui.showBottomButtons([]bottomButtonSpec{
		{hwnd: ui.hCancel, width: dialogCancelW},
		{hwnd: ui.hKillProc, width: dialogKillW},
	})
}

// applyProgress updates the progress bar and labels.
func (ui *DialogUI) applyProgress(ev *ProgressEvent) {
	ui.debugf("applyProgress enter phase=%s current=%q", ev.Phase, ev.CurrentFile)
	ui.debugf("applyProgress before set current file label")
	ui.setTextFunc(ui.hFileLbl, "当前文件："+emptyAsUnknown(ev.CurrentFile))
	ui.debugf("applyProgress after set current file label")

	if ev.Phase == "Stage" {
		if ev.BytesTotal > 0 {
			pos := uintptr(0)
			if ev.BytesDone > 0 {
				pos = uintptr(ev.BytesDone * 100 / ev.BytesTotal)
			}
			ui.debugf("applyProgress before set progress pos bytes pos=%d", pos)
			ui.sendMessageFunc(ui.hProgress, pbmSetPos, pos, 0)
			ui.debugf("applyProgress after set progress pos bytes")
		} else if ev.TotalFiles > 0 {
			pos := 0
			if ev.CompletedFiles > 0 {
				pos = ev.CompletedFiles * 100 / ev.TotalFiles
			}
			ui.debugf("applyProgress before set progress pos files pos=%d", pos)
			ui.sendMessageFunc(ui.hProgress, pbmSetPos, uintptr(pos), 0)
			ui.debugf("applyProgress after set progress pos files")
		}
		ui.debugf("applyProgress before set speed text")
		ui.setTextFunc(ui.hSpeedLbl, formatSpeed(ev.SpeedBytes))
		ui.debugf("applyProgress after set speed text")
		ui.debugf("applyProgress before set eta text")
		ui.setTextFunc(ui.hEtaLbl, formatETA(ev.BytesDone, ev.BytesTotal, ev.SpeedBytes))
		ui.debugf("applyProgress after set eta text")
	} else {
		ui.debugf("applyProgress before reset speed/eta text")
		ui.setTextFunc(ui.hSpeedLbl, "速度：—")
		ui.setTextFunc(ui.hEtaLbl, "剩余：—")
		ui.debugf("applyProgress after reset speed/eta text")
	}

	if ev.Phase == "Plan" && ev.TotalFiles > 0 {
		pos := 0
		if ev.CompletedFiles > 0 {
			pos = ev.CompletedFiles * 100 / ev.TotalFiles
		}
		ui.debugf("applyProgress before set plan progress pos=%d", pos)
		ui.sendMessageFunc(ui.hProgress, pbmSetPos, uintptr(pos), 0)
		ui.debugf("applyProgress after set plan progress pos")
		ui.debugf("applyProgress before set counts label")
		ui.setTextFunc(ui.hCountsLbl, fmt.Sprintf("扫描进度：%d / %d", ev.CompletedFiles, ev.TotalFiles))
		ui.debugf("applyProgress after set counts label")
	}

	switch ev.Phase {
	case "Check":
		ui.setStatusWithError("正在检查远端版本与清单…", false)
	case "Plan":
		ui.setStatusWithError("正在扫描本地文件并生成更新计划…", false)
	case "Stage":
		ui.setStatusWithError("正在下载并校验更新文件…", false)
	case "OccupancyCheck":
		ui.setStatusWithError("正在检查文件占用状态…", false)
	case "Backup":
		ui.setStatusWithError("正在备份："+ev.CurrentFile, false)
	case "Switch":
		ui.setStatusWithError("正在切换："+ev.CurrentFile, false)
	case "Commit":
		if strings.TrimSpace(ev.CurrentFile) != "" {
			ui.setStatusWithError("正在提交更新结果："+ev.CurrentFile, false)
		} else {
			ui.setStatusWithError("正在提交更新结果…", false)
		}
	case "Recover":
		ui.setStatusWithError("正在恢复："+ev.CurrentFile, false)
	}
	ui.debugf("applyProgress exit phase=%s current=%q", ev.Phase, ev.CurrentFile)
}

// appendError appends text to the error edit control.
func (ui *DialogUI) appendError(text string) {
	lenRet := ui.sendMessageFunc(ui.hErrorEdit, wmGetTextLength, 0, 0)
	if lenRet > 0 {
		ui.sendMessageFunc(ui.hErrorEdit, emSetSel, lenRet, lenRet)
		ptr, _ := syscall.UTF16PtrFromString("\r\n" + text)
		ui.sendMessageFunc(ui.hErrorEdit, emReplaceSel, 1, uintptr(unsafe.Pointer(ptr)))
	} else {
		ui.setTextFunc(ui.hErrorEdit, text)
	}
}

// handleDone is called when the work goroutine finishes.
func (ui *DialogUI) handleDone() {
	if ui.hwnd == 0 {
		return
	}
	debugLog(ui.Debug, "handleDone entered")
	ui.mu.Lock()
	result := ui.result
	ui.mu.Unlock()

	ui.resetBottomButtons()

	if result == ErrSelfUpdateHandoff {
		destroyWin(ui.hwnd)
		return
	}

	switch {
	case result == nil:
		ui.setStatusWithError("更新完成。", false)
		styleButton(ui.hCloseBtn, true)
		ui.showBottomButtons([]bottomButtonSpec{{hwnd: ui.hCloseBtn, width: dialogCloseW}})
	case result == ErrMissingConfig:
		ui.setStatusWithError("未找到可用配置。", false)
		styleButton(ui.hCloseBtn, true)
		ui.showBottomButtons([]bottomButtonSpec{{hwnd: ui.hCloseBtn, width: dialogCloseW}})
	case result == ErrUserCancelled:
		ui.setStatusWithError("用户已取消更新。", false)
		styleButton(ui.hCloseBtn, false)
		styleButton(ui.hRecheck, true)
		ui.showBottomButtons([]bottomButtonSpec{
			{hwnd: ui.hRecheck, width: dialogRecheckW},
			{hwnd: ui.hCloseBtn, width: dialogCloseW},
		})
	case result == ErrNoUpdate:
		ui.setStatusWithError("当前已是最新版本。", false)
		styleButton(ui.hCloseBtn, true)
		ui.showBottomButtons([]bottomButtonSpec{{hwnd: ui.hCloseBtn, width: dialogCloseW}})
	default:
		ui.setStatusWithError("更新失败。", true) // 红底红字 (匹配 .status-alert)
		ui.appendError(result.Error())
		styleButton(ui.hCloseBtn, false)
		styleButton(ui.hRecheck, true)
		ui.showBottomButtons([]bottomButtonSpec{
			{hwnd: ui.hRecheck, width: dialogRecheckW},
			{hwnd: ui.hCloseBtn, width: dialogCloseW},
		})
	}
	debugLog(ui.Debug, "handleDone exited, result=%v", result)
}

// formatSpeed formats a download speed.
func formatSpeed(bytesPerSec float64) string {
	if bytesPerSec <= 0 {
		return "速度：—"
	}
	return "速度：" + formatBytes(int64(bytesPerSec)) + "/s"
}

// formatETA formats the estimated time remaining.
func formatETA(done, total int64, speed float64) string {
	if speed <= 0 || total <= 0 || done >= total {
		return "剩余：—"
	}
	remaining := float64(total-done) / speed
	if remaining < 60 {
		return fmt.Sprintf("剩余：%.0f 秒", remaining)
	}
	if remaining < 3600 {
		return fmt.Sprintf("剩余：%.0f 分钟", remaining/60)
	}
	return fmt.Sprintf("剩余：%.1f 小时", remaining/3600)
}
