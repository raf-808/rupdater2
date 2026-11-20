package updatercore

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var compatibilityFilePolicy = DefaultFilePolicy()

func IsMetadataFile(path string) bool {
	normalized, err := NormalizeManifestPath(path)
	return err == nil && isMetadataPath(normalized)
}

func IsUserConfigFile(path string) bool { return compatibilityFilePolicy.ShouldPreserve(path) }

type ManifestPolicySummary struct {
	ScannedCount  int
	IncludedCount int
	ExcludedCount int
}

type PolicyPlanSummary struct {
	PreservedModify int
	PreservedDelete int
}

func GenerateManifest(rootDir, version string) (Manifest, error) {
	manifest, _, err := GenerateManifestWithPolicy(rootDir, version, DefaultFilePolicy())
	return manifest, err
}

// GenerateManifestWithPolicy 旧 API：签名与默认行为保持不变，等价于新 API 的
// workers=1 串行包装（WalkDir 回调内即时哈希、遇错即停、原错误先后顺序）。
func GenerateManifestWithPolicy(rootDir, version string, policy FilePolicy) (Manifest, ManifestPolicySummary, error) {
	return GenerateManifestWithPolicyWorkers(rootDir, version, policy, 1)
}

// MaxManifestHashWorkers 是新生成 API 允许的最大哈希 worker 数：防误配置的资源上限，
// 不是推荐性能值。超限返回错误而非静默截断；该上限只约束生成 API，不迁移到 Compare。
const MaxManifestHashWorkers = 256

// GenerateManifestWithPolicyWorkers 新生成 API：workers<=0 归一为 1（与现有 Compare
// 一致，走原串行路径）；2..MaxManifestHashWorkers 使用严格两阶段有界池（先完整收集
// 候选、遍历成功后才并发哈希）；>MaxManifestHashWorkers 返回错误与双零值，不遍历、
// 不哈希。任何遍历/哈希错误都返回 Manifest{} 与全零 ManifestPolicySummary，不返回
// 部分结果。
func GenerateManifestWithPolicyWorkers(rootDir, version string, policy FilePolicy, workers int) (Manifest, ManifestPolicySummary, error) {
	return generateManifestWithPolicyWorkers(rootDir, version, policy, workers, manifestGenerationOps{
		walkDir:      filepath.WalkDir,
		entryForPath: fileEntryForPath,
		afterCollect: nil,
	})
}

// manifestGenerationOps 是单次生成调用内的依赖值（最小测试缝）：默认生产绑定
// filepath.WalkDir / fileEntryForPath / nil，由导出 API 每次调用构造，无可写全局
// 默认实例；所有注入点由调用方闭包持有，不引入全局可并行写入 hook。
type manifestGenerationOps struct {
	walkDir      func(string, fs.WalkDirFunc) error
	entryForPath func(string, string) (FileEntry, error)
	afterCollect func()
}

func generateManifestWithPolicyWorkers(rootDir, version string, policy FilePolicy, workers int, ops manifestGenerationOps) (Manifest, ManifestPolicySummary, error) {
	// 参数检查顺序：版本非空（只检查 TrimSpace，不裁剪存储值）→ workers 归一/上限
	// → 根路径解析。
	if strings.TrimSpace(version) == "" {
		return Manifest{}, ManifestPolicySummary{}, fmt.Errorf("版本号不能为空")
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > MaxManifestHashWorkers {
		return Manifest{}, ManifestPolicySummary{}, fmt.Errorf("workers 不能大于 256：%d", workers)
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return Manifest{}, ManifestPolicySummary{}, err
	}
	manifest := Manifest{Version: version}
	if workers == 1 {
		// 串行兼容路径：visit 在 WalkDir 回调内立即哈希，失败直接终止遍历并返回
		// 原错误（保持旧的错误先后顺序）；不收集全树候选、不启动池。
		summary, err := walkManifestCandidates(absRoot, policy, ops.walkDir, func(fileName string) error {
			fileEntry, err := ops.entryForPath(absRoot, fileName)
			if err != nil {
				return err
			}
			manifest.Files = append(manifest.Files, fileEntry)
			return nil
		})
		if err != nil {
			return Manifest{}, ManifestPolicySummary{}, err
		}
		sortEntries(manifest.Files)
		return manifest, summary, nil
	}
	// 并发两阶段路径：阶段一单 goroutine 完整收集有序候选路径，遍历与哈希严格不重叠。
	var candidates []string
	summary, err := walkManifestCandidates(absRoot, policy, ops.walkDir, func(fileName string) error {
		candidates = append(candidates, fileName)
		return nil
	})
	if err != nil {
		// 遍历失败：原错误与双零值，哈希 0 次，完全不启动哈希池。
		return Manifest{}, ManifestPolicySummary{}, err
	}
	// 候选数为 0：不建池、不调用 afterCollect，Files 保持 nil（JSON 为 files:null）。
	if len(candidates) == 0 {
		return manifest, summary, nil
	}
	// 阶段间屏障：仅在并发路径完整收集成功后、开始非空候选哈希前调用一次。
	if ops.afterCollect != nil {
		ops.afterCollect()
	}
	// 阶段二：有界池并发哈希。
	results, err := hashManifestCandidates(absRoot, candidates, workers, ops.entryForPath)
	if err != nil {
		return Manifest{}, ManifestPolicySummary{}, err
	}
	manifest.Files = results
	sortEntries(manifest.Files)
	return manifest, summary, nil
}

// walkManifestCandidates 是串行与并发路径共享的同步遍历 helper：严格沿用原
// WalkDir 回调的过滤/计数次序（walkErr→根跳过→filepathRelSlash→目录/状态 SkipDir
// →metadata→非普通文件→scanned++→publish 排除/excluded++→included++→visit），
// 不按 publish 目录规则提前剪枝，不新增 Stat 预读或缓存文件大小；preserve 不参与
// 生成过滤。visit 只收到候选完整路径（不保留 DirEntry/句柄）；visit 或遍历错误原样
// 上返；遍历失败返回零 summary。
func walkManifestCandidates(absRoot string, policy FilePolicy, walkDir func(string, fs.WalkDirFunc) error, visit func(string) error) (ManifestPolicySummary, error) {
	var summary ManifestPolicySummary
	err := walkDir(absRoot, func(fileName string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if fileName == absRoot {
			return nil
		}
		rel, err := filepathRelSlash(absRoot, fileName)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if isStatePath(rel) {
				return filepath.SkipDir
			}
			return nil
		}
		if IsMetadataFile(rel) {
			return nil // 元数据文件不纳入清单
		}
		if entry.Type()&os.ModeType != 0 {
			return nil
		}
		summary.ScannedCount++
		if !policy.ShouldPublish(rel) {
			summary.ExcludedCount++
			return nil
		}
		summary.IncludedCount++
		return visit(fileName)
	})
	if err != nil {
		return ManifestPolicySummary{}, err
	}
	return summary, nil
}

// manifestHashJob 只携带候选索引与路径，不含内容、打开句柄或整份 DirEntry 信息。
type manifestHashJob struct {
	index int
	path  string
}

// hashManifestCandidates 对已收集的有序候选路径执行哈希：workers 归一后为 1 时直接
// 按候选顺序调用同一 entryForPath（不建池，遇错即停）；>1 才建无缓冲 jobs 的有界池；
// 候选数为 0 返回 nil 且不调用 entryForPath。结果按固定索引槽位回填（第 i 个结果对应
// 第 i 个候选），不并发 append、不按 map/完成次序聚合，以保持原候选次序供调用方
// 原样 sortEntries。首个哈希错误经调用内 done + sync.Once 原样保存并触发取消：
// 生产者每轮发送前先快速检查取消，再在发送/取消间 select，取消后停止派发；worker
// 接收阶段 select 同时监听取消与 jobs，接收后、开始新文件前再次检查取消——任一
// 观察点命中取消即退出该 worker，不再领取任务；fail 后该 worker 同样退出，其他
// 在途 I/O 等其自然返回并 defer Close 后收口，向上返回原 error（不包装，保留
// *os.PathError 等结构与 errors.Is 判断）。
func hashManifestCandidates(absRoot string, candidates []string, workers int, entryForPath func(string, string) (FileEntry, error)) ([]FileEntry, error) {
	total := len(candidates)
	if total == 0 {
		return nil, nil
	}
	if workers <= 0 {
		workers = 1
	}
	if workers == 1 {
		results := make([]FileEntry, total)
		for index, path := range candidates {
			fileEntry, err := entryForPath(absRoot, path)
			if err != nil {
				return nil, err
			}
			results[index] = fileEntry
		}
		return results, nil
	}
	// 有界池：实际 worker 数 W=min(workers, 候选数)；结果切片预分配长度 N。
	poolSize := workers
	if poolSize > total {
		poolSize = total
	}
	results := make([]FileEntry, total)
	// 调用内取消：首个哈希错误原样保存并关闭 done；done 与 firstErr 只由 once 写一次。
	done := make(chan struct{})
	var once sync.Once
	var firstErr error
	fail := func(err error) {
		once.Do(func() {
			firstErr = err
			close(done)
		})
	}
	// 无缓冲 jobs：只有空闲 worker 接收后生产者才能继续，有自然背压。
	jobs := make(chan manifestHashJob)
	var wg sync.WaitGroup
	worker := func() {
		defer wg.Done()
		for {
			// 接收阶段同时监听取消与 jobs：观察到取消即退出本 worker，不再领取；
			// jobs 已关闭（生产者停止派发）则正常收尾退出。
			var job manifestHashJob
			select {
			case <-done:
				return
			case received, ok := <-jobs:
				if !ok {
					return
				}
				job = received
			}
			// 接收后、开始新文件前检查取消：观察到取消即退出（本次领取作废，
			// 不写入结果）；不 continue 回接收点，避免取消后反复领取并丢弃。
			select {
			case <-done:
				return
			default:
			}
			fileEntry, err := entryForPath(absRoot, job.path)
			if err != nil {
				fail(err)
				// 首错已保存并触发取消：本 worker 退出；其他在途哈希自然结束后退出。
				return
			}
			// 每个索引恰有一个 worker 写入；不同索引互不重叠，无并发 append。
			results[job.index] = fileEntry
		}
	}
	for i := 0; i < poolSize; i++ {
		wg.Add(1)
		go worker()
	}
	// 生产者在调用 goroutine 同步发送：每轮先快速检查取消（取消后不再反复进入
	// 发送竞争），再保留发送/取消 select 兜底。
sendLoop:
	for index, path := range candidates {
		select {
		case <-done:
			break sendLoop
		default:
		}
		select {
		case jobs <- manifestHashJob{index: index, path: path}:
		case <-done:
			break sendLoop
		}
	}
	// 唯一 close：生产者停止派发后关闭 jobs，worker 接收 select 的 ok==false 分支随之
	// 退出，等待全部收口。
	close(jobs)
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	return results, nil
}

func NormalizeManifest(manifest Manifest) (Manifest, map[string]FileEntry, error) {
	result := Manifest{Version: manifest.Version, Files: make([]FileEntry, 0, len(manifest.Files))}
	entries := make(map[string]FileEntry, len(manifest.Files))
	for _, entry := range manifest.Files {
		normalized, err := NormalizeManifestPath(entry.Path)
		if err != nil {
			return Manifest{}, nil, err
		}
		entry.Path = normalized
		key := manifestKey(normalized)
		if _, exists := entries[key]; exists {
			return Manifest{}, nil, fmt.Errorf("清单包含重复路径：%s", normalized)
		}
		entries[key] = entry
		result.Files = append(result.Files, entry)
	}
	sortEntries(result.Files)
	return result, entries, nil
}

type planProgressReporter struct {
	emit      func(ProgressEvent)
	total     int
	completed int
	lastEmit  time.Time
	mu        sync.Mutex
}

func newPlanProgressReporter(total int, emit func(ProgressEvent)) *planProgressReporter {
	if total <= 0 || emit == nil {
		return nil
	}
	return &planProgressReporter{emit: emit, total: total}
}

func (r *planProgressReporter) advance(path string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.completed++
	now := time.Now()
	if r.lastEmit.IsZero() || r.completed == r.total || now.Sub(r.lastEmit) >= 200*time.Millisecond {
		r.lastEmit = now
		r.emit(ProgressEvent{
			Phase:          "Plan",
			CurrentFile:    path,
			CompletedFiles: r.completed,
			TotalFiles:     r.total,
		})
	}
}

func CompareManifests(rootDir string, installed *Manifest, remote Manifest) (Plan, error) {
	return CompareManifestsWithPolicy(rootDir, installed, remote, DefaultFilePolicy(), 1, nil)
}

type compareAction uint8

const (
	compareNoop compareAction = iota
	compareAdd
	compareModify
)

type compareRemoteResult struct {
	action       compareAction
	entry        FileEntry
	downloadSize int64
	preserved    bool
	err          error
}

func CompareManifestsWithProgress(rootDir string, installed *Manifest, remote Manifest, workers int, progress func(ProgressEvent)) (Plan, error) {
	return CompareManifestsWithPolicy(rootDir, installed, remote, DefaultFilePolicy(), workers, progress)
}

func CompareManifestsWithPolicy(rootDir string, installed *Manifest, remote Manifest, policy FilePolicy, workers int, progress func(ProgressEvent)) (Plan, error) {
	return compareManifestsWithPolicy(rootDir, installed, remote, policy, workers, progress, nil)
}

func CompareManifestsWithPolicySummary(rootDir string, installed *Manifest, remote Manifest, policy FilePolicy, workers int, progress func(ProgressEvent)) (Plan, PolicyPlanSummary, error) {
	var summary PolicyPlanSummary
	plan, err := compareManifestsWithPolicy(rootDir, installed, remote, policy, workers, progress, &summary)
	return plan, summary, err
}

func compareManifestsWithPolicy(rootDir string, installed *Manifest, remote Manifest, policy FilePolicy, workers int, progress func(ProgressEvent), summary *PolicyPlanSummary) (Plan, error) {
	remote, remoteMap, err := NormalizeManifest(remote)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{LatestVersion: remote.Version, RemoteManifest: remote}
	if installed == nil {
		plan.FirstInstallRecovery = true
		reporter := newPlanProgressReporter(len(remote.Files), progress)
		results := compareRemoteEntries(rootDir, remote.Files, nil, policy, workers, reporter)
		for _, result := range results {
			if result.err != nil {
				return Plan{}, result.err
			}
			if result.action == compareAdd {
				plan.Add = append(plan.Add, result.entry)
				plan.DownloadSize += result.downloadSize
			}
		}
		return plan, nil
	}

	installedNormalized, installedMap, err := NormalizeManifest(*installed)
	if err != nil {
		return Plan{}, err
	}
	reporter := newPlanProgressReporter(len(remote.Files)+len(installedNormalized.Files), progress)
	results := compareRemoteEntries(rootDir, remote.Files, installedMap, policy, workers, reporter)
	for _, result := range results {
		if result.err != nil {
			return Plan{}, result.err
		}
		switch result.action {
		case compareAdd:
			plan.Add = append(plan.Add, result.entry)
			plan.DownloadSize += result.downloadSize
		case compareModify:
			plan.Modify = append(plan.Modify, result.entry)
			plan.DownloadSize += result.downloadSize
		}
		if result.preserved {
			if summary != nil {
				summary.PreservedModify++
			}
		}
	}
	for _, installedEntry := range installedNormalized.Files {
		key := manifestKey(installedEntry.Path)
		// 元数据是不受管系统不变量；用户文件删除保护由显式策略决定。
		if IsMetadataFile(installedEntry.Path) {
			reporter.advance(installedEntry.Path)
			continue
		}
		if _, exists := remoteMap[key]; !exists {
			if policy.ShouldPreserve(installedEntry.Path) {
				if summary != nil {
					summary.PreservedDelete++
				}
			} else {
				plan.Delete = append(plan.Delete, installedEntry)
			}
		}
		reporter.advance(installedEntry.Path)
	}
	sortEntries(plan.Add)
	sortEntries(plan.Modify)
	sortEntries(plan.Delete)
	return plan, nil
}

func compareRemoteEntries(rootDir string, remoteFiles []FileEntry, installedMap map[string]FileEntry, policy FilePolicy, workers int, reporter *planProgressReporter) []compareRemoteResult {
	if workers <= 0 {
		workers = 1
	}
	type job struct {
		index int
		entry FileEntry
	}

	jobs := make(chan job)
	results := make([]compareRemoteResult, len(remoteFiles))
	var wg sync.WaitGroup

	worker := func() {
		defer wg.Done()
		for item := range jobs {
			results[item.index] = compareRemoteEntry(rootDir, item.entry, installedMap, policy)
			reporter.advance(item.entry.Path)
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for index, entry := range remoteFiles {
		jobs <- job{index: index, entry: entry}
	}
	close(jobs)
	wg.Wait()
	return results
}

func compareRemoteEntry(rootDir string, remoteEntry FileEntry, installedMap map[string]FileEntry, policy FilePolicy) compareRemoteResult {
	if IsMetadataFile(remoteEntry.Path) {
		return compareRemoteResult{}
	}

	if installedMap == nil {
		target, err := installPath(rootDir, remoteEntry.Path)
		if err != nil {
			return compareRemoteResult{err: err}
		}
		ok, err := VerifyFile(target, remoteEntry)
		if err != nil {
			return compareRemoteResult{err: fmt.Errorf("校验本地文件失败：%s：%w", remoteEntry.Path, err)}
		}
		if !ok {
			return compareRemoteResult{action: compareAdd, entry: remoteEntry, downloadSize: remoteEntry.Size}
		}
		return compareRemoteResult{}
	}

	key := manifestKey(remoteEntry.Path)
	installedEntry, exists := installedMap[key]
	switch {
	case !exists:
		target, err := installPath(rootDir, remoteEntry.Path)
		if err != nil {
			return compareRemoteResult{err: err}
		}
		if _, err := os.Stat(target); err == nil {
			ok, verifyErr := VerifyFile(target, remoteEntry)
			if verifyErr != nil {
				return compareRemoteResult{err: fmt.Errorf("校验本地未知文件失败：%s：%w", remoteEntry.Path, verifyErr)}
			}
			if ok {
				return compareRemoteResult{}
			}
			return compareRemoteResult{err: fmt.Errorf("新增文件目标已存在但不属于已安装清单，已中止以保护未知用户文件：%s", remoteEntry.Path)}
		} else if !os.IsNotExist(err) {
			return compareRemoteResult{err: err}
		}
		return compareRemoteResult{action: compareAdd, entry: remoteEntry, downloadSize: remoteEntry.Size}
	case !sameEntry(installedEntry, remoteEntry):
		if policy.ShouldPreserve(remoteEntry.Path) {
			// 智能合并：检查用户是否修改过本地文件
			target, err := installPath(rootDir, remoteEntry.Path)
			if err != nil {
				return compareRemoteResult{err: err}
			}
			// 与已安装清单中的版本比较，判断用户是否修改过
			localOk, localErr := VerifyFile(target, installedEntry)
			if localErr != nil {
				return compareRemoteResult{err: fmt.Errorf("校验本地用户配置文件失败：%s：%w", remoteEntry.Path, localErr)}
			}
			if localOk {
				// 本地文件与已安装清单一致（用户没改过），可以安全更新
				return compareRemoteResult{action: compareModify, entry: remoteEntry, downloadSize: remoteEntry.Size}
			}
			// 用户修改过，跳过覆盖，保留用户版本
			return compareRemoteResult{preserved: true}
		}
		return compareRemoteResult{action: compareModify, entry: remoteEntry, downloadSize: remoteEntry.Size}
	default:
		return compareRemoteResult{}
	}
}

func sameEntry(a, b FileEntry) bool {
	aPath, aErr := NormalizeManifestPath(a.Path)
	bPath, bErr := NormalizeManifestPath(b.Path)
	if aErr != nil || bErr != nil {
		return false
	}
	return manifestKey(aPath) == manifestKey(bPath) &&
		a.Size == b.Size &&
		a.HeadHash == b.HeadHash &&
		a.TailHash == b.TailHash
}

func filepathRelSlash(rootDir, fileName string) (string, error) {
	rel, err := filepath.Rel(rootDir, fileName)
	if err != nil {
		return "", err
	}
	return NormalizeManifestPath(filepath.ToSlash(rel))
}
