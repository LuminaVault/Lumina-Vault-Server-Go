package system

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Result is DockerResult.
type Result struct {
	Stdout, Stderr string
	ExitCode       int
}

// OK is DockerResult.ok.
func (r Result) OK() bool { return r.ExitCode == 0 }

// Docker is the part of DockerExec the update engine uses: run forks the docker
// binary, and an error means it could not be launched.
type Docker interface {
	Run(ctx context.Context, args []string) (Result, error)
}

// NoDocker is the default Docker: production has no docker binary (decision
// O1), so launching it fails, as ProcessDockerExec's spawn does.
type NoDocker struct{}

// ErrNoDocker is what launching a missing docker binary reports.
var ErrNoDocker = errors.New("spawnFailed(docker binary not found)")

func (NoDocker) Run(context.Context, []string) (Result, error) { return Result{}, ErrNoDocker }

// HealthProbe reports whether the gateway published on port answers
// GET /v1/models with a 2xx for apiKey.
type HealthProbe func(ctx context.Context, port int, apiKey string) bool

// LoopbackProbe is App+build's probe: http://127.0.0.1:<port>/v1/models with the
// bearer, 5 s timeout.
func LoopbackProbe(ctx context.Context, port int, apiKey string) bool {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/v1/models", nil)
	if err != nil {
		return false
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode >= 200 && resp.StatusCode < 300
}

// Tenants is the part of HermesContainerManager the update uses.
type Tenants interface {
	SetImage(ref string)
	ReprovisionAll(ctx context.Context) (int, error)
}

// noDockerTenants is the container manager in a pod with no docker:
// reprovisionAll starts with ensureNetworkExists, which throws.
type noDockerTenants struct {
	docker  Docker
	network string
}

func (noDockerTenants) SetImage(string) {}

func (t noDockerTenants) ReprovisionAll(ctx context.Context) (int, error) {
	res, err := t.docker.Run(ctx, []string{"network", "inspect", t.network})
	if err != nil {
		return 0, err
	}
	if !res.OK() {
		if res, err = t.docker.Run(ctx, []string{"network", "create", "--driver=bridge", t.network}); err != nil {
			return 0, err
		} else if !res.OK() {
			return 0, fmt.Errorf("nonZeroExit(args: [\"network\", \"create\", %q], stderr: %q, exitCode: %d)", t.network, res.Stderr, res.ExitCode)
		}
	}
	return 0, errors.New("tenant reprovision needs the container manager")
}

// updateError is HermesUpdateError.
type updateError struct {
	kind     string // dockerUnreachable, pullFailed, containerRunFailed, rollbackFailed, perTenantDisabled
	stderr   string
	exitCode int
}

func (e *updateError) Error() string {
	switch e.kind {
	case "pullFailed", "containerRunFailed":
		return fmt.Sprintf("%s(stderr: %q, exitCode: %d)", e.kind, e.stderr, e.exitCode)
	default:
		return fmt.Sprintf("%s(%q)", e.kind, e.stderr)
	}
}

// central is CentralHermesManager.
type central struct {
	docker                         Docker
	containerName, tempName        string
	registryImage, network, volume string
	port, tempPort                 int
	apiKey, mnemosyneDir           string
	probe                          HealthProbe
	interval                       time.Duration
}

func (c *central) fullRef(tag string) string { return c.registryImage + ":" + tag }

func (c *central) inspectValue(ctx context.Context, args ...string) *string {
	res, err := c.docker.Run(ctx, args)
	if err != nil || !res.OK() {
		return nil
	}
	v := strings.TrimSpace(res.Stdout)
	if v == "" {
		return nil
	}
	return &v
}

func (c *central) currentImageRef(ctx context.Context) *string {
	return c.inspectValue(ctx, "inspect", "--format", "{{.Config.Image}}", c.containerName)
}

func (c *central) currentImageDigest(ctx context.Context) *string {
	return c.inspectValue(ctx, "inspect", "--format", "{{.Image}}", c.containerName)
}

func (c *central) imageDigest(ctx context.Context, ref string) *string {
	return c.inspectValue(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
}

func (c *central) assertDockerReachable(ctx context.Context) error {
	res, err := c.docker.Run(ctx, []string{"version", "--format", "{{.Server.Version}}"})
	if err != nil {
		return err
	}
	if !res.OK() {
		return &updateError{kind: "dockerUnreachable", stderr: res.Stderr}
	}
	return nil
}

func (c *central) pull(ctx context.Context, ref string) error {
	res, err := c.docker.Run(ctx, []string{"pull", ref})
	if err != nil {
		return err
	}
	if !res.OK() {
		return &updateError{kind: "pullFailed", stderr: res.Stderr, exitCode: res.ExitCode}
	}
	return nil
}

func (c *central) runOn(ctx context.Context, name string, port int, image string) error {
	res, err := c.docker.Run(ctx, c.runArgs(name, port, image))
	if err != nil {
		return err
	}
	if !res.OK() {
		return &updateError{kind: "containerRunFailed", stderr: res.Stderr, exitCode: res.ExitCode}
	}
	return nil
}

func (c *central) rm(ctx context.Context, name string) {
	_, _ = c.docker.Run(ctx, []string{"rm", "-f", name})
}

func (c *central) runTemp(ctx context.Context, image string) error {
	c.rm(ctx, c.tempName)
	return c.runOn(ctx, c.tempName, c.tempPort, image)
}

func (c *central) promoteTempToCanonical(ctx context.Context, image string) error {
	c.rm(ctx, c.tempName)
	c.rm(ctx, c.containerName)
	return c.runOn(ctx, c.containerName, c.port, image)
}

func (c *central) removeTemp(ctx context.Context) { c.rm(ctx, c.tempName) }

func (c *central) restoreCanonical(ctx context.Context, image string) error {
	c.rm(ctx, c.containerName)
	return c.runOn(ctx, c.containerName, c.port, image)
}

func (c *central) awaitHealthy(ctx context.Context, port int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if c.probe(ctx, port, c.apiKey) {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(c.interval):
		}
	}
	return false
}

func (c *central) runArgs(name string, port int, image string) []string {
	return []string{
		"run",
		"--detach",
		"--restart=unless-stopped",
		"--name", name,
		"--network", c.network,
		"--publish", strconv.Itoa(port) + ":8642",
		"--volume", c.volume + ":/opt/data",
		"--env", "API_SERVER_ENABLED=true",
		"--env", "API_SERVER_HOST=0.0.0.0",
		"--env", "API_SERVER_PORT=8642",
		"--env", "API_SERVER_KEY=" + c.apiKey,
		"--env", "HERMES_HOME=/opt/data",
		"--env", "MNEMOSYNE_DATA_DIR=" + c.mnemosyneDir,
		image,
		"gateway", "run",
	}
}

// userMessage is HermesUpdateService.userMessage(for:).
func userMessage(err error) string {
	var ue *updateError
	if errors.As(err, &ue) {
		switch ue.kind {
		case "dockerUnreachable":
			return "Couldn't reach Docker on the server."
		case "pullFailed":
			low := strings.ToLower(ue.stderr)
			if strings.Contains(low, "unauthorized") || strings.Contains(low, "denied") {
				return "The server isn't authorized to download Hermes. Check the registry credentials."
			}
			return "Couldn't download the new version. Check the server's internet connection and try again."
		case "containerRunFailed":
			return "The new version failed to start."
		}
	}
	return fmt.Sprintf("The update failed: %v", err)
}
