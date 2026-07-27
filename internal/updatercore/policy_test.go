package updatercore

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestFilePolicyMatchesNormalizedRulesAndExcludeWins(t *testing.T) {
	policy, err := ParseFilePolicy([]byte(`{
		"schema_version": 1,
		"publish": {
			"include": {
				"directories": ["Apps\\Config"],
				"files": ["Updater.exe"],
				"extensions": [".exe"],
				"regex": ["logs/trace-[0-9]+\\.txt"]
			},
			"exclude": {"files": ["apps/config/secret.exe"]}
		},
		"preserve": {
			"include": {"extensions": [".ini"]},
			"exclude": {"files": ["system.ini"]}
		}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]bool{
		"apps/config/settings.json": true,
		"APPS\\CONFIG\\tool.EXE":    true,
		"apps/config-old/tool.exe":  true, // included by .exe, not by directory
		"apps/config/secret.exe":    false,
		"Updater.exe":               true,
		"logs/TRACE-12.txt":         true,
		"xlogs/trace-12.txt":        false,
	}
	for path, want := range cases {
		if got := policy.ShouldPublish(path); got != want {
			t.Errorf("ShouldPublish(%q) = %v, want %v", path, got, want)
		}
	}
	if !policy.ShouldPreserve("profiles/user.INI") {
		t.Error("expected .ini to be preserved case-insensitively")
	}
	if policy.ShouldPreserve("SYSTEM.ini") {
		t.Error("preserve exclude must win")
	}
}

func TestLoadFilePolicyRejectsLocalFileOverLimit(t *testing.T) {
	fileName := filepath.Join(t.TempDir(), PolicyFileName)
	if err := os.WriteFile(fileName, make([]byte, MaxPolicySize+1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadFilePolicy(fileName); err == nil || PolicyFailureReason(err) != "policy_too_large" {
		t.Fatalf("expected policy_too_large, got %v", err)
	}
}

func TestParseFilePolicyStrictValidation(t *testing.T) {
	tests := map[string]string{
		"missing schema":        `{}`,
		"null schema":           `{"schema_version":null}`,
		"zero schema":           `{"schema_version":0}`,
		"unsupported schema":    `{"schema_version":2}`,
		"unknown field":         `{"schema_version":1,"future":true}`,
		"trailing JSON":         `{"schema_version":1} {}`,
		"absolute path":         `{"schema_version":1,"publish":{"include":{"files":["C:\\bad"]}}}`,
		"parent path":           `{"schema_version":1,"publish":{"include":{"files":["../bad"]}}}`,
		"embedded parent path":  `{"schema_version":1,"publish":{"include":{"files":["a/../bad"]}}}`,
		"empty path":            `{"schema_version":1,"publish":{"include":{"files":[" "]}}}`,
		"bad extension":         `{"schema_version":1,"publish":{"include":{"extensions":[".tar.gz"]}}}`,
		"bad regex":             `{"schema_version":1,"publish":{"include":{"regex":["("]}}}`,
		"too long regex":        `{"schema_version":1,"publish":{"include":{"regex":["` + strings.Repeat("a", 513) + `"]}}}`,
		"wrong collection type": `{"schema_version":1,"publish":{"include":{"files":"a"}}}`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseFilePolicy([]byte(input)); err == nil {
				t.Fatal("expected policy validation error")
			}
		})
	}
	regexes := make([]string, 65)
	for i := range regexes {
		regexes[i] = "x"
	}
	input := `{"schema_version":1,"publish":{"include":{"regex":["` +
		strings.Join(regexes, `","`) + `"]}}}`
	if _, err := ParseFilePolicy([]byte(input)); err == nil {
		t.Fatal("expected regex count error")
	}
}

func TestDefaultFilePolicyIsCompatibleAndConcurrentReadSafe(t *testing.T) {
	policy := DefaultFilePolicy()
	if len(policy.Preserve.Include.Files) != 6 || len(policy.Preserve.Include.Extensions) != 10 {
		t.Fatalf("default compatibility rules = %d files/%d extensions, want 6/10",
			len(policy.Preserve.Include.Files), len(policy.Preserve.Include.Extensions))
	}
	for _, path := range []string{
		"AimanServer/webgemini_server_v2/config/config.json",
		"U-Geminiserver_core/lan.txt",
		"U-Geminiserver_core/static/hanzipinyin.json",
		"U-Geminiserver_core/static/rare_chars.json",
		"U-Geminiserver_core/static/units.json",
		"U-Geminiserver_core/static/voices.json",
		"any/file.ini", "any/file.json", "any/file.txt", "any/file.log",
		"any/file.yaml", "any/file.yml", "any/file.toml", "any/file.cfg",
		"any/file.conf", "any/file.config",
	} {
		if !policy.ShouldPreserve(path) {
			t.Errorf("default policy should preserve %q", path)
		}
	}
	for _, path := range []string{"latest.json", "manifest.json", "updater_state/x", "updater-policy.json"} {
		if policy.ShouldPreserve(path) {
			t.Errorf("system invariant should not preserve %q", path)
		}
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				if !policy.ShouldPreserve("x.ini") || !policy.ShouldPublish("app.exe") {
					t.Errorf("concurrent read returned inconsistent result")
					return
				}
			}
		}()
	}
	wg.Wait()
}
