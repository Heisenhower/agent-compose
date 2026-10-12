package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	networkapi "github.com/docker/docker/api/types/network"
	"github.com/docker/docker/client"
)

// dockerNetworkPolicyE2EImageEnv names the guest image the execution-matrix test
// runs in. The test needs a guest with curl and outbound network access: the
// declared-allowance row reaches a public host on purpose, so that the engine's
// proxy is what decides the connection instead of a local fixture that no
// declaration could name (loopback is refused whatever a policy says).
const dockerNetworkPolicyE2EImageEnv = "AGENT_COMPOSE_E2E_NETWORK_POLICY_IMAGE"

// TestE2EDockerNetworkPolicyExecutionMatrix states what a declared policy.network
// does on the default Docker driver, row by row:
//
//   - a declared allowance is tunnelled and succeeds (200);
//   - a host the policy does not allow is refused by the engine (403), before
//     any connection is opened;
//   - the metadata address is refused by the engine's own address rule;
//   - the guest carries the proxy environment (6 *_proxy variables);
//   - an agent that declared nothing keeps unrestricted egress and no proxy;
//   - an allow list without `default: deny` restricts nothing;
//   - editing the declaration reaches a sandbox that is already running, because
//     the proxy re-reads the declaration per request;
//   - the daemon records the refusals, and the address refusal names the engine
//     rule rather than the project's.
func TestE2EDockerNetworkPolicyExecutionMatrix(t *testing.T) {
	image := strings.TrimSpace(os.Getenv(dockerNetworkPolicyE2EImageEnv))
	if image == "" {
		t.Skipf("set %s to a local Docker guest image with curl and network access", dockerNetworkPolicyE2EImageEnv)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Minute)
	defer cancel()
	repoRoot := e2eRepoRoot(t)
	root, err := os.MkdirTemp(repoRoot, ".netpol-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	dataRoot := filepath.Join(root, "data")
	if err := os.MkdirAll(dataRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	projectRoot := filepath.Join(root, "project")
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	binary := e2eDaemonBinary(t, ctx, repoRoot, root)
	dockerClient := newE2EDockerClient(t, ctx, image)
	hostAddress := networkPolicyE2EHostAddress(t, ctx, dockerClient)
	address := unusedNetworkPolicyAddress(t, hostAddress)
	baseURL := "http://" + address
	// The sandbox reaches the proxy over the host address the Docker bridge
	// exposes, and that is also the base URL the daemon hands the guest.
	// The control socket lives in the system temporary directory: a socket path
	// is limited to about 108 bytes and a checkout path can exceed it.
	socketPath := filepath.Join(os.TempDir(), fmt.Sprintf("agent-compose-netpol-%d.sock", os.Getpid()))
	t.Cleanup(func() { _ = os.Remove(socketPath) })
	daemon := startE2EDaemonWithEnv(t, binary, repoRoot, dataRoot, address, image, map[string]string{
		"AGENT_COMPOSE_RUNTIME_BASE_URL": baseURL,
		"AGENT_COMPOSE_SOCKET":           socketPath,
	})
	waitForE2EDaemon(t, ctx, daemon, baseURL)
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("network policy daemon log:\n%s", daemon.logs.String())
		}
	})

	writeNetworkPolicyE2EProject(t, projectRoot, false)
	runNetworkPolicyE2ECLI(t, ctx, binary, baseURL, projectRoot, "up")

	const curl = `command -v curl >/dev/null || { echo NO_CURL; exit 9; }; curl -sS -m 20 -o /dev/null -w "%{http_code}"`
	allow := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "run", "matrix", "--rm", "--command", curl+" https://api.github.com/ 2>&1")
	if !strings.Contains(allow, "200") {
		t.Fatalf("declared allowance did not pass: %q", allow)
	}
	refused := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "run", "matrix", "--rm", "--command", curl+" https://example.com/ 2>&1")
	if !strings.Contains(refused, "403") {
		t.Fatalf("an undeclared host was not refused by the engine: %q", refused)
	}
	metadata := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "run", "matrix", "--rm", "--command", curl+" http://169.254.169.254/latest/meta-data/ 2>&1")
	if !strings.Contains(metadata, "403") {
		t.Fatalf("the metadata address was not refused: %q", metadata)
	}
	if proxied := networkPolicyE2EGuestResult(t, ctx, binary, baseURL, projectRoot, "run", "matrix", "--rm", "--command", `env | grep -ci "_proxy="`); proxied != "6" {
		t.Fatalf("declared sandbox proxy environment = %q, want 6 variables", proxied)
	}
	if open := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "run", "open", "--rm", "--command", curl+" https://example.com/ 2>&1"); !strings.Contains(open, "200") {
		t.Fatalf("an agent that declared nothing was restricted: %q", open)
	}
	if proxied := networkPolicyE2EGuestResult(t, ctx, binary, baseURL, projectRoot, "run", "open", "--rm", "--command", `env | grep -ci "_proxy="`); proxied != "0" {
		t.Fatalf("an agent that declared nothing carries a proxy: %q", proxied)
	}
	if listOnly := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "run", "allow-list-only", "--rm", "--command", curl+" https://example.com/ 2>&1"); !strings.Contains(listOnly, "200") {
		t.Fatalf("an allow list without a declared default restricted egress: %q", listOnly)
	}

	// One running sandbox, two declarations: the proxy decides per request, so
	// the edit reaches the sandbox that is already up.
	sandboxID := networkPolicyE2ESandboxID(t, ctx, binary, baseURL, projectRoot)
	beforeEdit := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "exec", sandboxID, "--command", curl+" https://example.com/ 2>&1")
	if !strings.Contains(beforeEdit, "403") {
		t.Fatalf("before the edit the undeclared host was not refused: %q", beforeEdit)
	}
	writeNetworkPolicyE2EProject(t, projectRoot, true)
	runNetworkPolicyE2ECLI(t, ctx, binary, baseURL, projectRoot, "up")
	afterEdit := runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, "exec", sandboxID, "--command", curl+" https://example.com/ 2>&1")
	if !strings.Contains(afterEdit, "200") {
		t.Fatalf("after the edit the running sandbox was still refused: %q", afterEdit)
	}

	logs := daemon.logs.String()
	if !strings.Contains(logs, "egress proxy denied a request") {
		t.Fatalf("the daemon did not record the refusals:\n%s", logs)
	}
	if !strings.Contains(logs, "engine.blocked-address") {
		t.Fatalf("the metadata refusal did not name the engine rule:\n%s", logs)
	}
}

// networkPolicyE2EHostAddress returns the host address a sandbox can reach, which
// is the Docker bridge gateway for the default network a sandbox starts on.
func networkPolicyE2EHostAddress(t *testing.T, ctx context.Context, dockerClient *client.Client) string {
	t.Helper()
	if configured := strings.TrimSpace(os.Getenv("AGENT_COMPOSE_E2E_NETWORK_POLICY_HOST_ADDRESS")); configured != "" {
		return configured
	}
	network, err := dockerClient.NetworkInspect(ctx, "bridge", networkapi.InspectOptions{})
	if err != nil {
		t.Skipf("inspect the Docker bridge network: %v", err)
	}
	for _, config := range network.IPAM.Config {
		if gateway := strings.TrimSpace(config.Gateway); gateway != "" {
			return gateway
		}
	}
	t.Skip("the Docker bridge network exposes no gateway address; set AGENT_COMPOSE_E2E_NETWORK_POLICY_HOST_ADDRESS")
	return ""
}

// unusedNetworkPolicyAddress reserves a free port on the address sandboxes use to
// reach the daemon, so the proxy listener is reachable from inside a sandbox.
func unusedNetworkPolicyAddress(t *testing.T, host string) string {
	t.Helper()
	listener, err := net.Listen("tcp", net.JoinHostPort(host, "0"))
	if err != nil {
		t.Fatalf("allocate daemon listen address on %s: %v", host, err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("release daemon listen address: %v", err)
	}
	return address
}

// runNetworkPolicyE2ECLI runs a CLI command that must succeed.
func runNetworkPolicyE2ECLI(t *testing.T, ctx context.Context, binary, baseURL, projectRoot string, args ...string) string {
	t.Helper()
	output, err := execNetworkPolicyE2ECLI(ctx, binary, baseURL, projectRoot, args...)
	if err != nil {
		t.Fatalf("agent-compose %s: %v\n%s", strings.Join(args, " "), err, output)
	}
	return output
}

// runNetworkPolicyE2EGuest runs a CLI command whose exit status reports the guest
// command's own failure, such as curl refusing a tunnelled connection.
func runNetworkPolicyE2EGuest(t *testing.T, ctx context.Context, binary, baseURL, projectRoot string, args ...string) string {
	t.Helper()
	output, _ := execNetworkPolicyE2ECLI(ctx, binary, baseURL, projectRoot, args...)
	return output
}

// networkPolicyE2EGuestResult returns the guest command's own first output line.
// A guest command that reports nothing, such as a grep with no match, still
// prints its result before the CLI reports the command's exit status.
func networkPolicyE2EGuestResult(t *testing.T, ctx context.Context, binary, baseURL, projectRoot string, args ...string) string {
	t.Helper()
	line, _, _ := strings.Cut(strings.TrimSpace(runNetworkPolicyE2EGuest(t, ctx, binary, baseURL, projectRoot, args...)), "\n")
	return strings.TrimSpace(line)
}

func execNetworkPolicyE2ECLI(ctx context.Context, binary, baseURL, projectRoot string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, binary, append([]string{"--host", baseURL}, args...)...)
	command.Dir = projectRoot
	// The test daemon listens on TCP; an inherited socket path would send the
	// command to whatever daemon happens to own it.
	command.Env = overrideE2EEnv(os.Environ(), map[string]string{"AGENT_COMPOSE_SOCKET": ""})
	output, err := command.CombinedOutput()
	return string(output), err
}

// networkPolicyE2ESandboxID keeps one sandbox running so the edit has something to
// reach.
func networkPolicyE2ESandboxID(t *testing.T, ctx context.Context, binary, baseURL, projectRoot string) string {
	t.Helper()
	output := runNetworkPolicyE2ECLI(t, ctx, binary, baseURL, projectRoot, "run", "live-update", "--keep-running", "--command", "true", "--json")
	var started struct {
		SandboxID string `json:"sandbox_id"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(output)), &started); err != nil {
		t.Fatalf("decode the started sandbox from %q: %v", strings.TrimSpace(output), err)
	}
	if strings.TrimSpace(started.SandboxID) == "" {
		t.Fatalf("no sandbox id in %q", strings.TrimSpace(output))
	}
	t.Cleanup(func() {
		cleanup := exec.Command(binary, "--host", baseURL, "sandbox", "rm", started.SandboxID)
		cleanup.Dir = projectRoot
		cleanup.Env = overrideE2EEnv(os.Environ(), map[string]string{"AGENT_COMPOSE_SOCKET": ""})
		_ = cleanup.Run()
	})
	return started.SandboxID
}

// writeNetworkPolicyE2EProject writes the project the matrix applies. The
// live-update agent starts without example.com allowed; the second write adds it,
// which is the edit the running sandbox must observe.
func writeNetworkPolicyE2EProject(t *testing.T, projectRoot string, liveUpdateAllowsExample bool) {
	t.Helper()
	liveUpdateAllow := "          - host: api.github.com\n            port: 443\n            protocol: https\n"
	if liveUpdateAllowsExample {
		liveUpdateAllow += "          - host: example.com\n            port: 443\n            protocol: https\n"
	}
	project := `name: netpol-e2e
agents:
  matrix:
    provider: codex
    system_prompt: |
      Answer with the single word requested and nothing else.
    policy:
      network:
        default: deny
        allow:
          - host: api.github.com
            port: 443
            protocol: https
    driver:
      docker: {}
  open:
    provider: codex
    system_prompt: |
      Answer with the single word requested and nothing else.
    driver:
      docker: {}
  allow-list-only:
    provider: codex
    system_prompt: |
      Answer with the single word requested and nothing else.
    policy:
      network:
        default: allow-all
        allow:
          - host: api.github.com
            port: 443
            protocol: https
    driver:
      docker: {}
  live-update:
    provider: codex
    system_prompt: |
      Answer with the single word requested and nothing else.
    policy:
      network:
        default: deny
        allow:
` + liveUpdateAllow + `    driver:
      docker: {}
`
	if err := os.WriteFile(filepath.Join(projectRoot, "agent-compose.yml"), []byte(project), 0o644); err != nil {
		t.Fatalf("write the network policy project: %v", err)
	}
}
