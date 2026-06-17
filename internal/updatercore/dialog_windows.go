//go:build windows

package updatercore

import (
	"syscall"
	"unsafe"
)

const (
	mbOK              = 0x00000000
	mbOKCancel        = 0x00000001
	mbIconInformation = 0x00000040
	mbIconWarning     = 0x00000030
	idOK              = 1
)

var (
	user32            = syscall.NewLazyDLL("user32.dll")
	procMessageBoxW   = user32.NewProc("MessageBoxW")
	procGetConsoleWin = syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow")
)

type DialogUI struct {
	*ConsoleUI
}

func DefaultUI(autoConfirm, silent bool) UI {
	if silent {
		return NewConsoleUI(autoConfirm, silent)
	}
	return &DialogUI{ConsoleUI: NewConsoleUI(autoConfirm, silent)}
}

func (ui *DialogUI) ConfirmPlan(plan Plan) bool {
	if ui.AutoConfirm {
		return ui.ConsoleUI.ConfirmPlan(plan)
	}
	message := "当前版本：" + emptyAsUnknown(plan.CurrentVersion) +
		"\n最新版本：" + plan.LatestVersion +
		"\n\n新增：" + itoa(len(plan.Add)) +
		"\n修改：" + itoa(len(plan.Modify)) +
		"\n删除：" + itoa(len(plan.Delete)) +
		"\n下载大小：" + formatBytes(plan.DownloadSize) +
		"\n\n是否开始更新？"
	return messageBox("U-GeminiServer 更新器", message, mbOKCancel|mbIconInformation) == idOK
}

func (ui *DialogUI) ConfirmProcessTermination(files []LockedFile) bool {
	if ui.AutoConfirm {
		return ui.ConsoleUI.ConfirmProcessTermination(files)
	}
	message := "以下文件正在被占用：\n"
	for _, file := range files {
		message += "- " + file.Path + "（进程：" + emptyAsUnknown(file.ProcessName) + "，PID：" + itoa(file.PID) + "）\n"
	}
	message += "\n是否允许更新器关闭上述进程后继续更新？"
	return messageBox("文件正在被占用", message, mbOKCancel|mbIconWarning) == idOK
}

func (ui *DialogUI) Info(message string) {
	ui.ConsoleUI.Info(message)
}

func (ui *DialogUI) Error(message string) {
	messageBox("更新失败", message, mbOK|mbIconWarning)
	ui.ConsoleUI.Error(message)
}

func messageBox(title, text string, flags uintptr) int {
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	textPtr, _ := syscall.UTF16PtrFromString(text)
	hwnd, _, _ := procGetConsoleWin.Call()
	ret, _, _ := procMessageBoxW.Call(hwnd, uintptr(unsafe.Pointer(textPtr)), uintptr(unsafe.Pointer(titlePtr)), flags)
	return int(ret)
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
