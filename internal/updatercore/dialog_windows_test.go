//go:build windows

package updatercore

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"syscall"
	"testing"
	"time"
	"unsafe"
)

func TestDialogUIShowVersionInfoQueuesVersionMessage(t *testing.T) {
	ui := NewDialogUI(false)
	ui.hwnd = 99

	var posted []uint32
	ui.postMessageFunc = func(_ uintptr, msg uint32) {
		posted = append(posted, msg)
	}

	ui.ShowVersionInfo("2026.06.16.1", "2026.06.17.1")

	if ui.pendingVersion == nil {
		t.Fatal("pendingVersion is nil")
	}
	if ui.pendingVersion.current != "2026.06.16.1" || ui.pendingVersion.latest != "2026.06.17.1" {
		t.Fatalf("pendingVersion = %#v", *ui.pendingVersion)
	}
	if len(posted) != 1 || posted[0] != wmAppVersion {
		t.Fatalf("posted messages = %#v", posted)
	}
}

func TestDialogUIHandleVersionMessageAppliesQueuedVersion(t *testing.T) {
	ui := NewDialogUI(false)
	ui.hVersionLbl = 1
	ui.hLatestLbl = 2

	var updates []struct {
		hwnd uintptr
		text string
	}
	ui.setTextFunc = func(hwnd uintptr, text string) {
		updates = append(updates, struct {
			hwnd uintptr
			text string
		}{hwnd: hwnd, text: text})
	}
	ui.ShowVersionInfo("2026.06.16.1", "2026.06.17.1")

	ui.handleMessage(0, wmAppVersion, 0, 0)

	if ui.pendingVersion != nil {
		t.Fatal("pendingVersion was not cleared")
	}
	if len(updates) != 2 {
		t.Fatalf("updates = %#v", updates)
	}
	if updates[0].hwnd != 1 || updates[0].text != "当前版本：2026.06.16.1" {
		t.Fatalf("first update = %#v", updates[0])
	}
	if updates[1].hwnd != 2 || updates[1].text != "最新版本：2026.06.17.1" {
		t.Fatalf("second update = %#v", updates[1])
	}
}

func TestDialogUIWindowCloseCancelsPendingPlanWait(t *testing.T) {
	ui := NewDialogUI(false)
	ui.inUpdate = true
	ui.waitingFor = waitPlan

	done := make(chan bool, 1)
	go func() {
		done <- ui.ConfirmPlan(Plan{CurrentVersion: "1", LatestVersion: "2"})
	}()

	ui.handleMessage(0, WM_CLOSE, 0, 0)

	select {
	case result := <-done:
		if result {
			t.Fatal("expected close to cancel pending plan confirmation")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("pending plan confirmation was not released on close")
	}
}

func TestDialogUICloseButtonMarksWindowDoneBeforeDestroy(t *testing.T) {
	ui := NewDialogUI(false)
	ui.inUpdate = true
	ui.hwnd = 42

	var destroyed uintptr
	oldDestroy := destroyWin
	destroyWin = func(hwnd uintptr) {
		destroyed = hwnd
	}
	defer func() { destroyWin = oldDestroy }()

	ui.handleMessage(42, WM_COMMAND, uintptr(idcCloseBtn), 0)

	if destroyed != 42 {
		t.Fatalf("destroyed hwnd = %d", destroyed)
	}
	if ui.inUpdate {
		t.Fatal("expected inUpdate to be false after close")
	}
	if !ui.updateDone {
		t.Fatal("expected updateDone to be true after close")
	}
}

func TestDialogUIWindowClosePostsQuit(t *testing.T) {
	ui := NewDialogUI(false)
	ui.updateDone = true
	ui.hwnd = 7

	var destroyed uintptr
	oldDestroy := destroyWin
	destroyWin = func(hwnd uintptr) {
		destroyed = hwnd
	}
	defer func() { destroyWin = oldDestroy }()

	var quitCode int32 = -1
	oldPostQuit := postQuit
	postQuit = func(code int32) {
		quitCode = code
	}
	defer func() { postQuit = oldPostQuit }()

	ui.handleMessage(7, WM_CLOSE, 0, 0)

	if destroyed != 7 {
		t.Fatalf("destroyed hwnd = %d", destroyed)
	}
	if quitCode != 0 {
		t.Fatalf("quitCode = %d", quitCode)
	}
}

func TestDialogUIApplyProgressDoesNotRewriteUnchangedStatus(t *testing.T) {
	ui := NewDialogUI(false)
	ui.hStatusLbl = 1
	ui.hFileLbl = 2
	ui.hSpeedLbl = 3
	ui.hEtaLbl = 4
	ui.hCountsLbl = 5
	ui.hProgress = 6

	var statusWrites int
	ui.setTextFunc = func(hwnd uintptr, text string) {
		if hwnd == ui.hStatusLbl {
			statusWrites++
		}
	}
	ui.sendMessageFunc = func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		return 0
	}

	ui.applyProgress(&ProgressEvent{Phase: "Plan", CurrentFile: "a.txt", CompletedFiles: 1, TotalFiles: 10})
	ui.applyProgress(&ProgressEvent{Phase: "Plan", CurrentFile: "b.txt", CompletedFiles: 2, TotalFiles: 10})

	if statusWrites != 1 {
		t.Fatalf("statusWrites = %d, want 1", statusWrites)
	}
}

func TestDialogUIProgressCoalescesPostedMessages(t *testing.T) {
	ui := NewDialogUI(false)
	ui.hwnd = 1
	ui.hStatusLbl = 1
	ui.hFileLbl = 2
	ui.hSpeedLbl = 3
	ui.hEtaLbl = 4
	ui.hCountsLbl = 5
	ui.hProgress = 6

	var posted []uint32
	ui.postMessageFunc = func(_ uintptr, msg uint32) {
		posted = append(posted, msg)
	}
	ui.setTextFunc = func(hwnd uintptr, text string) {}
	ui.sendMessageFunc = func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
		return 0
	}

	ui.Progress(ProgressEvent{Phase: "Plan", CurrentFile: "a.txt", CompletedFiles: 1, TotalFiles: 10})
	ui.Progress(ProgressEvent{Phase: "Plan", CurrentFile: "b.txt", CompletedFiles: 2, TotalFiles: 10})

	if len(posted) != 1 {
		t.Fatalf("posted count = %d, want 1", len(posted))
	}

	ui.handleMessage(0, wmAppProgress, 0, 0)
	ui.Progress(ProgressEvent{Phase: "Plan", CurrentFile: "c.txt", CompletedFiles: 3, TotalFiles: 10})

	if len(posted) != 2 {
		t.Fatalf("posted count after drain = %d, want 2", len(posted))
	}
}

func newDialogTestUI() *DialogUI {
	ui := NewDialogUI(false)
	ui.hwnd = 1
	ui.postMessageFunc = func(uintptr, uint32) {}
	ui.setTextFunc = func(uintptr, string) {}
	ui.sendMessageFunc = func(uintptr, uint32, uintptr, uintptr) uintptr { return 0 }
	ui.foregroundWindowFunc = func() uintptr { return ui.hwnd }
	return ui
}

func TestFileFailureDecisionAllowed(t *testing.T) {
	decisions := []FileFailureDecision{
		FileFailureDecisionRetry,
		FileFailureDecisionSkip,
		FileFailureDecisionSkipAll,
		FileFailureDecisionCancel,
	}
	for _, decision := range decisions {
		if !fileFailureDecisionAllowed(decision, false) {
			t.Fatalf("ordinary decision %q was rejected", decision)
		}
	}
	if !fileFailureDecisionAllowed(FileFailureDecisionRetry, true) || !fileFailureDecisionAllowed(FileFailureDecisionCancel, true) {
		t.Fatal("important files must allow retry and cancel")
	}
	for _, decision := range []FileFailureDecision{FileFailureDecisionSkip, FileFailureDecisionSkipAll} {
		if fileFailureDecisionAllowed(decision, true) {
			t.Fatalf("important decision %q was accepted", decision)
		}
	}
}

func TestDialogUIFileFailureButtonDecisionIsSerialized(t *testing.T) {
	ui := newDialogTestUI()
	posted := make(chan uint32, 1)
	ui.postMessageFunc = func(_ uintptr, message uint32) { posted <- message }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decision := make(chan FileFailureDecision, 1)
	go func() {
		decision <- ui.DecideFileFailure(ctx, FileFailure{Path: "data/a.wav", Error: "checksum mismatch"})
	}()

	select {
	case message := <-posted:
		if message != wmAppFileFailure {
			t.Fatalf("posted message = 0x%x", message)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("file failure message was not posted")
	}
	ui.handleMessage(ui.hwnd, wmAppFileFailure, 0, 0)
	if !ui.hadFileFailure {
		t.Fatal("hadFileFailure was not set when decision state opened")
	}
	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcSkip), 0)
	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcSkipAll), 0)

	select {
	case got := <-decision:
		if got != FileFailureDecisionSkip {
			t.Fatalf("decision = %q, want skip", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("file failure decision was not released")
	}
	if ui.fileFailureActive {
		t.Fatal("file failure state remained active after submission")
	}
}

func TestDialogUIImportantFailureRejectsSkipAndAllowsRetry(t *testing.T) {
	ui := newDialogTestUI()
	posted := make(chan uint32, 1)
	ui.postMessageFunc = func(_ uintptr, message uint32) { posted <- message }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decision := make(chan FileFailureDecision, 1)
	go func() {
		decision <- ui.DecideFileFailure(ctx, FileFailure{Path: "bin/core.dll", Error: "bad hash", Important: true})
	}()
	<-posted
	ui.handleMessage(ui.hwnd, wmAppFileFailure, 0, 0)

	if !ui.handleKeyboardMessage('s', 0) {
		t.Fatal("important-file shortcut should be consumed")
	}
	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcSkipAll), 0)
	select {
	case got := <-decision:
		t.Fatalf("invalid decision was submitted: %q", got)
	case <-time.After(50 * time.Millisecond):
	}

	ui.handleKeyboardMessage('r', 0)
	select {
	case got := <-decision:
		if got != FileFailureDecisionRetry {
			t.Fatalf("decision = %q, want retry", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retry decision was not released")
	}
}

func TestDialogUIShortcutsRespectForegroundRepeatAndState(t *testing.T) {
	ui := newDialogTestUI()
	foreground := uintptr(2)
	ui.foregroundWindowFunc = func() uintptr { return foreground }
	posted := make(chan uint32, 2)
	ui.postMessageFunc = func(_ uintptr, message uint32) { posted <- message }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decision := make(chan FileFailureDecision, 1)
	go func() {
		decision <- ui.DecideFileFailure(ctx, FileFailure{Path: "data/a.json", Error: "download failed"})
	}()
	<-posted
	ui.handleMessage(ui.hwnd, wmAppFileFailure, 0, 0)
	if ui.handleKeyboardMessage('s', 0) {
		t.Fatal("shortcut was intercepted while another window was foreground")
	}
	foreground = ui.hwnd
	if !ui.handleKeyboardMessage('S', uintptr(1)<<30) {
		t.Fatal("auto-repeat should be consumed")
	}
	select {
	case got := <-decision:
		t.Fatalf("auto-repeat submitted decision %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	if !ui.handleKeyboardMessage('s', 0) {
		t.Fatal("lowercase shortcut was not accepted")
	}
	if got := <-decision; got != FileFailureDecisionSkip {
		t.Fatalf("decision = %q, want skip", got)
	}
	if !ui.handleKeyboardMessage('a', 0) {
		t.Fatal("non-decision shortcut should be consumed while the window is foreground")
	}

	ctx2, cancel2 := context.WithCancel(context.Background())
	decision2 := make(chan FileFailureDecision, 1)
	go func() {
		decision2 <- ui.DecideFileFailure(ctx2, FileFailure{Path: "data/b.json", Error: "download failed"})
	}()
	<-posted
	ui.handleMessage(ui.hwnd, wmAppFileFailure, 0, 0)
	ui.handleKeyboardMessage('a', uintptr(1)<<30)
	select {
	case got := <-decision2:
		t.Fatalf("repeat from previous key submitted decision %q", got)
	case <-time.After(50 * time.Millisecond):
	}
	cancel2()
	if got := <-decision2; got != FileFailureDecisionCancel {
		t.Fatalf("cancelled decision = %q, want cancel", got)
	}
}

func TestDialogUIStartWorkResetsFileFailureState(t *testing.T) {
	ui := newDialogTestUI()
	ui.hwnd = 0
	ui.hadFileFailure = true
	ui.fallbackVisible = true
	ui.fileFailureActive = true
	workDone := make(chan struct{})
	ui.workFunc = func(context.Context) error {
		close(workDone)
		return nil
	}
	ui.ctxFunc = func() context.Context { return context.Background() }
	ui.postMessageFunc = func(uintptr, uint32) {}
	ui.startWork()
	if ui.hadFileFailure || ui.fallbackVisible || ui.fileFailureActive {
		t.Fatal("per-run failure state was not reset")
	}
	select {
	case <-workDone:
	case <-time.After(2 * time.Second):
		t.Fatal("test work did not finish")
	}
}

func newDialogTextTestUI() (*DialogUI, *string) {
	ui := newDialogTestUI()
	ui.hErrorEdit = 77
	text := new(string)
	ui.setTextFunc = func(hwnd uintptr, value string) {
		if hwnd == ui.hErrorEdit {
			*text = value
		}
	}
	ui.sendMessageFunc = func(hwnd uintptr, message uint32, _ uintptr, lParam uintptr) uintptr {
		if hwnd != ui.hErrorEdit {
			return 0
		}
		switch message {
		case wmGetTextLength:
			return uintptr(len(*text))
		case emReplaceSel:
			if lParam != 0 {
				units := unsafe.Slice((*uint16)(unsafe.Pointer(lParam)), 4096)
				for i, unit := range units {
					if unit == 0 {
						*text += syscall.UTF16ToString(units[:i])
						break
					}
				}
			}
		}
		return 0
	}
	return ui, text
}

func TestDialogUIFallbackVisibilityMatchesResult(t *testing.T) {
	partial := NewUpdatePartialError([]SkippedFile{{Path: "data/a.json", Error: "bad hash"}})
	tests := []struct {
		name           string
		result         error
		hadFileFailure bool
		wantFallback   bool
	}{
		{name: "success", result: nil},
		{name: "no update", result: ErrNoUpdate},
		{name: "cancel before failure", result: ErrUserCancelled},
		{name: "cancel after failure", result: ErrUserCancelled, hadFileFailure: true, wantFallback: true},
		{name: "partial", result: partial, wantFallback: true},
		{name: "failure", result: errors.New("network down"), wantFallback: true},
		{name: "self update handoff", result: ErrSelfUpdateHandoff},
	}
	oldDestroy := destroyWin
	destroyWin = func(uintptr) {}
	defer func() { destroyWin = oldDestroy }()

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ui, _ := newDialogTextTestUI()
			ui.result = tt.result
			ui.hadFileFailure = tt.hadFileFailure
			ui.hwnd = 1
			ui.handleDone()
			if ui.fallbackVisible != tt.wantFallback {
				t.Fatalf("fallbackVisible = %v, want %v", ui.fallbackVisible, tt.wantFallback)
			}
		})
	}
}

func TestDialogUIFallbackActionsUseFixedInfoAndPreserveErrors(t *testing.T) {
	ui, text := newDialogTextTestUI()
	ui.fallbackVisible = true
	var opened string
	var copied string
	ui.openURLFunc = func(url string) error {
		opened = url
		return nil
	}
	ui.copyTextFunc = func(value string) error {
		copied = value
		return nil
	}
	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcOpenFallback), 0)
	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcCopyFallback), 0)
	if opened != FullPackageURL {
		t.Fatalf("opened URL = %q, want %q", opened, FullPackageURL)
	}
	for _, want := range []string{FullPackageName, FullPackageURL, FullPackageExtractCode} {
		if !strings.Contains(copied, want) {
			t.Fatalf("copied text %q does not contain %q", copied, want)
		}
	}

	*text = "原始下载错误\r\n" + FallbackNotice()
	ui.openURLFunc = func(string) error { return errors.New("browser unavailable") }
	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcOpenFallback), 0)
	for _, want := range []string{"原始下载错误", FullPackageURL, "browser unavailable"} {
		if !strings.Contains(*text, want) {
			t.Fatalf("error text %q does not contain %q", *text, want)
		}
	}
}

func TestDialogUIFileFailureCancellationPreservesFailureText(t *testing.T) {
	for _, render := range []bool{false, true} {
		t.Run(fmt.Sprintf("rendered=%t", render), func(t *testing.T) {
			ui, text := newDialogTextTestUI()
			posted := make(chan uint32, 1)
			ui.postMessageFunc = func(_ uintptr, message uint32) { posted <- message }
			ctx, cancel := context.WithCancel(context.Background())
			decision := make(chan FileFailureDecision, 1)
			go func() {
				decision <- ui.DecideFileFailure(ctx, FileFailure{
					Path:  "data/cancelled.json",
					Error: "download failed",
				})
			}()

			if message := <-posted; message != wmAppFileFailure {
				t.Fatalf("posted message = 0x%x", message)
			}
			if render {
				ui.handleMessage(ui.hwnd, wmAppFileFailure, 0, 0)
			}
			cancel()
			select {
			case got := <-decision:
				if got != FileFailureDecisionCancel {
					t.Fatalf("decision = %q, want cancel", got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("cancelled file failure decision was not released")
			}

			ui.result = ErrUserCancelled
			ui.handleDone()
			if got := strings.Count(*text, "文件下载或校验失败："); got != 1 {
				t.Fatalf("failure text count = %d, want 1; text=%q", got, *text)
			}
			if !strings.Contains(*text, "data/cancelled.json") {
				t.Fatalf("failure path missing from text %q", *text)
			}
		})
	}
}

func TestDialogUICloseButtonUsesFileFailureCancelPath(t *testing.T) {
	ui := newDialogTestUI()
	posted := make(chan uint32, 1)
	ui.postMessageFunc = func(_ uintptr, message uint32) { posted <- message }
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	decision := make(chan FileFailureDecision, 1)
	go func() {
		decision <- ui.DecideFileFailure(ctx, FileFailure{Path: "data/close.json", Error: "download failed"})
	}()
	if message := <-posted; message != wmAppFileFailure {
		t.Fatalf("posted message = 0x%x", message)
	}

	var destroyed uintptr
	oldDestroy := destroyWin
	destroyWin = func(hwnd uintptr) { destroyed = hwnd }
	defer func() { destroyWin = oldDestroy }()

	ui.handleMessage(ui.hwnd, WM_COMMAND, uintptr(idcCloseBtn), 0)
	select {
	case got := <-decision:
		if got != FileFailureDecisionCancel {
			t.Fatalf("decision = %q, want cancel", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close-button decision was not released")
	}
	if destroyed != 0 {
		t.Fatalf("close button destroyed window during file failure: %d", destroyed)
	}
}

func TestDialogUISelfUpdateHandoffClearsFallbackState(t *testing.T) {
	ui, _ := newDialogTextTestUI()
	ui.hwnd = 1
	ui.fallbackVisible = true
	ui.result = ErrSelfUpdateHandoff

	oldDestroy := destroyWin
	destroyWin = func(uintptr) {}
	defer func() { destroyWin = oldDestroy }()

	ui.handleDone()
	if ui.fallbackVisible {
		t.Fatal("self-update handoff retained fallback state")
	}
}
