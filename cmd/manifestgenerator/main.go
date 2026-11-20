package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"updater/internal/updatercore"
)

// defaultManifestWorkers 是生成器专属的 --workers 默认值：规格 §3.2 固定为 16，
// 不按 CPU 核数/GOMAXPROCS 推导（与 Updater 的 -workers 默认 4 是两个程序各自参数）。
const defaultManifestWorkers = 16

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ManifestGenerator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "要扫描的发布目录")
	version := flags.String("version", "", "发布版本号")
	output := flags.String("output", "", "manifest.json 输出路径")
	workers := flags.Int("workers", defaultManifestWorkers, "哈希并发 worker 数（1-256，默认 16；1 为串行）")
	if err := flags.Parse(args); err != nil {
		// 标准库 flag 已把解析错误（含 --help/-h 的默认用法说明）写入 stderr；退出码保持 2。
		return 2
	}

	if *dir == "" || *version == "" || *output == "" {
		fmt.Fprintln(stderr, "用法：ManifestGenerator.exe --dir <发布目录> --version <版本号> --output <manifest.json>")
		fmt.Fprintln(stderr, "可选参数：--workers <N>（哈希并发 worker 数，1-256，默认 16；1 为串行）")
		return 2
	}

	// workers 范围校验：必填参数检查之后、策略加载之前；CLI 拒绝 <=0，
	// 不沿用核心 API 的 <=0 归一化语义（两个边界契约不得混淆）。
	if *workers < 1 || *workers > updatercore.MaxManifestHashWorkers {
		fmt.Fprintf(stderr, "--workers 必须是 1 到 %d 的整数（收到：%d）\n",
			updatercore.MaxManifestHashWorkers, *workers)
		return 2
	}

	policyPath := filepath.Join(*dir, updatercore.PolicyFileName)
	policy, err := updatercore.LoadFilePolicy(policyPath)
	if err != nil {
		fmt.Fprintf(stderr, "policy_load_failed source=local policy_file=%s failure_reason=%s\n",
			updatercore.PolicyFileName, updatercore.PolicyFailureReason(err))
		fmt.Fprintf(stderr, "加载策略失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout, "policy_load_success source=local schema_version=%d policy_file=%s\n",
		policy.SchemaVersion, updatercore.PolicyFileName)
	// 唯一新增的正常日志：紧随 policy_load_success、调用生成 API 前，恰好一次；
	// 记录校验通过的请求值（非 min 后实际池大小），显式 1 显示 1，小目录请求 16 仍显示 16。
	fmt.Fprintf(stdout, "manifest_workers workers=%d\n", *workers)
	manifest, summary, err := updatercore.GenerateManifestWithPolicyWorkers(*dir, *version, policy, *workers)
	if err != nil {
		fmt.Fprintf(stderr, "生成清单失败：%v\n", err)
		return 1
	}
	if err := updatercore.WriteJSONAtomic(*output, manifest); err != nil {
		fmt.Fprintf(stderr, "写入清单失败：%v\n", err)
		return 1
	}
	fmt.Fprintf(stdout,
		"policy_manifest_summary scanned_count=%d included_count=%d excluded_count=%d\n",
		summary.ScannedCount, summary.IncludedCount, summary.ExcludedCount)
	fmt.Fprintf(stdout, "已生成清单：%s（版本：%s，文件数：%d）\n", *output, manifest.Version, len(manifest.Files))
	return 0
}
