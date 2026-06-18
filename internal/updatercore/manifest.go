package updatercore

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

func CompareManifests(rootDir string, installed *Manifest, remote Manifest) (Plan, error) {
	remote, remoteMap, err := NormalizeManifest(remote)
	if err != nil {
		return Plan{}, err
	}
	plan := Plan{LatestVersion: remote.Version, RemoteManifest: remote}
	if installed == nil {
		plan.FirstInstallRecovery = true
		for _, entry := range remote.Files {
			if IsMetadataFile(entry.Path) {
				continue // 元数据文件完全忽略
			}
			target, err := installPath(rootDir, entry.Path)
			if err != nil {
				return Plan{}, err
			}
			ok, err := VerifyFile(target, entry)
			if err != nil {
				return Plan{}, fmt.Errorf("校验本地文件失败：%s：%w", entry.Path, err)
			}
			if !ok {
				plan.Add = append(plan.Add, entry)
				plan.DownloadSize += entry.Size
			}
		}
		return plan, nil
	}

	_, installedMap, err := NormalizeManifest(*installed)
	if err != nil {
		return Plan{}, err
	}
	for key, remoteEntry := range remoteMap {
		// 元数据文件完全忽略
		if IsMetadataFile(remoteEntry.Path) {
			continue
		}
		installedEntry, exists := installedMap[key]
		switch {
		case !exists:
			target, err := installPath(rootDir, remoteEntry.Path)
			if err != nil {
				return Plan{}, err
			}
			if _, err := os.Stat(target); err == nil {
				ok, verifyErr := VerifyFile(target, remoteEntry)
				if verifyErr != nil {
					return Plan{}, fmt.Errorf("校验本地未知文件失败：%s：%w", remoteEntry.Path, verifyErr)
				}
				if ok {
					continue
				}
				return Plan{}, fmt.Errorf("新增文件目标已存在但不属于已安装清单，已中止以保护未知用户文件：%s", remoteEntry.Path)
			} else if !os.IsNotExist(err) {
				return Plan{}, err
			}
			plan.Add = append(plan.Add, remoteEntry)
			plan.DownloadSize += remoteEntry.Size
		case !sameEntry(installedEntry, remoteEntry):
			// 用户配置文件：远端与本地不同 → 跳过不覆盖（保护用户修改）
			if IsUserConfigFile(remoteEntry.Path) {
				continue
			}
			plan.Modify = append(plan.Modify, remoteEntry)
			plan.DownloadSize += remoteEntry.Size
		}
	}
	for key, installedEntry := range installedMap {
		// 元数据文件和用户配置文件都不删除
		if IsMetadataFile(installedEntry.Path) || IsUserConfigFile(installedEntry.Path) {
			continue
		}
		if _, exists := remoteMap[key]; !exists {
			plan.Delete = append(plan.Delete, installedEntry)
		}
	}
	sortEntries(plan.Add)
	sortEntries(plan.Modify)
	sortEntries(plan.Delete)
	return plan, nil
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
