package updatercore

import (
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

func NormalizeManifestPath(raw string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", fmt.Errorf("路径不能为空")
	}
	if filepath.IsAbs(value) || path.IsAbs(strings.ReplaceAll(value, "\\", "/")) || hasWindowsDrivePrefix(value) {
		return "", fmt.Errorf("拒绝绝对路径：%s", raw)
	}

	clean := path.Clean(strings.ReplaceAll(value, "\\", "/"))
	if clean == "." || clean == ".." || strings.HasPrefix(clean, "../") {
		return "", fmt.Errorf("拒绝越级路径：%s", raw)
	}
	for _, part := range strings.Split(clean, "/") {
		if part == "" || part == "." || part == ".." || strings.ContainsRune(part, 0) {
			return "", fmt.Errorf("非法路径片段：%s", raw)
		}
	}
	return clean, nil
}

func manifestKey(normalized string) string {
	if runtime.GOOS == "windows" {
		return strings.ToLower(normalized)
	}
	return normalized
}

func samePath(a, b string) bool {
	if runtime.GOOS == "windows" {
		return strings.EqualFold(filepath.Clean(a), filepath.Clean(b))
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func installPath(rootDir, rel string) (string, error) {
	normalized, err := NormalizeManifestPath(rel)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(rootDir)
	if err != nil {
		return "", err
	}
	target := filepath.Join(absRoot, filepath.FromSlash(normalized))
	if err := ensureInside(absRoot, target); err != nil {
		return "", err
	}
	return target, nil
}

func statePath(rootDir string, parts ...string) string {
	all := append([]string{rootDir, StateDirName}, parts...)
	return filepath.Join(all...)
}

func stagingPath(rootDir, rel string) (string, error) {
	normalized, err := NormalizeManifestPath(rel)
	if err != nil {
		return "", err
	}
	base := statePath(rootDir, "staging")
	target := filepath.Join(base, filepath.FromSlash(normalized))
	if err := ensureInside(base, target); err != nil {
		return "", err
	}
	return target, nil
}

func rollbackPath(rootDir, rel string) (string, error) {
	normalized, err := NormalizeManifestPath(rel)
	if err != nil {
		return "", err
	}
	base := statePath(rootDir, "rollback")
	target := filepath.Join(base, filepath.FromSlash(normalized))
	if err := ensureInside(base, target); err != nil {
		return "", err
	}
	return target, nil
}

func ensureInside(root, target string) error {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	absTarget, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(absRoot, absTarget)
	if err != nil {
		return err
	}
	if rel == "." {
		return nil
	}
	if strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." || filepath.IsAbs(rel) {
		return fmt.Errorf("路径越过安装根目录：%s", target)
	}
	return nil
}

func hasWindowsDrivePrefix(value string) bool {
	if len(value) >= 2 && value[1] == ':' {
		first := value[0]
		return (first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z')
	}
	return strings.HasPrefix(value, `\\`)
}

func sortEntries(entries []FileEntry) {
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && strings.Compare(entries[j-1].Path, entries[j].Path) > 0; j-- {
			entries[j-1], entries[j] = entries[j], entries[j-1]
		}
	}
}
