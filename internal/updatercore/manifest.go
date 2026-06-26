package updatercore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 元数据文件：不纳入 manifest，所有组件完全忽略
var metadataFiles = map[string]bool{
	"latest.json":   true,
	"manifest.json": true,
}

// 用户配置文件：纳入 manifest（供 Sync-Cos.ps1 正常同步），但 Updater.exe 端保护不覆盖
var userConfigFiles = map[string]bool{
	"AimanServer/webgemini_server_v2/config/config.json": true,
	"U-Geminiserver_core/lan.txt":                        true,
	"U-Geminiserver_core/static/hanzipinyin.json":        true,
	"U-Geminiserver_core/static/rare_chars.json":         true,
	"U-Geminiserver_core/static/units.json":              true,
	"U-Geminiserver_core/static/voices.json":             true,
}

// 用户配置文件扩展名通配
var userConfigExts = map[string]bool{
	".ini": true,
}

func IsMetadataFile(path string) bool { return metadataFiles[path] }
func IsUserConfigFile(path string) bool {
	if userConfigFiles[path] {
		return true
	}
	return userConfigExts[filepath.Ext(path)]
}

func GenerateManifest(rootDir, version string) (Manifest, error) {
	if strings.TrimSpace(version) == "" {
		return Manifest{}, fmt.Errorf("版本号不能为空")
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return Manifest{}, err
	}
	var manifest Manifest
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
			if rel == StateDirName || strings.HasPrefix(rel, StateDirName+"/") {
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
		fileEntry, err := fileEntryForPath(absRoot, fileName)
		if err != nil {
			return err
		}
		manifest.Files = append(manifest.Files, fileEntry)
		return nil
	})
	if err != nil {
		return Manifest{}, err
	}
	sortEntries(manifest.Files)
	return manifest, nil
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
	return CompareManifestsWithProgress(rootDir, installed, remote, 1, nil)
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
	err          error
}

func CompareManifestsWithProgress(rootDir string, installed *Manifest, remote Manifest, workers int, progress func(ProgressEvent)) (Plan, error) {
	remote, remoteMap, err := NormalizeManifest(remote)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{LatestVersion: remote.Version, RemoteManifest: remote}
	if installed == nil {
		plan.FirstInstallRecovery = true
		reporter := newPlanProgressReporter(len(remote.Files), progress)
		results := compareRemoteEntries(rootDir, remote.Files, nil, workers, reporter)
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
	results := compareRemoteEntries(rootDir, remote.Files, installedMap, workers, reporter)
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
	}
	for _, installedEntry := range installedNormalized.Files {
		key := manifestKey(installedEntry.Path)
		// 元数据文件和用户配置文件都不删除
		if IsMetadataFile(installedEntry.Path) || IsUserConfigFile(installedEntry.Path) {
			reporter.advance(installedEntry.Path)
			continue
		}
		if _, exists := remoteMap[key]; !exists {
			plan.Delete = append(plan.Delete, installedEntry)
		}
		reporter.advance(installedEntry.Path)
	}
	sortEntries(plan.Add)
	sortEntries(plan.Modify)
	sortEntries(plan.Delete)
	return plan, nil
}

func compareRemoteEntries(rootDir string, remoteFiles []FileEntry, installedMap map[string]FileEntry, workers int, reporter *planProgressReporter) []compareRemoteResult {
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
			results[item.index] = compareRemoteEntry(rootDir, item.entry, installedMap)
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

func compareRemoteEntry(rootDir string, remoteEntry FileEntry, installedMap map[string]FileEntry) compareRemoteResult {
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
		if IsUserConfigFile(remoteEntry.Path) {
			return compareRemoteResult{}
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
