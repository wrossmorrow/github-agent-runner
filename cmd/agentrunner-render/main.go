// Command agentrunner-render compiles AgenticRunnerToolchain and
// AgenticRunnerDeployment inputs into ARC v2 + ConfigMap + NetworkPolicy JSON.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
	"github.com/wrossmorrow/agentrunner/pkg/render"
)

func main() {
	example := flag.Bool("example", false, "render a built-in Go reviewer example")
	asList := flag.Bool("list", false, "wrap output in a Kubernetes v1 List (for kubectl apply -f -)")
	toolchainPath := flag.String("toolchain", "", "path to AgenticRunnerToolchain JSON")
	deploymentPath := flag.String("deployment", "", "path to AgenticRunnerDeployment JSON")

	repoURL := flag.String("repo-url", "", "override spec.runner.githubConfigUrl")
	gateway := flag.String("gateway", "", "override spec.agent.gateway.endpoint")
	model := flag.String("model", "", "override spec.agent.model")
	image := flag.String("image", "", "override toolchain spec.image")
	githubAuthImage := flag.String("github-auth-image", "", "override github-auth sidecar image")
	githubAppID := flag.String("github-app-id", "", "override spec.agent.githubAppId")
	githubInstallID := flag.String("github-install-id", "", "override spec.agent.githubAppInstallationId")
	namespace := flag.String("namespace", "", "override deployment metadata.namespace")
	arcVersion := flag.String("arc-version", render.DefaultARCVersion, "ARC controller version stamped onto AutoscalingRunnerSet labels")
	arcControllerNS := flag.String("arc-controller-namespace", render.DefaultARCControllerNamespace, "namespace of the ARC controller ServiceAccount")
	arcControllerSA := flag.String("arc-controller-sa", render.DefaultARCControllerSA, "ARC controller ServiceAccount name")
	flag.Parse()

	var tc *v1alpha1.AgenticRunnerToolchain
	var dep *v1alpha1.AgenticRunnerDeployment
	var err error

	if *example {
		dep, tc = exampleObjects()
	} else {
		if *toolchainPath == "" || *deploymentPath == "" {
			fmt.Fprintln(os.Stderr, "usage: agentrunner-render --example | -toolchain <file.json> -deployment <file.json>")
			os.Exit(2)
		}
		tc, err = readJSON[v1alpha1.AgenticRunnerToolchain](*toolchainPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading toolchain: %v\n", err)
			os.Exit(1)
		}
		dep, err = readJSON[v1alpha1.AgenticRunnerDeployment](*deploymentPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "reading deployment: %v\n", err)
			os.Exit(1)
		}
	}

	if *repoURL != "" {
		dep.Spec.Runner.GitHubConfigURL = *repoURL
	}
	if *gateway != "" {
		dep.Spec.Agent.Gateway.Endpoint = *gateway
	}
	if *model != "" {
		dep.Spec.Agent.Model = *model
	}
	if *image != "" {
		tc.Spec.Image = *image
	}
	if *githubAppID != "" {
		dep.Spec.Agent.GitHubAppID = *githubAppID
	}
	if *githubInstallID != "" {
		dep.Spec.Agent.GitHubInstallID = *githubInstallID
	}
	if *namespace != "" {
		dep.Metadata.Namespace = *namespace
	}

	opts := render.Options{
		DefaultGitHubAuthImage:      *githubAuthImage,
		ARCVersion:                  *arcVersion,
		ARCControllerNamespace:      *arcControllerNS,
		ARCControllerServiceAccount: *arcControllerSA,
	}

	bundle, err := render.Render(dep, tc, opts)
	if err != nil {
		fmt.Fprintf(os.Stderr, "render failed: %v\n", err)
		os.Exit(1)
	}

	var out any = bundle.Objects()
	if *asList {
		out = bundle.List()
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintf(os.Stderr, "encoding output: %v\n", err)
		os.Exit(1)
	}
}

func readJSON[T any](path string) (*T, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	return &v, nil
}

func exampleObjects() (*v1alpha1.AgenticRunnerDeployment, *v1alpha1.AgenticRunnerToolchain) {
	tc := &v1alpha1.AgenticRunnerToolchain{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.Group + "/" + v1alpha1.Version, Kind: "AgenticRunnerToolchain"},
		Metadata: v1alpha1.ObjectMeta{Name: "go-1.26"},
		Spec: v1alpha1.AgenticRunnerToolchainSpec{
			Image: "ghcr.io/wrossmorrow/agentrunner-golang:1.26",
			Resources: v1alpha1.ResourceRequirements{
				Requests: map[string]string{"cpu": "2", "memory": "4Gi"},
				Limits:   map[string]string{"cpu": "4", "memory": "8Gi"},
			},
			EgressAllowlist: []string{"proxy.golang.org", "sum.golang.org"},
			AgentsMD:        "Toolchain: Go 1.26, protoc, buf, git, gh.\nPrefer `go test -race ./...` when validating Go changes.",
		},
	}
	dep := &v1alpha1.AgenticRunnerDeployment{
		TypeMeta: v1alpha1.TypeMeta{APIVersion: v1alpha1.Group + "/" + v1alpha1.Version, Kind: "AgenticRunnerDeployment"},
		Metadata: v1alpha1.ObjectMeta{Name: "go-reviewer", Namespace: "actions-runners"},
		Spec: v1alpha1.AgenticRunnerDeploymentSpec{
			Runner: v1alpha1.RunnerSpec{
				GitHubConfigURL:    "https://github.com/my-org/my-repo",
				GitHubConfigSecret: "arc-github-auth",
				RunnerScaleSetName: "agentic-go-review",
				MinRunners:         0,
				MaxRunners:         5,
			},
			ToolchainRef: v1alpha1.LocalRef{Name: "go-1.26"},
			Agent: v1alpha1.AgentSpec{
				Harness: "pi",
				Model:   "claude-sonnet-4-5",
				Gateway: v1alpha1.GatewaySpec{
					Endpoint:      "https://agent-gateway.internal/v1",
					AuthSecretRef: &v1alpha1.SecretRef{Name: "agent-gateway-token"},
				},
				Limits: v1alpha1.LimitsSpec{
					MaxTurns:    25,
					MaxDuration: "10m",
					MaxCostUSD:  2.00,
				},
				SystemPromptRef:    &v1alpha1.ConfigMapKeyRef{ConfigMapName: "org-review-policy"},
				GitHubAppSecretRef: &v1alpha1.SecretRef{Name: "review-bot-app-key"},
			},
		},
	}
	return dep, tc
}
