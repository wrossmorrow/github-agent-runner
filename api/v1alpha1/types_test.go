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

	for _, mode := range []string{AgentModeReadOnly, AgentModeSuggest, AgentModeWriteBranch} {
		obj := validDeployment()
		obj.Spec.Agent.Mode = mode
		obj.Spec.Agent.EgressAllowlist = []string{"staging.internal.example"}
		obj.Spec.Runner.ServiceAccountName = "deploy-verifier.ro"
		if err := obj.Validate(); err != nil {
			t.Fatalf("expected valid deployment with mode %q, got %v", mode, err)
		}
	}

	zeroRetries := 0
	withZeroRetries := validDeployment()
	withZeroRetries.Spec.Agent.Limits.MaxTransportRetries = &zeroRetries
	if err := withZeroRetries.Validate(); err != nil {
		t.Fatalf("expected maxTransportRetries=0 to be valid, got %v", err)
	}

	customProtected := []string{"main", "release"}
	withCustomProtected := validDeployment()
	withCustomProtected.Spec.Agent.ProtectedBranches = &customProtected
	if err := withCustomProtected.Validate(); err != nil {
		t.Fatalf("expected custom protectedBranches to be valid, got %v", err)
	}

	emptyProtected := []string{}
	withEmptyProtected := validDeployment()
	withEmptyProtected.Spec.Agent.ProtectedBranches = &emptyProtected
	if err := withEmptyProtected.Validate(); err != nil {
		t.Fatalf("expected empty protectedBranches slice to be valid, got %v", err)
	}

	withSessionSink := validDeployment()
	withSessionSink.Spec.Agent.SessionSink = &SessionSinkSpec{
		Endpoint:      "http://session-hub.session-system.svc.cluster.local:8080/v1/sessions",
		AuthSecretRef: &SecretRef{Name: "session-sink-token"},
	}
	if err := withSessionSink.Validate(); err != nil {
		t.Fatalf("expected valid sessionSink to pass validation, got %v", err)
	}

	negRetries := -1
	badProtected := []string{"main", "bad branch"}
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
		{
			name:    "unsupported agent mode",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Mode = "autonomous" },
			wantErr: "spec.agent.mode",
		},
		{
			name: "invalid agent egressAllowlist",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.EgressAllowlist = []string{"https://staging.example.com/health"}
			},
			wantErr: "spec.agent.egressAllowlist",
		},
		{
			name:    "invalid serviceAccountName with slash",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Runner.ServiceAccountName = "ns/sa" },
			wantErr: "serviceAccountName",
		},
		{
			name:    "invalid serviceAccountName uppercase and underscore",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Runner.ServiceAccountName = "Deploy_Verifier" },
			wantErr: "serviceAccountName",
		},
		{
			name:    "invalid serviceAccountName leading hyphen",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Runner.ServiceAccountName = "-bad" },
			wantErr: "serviceAccountName",
		},
		{
			name:    "negative maxTransportRetries",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.Limits.MaxTransportRetries = &negRetries },
			wantErr: "maxTransportRetries",
		},
		{
			name:    "invalid protectedBranches entry with whitespace",
			mutate:  func(d *AgenticRunnerDeployment) { d.Spec.Agent.ProtectedBranches = &badProtected },
			wantErr: "protectedBranches",
		},
		{
			name: "blank protectedBranches entry",
			mutate: func(d *AgenticRunnerDeployment) {
				blank := []string{"   "}
				d.Spec.Agent.ProtectedBranches = &blank
			},
			wantErr: "protectedBranches",
		},
		{
			name: "invalid sessionSink endpoint",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.SessionSink = &SessionSinkSpec{Endpoint: "not-a-url"}
			},
			wantErr: "sessionSink.endpoint",
		},
		{
			name: "duplicate inline mcpServers name",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.MCPServers = []InlineMCPServerSpec{
					{Name: "docs", AgenticRunnerMCPServerSpec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com/1"}},
					{Name: "docs", AgenticRunnerMCPServerSpec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com/2"}},
				}
			},
			wantErr: "duplicate spec.agent.mcpServers name",
		},
		{
			name: "invalid mcpServerRefs name",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.MCPServerRefs = []LocalRef{{Name: "Invalid_Upper"}}
			},
			wantErr: "mcpServerRefs",
		},
		{
			name: "duplicate mcpServerRefs name",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.MCPServerRefs = []LocalRef{{Name: "docs"}, {Name: "docs"}}
			},
			wantErr: "duplicate spec.agent.mcpServerRefs name",
		},
		{
			name: "normalized collision in mcpServerRefs",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.MCPServerRefs = []LocalRef{{Name: "foo-bar"}, {Name: "foo_bar"}}
			},
			wantErr: "token-env normalization",
		},
		{
			name: "normalized collision in inline mcpServers",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.MCPServers = []InlineMCPServerSpec{
					{Name: "foo-bar", AgenticRunnerMCPServerSpec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com/1"}},
					{Name: "foo_bar", AgenticRunnerMCPServerSpec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com/2"}},
				}
			},
			wantErr: "token-env normalization",
		},
		{
			name: "normalized collision across mcpServerRefs and inline mcpServers",
			mutate: func(d *AgenticRunnerDeployment) {
				d.Spec.Agent.MCPServerRefs = []LocalRef{{Name: "foo-bar"}}
				d.Spec.Agent.MCPServers = []InlineMCPServerSpec{
					{Name: "foo_bar", AgenticRunnerMCPServerSpec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com/2"}},
				}
			},
			wantErr: "token-env normalization",
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

	overrideDep := validDeployment()
	overrideDep.Spec.Agent.MCPServerRefs = []LocalRef{{Name: "foo-bar"}}
	overrideDep.Spec.Agent.MCPServers = []InlineMCPServerSpec{
		{Name: "foo-bar", AgenticRunnerMCPServerSpec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com/override"}},
	}
	if err := overrideDep.Validate(); err != nil {
		t.Fatalf("expected exact-name inline MCP override to pass validation, got %v", err)
	}
}

func TestMCPServerValidate(t *testing.T) {
	httpSrv := &AgenticRunnerMCPServer{
		Metadata: ObjectMeta{Name: "internal-docs", Namespace: "actions-runners"},
		Spec: AgenticRunnerMCPServerSpec{
			URL:           "http://docs-mcp.platform.svc.cluster.local:8080/mcp",
			AuthSecretRef: &SecretRef{Name: "docs-mcp-token"},
			AgentsMD:      "Use internal-docs.",
		},
	}
	if err := httpSrv.Validate(); err != nil {
		t.Fatalf("expected valid remote HTTP MCP server, got %v", err)
	}

	stdioSrv := &AgenticRunnerMCPServer{
		Metadata: ObjectMeta{Name: "local-tool", Namespace: "actions-runners"},
		Spec: AgenticRunnerMCPServerSpec{
			Command: "/usr/local/bin/my-mcp",
			Args:    []string{"--stdio"},
		},
	}
	if err := stdioSrv.Validate(); err != nil {
		t.Fatalf("expected valid stdio MCP server, got %v", err)
	}

	cases := []struct {
		name    string
		srv     AgenticRunnerMCPServer
		wantErr string
	}{
		{
			name:    "invalid name",
			srv:     AgenticRunnerMCPServer{Metadata: ObjectMeta{Name: "-bad"}, Spec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com"}},
			wantErr: "metadata.name",
		},
		{
			name:    "both url and command set",
			srv:     AgenticRunnerMCPServer{Metadata: ObjectMeta{Name: "docs"}, Spec: AgenticRunnerMCPServerSpec{URL: "https://mcp.example.com", Command: "mcp"}},
			wantErr: "exactly one of url or command",
		},
		{
			name:    "neither url nor command set",
			srv:     AgenticRunnerMCPServer{Metadata: ObjectMeta{Name: "docs"}},
			wantErr: "exactly one of url or command",
		},
		{
			name:    "command contains spaces",
			srv:     AgenticRunnerMCPServer{Metadata: ObjectMeta{Name: "docs"}, Spec: AgenticRunnerMCPServerSpec{Command: "npx -y foo"}},
			wantErr: "without whitespace",
		},
		{
			name:    "authSecretRef on stdio server",
			srv:     AgenticRunnerMCPServer{Metadata: ObjectMeta{Name: "docs"}, Spec: AgenticRunnerMCPServerSpec{Command: "/usr/local/bin/mcp", AuthSecretRef: &SecretRef{Name: "tok"}}},
			wantErr: "only valid when url is set",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.srv.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}
