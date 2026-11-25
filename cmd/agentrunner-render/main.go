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
	toolchainPath := flag.String("toolchain", "", "path to AgenticRunnerToolchain JSON")
	deploymentPath := flag.String("deployment", "", "path to AgenticRunnerDeployment JSON")
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

	bundle, err := render.Render(dep, tc, render.Options{})
	if err != nil {
		fmt.Fprintf(os.Stderr, "render failed: %v\n", err)
		os.Exit(1)
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bundle.Objects()); err != nil {
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
		Metadata: v1alpha1.ObjectMeta{Name: "go-1.23"},
		Spec: v1alpha1.AgenticRunnerToolchainSpec{
			Image: "ghcr.io/org/agentrunner-go:1.23",
			Resources: v1alpha1.ResourceRequirements{
				Requests: map[string]string{"cpu": "2", "memory": "4Gi"},
				Limits:   map[string]string{"cpu": "4", "memory": "8Gi"},
			},
			EgressAllowlist: []string{"proxy.golang.org", "sum.golang.org"},
			AgentsMD:        "Toolchain: Go 1.23, golangci-lint, protoc, buf, git, gh.\nPrefer `go test -race ./...` when validating Go changes.",
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
			ToolchainRef: v1alpha1.LocalRef{Name: "go-1.23"},
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
