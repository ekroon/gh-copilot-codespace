package ssh

import (
	"testing"

	"github.com/ekroon/gh-copilot-codespace/internal/helperinfo"
)

func TestFilesystemHelperRequiresVerifiedSelection(t *testing.T) {
	client := NewClient("demo")
	if got := client.FilesystemHelperPath(); got != "" {
		t.Fatalf("default helper path = %q, want empty", got)
	}

	if err := client.SelectFilesystemHelper("/remote/helper", helperinfo.Current()); err != nil {
		t.Fatalf("SelectFilesystemHelper() error = %v", err)
	}
	if got := client.FilesystemHelperPath(); got != "/remote/helper" {
		t.Fatalf("selected helper path = %q, want /remote/helper", got)
	}
}
