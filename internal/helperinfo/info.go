package helperinfo

import (
	"encoding/json"
	"fmt"
	"runtime/debug"
	"slices"
	"strings"
)

const (
	SchemaVersion             = 1
	DaemonProtocolVersion     = "2"
	FilesystemProtocolVersion = "1"
	CapabilityDaemon          = "daemon"
	CapabilityFilesystem      = "filesystem"
)

type Info struct {
	SchemaVersion      int      `json:"schemaVersion"`
	Version            string   `json:"version"`
	DaemonProtocol     string   `json:"daemonProtocol"`
	FilesystemProtocol string   `json:"filesystemProtocol"`
	Capabilities       []string `json:"capabilities"`
}

func Current() Info {
	build, _ := debug.ReadBuildInfo()
	return Info{
		SchemaVersion:      SchemaVersion,
		Version:            VersionFromBuildInfo(build),
		DaemonProtocol:     DaemonProtocolVersion,
		FilesystemProtocol: FilesystemProtocolVersion,
		Capabilities:       []string{CapabilityDaemon, CapabilityFilesystem},
	}
}

func VersionFromBuildInfo(build *debug.BuildInfo) string {
	if build == nil {
		return "devel"
	}
	if version := strings.TrimSpace(build.Main.Version); version != "" && version != "(devel)" {
		return version
	}
	for _, setting := range build.Settings {
		if setting.Key != "vcs.revision" || setting.Value == "" {
			continue
		}
		revision := setting.Value
		if len(revision) > 12 {
			revision = revision[:12]
		}
		if buildSetting(build, "vcs.modified") == "true" {
			return "dev-" + revision + "-modified"
		}
		return "dev-" + revision
	}
	return "devel"
}

func ReleaseTagFromBuildInfo(build *debug.BuildInfo) (string, error) {
	if build != nil {
		if version := strings.TrimSpace(build.Main.Version); strings.HasPrefix(version, "v") {
			return version, nil
		}
		for _, setting := range build.Settings {
			if setting.Key == "vcs.revision" && len(setting.Value) >= 7 && buildSetting(build, "vcs.modified") != "true" {
				return "dev-" + setting.Value[:7], nil
			}
		}
	}
	return "", fmt.Errorf("helper build has no exact release tag")
}

func buildSetting(build *debug.BuildInfo, key string) string {
	if build == nil {
		return ""
	}
	for _, setting := range build.Settings {
		if setting.Key == key {
			return setting.Value
		}
	}
	return ""
}

func Validate(info Info) error {
	if info.SchemaVersion != SchemaVersion {
		return fmt.Errorf("helper info schema version %d is incompatible (want %d)", info.SchemaVersion, SchemaVersion)
	}
	if info.Version == "" {
		return fmt.Errorf("helper version is empty")
	}
	if info.DaemonProtocol != DaemonProtocolVersion {
		return fmt.Errorf("helper daemon protocol %q is incompatible (want %q)", info.DaemonProtocol, DaemonProtocolVersion)
	}
	if info.FilesystemProtocol != FilesystemProtocolVersion {
		return fmt.Errorf("helper filesystem protocol %q is incompatible (want %q)", info.FilesystemProtocol, FilesystemProtocolVersion)
	}
	for _, capability := range []string{CapabilityDaemon, CapabilityFilesystem} {
		if !slices.Contains(info.Capabilities, capability) {
			return fmt.Errorf("helper is missing required %s capability", capability)
		}
	}
	return nil
}

func Parse(data []byte) (Info, error) {
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Info{}, fmt.Errorf("decode helper info: %w", err)
	}
	if err := Validate(info); err != nil {
		return Info{}, err
	}
	return info, nil
}

func (i Info) Marshal() ([]byte, error) {
	return json.Marshal(i)
}
