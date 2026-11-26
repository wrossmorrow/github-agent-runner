package v1alpha1

import (
	"strings"
	"testing"
)

func validToolchain() *AgenticRunnerToolchain {
	return &AgenticRunnerToolchain{
		TypeMeta: TypeMeta{APIVersion: Group + "/" + Version, Kind: "AgenticRunnerToolchain"},
		Metadata: ObjectMeta{Name: "go-1.23"},
		Spec: AgenticRunnerToolchainSpec{
			Image:           "ghcr.io/org/agentrunner-go:1.23",
			EgressAllowlist: []string{"proxy.golang.org", "sum.golang.org"},
			AgentsMD:        "Run `go test -race ./...`",
		},
	}
}

func validDeployment() *AgenticRunnerDeployment {
	return &AgenticRunnerDeployment{
		TypeMeta: TypeMeta{APIVersion: Group + "/" + Version, Kind: "AgenticRunnerDeployment"},
		Metadata: ObjectMeta{Name: "go-reviewer", Namespace: "actions-runners"},
		Spec: AgenticRunnerDeploymentSpec{
			Runner: RunnerSpec{
				GitHubConfigURL:    "https://github.com/my-org/my-repo",
				RunnerScaleSetName: "agentic-go-review",
				MinRunners:         0,
				MaxRunners:         5,
			},
			ToolchainRef: LocalRef{Name: "go-1.23"},
			Agent: AgentSpec{
				Harness: "pi",
				Model:   "claude-sonnet-4-5",
				Gateway: GatewaySpec{Endpoint: "https://agent-gateway.internal/v1"},
				Limits: LimitsSpec{
					MaxTurns:    25,
					MaxDuration: "10m",
					MaxCostUSD:  2.0,
				},
			},
		},
	}
}

func TestToolchainValidate(t *testing.T) {
	if err := validToolchain().Validate(); err != nil {
		t.Fatalf("expected valid toolchain, got %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*AgenticRunnerToolchain)
		wantErr string
	}{
		{
			name:    "missing name",
			mutate:  func(tc *AgenticRunnerToolchain) { tc.Metadata.Name = "" },
			wantErr: "metadata.name",
		},
		{
			name:    "missing image",
			mutate:  func(tc *AgenticRunnerToolchain) { tc.Spec.Image = "  " },
			wantErr: "spec.image",
		},
		{
			name:    "bad egress entry with path",
			mutate:  func(tc *AgenticRunnerToolchain) { tc.Spec.EgressAllowlist = []string{"proxy.golang.org/foo"} },
			wantErr: "egressAllowlist",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := validToolchain()
			tc.mutate(obj)
			err := obj.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestDeploymentValidate(t *testing.T) {
	if err := validDeployment().Validate(); err != nil {
		t.Fatalf("expected valid deployment, got %v", err)
	}

	cases := []struct {
		name    string
		mutate  func(*AgenticRunnerDeployment)
		wantErr string
	}{
		{
			name:    "missing name",
			mutate:  func(d *AgenticRunnerDeployment) { d.Metadata.Name = "" },
			wantErr: "metadata.name",
		},
		{
			name:    "invalid githubConfigUrl",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Runner.GitHubConfigURL = "ftp://github.com/org" },
			wantErr: "githubConfigUrl",
		},
		{
			name:    "missing runnerScaleSetName",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Runner.RunnerScaleSetName = "" },
			wantErr: "runnerScaleSetName",
		},
		{
			name: "maxRunners less than minRunners",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Runner.MinRunners = 4
				d.Spec.Runner.MaxRunners = 2
			},
			wantErr: "maxRunners",
		},
		{
			name:    "missing toolchainRef",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.ToolchainRef.Name = "" },
			wantErr: "toolchainRef",
		},
		{
			name:    "unsupported harness",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Harness = "other" },
			wantErr: "harness",
		},
		{
			name:    "missing model",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Model = "" },
			wantErr: "model",
		},
		{
			name:    "bad gateway endpoint",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Gateway.Endpoint = "not-a-url" },
			wantErr: "gateway.endpoint",
		},
		{
			name:    "negative maxTurns",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Limits.MaxTurns = -1 },
			wantErr: "maxTurns",
		},
		{
			name:    "negative maxCostUsd",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Limits.MaxCostUSD = -0.5 },
			wantErr: "maxCostUsd",
		},
		{
			name:    "bad maxDuration",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Limits.MaxDuration = "forever" },
			wantErr: "maxDuration",
		},
		{
			name:    "negative contextWindow",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Limits.ContextWindow = -1 },
			wantErr: "contextWindow",
		},
		{
			name:    "negative maxTokens",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Limits.MaxTokens = -1 },
			wantErr: "maxTokens",
		},
		{
			name: "maxTokens exceeds contextWindow",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.Limits.ContextWindow = 8192
				d.Spec.Agent.Limits.MaxTokens = 16384
			},
			wantErr: "maxTokens must be <=",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			obj := validDeployment()
			tc.mutate(obj)
			err := obj.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
