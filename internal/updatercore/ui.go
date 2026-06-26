package updatercore

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
)

type UI interface {
	ConfirmPlan(plan Plan) bool
	ConfirmProcessTermination(files []LockedFile) bool
	Progress(event ProgressEvent)
	Info(message string)
	Error(message string)
	ShowVersionInfo(current, latest string)
}

// GUIRunner is implemented by UIs that need a message loop on the main thread.
type GUIRunner interface {
	RunMessageLoop(work func(context.Context) error, ctx context.Context) error
}

// CancelSetter is implemented by UIs that can trigger context cancellation.
type CancelSetter interface {
	SetCancel(cancel context.CancelFunc)
}

type ConsoleUI struct {
	In          io.Reader
	Out         io.Writer
	AutoConfirm bool
	Silent      bool
}

func NewConsoleUI(autoConfirm, silent bool) *ConsoleUI {
	return &ConsoleUI{
		In:          os.Stdin,
		Out:         os.Stdout,
		AutoConfirm: autoConfirm,
		Silent:      silent,
	}
}

func (ui *ConsoleUI) ConfirmPlan(plan Plan) bool {
	message := fmt.Sprintf("当前版本：%s\n最新版本：%s\n\n新增：%d\n修改：%d\n删除：%d\n下载大小：%s\n\n是否开始更新？请输入“是”继续：",
		emptyAsUnknown(plan.CurrentVersion), plan.LatestVersion, len(plan.Add), len(plan.Modify), len(plan.Delete), formatBytes(plan.DownloadSize))
	return ui.confirm(message)
}

func (ui *ConsoleUI) ConfirmProcessTermination(files []LockedFile) bool {
	var builder strings.Builder
	builder.WriteString("以下文件正在被占用：\n")
	for _, file := range files {
		fmt.Fprintf(&builder, "- %s（进程：%s，PID：%d）\n", file.Path, emptyAsUnknown(file.ProcessName), file.PID)
	}
	builder.WriteString("\n是否允许更新器关闭上述进程后继续更新？请输入“是”继续：")
	return ui.confirm(builder.String())
}

func (ui *ConsoleUI) Progress(event ProgressEvent) {
	if ui.Silent {
		return
	}
	switch event.Phase {
	case "Check":
		fmt.Fprintln(ui.Out, "正在检查远端版本与清单")
	case "Plan":
		if event.TotalFiles > 0 {
			fmt.Fprintf(ui.Out, "正在扫描本地文件：%s（%d/%d）\n", event.CurrentFile, event.CompletedFiles, event.TotalFiles)
			return
		}
		fmt.Fprintln(ui.Out, "正在生成更新计划")
	case "Stage":
		fmt.Fprintf(ui.Out, "正在下载：%s（%d/%d）\n", event.CurrentFile, event.CompletedFiles, event.TotalFiles)
	case "OccupancyCheck":
		fmt.Fprintln(ui.Out, "正在检查文件占用状态")
	case "Backup":
		fmt.Fprintf(ui.Out, "正在备份：%s\n", event.CurrentFile)
	case "Switch":
		fmt.Fprintf(ui.Out, "正在切换：%s\n", event.CurrentFile)
	case "Commit":
		if strings.TrimSpace(event.CurrentFile) != "" {
			fmt.Fprintf(ui.Out, "正在提交更新结果：%s\n", event.CurrentFile)
			return
		}
		fmt.Fprintln(ui.Out, "正在提交更新结果")
	case "Recover":
		fmt.Fprintf(ui.Out, "正在恢复：%s\n", event.CurrentFile)
	default:
		fmt.Fprintln(ui.Out, event.Phase)
	}
}

func (ui *ConsoleUI) Info(message string) {
	if ui.Silent {
		return
	}
	fmt.Fprintln(ui.Out, message)
}

func (ui *ConsoleUI) Error(message string) {
	fmt.Fprintln(ui.Out, message)
}

func (ui *ConsoleUI) ShowVersionInfo(current, latest string) {
	if ui.Silent {
		return
	}
	fmt.Fprintf(ui.Out, "当前版本：%s  最新版本：%s\n", emptyAsUnknown(current), latest)
}

func (ui *ConsoleUI) confirm(message string) bool {
	if ui.AutoConfirm {
		if !ui.Silent {
			fmt.Fprintln(ui.Out, message)
			fmt.Fprintln(ui.Out, "是")
		}
		return true
	}
	fmt.Fprintln(ui.Out, message)
	scanner := bufio.NewScanner(ui.In)
	if !scanner.Scan() {
		return false
	}
	answer := strings.TrimSpace(scanner.Text())
	return answer == "是" || answer == "y" || answer == "Y"
}

func emptyAsUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "未知"
	}
	return value
}

func formatBytes(value int64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := int64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(value)/float64(div), "KMGTPE"[exp])
}
