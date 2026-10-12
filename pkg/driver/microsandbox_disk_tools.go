//go:build linux && cgo && microsandboxcgo

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// validateMicrosandboxDiskTools checks the host tools the base-disk build needs.
// The `-d root-directory` capability is read from mkfs.ext4's usage text, so the
// probe runs under the C locale: a translated usage line (for example
// `[-d 根目录]` on a zh_CN host) would otherwise be read as a tool that cannot
// unpack a root directory, failing a sandbox the host could have started.
func validateMicrosandboxDiskTools() error {
	if _, err := exec.LookPath("qemu-img"); err != nil {
		return fmt.Errorf("microsandbox requires qemu-img: %w", err)
	}
	mkfsPath, err := exec.LookPath("mkfs.ext4")
	if err != nil {
		return fmt.Errorf("microsandbox requires mkfs.ext4: %w", err)
	}
	command := exec.Command(mkfsPath)
	command.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	output, _ := command.CombinedOutput()
	if !strings.Contains(string(output), "[-d root-directory") {
		return fmt.Errorf("microsandbox requires mkfs.ext4 with -d root-directory support")
	}
	return nil
}
