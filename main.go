package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"updater/internal/updatercore"
)

func main() {
	rootDir := flag.String("root", "", "安装根目录，默认使用 Updater.exe 所在目录")
	silent := flag.Bool("silent", false, "静默模式，自动确认更新")
	debug := flag.Bool("debug", false, "输出调试信息")
	yes := flag.Bool("yes", false, "自动确认用户提示")
	workers := flag.Int("workers", 4, "并发工作线程数（Plan 扫描和下载阶段共用）")
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
	switch {
	case err == nil:
		return
	case errors.Is(err, updatercore.ErrSelfUpdateHandoff):
		return
	case errors.Is(err, updatercore.ErrNoUpdate):
		return
	case errors.Is(err, updatercore.ErrUserCancelled):
		fmt.Fprintln(os.Stderr, "用户已取消更新。")
		os.Exit(2)
	default:
		fmt.Fprintf(os.Stderr, "更新失败：%v\n", err)
		os.Exit(1)
	}
}
