//go:build windows

package updatercore

import (
	"testing"
	"time"
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
