package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"

	"updater/internal/updatercore"
)

func TestReportResultOrdinaryFailureUsesExitOneAndStderrFallback(t *testing.T) {
	var stderr bytes.Buffer
	err := errors.New("latest.json 请求失败")

	if got := reportResult(err, &stderr); got != 1 {
		t.Fatalf("reportResult exit code = %d, want 1", got)
	}
	output := stderr.String()
	for _, want := range []string{"更新失败：latest.json 请求失败", updatercore.FullPackageName, updatercore.FullPackageURL, updatercore.FullPackageExtractCode, updatercore.FullPackageSuggestion} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr %q does not contain %q", output, want)
		}
	}
}

func TestReportResultPartialUsesExitThreeAndPrintsSummaryWithoutUIInfo(t *testing.T) {
	var stderr bytes.Buffer
	partial := fmt.Errorf("Stage 已完成：%w", updatercore.NewUpdatePartialError([]updatercore.SkippedFile{
		{Path: "uploads/example.wav", Error: "大小或 SHA-256 采样哈希不匹配"},
	}))

	if got := reportResult(partial, &stderr); got != 3 {
		t.Fatalf("reportResult exit code = %d, want 3", got)
	}
	output := stderr.String()
	for _, want := range []string{"uploads/example.wav", "大小或 SHA-256 采样哈希不匹配", updatercore.FullPackageURL, updatercore.FullPackageSuggestion} {
		if !strings.Contains(output, want) {
			t.Fatalf("stderr %q does not contain %q", output, want)
		}
	}
}

func TestReportResultKeepsZeroAndCancelResultsWithoutFallback(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		code int
	}{
		{name: "nil", code: 0},
		{name: "no update", err: updatercore.ErrNoUpdate, code: 0},
		{name: "self update", err: updatercore.ErrSelfUpdateHandoff, code: 0},
		{name: "cancel", err: updatercore.ErrUserCancelled, code: 2},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stderr bytes.Buffer
			if got := reportResult(tt.err, &stderr); got != tt.code {
				t.Fatalf("reportResult exit code = %d, want %d", got, tt.code)
			}
			if strings.Contains(stderr.String(), updatercore.FullPackageURL) {
				t.Fatalf("unexpected fallback notice in stderr: %q", stderr.String())
			}
		})
	}
}
