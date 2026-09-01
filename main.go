package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"updater/internal/updatercore"
)

func main() {
	rootDir := flag.String("root", "", "安装根目录，默认使用 Updater.exe 所在目录")
	silent := flag.Bool("silent", false, "静默模式，自动确认更新")
	debug := flag.Bool("debug", false, "输出调试信息")
	yes := flag.Bool("yes", false, "自动确认用户提示")
	workers := flag.Int("workers", 16, "并发工作线程数（Plan 扫描和下载阶段共用）")
	showVersion := flag.Bool("version", false, "显示版本")
	completeSelfUpdate := flag.Bool("complete-self-update", false, "内部参数：完成 Updater.exe 自更新")
	skipSelfUpdate := flag.Bool("skip-self-update", false, "内部参数：跳过本次自更新交接检查")
	selfTarget := flag.String("self-target", "", "内部参数：自更新目标路径")
	selfPending := flag.String("self-pending", "", "内部参数：待安装更新器路径")
	flag.Parse()

	if *showVersion {
		fmt.Println(updatercore.ProgramVersion)
		return
	}

	opts := updatercore.Options{
		RootDir:            *rootDir,
		Silent:             *silent,
		Debug:              *debug,
		AutoConfirm:        *yes || *silent,
		Workers:            *workers,
		CompleteSelfUpdate: *completeSelfUpdate,
		SkipSelfUpdate:     *skipSelfUpdate,
		SelfUpdateTarget:   *selfTarget,
		SelfUpdatePending:  *selfPending,
	}

	err := updatercore.Run(context.Background(), opts)
	if code := reportResult(err, os.Stderr); code != 0 {
		os.Exit(code)
	}
}

// reportResult is the process result boundary. It writes failure output
// directly to stderr so Silent mode cannot hide the summary through UI.Info.
func reportResult(err error, stderr io.Writer) int {
	result := updatercore.ClassifyResult(err)
	switch result {
	case updatercore.ResultSuccess, updatercore.ResultNoUpdate, updatercore.ResultSelfUpdateHandoff:
		return updatercore.ExitCode(err)
	case updatercore.ResultCancelled:
		fmt.Fprintln(stderr, "用户已取消更新。")
		return updatercore.ExitCode(err)
	case updatercore.ResultPartial:
		fmt.Fprintf(stderr, "%v\n", err)
		if updatercore.ShouldShowFallback(err, false) {
			updatercore.WriteFallbackNotice(stderr)
		}
		return updatercore.ExitCode(err)
	default:
		fmt.Fprintf(stderr, "更新失败：%v\n", err)
		if updatercore.ShouldShowFallback(err, false) {
			updatercore.WriteFallbackNotice(stderr)
		}
		return updatercore.ExitCode(err)
	}
}
