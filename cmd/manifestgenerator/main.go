package main

import (
	"flag"
	"fmt"
	"os"

	"updater/internal/updatercore"
)

func main() {
	dir := flag.String("dir", "", "要扫描的发布目录")
	version := flag.String("version", "", "发布版本号")
	output := flag.String("output", "", "manifest.json 输出路径")
	flag.Parse()

	if *dir == "" || *version == "" || *output == "" {
		fmt.Fprintln(os.Stderr, "用法：ManifestGenerator.exe --dir <发布目录> --version <版本号> --output <manifest.json>")
		os.Exit(2)
	}

	manifest, err := updatercore.GenerateManifest(*dir, *version)
	if err != nil {
		fmt.Fprintf(os.Stderr, "生成清单失败：%v\n", err)
		os.Exit(1)
	}
	if err := updatercore.WriteJSONAtomic(*output, manifest); err != nil {
		fmt.Fprintf(os.Stderr, "写入清单失败：%v\n", err)
		os.Exit(1)
	}
	fmt.Printf("已生成清单：%s（版本：%s，文件数：%d）\n", *output, manifest.Version, len(manifest.Files))
}
