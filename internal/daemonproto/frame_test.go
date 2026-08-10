package daemonproto

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRequestRoundTrip(t *testing.T) {
	want, err := NewRequest(42, VerbViewFile, ViewFileParams{Path: "/etc/hosts", ViewRange: []int{1, 50}}, "key-abc")
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	if got.Type != TypeRequest || got.ID != 42 || got.Verb != VerbViewFile || got.IdempotencyKey != "key-abc" {
		t.Fatalf("decoded frame = %#v, want type=req id=42 verb=view_file key=key-abc", got)
	}

	var params ViewFileParams
	if err := json.Unmarshal(got.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}
	if params.Path != "/etc/hosts" || len(params.ViewRange) != 2 || params.ViewRange[0] != 1 || params.ViewRange[1] != 50 {
		t.Fatalf("params = %+v, want path=/etc/hosts range=[1,50]", params)
	}
}

func TestResponseRoundTrip(t *testing.T) {
	want, err := NewResponse(7, RunBashResult{Stdout: "hello\n", Stderr: "", ExitCode: 0})
	if err != nil {
		t.Fatalf("NewResponse: %v", err)
	}

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(want); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Type != TypeResponse || got.ID != 7 || got.Error != nil {
		t.Fatalf("decoded frame = %#v, want successful response", got)
	}

	var result RunBashResult
	if err := json.Unmarshal(got.Result, &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if result.Stdout != "hello\n" || result.ExitCode != 0 {
		t.Fatalf("result = %+v, want stdout=hello\\n exit=0", result)
	}
}

func TestErrorResponseRoundTrip(t *testing.T) {
	in := NewErrorResponse(11, ErrCodeBadRequest, "missing path")

	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Type != TypeResponse || got.ID != 11 {
		t.Fatalf("decoded frame type/id = %s/%d, want resp/11", got.Type, got.ID)
	}
	if got.Error == nil || got.Error.Code != ErrCodeBadRequest || got.Error.Message != "missing path" {
		t.Fatalf("error payload = %+v, want BAD_REQUEST/missing path", got.Error)
	}
	if len(got.Result) != 0 {
		t.Fatalf("error response carries result = %q, want empty", string(got.Result))
	}
}

func TestCancelRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(NewCancel(99)); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Type != TypeCancel || got.ID != 99 {
		t.Fatalf("decoded frame = %#v, want cancel id=99", got)
	}
}

func TestHelloRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(NewHello(ProtocolVersion, AllVerbs())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	got, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Type != TypeHello || got.Version != ProtocolVersion {
		t.Fatalf("decoded hello = %#v, want type=hello version=%s", got, ProtocolVersion)
	}
	if len(got.Verbs) != len(AllVerbs()) {
		t.Fatalf("verbs len = %d, want %d", len(got.Verbs), len(AllVerbs()))
	}
}

func TestProtocolVersionIncludesFilesystemSafetyContract(t *testing.T) {
	if ProtocolVersion != "2" {
		t.Fatalf("ProtocolVersion = %q, want 2", ProtocolVersion)
	}
}

func TestDecodeEOFUnwrapped(t *testing.T) {
	_, err := NewDecoder(strings.NewReader("")).Read()
	if !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF on empty stream, got %v", err)
	}
}

func TestLargeFrameSurvivesScannerLimit(t *testing.T) {
	// Default bufio.Scanner limit is 64 KiB. Verify we're well past that and
	// that the decoder still handles it without truncation. File reads and
	// command outputs can easily exceed this.
	const payloadBytes = 4 * 1024 * 1024 // 4 MiB
	huge := strings.Repeat("a", payloadBytes)

	in, err := NewResponse(1, ViewFileResult{Content: huge})
	if err != nil {
		t.Fatalf("NewResponse: %v", err)
	}
	var buf bytes.Buffer
	if err := NewEncoder(&buf).Write(in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if buf.Len() < payloadBytes {
		t.Fatalf("encoded buffer length = %d, want >= payload (%d)", buf.Len(), payloadBytes)
	}

	out, err := NewDecoder(&buf).Read()
	if err != nil {
		t.Fatalf("Read large frame: %v", err)
	}
	var result ViewFileResult
	if err := json.Unmarshal(out.Result, &result); err != nil {
		t.Fatalf("unmarshal large result: %v", err)
	}
	if len(result.Content) != payloadBytes {
		t.Fatalf("decoded content len = %d, want %d", len(result.Content), payloadBytes)
	}
}

func TestVerbIsMutating(t *testing.T) {
	mutating := map[Verb]bool{
		VerbEditFile:            true,
		VerbCreateFile:          true,
		VerbRunBash:             true,
		VerbStartSession:        true,
		VerbStartProcessSession: true,
		VerbWriteSession:        true,
		VerbStopSession:         true,
		VerbViewFile:            false,
		VerbGrep:                false,
		VerbGlob:                false,
		VerbReadSession:         false,
		VerbListSessions:        false,
		VerbPing:                false,
	}
	for v, want := range mutating {
		if got := v.IsMutating(); got != want {
			t.Errorf("Verb(%s).IsMutating() = %v, want %v", v, got, want)
		}
	}
}

func TestAllVerbsContainsEveryDefinedVerb(t *testing.T) {
	got := AllVerbs()
	seen := make(map[Verb]bool, len(got))
	for _, v := range got {
		if seen[v] {
			t.Errorf("AllVerbs contains duplicate: %s", v)
		}
		seen[v] = true
	}
	for _, want := range []Verb{
		VerbViewFile, VerbReadFile, VerbEditFile, VerbCreateFile, VerbWriteFile, VerbRunBash, VerbGrep,
		VerbGlob, VerbApplyPatch, VerbStartSession, VerbStartProcessSession, VerbWriteSession, VerbReadSession,
		VerbStopSession, VerbListSessions, VerbPing,
	} {
		if !seen[want] {
			t.Errorf("AllVerbs missing %s", want)
		}
	}
}

func TestMultipleFramesInStream(t *testing.T) {
	var buf bytes.Buffer
	enc := NewEncoder(&buf)
	for i := uint64(1); i <= 5; i++ {
		f, err := NewRequest(i, VerbPing, PingParams{}, "")
		if err != nil {
			t.Fatalf("NewRequest: %v", err)
		}
		if err := enc.Write(f); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	dec := NewDecoder(&buf)
	for i := uint64(1); i <= 5; i++ {
		got, err := dec.Read()
		if err != nil {
			t.Fatalf("Read frame %d: %v", i, err)
		}
		if got.ID != i || got.Verb != VerbPing {
			t.Fatalf("frame %d = %+v, want id=%d verb=ping", i, got, i)
		}
	}
	if _, err := dec.Read(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected EOF after 5 frames, got %v", err)
	}
}
