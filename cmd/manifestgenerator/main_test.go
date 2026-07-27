package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"updater/internal/updatercore"
)

func TestRunLoadsFixedRootPolicyAndRejectsMissingOrInvalidPolicy(t *testing.T) {
	for name, policyData := range map[string][]byte{
		"missing": nil,
		"invalid": []byte(`{"schema_version":1} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if policyData != nil {
				if err := os.WriteFile(filepath.Join(root, updatercore.PolicyFileName), policyData, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"--dir", root, "--version", "1", "--output", filepath.Join(root, "manifest.json")}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
			}
			if !bytes.Contains(stderr.Bytes(), []byte("policy_load_failed")) ||
				!bytes.Contains(stderr.Bytes(), []byte("failure_reason=")) {
				t.Fatalf("missing policy failure event: %s", stderr.String())
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
	if !bytes.Contains(stdout.Bytes(), []byte("policy_manifest_summary")) ||
		!bytes.Contains(stdout.Bytes(), []byte("scanned_count=3 included_count=2 excluded_count=1")) {
		t.Fatalf("missing summary: %s", stdout.String())
	}
}
