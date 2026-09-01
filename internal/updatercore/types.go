package updatercore

import (
	"errors"
	"net/http"
)

const (
	ProgramVersion = "u-gemini-updater/1.0.0"
	StateDirName   = "updater_state"

	sampleThreshold = int64(2 * 1024 * 1024)
	sampleSize      = int64(1024 * 1024)
)

var (
	ErrNoUpdate          = errors.New("已是最新版本，无需更新")
	ErrUpdatePartial     = errors.New("update partially completed")
	ErrUserCancelled     = errors.New("user cancelled")
	ErrSelfUpdateHandoff = errors.New("self update handoff started")
	ErrMissingConfig     = errors.New("missing updater config")
)

type Config struct {
	LatestURL string `json:"latest_url"`
}

type LatestInfo struct {
	Version      string `json:"version"`
	ManifestURL  string `json:"manifest_url"`
	FilesBaseURL string `json:"files_base_url"`
}

type FileEntry struct {
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	HeadHash string `json:"head_hash"`
	TailHash string `json:"tail_hash"`
}

type Manifest struct {
	Version string      `json:"version"`
	Files   []FileEntry `json:"files"`
}

type VersionState struct {
	Version string `json:"version"`
}

type Options struct {
	RootDir      string
	Silent       bool
	Debug        bool
	AutoConfirm  bool
	Workers      int
	Client       *http.Client
	UI           UI
	StateDir     string
	KeepStateDir bool

	CompleteSelfUpdate bool
	SkipSelfUpdate     bool
	SelfUpdateTarget   string
	SelfUpdatePending  string
	ExePath            string
}

type Plan struct {
	CurrentVersion       string
	LatestVersion        string
	Add                  []FileEntry
	Modify               []FileEntry
	Delete               []FileEntry
	DownloadSize         int64
	FirstInstallRecovery bool
	RemoteManifest       Manifest
}

type Session struct {
	Phase             string      `json:"phase"`
	TargetVersion     string      `json:"target_version"`
	Add               []string    `json:"add"`
	Modify            []string    `json:"modify"`
	Delete            []string    `json:"delete"`
	BackedUp          []MovedFile `json:"backed_up"`
	Switched          []MovedFile `json:"switched"`
	PendingSelfUpdate string      `json:"pending_self_update,omitempty"`
	StartedAt         string      `json:"started_at"`
}

type MovedFile struct {
	Path       string `json:"path"`
	BackupPath string `json:"backup_path,omitempty"`
}

type LockedFile struct {
	Path        string
	ProcessName string
	PID         int
}

type ProgressEvent struct {
	Phase          string
	CurrentFile    string
	CompletedFiles int
	TotalFiles     int
	BytesDone      int64
	BytesTotal     int64
	SpeedBytes     float64
}

// FileFailure describes a file that did not pass the existing download and
// verification attempts. Path is the normalized relative path shown to the
// user; Error is the user-readable reason for the failure.
type FileFailure struct {
	Path      string
	Error     string
	Important bool
}

// FileFailureDecision is the action selected for a failed file.
type FileFailureDecision string

const (
	FileFailureDecisionRetry   FileFailureDecision = "retry"
	FileFailureDecisionSkip    FileFailureDecision = "skip"
	FileFailureDecisionSkipAll FileFailureDecision = "skip_all"
	FileFailureDecisionCancel  FileFailureDecision = "cancel"

	// Short names keep the contract convenient for callers that already have
	// the FileFailureDecision type in scope.
	FileFailureRetry   = FileFailureDecisionRetry
	FileFailureSkip    = FileFailureDecisionSkip
	FileFailureSkipAll = FileFailureDecisionSkipAll
	FileFailureCancel  = FileFailureDecisionCancel
)

// SkippedFile records a failed file that the user elected not to install in
// the current run.
type SkippedFile struct {
	Path  string
	Error string
}

// StageResult contains the plan that may actually be applied and the files
// deliberately left for a later run.
type StageResult struct {
	EffectivePlan Plan
	Skipped       []SkippedFile
}
