//go:build linux && cgo && microsandboxcgo

package driver

import (
	"os"
	"path/filepath"
	"testing"
)

// TestValidateMicrosandboxDiskToolsReadsTheCapabilityInTheCLocale pins that a
// host whose mkfs.ext4 prints a translated usage line still passes the check. The
// capability is read from that text, so the probe has to ask for the C locale
// rather than inherit the caller's.
func TestValidateMicrosandboxDiskToolsReadsTheCapabilityInTheCLocale(t *testing.T) {
	tools := t.TempDir()
	writeTool := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(tools, name), []byte(body), 0o755); err != nil {
			t.Fatalf("write the %s fixture: %v", name, err)
		}
	}
	writeTool("qemu-img", "#!/bin/sh\nexit 0\n")
	writeTool("mkfs.ext4", `#!/bin/sh
case "${LC_ALL:-${LANG:-}}" in
  C|C.*|POSIX) echo "	[-d root-directory]";;
  *) echo "	[-d 根目录]";;
esac
`)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("LC_ALL", "zh_CN.UTF-8")
	t.Setenv("LANG", "zh_CN.UTF-8")
	if err := validateMicrosandboxDiskTools(); err != nil {
		t.Fatalf("validateMicrosandboxDiskTools() = %v, want the host tool accepted", err)
	}
}
