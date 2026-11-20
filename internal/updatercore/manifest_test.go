package updatercore

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

func TestGenerateManifestSkipsUpdaterStateAndUsesSHA256Samples(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.txt"), []byte("abc"))
	writeTestFile(t, filepath.Join(root, StateDirName, "config.json"), []byte("{}"))

	manifest, err := GenerateManifest(root, "6.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Version != "6.0.0" {
		t.Fatalf("version = %q", manifest.Version)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("files = %d, want 1: %#v", len(manifest.Files), manifest.Files)
	}
	wantHash := sha256Hex([]byte("abc"))
	got := manifest.Files[0]
	if got.Path != "app.txt" || got.Size != 3 || got.HeadHash != wantHash || got.TailHash != wantHash {
		t.Fatalf("unexpected entry: %#v", got)
	}
}

func TestGenerateManifestSkipsMetadataFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "app.txt"), []byte("abc"))
	writeTestFile(t, filepath.Join(root, "latest.json"), []byte(`{"version":"1.0.0"}`))
	writeTestFile(t, filepath.Join(root, "manifest.json"), []byte(`{"version":"1.0.0","files":[]}`))

	manifest, err := GenerateManifest(root, "6.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 1 {
		t.Fatalf("files = %d, want 1: %#v", len(manifest.Files), manifest.Files)
	}
	if manifest.Files[0].Path != "app.txt" {
		t.Fatalf("unexpected file path: %q", manifest.Files[0].Path)
	}
}

func TestGenerateManifestWithPolicyAppliesPublishRulesAndSystemInvariants(t *testing.T) {
	root := t.TempDir()
	for path, data := range map[string][]byte{
		"apps/app.exe":                       []byte("app"),
		"apps/cache/temp.bin":                []byte("cache"),
		"other.bin":                          []byte("other"),
		"latest.json":                        []byte("{}"),
		"manifest.json":                      []byte("{}"),
		"updater-policy.json":                []byte(`{"schema_version":1}`),
		"updater_state/forced-by-policy.exe": []byte("state"),
	} {
		writeTestFile(t, filepath.Join(root, filepath.FromSlash(path)), data)
	}
	policy, err := ParseFilePolicy([]byte(`{
		"schema_version":1,
		"publish":{
			"include":{"directories":["apps"],"files":["latest.json"],"extensions":[".json"]},
			"exclude":{"directories":["apps/cache"],"files":["updater-policy.json"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	manifest, summary, err := GenerateManifestWithPolicy(root, "7.0.0", policy)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, entry := range manifest.Files {
		paths = append(paths, entry.Path)
	}
	got := strings.Join(paths, ",")
	if got != "apps/app.exe,updater-policy.json" {
		t.Fatalf("manifest paths = %q", got)
	}
	if summary.ScannedCount != 4 || summary.IncludedCount != 2 || summary.ExcludedCount != 2 {
		t.Fatalf("unexpected summary: %#v", summary)
	}
}

func TestCompareManifestsLargeSetAndUnknownFileProtection(t *testing.T) {
	root := t.TempDir()
	installed := Manifest{Version: "1.0.0"}
	remote := Manifest{Version: "2.0.0"}
	for i := 0; i < 10000; i++ {
		entry := FileEntry{
			Path:     "dir/file-" + padInt(i) + ".bin",
			Size:     10,
			HeadHash: "old",
			TailHash: "old",
		}
		installed.Files = append(installed.Files, entry)
		if i == 9999 {
			continue
		}
		if i == 1234 {
			entry.Size = 11
			entry.HeadHash = "new"
			entry.TailHash = "new"
		}
		remote.Files = append(remote.Files, entry)
	}
	remote.Files = append(remote.Files, FileEntry{Path: "new.bin", Size: 1, HeadHash: "n", TailHash: "n"})

	plan, err := CompareManifests(root, &installed, remote)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != 1 || len(plan.Modify) != 1 || len(plan.Delete) != 1 {
		t.Fatalf("counts add/modify/delete = %d/%d/%d", len(plan.Add), len(plan.Modify), len(plan.Delete))
	}
	if plan.Add[0].Path != "new.bin" || plan.Modify[0].Path != "dir/file-01234.bin" || plan.Delete[0].Path != "dir/file-09999.bin" {
		t.Fatalf("unexpected plan: %#v", plan)
	}

	writeTestFile(t, filepath.Join(root, "conflict.txt"), []byte("local user data"))
	remoteConflict := Manifest{Version: "2.0.0", Files: []FileEntry{entryForBytes("conflict.txt", []byte("remote data"))}}
	_, err = CompareManifests(root, &Manifest{Version: "1.0.0"}, remoteConflict)
	if err == nil || !strings.Contains(err.Error(), "保护未知用户文件") {
		t.Fatalf("expected unknown file protection error, got %v", err)
	}
}

func TestCompareManifestsWithProgressReportsPlanProgress(t *testing.T) {
	root := t.TempDir()
	remote := Manifest{
		Version: "2.0.0",
		Files: []FileEntry{
			entryForBytes("app.exe", []byte("new")),
			entryForBytes("dir/data.bin", []byte("payload")),
			entryForBytes("config.ini", []byte("k=v")),
		},
	}

	var events []ProgressEvent
	plan, err := CompareManifestsWithProgress(root, nil, remote, 4, func(event ProgressEvent) {
		events = append(events, event)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Add) != len(remote.Files) {
		t.Fatalf("add count = %d, want %d", len(plan.Add), len(remote.Files))
	}
	if len(events) == 0 {
		t.Fatal("expected progress events")
	}
	last := events[len(events)-1]
	if last.Phase != "Plan" {
		t.Fatalf("last phase = %q", last.Phase)
	}
	if last.CompletedFiles != len(remote.Files) || last.TotalFiles != len(remote.Files) {
		t.Fatalf("unexpected progress totals: %#v", last)
	}
	if strings.TrimSpace(last.CurrentFile) == "" {
		t.Fatalf("expected last current file to be set: %#v", last)
	}
}

func TestCompareManifestsWithProgressParallelWorkersMatchSingleWorker(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "keep.bin"), []byte("same"))
	writeTestFile(t, filepath.Join(root, "change.bin"), []byte("old"))
	installed := Manifest{Version: "1.0.0", Files: []FileEntry{
		entryForBytes("keep.bin", []byte("same")),
		entryForBytes("change.bin", []byte("old")),
	}}
	remote := Manifest{Version: "2.0.0", Files: []FileEntry{
		entryForBytes("keep.bin", []byte("same")),
		entryForBytes("change.bin", []byte("new")),
		entryForBytes("add.bin", []byte("fresh")),
	}}

	single, err := CompareManifestsWithProgress(root, &installed, remote, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := CompareManifestsWithProgress(root, &installed, remote, 4, nil)
	if err != nil {
		t.Fatal(err)
	}

	if len(single.Add) != len(parallel.Add) || len(single.Modify) != len(parallel.Modify) || len(single.Delete) != len(parallel.Delete) {
		t.Fatalf("single=%#v parallel=%#v", single, parallel)
	}
	if single.DownloadSize != parallel.DownloadSize {
		t.Fatalf("download size single=%d parallel=%d", single.DownloadSize, parallel.DownloadSize)
	}
	if parallel.Add[0].Path != "add.bin" || parallel.Modify[0].Path != "change.bin" {
		t.Fatalf("unexpected parallel plan: %#v", parallel)
	}
}

func TestCompareManifestsWithPolicyProtectsOnlyUserModifiedFiles(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "user.ini"), []byte("user changed"))
	writeTestFile(t, filepath.Join(root, "clean.ini"), []byte("old clean"))
	writeTestFile(t, filepath.Join(root, "system.ini"), []byte("old system"))
	writeTestFile(t, filepath.Join(root, "removed.ini"), []byte("keep removed"))

	installed := Manifest{Version: "1", Files: []FileEntry{
		entryForBytes("user.ini", []byte("old user")),
		entryForBytes("clean.ini", []byte("old clean")),
		entryForBytes("system.ini", []byte("old system")),
		entryForBytes("removed.ini", []byte("old removed")),
	}}
	remote := Manifest{Version: "2", Files: []FileEntry{
		entryForBytes("user.ini", []byte("new user")),
		entryForBytes("clean.ini", []byte("new clean")),
		entryForBytes("system.ini", []byte("new system")),
	}}
	policy, err := ParseFilePolicy([]byte(`{
		"schema_version":1,
		"preserve":{
			"include":{"extensions":[".ini"]},
			"exclude":{"files":["system.ini"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}

	single, singleSummary, err := CompareManifestsWithPolicySummary(root, &installed, remote, policy, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallel, parallelSummary, err := CompareManifestsWithPolicySummary(root, &installed, remote, policy, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	for name, result := range map[string]struct {
		plan    Plan
		summary PolicyPlanSummary
	}{
		"single":   {plan: single, summary: singleSummary},
		"parallel": {plan: parallel, summary: parallelSummary},
	} {
		plan := result.plan
		if len(plan.Modify) != 2 || plan.Modify[0].Path != "clean.ini" || plan.Modify[1].Path != "system.ini" {
			t.Fatalf("%s unexpected modify: %#v", name, plan.Modify)
		}
		if len(plan.Delete) != 0 {
			t.Fatalf("%s protected delete unexpectedly planned: %#v", name, plan.Delete)
		}
		if result.summary.PreservedModify != 1 || result.summary.PreservedDelete != 1 {
			t.Fatalf("%s preserved counts = %d/%d", name, result.summary.PreservedModify, result.summary.PreservedDelete)
		}
	}
}

func TestCompareManifestsWithPolicyStillRejectsUnknownTarget(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "unknown.ini"), []byte("local"))
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1,"preserve":{"include":{"extensions":[".ini"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	remote := Manifest{Version: "2", Files: []FileEntry{entryForBytes("unknown.ini", []byte("remote"))}}
	if _, err := CompareManifestsWithPolicy(root, &Manifest{Version: "1"}, remote, policy, 2, nil); err == nil ||
		!strings.Contains(err.Error(), "保护未知用户文件") {
		t.Fatalf("expected unknown target conflict, got %v", err)
	}
}

func writeTestFile(t *testing.T, fileName string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(fileName), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fileName, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func entryForBytes(path string, data []byte) FileEntry {
	hash := sha256Hex(data)
	return FileEntry{Path: path, Size: int64(len(data)), HeadHash: hash, TailHash: hash}
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func padInt(value int) string {
	text := "00000" + strconvItoa(value)
	return text[len(text)-5:]
}

func strconvItoa(value int) string {
	if value == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = byte('0' + value%10)
		value /= 10
	}
	return string(buf[i:])
}

// =============================================================================
// ManifestGenerator 并发哈希（W1）核心 Acceptance 测试与 benchmark
//
// 本段测试按 plan §3 最小测试缝契约编写，引用以下尚未实现的生产符号（W3 才在
// manifest.go 中实现）：
//   - 导出 API：GenerateManifestWithPolicyWorkers、MaxManifestHashWorkers
//   - 未导出测试缝：manifestGenerationOps（含 walkDir/entryForPath/afterCollect 字段）、
//     generateManifestWithPolicyWorkers、walkManifestCandidates、hashManifestCandidates
//
// 因此在 W3 之前本包整体编译失败（Inner RED）。这是"新 API/测试缝尚未实现"的预期
// RED，行为断言必须等 W3 实现后逐例运行；证据如实记录该状态，不冒称已逐例失败。
// 测试缝默认生产绑定：walkDir=filepath.WalkDir、entryForPath=fileEntryForPath、
// afterCollect=nil；所有注入点与计数器均由单次测试闭包持有，无全局可写 hook。
// =============================================================================

// mgWriteSizedFile 写入确定字节数的文件，内容为确定性模式，供采样阈值边界用例使用。
func mgWriteSizedFile(tb testing.TB, fileName string, size int64) {
	data := make([]byte, size)
	for i := range data {
		data[i] = byte(i % 251)
	}
	if err := os.WriteFile(fileName, data, 0o644); err != nil {
		tb.Fatalf("写入 %s 失败：%v", fileName, err)
	}
}

// mgSameManifest 逐字段比较两个 Manifest（版本号与全部 FileEntry 字段）。
// Manifest 含切片字段不可用 == 比较，必须逐项比较。
func mgSameManifest(a, b Manifest) bool {
	if a.Version != b.Version || len(a.Files) != len(b.Files) {
		return false
	}
	for i := range a.Files {
		if a.Files[i] != b.Files[i] {
			return false
		}
	}
	return true
}

// mgSerializeForCompare 用生产 WriteJSONAtomic 将结果写到扫描根之外的目录并读回
// 原字节，保证比较对象是真实序列化产物而非内存结构。
func mgSerializeForCompare(t *testing.T, outDir, name string, manifest Manifest) []byte {
	t.Helper()
	outPath := filepath.Join(outDir, name+".json")
	if err := WriteJSONAtomic(outPath, manifest); err != nil {
		t.Fatalf("写出 %s 失败：%v", name, err)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("读回 %s 失败：%v", outPath, err)
	}
	return data
}

// mgAssertZeroResult 断言出错时返回双零值：空 Manifest（版本为空且 Files 为 nil）
// 与全零 ManifestPolicySummary。
func mgAssertZeroResult(t *testing.T, manifest Manifest, summary ManifestPolicySummary, context string) {
	t.Helper()
	if manifest.Version != "" || manifest.Files != nil {
		t.Fatalf("%s：出错应返回空 Manifest，得到 version=%q files=%#v", context, manifest.Version, manifest.Files)
	}
	if summary != (ManifestPolicySummary{}) {
		t.Fatalf("%s：出错应返回全零 summary，得到 %#v", context, summary)
	}
}

// mgAssertSmallFileEntry 断言小文件条目与固定内容一致（head==tail==内容 SHA-256）。
func mgAssertSmallFileEntry(t *testing.T, manifest Manifest, rel string, size int64, wantHash string) {
	t.Helper()
	for _, entry := range manifest.Files {
		if entry.Path == rel {
			if entry.Size != size || entry.HeadHash != wantHash || entry.TailHash != wantHash {
				t.Fatalf("文件 %s 条目与固定内容不符：%#v（期望 size=%d head=tail=%s）", rel, entry, size, wantHash)
			}
			return
		}
	}
	t.Fatalf("清单缺少文件 %s", rel)
}

// mgAssertEntryMatchesHashFile 断言清单条目与直接调用原 HashFile 的结果一致，
// 证明采样算法（<2MiB 全量、>=2MiB 首尾各 1MiB）未被改动。
func mgAssertEntryMatchesHashFile(t *testing.T, root string, manifest Manifest, rel string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(rel))
	size, head, tail, err := HashFile(full)
	if err != nil {
		t.Fatalf("直接 HashFile(%s) 失败：%v", rel, err)
	}
	for _, entry := range manifest.Files {
		if entry.Path == rel {
			if entry.Size != size || entry.HeadHash != head || entry.TailHash != tail {
				t.Fatalf("采样阈值文件 %s 的摘要与原 HashFile 不一致：entry=%#v hashFile=(%d,%s,%s)", rel, entry, size, head, tail)
			}
			return
		}
	}
	t.Fatalf("清单缺少采样阈值文件 %s", rel)
}

// mgActiveCounter 统计 entryForPath 包装层的并发活跃数与最大活跃数。
type mgActiveCounter struct {
	mu     sync.Mutex
	active int
	max    int
}

func (c *mgActiveCounter) wrap(inner func(string, string) (FileEntry, error)) func(string, string) (FileEntry, error) {
	return func(rootDir, fileName string) (FileEntry, error) {
		c.mu.Lock()
		c.active++
		if c.active > c.max {
			c.max = c.active
		}
		c.mu.Unlock()
		defer func() {
			c.mu.Lock()
			c.active--
			c.mu.Unlock()
		}()
		return inner(rootDir, fileName)
	}
}

func (c *mgActiveCounter) snapshot() (active, maxActive int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.active, c.max
}

// mgGateCall 记录 T10 单个受控调用：门控携带路径，release/finished 为该调用独有的
// 独立信号——放行错误候选不会自动放行其他在途或后续调用（评审 P1-2 返工要求）。
// releaseOnce 幂等：正常放行与清理兜底可重复调用，不会重复 close panic。
type mgGateCall struct {
	fileName   string
	release    chan struct{}
	finished   chan struct{}
	releaseOne sync.Once
}

func (c *mgGateCall) releaseOnce() {
	c.releaseOne.Do(func() { close(c.release) })
}

// mgGateHelper 是 T10 的阻塞 entryForPath：每次调用登记开始信号（携带路径）并阻塞在
// 该调用自己的释放门控上；放行后返回条目，errPath 候选返回原 sentinel 错误。
// 对 workers=1（串行路径）同样生效，证明串行与并发共用同一 entryForPath 注入点。
// 活跃数/处理数/错误触发计数由 mutex 保护，可被多 worker 与测试 goroutine 并发读写。
//
// 故障后收口协议（评审 r2 P1-2）：errArmed 标记"故障候选的错误已被 helper 记录"。
// 错误候选在错误分支先武装 errArmed、再递增 errFired，两者都严格早于该调用 finished
// 信号关闭（同一 goroutine 程序序）。测试侧在观察到 errCall.finished 之后才放行其余
// 首批调用，因此此后新进入 helper 的调用必然看到 armed——它们是 spec §5.3 允许的
// 竞态新在途调用（生产侧尚未记录首错/关闭取消通道时，worker 可合法领取下一候选）：
// 这些入口不再新建门控、不发开始信号，直接返回同一 sentinel 并立即完成，使正确实现
// 等待中的"新在途调用"能正常完成并收口，测试不再冻结规格允许的调用。errArmed 之前
// 的入口（受控首批）仍走逐调用独立门控，由测试显式放行。errFired 只统计错误候选
// 本身的首故障（恰为 1）；raceEntries 仅记录竞态新入口数供诊断输出，其出现与否取决于
// 合法调度，不作为确定性证据、不参与断言。
type mgGateHelper struct {
	mu          sync.Mutex
	active      int
	max         int
	processed   int
	errFired    int
	raceEntries int
	errArmed    bool
	starts      chan *mgGateCall
	errPath     string
	err         error
}

func (h *mgGateHelper) entryForPath(rootDir, fileName string) (FileEntry, error) {
	h.mu.Lock()
	armed := h.errArmed
	h.active++
	h.processed++
	if h.active > h.max {
		h.max = h.active
	}
	h.mu.Unlock()
	if armed {
		// 故障已被 helper 记录后的新入口：直接返回同一 sentinel，不新建门控
		//（评审 r2 P1-2：有界、可收口的处理协议，正确实现可等待其完成）。
		h.mu.Lock()
		h.active--
		h.raceEntries++
		h.mu.Unlock()
		return FileEntry{}, h.err
	}
	call := &mgGateCall{fileName: fileName, release: make(chan struct{}), finished: make(chan struct{})}
	h.starts <- call
	<-call.release
	defer close(call.finished)
	h.mu.Lock()
	h.active--
	h.mu.Unlock()
	if h.errPath != "" && fileName == h.errPath {
		h.mu.Lock()
		h.errArmed = true // 先武装故障标记：严格早于本调用 finished 关闭（程序序）
		h.errFired++
		h.mu.Unlock()
		return FileEntry{}, h.err
	}
	return FileEntry{Path: filepath.ToSlash(fileName), Size: 1, HeadHash: "t10-helper", TailHash: "t10-helper"}, nil
}

func (h *mgGateHelper) snapshot() (active, maxActive int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.active, h.max
}

func (h *mgGateHelper) processedCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.processed
}

func (h *mgGateHelper) errFiredCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errFired
}

// raceEntryCount 返回故障被 helper 记录后直接返回 sentinel 的竞态新入口数
// （仅诊断输出用，不参与断言）。
func (h *mgGateHelper) raceEntryCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.raceEntries
}

// mgLockScript：Windows 真实拒读子进程脚本（PowerShell 字节区间独占锁，底层 LockFileEx）。
// 被锁路径经环境变量 MG_LOCK_PATH 传入，避免中文/空格路径的命令行转义问题。
//
// 机制说明（评审 P2 返工后的方案，与 W2 在本机独立验证一致）：
// 原方案的 [IO.File]::Open(..., FileShare.None) 共享占用在提升（管理员）令牌下会被
// Go syscall.Open 自动附加的 FILE_FLAG_BACKUP_SEMANTICS 绕过，而原补救手段
// （AdjustTokenPrivileges + SE_PRIVILEGE_REMOVED）修改的是整个测试进程令牌且不可恢复，
// 属进程级副作用，且其 Windows 专属 syscall 符号会让本通用 _test.go 无法在非 Windows
// 平台编译。故改用字节区间独占锁：子进程以 FileShare.ReadWrite 打开文件（允许他人
// 取句柄）并 Lock 整个文件内容区间，其他进程读取该区间时稳定产生真实
// ERROR_LOCK_VIOLATION，使 HashFile 真实失败；不依赖任何令牌权限，无进程级副作用。
// 流程：独占打开并锁定整个文件内容 → stdout READY 握手 → 阻塞读 stdin 一行（或关闭）
// → 解锁退出。
const mgLockScript = `$ErrorActionPreference = 'Stop'; ` +
	`$fs = [IO.File]::Open($env:MG_LOCK_PATH, [IO.FileMode]::Open, [IO.FileAccess]::Read, [IO.FileShare]::ReadWrite); ` +
	`$fs.Lock(0, $fs.Length); ` +
	`[Console]::Out.WriteLine('READY'); [Console]::Out.Flush(); ` +
	`$null = [Console]::In.ReadLine(); $fs.Dispose(); exit 0`

// mgLockFileExclusive 用 PowerShell 子进程对指定文件建立字节区间独占锁（LockFileEx），
// 使其他进程对文件内容的读取真实失败（Windows 真实拒读占用，不用 chmod 伪造）；
// 返回幂等释放函数并注册 t.Cleanup 兜底：测试失败退出时子进程也会被释放回收。
// 占用是否确实生效由调用方"直接 HashFile 必须失败"的前置验证最后把关（若仍可读则
// 测试失败，不能作为拒读证据）。
func mgLockFileExclusive(t *testing.T, path string) func() {
	t.Helper()
	if runtime.GOOS != "windows" {
		t.Skip("Windows 字节区间独占锁仅在 Windows 上有效；非 Windows 平台该部分记录为未验收")
	}
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", mgLockScript)
	cmd.Env = append(os.Environ(), "MG_LOCK_PATH="+path)
	var stderrBuf bytes.Buffer
	cmd.Stderr = &stderrBuf
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("创建占用子进程 stdin 失败：%v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("创建占用子进程 stdout 失败：%v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动占用子进程失败：%v", err)
	}
	// READY 握手（带超时，避免子进程异常时挂起测试）。
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
			t.Fatalf("占用握手失败：%v；stderr：%s", err, strings.TrimSpace(stderrBuf.String()))
		}
	case <-time.After(30 * time.Second):
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("占用握手超时；stderr：%s", strings.TrimSpace(stderrBuf.String()))
	}
	rawRelease := func() {
		_, _ = fmt.Fprintf(stdin, "\n")
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
	var releaseOnce sync.Once
	doRelease := func() {
		releaseOnce.Do(rawRelease)
	}
	t.Cleanup(doRelease)
	return doRelease
}

// T01 ParallelMatchesSerialBytes：固定目录覆盖嵌套/混合大小写、中文空格、零字节、
// 2MiB-1、2MiB、2MiB+1 及更大文件；旧包装与新 API workers=1/2/8/16/32 多次执行；
// 输出经真实 WriteJSONAtomic 序列化后逐字节比较；用 channel 控制错开完成次序。
func TestGenerateManifestParallelMatchesSerialBytes(t *testing.T) {
	root := t.TempDir()
	outDir := t.TempDir() // 扫描根之外，存放 WriteJSONAtomic 序列化产物
	writeTestFile(t, filepath.Join(root, "app.txt"), []byte("abc"))
	writeTestFile(t, filepath.Join(root, "MixedCase", "File.TXT"), []byte("MiXeD"))
	writeTestFile(t, filepath.Join(root, "Dir A", "中文 目录", "data.bin"), []byte("嵌套内容"))
	writeTestFile(t, filepath.Join(root, "Deep", "sub", "nested-deep.txt"), []byte("deep"))
	writeTestFile(t, filepath.Join(root, "zero.bin"), nil)
	mgWriteSizedFile(t, filepath.Join(root, "edge-2mib-1.bin"), sampleThreshold-1)
	mgWriteSizedFile(t, filepath.Join(root, "edge-2mib.bin"), sampleThreshold)
	mgWriteSizedFile(t, filepath.Join(root, "edge-2mib+1.bin"), sampleThreshold+1)
	mgWriteSizedFile(t, filepath.Join(root, "large-3mib.bin"), sampleThreshold+sampleSize)
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	const version = "parallel-hash-t01"
	// 串行基准：旧包装
	baseline, baselineSummary, err := GenerateManifestWithPolicy(root, version, policy)
	if err != nil {
		t.Fatal(err)
	}
	if len(baseline.Files) != 9 {
		t.Fatalf("基准清单条目数 = %d，期望 9", len(baseline.Files))
	}
	baselineBytes := mgSerializeForCompare(t, outDir, "baseline", baseline)
	// 小文件与零字节文件：head==tail==内容 SHA-256（全量采样）
	mgAssertSmallFileEntry(t, baseline, "app.txt", 3, sha256Hex([]byte("abc")))
	mgAssertSmallFileEntry(t, baseline, "zero.bin", 0, sha256Hex(nil))
	mgAssertSmallFileEntry(t, baseline, "Dir A/中文 目录/data.bin", int64(len([]byte("嵌套内容"))), sha256Hex([]byte("嵌套内容")))
	// 采样阈值边界文件：摘要对照原 HashFile（不改采样算法）
	for _, rel := range []string{"edge-2mib-1.bin", "edge-2mib.bin", "edge-2mib+1.bin", "large-3mib.bin"} {
		mgAssertEntryMatchesHashFile(t, root, baseline, rel)
	}
	// 旧包装与新 API workers=1/2/8/16/32 各 3 次，输出逐字节一致
	for _, workers := range []int{1, 2, 8, 16, 32} {
		for round := 1; round <= 3; round++ {
			manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policy, workers)
			if err != nil {
				t.Fatalf("workers=%d round=%d：生成失败：%v", workers, round, err)
			}
			if !mgSameManifest(baseline, manifest) {
				t.Fatalf("workers=%d round=%d：全字段与串行基准不一致", workers, round)
			}
			if summary != baselineSummary {
				t.Fatalf("workers=%d round=%d：三计数与串行基准不一致：%#v", workers, round, summary)
			}
			out := mgSerializeForCompare(t, outDir, fmt.Sprintf("w%d-r%d", workers, round), manifest)
			if !bytes.Equal(baselineBytes, out) {
				t.Fatalf("workers=%d round=%d：WriteJSONAtomic 输出字节与串行基准不一致", workers, round)
			}
		}
	}
	// 候选序：经同一 walkManifestCandidates 收集缝取得（与生成内部收集一致的
	// WalkDir 词法序），用于确认完成序列确实错开（评审 P2-2）
	var candidateOrder []string
	if _, collectErr := walkManifestCandidates(root, policy, filepath.WalkDir, func(p string) error {
		candidateOrder = append(candidateOrder, p)
		return nil
	}); collectErr != nil {
		t.Fatalf("收集候选序失败：%v", collectErr)
	}
	if len(candidateOrder) != 9 {
		t.Fatalf("候选序长度 = %d，期望 9", len(candidateOrder))
	}
	// 用门控错开完成次序（评审 r2 P2-2 确定性协议）：全部候选开始后按候选路径建立
	// 门控映射，按确定的逆候选序逐项"放行→等待该项完成确认"（一次只放行一项），
	// 完成序列由 helper 先记录、最后才关闭该项完成信号；随后断言完成序列恰为逆候选
	// 序——强于旧"与候选序不同"检查（旧检查在"先连续放行全部门控"的合法调度下
	// 可能误报失败）。
	for _, workers := range []int{16, 32} {
		t.Run(fmt.Sprintf("stagger-workers-%d", workers), func(t *testing.T) {
			var completionOrder []string
			manifest, summary, err := mgRunStaggered(t, root, version, policy, candidateOrder, workers, &completionOrder)
			if err != nil {
				t.Fatal(err)
			}
			if !mgSameManifest(baseline, manifest) || summary != baselineSummary {
				t.Fatalf("错开完成次序后结果与串行基准不一致：workers=%d", workers)
			}
			out := mgSerializeForCompare(t, outDir, fmt.Sprintf("stagger-w%d", workers), manifest)
			if !bytes.Equal(baselineBytes, out) {
				t.Fatalf("错开完成次序后输出字节与串行基准不一致：workers=%d", workers)
			}
			// 真实完成顺序确认（评审 r2 P2-2）：错序刺激已确定性生效——完成序列必须
			// 恰为逆候选序（逐项"放行→等待完成→记录"的必然结果，蕴含 ≠ 候选序）。
			if len(completionOrder) != len(candidateOrder) {
				t.Fatalf("完成序列长度 = %d，与候选数 %d 不一致", len(completionOrder), len(candidateOrder))
			}
			for i := range candidateOrder {
				if completionOrder[i] != candidateOrder[len(candidateOrder)-1-i] {
					t.Fatalf("完成序列不是确定的逆候选序，错序刺激未确定性生效：完成序=%v；候选序=%v", completionOrder, candidateOrder)
				}
			}
		})
	}
}

// mgStaggerCall 记录错开次序场景的单个受控调用：门控携带路径，release/finished 为该
// 调用独有的独立信号；releaseOnce 幂等（正常放行与清理兜底可重复调用）。
type mgStaggerCall struct {
	path       string
	release    chan struct{}
	finished   chan struct{}
	releaseOne sync.Once
}

func (c *mgStaggerCall) releaseOnce() {
	c.releaseOne.Do(func() { close(c.release) })
}

// mgRunStaggered 通过测试缝 entryForPath 给每个候选挂独立门控：收集全部候选的开始
// 信号后按候选路径建立"路径→门控"映射，按确定的逆候选序（candidateOrder 由调用方
// 经 walkManifestCandidates 收集缝取得，与生成内部收集一致的 WalkDir 词法序）逐项
// "放行→等待该项哈希完成确认"，一次只放行一项——其余候选仍被各自门控阻塞，完成与
// 完成序列记录因此严格按逆候选序发生，不依赖调度快慢（评审 r2 P2-2）。门控携带路径；
// helper 先把完成路径记入 *completionOrder、最后才关闭该项 finished（完成序列先记录），
// 测试观察到 finished 时该项记录必然已写入。候选数必须 <= workers 才能让全部候选
// 同时开始。全程无 sleep。幂等清理提前注册（评审 P2-3）：任何失败退出（含看门狗
// Fatal）都放行全部门控、排空后续开始信号并有界等待生成 goroutine 收口；清理计时器
// 在进入清理时才创建（评审 r2 P2-3），给完整清理预算，不遗留阻塞 goroutine。
func mgRunStaggered(t *testing.T, root, version string, policy FilePolicy, candidateOrder []string, workers int, completionOrder *[]string) (Manifest, ManifestPolicySummary, error) {
	t.Helper()
	fileCount := len(candidateOrder)
	starts := make(chan *mgStaggerCall, fileCount)
	var completionsMu sync.Mutex
	wrapped := func(rootDir, fileName string) (FileEntry, error) {
		call := &mgStaggerCall{path: fileName, release: make(chan struct{}), finished: make(chan struct{})}
		starts <- call
		<-call.release
		entry, err := fileEntryForPath(rootDir, fileName)
		// 完成序列先记录、最后才关闭该项完成信号（评审 r2 P2-2）：
		// 测试观察到 finished 时该项记录必然已在序列中，逐项"放行→等待完成"后
		// 记录次序确定地恰为逆候选序。
		completionsMu.Lock()
		*completionOrder = append(*completionOrder, call.path)
		completionsMu.Unlock()
		close(call.finished)
		return entry, err
	}
	type result struct {
		manifest Manifest
		summary  ManifestPolicySummary
		err      error
	}
	done := make(chan result, 1)
	// genDone 在后台 goroutine 送出结果后关闭：done 是一次性缓冲 channel，
	// 结果被主流程取走后不再可接收，清理必须等待独立的收口信号。
	genDone := make(chan struct{})
	var collected []*mgStaggerCall
	t.Cleanup(func() {
		cleanupDeadline := time.After(15 * time.Second) // 进入清理时才启动（评审 r2 P2-3）
		for {
			for _, call := range collected {
				call.releaseOnce()
			}
			select {
			case call := <-starts:
				call.releaseOnce()
				continue
			case <-genDone:
				return
			case <-cleanupDeadline:
				t.Logf("警告：错开次序子测清理等待生成收口超时（15s），疑似实现死锁，已如实记录")
				return
			}
		}
	})
	go func() {
		ops := manifestGenerationOps{walkDir: filepath.WalkDir, entryForPath: wrapped, afterCollect: nil}
		manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policy, workers, ops)
		done <- result{manifest, summary, err}
		close(genDone)
	}()
	// 收集全部候选的开始信号（门控携带路径）
	for i := 0; i < fileCount; i++ {
		select {
		case call := <-starts:
			collected = append(collected, call)
		case <-time.After(30 * time.Second):
			t.Fatalf("错开次序子测等待候选开始超时（已开始 %d/%d）", len(collected), fileCount)
		}
	}
	// 按候选路径建立门控映射（评审 r2 P2-2）：不依赖开始信号到达次序，以收集缝的
	// 候选序为准核对每个候选都恰有门控，随后按逆候选序逐项操作。
	gates := make(map[string]*mgStaggerCall, fileCount)
	for _, call := range collected {
		gates[call.path] = call
	}
	if len(gates) != fileCount {
		t.Fatalf("开始信号按路径归并后 = %d 个门控，与候选数 %d 不一致（存在重复或缺失路径）", len(gates), fileCount)
	}
	for _, p := range candidateOrder {
		if gates[p] == nil {
			t.Fatalf("候选 %s 未收到开始信号，无法按逆候选序安排错序刺激", p)
		}
	}
	// 按确定的逆候选序逐项"放行→等待该项哈希完成确认"（评审 r2 P2-2）：一次只
	// 放行一项，其余候选仍被各自门控阻塞，完成序列因此确定地恰为逆候选序。
	for i := fileCount - 1; i >= 0; i-- {
		call := gates[candidateOrder[i]]
		call.releaseOnce()
		select {
		case <-call.finished:
		case <-time.After(30 * time.Second):
			t.Fatalf("错开次序子测等待候选 %s 的哈希完成确认超时", filepath.Base(call.path))
		}
	}
	select {
	case res := <-done:
		return res.manifest, res.summary, res.err
	case <-time.After(60 * time.Second):
		t.Fatal("错开次序子测生成调用超时，疑似死锁")
	}
	return Manifest{}, ManifestPolicySummary{}, nil
}

// T02 WorkersNormalization：-1/0/1 的成功与错误结果一致；旧 GenerateManifest/
// GenerateManifestWithPolicy 保持 workers=1 的输出；版本空白优先于超限；版本字符串不裁剪。
func TestGenerateManifestWorkersNormalization(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "a.txt"), []byte("A"))
	writeTestFile(t, filepath.Join(root, "c.ini"), []byte("C"))
	const version = "parallel-hash-t02"
	// workers=1 基准（默认策略，与旧 GenerateManifest 可比）
	base, baseSummary, err := GenerateManifestWithPolicyWorkers(root, version, DefaultFilePolicy(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(base.Files) != 2 {
		t.Fatalf("基准条目数 = %d，期望 2", len(base.Files))
	}
	for _, workers := range []int{-1, 0, 1} {
		manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, DefaultFilePolicy(), workers)
		if err != nil {
			t.Fatalf("workers=%d：生成失败：%v", workers, err)
		}
		if !mgSameManifest(base, manifest) {
			t.Fatalf("workers=%d：结果与 workers=1 不一致", workers)
		}
		if summary != baseSummary {
			t.Fatalf("workers=%d：summary 与 workers=1 不一致：%#v", workers, summary)
		}
	}
	// 旧两个 API 仍是 workers=1 串行包装：输出完全一致
	oldWithPolicy, oldSummary, err := GenerateManifestWithPolicy(root, version, DefaultFilePolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !mgSameManifest(base, oldWithPolicy) || oldSummary != baseSummary {
		t.Fatal("旧 GenerateManifestWithPolicy 输出与新 API workers=1 不一致")
	}
	oldSimple, err := GenerateManifest(root, version)
	if err != nil {
		t.Fatal(err)
	}
	if !mgSameManifest(base, oldSimple) {
		t.Fatal("旧 GenerateManifest 输出与新 API workers=1 不一致")
	}
	// 错误结果一致性：不存在的根，-1/0/1 与旧 API 返回同一错误、双零值
	missing := filepath.Join(t.TempDir(), "missing-root")
	policyAll, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	var wantErrText string
	for i, workers := range []int{-1, 0, 1} {
		manifest, summary, err := GenerateManifestWithPolicyWorkers(missing, version, policyAll, workers)
		if err == nil {
			t.Fatalf("workers=%d：不存在的根应报错", workers)
		}
		if i == 0 {
			wantErrText = err.Error()
		} else if err.Error() != wantErrText {
			t.Fatalf("workers=%d：错误文本与 workers=-1 不一致：%v vs %v", workers, err, wantErrText)
		}
		mgAssertZeroResult(t, manifest, summary, fmt.Sprintf("不存在的根 workers=%d", workers))
	}
	_, _, oldMissingErr := GenerateManifestWithPolicy(missing, version, policyAll)
	if oldMissingErr == nil || oldMissingErr.Error() != wantErrText {
		t.Fatalf("旧 API 错误与新 API workers=1 不一致：%v vs %v", oldMissingErr, wantErrText)
	}
	// 版本空白优先于 workers 超限
	manifest, summary, err := GenerateManifestWithPolicyWorkers(root, "   ", policyAll, 257)
	if err == nil || err.Error() != "版本号不能为空" {
		t.Fatalf("版本空白应优先于 workers 上限，得到 %v", err)
	}
	mgAssertZeroResult(t, manifest, summary, "版本空白+workers=257")
	// 版本字符串不裁剪
	padded := "  7.0.0-rc1  "
	paddedManifest, _, err := GenerateManifestWithPolicyWorkers(root, padded, policyAll, 16)
	if err != nil {
		t.Fatal(err)
	}
	if paddedManifest.Version != padded {
		t.Fatalf("版本字符串被裁剪：%q，期望 %q", paddedManifest.Version, padded)
	}
}

// T03 WorkersLimit（API 层）：256 合法且小目录实际活跃<=候选数；257 返回
// `workers 不能大于 256：257` 与双零值、不扫描不哈希；上限检查先于遍历（表述收窄，
// 见评审 P2-6：不存在的根通常 filepath.Abs 仍成功，不能证明先于根路径解析）。
func TestGenerateManifestWorkersLimit(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"f1.bin", "f2.bin", "f3.bin"} {
		writeTestFile(t, filepath.Join(root, name), []byte(name))
	}
	const version = "parallel-hash-t03"
	policyAll, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	// 256 合法（导出 API）
	manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policyAll, MaxManifestHashWorkers)
	if err != nil {
		t.Fatalf("workers=256 应合法：%v", err)
	}
	if len(manifest.Files) != 3 || summary.ScannedCount != 3 || summary.IncludedCount != 3 || summary.ExcludedCount != 0 {
		t.Fatalf("workers=256 结果异常：files=%d summary=%#v", len(manifest.Files), summary)
	}
	// 256 合法（测试缝 + 活跃计数）：小目录实际活跃 <= 候选数
	counter := &mgActiveCounter{}
	ops := manifestGenerationOps{walkDir: filepath.WalkDir, entryForPath: counter.wrap(fileEntryForPath), afterCollect: nil}
	seamManifest, seamSummary, err := generateManifestWithPolicyWorkers(root, version, policyAll, MaxManifestHashWorkers, ops)
	if err != nil {
		t.Fatal(err)
	}
	if !mgSameManifest(manifest, seamManifest) || seamSummary != summary {
		t.Fatal("缝层 workers=256 结果与导出 API 不一致")
	}
	if active, max := counter.snapshot(); active != 0 {
		t.Fatalf("返回后活跃数 = %d，应全部收口", active)
	} else if max < 1 || max > 3 {
		t.Fatalf("workers=256 小目录实际活跃 = %d，必须在 1..候选数(3) 之间", max)
	}
	// 常量契约：规格锁定 256
	if MaxManifestHashWorkers != 256 {
		t.Fatalf("MaxManifestHashWorkers = %d，规格锁定为 256", MaxManifestHashWorkers)
	}
	// 257：导出 API 返回规定错误与双零值
	m257, s257, err257 := GenerateManifestWithPolicyWorkers(root, version, policyAll, MaxManifestHashWorkers+1)
	if err257 == nil || err257.Error() != "workers 不能大于 256：257" {
		t.Fatalf("期望错误 `workers 不能大于 256：257`，得到 %v", err257)
	}
	mgAssertZeroResult(t, m257, s257, "导出 API workers=257")
	// 257：缝层注入计数器，证明不扫描、不哈希、双零值
	walkCalls, hashCalls := 0, 0
	ops257 := manifestGenerationOps{
		walkDir: func(rootDir string, fn fs.WalkDirFunc) error {
			walkCalls++
			return filepath.WalkDir(rootDir, fn)
		},
		entryForPath: func(rootDir, fileName string) (FileEntry, error) {
			hashCalls++
			return fileEntryForPath(rootDir, fileName)
		},
		afterCollect: nil,
	}
	mSeam, sSeam, errSeam := generateManifestWithPolicyWorkers(root, version, policyAll, MaxManifestHashWorkers+1, ops257)
	if errSeam == nil || errSeam.Error() != "workers 不能大于 256：257" {
		t.Fatalf("缝层 257 期望相同错误，得到 %v", errSeam)
	}
	mgAssertZeroResult(t, mSeam, sSeam, "缝层 workers=257")
	if walkCalls != 0 || hashCalls != 0 {
		t.Fatalf("workers=257 不应扫描或哈希：walk=%d hash=%d", walkCalls, hashCalls)
	}
	// 上限检查先于遍历（表述收窄，评审 P2-6）：不存在的根 + 257 → workers 错误而非
	// 遍历错误。注意：不存在的路径通常 filepath.Abs 仍成功，本用例证明的是上限检查
	// 先于 walkDir 遍历，不能证明先于根路径解析（令 Abs 失败的输入依赖平台行为，
	// 需经前置断言确认，此处不采用）。
	mRoot, sRoot, errRoot := GenerateManifestWithPolicyWorkers(filepath.Join(t.TempDir(), "missing-root"), version, policyAll, MaxManifestHashWorkers+1)
	if errRoot == nil || errRoot.Error() != "workers 不能大于 256：257" {
		t.Fatalf("上限检查应先于根路径遍历，得到 %v", errRoot)
	}
	mgAssertZeroResult(t, mRoot, sRoot, "非法根+workers=257")
}

// T04 EmptyDirectory：真正空根 + 内存策略（无磁盘策略文件），workers=1/16 均版本保留、
// Files=nil、summary 全 0、JSON files:null、无哈希调用、afterCollect 不调用。
func TestGenerateManifestEmptyDirectory(t *testing.T) {
	root := t.TempDir()
	outDir := t.TempDir()
	const version = "parallel-hash-t04"
	policyAll, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{1, 16} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policyAll, workers)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Version != version {
				t.Fatalf("version = %q，期望保留 %q", manifest.Version, version)
			}
			if manifest.Files != nil {
				t.Fatalf("空结果 Files 应保持 nil，得到 %#v", manifest.Files)
			}
			if summary != (ManifestPolicySummary{}) {
				t.Fatalf("summary 应全 0，得到 %#v", summary)
			}
			out := mgSerializeForCompare(t, outDir, fmt.Sprintf("empty-w%d", workers), manifest)
			expected := "{\n  \"version\": \"" + version + "\",\n  \"files\": null\n}\n"
			if string(out) != expected {
				t.Fatalf("空结果 JSON 与期望不一致：\n期望=%q\n实际=%q", expected, out)
			}
		})
	}
	// 缝层证明：无哈希调用、afterCollect 不调用（1/16 皆然）
	for _, workers := range []int{1, 16} {
		hashCalls, afterCollectCalls := 0, 0
		ops := manifestGenerationOps{
			walkDir:      filepath.WalkDir,
			entryForPath: func(string, string) (FileEntry, error) { hashCalls++; return FileEntry{}, nil },
			afterCollect: func() { afterCollectCalls++ },
		}
		manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policyAll, workers, ops)
		if err != nil {
			t.Fatal(err)
		}
		if manifest.Files != nil || summary != (ManifestPolicySummary{}) {
			t.Fatalf("空根缝层结果异常：files=%#v summary=%#v", manifest.Files, summary)
		}
		if hashCalls != 0 {
			t.Fatalf("空根 workers=%d 仍发生 %d 次哈希调用", workers, hashCalls)
		}
		if afterCollectCalls != 0 {
			t.Fatalf("空根 workers=%d afterCollect 被调用 %d 次（仅在非空候选哈希前调用）", workers, afterCollectCalls)
		}
	}
}

// T05 OnlyExcluded：仅 3 个被 publish 排除的普通文件、无磁盘策略文件；
// workers=1/16 summary=3/0/3、Files=nil、哈希 0 次。
func TestGenerateManifestOnlyExcluded(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"a.bin", "b.bin", "c.bin"} {
		writeTestFile(t, filepath.Join(root, name), []byte(name))
	}
	const version = "parallel-hash-t05"
	policyExcludeAll, err := ParseFilePolicy([]byte(`{"schema_version":1,"publish":{"exclude":{"extensions":[".bin"]}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{1, 16} {
		manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policyExcludeAll, workers)
		if err != nil {
			t.Fatal(err)
		}
		if summary.ScannedCount != 3 || summary.IncludedCount != 0 || summary.ExcludedCount != 3 {
			t.Fatalf("workers=%d summary=%#v，期望 3/0/3", workers, summary)
		}
		if manifest.Files != nil {
			t.Fatalf("workers=%d 全排除时 Files 应保持 nil，得到 %#v", workers, manifest.Files)
		}
		if manifest.Version != version {
			t.Fatalf("workers=%d version = %q", workers, manifest.Version)
		}
	}
	// 缝层：哈希 0 次（1/16 都不进入哈希阶段）
	for _, workers := range []int{1, 16} {
		hashCalls := 0
		ops := manifestGenerationOps{
			walkDir:      filepath.WalkDir,
			entryForPath: func(string, string) (FileEntry, error) { hashCalls++; return FileEntry{}, nil },
			afterCollect: nil,
		}
		manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policyExcludeAll, workers, ops)
		if err != nil {
			t.Fatal(err)
		}
		if hashCalls != 0 {
			t.Fatalf("workers=%d 全排除仍发生 %d 次哈希调用", workers, hashCalls)
		}
		if manifest.Files != nil || summary.ScannedCount != 3 || summary.IncludedCount != 0 || summary.ExcludedCount != 3 {
			t.Fatalf("workers=%d 缝层结果异常：files=%#v summary=%#v", workers, manifest.Files, summary)
		}
	}
}

// T06 SystemInvariantsAndCounts：普通 kept.bin、excluded.bin、策略文件、根 metadata 两个、
// updater_state 子树；策略试图强制 include 系统文件/排除策略文件；workers=1/16 仅
// kept.bin+策略文件入清单，summary=3/2/1，状态子树与 metadata 不计 excluded，
// 无排除项进入哈希阶段。
func TestGenerateManifestSystemInvariantsAndCounts(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "kept.bin"), []byte("kept"))
	writeTestFile(t, filepath.Join(root, "excluded.bin"), []byte("excluded"))
	writeTestFile(t, filepath.Join(root, "updater-policy.json"), []byte(`{"schema_version":1}`))
	writeTestFile(t, filepath.Join(root, "latest.json"), []byte(`{"version":"ignored"}`))
	writeTestFile(t, filepath.Join(root, "manifest.json"), []byte(`{"version":"ignored","files":[]}`))
	writeTestFile(t, filepath.Join(root, StateDirName, "state-should-not-count.txt"), []byte("state"))
	writeTestFile(t, filepath.Join(root, StateDirName, "nested", "deep-state.txt"), []byte("state"))
	// 策略意图：强制 include 状态子树与根 metadata、排除策略文件；系统不变量必须优先。
	policy, err := ParseFilePolicy([]byte(`{
		"schema_version":1,
		"publish":{
			"include":{"directories":["updater_state"],"files":["latest.json","manifest.json","kept.bin"]},
			"exclude":{"files":["updater-policy.json","excluded.bin"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	const version = "parallel-hash-t06"
	for _, workers := range []int{1, 16} {
		manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policy, workers)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, entry := range manifest.Files {
			paths = append(paths, entry.Path)
		}
		if got := strings.Join(paths, ","); got != "kept.bin,updater-policy.json" {
			t.Fatalf("workers=%d 清单路径 = %q，期望 kept.bin,updater-policy.json", workers, got)
		}
		if summary.ScannedCount != 3 || summary.IncludedCount != 2 || summary.ExcludedCount != 1 {
			t.Fatalf("workers=%d summary=%#v，期望 3/2/1", workers, summary)
		}
	}
	// 缝层：进入哈希阶段的候选恰好是 kept.bin 与 updater-policy.json（WalkDir 词法序）。
	// 记录器必须线程安全：并发路径下多个 worker 同时写共享切片/map 会产生数据竞争
	// （评审 P1-1），此处用局部 mutex 保护；不得以串行化生产调用的方式消除竞争。
	var hashedMu sync.Mutex
	hashedCounts := make(map[string]int)
	ops := manifestGenerationOps{
		walkDir: filepath.WalkDir,
		entryForPath: func(rootDir, fileName string) (FileEntry, error) {
			hashedMu.Lock()
			hashedCounts[filepath.Base(fileName)]++
			hashedMu.Unlock()
			return fileEntryForPath(rootDir, fileName)
		},
		afterCollect: nil,
	}
	_, _, err = generateManifestWithPolicyWorkers(root, version, policy, 16, ops)
	if err != nil {
		t.Fatal(err)
	}
	// 调用返回后按精确计数断言：两个允许候选各恰好被调用 1 次，其余路径 0 次；
	// 不用集合去重掩盖重复或遗漏调用。并发下调用次序不定，但调用次数契约确定。
	if len(hashedCounts) != 2 {
		t.Fatalf("进入哈希的候选 = %v，期望恰好 kept.bin 与 updater-policy.json（排除项不得进入哈希）", hashedCounts)
	}
	if hashedCounts["kept.bin"] != 1 || hashedCounts["updater-policy.json"] != 1 {
		t.Fatalf("允许候选应各被哈希恰 1 次：kept.bin=%d 次、updater-policy.json=%d 次（完整计数=%v）",
			hashedCounts["kept.bin"], hashedCounts["updater-policy.json"], hashedCounts)
	}
}

// T07 NonRegularAndRelativeMetadata：sub/manifest.json 按普通规则处理（根相对 metadata
// 判断不波及子目录）、混合大小写根 metadata 仍被跳过、符号链接不哈希不计数；
// 链接不可用时单独记录该链接子测，不跳过其余断言。
func TestGenerateManifestNonRegularAndRelativeMetadata(t *testing.T) {
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "sub", "manifest.json"), []byte(`{"sub":true}`))
	writeTestFile(t, filepath.Join(root, "Latest.JSON"), []byte(`{"root":true}`))
	writeTestFile(t, filepath.Join(root, "target.bin"), []byte("link-target"))
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	const version = "parallel-hash-t07"
	linkCreated := false
	if linkErr := os.Symlink(filepath.Join(root, "target.bin"), filepath.Join(root, "link.bin")); linkErr == nil {
		linkCreated = true
	} else {
		t.Logf("记录：符号链接在本环境不可用（%v），链接子测单独记录未执行，其余断言继续", linkErr)
	}
	var lastManifest Manifest
	for _, workers := range []int{1, 16} {
		manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policy, workers)
		if err != nil {
			t.Fatal(err)
		}
		var paths []string
		for _, entry := range manifest.Files {
			paths = append(paths, entry.Path)
		}
		if got := strings.Join(paths, ","); got != "sub/manifest.json,target.bin" {
			t.Fatalf("workers=%d 清单路径 = %q，期望 sub/manifest.json,target.bin", workers, got)
		}
		if summary.ScannedCount != 2 || summary.IncludedCount != 2 || summary.ExcludedCount != 0 {
			t.Fatalf("workers=%d summary=%#v，期望 2/2/0（根 metadata 与非普通文件不计数）", workers, summary)
		}
		lastManifest = manifest
	}
	// 子目录同名 metadata 按普通规则发布且哈希正确
	for _, entry := range lastManifest.Files {
		if entry.Path == "sub/manifest.json" {
			want := sha256Hex([]byte(`{"sub":true}`))
			if entry.Size != int64(len([]byte(`{"sub":true}`))) || entry.HeadHash != want || entry.TailHash != want {
				t.Fatalf("子目录 metadata 文件条目异常：%#v", entry)
			}
		}
	}
	if linkCreated {
		t.Run("symlink-not-counted", func(t *testing.T) {
			manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policy, 16)
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range manifest.Files {
				if entry.Path == "link.bin" {
					t.Fatalf("符号链接被当作普通文件哈希：%#v", entry)
				}
			}
			if summary.ScannedCount != 2 || summary.IncludedCount != 2 {
				t.Fatalf("链接子测 summary=%#v，期望 2/2/0（链接不计数）", summary)
			}
		})
		// 缝层补充断言（评审 P1-3）：非普通文件不得被哈希。仅凭最终清单断言无法发现
		// "先哈希链接、随后丢弃结果"的错误实现，因此经同一 manifestGenerationOps.
		// entryForPath 缝 + 线程安全记录器（局部 mutex，评审 P1-1 同因）分别验证
		// workers=1/16：link.bin 调用 0 次、两个正常候选各恰好调用 1 次。
		for _, workers := range []int{1, 16} {
			t.Run(fmt.Sprintf("symlink-not-hashed-workers-%d", workers), func(t *testing.T) {
				var hashedMu sync.Mutex
				hashedCounts := make(map[string]int)
				ops := manifestGenerationOps{
					walkDir: filepath.WalkDir,
					entryForPath: func(rootDir, fileName string) (FileEntry, error) {
						rel, relErr := filepath.Rel(rootDir, fileName)
						if relErr != nil {
							rel = fileName
						}
						hashedMu.Lock()
						hashedCounts[filepath.ToSlash(rel)]++
						hashedMu.Unlock()
						return fileEntryForPath(rootDir, fileName)
					},
					afterCollect: nil,
				}
				manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policy, workers, ops)
				if err != nil {
					t.Fatal(err)
				}
				// 保留既有结果与计数断言（缝层与导出 API 契约一致）
				if summary.ScannedCount != 2 || summary.IncludedCount != 2 || summary.ExcludedCount != 0 {
					t.Fatalf("workers=%d 缝层 summary=%#v，期望 2/2/0（链接不计数）", workers, summary)
				}
				if len(manifest.Files) != 2 {
					t.Fatalf("workers=%d 缝层清单条目数 = %d，期望 2（链接不入清单）", workers, len(manifest.Files))
				}
				for _, entry := range manifest.Files {
					if entry.Path == "link.bin" {
						t.Fatalf("workers=%d 符号链接被当作普通文件哈希：%#v", workers, entry)
					}
				}
				// 非普通文件不哈希：link.bin 调用次数必须为 0
				if hashedCounts["link.bin"] != 0 {
					t.Fatalf("workers=%d 非普通文件 link.bin 被哈希 %d 次（完整计数=%v）", workers, hashedCounts["link.bin"], hashedCounts)
				}
				// 两个正常候选各恰好被调用 1 次，且没有其他路径进入哈希
				if hashedCounts["sub/manifest.json"] != 1 || hashedCounts["target.bin"] != 1 {
					t.Fatalf("workers=%d 正常候选应各被哈希恰 1 次：sub/manifest.json=%d、target.bin=%d（完整计数=%v）",
						workers, hashedCounts["sub/manifest.json"], hashedCounts["target.bin"], hashedCounts)
				}
				if len(hashedCounts) != 2 {
					t.Fatalf("workers=%d 进入哈希的候选 = %v，期望恰好 sub/manifest.json 与 target.bin", workers, hashedCounts)
				}
			})
		}
	} else {
		t.Logf("记录：符号链接子测在本环境未执行（创建失败）；根相对 metadata 与非普通文件过滤断言已全部执行")
	}
}

// T08 DeletedCandidateAborts：afterCollect 屏障（收集完成、哈希开始前）删除一个已入选
// 文件，16 worker；返回 os.IsNotExist 可识别的原路径错误、双零值、全部任务收口；无 sleep。
func TestGenerateManifestDeletedCandidateAborts(t *testing.T) {
	root := t.TempDir()
	const fileCount = 8
	for i := 0; i < fileCount; i++ {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("f%d.bin", i)), []byte(fmt.Sprintf("content-%d", i)))
	}
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	const version = "parallel-hash-t08"
	deletedFull := filepath.Join(root, "f3.bin")
	counter := &mgActiveCounter{}
	var afterCollectCalls int
	ops := manifestGenerationOps{
		walkDir:      filepath.WalkDir,
		entryForPath: counter.wrap(fileEntryForPath),
		afterCollect: func() {
			afterCollectCalls++
			// 确定性时序：收集已完整结束、哈希尚未开始，此时删除候选不依赖 sleep。
			if removeErr := os.Remove(deletedFull); removeErr != nil {
				t.Errorf("afterCollect 屏障内删除候选失败：%v", removeErr)
			}
		},
	}
	manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policy, 16, ops)
	if afterCollectCalls != 1 {
		t.Fatalf("afterCollect 调用次数 = %d，期望恰为 1", afterCollectCalls)
	}
	if err == nil {
		t.Fatal("删除候选后生成应失败")
	}
	if !os.IsNotExist(err) {
		t.Fatalf("期望 os.IsNotExist 可识别的原错误，得到 %v", err)
	}
	var pathErr *fs.PathError
	if !errors.As(err, &pathErr) {
		t.Fatalf("期望保留 *fs.PathError 原错误结构，得到 %T：%v", err, err)
	}
	// 原错误结构断言加强（评审 P2-5）：Path 与被删除候选路径精确一致（不用子串匹配），
	// 并校验 Op 为 open（HashFile 首个 I/O 动作），排除错误被包装或合成。
	if pathErr.Path != deletedFull {
		t.Fatalf("PathError.Path = %q，应与被删除候选路径精确一致：%q", pathErr.Path, deletedFull)
	}
	if pathErr.Op != "open" {
		t.Fatalf("PathError.Op = %q，期望 open（删除候选应在 Open 阶段失败）", pathErr.Op)
	}
	mgAssertZeroResult(t, manifest, summary, "删除候选")
	if active, max := counter.snapshot(); active != 0 {
		t.Fatalf("返回后活跃数 = %d，全部任务应已收口", active)
	} else if max > 16 {
		t.Fatalf("最大活跃 = %d，超过 workers=16", max)
	}
}

// T09 UnreadableFileAborts（API 层）：Windows 对已 publish 的普通数据文件建立字节区间
// 独占锁（LockFileEx）真实拒读占用（不占用策略文件；无进程级令牌/权限副作用，
// 见 mgLockScript 说明）；先证明直接 HashFile 真实失败并保存其原错误结构字段作为
// 对照基准，再测 workers=1/16 返回同一原错误（类型、完整 Path、Op、底层错误与文本
// 一致，评审 P2-5）与双零值；解除占用后文件可重命名/删除。
func TestGenerateManifestUnreadableFileAborts(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("T09 的字节区间独占锁真实拒读验证只能在 Windows 执行；非 Windows 环境如实记录该部分未验收")
	}
	root := t.TempDir()
	writeTestFile(t, filepath.Join(root, "locked.bin"), []byte("locked content"))
	writeTestFile(t, filepath.Join(root, "free.bin"), []byte("free content"))
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	const version = "parallel-hash-t09"
	lockedPath := filepath.Join(root, "locked.bin")
	release := mgLockFileExclusive(t, lockedPath)
	// 先证明直接 HashFile 真实失败（占用确实拒读；若仍可读则不能通过 T09），
	// 并保存 PathError 各字段与错误文本，作为生成错误的对照基准（评审 P2-5）。
	_, _, _, hashErr := HashFile(lockedPath)
	if hashErr == nil {
		t.Fatal("直接 HashFile 仍可读，字节区间独占锁未生效，不能作为 T09 证据")
	}
	if os.IsNotExist(hashErr) {
		t.Fatalf("占用错误不应是文件缺失：%v", hashErr)
	}
	var directPathErr *fs.PathError
	if !errors.As(hashErr, &directPathErr) {
		t.Fatalf("直接 HashFile 错误应为 *fs.PathError，得到 %T：%v", hashErr, hashErr)
	}
	directOp, directPath, directErr := directPathErr.Op, directPathErr.Path, directPathErr.Err
	directText := hashErr.Error()
	t.Logf("直接 HashFile 已确认真实失败：%v（Op=%s Path=%s 底层=%v）", hashErr, directOp, directPath, directErr)
	for _, workers := range []int{1, 16} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policy, workers)
			if err == nil {
				t.Fatal("占用文件应导致生成失败")
			}
			// 生成错误必须与直接 HashFile 的真实错误同构：类型、完整 Path、Op、
			// 底层错误与文本逐一对照，排除合成错误或错误包装（评审 P2-5）。
			var genPathErr *fs.PathError
			if !errors.As(err, &genPathErr) {
				t.Fatalf("生成错误应为 *fs.PathError，得到 %T：%v", err, err)
			}
			if genPathErr.Path != directPath {
				t.Fatalf("生成错误 Path = %q，应与直接 HashFile 错误精确一致：%q", genPathErr.Path, directPath)
			}
			if genPathErr.Op != directOp {
				t.Fatalf("生成错误 Op = %q，应与直接 HashFile 一致：%q", genPathErr.Op, directOp)
			}
			if genPathErr.Err != directErr {
				t.Fatalf("生成错误底层错误 = %v，应与直接 HashFile 一致：%v", genPathErr.Err, directErr)
			}
			if err.Error() != directText {
				t.Fatalf("生成错误文本应与直接 HashFile 一致：%v vs %v", err, directText)
			}
			mgAssertZeroResult(t, manifest, summary, fmt.Sprintf("占用文件 workers=%d", workers))
		})
	}
	// 解除占用后所有文件可重命名/删除
	release()
	renamed := lockedPath + ".renamed"
	if renameErr := os.Rename(lockedPath, renamed); renameErr != nil {
		t.Fatalf("释放占用后无法重命名被占用文件：%v", renameErr)
	}
	if renameBackErr := os.Rename(renamed, lockedPath); renameBackErr != nil {
		t.Fatalf("释放占用后无法改回文件名：%v", renameBackErr)
	}
	for _, name := range []string{"locked.bin", "free.bin"} {
		if removeErr := os.Remove(filepath.Join(root, name)); removeErr != nil {
			t.Fatalf("释放占用后无法删除 %s：%v", name, removeErr)
		}
	}
}

// T10 PoolBoundAndCancellation：至少 1000 个候选，局部阻塞 hash helper（逐调用独立
// 门控 + 故障后收口协议）对 workers=1/-1（归一串行）/16/256 均生效；故障候选置于
// 受控首批，用显式开始/释放/完成信号安排确定性故障窗口：先只放行错误候选并等待
// helper 记录其错误（errArmed 先于 finished 武装，评审 r2 P1-2），其余在途调用仍被
// 各自门控阻塞，随后逐个独立放行（允许 spec §5 规定的竞态在途操作完成），不以
// "调度足够快"为前提。helper 记录故障后的新入口（spec §5.3 允许的竞态新在途调用）
// 直接返回同一 sentinel、不新建门控，使正确实现等待中的新在途调用能完成并收口——
// 测试不冻结规格允许的调用，也不把 helper 的 finished 当作生产侧取消已生效的同步点
// （评审 r2 P1-2）。成功与错误分支返回后统一断言 active==0 与最终最大活跃数不超界；
// 记录最大活跃==min(workers,N)、保留原 sentinel、双零值、errFired==1（首故障）、
// 并发路径处理数<总数、全部收口；小 N 场景以收口后精确调用数证明实际池裁剪。超时
// 仅作死锁看门狗；门控与后台生成调用注册幂等清理（清理计时器进入清理时才创建，
// 评审 r2 P2-3），测试失败退出也释放并等待收口（评审 P1-2/P2-3）。
func TestGenerateManifestPoolBoundAndCancellation(t *testing.T) {
	root := t.TempDir()
	const total = 1000
	for i := 0; i < total; i++ {
		writeTestFile(t, filepath.Join(root, fmt.Sprintf("c%04d.bin", i)), []byte(fmt.Sprintf("c%d", i)))
	}
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	const version = "parallel-hash-t10"
	sentinel := errors.New("T10 测试哨兵错误")
	newHelper := func(buffer int, errPath string) *mgGateHelper {
		return &mgGateHelper{
			starts:  make(chan *mgGateCall, buffer),
			errPath: errPath,
			err:     sentinel,
		}
	}
	// assertPoolClosed：生成调用返回后的统一收口断言（成功与错误分支都必须满足）：
	// active==0（全部调用收口）且最终最大活跃数不超过 min(workers,N) 上界（评审 P1-2 ③）。
	assertPoolClosed := func(t *testing.T, helper *mgGateHelper, bound int, context string) {
		t.Helper()
		if active, max := helper.snapshot(); active != 0 {
			t.Fatalf("%s：返回后活跃数 = %d，应全部收口", context, active)
		} else if max > bound {
			t.Fatalf("%s：最终最大活跃 = %d，超过 min(workers,N)=%d 上界", context, max, bound)
		}
	}
	// runGateScenario：启动生成（后台 goroutine），等待恰好 min(workers,N) 个受控调用
	// 全部开始（受控首批），断言满负荷活跃数；随后按场景放行并等待收口。
	// 幂等清理提前注册：任何失败退出（含看门狗 Fatal）都会放行全部门控、排空开始信号
	// 并有界等待生成 goroutine 收口，不遗留阻塞 goroutine（评审 P1-2 ⑥/P2-3）。
	runGateScenario := func(name string, rootDir string, candidateCount, workers int, errPath string, verify func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper)) {
		t.Run(name, func(t *testing.T) {
			helper := newHelper(candidateCount, errPath)
			type result struct {
				manifest Manifest
				summary  ManifestPolicySummary
				err      error
			}
			done := make(chan result, 1)
			// genDone 在后台 goroutine 送出结果后关闭：done 是一次性缓冲 channel，
			// 结果被主流程取走后不再可接收，清理必须等待独立的收口信号。
			genDone := make(chan struct{})
			var firstBatch []*mgGateCall
			// 清理计时器在真正进入清理时才创建（评审 r2 P2-3）：执行阶段耗时（等待
			// 开始/返回的看门狗预算）不计入清理预算，进入清理时才给予完整 15s 期限。
			t.Cleanup(func() {
				cleanupDeadline := time.After(15 * time.Second)
				for {
					for _, call := range firstBatch {
						call.releaseOnce()
					}
					select {
					case call := <-helper.starts:
						call.releaseOnce()
						continue
					case <-genDone:
						return
					case <-cleanupDeadline:
						t.Logf("警告：%s 清理等待生成收口超时（15s），疑似实现死锁，已如实记录", name)
						return
					}
				}
			})
			go func() {
				ops := manifestGenerationOps{walkDir: filepath.WalkDir, entryForPath: helper.entryForPath, afterCollect: nil}
				manifest, summary, err := generateManifestWithPolicyWorkers(rootDir, version, policy, workers, ops)
				done <- result{manifest, summary, err}
				close(genDone)
			}()
			effective := workers
			if effective <= 0 {
				effective = 1 // 与 API 契约一致：workers<=0 归一为 1
			}
			bound := effective
			if bound > candidateCount {
				bound = candidateCount
			}
			// 受控首批：无缓冲 jobs 按候选序派发且全部 worker 都阻塞在首批调用内，
			// 因此最先开始的 bound 个调用恰为前 bound 个候选（含故障候选）。
			for i := 0; i < bound; i++ {
				select {
				case call := <-helper.starts:
					firstBatch = append(firstBatch, call)
				case <-time.After(30 * time.Second):
					t.Fatalf("等待第 %d/%d 个候选开始超时，疑似池未按预期启动", i+1, bound)
				}
			}
			if _, maxActive := helper.snapshot(); maxActive != bound {
				t.Fatalf("满负荷最大活跃 = %d，期望恰为 min(workers=%d, N=%d) = %d", maxActive, workers, candidateCount, bound)
			}
			if errPath == "" {
				// 成功场景：放行首批后进入放行循环——后续调用随开始逐个独立放行
				//（未开始的调用不会被预先放行），直到生成调用返回；超时仅作死锁看门狗。
				for _, call := range firstBatch {
					call.releaseOnce()
				}
				for {
					select {
					case call := <-helper.starts:
						call.releaseOnce()
					case res := <-done:
						verify(t, res.manifest, res.summary, res.err, helper)
						assertPoolClosed(t, helper, bound, name)
						return
					case <-time.After(60 * time.Second):
						t.Fatalf("生成调用超时未返回，疑似死锁（workers=%d）", workers)
					}
				}
			}
			// 错误场景（评审 P1-2 ①⑤）：故障候选必须在受控首批中；先验证这一点，
			// 再只放行错误候选并等待其完成（其余 bound-1 个在途调用仍被各自门控阻塞，
			// 独立释放信号互不牵连），构成不依赖调度快慢的确定性故障窗口。
			var errCall *mgGateCall
			var others []*mgGateCall
			for _, call := range firstBatch {
				if call.fileName == errPath {
					errCall = call
				} else {
					others = append(others, call)
				}
			}
			if errCall == nil {
				var batchNames []string
				for _, call := range firstBatch {
					batchNames = append(batchNames, filepath.Base(call.fileName))
				}
				t.Fatalf("错误候选 %s 不在受控首批中，无法安排确定性故障窗口（首批=%v）", errPath, batchNames)
			}
			errCall.releaseOnce()
			select {
			case <-errCall.finished:
			case <-time.After(30 * time.Second):
				t.Fatalf("等待错误候选 %s 完成超时，疑似取消未生效或收口未达成", filepath.Base(errPath))
			}
			// finished 只证明 helper 即将返回错误候选，不证明生产 worker 已记录首错、
			// 关闭取消通道——不把 finished 当作生产侧取消已生效的同步点（评审 r2 P1-2）。
			// 但 helper 已先于 finished 武装 errArmed：从现在起新进入 helper 的调用
			//（spec §5.3 允许的竞态新在途调用）会直接返回同一 sentinel 并立即完成，
			// 不会新建无人放行的永久门控，正确实现等待中的新在途调用由此正常收口。
			// 此刻其余在途首批调用仍阻塞（故障窗口成立），逐个独立放行它们。
			for _, call := range others {
				call.releaseOnce()
			}
			var res result
			select {
			case res = <-done:
			case <-time.After(60 * time.Second):
				t.Fatalf("生成调用超时未返回，疑似死锁（workers=%d，错误场景）", workers)
			}
			t.Logf("%s：故障被 helper 记录后的竞态新入口（直接返回 sentinel、无门控收口）= %d 个", name, helper.raceEntryCount())
			verify(t, res.manifest, res.summary, res.err, helper)
			assertPoolClosed(t, helper, bound, name)
		})
	}
	assertFullSuccess := func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, helper *mgGateHelper, wantMax int) {
		t.Helper()
		if helper.processedCount() != total {
			t.Fatalf("处理数 = %d，期望 %d", helper.processedCount(), total)
		}
		if active, max := helper.snapshot(); active != 0 {
			t.Fatalf("返回后活跃数 = %d，应全部收口", active)
		} else if max != wantMax {
			t.Fatalf("最大活跃 = %d，期望 %d", max, wantMax)
		}
		if len(manifest.Files) != total {
			t.Fatalf("清单条目数 = %d，期望 %d", len(manifest.Files), total)
		}
		if summary.ScannedCount != total || summary.IncludedCount != total || summary.ExcludedCount != 0 {
			t.Fatalf("summary = %#v，期望 %d/%d/0", summary, total, total)
		}
	}
	// 成功场景：串行（1 与归一 -1）、16、256 满负荷上限恰为 min(workers,N)
	runGateScenario("serial-workers-1", root, total, 1, "", func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if err != nil {
			t.Fatalf("workers=1 应成功：%v", err)
		}
		assertFullSuccess(t, manifest, summary, helper, 1)
	})
	runGateScenario("normalized-negative-serial", root, total, -1, "", func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if err != nil {
			t.Fatalf("workers=-1（归一为 1）应成功：%v", err)
		}
		assertFullSuccess(t, manifest, summary, helper, 1)
	})
	runGateScenario("parallel-workers-16", root, total, 16, "", func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if err != nil {
			t.Fatalf("workers=16 应成功：%v", err)
		}
		assertFullSuccess(t, manifest, summary, helper, 16)
	})
	runGateScenario("parallel-workers-256", root, total, 256, "", func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if err != nil {
			t.Fatalf("workers=256 应成功：%v", err)
		}
		assertFullSuccess(t, manifest, summary, helper, 256)
	})
	// 错误场景（故障候选均在受控首批内）：sentinel 原错误、双零值、取消生效
	// （并发路径处理数 < 总数）；返回后 active==0 与最大活跃上界由 runGateScenario
	// 统一断言（assertPoolClosed），成功与错误分支一致。
	runGateScenario("serial-error-first-candidate", root, total, 1, filepath.Join(root, "c0000.bin"), func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if !errors.Is(err, sentinel) {
			t.Fatalf("应保留原 sentinel 错误，得到 %v", err)
		}
		mgAssertZeroResult(t, manifest, summary, "串行首候选错误")
		if helper.processedCount() != 1 {
			t.Fatalf("串行首错应立即终止，处理数 = %d，期望 1", helper.processedCount())
		}
	})
	runGateScenario("parallel-error-first-batch-16", root, total, 16, filepath.Join(root, "c0005.bin"), func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if !errors.Is(err, sentinel) {
			t.Fatalf("应保留原 sentinel 错误，得到 %v", err)
		}
		mgAssertZeroResult(t, manifest, summary, "并发错误 workers=16")
		// 确定性证明：错误候选恰被处理一次（errFired）。
		// 处理数计数的入口次序允许与派发次序漂移（spec §5 允许检查与开始之间的竞态窗口，
		// 在途 worker 可在错误候选入口之前先进入更晚收到的调用），因此处理数不必恰为
		// errIndex+1；取消的确定性证据是"错误候选被处理"且未处理全部候选。
		if helper.errFiredCount() != 1 {
			t.Fatalf("哨兵错误应恰好由错误候选触发一次，实际 %d 次", helper.errFiredCount())
		}
		processed := helper.processedCount()
		if processed >= total {
			t.Fatalf("出错后取消未生效，处理了全部 %d 个候选", processed)
		}
	})
	runGateScenario("parallel-error-first-batch-256", root, total, 256, filepath.Join(root, "c0100.bin"), func(t *testing.T, manifest Manifest, summary ManifestPolicySummary, err error, helper *mgGateHelper) {
		if !errors.Is(err, sentinel) {
			t.Fatalf("应保留原 sentinel 错误，得到 %v", err)
		}
		mgAssertZeroResult(t, manifest, summary, "并发错误 workers=256")
		if helper.errFiredCount() != 1 {
			t.Fatalf("哨兵错误应恰好由错误候选触发一次，实际 %d 次", helper.errFiredCount())
		}
		processed := helper.processedCount()
		if processed >= total {
			t.Fatalf("出错后取消未生效，处理了全部 %d 个候选", processed)
		}
	})
	// 小 N 场景：候选只有 10 个时池裁剪为 min(256,10)=10；第 11 次调用永不出现由
	// 收口后的精确调用数断言证明（取代依赖调度快慢的 250ms 负检查，评审 P2-3）；
	// goroutine 创建数量是否同步裁剪属实现级核对，由 W3/W5 代码审查与 race 验证承担。
	t.Run("pool-trimmed-to-small-N", func(t *testing.T) {
		smallRoot := t.TempDir()
		const smallN = 10
		for i := 0; i < smallN; i++ {
			writeTestFile(t, filepath.Join(smallRoot, fmt.Sprintf("s%02d.bin", i)), []byte("s"))
		}
		helper := newHelper(smallN, "")
		type result struct {
			manifest Manifest
			summary  ManifestPolicySummary
			err      error
		}
		done := make(chan result, 1)
		// genDone 在后台 goroutine 送出结果后关闭：done 是一次性缓冲 channel，
		// 结果被主流程取走后不再可接收，清理必须等待独立的收口信号。
		genDone := make(chan struct{})
		var firstBatch []*mgGateCall
		// 清理计时器在真正进入清理时才创建（评审 r2 P2-3）：执行阶段耗时不计入清理
		// 预算，进入清理时才给予完整 15s 期限。
		// 提前注册幂等清理：失败退出也放行全部门控、排空开始信号并等待收口。
		t.Cleanup(func() {
			cleanupDeadline := time.After(15 * time.Second)
			for {
				for _, call := range firstBatch {
					call.releaseOnce()
				}
				select {
				case call := <-helper.starts:
					call.releaseOnce()
					continue
				case <-genDone:
					return
				case <-cleanupDeadline:
					t.Logf("警告：pool-trimmed-to-small-N 清理等待生成收口超时（15s），疑似实现死锁，已如实记录")
					return
				}
			}
		})
		go func() {
			ops := manifestGenerationOps{walkDir: filepath.WalkDir, entryForPath: helper.entryForPath, afterCollect: nil}
			manifest, summary, err := generateManifestWithPolicyWorkers(smallRoot, version, policy, MaxManifestHashWorkers, ops)
			done <- result{manifest, summary, err}
			close(genDone)
		}()
		for i := 0; i < smallN; i++ {
			select {
			case call := <-helper.starts:
				firstBatch = append(firstBatch, call)
			case <-time.After(30 * time.Second):
				t.Fatalf("等待第 %d/%d 个候选开始超时", i+1, smallN)
			}
		}
		if _, max := helper.snapshot(); max != smallN {
			t.Fatalf("候选只有 %d 个时最大活跃 = %d，池应裁剪为 min(workers,N)", smallN, max)
		}
		// 放行全部首批（恰为 smallN 个门控），等待生成收口后做精确调用数断言。
		for _, call := range firstBatch {
			call.releaseOnce()
		}
		var res result
		select {
		case res = <-done:
		case <-time.After(60 * time.Second):
			t.Fatal("小目录生成调用超时，疑似死锁")
		}
		if res.err != nil {
			t.Fatalf("小目录生成应成功：%v", res.err)
		}
		// 收口后精确调用数断言：候选只有 10 个，第 11 次哈希调用永不出现；
		// 精确计数不依赖"调度足够快"，也不在超时路径遗留阻塞 goroutine。
		if helper.processedCount() != smallN {
			t.Fatalf("收口后处理数 = %d，期望恰为候选数 %d（第 11 次调用不应出现）", helper.processedCount(), smallN)
		}
		if active, max := helper.snapshot(); active != 0 {
			t.Fatalf("返回后活跃数 = %d，应全部收口", active)
		} else if max != smallN {
			t.Fatalf("候选只有 %d 个时最大活跃 = %d，池应裁剪为 min(workers,N)", smallN, max)
		}
		if len(res.manifest.Files) != smallN || res.summary.IncludedCount != smallN {
			t.Fatalf("小目录结果异常：files=%d summary=%#v", len(res.manifest.Files), res.summary)
		}
	})
	// 缝 helper 契约：N=0 返回 nil 且不调用 entryForPath
	t.Run("hash-helper-empty-candidates", func(t *testing.T) {
		calls := 0
		results, err := hashManifestCandidates(root, nil, 16, func(string, string) (FileEntry, error) {
			calls++
			return FileEntry{}, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if results != nil {
			t.Fatalf("N=0 应返回 nil 结果切片，得到 %#v", results)
		}
		if calls != 0 {
			t.Fatalf("N=0 不应调用 entryForPath，实际 %d 次", calls)
		}
	})
	// 缝 helper 契约：串行与并发共用同一入口处理同一候选列表；索引槽位契约直接检查
	// 第 i 个结果对应第 i 个候选（评审 P2-2：排序会掩盖完成顺序聚合错误，已移出断言）
	t.Run("hash-helper-direct-call", func(t *testing.T) {
		var candidates []string
		for i := 0; i < 5; i++ {
			candidates = append(candidates, filepath.Join(root, fmt.Sprintf("c%04d.bin", i)))
		}
		serial, serialErr := hashManifestCandidates(root, candidates, 1, fileEntryForPath)
		if serialErr != nil {
			t.Fatal(serialErr)
		}
		pool, poolErr := hashManifestCandidates(root, candidates, 8, fileEntryForPath)
		if poolErr != nil {
			t.Fatal(poolErr)
		}
		if len(serial) != len(candidates) || len(pool) != len(candidates) {
			t.Fatalf("结果数 serial=%d pool=%d，期望 %d", len(serial), len(pool), len(candidates))
		}
		// 不排序：第 i 个结果必须对应第 i 个候选（索引槽位回填契约），且串行与并发
		// 结果逐位完全一致。fileEntryForPath 的 Path 为斜杠相对路径。
		for i := range candidates {
			rel, relErr := filepath.Rel(root, candidates[i])
			if relErr != nil {
				t.Fatal(relErr)
			}
			wantPath := filepath.ToSlash(rel)
			if serial[i].Path != wantPath {
				t.Fatalf("串行结果第 %d 项 Path = %q，应对应候选 %q（索引槽位契约）", i, serial[i].Path, wantPath)
			}
			if pool[i].Path != wantPath {
				t.Fatalf("并发结果第 %d 项 Path = %q，应对应候选 %q（索引槽位契约）", i, pool[i].Path, wantPath)
			}
			if serial[i] != pool[i] {
				t.Fatalf("串行与并发结果第 %d 项不一致（未排序逐位比较）：%#v vs %#v", i, serial[i], pool[i])
			}
		}
	})
}

// T11 WalkFailureAndSerialErrorOrder：同一 walkDir 注入点控制较早文件与较晚遍历错误。
// workers>1 收集失败时哈希 0 次、返回原遍历错误与双零值；workers=1 在较早文件哈希
// 即失败时返回较早的哈希错误而非后遍历错误（保持旧串行顺序）。
func TestGenerateManifestWalkFailureAndSerialErrorOrder(t *testing.T) {
	const version = "parallel-hash-t11"
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		t.Fatal(err)
	}
	walkSentinel := errors.New("T11 注入的遍历错误")
	// scriptedWalk：按给定文件次序逐个回调（真实 Stat 构造 DirEntry），最后注入原遍历错误；
	// 遍历错误通过 walkDir 注入点给出，不用 chmod/ACL 伪造 Windows 遍历错误。
	scriptedWalk := func(files []string) func(string, fs.WalkDirFunc) error {
		return func(rootDir string, walkFn fs.WalkDirFunc) error {
			for _, name := range files {
				full := filepath.Join(rootDir, name)
				info, statErr := os.Stat(full)
				if statErr != nil {
					return statErr
				}
				if callErr := walkFn(full, fs.FileInfoToDirEntry(info), nil); callErr != nil {
					return callErr
				}
			}
			return walkFn(filepath.Join(rootDir, "zz-late-unreachable"), nil, walkSentinel)
		}
	}
	// 场景 1（并发收集失败）：workers=8，收集阶段在较晚位置失败 → 哈希 0 次、原遍历错误、双零值
	t.Run("concurrent-collect-failure", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "a.bin"), []byte("a"))
		writeTestFile(t, filepath.Join(root, "b.bin"), []byte("b"))
		hashCalls := 0
		ops := manifestGenerationOps{
			walkDir:      scriptedWalk([]string{"a.bin", "b.bin"}),
			entryForPath: func(string, string) (FileEntry, error) { hashCalls++; return FileEntry{}, nil },
			afterCollect: nil,
		}
		manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policy, 8, ops)
		if !errors.Is(err, walkSentinel) {
			t.Fatalf("应返回原遍历错误，得到 %v", err)
		}
		if hashCalls != 0 {
			t.Fatalf("收集失败不应进入哈希阶段，实际哈希 %d 次", hashCalls)
		}
		mgAssertZeroResult(t, manifest, summary, "并发收集失败")
	})
	// 场景 2（串行受控哈希错误）：workers=1，较早文件哈希失败 → 返回早哈希错误而非后遍历错误
	t.Run("serial-early-hash-error-wins", func(t *testing.T) {
		root := t.TempDir()
		writeTestFile(t, filepath.Join(root, "a.bin"), []byte("a"))
		writeTestFile(t, filepath.Join(root, "b.bin"), []byte("b"))
		hashSentinel := errors.New("T11 注入的早期哈希错误")
		ops := manifestGenerationOps{
			walkDir: scriptedWalk([]string{"a.bin", "b.bin"}),
			entryForPath: func(rootDir, fileName string) (FileEntry, error) {
				if filepath.Base(fileName) == "a.bin" {
					return FileEntry{}, hashSentinel
				}
				return fileEntryForPath(rootDir, fileName)
			},
			afterCollect: nil,
		}
		manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policy, 1, ops)
		if !errors.Is(err, hashSentinel) {
			t.Fatalf("串行应返回较早的哈希错误，得到 %v", err)
		}
		if errors.Is(err, walkSentinel) {
			t.Fatal("串行不应返回较晚的遍历错误（旧顺序：遇错即停）")
		}
		mgAssertZeroResult(t, manifest, summary, "串行早哈希错误")
	})
	// 场景 3（串行真实占用）：较早文件被字节区间独占锁（LockFileEx）真实拒读占用 →
	// 真实 HashFile 错误优先于注入的后遍历错误（真实 Open/Read/Close 错误由真实
	// HashFile 证明）。
	if runtime.GOOS == "windows" {
		t.Run("serial-real-lock-error-wins", func(t *testing.T) {
			root := t.TempDir()
			writeTestFile(t, filepath.Join(root, "a-locked.bin"), []byte("real io"))
			writeTestFile(t, filepath.Join(root, "b.bin"), []byte("b"))
			release := mgLockFileExclusive(t, filepath.Join(root, "a-locked.bin"))
			hashCalls := 0
			ops := manifestGenerationOps{
				walkDir: scriptedWalk([]string{"a-locked.bin", "b.bin"}),
				entryForPath: func(rootDir, fileName string) (FileEntry, error) {
					hashCalls++
					return fileEntryForPath(rootDir, fileName)
				},
				afterCollect: nil,
			}
			manifest, summary, err := generateManifestWithPolicyWorkers(root, version, policy, 1, ops)
			if err == nil {
				t.Fatal("占用文件应导致串行生成失败")
			}
			if errors.Is(err, walkSentinel) {
				t.Fatalf("串行应返回较早的真实 I/O 错误，而非注入的后遍历错误：%v", err)
			}
			if !strings.Contains(err.Error(), "a-locked.bin") {
				t.Fatalf("错误应指向被占用文件：%v", err)
			}
			mgAssertZeroResult(t, manifest, summary, "串行真实占用")
			if hashCalls != 1 {
				t.Fatalf("串行应在首个文件失败后立即停止，哈希调用 = %d，期望 1", hashCalls)
			}
			release()
			for _, name := range []string{"a-locked.bin", "b.bin"} {
				if removeErr := os.Remove(filepath.Join(root, name)); removeErr != nil {
					t.Fatalf("释放占用后无法删除 %s：%v", name, removeErr)
				}
			}
		})
	} else {
		t.Logf("记录：非 Windows 环境未执行 T11 真实占用子测，其余子测（并发收集失败、串行受控顺序）已全部执行")
	}
}

// T14 LegacyGolden（API 层）：W0 冻结的旧核心 API 空结果金样与新 API workers=1/16 的
// 真实 WriteJSONAtomic 输出逐字节比较。金样来源：W0 步骤 7，在修改前的旧核心 API 上
// 调用 GenerateManifestWithPolicy(t.TempDir(), "parallel-hash-golden", DefaultFilePolicy())
// 并经真实 WriteJSONAtomic 写出（来源 commit 与字节 SHA-256 如下常量）。expected 为嵌入
// 常量，不得由新代码动态生成。
const (
	mgLegacyEmptyGoldenVersion = "parallel-hash-golden"
	mgLegacyEmptyGoldenJSON    = "{\n  \"version\": \"parallel-hash-golden\",\n  \"files\": null\n}\n"
	mgLegacyEmptyGoldenSHA256  = "1e9df9d06bd39ae907fe5e0991ce61fa0b190ab3171ced2547ee603f7f81494a"
	mgLegacyGoldenSourceCommit = "bab112fe70482970727362dec67847267fad6f99"
)

func TestGenerateManifestLegacyEmptyGolden(t *testing.T) {
	// 自检：嵌入金样常量与 W0 冻结的来源 SHA-256 一致，防止常量被无意改动。
	sum := sha256Hex([]byte(mgLegacyEmptyGoldenJSON))
	if sum != mgLegacyEmptyGoldenSHA256 {
		t.Fatalf("嵌入金样常量的 SHA-256 = %s，与 W0 冻结值 %s 不符", sum, mgLegacyEmptyGoldenSHA256)
	}
	if !strings.Contains(mgLegacyEmptyGoldenJSON, "\"files\": null") {
		t.Fatal("嵌入金样必须原样保留 files:null（不得改为 []）")
	}
	policy := DefaultFilePolicy() // 与 W0 捕获完全一致：默认策略 + 空根
	for _, workers := range []int{1, 16} {
		t.Run(fmt.Sprintf("workers-%d", workers), func(t *testing.T) {
			root := t.TempDir()
			manifest, summary, err := GenerateManifestWithPolicyWorkers(root, mgLegacyEmptyGoldenVersion, policy, workers)
			if err != nil {
				t.Fatal(err)
			}
			if manifest.Version != mgLegacyEmptyGoldenVersion {
				t.Fatalf("version = %q，期望 %q", manifest.Version, mgLegacyEmptyGoldenVersion)
			}
			if manifest.Files != nil {
				t.Fatalf("空结果 Files 应为 nil：%#v", manifest.Files)
			}
			if summary != (ManifestPolicySummary{}) {
				t.Fatalf("summary 应全 0：%#v", summary)
			}
			outDir := t.TempDir()
			outPath := filepath.Join(outDir, fmt.Sprintf("golden-new-w%d.json", workers))
			if err := WriteJSONAtomic(outPath, manifest); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(outPath)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(data, []byte(mgLegacyEmptyGoldenJSON)) {
				t.Fatalf("新 workers=%d 输出与 W0 旧 API 金样不一致：\n金样=%q\n实际=%q", workers, mgLegacyEmptyGoldenJSON, data)
			}
		})
	}
}

// BenchmarkGenerateManifest：全流程基准。fixture 创建与结果一致性验证在计时外完成；
// 计时覆盖遍历、过滤、哈希、排序，不含 JSON 写盘；出错 Fatal 而非 skip；报告 allocs/op。
func BenchmarkGenerateManifest(b *testing.B) {
	root := b.TempDir()
	// 计时外创建固定 fixture：小文件 + 中等文件 + 采样阈值边界文件
	for i := 0; i < 100; i++ {
		mgWriteSizedFile(b, filepath.Join(root, fmt.Sprintf("small-%04d.txt", i)), int64(256+i))
	}
	for i := 0; i < 10; i++ {
		mgWriteSizedFile(b, filepath.Join(root, fmt.Sprintf("medium-%02d.bin", i)), int64(64*1024+i))
	}
	mgWriteSizedFile(b, filepath.Join(root, "edge-2mib-1.bin"), sampleThreshold-1)
	mgWriteSizedFile(b, filepath.Join(root, "edge-2mib.bin"), sampleThreshold)
	mgWriteSizedFile(b, filepath.Join(root, "edge-2mib+1.bin"), sampleThreshold+1)
	mgWriteSizedFile(b, filepath.Join(root, "large-4mib.bin"), 4*1024*1024)
	policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
	if err != nil {
		b.Fatal(err)
	}
	const version = "parallel-hash-bench"
	// 计时前一次验证结果一致：旧串行包装 vs 新 workers=1/16，全字段与三计数一致
	serialManifest, serialSummary, err := GenerateManifestWithPolicy(root, version, policy)
	if err != nil {
		b.Fatal(err)
	}
	for _, workers := range []int{1, 16} {
		manifest, summary, err := GenerateManifestWithPolicyWorkers(root, version, policy, workers)
		if err != nil {
			b.Fatal(err)
		}
		if !mgSameManifest(serialManifest, manifest) {
			b.Fatalf("workers=%d 与串行结果不一致", workers)
		}
		if summary != serialSummary {
			b.Fatalf("workers=%d 与串行 summary 不一致：%#v", workers, summary)
		}
	}
	for _, workers := range []int{1, 8, 16, 32} {
		workers := workers
		b.Run(fmt.Sprintf("Workers=%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				manifest, _, err := GenerateManifestWithPolicyWorkers(root, version, policy, workers)
				if err != nil {
					b.Fatal(err)
				}
				if len(manifest.Files) != len(serialManifest.Files) {
					b.Fatalf("结果条目数漂移：%d vs %d", len(manifest.Files), len(serialManifest.Files))
				}
			}
		})
	}
}

// BenchmarkGenerateManifestHash：仅候选哈希阶段（helper 启动、调度、聚合、全部收口），
// 不含遍历、策略、排序、JSON 与候选构造。workers=1 对同一候选列表顺序执行原
// fileEntryForPath。环境变量 MG_BENCH_ROOT 与 MG_BENCH_SAMPLE_LIST 同时设置时按
// 固定样本列表运行（NAS 模式）；均未设置时使用本地小型 fixture，不自动访问 NAS；
// 只设置其中一个直接 Fatal。样本列表按无 BOM UTF-8 JSON 路径数组解析，路径解析复用
// NormalizeManifestPath/installPath 安全边界，拒绝绝对与越界路径。加载、验证、候选构造
// 在计时外；计时外不预哈希、不读取样本内容；任何失败 Fatal 而非 skip；报告
// ns/op、allocs/op、sample_files。
func BenchmarkGenerateManifestHash(b *testing.B) {
	var candidates []string
	root := os.Getenv("MG_BENCH_ROOT")
	listPath := os.Getenv("MG_BENCH_SAMPLE_LIST")
	switch {
	case root == "" && listPath == "":
		// 本地小型 fixture（构造与候选收集在计时外）
		root = b.TempDir()
		for i := 0; i < 64; i++ {
			mgWriteSizedFile(b, filepath.Join(root, fmt.Sprintf("s%03d.bin", i)), int64(4*1024+i))
		}
		mgWriteSizedFile(b, filepath.Join(root, "edge-2mib.bin"), sampleThreshold)
		mgWriteSizedFile(b, filepath.Join(root, "edge-2mib+1.bin"), sampleThreshold+1)
		policy, err := ParseFilePolicy([]byte(`{"schema_version":1}`))
		if err != nil {
			b.Fatal(err)
		}
		summary, collectErr := walkManifestCandidates(root, policy, filepath.WalkDir, func(p string) error {
			candidates = append(candidates, p)
			return nil
		})
		if collectErr != nil {
			b.Fatal(collectErr)
		}
		if summary.IncludedCount != len(candidates) {
			b.Fatalf("本地候选收集计数异常：summary=%#v candidates=%d", summary, len(candidates))
		}
		if len(candidates) == 0 {
			b.Fatal("本地 fixture 未收集到候选")
		}
	case root == "" || listPath == "":
		b.Fatal("MG_BENCH_ROOT 与 MG_BENCH_SAMPLE_LIST 必须同时设置（NAS 固定样本模式）或同时不设置（本地模式）")
	default:
		// NAS 固定样本模式：样本列表为无 BOM UTF-8 JSON 相对路径数组
		data, err := os.ReadFile(listPath)
		if err != nil {
			b.Fatalf("读取样本列表失败：%v", err)
		}
		if !utf8.Valid(data) {
			b.Fatal("样本列表不是有效 UTF-8")
		}
		if bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}) {
			b.Fatal("样本列表带 BOM，必须是无 BOM UTF-8")
		}
		var relPaths []string
		if err := json.Unmarshal(data, &relPaths); err != nil {
			b.Fatalf("样本列表 JSON 解析失败：%v", err)
		}
		if len(relPaths) == 0 {
			b.Fatal("样本列表为空")
		}
		for _, rel := range relPaths {
			// 复用现有路径安全边界：拒绝绝对路径与越界路径，不自行重写路径规则
			abs, pathErr := installPath(root, rel)
			if pathErr != nil {
				b.Fatalf("样本路径未通过安全边界校验：%s：%v", rel, pathErr)
			}
			candidates = append(candidates, abs)
		}
	}
	for _, workers := range []int{1, 8, 16, 32} {
		workers := workers
		b.Run(fmt.Sprintf("Workers=%d", workers), func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				results, err := hashManifestCandidates(root, candidates, workers, fileEntryForPath)
				if err != nil {
					b.Fatal(err)
				}
				if len(results) != len(candidates) {
					b.Fatalf("结果条目数漂移：%d vs %d", len(results), len(candidates))
				}
			}
			// 自定义指标必须在最后一次 ResetTimer 之后报告：ResetTimer 会清空此前
			// ReportMetric 写入的 sample_files（评审 P2-1），导致 W6 指标解析失败。
			// 计时循环结束、StopTimer 后报告，保证四组输出均含 sample_files。
			b.StopTimer()
			b.ReportMetric(float64(len(candidates)), "sample_files")
		})
	}
}
