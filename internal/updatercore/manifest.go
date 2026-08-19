package updatercore

import (
	"fmt"
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

func GenerateManifestWithPolicy(rootDir, version string, policy FilePolicy) (Manifest, ManifestPolicySummary, error) {
	if strings.TrimSpace(version) == "" {
		return Manifest{}, ManifestPolicySummary{}, fmt.Errorf("版本号不能为空")
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return Manifest{}, ManifestPolicySummary{}, err
	}
	var manifest Manifest
	var summary ManifestPolicySummary
	manifest.Version = version
	err = filepath.WalkDir(absRoot, func(fileName string, entry os.DirEntry, walkErr error) error {
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
		fileEntry, err := fileEntryForPath(absRoot, fileName)
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
