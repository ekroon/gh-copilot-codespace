package helperinfo

import (
	"runtime/debug"
	"testing"
)

func TestValidateRequiresCurrentFilesystemCapability(t *testing.T) {
	info := Info{
		SchemaVersion:      SchemaVersion,
		Version:            "test",
		DaemonProtocol:     DaemonProtocolVersion,
		FilesystemProtocol: FilesystemProtocolVersion,
		Capabilities:       []string{CapabilityDaemon, CapabilityFilesystem},
	}
	if err := Validate(info); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}

	info.Capabilities = []string{CapabilityDaemon}
	if err := Validate(info); err == nil {
		t.Fatal("Validate() error = nil, want missing filesystem capability")
	}
}

func TestReleaseTagFromBuildInfoRequiresExactBuildIdentity(t *testing.T) {
	build := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "1234567890abcdef"},
			{Key: "vcs.modified", Value: "false"},
		},
	}
	tag, err := ReleaseTagFromBuildInfo(build)
	if err != nil {
		t.Fatalf("ReleaseTagFromBuildInfo() error = %v", err)
	}
	if tag != "dev-1234567" {
		t.Fatalf("release tag = %q, want dev-1234567", tag)
	}

	build.Settings[1].Value = "true"
	if _, err := ReleaseTagFromBuildInfo(build); err == nil {
		t.Fatal("ReleaseTagFromBuildInfo() error = nil for modified build")
	}
}

func TestReleaseTagFromBuildInfoMapsGoPseudoVersionToDevRelease(t *testing.T) {
	build := &debug.BuildInfo{
		Main: debug.Module{Version: "v0.0.0-20260810143503-3010c1869036"},
	}

	tag, err := ReleaseTagFromBuildInfo(build)
	if err != nil {
		t.Fatalf("ReleaseTagFromBuildInfo() error = %v", err)
	}
	if tag != "dev-3010c18" {
		t.Fatalf("release tag = %q, want dev-3010c18", tag)
	}
}
