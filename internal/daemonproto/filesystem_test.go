package daemonproto

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/ssh"
)

func TestViewFileParamsRoundTripPreservesExtendedFields(t *testing.T) {
	want, err := NewRequest(12, VerbViewFile, ViewFileParams{
		Path:                "/workspaces/repo/main.go",
		ViewRange:           []int{5, -1},
		ForceReadLargeFiles: true,
	}, "")
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	var params ViewFileParams
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if params.Path != "/workspaces/repo/main.go" || !params.ForceReadLargeFiles {
		t.Fatalf("params = %+v", params)
	}
}

func TestViewFileResultRoundTripPreservesStructuredFields(t *testing.T) {
	want, err := NewResponse(13, ViewFileResult{
		Kind:       ssh.ViewKindDirectory,
		Content:    "internal/\n",
		Entries:    []string{"internal/ssh/client.go"},
		Truncated:  true,
		MimeType:   "text/plain",
		Base64Data: "aW50ZXJuYWwvCg==",
	})
	if err != nil {
		t.Fatalf("NewResponse() error = %v", err)
	}

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}

	var result ViewFileResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if result.Kind != ssh.ViewKindDirectory || !result.Truncated || result.MimeType != "text/plain" {
		t.Fatalf("result = %+v", result)
	}
}

func TestApplyPatchRoundTrip(t *testing.T) {
	want, err := NewRequest(14, VerbApplyPatch, ApplyPatchParams{
		Patch: "*** Begin Patch\n*** End Patch\n",
		Cwd:   "/workspaces/repo",
	}, "patch-1")
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	if got.Verb != VerbApplyPatch {
		t.Fatalf("Verb = %q, want %q", got.Verb, VerbApplyPatch)
	}

	var params ApplyPatchParams
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if params.Cwd != "/workspaces/repo" || params.Patch == "" {
		t.Fatalf("params = %+v", params)
	}
}

func TestWriteFileParamsRoundTripPreservesBinaryBytes(t *testing.T) {
	content := []byte{0x00, 0xff, 'x'}
	want, err := NewRequest(15, VerbWriteFile, WriteFileParams{
		Path:      "/workspaces/repo/blob.bin",
		Data:      content,
		Overwrite: true,
		Root:      "/workspaces/repo",
	}, "write-1")
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read() error = %v", err)
	}
	var params WriteFileParams
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if params.Path != "/workspaces/repo/blob.bin" || !params.Overwrite || params.Root != "/workspaces/repo" {
		t.Fatalf("params = %+v", params)
	}
	if !bytes.Equal(params.Data, content) {
		t.Fatalf("data = %v, want %v", params.Data, content)
	}
}

func TestReadFileParamsAndResultRoundTripPreserveRootedBinaryBytes(t *testing.T) {
	content := []byte{0x00, 0xff, 'x'}
	request, err := NewRequest(16, VerbReadFile, ReadFileParams{
		Path: "/workspaces/repo/blob.bin",
		Root: "/workspaces/repo",
	}, "")
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	var requestBuffer bytes.Buffer
	if err := NewEncoder(&requestBuffer).Write(request); err != nil {
		t.Fatalf("Write(request) error = %v", err)
	}
	decodedRequest, err := NewDecoder(&requestBuffer).Read()
	if err != nil {
		t.Fatalf("Read(request) error = %v", err)
	}
	var params ReadFileParams
	if err := json.Unmarshal(decodedRequest.Params, &params); err != nil {
		t.Fatalf("Unmarshal(params) error = %v", err)
	}
	if params.Path != "/workspaces/repo/blob.bin" || params.Root != "/workspaces/repo" {
		t.Fatalf("params = %+v", params)
	}

	response, err := NewResponse(16, ReadFileResult{Data: content})
	if err != nil {
		t.Fatalf("NewResponse() error = %v", err)
	}
	var responseBuffer bytes.Buffer
	if err := NewEncoder(&responseBuffer).Write(response); err != nil {
		t.Fatalf("Write(response) error = %v", err)
	}
	decodedResponse, err := NewDecoder(&responseBuffer).Read()
	if err != nil {
		t.Fatalf("Read(response) error = %v", err)
	}
	var result ReadFileResult
	if err := json.Unmarshal(decodedResponse.Result, &result); err != nil {
		t.Fatalf("Unmarshal(result) error = %v", err)
	}
	if !bytes.Equal(result.Data, content) {
		t.Fatalf("data = %v, want %v", result.Data, content)
	}
}

func TestVerbEnumerationAdvertisesImplementedFilesystemVerbs(t *testing.T) {
	for _, want := range []Verb{VerbApplyPatch, VerbReadFile, VerbWriteFile} {
		if !containsVerb(AllVerbs(), want) {
			t.Fatalf("AllVerbs() missing %q", want)
		}
		if !containsVerb(AllDefinedVerbs(), want) {
			t.Fatalf("AllDefinedVerbs() missing %q", want)
		}
	}
	for _, want := range []Verb{VerbViewFile, VerbReadFile, VerbWriteFile, VerbGrep, VerbGlob, VerbApplyPatch} {
		if !containsVerb(FilesystemVerbs(), want) {
			t.Fatalf("FilesystemVerbs() missing %q", want)
		}
	}
}

func TestVerbApplyPatchIsMutating(t *testing.T) {
	if !VerbApplyPatch.IsMutating() {
		t.Fatalf("%q should be mutating", VerbApplyPatch)
	}
	if !VerbWriteFile.IsMutating() {
		t.Fatalf("%q should be mutating", VerbWriteFile)
	}
}

func containsVerb(verbs []Verb, want Verb) bool {
	for _, got := range verbs {
		if got == want {
			return true
		}
	}
	return false
}
