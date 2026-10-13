package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The agent policy block was renamed from `sandbox` to `policy`. The deprecated
// key keeps working, but a project that still writes it has to be told, at the
// moment it is read, which key replaces it.
func TestComposeConfigWarnsAboutTheDeprecatedSandboxPolicyKey(t *testing.T) {
	for _, tt := range []struct {
		name     string
		agent    string
		wantWarn bool
	}{
		{
			name: "deprecated key is reported",
			agent: `
    sandbox:
      stopped_runtime_policy: retain`,
			wantWarn: true,
		},
		{
			name: "current key is quiet",
			agent: `
    policy:
      stopped_runtime_policy: retain`,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			composePath := filepath.Join(dir, "agent-compose.yml")
			body := "name: deprecated-policy\nagents:\n  worker:\n    provider: codex\n" + tt.agent + "\n"
			if err := os.WriteFile(composePath, []byte(body), 0o644); err != nil {
				t.Fatalf("write compose file: %v", err)
			}

			_, stderr, _, exitCode := executeCLICommand("config", "--file", composePath)
			if exitCode != 0 {
				t.Fatalf("config exit code = %d, stderr = %q", exitCode, stderr)
			}
			want := "Warning: agents.worker.sandbox is deprecated and will be removed in a future release; use agents.worker.policy instead."
			switch {
			case tt.wantWarn && !strings.Contains(stderr, want):
				t.Fatalf("stderr = %q, want the deprecation warning %q", stderr, want)
			case !tt.wantWarn && strings.Contains(stderr, "deprecated"):
				t.Fatalf("stderr = %q, want no deprecation warning for the current key", stderr)
			}
		})
	}
}

// The rendered configuration always carries the current key, so a project that
// follows the warning converges on `policy` without any further edit.
func TestComposeConfigRendersTheDeprecatedBlockUnderPolicy(t *testing.T) {
	dir := t.TempDir()
	composePath := filepath.Join(dir, "agent-compose.yml")
	body := "name: deprecated-policy\nagents:\n  worker:\n    provider: codex\n    sandbox:\n      stopped_runtime_policy: retain\n"
	if err := os.WriteFile(composePath, []byte(body), 0o644); err != nil {
		t.Fatalf("write compose file: %v", err)
	}

	stdout, stderr, _, exitCode := executeCLICommand("config", "--file", composePath, "--json")
	if exitCode != 0 {
		t.Fatalf("config exit code = %d, stderr = %q", exitCode, stderr)
	}
	if !strings.Contains(stdout, `"policy"`) {
		t.Fatalf("stdout = %q, want the policy key", stdout)
	}
	if strings.Contains(stdout, `"sandbox"`) {
		t.Fatalf("stdout = %q, want no sandbox key in the normalized spec", stdout)
	}
}
