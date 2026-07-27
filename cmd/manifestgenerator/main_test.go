package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"updater/internal/updatercore"
)

func TestRunLoadsFixedRootPolicyAndRejectsMissingOrInvalidPolicy(t *testing.T) {
	for name, testCase := range map[string]struct {
		policyData    []byte
		failureReason string
	}{
		"missing": {failureReason: "policy_download_failed"},
		"invalid": {
			policyData:    []byte(`{"schema_version":1} {}`),
			failureReason: "policy_invalid_json",
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if testCase.policyData != nil {
				if err := os.WriteFile(filepath.Join(root, updatercore.PolicyFileName), testCase.policyData, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			var stdout, stderr bytes.Buffer
			code := run([]string{"--dir", root, "--version", "1", "--output", filepath.Join(root, "manifest.json")}, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("exit code = %d, stderr=%s", code, stderr.String())
			}
			if bytes.Count(stderr.Bytes(), []byte("policy_load_failed")) != 1 {
				t.Fatalf("policy_load_failed must occur exactly once: %s", stderr.String())
			}
			for _, field := range []string{
				"source=local",
				"policy_file=" + updatercore.PolicyFileName,
				"failure_reason=" + testCase.failureReason,
			} {
				if !bytes.Contains(stderr.Bytes(), []byte(field)) {
					t.Fatalf("missing %q in policy failure event: %s", field, stderr.String())
				}
			}
			if bytes.Contains(stdout.Bytes(), []byte("policy_load_success")) ||
				bytes.Contains(stdout.Bytes(), []byte("policy_load_failed")) ||
				bytes.Contains(stderr.Bytes(), []byte("policy_load_success")) {
				t.Fatalf("success/failure events are not mutually exclusive: stdout=%s stderr=%s", stdout.String(), stderr.String())
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
	if bytes.Count(stdout.Bytes(), []byte("policy_load_success")) != 1 {
		t.Fatalf("policy_load_success must occur exactly once: %s", stdout.String())
	}
	for _, field := range []string{
		"source=local",
		"schema_version=1",
		"policy_file=" + updatercore.PolicyFileName,
	} {
		if !bytes.Contains(stdout.Bytes(), []byte(field)) {
			t.Fatalf("missing %q in policy success event: %s", field, stdout.String())
		}
	}
	if bytes.Contains(stdout.Bytes(), []byte("policy_load_failed")) ||
		bytes.Contains(stderr.Bytes(), []byte("policy_load_success")) ||
		bytes.Contains(stderr.Bytes(), []byte("policy_load_failed")) {
		t.Fatalf("success/failure events are not mutually exclusive: stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
	if !bytes.Contains(stdout.Bytes(), []byte("policy_manifest_summary")) ||
		!bytes.Contains(stdout.Bytes(), []byte("scanned_count=3 included_count=2 excluded_count=1")) {
		t.Fatalf("missing summary: %s", stdout.String())
	}
}
