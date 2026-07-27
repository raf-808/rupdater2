package updatercore

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"regexp"
	"strings"
)

const (
	PolicyFileName      = "updater-policy.json"
	PolicyCandidateName = "policy_candidate.json"
	MaxPolicySize       = int64(256 * 1024)
	policySchemaVersion = 1
	maxPolicyRegexCount = 64
	maxPolicyRegexBytes = 512
)

type PolicyError struct {
	Reason string
	Err    error
}

func (e *PolicyError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}

func (e *PolicyError) Unwrap() error { return e.Err }

func PolicyFailureReason(err error) string {
	var policyErr *PolicyError
	if errors.As(err, &policyErr) {
		return policyErr.Reason
	}
	return "policy_download_failed"
}

func policyError(reason, format string, args ...any) error {
	return &PolicyError{Reason: reason, Err: fmt.Errorf(format, args...)}
}

type FilePolicy struct {
	SchemaVersion int           `json:"schema_version"`
	Publish       PolicySection `json:"publish"`
	Preserve      PolicySection `json:"preserve"`
}

type PolicySection struct {
	Include MatchSet `json:"include"`
	Exclude MatchSet `json:"exclude"`
}

type MatchSet struct {
	Directories []string `json:"directories"`
	Files       []string `json:"files"`
	Extensions  []string `json:"extensions"`
	Regex       []string `json:"regex"`

	directorySet map[string]struct{}
	fileSet      map[string]struct{}
	extensionSet map[string]struct{}
	compiled     []*regexp.Regexp
}

type policyJSON struct {
	SchemaVersion *int           `json:"schema_version"`
	Publish       *policySection `json:"publish"`
	Preserve      *policySection `json:"preserve"`
}

type policySection struct {
	Include *matchSetJSON `json:"include"`
	Exclude *matchSetJSON `json:"exclude"`
}

type matchSetJSON struct {
	Directories []string `json:"directories"`
	Files       []string `json:"files"`
	Extensions  []string `json:"extensions"`
	Regex       []string `json:"regex"`
}

func ParseFilePolicy(data []byte) (FilePolicy, error) {
	if int64(len(data)) > MaxPolicySize {
		return FilePolicy{}, policyError("policy_too_large", "策略文件超过 %d 字节", MaxPolicySize)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var wire policyJSON
	if err := decoder.Decode(&wire); err != nil {
		return FilePolicy{}, policyError("policy_invalid_json", "严格解析 JSON 失败：%v", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return FilePolicy{}, policyError("policy_invalid_json", "策略 JSON 后存在第二个值")
		}
		return FilePolicy{}, policyError("policy_invalid_json", "策略 JSON 尾部非法：%v", err)
	}
	if wire.SchemaVersion == nil {
		return FilePolicy{}, policyError("policy_unsupported_schema", "schema_version 必须显式提供")
	}
	if *wire.SchemaVersion != policySchemaVersion {
		return FilePolicy{}, policyError("policy_unsupported_schema", "不支持 schema_version=%d", *wire.SchemaVersion)
	}

	policy := FilePolicy{SchemaVersion: *wire.SchemaVersion}
	if wire.Publish != nil {
		policy.Publish = sectionFromJSON(*wire.Publish)
	}
	if wire.Preserve != nil {
		policy.Preserve = sectionFromJSON(*wire.Preserve)
	}
	if err := normalizePolicy(&policy); err != nil {
		return FilePolicy{}, err
	}
	return policy, nil
}

func LoadFilePolicy(fileName string) (FilePolicy, error) {
	file, err := os.Open(fileName)
	if err != nil {
		return FilePolicy{}, policyError("policy_download_failed", "打开策略文件 %s 失败：%v", fileName, err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, MaxPolicySize+1))
	if err != nil {
		return FilePolicy{}, policyError("policy_download_failed", "读取策略文件 %s 失败：%v", fileName, err)
	}
	if int64(len(data)) > MaxPolicySize {
		return FilePolicy{}, policyError("policy_too_large", "策略文件 %s 超过 %d 字节", fileName, MaxPolicySize)
	}
	return ParseFilePolicy(data)
}

func DefaultFilePolicy() FilePolicy {
	policy := FilePolicy{
		SchemaVersion: policySchemaVersion,
		Preserve: PolicySection{
			Include: MatchSet{
				Files: []string{
					"AimanServer/webgemini_server_v2/config/config.json",
					"U-Geminiserver_core/lan.txt",
					"U-Geminiserver_core/static/hanzipinyin.json",
					"U-Geminiserver_core/static/rare_chars.json",
					"U-Geminiserver_core/static/units.json",
					"U-Geminiserver_core/static/voices.json",
				},
				Extensions: []string{
					".ini", ".json", ".txt", ".log", ".yaml",
					".yml", ".toml", ".cfg", ".conf", ".config",
				},
			},
			Exclude: MatchSet{Files: []string{PolicyFileName}},
		},
	}
	if err := normalizePolicy(&policy); err != nil {
		panic(err)
	}
	return policy
}

func (p FilePolicy) ShouldPublish(rawPath string) bool {
	normalized, ok := policyMatchPath(rawPath)
	if !ok || isMetadataPath(normalized) || isStatePath(normalized) {
		return false
	}
	if strings.EqualFold(normalized, PolicyFileName) {
		return true
	}
	include := p.Publish.Include.empty() || p.Publish.Include.matches(normalized)
	return include && !p.Publish.Exclude.matches(normalized)
}

func (p FilePolicy) ShouldPreserve(rawPath string) bool {
	normalized, ok := policyMatchPath(rawPath)
	if !ok || isMetadataPath(normalized) || isStatePath(normalized) || strings.EqualFold(normalized, PolicyFileName) {
		return false
	}
	return p.Preserve.Include.matches(normalized) && !p.Preserve.Exclude.matches(normalized)
}

func sectionFromJSON(section policySection) PolicySection {
	result := PolicySection{}
	if section.Include != nil {
		result.Include = matchSetFromJSON(*section.Include)
	}
	if section.Exclude != nil {
		result.Exclude = matchSetFromJSON(*section.Exclude)
	}
	return result
}

func matchSetFromJSON(set matchSetJSON) MatchSet {
	return MatchSet{
		Directories: set.Directories,
		Files:       set.Files,
		Extensions:  set.Extensions,
		Regex:       set.Regex,
	}
}

func normalizePolicy(policy *FilePolicy) error {
	sets := []*MatchSet{
		&policy.Publish.Include, &policy.Publish.Exclude,
		&policy.Preserve.Include, &policy.Preserve.Exclude,
	}
	for _, set := range sets {
		if err := set.normalize(); err != nil {
			return err
		}
	}
	return nil
}

func (set *MatchSet) normalize() error {
	var err error
	set.Directories, set.directorySet, err = normalizePolicyPaths(set.Directories)
	if err != nil {
		return err
	}
	set.Files, set.fileSet, err = normalizePolicyPaths(set.Files)
	if err != nil {
		return err
	}

	set.extensionSet = make(map[string]struct{}, len(set.Extensions))
	extensions := make([]string, 0, len(set.Extensions))
	for _, extension := range set.Extensions {
		normalized := strings.ToLower(strings.TrimSpace(extension))
		if len(normalized) < 2 || normalized[0] != '.' ||
			strings.Contains(normalized[1:], ".") ||
			strings.ContainsAny(normalized, `/\`) {
			return policyError("policy_invalid_extension", "非法扩展名：%q", extension)
		}
		if _, exists := set.extensionSet[normalized]; !exists {
			set.extensionSet[normalized] = struct{}{}
			extensions = append(extensions, normalized)
		}
	}
	set.Extensions = extensions

	if len(set.Regex) > maxPolicyRegexCount {
		return policyError("policy_invalid_regex", "每组正则最多 %d 条", maxPolicyRegexCount)
	}
	regexSeen := make(map[string]struct{}, len(set.Regex))
	regexValues := make([]string, 0, len(set.Regex))
	set.compiled = make([]*regexp.Regexp, 0, len(set.Regex))
	for _, expression := range set.Regex {
		if expression == "" || len([]byte(expression)) > maxPolicyRegexBytes {
			return policyError("policy_invalid_regex", "正则为空或超过 %d 字节", maxPolicyRegexBytes)
		}
		if _, exists := regexSeen[expression]; exists {
			continue
		}
		compiled, err := regexp.Compile(`(?i)^(?:` + expression + `)$`)
		if err != nil {
			return policyError("policy_invalid_regex", "非法正则 %q：%v", expression, err)
		}
		regexSeen[expression] = struct{}{}
		regexValues = append(regexValues, expression)
		set.compiled = append(set.compiled, compiled)
	}
	set.Regex = regexValues
	return nil
}

func normalizePolicyPaths(values []string) ([]string, map[string]struct{}, error) {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		for _, part := range strings.Split(strings.ReplaceAll(value, `\`, "/"), "/") {
			if part == ".." {
				return nil, nil, policyError("policy_invalid_path", "策略路径不得包含 ..：%q", value)
			}
		}
		normalized, err := NormalizeManifestPath(value)
		if err != nil {
			return nil, nil, policyError("policy_invalid_path", "非法策略路径 %q：%v", value, err)
		}
		key := strings.ToLower(normalized)
		if _, exists := seen[key]; !exists {
			seen[key] = struct{}{}
			result = append(result, normalized)
		}
	}
	return result, seen, nil
}

func (set MatchSet) empty() bool {
	return len(set.Directories) == 0 && len(set.Files) == 0 &&
		len(set.Extensions) == 0 && len(set.Regex) == 0
}

func (set MatchSet) matches(normalized string) bool {
	key := strings.ToLower(normalized)
	if _, exists := set.fileSet[key]; exists {
		return true
	}
	for directory := range set.directorySet {
		if key == directory || strings.HasPrefix(key, directory+"/") {
			return true
		}
	}
	if _, exists := set.extensionSet[strings.ToLower(path.Ext(normalized))]; exists {
		return true
	}
	for _, expression := range set.compiled {
		if expression.MatchString(normalized) {
			return true
		}
	}
	return false
}

func policyMatchPath(rawPath string) (string, bool) {
	normalized, err := NormalizeManifestPath(rawPath)
	return normalized, err == nil
}

func isMetadataPath(normalized string) bool {
	return strings.EqualFold(normalized, "latest.json") || strings.EqualFold(normalized, "manifest.json")
}

func isStatePath(normalized string) bool {
	return strings.EqualFold(normalized, StateDirName) ||
		strings.HasPrefix(strings.ToLower(normalized), strings.ToLower(StateDirName)+"/")
}
