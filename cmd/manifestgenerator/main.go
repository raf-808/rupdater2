package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"updater/internal/updatercore"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("ManifestGenerator", flag.ContinueOnError)
	flags.SetOutput(stderr)
	dir := flags.String("dir", "", "要扫描的发布目录")
	version := flags.String("version", "", "发布版本号")
	output := flags.String("output", "", "manifest.json 输出路径")
	if err := flags.Parse(args); err != nil {
		return 2
	}

	if *dir == "" || *version == "" || *output == "" {
		fmt.Fprintln(stderr, "用法：ManifestGenerator.exe --dir <发布目录> --version <版本号> --output <manifest.json>")
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
	manifest, summary, err := updatercore.GenerateManifestWithPolicy(*dir, *version, policy)
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
