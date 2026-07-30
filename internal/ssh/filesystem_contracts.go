package ssh

import (
	"context"
	"errors"
	"strings"
)

type ViewKind string

const (
	ViewKindFile      ViewKind = "file"
	ViewKindDirectory ViewKind = "directory"
	ViewKindImage     ViewKind = "image"

	// MaxFileTransferBytes bounds raw copy payloads before JSON/base64 amplification.
	MaxFileTransferBytes = 16 * 1024 * 1024
	// MaxViewBytes bounds non-forced file and directory view content.
	MaxViewBytes = 20 * 1024
	// MaxDirectoryEntries bounds a directory view across its entire traversal.
	MaxDirectoryEntries = 1000
	// MaxGrepOutputBytes bounds grep output independently of its line limit.
	MaxGrepOutputBytes = 1024 * 1024
	// MaxGrepInputBytes bounds each file read by the direct-SSH grep fallback.
	MaxGrepInputBytes = MaxFileTransferBytes
)

type GrepOutputMode string

const (
	GrepOutputModeContent          GrepOutputMode = "content"
	GrepOutputModeFilesWithMatches GrepOutputMode = "files_with_matches"
	GrepOutputModeCount            GrepOutputMode = "count"

	// DefaultGlobLimit bounds legacy callers that do not provide a limit.
	DefaultGlobLimit = 1000
	// MaxGlobLimit prevents callers from requesting an unbounded result.
	MaxGlobLimit = 10000
)

var (
	ErrFilesystemContractUnsupported = errors.New("ssh: structured filesystem contract not supported by executor")
	ErrMultiplePathsUnsupported      = errors.New("ssh: multiple paths require a structured executor")
	ErrApplyPatchUnsupported         = errors.New("ssh: apply_patch not supported by executor")
	ErrFileTransferTooLarge          = errors.New("ssh: file transfer exceeds safe maximum")
)

type ViewRequest struct {
	Path                string `json:"path"`
	ViewRange           []int  `json:"view_range,omitempty"`
	ForceReadLargeFiles bool   `json:"forceReadLargeFiles,omitempty"`
}

func (r ViewRequest) Normalize() ViewRequest {
	out := r
	if len(out.ViewRange) == 2 {
		out.ViewRange = append([]int(nil), out.ViewRange...)
	} else {
		out.ViewRange = nil
	}
	return out
}

func (r ViewRequest) LegacyViewRange() []int {
	return r.Normalize().ViewRange
}

type ViewResult struct {
	Kind       ViewKind `json:"kind,omitempty"`
	Content    string   `json:"content,omitempty"`
	Entries    []string `json:"entries,omitempty"`
	Truncated  bool     `json:"truncated,omitempty"`
	Limit      int      `json:"limit,omitempty"`
	ByteLimit  int      `json:"byte_limit,omitempty"`
	MimeType   string   `json:"mime_type,omitempty"`
	Base64Data string   `json:"base64_data,omitempty"`
}

type GrepRequest struct {
	Pattern         string         `json:"pattern"`
	Path            string         `json:"path,omitempty"`
	Paths           []string       `json:"paths,omitempty"`
	Glob            string         `json:"glob,omitempty"`
	OutputMode      GrepOutputMode `json:"output_mode,omitempty"`
	Type            string         `json:"type,omitempty"`
	CaseInsensitive bool           `json:"-i,omitempty"`
	AfterContext    int            `json:"-A,omitempty"`
	BeforeContext   int            `json:"-B,omitempty"`
	Context         int            `json:"-C,omitempty"`
	LineNumbers     *bool          `json:"-n,omitempty"`
	HeadLimit       int            `json:"head_limit,omitempty"`
	Multiline       bool           `json:"multiline,omitempty"`
	Cwd             string         `json:"cwd,omitempty"`
}

func (r GrepRequest) Normalize() GrepRequest {
	out := r
	out.Paths = normalizeSearchPaths(r.Path, r.Paths)
	if out.Path == "" && len(out.Paths) == 1 {
		out.Path = out.Paths[0]
	}
	if out.OutputMode == "" {
		out.OutputMode = GrepOutputModeContent
	}
	if out.LineNumbers != nil {
		value := *out.LineNumbers
		out.LineNumbers = &value
	} else if out.OutputMode == GrepOutputModeContent {
		out.LineNumbers = boolPtr(true)
	}
	return out
}

func (r GrepRequest) EffectivePaths() []string {
	return normalizeSearchPaths(r.Path, r.Paths)
}

func (r GrepRequest) LegacyPath() (string, error) {
	out := r.Normalize()
	if len(out.Paths) > 1 {
		return "", ErrMultiplePathsUnsupported
	}
	return out.Path, nil
}

type GrepResult struct {
	Output         string         `json:"output,omitempty"`
	OutputMode     GrepOutputMode `json:"output_mode,omitempty"`
	Paths          []string       `json:"paths,omitempty"`
	Truncated      bool           `json:"truncated,omitempty"`
	Limit          int            `json:"limit,omitempty"`
	ByteLimit      int            `json:"byte_limit,omitempty"`
	SkippedFiles   int            `json:"skipped_files,omitempty"`
	InputByteLimit int            `json:"input_byte_limit,omitempty"`
}

type GlobRequest struct {
	Pattern string   `json:"pattern"`
	Path    string   `json:"path,omitempty"`
	Paths   []string `json:"paths,omitempty"`
	Cwd     string   `json:"cwd,omitempty"`
	Limit   int      `json:"limit,omitempty"`
}

func (r GlobRequest) Normalize() GlobRequest {
	out := r
	out.Paths = normalizeSearchPaths(r.Path, r.Paths)
	if out.Path == "" && len(out.Paths) == 1 {
		out.Path = out.Paths[0]
	}
	switch {
	case out.Limit <= 0:
		out.Limit = DefaultGlobLimit
	case out.Limit > MaxGlobLimit:
		out.Limit = MaxGlobLimit
	}
	return out
}

func (r GlobRequest) EffectivePaths() []string {
	return normalizeSearchPaths(r.Path, r.Paths)
}

func (r GlobRequest) LegacyPath() (string, error) {
	out := r.Normalize()
	if len(out.Paths) > 1 {
		return "", ErrMultiplePathsUnsupported
	}
	return out.Path, nil
}

type GlobResult struct {
	Output    string   `json:"output,omitempty"`
	Paths     []string `json:"paths,omitempty"`
	Truncated bool     `json:"truncated,omitempty"`
	Limit     int      `json:"limit,omitempty"`
}

type ApplyPatchRequest struct {
	Patch string `json:"patch"`
	Cwd   string `json:"cwd,omitempty"`
}

type ApplyPatchResult struct {
	Output       string `json:"output,omitempty"`
	FilesChanged int    `json:"files_changed,omitempty"`
}

type RootedReadRequest struct {
	Path string `json:"path"`
	Root string `json:"root"`
}

type RootedWriteRequest struct {
	Path      string `json:"path"`
	Root      string `json:"root"`
	Data      []byte `json:"data"`
	Overwrite bool   `json:"overwrite"`
}

type ViewExecutor interface {
	View(context.Context, ViewRequest) (ViewResult, error)
}

type GrepExecutor interface {
	GrepFiles(context.Context, GrepRequest) (GrepResult, error)
}

type GlobExecutor interface {
	GlobFiles(context.Context, GlobRequest) (GlobResult, error)
}

type FilesystemExecutor interface {
	ViewExecutor
	GrepExecutor
	GlobExecutor
}

type ApplyPatchExecutor interface {
	ApplyPatch(context.Context, ApplyPatchRequest) (ApplyPatchResult, error)
}

type RootedFileExecutor interface {
	ReadFileRooted(context.Context, RootedReadRequest) ([]byte, error)
	WriteFileRooted(context.Context, RootedWriteRequest) error
}

type legacyViewExecutor interface {
	ViewFile(context.Context, string, []int) (string, error)
}

type legacyGrepExecutor interface {
	Grep(context.Context, string, string, string, string) (string, error)
}

type legacyGlobExecutor interface {
	Glob(context.Context, string, string, string) (string, error)
}

func ExecuteView(ctx context.Context, exec any, req ViewRequest) (ViewResult, error) {
	req = req.Normalize()
	if structured, ok := exec.(ViewExecutor); ok {
		return structured.View(ctx, req)
	}
	legacy, ok := exec.(legacyViewExecutor)
	if !ok {
		return ViewResult{}, ErrFilesystemContractUnsupported
	}
	content, err := legacy.ViewFile(ctx, req.Path, req.LegacyViewRange())
	return ViewResult{
		Kind:    ViewKindFile,
		Content: content,
	}, err
}

func ExecuteGrep(ctx context.Context, exec any, req GrepRequest) (GrepResult, error) {
	req = req.Normalize()
	if structured, ok := exec.(GrepExecutor); ok {
		return structured.GrepFiles(ctx, req)
	}
	legacy, ok := exec.(legacyGrepExecutor)
	if !ok {
		return GrepResult{}, ErrFilesystemContractUnsupported
	}
	path, err := req.LegacyPath()
	if err != nil {
		return GrepResult{}, err
	}
	output, err := legacy.Grep(ctx, req.Pattern, path, req.Glob, req.Cwd)
	return GrepResult{
		Output:     output,
		OutputMode: req.OutputMode,
		Paths:      req.EffectivePaths(),
	}, err
}

func ExecuteGlob(ctx context.Context, exec any, req GlobRequest) (GlobResult, error) {
	req = req.Normalize()
	if structured, ok := exec.(GlobExecutor); ok {
		result, err := structured.GlobFiles(ctx, req)
		return boundGlobResult(result, req), err
	}
	legacy, ok := exec.(legacyGlobExecutor)
	if !ok {
		return GlobResult{}, ErrFilesystemContractUnsupported
	}
	path, err := req.LegacyPath()
	if err != nil {
		return GlobResult{}, err
	}
	output, err := legacy.Glob(ctx, req.Pattern, path, req.Cwd)
	return boundGlobResult(GlobResult{
		Output: output,
		Paths:  req.EffectivePaths(),
	}, req), err
}

func boundGlobResult(result GlobResult, req GlobRequest) GlobResult {
	req = req.Normalize()
	if len(result.Paths) == 0 {
		result.Paths = req.EffectivePaths()
	}
	result.Limit = req.Limit
	if result.Output == "" {
		return result
	}

	hasTrailingNewline := strings.HasSuffix(result.Output, "\n")
	lines := strings.Split(strings.TrimSuffix(result.Output, "\n"), "\n")
	if len(lines) <= req.Limit {
		return result
	}
	lines = lines[:req.Limit]
	result.Output = strings.Join(lines, "\n")
	if hasTrailingNewline {
		result.Output += "\n"
	}
	result.Truncated = true
	return result
}

func ExecuteApplyPatch(ctx context.Context, exec any, req ApplyPatchRequest) (ApplyPatchResult, error) {
	structured, ok := exec.(ApplyPatchExecutor)
	if !ok {
		return ApplyPatchResult{}, ErrApplyPatchUnsupported
	}
	return structured.ApplyPatch(ctx, req)
}

func normalizeSearchPaths(path string, paths []string) []string {
	switch {
	case len(paths) > 0:
		return append([]string(nil), paths...)
	case path != "":
		return []string{path}
	default:
		return []string{"."}
	}
}

func boolPtr(value bool) *bool {
	return &value
}
