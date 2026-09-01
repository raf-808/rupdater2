package updatercore

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

func debugLog(enabled bool, format string, args ...any) {
	if !enabled {
		return
	}
	fmt.Fprintf(os.Stdout, "[debug] "+format+"\n", args...)
}

func Run(ctx context.Context, opts Options) error {
	rootDir, exePath, err := resolveRuntimePaths(opts)
	if err != nil {
		return err
	}
	stateDir, err := resolveStateDir(rootDir, opts.StateDir)
	if err != nil {
		return err
	}
	ui := opts.UI
	if ui == nil {
		ui = DefaultUI(opts.AutoConfirm, opts.Silent)
	}
	if opts.Workers <= 0 {
		opts.Workers = 4
	}
	if opts.Client == nil {
		opts.Client = defaultHTTPClient()
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if setter, ok := ui.(CancelSetter); ok {
		setter.SetCancel(cancel)
	}
	if dialogUI, ok := ui.(*DialogUI); ok {
		dialogUI.Debug = opts.Debug
	}

	work := func(ctx context.Context) error {
		defer cleanupPolicyCandidate(stateDir)
		if opts.CompleteSelfUpdate {
			return completeSelfUpdate(rootDir, opts.SelfUpdateTarget, opts.SelfUpdatePending, ui)
		}
		if err := recoverIfNeeded(stateDir, rootDir, ui); err != nil {
			return err
		}
		if err := maybeHandoffSelfUpdate(rootDir, opts, ui); err != nil {
			return err
		}
		return runUpdate(ctx, rootDir, stateDir, exePath, opts, ui)
	}

	if runner, ok := ui.(GUIRunner); ok {
		return runner.RunMessageLoop(work, ctx)
	}
	return work(ctx)
}

func runUpdate(ctx context.Context, rootDir, stateDir, exePath string, opts Options, ui UI) error {
	version := VersionState{}
	_, err := readOptionalJSON(filepath.Join(stateDir, "version.json"), &version)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("读取本地版本失败：%w", err)
	}

	config := Config{}
	if ok, err := readOptionalJSON(filepath.Join(stateDir, "config.json"), &config); err != nil {
		return fmt.Errorf("读取配置失败：%w", err)
	} else if !ok || !validURL(config.LatestURL) {
		ui.ShowVersionInfo(version.Version, "未知")
		ui.Info("未找到可用配置，界面已启动。")
		return ErrMissingConfig
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	ui.Progress(ProgressEvent{Phase: "Check"})
	latest := LatestInfo{}
	if err := fetchJSON(ctx, opts.Client, config.LatestURL, &latest); err != nil {
		return fmt.Errorf("下载 latest.json 失败：%w", err)
	}
	if latest.Version == "" || !validURL(latest.ManifestURL) || !validURL(latest.FilesBaseURL) {
		return fmt.Errorf("latest.json 缺少必要字段")
	}
	if latest.Version == version.Version {
		ui.ShowVersionInfo(version.Version, latest.Version)
		ui.Info("当前已经是最新版本。")
		return ErrNoUpdate
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	remoteManifest := Manifest{}
	if err := fetchJSON(ctx, opts.Client, latest.ManifestURL, &remoteManifest); err != nil {
		return fmt.Errorf("下载 manifest.json 失败：%w", err)
	}
	if remoteManifest.Version != latest.Version {
		return fmt.Errorf("latest.json 版本与 manifest.json 版本不一致：%s != %s", latest.Version, remoteManifest.Version)
	}
	remoteManifest, _, err = NormalizeManifest(remoteManifest)
	if err != nil {
		return fmt.Errorf("远端清单非法：%w", err)
	}

	var installed *Manifest
	installedManifest := Manifest{}
	if ok, err := readOptionalJSON(filepath.Join(stateDir, "installed_manifest.json"), &installedManifest); err != nil {
		return fmt.Errorf("读取已安装清单失败：%w", err)
	} else if ok {
		installed = &installedManifest
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	policy, err := loadPolicyForPlan(ctx, stateDir, latest.FilesBaseURL, opts.Client, installed, remoteManifest, opts.Debug)
	if err != nil {
		return err
	}

	plan, planSummary, err := CompareManifestsWithPolicySummary(rootDir, installed, remoteManifest, policy, opts.Workers, ui.Progress)
	if err != nil {
		return err
	}
	debugLog(opts.Debug,
		"policy_plan_summary add_count=%d modify_count=%d delete_count=%d preserved_modify=%d preserved_delete=%d",
		len(plan.Add), len(plan.Modify), len(plan.Delete), planSummary.PreservedModify, planSummary.PreservedDelete)
	plan.CurrentVersion = version.Version
	plan.LatestVersion = latest.Version
	for _, entry := range plan.Delete {
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			return fmt.Errorf("远端清单不能删除当前 Updater.exe")
		}
	}
	if len(plan.Add) == 0 && len(plan.Modify) == 0 && len(plan.Delete) == 0 {
		ui.Info("文件已与最新清单一致，正在提交版本状态。")
		return commitWithProgress(stateDir, remoteManifest, latest.Version, ui, opts.Debug)
	}
	if !ui.ConfirmPlan(plan) {
		return ErrUserCancelled
	}

	session := newSession(plan)
	if err := writeSession(stateDir, session, "Plan"); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	stageResult, err := stageFiles(ctx, rootDir, stateDir, latest.FilesBaseURL, opts.Client, opts.Workers, plan, ui)
	if err != nil {
		cleanupBeforeRootChange(stateDir)
		return err
	}
	plan = stageResult.EffectivePlan
	skipped := stageResult.Skipped
	var partialManifest Manifest
	if len(skipped) > 0 {
		partialManifest, err = buildPartialInstalledManifestWithSkipped(installed, plan, skipped, version.Version)
		if err != nil {
			cleanupBeforeRootChange(stateDir)
			return err
		}
	}

	// Stage decisions can remove files from the transaction. Rebuild the
	// session before any operation can touch the installation root.
	session = newSession(plan)
	if err := writeSession(stateDir, session, "OccupancyCheck"); err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	ui.Progress(ProgressEvent{Phase: "OccupancyCheck"})
	if err := occupancyCheck(rootDir, plan, exePath, ui); err != nil {
		cleanupBeforeRootChange(stateDir)
		return err
	}

	if err := ctx.Err(); err != nil {
		cleanupBeforeRootChange(stateDir)
		return ErrUserCancelled
	}

	if err := backupFiles(rootDir, stateDir, plan, exePath, &session, ui); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return ErrUserCancelled
	}

	if err := switchFiles(rootDir, stateDir, plan, exePath, &session, ui); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return err
	}

	if err := ctx.Err(); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return ErrUserCancelled
	}

	if len(skipped) > 0 {
		sessionRemoved, err := commitPartialWithProgress(stateDir, partialManifest, skipped, ui, opts.Debug)
		if err != nil {
			if !sessionRemoved {
				_ = recoverFromSession(stateDir, rootDir, session, ui)
			}
			return err
		}
		ui.Info(fmt.Sprintf("更新部分完成：有 %d 个文件被跳过，下次检查将继续重试。", len(skipped)))
		return NewUpdatePartialError(skipped)
	}

	ui.Progress(ProgressEvent{Phase: "Commit"})
	if err := commitWithProgress(stateDir, remoteManifest, latest.Version, ui, opts.Debug); err != nil {
		_ = recoverFromSession(stateDir, rootDir, session, ui)
		return err
	}
	cleanupAfterCommit(stateDir)
	ui.Info("更新完成。")
	return nil
}

func loadPolicyForPlan(ctx context.Context, stateDir, filesBaseURL string, client *http.Client, installed *Manifest, remote Manifest, debug bool) (FilePolicy, error) {
	remoteEntry, remoteHasPolicy := manifestEntry(remote, PolicyFileName)
	installedHasPolicy := false
	if installed != nil {
		_, installedHasPolicy = manifestEntry(*installed, PolicyFileName)
	}
	if !remoteHasPolicy {
		if installedHasPolicy {
			err := policyError("policy_missing_manifest_entry", "已安装清单包含 %s，但远端清单缺失该条目", PolicyFileName)
			debugPolicyLoadFailure(debug, "remote", err)
			return FilePolicy{}, err
		}
		policy := DefaultFilePolicy()
		debugLog(debug, "policy_load_success source=fallback schema_version=%d policy_file=%s", policy.SchemaVersion, PolicyFileName)
		return policy, nil
	}
	if remoteEntry.Size > MaxPolicySize {
		err := policyError("policy_too_large", "清单声明 %s 为 %d 字节，超过 %d 字节", PolicyFileName, remoteEntry.Size, MaxPolicySize)
		debugPolicyLoadFailure(debug, "remote", err)
		return FilePolicy{}, err
	}

	candidate := filepath.Join(stateDir, PolicyCandidateName)
	rawURL, err := fileDownloadURL(filesBaseURL, remoteEntry.Path)
	if err != nil {
		wrapped := policyError("policy_download_failed", "构造策略下载地址失败：%v", err)
		debugPolicyLoadFailure(debug, "remote", wrapped)
		return FilePolicy{}, wrapped
	}
	if err := downloadFileLimited(ctx, client, rawURL, candidate, MaxPolicySize); err != nil {
		if PolicyFailureReason(err) == "policy_too_large" {
			debugPolicyLoadFailure(debug, "remote", err)
			return FilePolicy{}, err
		}
		wrapped := policyError("policy_download_failed", "下载候选策略失败：%v", err)
		debugPolicyLoadFailure(debug, "remote", wrapped)
		return FilePolicy{}, wrapped
	}
	ok, err := VerifyFile(candidate, remoteEntry)
	if err != nil || !ok {
		if err == nil {
			err = fmt.Errorf("大小或 SHA-256 哈希不匹配")
		}
		wrapped := policyError("policy_hash_mismatch", "候选策略校验失败：%v", err)
		debugPolicyLoadFailure(debug, "remote", wrapped)
		return FilePolicy{}, wrapped
	}
	policy, err := LoadFilePolicy(candidate)
	if err != nil {
		debugPolicyLoadFailure(debug, "remote", err)
		return FilePolicy{}, err
	}
	debugLog(debug, "policy_load_success source=remote schema_version=%d policy_file=%s", policy.SchemaVersion, PolicyFileName)
	return policy, nil
}

func manifestEntry(manifest Manifest, wanted string) (FileEntry, bool) {
	for _, entry := range manifest.Files {
		normalized, err := NormalizeManifestPath(entry.Path)
		if err == nil && strings.EqualFold(normalized, wanted) {
			entry.Path = normalized
			return entry, true
		}
	}
	return FileEntry{}, false
}

func debugPolicyLoadFailure(debug bool, source string, err error) {
	debugLog(debug, "policy_load_failed source=%s policy_file=%s failure_reason=%s", source, PolicyFileName, PolicyFailureReason(err))
}

func cleanupPolicyCandidate(stateDir string) {
	candidate := filepath.Join(stateDir, PolicyCandidateName)
	_ = os.Remove(candidate)
	_ = os.Remove(candidate + ".download")
}

func resolveRuntimePaths(opts Options) (string, string, error) {
	exePath := opts.ExePath
	if exePath == "" {
		var err error
		exePath, err = os.Executable()
		if err != nil {
			return "", "", err
		}
	}
	exePath, err := filepath.Abs(exePath)
	if err != nil {
		return "", "", err
	}
	rootDir := opts.RootDir
	if rootDir == "" {
		rootDir = filepath.Dir(exePath)
	}
	rootDir, err = filepath.Abs(rootDir)
	if err != nil {
		return "", "", err
	}
	return rootDir, exePath, nil
}

func resolveStateDir(rootDir, configured string) (string, error) {
	if configured != "" {
		return filepath.Abs(configured)
	}
	candidates := []string{
		filepath.Join(rootDir, StateDirName),
		filepath.Join(filepath.Dir(rootDir), StateDirName),
		filepath.Join(rootDir, "build", StateDirName),
	}
	for _, candidate := range candidates {
		if _, err := os.Stat(candidate); err == nil {
			return candidate, nil
		}
	}
	return filepath.Abs(filepath.Join(rootDir, StateDirName))
}

func validURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme != "" && parsed.Host != ""
}

func newSession(plan Plan) Session {
	session := Session{
		TargetVersion: plan.LatestVersion,
		Add:           pathsOf(plan.Add),
		Modify:        pathsOf(plan.Modify),
		Delete:        pathsOf(plan.Delete),
		StartedAt:     time.Now().Format(time.RFC3339),
	}
	return session
}

func pathsOf(entries []FileEntry) []string {
	paths := make([]string, 0, len(entries))
	for _, entry := range entries {
		paths = append(paths, entry.Path)
	}
	return paths
}

func writeSession(stateDir string, session Session, phase string) error {
	session.Phase = phase
	return WriteJSONAtomic(filepath.Join(stateDir, "session.json"), session)
}

type stageOutcomeStatus uint8

const (
	stageDownloaded stageOutcomeStatus = iota + 1
	stageFailed
	stageCancelled
	stageSkipped
)

type stageOutcome struct {
	index  int
	entry  FileEntry
	status stageOutcomeStatus
	err    error
}

func isImportantFile(rawPath string) bool {
	normalized, err := NormalizeManifestPath(rawPath)
	if err != nil {
		return false
	}
	for _, special := range []string{"manifest.json", "latest.json", "updater-policy.json"} {
		if stringsEqualFold(normalized, special) {
			return true
		}
	}
	base := normalized
	if slash := strings.LastIndexByte(base, '/'); slash >= 0 {
		base = base[slash+1:]
	}
	if dot := strings.LastIndexByte(base, '.'); dot >= 0 && dot < len(base)-1 {
		ext := base[dot:]
		for _, importantExt := range []string{".dll", ".exe", ".usf", ".ush"} {
			if stringsEqualFold(ext, importantExt) {
				return true
			}
		}
	}
	return false
}

func isRequiredFile(path string) bool { return isImportantFile(path) }

func normalizeStageEntries(entries []FileEntry) ([]FileEntry, error) {
	if entries == nil {
		return nil, nil
	}
	normalized := make([]FileEntry, len(entries))
	copy(normalized, entries)
	for i := range normalized {
		path, err := NormalizeManifestPath(normalized[i].Path)
		if err != nil {
			return nil, err
		}
		normalized[i].Path = path
	}
	return normalized, nil
}

func normalizeStagePlan(plan Plan) (Plan, error) {
	var err error
	if plan.Add, err = normalizeStageEntries(plan.Add); err != nil {
		return Plan{}, err
	}
	if plan.Modify, err = normalizeStageEntries(plan.Modify); err != nil {
		return Plan{}, err
	}
	if plan.Delete, err = normalizeStageEntries(plan.Delete); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func stateStagingPath(stateDir, rel string) (string, error) {
	normalized, err := NormalizeManifestPath(rel)
	if err != nil {
		return "", err
	}
	base := filepath.Join(stateDir, "staging")
	target := filepath.Join(base, filepath.FromSlash(normalized))
	if err := ensureInside(base, target); err != nil {
		return "", err
	}
	return target, nil
}

func stateRollbackPath(stateDir, rel string) (string, error) {
	normalized, err := NormalizeManifestPath(rel)
	if err != nil {
		return "", err
	}
	base := filepath.Join(stateDir, "rollback")
	target := filepath.Join(base, filepath.FromSlash(normalized))
	if err := ensureInside(base, target); err != nil {
		return "", err
	}
	return target, nil
}

func stageFiles(ctx context.Context, rootDir, stateDir, filesBaseURL string, client *http.Client, workers int, plan Plan, ui UI) (StageResult, error) {
	if err := ctx.Err(); err != nil {
		return StageResult{}, ErrUserCancelled
	}
	plan, err := normalizeStagePlan(plan)
	if err != nil {
		return StageResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return StageResult{}, ErrUserCancelled
	}
	result := StageResult{EffectivePlan: plan}
	entries := append([]FileEntry{}, plan.Add...)
	entries = append(entries, plan.Modify...)
	if len(entries) == 0 {
		return result, nil
	}
	if workers <= 0 {
		workers = 1
	}
	if client == nil {
		client = defaultHTTPClient()
	}

	stagingDir := filepath.Join(stateDir, "staging")
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return StageResult{}, err
	}
	cleanupStagingTempFiles(stagingDir)
	cleanupStagingOrphans(stagingDir, entries, rootDir)

	var bytesTotal int64
	for _, entry := range entries {
		bytesTotal += entry.Size
	}

	type stageJob struct {
		index int
		entry FileEntry
	}
	jobs := make(chan stageJob)
	outcomesCh := make(chan stageOutcome, len(entries))
	var wg sync.WaitGroup
	var progressMu sync.Mutex
	var completed int32
	var bytesDone int64
	lastProgress := time.Now()
	lastSpeedTime := time.Now()
	lastSpeedBytes := int64(0)
	speed := float64(0)

	reportCached := func(entry FileEntry) {
		progressMu.Lock()
		completedCount := atomic.AddInt32(&completed, 1)
		newBytesDone := atomic.AddInt64(&bytesDone, entry.Size)
		ui.Progress(ProgressEvent{
			Phase:          "Stage",
			CurrentFile:    entry.Path + " (已缓存)",
			CompletedFiles: int(completedCount),
			TotalFiles:     len(entries),
			BytesDone:      newBytesDone,
			BytesTotal:     bytesTotal,
			SpeedBytes:     0,
		})
		progressMu.Unlock()
	}

	progressFor := func(entry FileEntry) func(int64) {
		return func(n int64) {
			done := atomic.AddInt64(&bytesDone, n)
			now := time.Now()
			progressMu.Lock()
			if now.Sub(lastProgress) >= 200*time.Millisecond {
				lastProgress = now
				elapsed := now.Sub(lastSpeedTime).Seconds()
				if elapsed > 0 {
					speed = float64(done-lastSpeedBytes) / elapsed
				}
				lastSpeedTime = now
				lastSpeedBytes = done
				ui.Progress(ProgressEvent{
					Phase:          "Stage",
					CurrentFile:    entry.Path,
					CompletedFiles: int(atomic.LoadInt32(&completed)),
					TotalFiles:     len(entries),
					BytesDone:      done,
					BytesTotal:     bytesTotal,
					SpeedBytes:     speed,
				})
			}
			progressMu.Unlock()
		}
	}

	runEntry := func(runCtx context.Context, entry FileEntry) error {
		dest, err := stateStagingPath(stateDir, entry.Path)
		if err != nil {
			return err
		}
		ok, err := VerifyFile(dest, entry)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("验证已存在文件失败：%s：%w", entry.Path, err)
		}
		if ok {
			reportCached(entry)
			return nil
		}
		rawURL, err := fileDownloadURL(filesBaseURL, entry.Path)
		if err != nil {
			return fmt.Errorf("构造下载地址失败：%s：%w", entry.Path, err)
		}
		if err := downloadAndVerify(runCtx, client, rawURL, dest, entry, progressFor(entry)); err != nil {
			return fmt.Errorf("下载失败：%s：%w", entry.Path, err)
		}
		progressMu.Lock()
		completedCount := atomic.AddInt32(&completed, 1)
		ui.Progress(ProgressEvent{
			Phase:          "Stage",
			CurrentFile:    entry.Path,
			CompletedFiles: int(completedCount),
			TotalFiles:     len(entries),
			BytesDone:      atomic.LoadInt64(&bytesDone),
			BytesTotal:     bytesTotal,
			SpeedBytes:     speed,
		})
		progressMu.Unlock()
		return nil
	}

	worker := func() {
		defer wg.Done()
		for item := range jobs {
			if ctx.Err() != nil {
				outcomesCh <- stageOutcome{index: item.index, entry: item.entry, status: stageCancelled}
				continue
			}
			err := runEntry(ctx, item.entry)
			if err != nil {
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					outcomesCh <- stageOutcome{index: item.index, entry: item.entry, status: stageCancelled, err: err}
				} else {
					outcomesCh <- stageOutcome{index: item.index, entry: item.entry, status: stageFailed, err: err}
				}
				continue
			}
			outcomesCh <- stageOutcome{index: item.index, entry: item.entry, status: stageDownloaded}
		}
	}

	for i := 0; i < workers; i++ {
		wg.Add(1)
		go worker()
	}
	for index, entry := range entries {
		select {
		case jobs <- stageJob{index: index, entry: entry}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(jobs)
	wg.Wait()
	close(outcomesCh)

	outcomes := make([]stageOutcome, len(entries))
	received := make([]bool, len(entries))
	for outcome := range outcomesCh {
		if outcome.index >= 0 && outcome.index < len(outcomes) {
			outcomes[outcome.index] = outcome
			received[outcome.index] = true
		}
	}
	if ctx.Err() != nil {
		return StageResult{}, ErrUserCancelled
	}
	for index := range entries {
		if !received[index] {
			return StageResult{}, fmt.Errorf("Stage 未返回文件结果：%s", entries[index].Path)
		}
		if outcomes[index].status == stageCancelled {
			return StageResult{}, ErrUserCancelled
		}
	}

	requiredFailures := make([]int, 0)
	ordinaryFailures := make([]int, 0)
	for index, outcome := range outcomes {
		if outcome.status != stageFailed {
			continue
		}
		if isImportantFile(outcome.entry.Path) {
			requiredFailures = append(requiredFailures, index)
		} else {
			ordinaryFailures = append(ordinaryFailures, index)
		}
	}

	failureUI, interactive := ui.(FileFailureUI)
	if !interactive {
		if len(requiredFailures) > 0 {
			return StageResult{}, stageOutcomeError(outcomes[requiredFailures[0]])
		}
		if len(ordinaryFailures) > 0 {
			return StageResult{}, stageOutcomeError(outcomes[ordinaryFailures[0]])
		}
	}

	for _, index := range requiredFailures {
		outcome := &outcomes[index]
		resolved := false
		for !resolved {
			if err := ctx.Err(); err != nil {
				return StageResult{}, ErrUserCancelled
			}
			decision := failureUI.DecideFileFailure(ctx, stageFileFailure(*outcome))
			if err := ctx.Err(); err != nil {
				return StageResult{}, ErrUserCancelled
			}
			switch decision {
			case FileFailureDecisionRetry:
				err := runEntry(ctx, outcome.entry)
				if err == nil {
					if ctx.Err() != nil {
						return StageResult{}, ErrUserCancelled
					}
					outcome.status = stageDownloaded
					outcome.err = nil
					resolved = true
					continue
				}
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return StageResult{}, ErrUserCancelled
				}
				outcome.err = err
			case FileFailureDecisionCancel:
				return StageResult{}, ErrUserCancelled
			default:
				return StageResult{}, stageOutcomeError(*outcome)
			}
		}
	}

	var skipped []SkippedFile
	skipAll := false
	for _, index := range ordinaryFailures {
		outcome := &outcomes[index]
		if skipAll {
			outcome.status = stageSkipped
			skipped = append(skipped, skippedFileFromOutcome(*outcome))
			continue
		}
		resolved := false
		for !resolved {
			if err := ctx.Err(); err != nil {
				return StageResult{}, ErrUserCancelled
			}
			decision := failureUI.DecideFileFailure(ctx, stageFileFailure(*outcome))
			if err := ctx.Err(); err != nil {
				return StageResult{}, ErrUserCancelled
			}
			switch decision {
			case FileFailureDecisionRetry:
				err := runEntry(ctx, outcome.entry)
				if err == nil {
					if ctx.Err() != nil {
						return StageResult{}, ErrUserCancelled
					}
					outcome.status = stageDownloaded
					outcome.err = nil
					resolved = true
					continue
				}
				if ctx.Err() != nil || errors.Is(err, context.Canceled) {
					return StageResult{}, ErrUserCancelled
				}
				outcome.err = err
			case FileFailureDecisionSkip:
				outcome.status = stageSkipped
				skipped = append(skipped, skippedFileFromOutcome(*outcome))
				resolved = true
			case FileFailureDecisionSkipAll:
				outcome.status = stageSkipped
				skipped = append(skipped, skippedFileFromOutcome(*outcome))
				skipAll = true
				resolved = true
			case FileFailureDecisionCancel:
				return StageResult{}, ErrUserCancelled
			default:
				return StageResult{}, stageOutcomeError(*outcome)
			}
		}
	}

	result.EffectivePlan = effectivePlanFromStage(plan, outcomes, skipped)
	result.Skipped = skipped
	return result, nil
}

func stageOutcomeError(outcome stageOutcome) error {
	if outcome.err != nil {
		return outcome.err
	}
	return fmt.Errorf("文件处理失败：%s", outcome.entry.Path)
}

func stageFileFailure(outcome stageOutcome) FileFailure {
	failure := FileFailure{
		Path:      outcome.entry.Path,
		Important: isImportantFile(outcome.entry.Path),
	}
	if outcome.err != nil {
		failure.Error = outcome.err.Error()
	}
	return failure
}

func skippedFileFromOutcome(outcome stageOutcome) SkippedFile {
	skipped := SkippedFile{Path: outcome.entry.Path}
	if outcome.err != nil {
		skipped.Error = outcome.err.Error()
	}
	return skipped
}

func effectivePlanFromStage(plan Plan, outcomes []stageOutcome, skipped []SkippedFile) Plan {
	result := plan
	successful := make(map[string]bool, len(outcomes))
	for _, outcome := range outcomes {
		if outcome.status == stageDownloaded {
			successful[manifestKey(outcome.entry.Path)] = true
		}
	}
	result.Add = filterSuccessfulEntries(plan.Add, successful)
	result.Modify = filterSuccessfulEntries(plan.Modify, successful)
	result.Delete = append([]FileEntry(nil), plan.Delete...)
	if len(skipped) > 0 {
		result.Delete = nil
	}
	result.DownloadSize = 0
	for _, entry := range result.Add {
		result.DownloadSize += entry.Size
	}
	for _, entry := range result.Modify {
		result.DownloadSize += entry.Size
	}
	return result
}

func filterSuccessfulEntries(entries []FileEntry, successful map[string]bool) []FileEntry {
	if entries == nil {
		return nil
	}
	filtered := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		if successful[manifestKey(entry.Path)] {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func occupancyCheck(rootDir string, plan Plan, exePath string, ui UI) error {
	var targets []string
	for _, entry := range append(append([]FileEntry{}, plan.Modify...), plan.Delete...) {
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			continue
		}
		target, err := installPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(target); errors.Is(err, os.ErrNotExist) {
			continue
		}
		targets = append(targets, target)
	}
	locked, err := FindLockedFiles(targets)
	if err != nil {
		return err
	}
	if len(locked) == 0 {
		return nil
	}
	if !ui.ConfirmProcessTermination(locked) {
		return ErrUserCancelled
	}
	if err := TerminateLockedProcesses(locked); err != nil {
		return err
	}
	locked, err = FindLockedFiles(targets)
	if err != nil {
		return err
	}
	if len(locked) > 0 {
		return fmt.Errorf("文件仍然被占用，更新已中止")
	}
	return nil
}

func backupFiles(rootDir, stateDir string, plan Plan, exePath string, session *Session, ui UI) error {
	if err := os.RemoveAll(filepath.Join(stateDir, "rollback")); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(stateDir, "rollback"), 0o755); err != nil {
		return err
	}
	if err := writeSession(stateDir, *session, "Backup"); err != nil {
		return err
	}
	entries := append([]FileEntry{}, plan.Modify...)
	entries = append(entries, plan.Delete...)
	for _, entry := range entries {
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			continue
		}
		source, err := installPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if _, err := os.Stat(source); errors.Is(err, os.ErrNotExist) {
			continue
		}
		backup, err := stateRollbackPath(stateDir, entry.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(backup), 0o755); err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Backup", CurrentFile: entry.Path})
		if err := os.Rename(source, backup); err != nil {
			return fmt.Errorf("备份失败：%s：%w", entry.Path, err)
		}
		session.BackedUp = append(session.BackedUp, MovedFile{Path: entry.Path, BackupPath: backup})
		if err := writeSession(stateDir, *session, "Backup"); err != nil {
			return err
		}
	}
	return nil
}

func switchFiles(rootDir, stateDir string, plan Plan, exePath string, session *Session, ui UI) error {
	if err := writeSession(stateDir, *session, "Switch"); err != nil {
		return err
	}
	entries := append([]FileEntry{}, plan.Add...)
	entries = append(entries, plan.Modify...)
	for _, entry := range entries {
		staged, err := stateStagingPath(stateDir, entry.Path)
		if err != nil {
			return err
		}
		if isSelfUpdaterEntry(rootDir, entry, exePath) {
			pending := pendingSelfUpdatePath(rootDir)
			if err := os.MkdirAll(filepath.Dir(pending), 0o755); err != nil {
				return err
			}
			_ = os.Remove(pending)
			if err := os.Rename(staged, pending); err != nil {
				return fmt.Errorf("保存 Updater.exe 自更新文件失败：%w", err)
			}
			target, err := installPath(rootDir, entry.Path)
			if err != nil {
				return err
			}
			if err := markSelfUpdate(rootDir, target, pending); err != nil {
				return err
			}
			session.PendingSelfUpdate = pending
			if err := writeSession(stateDir, *session, "Switch"); err != nil {
				return err
			}
			continue
		}
		target, err := installPath(rootDir, entry.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Switch", CurrentFile: entry.Path})
		_ = os.Remove(target)
		if err := os.Rename(staged, target); err != nil {
			return fmt.Errorf("切换失败：%s：%w", entry.Path, err)
		}
		session.Switched = append(session.Switched, MovedFile{Path: entry.Path})
		if err := writeSession(stateDir, *session, "Switch"); err != nil {
			return err
		}
	}
	return nil
}

func buildPartialInstalledManifest(installed *Manifest, plan Plan, version string) (Manifest, error) {
	return buildPartialInstalledManifestWithSkipped(installed, plan, nil, version)
}

func buildPartialInstalledManifestWithSkipped(installed *Manifest, plan Plan, skipped []SkippedFile, version string) (Manifest, error) {
	entries := make(map[string]FileEntry)
	if installed != nil {
		_, installedEntries, err := NormalizeManifest(*installed)
		if err != nil {
			return Manifest{}, err
		}
		entries = installedEntries
	}

	skippedKeys := make(map[string]struct{}, len(skipped))
	for _, skippedFile := range skipped {
		normalizedPath, err := NormalizeManifestPath(skippedFile.Path)
		if err != nil {
			return Manifest{}, err
		}
		skippedKeys[manifestKey(normalizedPath)] = struct{}{}
	}

	// A first-install recovery may have remote files that were already valid
	// on disk and therefore never appeared in Plan.Add. Include those entries,
	// but never turn a skipped Add into an installed entry. Existing entries
	// remain authoritative for preserved user files and skipped modifications.
	if len(plan.RemoteManifest.Files) > 0 {
		remote, _, err := NormalizeManifest(plan.RemoteManifest)
		if err != nil {
			return Manifest{}, err
		}
		for _, entry := range remote.Files {
			key := manifestKey(entry.Path)
			if _, wasSkipped := skippedKeys[key]; wasSkipped {
				continue
			}
			if _, exists := entries[key]; !exists {
				entries[key] = entry
			}
		}
	}

	for _, entry := range append(append([]FileEntry{}, plan.Add...), plan.Modify...) {
		normalizedPath, err := NormalizeManifestPath(entry.Path)
		if err != nil {
			return Manifest{}, err
		}
		entry.Path = normalizedPath
		entries[manifestKey(normalizedPath)] = entry
	}

	files := make([]FileEntry, 0, len(entries))
	for _, entry := range entries {
		files = append(files, entry)
	}
	sortEntries(files)
	return Manifest{Version: version, Files: files}, nil
}

func commitPartialWithProgress(stateDir string, manifest Manifest, skipped []SkippedFile, ui UI, debug bool) (bool, error) {
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在规范化部分安装清单"})
	}
	normalized, _, err := NormalizeManifest(manifest)
	if err != nil {
		return false, err
	}
	if err := os.Remove(filepath.Join(stateDir, "session.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	debugLog(debug, "partial commit: write installed_manifest start")
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在写入部分 installed_manifest.json"})
	}
	if err := WriteJSONAtomic(filepath.Join(stateDir, "installed_manifest.json"), normalized); err != nil {
		cleanupAfterCommit(stateDir)
		return true, NewUpdatePartialError(skipped, err)
	}
	debugLog(debug, "partial commit: write installed_manifest done")
	cleanupAfterCommit(stateDir)
	return true, nil
}

func commit(stateDir string, manifest Manifest, version string) error {
	return commitWithProgress(stateDir, manifest, version, nil, false)
}

func commitWithProgress(stateDir string, manifest Manifest, version string, ui UI, debug bool) error {
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在规范化清单"})
	}
	debugLog(debug, "commit: normalize start")
	normalized, _, err := NormalizeManifest(manifest)
	if err != nil {
		return err
	}
	debugLog(debug, "commit: normalize done")
	manifestPath := filepath.Join(stateDir, "installed_manifest.json")
	versionPath := filepath.Join(stateDir, "version.json")
	oldManifest, hadManifest, err := readStateFile(manifestPath)
	if err != nil {
		return err
	}
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在写入 installed_manifest.json"})
	}
	debugLog(debug, "commit: write installed_manifest start")
	if err := WriteJSONAtomic(manifestPath, normalized); err != nil {
		return err
	}
	debugLog(debug, "commit: write installed_manifest done")
	if ui != nil {
		ui.Progress(ProgressEvent{Phase: "Commit", CurrentFile: "正在写入 version.json"})
	}
	debugLog(debug, "commit: write version start")
	if err := WriteJSONAtomic(versionPath, VersionState{Version: version}); err != nil {
		_ = restoreStateFile(manifestPath, oldManifest, hadManifest)
		return err
	}
	debugLog(debug, "commit: write version done")
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
	return nil
}

func readStateFile(fileName string) ([]byte, bool, error) {
	data, err := os.ReadFile(fileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func restoreStateFile(fileName string, data []byte, existed bool) error {
	if !existed {
		return os.Remove(fileName)
	}
	if err := os.MkdirAll(filepath.Dir(fileName), 0o755); err != nil {
		return err
	}
	return os.WriteFile(fileName, data, 0o644)
}

// cleanupStagingTempFiles 清理 staging 目录中的临时文件（.download 文件）
func cleanupStagingTempFiles(stagingDir string) {
	_ = filepath.Walk(stagingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if !info.IsDir() && strings.HasSuffix(path, ".download") {
			_ = os.Remove(path)
		}
		return nil
	})
}

// cleanupStagingOrphans 清理 staging 目录中不在当前 manifest 中的文件
func cleanupStagingOrphans(stagingDir string, entries []FileEntry, _ string) {
	// 构建期望的文件路径集合
	expected := make(map[string]bool)
	for _, entry := range entries {
		normalized, err := NormalizeManifestPath(entry.Path)
		if err != nil {
			continue
		}
		staged := filepath.Join(stagingDir, filepath.FromSlash(normalized))
		if err := ensureInside(stagingDir, staged); err != nil {
			continue
		}
		expected[staged] = true
	}

	// 删除不在期望集合中的文件
	_ = filepath.Walk(stagingDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			return nil
		}
		if !expected[path] {
			_ = os.Remove(path)
		}
		return nil
	})
}

func cleanupBeforeRootChange(stateDir string) {
	_ = os.RemoveAll(filepath.Join(stateDir, "staging"))
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
}

func cleanupAfterCommit(stateDir string) {
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
	_ = os.RemoveAll(filepath.Join(stateDir, "staging"))
	_ = os.RemoveAll(filepath.Join(stateDir, "rollback"))
}

func recoverIfNeeded(stateDir, rootDir string, ui UI) error {
	var session Session
	ok, err := readOptionalJSON(filepath.Join(stateDir, "session.json"), &session)
	if err != nil {
		return fmt.Errorf("读取恢复状态失败：%w", err)
	}
	if !ok {
		return nil
	}
	ui.Info("检测到上次更新未完成，正在恢复到稳定状态。")
	return recoverFromSession(stateDir, rootDir, session, ui)
}

func recoverFromSession(stateDir, rootDir string, session Session, ui UI) error {
	for i := len(session.Switched) - 1; i >= 0; i-- {
		item := session.Switched[i]
		target, err := installPath(rootDir, item.Path)
		if err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Recover", CurrentFile: item.Path})
		_ = os.Remove(target)
	}
	for i := len(session.BackedUp) - 1; i >= 0; i-- {
		item := session.BackedUp[i]
		target, err := installPath(rootDir, item.Path)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		ui.Progress(ProgressEvent{Phase: "Recover", CurrentFile: item.Path})
		if _, err := os.Stat(item.BackupPath); err == nil {
			_ = os.Remove(target)
			if err := os.Rename(item.BackupPath, target); err != nil {
				return fmt.Errorf("恢复失败：%s：%w", item.Path, err)
			}
		}
	}
	if session.PendingSelfUpdate != "" {
		_ = os.Remove(session.PendingSelfUpdate)
		_ = os.Remove(selfUpdateMarkerPath(rootDir))
	}
	_ = os.RemoveAll(filepath.Join(stateDir, "staging"))
	_ = os.RemoveAll(filepath.Join(stateDir, "rollback"))
	_ = os.Remove(filepath.Join(stateDir, "session.json"))
	return nil
}
