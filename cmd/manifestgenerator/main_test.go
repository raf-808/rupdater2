package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"updater/internal/updatercore"
)

func TestRunLoadsFixedRootPolicyAndRejectsMissingOrInvalidPolicy(t *testing.T) {
	for name, testCase := range map[string]struct {
		policyData    []byte
		failureReason string
	}{
		"missing": {failureReason: "policy_download_failed"},
		"invalid": {
			policyData:    []byte(`{"schema_version":1} {}`),
			failureReason: "policy_invalid_json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if testCase.policyData != nil {
				if err := os.WriteFile(filepath.Join(root, updatercore.PolicyFileName), testCase.policyData, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"--dir", root, "--version", "1", "--output", filepath.Join(root, "manifest.json")}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
			}
			if bytes.Count(stderr.Bytes(), []byte("policy_load_failed")) != 1 {
				t.Fatalf("policy_load_failed must occur exactly once: %s", stderr.String())
			}
			for _, field := range []string{
				"source=local",
				"policy_file=" + updatercore.PolicyFileName,
				"failure_reason=" + testCase.failureReason,
			} {
				if !bytes.Contains(stderr.Bytes(), []byte(field)) {
					t.Fatalf("missing %q in policy failure event: %s", field, stderr.String())
				}
			}
			if bytes.Contains(stdout.Bytes(), []byte("policy_load_success")) ||
				bytes.Contains(stdout.Bytes(), []byte("policy_load_failed")) ||
				bytes.Contains(stderr.Bytes(), []byte("policy_load_success")) {
				t.Fatalf("success/failure events are not mutually exclusive: stdout=%s stderr=%s", stdout.String(), stderr.String())
			}
		})
	}
}

func TestRunGeneratesManifestWithPolicyAndSummary(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, updatercore.PolicyFileName), []byte(`{
		"schema_version":1,
		"publish":{"include":{"extensions":[".exe"]}}
	}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "keep.exe"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "drop.bin"), []byte("drop"), 0o644); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(root, "manifest.json")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"--dir", root, "--version", "1", "--output", output}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
	}
	var manifest updatercore.Manifest
	if err := updatercore.ReadJSONFile(output, &manifest); err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 2 || manifest.Files[0].Path != "keep.exe" ||
		manifest.Files[1].Path != updatercore.PolicyFileName {
		t.Fatalf("unexpected manifest: %#v", manifest.Files)
	}
	if bytes.Count(stdout.Bytes(), []byte("policy_load_success")) != 1 {
		t.Fatalf("policy_load_success must occur exactly once: %s", stdout.String())
	}
	for _, field := range []string{
		"source=local",
		"schema_version=1",
		"policy_file=" + updatercore.PolicyFileName,
	} {
		if !bytes.Contains(stdout.Bytes(), []byte(field)) {
			t.Fatalf("missing %q in policy success event: %s", field, stdout.String())
		}
	}
	if bytes.Contains(stdout.Bytes(), []byte("policy_load_failed")) ||
		bytes.Contains(stderr.Bytes(), []byte("policy_load_success")) ||
		bytes.Contains(stderr.Bytes(), []byte("policy_load_failed")) {
		t.Fatalf("success/failure events are not mutually exclusive: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("policy_manifest_summary")) ||
		!bytes.Contains(stdout.Bytes(), []byte("scanned_count=3 included_count=2 excluded_count=1")) {
		t.Fatalf("missing summary: %s", stdout.String())
	}
}

// =============================================================================
// 以下为 manifestgenerator-parallel-hash 工作包 W2 新增的 CLI Acceptance 测试。
// 规格依据：.trae/specs/manifestgenerator-parallel-hash/spec.md §3.2、§5 第 8 条、§7（T03/T09/T12/T13/T14）。
// 约束：默认 16、上限 256 按规格字面固定在测试常量中，不引用尚未导出的 updatercore 常量；
// 当前生产 CLI（main.go）尚不识别 --workers，新增测试在旧入口上应表现为行为 RED
// （运行时失败：flag 不识别、缺 workers 日志），而不是编译失败。
// =============================================================================

const (
	// w2CLIWorkersDefault / w2CLIWorkersMax：规格 §3.2 字面默认值 16 与上限 256。
	w2CLIWorkersDefault = 16
	w2CLIWorkersMax     = 256
	// w2CLIWorkersHelpText：规格 §3.2 规定的 --workers 帮助文案（逐字）。
	w2CLIWorkersHelpText = "哈希并发 worker 数（1-256，默认 16；1 为串行）"
	// w2PolicyJSON：与 W0 CLI fixture 一致的根策略固定字节（无换行）。
	w2PolicyJSON = `{"schema_version":1}`
	// w2UsageLine：当前生产 main.go 的既有用法行（W4 只能保留原文，另附 workers 说明）。
	w2UsageLine = "用法：ManifestGenerator.exe --dir <发布目录> --version <版本号> --output <manifest.json>"
	// w2RangeErrFormat：规格 §3.2 规定的 workers 范围错误文案（逐字，含全角标点）。
	w2RangeErrFormat = "--workers 必须是 1 到 256 的整数（收到：%d）"
)

// W0 CLI 金样来源：E\ManifestGenerator-before.exe（SHA-256
// 67a18cba527666662b285701597caa9941b92595fd59bb560a23cdd1ea6d0c50），
// 生成命令、fixture 与核验记录见
// test_results\manifestgenerator-parallel-hash\baseline\golden-cli-generation.md。
// 下列常量与 baseline 目录下的金样 JSON 逐字节一致，SHA-256 为 W0 实测值；
// 测试运行时先做常量自检，转写失真直接失败；不得依据新实现动态改写金样。
const (
	w2GoldenOrdinarySHA256   = "84ebb2014165e67705f7daabb2910ce436b9b4beaf4bbf7f668a1e1e02019ead"
	w2GoldenNestedSHA256     = "b76bf90d03b06ee9fb37c3fe3871eab0d3b095aebb40b5683d69a1a444dfd284"
	w2GoldenPolicyOnlySHA256 = "77c9301cc969e0683072e17b0840d0d318de92e6657de5f280aaf122d8d34ae6"
)

// w2GoldenOrdinaryJSON：baseline\golden-ordinary.json 原字节（含末尾换行）。
const w2GoldenOrdinaryJSON = `{
  "version": "parallel-hash-golden",
  "files": [
    {
      "path": "app.bin",
      "size": 3,
      "head_hash": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad",
      "tail_hash": "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
    },
    {
      "path": "updater-policy.json",
      "size": 20,
      "head_hash": "a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23",
      "tail_hash": "a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23"
    }
  ]
}
`

// w2GoldenNestedJSON：baseline\golden-nested.json 原字节（含末尾换行）。
const w2GoldenNestedJSON = `{
  "version": "parallel-hash-golden",
  "files": [
    {
      "path": "Dir A/中文/data.bin",
      "size": 6,
      "head_hash": "233562de1a0288b139c4fa40b7d189f806e906eeb048517aeb67f34ac0e2faf1",
      "tail_hash": "233562de1a0288b139c4fa40b7d189f806e906eeb048517aeb67f34ac0e2faf1"
    },
    {
      "path": "updater-policy.json",
      "size": 20,
      "head_hash": "a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23",
      "tail_hash": "a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23"
    },
    {
      "path": "z.bin",
      "size": 1,
      "head_hash": "594e519ae499312b29433b7dd8a97ff068defcba9755b6d5d00e84c524d67b06",
      "tail_hash": "594e519ae499312b29433b7dd8a97ff068defcba9755b6d5d00e84c524d67b06"
    }
  ]
}
`

// w2GoldenPolicyOnlyJSON：baseline\golden-policy-only.json 原字节（含末尾换行）。
// 注意：CLI 层策略文件强制入清单，policy-only 的合法结果必须是 1/1/0 且 files 非 null。
const w2GoldenPolicyOnlyJSON = `{
  "version": "parallel-hash-golden",
  "files": [
    {
      "path": "updater-policy.json",
      "size": 20,
      "head_hash": "a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23",
      "tail_hash": "a9d5f6d002d956b8af5787a05e0ca000d45c03977ffa54ee8fbed719fed5fd23"
    }
  ]
}
`

// w2LockScript：Windows 真实拒读子进程脚本（与 W1 同思路的局部 os/exec PowerShell 握手）。
// 被锁路径经环境变量 W2_LOCK_PATH 传入，避免中文/空格路径的命令行转义问题。
// 机制说明（本机实测，2026-11-18，Windows 10.0.26200）：
// 计划原文建议的 [IO.File]::Open(..., FileShare.None)（含裸 Win32 CreateFile dwShareMode=0）
// 在本机上无法阻止 Go 进程（os.Open/io.Copy）访问同一文件——MSYS 侧派生的 Go 进程打开
// 始终成功；而字节区间独占锁（FileStream.Lock，底层 LockFileEx）能在 Go 读取时稳定产生
// 真实 ERROR_LOCK_VIOLATION。T09 契约要求的是“HashFile 真实失败”，故采用区间锁方案，
// 并在测试内先用导出的 HashFile 自证锁确实拒读，前提不成立则测试失败。
// 流程：独占打开并锁定整个文件内容 → 输出 READY 握手 → 阻塞读取 stdin 一行（或关闭）
// → 解锁退出。
const w2LockScript = `$ErrorActionPreference = 'Stop'; ` +
	`$fs = [IO.File]::Open($env:W2_LOCK_PATH, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite); ` +
	`$fs.Lock(0, $fs.Length); ` +
	`[Console]::Out.WriteLine('READY'); [Console]::Out.Flush(); ` +
	`$null = [Console]::In.ReadLine(); $fs.Dispose(); exit 0`

// w2WriteFile 在指定路径写入内容（自动创建父目录），失败即测试失败。
func w2WriteFile(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// w2BuildRootWithPolicy 创建扫描根：固定根策略 + 附加文件（相对路径以斜杠书写）。
func w2BuildRootWithPolicy(t *testing.T, files map[string][]byte) string {
	t.Helper()
	root := t.TempDir()
	w2WriteFile(t, filepath.Join(root, updatercore.PolicyFileName), []byte(w2PolicyJSON))
	for rel, data := range files {
		w2WriteFile(t, filepath.Join(root, filepath.FromSlash(rel)), data)
	}
	return root
}

// w2ParamErrorFixtureVariants 参数错误场景的扫描根 fixture 变体（评审 C4 返工新增，
// 对齐 plan §4 W2 第 3 步：用不存在/无效策略 fixture 证明参数错误先于策略加载）：
//   - 有效策略：若实现先加载策略，会在 stdout 留下 policy_load_success；
//   - 缺失策略：若实现先加载策略，会在 stderr 留下 policy_load_failed（policy_download_failed）；
//   - 无效策略：若实现先加载策略，会在 stderr 留下 policy_load_failed（policy_invalid_json）。
//
// 三种变体下参数错误都必须仍退出 2、无任何策略/生成日志、不创建输出目录。
var w2ParamErrorFixtureVariants = []struct {
	name       string
	policyData []byte // nil 表示根目录不放策略文件
}{
	{name: "有效策略", policyData: []byte(w2PolicyJSON)},
	{name: "缺失策略", policyData: nil},
	{name: "无效策略", policyData: []byte(`{"schema_version":1} {}`)},
}

// w2BuildParamErrorRoot 按变体构造参数错误场景的扫描根。
func w2BuildParamErrorRoot(t *testing.T, policyData []byte) string {
	t.Helper()
	root := t.TempDir()
	if policyData != nil {
		w2WriteFile(t, filepath.Join(root, updatercore.PolicyFileName), policyData)
	}
	return root
}

// 三棵 W0 CLI fixture 的等价重建（字节与 baseline\fixtures 完全一致）。
func w2BuildGoldenOrdinaryRoot(t *testing.T) string {
	t.Helper()
	return w2BuildRootWithPolicy(t, map[string][]byte{"app.bin": []byte("abc")})
}

func w2BuildGoldenNestedRoot(t *testing.T) string {
	t.Helper()
	return w2BuildRootWithPolicy(t, map[string][]byte{
		"Dir A/中文/data.bin": []byte("nested"),
		"z.bin":             []byte("z"),
	})
}

func w2BuildGoldenPolicyOnlyRoot(t *testing.T) string {
	t.Helper()
	return w2BuildRootWithPolicy(t, nil)
}

// w2DirListing 递归列出目录内全部条目（相对路径，排序后返回），用于断言无新增文件。
func w2DirListing(t *testing.T, dir string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, relErr := filepath.Rel(dir, path)
		if relErr != nil {
			return relErr
		}
		names = append(names, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(names)
	return names
}

// w2AssertContains 断言缓冲区包含全部给定子串。
func w2AssertContains(t *testing.T, buf *bytes.Buffer, label string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if !bytes.Contains(buf.Bytes(), []byte(token)) {
			t.Fatalf("%s 应包含 %q，实际：%q", label, token, buf.String())
		}
	}
}

// w2AssertNotContains 断言缓冲区不包含任何给定子串。
func w2AssertNotContains(t *testing.T, buf *bytes.Buffer, label string, tokens ...string) {
	t.Helper()
	for _, token := range tokens {
		if bytes.Contains(buf.Bytes(), []byte(token)) {
			t.Fatalf("%s 不应包含 %q，实际：%q", label, token, buf.String())
		}
	}
}

// w2AssertExactLines 对缓冲区做精确逐行比较（评审 C2/C4 返工新增）：
// 缓冲区内容必须恰好由给定各行构成，不允许多余行、额外空行或其他内容；
// 结尾允许恰好一个换行（Go Fprintln 惯例），多于一个结尾换行视为多余空行判失败。
// 子串比较过弱（如 "manifest_workers workers=1" 会误匹配 "workers=16"），
// 涉及行数/行序/请求值契约的场景一律使用本函数。
func w2AssertExactLines(t *testing.T, buf *bytes.Buffer, label string, wantLines ...string) {
	t.Helper()
	lines := strings.Split(buf.String(), "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) != len(wantLines) {
		t.Fatalf("%s 行数不符：期望 %d 行，实际 %d 行\n期望：%q\n实际：%q",
			label, len(wantLines), len(lines), wantLines, lines)
	}
	for i, want := range wantLines {
		if lines[i] != want {
			t.Fatalf("%s 第 %d 行不符\n期望：%q\n实际行：%q\n全部实际行：%q",
				label, i+1, want, lines[i], lines)
		}
	}
}

// w2RequirePathAbsent 断言路径不存在（含父目录未创建）。
func w2RequirePathAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		t.Fatalf("路径不应存在：%s", path)
	} else if !os.IsNotExist(err) {
		t.Fatalf("检查路径 %s 失败：%v", path, err)
	}
}

// w2RequireNoTmpOrBak 断言目录清单中没有 .tmp/.bak 临时或备份文件。
func w2RequireNoTmpOrBak(t *testing.T, listing []string) {
	t.Helper()
	for _, name := range listing {
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".bak") {
			t.Fatalf("不应出现临时/备份文件：%s", name)
		}
	}
}

// w2ExpectedSuccessStdout 构造成功场景的完整 stdout 期望文本：
// 行序固定为 policy_load_success → manifest_workers → policy_manifest_summary → 已生成清单。
func w2ExpectedSuccessStdout(output, version, summaryLine string, workers, fileCount int) string {
	return "policy_load_success source=local schema_version=1 policy_file=updater-policy.json\n" +
		fmt.Sprintf("manifest_workers workers=%d\n", workers) +
		summaryLine + "\n" +
		fmt.Sprintf("已生成清单：%s（版本：%s，文件数：%d）\n", output, version, fileCount)
}

// w2LockFileExclusive 用 PowerShell 子进程对指定文件建立字节区间独占锁（LockFileEx），
// 使其他进程对文件内容的读取真实失败；返回释放函数（幂等，注册 t.Cleanup 兜底）。
// 仅 Windows 有效，其他平台跳过并如实记录。
func w2LockFileExclusive(t *testing.T, path string) (release func()) {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("Windows 字节区间独占锁仅在 Windows 上有效；非 Windows 平台该部分记录为未验收")
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", w2LockScript)
	cmd.Env = append(os.Environ(), "W2_LOCK_PATH="+path)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	readyCh := make(chan error, 1)
	go func() {
		line, readErr := bufio.NewReader(stdout).ReadString('\n')
		if readErr != nil {
			readyCh <- fmt.Errorf("读取 READY 失败：%v", readErr)
			return
		}
		if strings.TrimSpace(line) != "READY" {
			readyCh <- fmt.Errorf("首行不是 READY：%q", line)
			return
		}
		readyCh <- nil
	}()
	select {
	case err := <-readyCh:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			t.Fatalf("占用进程未能就绪：%v；stderr=%s", err, stderrBuf.String())
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("等待占用进程 READY 超时（30s）；stderr=%s", stderrBuf.String())
	}
	// 注意：不得把原始释放闭包赋给命名返回值 release（本函数签名为 (release func())），
	// 否则 return 时命名返回值被重新赋值， Once 包装闭包捕获的变量会被换成包装自身，
	// 造成 once.Do 递归自死锁。原始逻辑必须保存在独立局部变量中。
	rawRelease := func() {
		_, _ = stdin.Write([]byte("release\n"))
		_ = stdin.Close()
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}
	var once sync.Once
	release = func() { once.Do(rawRelease) }
	t.Cleanup(release)
	return release
}

// TestRunWorkersSuccessMatrixLogsRequestedWorkers 覆盖 T12 成功侧与 T03 合法边界（1/256）：
// 缺省应为默认 16；显式 1/16/256 均接受；标准单横线与等号形式保留。
// 规格要求：manifest_workers 行恰好一次，写 stdout，与 policy_load_success 同流，
// 紧随其后、生成调用前；记录的是请求值（小目录请求 16 仍记 16，256 仍记 256），
// 原 summary/成功行文本、流归属与顺序不变，stderr 为空、无额外日志。
// 有效输出写到扫描根之外的独立临时目录。
func TestRunWorkersSuccessMatrixLogsRequestedWorkers(t *testing.T) {
	cases := []struct {
		name        string
		extraArgs   []string
		wantWorkers int
	}{
		{name: "T12/缺省_默认16", extraArgs: nil, wantWorkers: w2CLIWorkersDefault},
		{name: "T12/显式1_串行回退", extraArgs: []string{"--workers", "1"}, wantWorkers: 1},
		{name: "T12/显式16", extraArgs: []string{"--workers", "16"}, wantWorkers: 16},
		{name: "T12/显式256_上限", extraArgs: []string{"--workers", "256"}, wantWorkers: w2CLIWorkersMax},
		{name: "T12/单横线空格_workers_16", extraArgs: []string{"-workers", "16"}, wantWorkers: 16},
		{name: "T12/单横线等号_workers=1", extraArgs: []string{"-workers=1"}, wantWorkers: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			root := w2BuildRootWithPolicy(t, map[string][]byte{"app.bin": []byte("abc")})
			outDir := t.TempDir() // 扫描根之外的输出目录
			output := filepath.Join(outDir, "manifest.json")
			version := "w2-success-matrix"
			args := []string{"--dir", root, "--version", version, "--output", output}
			args = append(args, tc.extraArgs...)
			var stdout, stderr bytes.Buffer
			code := run(args, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("退出码 = %d，期望 0；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if stderr.Len() != 0 {
				t.Fatalf("成功场景 stderr 必须为空：%q", stderr.String())
			}
			wantStdout := w2ExpectedSuccessStdout(output, version,
				"policy_manifest_summary scanned_count=2 included_count=2 excluded_count=0",
				tc.wantWorkers, 2)
			if got := stdout.String(); got != wantStdout {
				t.Fatalf("stdout 不符（行格式/字段/顺序契约）\n期望：\n%s\n实际：\n%s", wantStdout, got)
			}
			if got := strings.Count(stdout.String(), "manifest_workers workers="); got != 1 {
				t.Fatalf("manifest_workers 行应恰好一次，实际 %d 次：%s", got, stdout.String())
			}
			var manifest updatercore.Manifest
			if err := updatercore.ReadJSONFile(output, &manifest); err != nil {
				t.Fatal(err)
			}
			if manifest.Version != version {
				t.Fatalf("version 应原样保留 %q，实际 %q", version, manifest.Version)
			}
			if len(manifest.Files) != 2 || manifest.Files[0].Path != "app.bin" ||
				manifest.Files[1].Path != updatercore.PolicyFileName {
				t.Fatalf("输出清单内容不符：%#v", manifest.Files)
			}
		})
	}
}

// TestRunWorkersParseErrorsExit2WithoutPolicyIO 覆盖 T12 解析类错误：
// 非整数、整数溢出、缺 workers 值、未知参数 → 退出 2，标准 flag 错误写 stderr；
// 参数错误先于策略加载：不产生任何 policy_load_*/manifest_workers/summary/成功行，
// 也不创建输出父目录、输出文件或临时文件。
// 按评审 C4 对齐 plan：每个解析错误均在三种 fixture 变体（有效/缺失/无效策略）下验证，
// 若实现先于参数校验加载策略，三种变体必然分别在 stdout/stderr 留下可拦截的痕迹。
func TestRunWorkersParseErrorsExit2WithoutPolicyIO(t *testing.T) {
	cases := []struct {
		name        string
		extraArgs   []string
		wantErrText string
	}{
		{name: "T12/workers非整数abc", extraArgs: []string{"--workers", "abc"},
			wantErrText: `invalid value "abc" for flag -workers`},
		{name: "T12/workers整数溢出", extraArgs: []string{"--workers", "99999999999999999999"},
			wantErrText: `invalid value "99999999999999999999" for flag -workers: value out of range`},
		{name: "T12/workers缺值", extraArgs: []string{"--workers"},
			wantErrText: "flag needs an argument: -workers"},
		{name: "T12/未知参数verbose", extraArgs: []string{"--verbose"},
			wantErrText: "flag provided but not defined: -verbose"},
	}
	for _, tc := range cases {
		for _, variant := range w2ParamErrorFixtureVariants {
			t.Run(tc.name+"_"+variant.name, func(t *testing.T) {
				root := w2BuildParamErrorRoot(t, variant.policyData)
				outBase := t.TempDir()
				outParent := filepath.Join(outBase, "w2-missing-parent")
				output := filepath.Join(outParent, "manifest.json")
				args := []string{"--dir", root, "--version", "w2-parse", "--output", output}
				args = append(args, tc.extraArgs...)
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				if code != 2 {
					t.Fatalf("退出码 = %d，期望 2；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("解析错误时 stdout 必须为空：%q", stdout.String())
				}
				w2AssertContains(t, &stderr, "stderr", tc.wantErrText)
				w2AssertNotContains(t, &stdout, "stdout", "policy_load_", "manifest_workers", "policy_manifest_summary", "已生成清单")
				w2AssertNotContains(t, &stderr, "stderr", "policy_load_success", "policy_load_failed",
					"manifest_workers", "policy_manifest_summary", "已生成清单")
				w2RequirePathAbsent(t, outParent)
				w2RequirePathAbsent(t, output)
			})
		}
	}
}

// TestRunWorkersRangeErrorsExit2BeforePolicyLoad 覆盖 T03 的 CLI 侧边界：
// 显式 0、负数、257 均超出 1..256，退出 2，stderr 逐字给出规定文案；
// 范围校验发生在必填参数检查之后、策略加载之前。
// 按评审 C4 对齐 plan：每个范围错误均在三种 fixture 变体（有效/缺失/无效策略）下验证，
// 且 stderr 可直接精确比较整行——恰好一行规定文案（含结尾换行差异容忍），无任何额外行。
// API 的 <=0 归一化不迁移到 CLI：0/负数在 CLI 一律拒绝。
func TestRunWorkersRangeErrorsExit2BeforePolicyLoad(t *testing.T) {
	cases := []struct {
		name  string
		value int
	}{
		{name: "T12/显式0_范围拒绝", value: 0},
		{name: "T12/负数-1_范围拒绝", value: -1},
		{name: "T12/超上限257_范围拒绝", value: 257},
	}
	for _, tc := range cases {
		for _, variant := range w2ParamErrorFixtureVariants {
			t.Run(tc.name+"_"+variant.name, func(t *testing.T) {
				root := w2BuildParamErrorRoot(t, variant.policyData)
				outBase := t.TempDir()
				outParent := filepath.Join(outBase, "w2-missing-parent")
				output := filepath.Join(outParent, "manifest.json")
				args := []string{"--dir", root, "--version", "w2-range", "--output", output,
					"--workers", fmt.Sprintf("%d", tc.value)}
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				if code != 2 {
					t.Fatalf("退出码 = %d，期望 2；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("范围错误时 stdout 必须为空：%q", stdout.String())
				}
				// 范围错误整行精确比较：恰好一行规定文案，多一行/少一行/改一字都判失败。
				w2AssertExactLines(t, &stderr, "stderr", fmt.Sprintf(w2RangeErrFormat, tc.value))
				// 必填参数齐全时不应出现用法行（说明走的是范围分支而非缺参分支）。
				w2AssertNotContains(t, &stderr, "stderr", w2UsageLine)
				// 范围校验先于策略加载：不得出现任何策略/生成痕迹。
				w2AssertNotContains(t, &stdout, "stdout",
					"policy_load_success", "policy_load_failed", "manifest_workers", "policy_manifest_summary", "已生成清单")
				w2AssertNotContains(t, &stderr, "stderr",
					"policy_load_success", "policy_load_failed", "manifest_workers", "policy_manifest_summary", "已生成清单")
				w2RequirePathAbsent(t, outParent)
				w2RequirePathAbsent(t, output)
			})
		}
	}
}

// TestRunMissingRequiredArgsBeforeWorkersRange 覆盖 T12 缺参场景与检查顺序契约：
// dir/version/output 缺失（任一或全部）→ 退出 2，保留原用法行并另附 workers 可选说明；
// 必填参数检查先于 workers 范围校验：缺必填且 workers=0/257 时只报用法，不报范围错误；
// 全程不做策略 I/O，不创建输出父目录。
// 按评审 C4 对齐 plan：每个缺参场景均在三种 fixture 变体（有效/缺失/无效策略）下验证，
// 确认参数错误仍退出 2 且无论 fixture 状态如何都不做策略 I/O。
func TestRunMissingRequiredArgsBeforeWorkersRange(t *testing.T) {
	cases := []struct {
		name          string
		provideOutput bool
		makeArgs      func(root, output string) []string
	}{
		{name: "T12/缺dir", provideOutput: true, makeArgs: func(root, output string) []string {
			return []string{"--version", "v", "--output", output}
		}},
		{name: "T12/缺version", provideOutput: true, makeArgs: func(root, output string) []string {
			return []string{"--dir", root, "--output", output}
		}},
		{name: "T12/缺output", provideOutput: false, makeArgs: func(root, output string) []string {
			return []string{"--dir", root, "--version", "v"}
		}},
		{name: "T12/无任何参数", provideOutput: false, makeArgs: func(root, output string) []string {
			return nil
		}},
		{name: "T12/必填缺失先于workers范围_收到0", provideOutput: true, makeArgs: func(root, output string) []string {
			return []string{"--workers", "0", "--version", "v", "--output", output}
		}},
		{name: "T12/必填缺失先于workers范围_收到257", provideOutput: false, makeArgs: func(root, output string) []string {
			return []string{"--workers", "257", "--dir", root}
		}},
	}
	for _, tc := range cases {
		for _, variant := range w2ParamErrorFixtureVariants {
			t.Run(tc.name+"_"+variant.name, func(t *testing.T) {
				root := w2BuildParamErrorRoot(t, variant.policyData)
				outBase := t.TempDir()
				outParent := filepath.Join(outBase, "w2-missing-parent")
				output := filepath.Join(outParent, "manifest.json")
				args := tc.makeArgs(root, output)
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				if code != 2 {
					t.Fatalf("退出码 = %d，期望 2；args=%v stdout=%q stderr=%q", code, args, stdout.String(), stderr.String())
				}
				if stdout.Len() != 0 {
					t.Fatalf("缺参时 stdout 必须为空：%q", stdout.String())
				}
				// 保留原用法行，另附 workers 可选说明；必填缺失时不得出现 workers 范围错误。
				w2AssertContains(t, &stderr, "stderr", w2UsageLine, "--workers")
				w2AssertNotContains(t, &stderr, "stderr", "必须是 1 到 256")
				w2AssertNotContains(t, &stdout, "stdout",
					"policy_load_success", "policy_load_failed", "manifest_workers", "policy_manifest_summary", "已生成清单")
				w2AssertNotContains(t, &stderr, "stderr",
					"policy_load_success", "policy_load_failed", "manifest_workers", "policy_manifest_summary", "已生成清单")
				if tc.provideOutput {
					w2RequirePathAbsent(t, outParent)
					w2RequirePathAbsent(t, output)
				}
			})
		}
	}
}

// TestRunHelpShowsWorkersUsageAndExits2 覆盖 T12 帮助场景：
// --help/-h 保留 ContinueOnError → 退出 2 的既有行为；帮助内容必须包含原三个参数说明
// 及规格规定的 workers 完整文案；不加载策略，stdout 为空。
func TestRunHelpShowsWorkersUsageAndExits2(t *testing.T) {
	for _, helpFlag := range []string{"--help", "-h"} {
		t.Run("T12/help_"+strings.TrimLeft(helpFlag, "-"), func(t *testing.T) {
			root := w2BuildRootWithPolicy(t, map[string][]byte{"app.bin": []byte("abc")})
			var stdout, stderr bytes.Buffer
			code := run([]string{helpFlag, "--dir", root}, &stdout, &stderr)
			if code != 2 {
				t.Fatalf("%s 退出码 = %d，期望 2（保持既有 ContinueOnError 行为）", helpFlag, code)
			}
			if stdout.Len() != 0 {
				t.Fatalf("帮助输出应写 stderr，stdout 必须为空：%q", stdout.String())
			}
			w2AssertContains(t, &stderr, "stderr", w2CLIWorkersHelpText)
			// 原参数说明保留。
			w2AssertContains(t, &stderr, "stderr", "-workers", "要扫描的发布目录", "发布版本号", "manifest.json 输出路径")
			w2AssertNotContains(t, &stdout, "stdout",
				"policy_load_success", "policy_load_failed", "manifest_workers", "policy_manifest_summary", "已生成清单")
			w2AssertNotContains(t, &stderr, "stderr",
				"policy_load_success", "policy_load_failed", "manifest_workers", "policy_manifest_summary", "已生成清单")
		})
	}
}

// TestRunPolicyFailureLeavesOutputUntouched 覆盖 T13 策略失败场景：
// 策略缺失 → 退出 1，原 policy_load_failed 与中文错误格式不变，无 manifest_workers 行；
// 两种输出前置状态分别验证：
// ①已有输出为固定字节 → 失败后字节不变、目录清单不变、无 .tmp/.bak；
// ②新输出且父目录尚不存在 → 失败后父目录不被创建。
func TestRunPolicyFailureLeavesOutputUntouched(t *testing.T) {
	t.Run("T13/策略失败_已有输出固定字节不变", func(t *testing.T) {
		root := t.TempDir() // 不放策略文件 → policy_download_failed
		outDir := t.TempDir()
		output := filepath.Join(outDir, "manifest.json")
		frozen := []byte("w2-frozen-existing-output\n")
		w2WriteFile(t, output, frozen)
		before := w2DirListing(t, outDir)
		var stdout, stderr bytes.Buffer
		code := run([]string{"--dir", root, "--version", "w2-policy-fail", "--output", output}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		w2AssertContains(t, &stderr, "stderr", "policy_load_failed", "加载策略失败：")
		// 策略失败无 workers 行，也无 summary/成功行；stdout 保持为空。
		if stdout.Len() != 0 {
			t.Fatalf("策略失败时 stdout 必须为空：%q", stdout.String())
		}
		w2AssertNotContains(t, &stderr, "stderr",
			"manifest_workers", "policy_manifest_summary", "已生成清单", "生成清单失败", "写入清单失败")
		got, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, frozen) {
			t.Fatalf("既有输出字节被改动：%q", got)
		}
		after := w2DirListing(t, outDir)
		if !slices.Equal(before, after) {
			t.Fatalf("输出目录出现新增文件：before=%v after=%v", before, after)
		}
		w2RequireNoTmpOrBak(t, after)
	})
	t.Run("T13/策略失败_新输出父目录不创建", func(t *testing.T) {
		root := t.TempDir()
		outParent := filepath.Join(t.TempDir(), "w2-missing-parent")
		output := filepath.Join(outParent, "manifest.json")
		var stdout, stderr bytes.Buffer
		code := run([]string{"--dir", root, "--version", "w2-policy-fail", "--output", output}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		w2AssertContains(t, &stderr, "stderr", "policy_load_failed")
		w2AssertNotContains(t, &stderr, "stderr", "manifest_workers", "policy_manifest_summary", "已生成清单")
		w2RequirePathAbsent(t, outParent)
		w2RequirePathAbsent(t, output)
	})
}

// TestRunLockedDataFileAbortsGenerationWithoutOutput 覆盖 T09（CLI 层，Windows 真实拒读）：
// 用 PowerShell 子进程对被 publish 包含的普通数据文件建立字节区间独占锁
// （不占用策略文件，释放路径见文件内 w2LockScript 说明）。
// HashFile 拒读预验证放在父级 setup（评审 C1 返工）：子测 Fatal 不会终止父测试，
// 且单独筛选生成失败子测时会跳过子测内的预验证，故前提校验必须在父级无条件执行，
// 前提不成立立即终止整个场景；同时保存真实底层错误供生成失败断言逐字比对（评审 C2）。
// 再验证 CLI 生成失败退出 1、stdout 精确两行（策略成功 + 对应请求值 workers）、
// stderr 精确一行（原前缀 + 真实底层错误）、无 summary/成功行、
// 输出不被触碰、.tmp/.bak 零产生（显式 1 与缺省默认 16 分别验证）；
// 新输出/父目录不存在同样覆盖 workers=1 与缺省默认 16（评审 C3 返工）；
// 最后释放占用并显式验证全部扫描输入文件均可重命名、可删除（评审 C3 返工）。
func TestRunLockedDataFileAbortsGenerationWithoutOutput(t *testing.T) {
	root := t.TempDir()
	policyPath := filepath.Join(root, updatercore.PolicyFileName)
	w2WriteFile(t, policyPath, []byte(w2PolicyJSON))
	lockedPath := filepath.Join(root, "data.bin")
	w2WriteFile(t, lockedPath, []byte("locked-data"))
	otherPath := filepath.Join(root, "other.bin")
	w2WriteFile(t, otherPath, []byte("other"))
	release := w2LockFileExclusive(t, lockedPath)

	// 父级 setup：拒读预验证是本场景全部子测的强制前置（评审 C1 返工）。
	// 直接 HashFile 必须真实失败（读不失败即前提不成立，立即终止整个场景），
	// 且错误必须是真实文件系统错误（*fs.PathError）并指向被锁文件本身，
	// 排除合成错误或错误包装冒充拒读。
	_, _, _, lockErr := updatercore.HashFile(lockedPath)
	if lockErr == nil {
		t.Fatalf("字节区间锁未能阻止 HashFile 读取，T09 前提不成立，不得继续")
	}
	var pathErr *fs.PathError
	if !errors.As(lockErr, &pathErr) {
		t.Fatalf("预验证错误应为真实文件系统错误（*fs.PathError），实际 %T：%v", lockErr, lockErr)
	}
	if pathErr.Path != lockedPath {
		t.Fatalf("预验证错误路径应指向被锁文件 %s，实际：%s", lockedPath, pathErr.Path)
	}
	// 生成失败走同一 HashFile 路径且不包装原错误（规格 §5 第 5/6 条），
	// 故预验证捕获的错误文本即 CLI stderr 应逐字出现的真实底层错误。
	lockErrText := lockErr.Error()

	// 已有输出固定字节：生成失败场景共用同一冻结输出，逐次核对字节与目录清单不变。
	outDir := t.TempDir()
	output := filepath.Join(outDir, "manifest.json")
	frozen := []byte("w2-frozen-existing-output\n")
	w2WriteFile(t, output, frozen)
	before := w2DirListing(t, outDir)

	// assertGenerationFailed（评审 C2 返工）：生成失败场景输出精确逐行比较——
	// stdout 恰好两行：策略成功行 + 对应请求值的 workers 行（各恰好一次、无额外行；
	// 原 `manifest_workers workers=1` 子串断言会误匹配 workers=16，已弃用）；
	// stderr 恰好一行：原 `生成清单失败：` 前缀 + 预验证捕获的真实底层错误。
	// 同时核对既有输出字节不变、目录清单不变、无 .tmp/.bak。
	assertGenerationFailed := func(t *testing.T, stdout, stderr *bytes.Buffer, wantWorkersLine string) {
		t.Helper()
		w2AssertExactLines(t, stdout, "stdout",
			"policy_load_success source=local schema_version=1 policy_file=updater-policy.json",
			wantWorkersLine)
		w2AssertExactLines(t, stderr, "stderr", "生成清单失败："+lockErrText)
		got, err := os.ReadFile(output)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, frozen) {
			t.Fatalf("既有输出字节被改动：%q", got)
		}
		after := w2DirListing(t, outDir)
		if !slices.Equal(before, after) {
			t.Fatalf("输出目录出现新增文件：before=%v after=%v", before, after)
		}
		w2RequireNoTmpOrBak(t, after)
	}

	t.Run("T09/workers1_生成失败退出1_已有输出不变", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--dir", root, "--version", "w2-lock", "--output", output, "--workers", "1"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		assertGenerationFailed(t, &stdout, &stderr, "manifest_workers workers=1")
	})

	t.Run("T09/缺省默认16_生成失败退出1_已有输出不变", func(t *testing.T) {
		var stdout, stderr bytes.Buffer
		code := run([]string{"--dir", root, "--version", "w2-lock", "--output", output}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		assertGenerationFailed(t, &stdout, &stderr, fmt.Sprintf("manifest_workers workers=%d", w2CLIWorkersDefault))
	})

	t.Run("T09/workers1_生成失败_新输出父目录不创建", func(t *testing.T) {
		outParent := filepath.Join(t.TempDir(), "w2-missing-parent")
		newOutput := filepath.Join(outParent, "manifest.json")
		var stdout, stderr bytes.Buffer
		code := run([]string{"--dir", root, "--version", "w2-lock", "--output", newOutput, "--workers", "1"}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		assertGenerationFailed(t, &stdout, &stderr, "manifest_workers workers=1")
		w2RequirePathAbsent(t, outParent)
		w2RequirePathAbsent(t, newOutput)
	})

	// 评审 C3 返工：新输出/父目录不存在的保护此前仅测 workers=1，补缺省默认 16 场景。
	t.Run("T09/缺省默认16_生成失败_新输出父目录不创建", func(t *testing.T) {
		outParent := filepath.Join(t.TempDir(), "w2-missing-parent")
		newOutput := filepath.Join(outParent, "manifest.json")
		var stdout, stderr bytes.Buffer
		code := run([]string{"--dir", root, "--version", "w2-lock", "--output", newOutput}, &stdout, &stderr)
		if code != 1 {
			t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		assertGenerationFailed(t, &stdout, &stderr, fmt.Sprintf("manifest_workers workers=%d", w2CLIWorkersDefault))
		w2RequirePathAbsent(t, outParent)
		w2RequirePathAbsent(t, newOutput)
	})

	// 评审 C3 返工：释放后显式验证全部扫描输入文件（被锁数据文件、其他数据文件、策略文件）
	// 均可重命名且可删除，不只删除被锁数据文件。
	t.Run("T09/释放占用后扫描输入文件均可重命名且可删除", func(t *testing.T) {
		release()
		for _, path := range []string{lockedPath, otherPath, policyPath} {
			renamed := path + ".w2-release-check"
			if err := os.Rename(path, renamed); err != nil {
				t.Fatalf("释放占用后仍无法重命名 %s（锁未真正释放或文件仍被占用）：%v", path, err)
			}
			if err := os.Rename(renamed, path); err != nil {
				t.Fatalf("重命名回原名失败 %s：%v", path, err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatalf("释放占用后仍无法删除 %s：%v", path, err)
			}
			w2RequirePathAbsent(t, path)
		}
	})
}

// TestRunWriteFailureToBlockedParentReturns1 覆盖 T13 写入失败场景：
// 输出父路径是普通文件，可靠触发 WriteJSONAtomic 的 MkdirAll 失败（不以 chmod 为证据）；
// 生成阶段已成功（策略与 workers 行已在 stdout），写入失败退出 1，
// stderr 为原 `写入清单失败：<错误>`（原前缀+真实错误），无 summary/成功行。
// r3（评审 r2 第四节第 1 条）：stdout 由子串弱匹配升级为 w2AssertExactLines 精确逐行——
// 恰好两行 policy_load_success + 对应请求值的 manifest_workers 行（workers=16 不得冒充请求 1），
// 各恰好一次、顺序正确、无额外行；写入错误的既有保证（stderr 原前缀+真实错误、退出 1、
// 输出不被触碰——本场景输出父路径为普通文件，输出与 .tmp 均不可能产生）原样维持；
// 按规格，本用例仍不扩大为"所有写出失败均无临时文件"的保证，不新增临时文件事务类断言。
func TestRunWriteFailureToBlockedParentReturns1(t *testing.T) {
	root := w2BuildRootWithPolicy(t, map[string][]byte{"keep.bin": []byte("keep")})
	outDir := t.TempDir()
	blocker := filepath.Join(outDir, "blocker")
	w2WriteFile(t, blocker, []byte("占位普通文件，使输出父路径不可能是目录"))
	output := filepath.Join(blocker, "manifest.json")
	var stdout, stderr bytes.Buffer
	code := run([]string{"--dir", root, "--version", "w2-write-fail", "--output", output, "--workers", "1"}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("退出码 = %d，期望 1；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	// 写入错误既有保证原样维持：stderr 原前缀（真实错误随 %v 附于其后）+ 无生成/策略/成功痕迹。
	w2AssertContains(t, &stderr, "stderr", "写入清单失败：")
	w2AssertNotContains(t, &stderr, "stderr", "生成清单失败：", "policy_load_failed", "policy_manifest_summary", "已生成清单")
	// r3 返工（与评审 C2 修复同型）：stdout 精确两行——policy_load_success + 请求 1 对应的
	// manifest_workers 行，各恰好一次、顺序正确、无额外行
	// （原 `manifest_workers workers=1` 子串断言会误匹配 workers=16，已弃用）。
	w2AssertExactLines(t, &stdout, "stdout",
		"policy_load_success source=local schema_version=1 policy_file=updater-policy.json",
		"manifest_workers workers=1")
	// 无 summary/成功行的既有 NotContains 断言保留：精确比较已隐含，此处作为双保险，不削弱。
	w2AssertNotContains(t, &stdout, "stdout", "policy_manifest_summary", "已生成清单")
}

// TestRunLegacyGoldenCLIBytesMatchWithWorkers1And16 覆盖 T14 的 CLI 层金样：
// 将 W0 冻结的三份 CLI 金样原字节与来源 SHA-256 嵌入常量并先做转写自检；
// 在临时目录重建同一 fixture/版本（parallel-hash-golden），
// 显式 --workers 1 与 16 的输出必须与旧金样逐字节一致。
// policy-only 的合法结果必须是 1/1/0（策略文件强制入清单），不得期待 files:null。
// 此单元层证据链不替代 W6 的真实旧 EXE 全量对照。
func TestRunLegacyGoldenCLIBytesMatchWithWorkers1And16(t *testing.T) {
	// 金样常量转写自检：SHA-256 与 W0 实测值逐一比对，失真立即失败。
	goldens := map[string]struct {
		data string
		sha  string
	}{
		"ordinary":    {w2GoldenOrdinaryJSON, w2GoldenOrdinarySHA256},
		"nested":      {w2GoldenNestedJSON, w2GoldenNestedSHA256},
		"policy-only": {w2GoldenPolicyOnlyJSON, w2GoldenPolicyOnlySHA256},
	}
	for name, g := range goldens {
		sum := sha256.Sum256([]byte(g.data))
		if got := hex.EncodeToString(sum[:]); got != g.sha {
			t.Fatalf("W0 金样常量转写失真（%s）：got %s want %s", name, got, g.sha)
		}
	}

	fixtures := []struct {
		name        string
		buildRoot   func(t *testing.T) string
		golden      string
		summaryLine string
		fileCount   int
	}{
		{
			name:        "ordinary",
			buildRoot:   w2BuildGoldenOrdinaryRoot,
			golden:      w2GoldenOrdinaryJSON,
			summaryLine: "policy_manifest_summary scanned_count=2 included_count=2 excluded_count=0",
			fileCount:   2,
		},
		{
			name:        "nested",
			buildRoot:   w2BuildGoldenNestedRoot,
			golden:      w2GoldenNestedJSON,
			summaryLine: "policy_manifest_summary scanned_count=3 included_count=3 excluded_count=0",
			fileCount:   3,
		},
		{
			name:        "policy-only",
			buildRoot:   w2BuildGoldenPolicyOnlyRoot,
			golden:      w2GoldenPolicyOnlyJSON,
			summaryLine: "policy_manifest_summary scanned_count=1 included_count=1 excluded_count=0",
			fileCount:   1,
		},
	}
	workerCases := []struct {
		label     string
		extraArgs []string
		workers   int
	}{
		{label: "显式workers1", extraArgs: []string{"--workers", "1"}, workers: 1},
		{label: "显式workers16", extraArgs: []string{"--workers", "16"}, workers: 16},
	}
	for _, f := range fixtures {
		for _, w := range workerCases {
			t.Run("T14/"+f.name+"/"+w.label, func(t *testing.T) {
				root := f.buildRoot(t)
				outDir := t.TempDir() // 扫描根之外
				output := filepath.Join(outDir, "manifest.json")
				version := "parallel-hash-golden"
				args := []string{"--dir", root, "--version", version, "--output", output}
				args = append(args, w.extraArgs...)
				var stdout, stderr bytes.Buffer
				code := run(args, &stdout, &stderr)
				if code != 0 {
					t.Fatalf("退出码 = %d，期望 0；stdout=%q stderr=%q", code, stdout.String(), stderr.String())
				}
				if stderr.Len() != 0 {
					t.Fatalf("成功场景 stderr 必须为空：%q", stderr.String())
				}
				wantStdout := w2ExpectedSuccessStdout(output, version, f.summaryLine, w.workers, f.fileCount)
				if got := stdout.String(); got != wantStdout {
					t.Fatalf("stdout 不符\n期望：\n%s\n实际：\n%s", wantStdout, got)
				}
				data, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(data, []byte(f.golden)) {
					sum := sha256.Sum256(data)
					t.Fatalf("输出与 W0 金样不逐字节一致（%s/%s）：实际 SHA-256 %s，内容：\n%s",
						f.name, w.label, hex.EncodeToString(sum[:]), data)
				}
			})
		}
	}
}
