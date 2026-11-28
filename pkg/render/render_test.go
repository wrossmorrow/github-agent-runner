package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wrossmorrow/github-agent-runner/api/v1alpha1"
)

func sampleInputs() (*v1alpha1.AgenticRunnerDeployment, *v1alpha1.AgenticRunnerToolchain) {
	tc := &v1alpha1.AgenticRunnerToolchain{
		Metadata: v1alpha1.ObjectMeta{Name: "go-1.23"},
		Spec: v1alpha1.AgenticRunnerToolchainSpec{
			Image: "ghcr.io/org/agentrunner-go:1.23",
			Resources: v1alpha1.ResourceRequirements{
				Requests: map[string]string{"cpu": "2", "memory": "4Gi"},
				Limits:   map[string]string{"cpu": "4", "memory": "8Gi"},
			},
			EgressAllowlist: []string{"proxy.golang.org", "sum.golang.org"},
			AgentsMD:        "Prefer `go test -race ./...`.",
		},
	}
	dep := &v1alpha1.AgenticRunnerDeployment{
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
					Endpoint:      "https://agent-gateway.internal:8443/v1",
					AuthSecretRef: &v1alpha1.SecretRef{Name: "gateway-secret", Key: "api-key"},
				},
				Limits: v1alpha1.LimitsSpec{
					MaxTurns:            25,
					MaxDuration:         "10m",
					MaxCostUSD:          2.0,
					MaxTransportRetries: intPtr(3),
				},
				SystemPromptRef:    &v1alpha1.ConfigMapKeyRef{ConfigMapName: "org-review-policy"},
				AgentsMD:           "Deployment override guidance.",
				GitHubAppSecretRef: &v1alpha1.SecretRef{Name: "review-bot-key"},
				GitHubAppID:        "12345",
				GitHubInstallID:    "67890",
			},
		},
	}
	return dep, tc
}

func TestRenderARCv2AndSidecars(t *testing.T) {
	dep, tc := sampleInputs()
	bundle, err := Render(dep, tc, Options{})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	if got := len(bundle.Objects()); got != 6 {
		t.Fatalf("expected 6 objects, got %d", got)
	}
	if bundle.List()["kind"] != "List" {
		t.Fatalf("expected List kind, got %v", bundle.List()["kind"])
	}
	if bundle.ARCControllerRole["kind"] != "Role" || bundle.ARCControllerRoleBinding["kind"] != "RoleBinding" {
		t.Fatalf("expected Role and RoleBinding, got %v / %v", bundle.ARCControllerRole["kind"], bundle.ARCControllerRoleBinding["kind"])
	}
	subjects := bundle.ARCControllerRoleBinding["subjects"].([]K8sObject)
	if subjects[0]["name"] != DefaultARCControllerSA || subjects[0]["namespace"] != DefaultARCControllerNamespace {
		t.Fatalf("unexpected RoleBinding subject: %#v", subjects)
	}

	// Verify Agent ConfigMap
	agentData := bundle.AgentConfigMap["data"].(map[string]string)
	var rtCfg AgentRuntimeConfig
	if err := json.Unmarshal([]byte(agentData["config.json"]), &rtCfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if rtCfg.Model != "claude-sonnet-4-5" || rtCfg.GatewayEndpoint != "https://agent-gateway.internal:8443/v1" || rtCfg.Mode != v1alpha1.AgentModeReadOnly {
		t.Fatalf("unexpected runtime config: %+v", rtCfg)
	}
	if rtCfg.Limits.MaxTransportRetries == nil || *rtCfg.Limits.MaxTransportRetries != 3 {
		t.Fatalf("expected Limits.MaxTransportRetries=3 in config.json, got %+v", rtCfg.Limits.MaxTransportRetries)
	}
	if !strings.Contains(agentData["15-toolchain-crd.md"], "go test -race") {
		t.Fatalf("missing toolchain agentsMd fragment: %q", agentData["15-toolchain-crd.md"])
	}
	if !strings.Contains(agentData["20-deployment.md"], "Deployment override") {
		t.Fatalf("missing deployment agentsMd fragment: %q", agentData["20-deployment.md"])
	}

	// Verify Squid allowlist includes cluster defaults, toolchain allowlist, and gateway host
	egressData := bundle.EgressConfigMap["data"].(map[string]string)
	squidConf := egressData["squid.conf"]
	for _, want := range []string{"api.github.com", "proxy.golang.org", "agent-gateway.internal", "max_filedescriptors 1024", "/var/log/squid/cache.log", "http_access deny all"} {
		if !strings.Contains(squidConf, want) {
			t.Fatalf("squid.conf missing %q:\n%s", want, squidConf)
		}
	}

	// Verify AutoscalingRunnerSet PodSpec & ARC version label
	arsMeta := bundle.AutoscalingRunnerSet["metadata"].(K8sObject)
	arsLabels := arsMeta["labels"].(map[string]string)
	if arsLabels["app.kubernetes.io/version"] != DefaultARCVersion {
		t.Fatalf("expected app.kubernetes.io/version=%q, got %q", DefaultARCVersion, arsLabels["app.kubernetes.io/version"])
	}
	spec := bundle.AutoscalingRunnerSet["spec"].(K8sObject)
	if bundle.AutoscalingRunnerSet["kind"] != "AutoscalingRunnerSet" {
		t.Fatalf("unexpected kind: %v", bundle.AutoscalingRunnerSet["kind"])
	}
	podSpec := spec["template"].(K8sObject)["spec"].(K8sObject)
	if podSpec["securityContext"].(K8sObject)["fsGroup"] != SharedFSGroup {
		t.Fatalf("expected fsGroup %d", SharedFSGroup)
	}
	if _, hasSA := podSpec["serviceAccountName"]; hasSA {
		t.Fatalf("expected serviceAccountName omitted when unset, got %v", podSpec["serviceAccountName"])
	}

	containers := podSpec["containers"].([]K8sObject)
	if len(containers) != 1 || containers[0]["image"] != DefaultRunnerImage {
		t.Fatalf("expected single stock runner container, got %#v", containers)
	}
	runnerCmd, _ := containers[0]["command"].([]string)
	if len(runnerCmd) != 1 || runnerCmd[0] != "/home/runner/run.sh" {
		t.Fatalf("expected runner command [/home/runner/run.sh], got %#v", containers[0]["command"])
	}
	runnerMounts := containers[0]["volumeMounts"].([]K8sObject)
	if len(runnerMounts) != 1 || runnerMounts[0]["mountPath"] != SharedWorkPath {
		t.Fatalf("runner should only mount %s, got %#v", SharedWorkPath, runnerMounts)
	}

	inits := podSpec["initContainers"].([]K8sObject)
	if len(inits) != 3 {
		t.Fatalf("expected 3 native sidecars, got %d", len(inits))
	}
	byName := map[string]K8sObject{}
	for _, ic := range inits {
		if ic["restartPolicy"] != "Always" {
			t.Fatalf("sidecar %v missing restartPolicy: Always", ic["name"])
		}
		if ic["imagePullPolicy"] != DefaultImagePullPolicy {
			t.Fatalf("sidecar %v imagePullPolicy = %v, want %s", ic["name"], ic["imagePullPolicy"], DefaultImagePullPolicy)
		}
		byName[ic["name"].(string)] = ic
	}

	agent := byName["agent"]
	if agent["image"] != tc.Spec.Image {
		t.Fatalf("agent image = %v, want %v", agent["image"], tc.Spec.Image)
	}
	agentEnv := map[string]string{}
	for _, e := range agent["env"].([]K8sObject) {
		if v, ok := e["value"].(string); ok {
			agentEnv[e["name"].(string)] = v
		}
	}
	if agentEnv["AGENTRUNNER_MODE"] != v1alpha1.AgentModeReadOnly {
		t.Fatalf("AGENTRUNNER_MODE = %q, want %q", agentEnv["AGENTRUNNER_MODE"], v1alpha1.AgentModeReadOnly)
	}
	wantNoProxy := "localhost,127.0.0.1,::1,agent-gateway.internal"
	if agentEnv["NO_PROXY"] != wantNoProxy || agentEnv["no_proxy"] != wantNoProxy {
		t.Fatalf("expected NO_PROXY/no_proxy=%q, got NO_PROXY=%q no_proxy=%q", wantNoProxy, agentEnv["NO_PROXY"], agentEnv["no_proxy"])
	}
	if !strings.Contains(agentEnv["JAVA_TOOL_OPTIONS"], "http.nonProxyHosts=localhost|127.0.0.1|agent-gateway.internal") {
		t.Fatalf("expected JAVA_TOOL_OPTIONS to bypass gateway host, got %q", agentEnv["JAVA_TOOL_OPTIONS"])
	}
	agentProbe := agent["readinessProbe"].(K8sObject)
	if agentProbe["exec"] == nil {
		t.Fatalf("expected agent exec readinessProbe for loopback, got %#v", agentProbe)
	}
	var sawWork, sawTokensRO bool
	for _, vm := range agent["volumeMounts"].([]K8sObject) {
		if vm["name"] == "work" && vm["mountPath"] == SharedWorkPath {
			sawWork = true
		}
		if vm["name"] == "gh-token" && vm["readOnly"] == true {
			sawTokensRO = true
		}
	}
	if !sawWork || !sawTokensRO {
		t.Fatalf("agent mounts missing work=%v or ro tokens=%v", sawWork, sawTokensRO)
	}

	ghAuth := byName["github-auth"]
	if ghAuth["readinessProbe"] == nil {
		t.Fatal("expected github-auth readinessProbe")
	}
	var sawKeyDir bool
	for _, vm := range ghAuth["volumeMounts"].([]K8sObject) {
		if vm["name"] == "gh-app-key" && vm["mountPath"] == "/run/secrets/gh-app" && vm["readOnly"] == true {
			sawKeyDir = true
		}
	}
	if !sawKeyDir {
		t.Fatalf("expected gh-app-key mounted at /run/secrets/gh-app, got %#v", ghAuth["volumeMounts"])
	}
	envMap := map[string]string{}
	for _, e := range ghAuth["env"].([]K8sObject) {
		envMap[e["name"].(string)] = e["value"].(string)
	}
	if envMap["GITHUB_APP_PRIVATE_KEY_PATH"] != "/run/secrets/gh-app/private-key.pem" {
		t.Fatalf("unexpected key path env: %v", envMap)
	}
	if envMap["GITHUB_REPO_NAME"] != "my-org/my-repo" {
		t.Fatalf("unexpected GITHUB_REPO_NAME: %v", envMap)
	}

	// Verify NetworkPolicy includes custom gateway port 8443
	npEgress := bundle.NetworkPolicy["spec"].(K8sObject)["egress"].([]K8sObject)
	portList := npEgress[1]["ports"].([]K8sObject)
	if len(portList) != 2 || portList[1]["port"] != 8443 {
		t.Fatalf("expected NetworkPolicy to allow 443 and 8443, got %#v", portList)
	}
}

func intPtr(v int) *int { return &v }

func TestRenderToolchainMismatch(t *testing.T) {
	dep, tc := sampleInputs()
	tc.Metadata.Name = "python-3.12"
	if _, err := Render(dep, tc, Options{}); err == nil {
		t.Fatal("expected toolchain mismatch error")
	}
}

func TestRenderGitHubAppEnvResolution(t *testing.T) {
	findEnv := func(b *Bundle, name string) (K8sObject, bool) {
		inits := b.AutoscalingRunnerSet["spec"].(K8sObject)["template"].(K8sObject)["spec"].(K8sObject)["initContainers"].([]K8sObject)
		for _, ic := range inits {
			if ic["name"] != "github-auth" {
				continue
			}
			for _, e := range ic["env"].([]K8sObject) {
				if e["name"] == name {
					return e, true
				}
			}
		}
		return nil, false
	}

	t.Run("secret fallback when inline and configmap omitted", func(t *testing.T) {
		dep, tc := sampleInputs()
		dep.Spec.Agent.GitHubAppID = ""
		dep.Spec.Agent.GitHubInstallID = ""

		b, err := Render(dep, tc, Options{})
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		appIDEnv, ok := findEnv(b, "GITHUB_APP_ID")
		if !ok {
			t.Fatal("missing GITHUB_APP_ID")
		}
		secRef := appIDEnv["valueFrom"].(K8sObject)["secretKeyRef"].(K8sObject)
		if secRef["name"] != "review-bot-key" || secRef["key"] != "github_app_id" || secRef["optional"] != true {
			t.Fatalf("unexpected secretKeyRef: %#v", secRef)
		}
	})

	t.Run("configmap takes precedence over secret fallback", func(t *testing.T) {
		dep, tc := sampleInputs()
		dep.Spec.Agent.GitHubAppID = ""
		dep.Spec.Agent.GitHubInstallID = ""
		dep.Spec.Agent.GitHubAppConfigMapRef = &v1alpha1.ConfigMapKeyRef{ConfigMapName: "review-bot-meta"}

		b, err := Render(dep, tc, Options{})
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		installEnv, ok := findEnv(b, "GITHUB_APP_INSTALLATION_ID")
		if !ok {
			t.Fatal("missing GITHUB_APP_INSTALLATION_ID")
		}
		cmRef := installEnv["valueFrom"].(K8sObject)["configMapKeyRef"].(K8sObject)
		if cmRef["name"] != "review-bot-meta" || cmRef["key"] != "github_app_installation_id" || cmRef["optional"] != true {
			t.Fatalf("unexpected configMapKeyRef: %#v", cmRef)
		}
	})
}

func TestRenderModeEgressAndServiceAccount(t *testing.T) {
	dep, tc := sampleInputs()
	dep.Spec.Agent.Mode = v1alpha1.AgentModeSuggest
	dep.Spec.Agent.EgressAllowlist = []string{"staging.internal.example", "proxy.golang.org"}
	dep.Spec.Runner.ServiceAccountName = "deploy-verifier-ro"

	bundle, err := Render(dep, tc, Options{})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	var rtCfg AgentRuntimeConfig
	agentData := bundle.AgentConfigMap["data"].(map[string]string)
	if err := json.Unmarshal([]byte(agentData["config.json"]), &rtCfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if rtCfg.Mode != v1alpha1.AgentModeSuggest {
		t.Fatalf("config.json mode = %q, want %q", rtCfg.Mode, v1alpha1.AgentModeSuggest)
	}

	squidConf := bundle.EgressConfigMap["data"].(map[string]string)["squid.conf"]
	if !strings.Contains(squidConf, "acl allowed_domains dstdomain staging.internal.example\n") {
		t.Fatalf("squid.conf missing deployment egress host:\n%s", squidConf)
	}
	if got := strings.Count(squidConf, "acl allowed_domains dstdomain proxy.golang.org\n"); got != 1 {
		t.Fatalf("expected deduplicated proxy.golang.org ACL, got %d occurrences:\n%s", got, squidConf)
	}

	podSpec := bundle.AutoscalingRunnerSet["spec"].(K8sObject)["template"].(K8sObject)["spec"].(K8sObject)
	if podSpec["serviceAccountName"] != "deploy-verifier-ro" {
		t.Fatalf("serviceAccountName = %v, want deploy-verifier-ro", podSpec["serviceAccountName"])
	}
	var agentMode string
	for _, ic := range podSpec["initContainers"].([]K8sObject) {
		if ic["name"] != "agent" {
			continue
		}
		for _, e := range ic["env"].([]K8sObject) {
			if e["name"] == "AGENTRUNNER_MODE" {
				agentMode, _ = e["value"].(string)
			}
		}
	}
	if agentMode != v1alpha1.AgentModeSuggest {
		t.Fatalf("AGENTRUNNER_MODE = %q, want %q", agentMode, v1alpha1.AgentModeSuggest)
	}
}

func TestRenderProtectedBranches(t *testing.T) {
	t.Run("omitted stays nil in config.json", func(t *testing.T) {
		dep, tc := sampleInputs()
		bundle, err := Render(dep, tc, Options{})
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		var rtCfg AgentRuntimeConfig
		if err := json.Unmarshal([]byte(bundle.AgentConfigMap["data"].(map[string]string)["config.json"]), &rtCfg); err != nil {
			t.Fatalf("unmarshal config.json: %v", err)
		}
		if rtCfg.ProtectedBranches != nil {
			t.Fatalf("expected omitted ProtectedBranches to stay nil, got %v", *rtCfg.ProtectedBranches)
		}
	})

	t.Run("custom list is preserved in config.json", func(t *testing.T) {
		dep, tc := sampleInputs()
		custom := []string{"main", "release"}
		dep.Spec.Agent.ProtectedBranches = &custom
		bundle, err := Render(dep, tc, Options{})
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		var rtCfg AgentRuntimeConfig
		if err := json.Unmarshal([]byte(bundle.AgentConfigMap["data"].(map[string]string)["config.json"]), &rtCfg); err != nil {
			t.Fatalf("unmarshal config.json: %v", err)
		}
		if rtCfg.ProtectedBranches == nil || len(*rtCfg.ProtectedBranches) != 2 || (*rtCfg.ProtectedBranches)[1] != "release" {
			t.Fatalf("expected custom protectedBranches, got %+v", rtCfg.ProtectedBranches)
		}
	})

	t.Run("explicit empty slice renders empty JSON array", func(t *testing.T) {
		dep, tc := sampleInputs()
		empty := []string{}
		dep.Spec.Agent.ProtectedBranches = &empty
		bundle, err := Render(dep, tc, Options{})
		if err != nil {
			t.Fatalf("Render failed: %v", err)
		}
		cfgJSON := bundle.AgentConfigMap["data"].(map[string]string)["config.json"]
		if !strings.Contains(cfgJSON, `"protectedBranches": []`) {
			t.Fatalf("expected explicit empty protectedBranches array in config.json, got:\n%s", cfgJSON)
		}
		var rtCfg AgentRuntimeConfig
		if err := json.Unmarshal([]byte(cfgJSON), &rtCfg); err != nil {
			t.Fatalf("unmarshal config.json: %v", err)
		}
		if rtCfg.ProtectedBranches == nil || len(*rtCfg.ProtectedBranches) != 0 {
			t.Fatalf("expected empty non-nil ProtectedBranches slice, got %+v", rtCfg.ProtectedBranches)
		}
	})
}

func TestRenderSessionSink(t *testing.T) {
	dep, tc := sampleInputs()
	dep.Spec.Agent.SessionSink = &v1alpha1.SessionSinkSpec{
		Endpoint:      "http://session-hub.session-system.svc.cluster.local:8090/v1/sessions",
		AuthSecretRef: &v1alpha1.SecretRef{Name: "session-sink-secret", Key: "bearer"},
	}

	bundle, err := Render(dep, tc, Options{})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	var rtCfg AgentRuntimeConfig
	if err := json.Unmarshal([]byte(bundle.AgentConfigMap["data"].(map[string]string)["config.json"]), &rtCfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if rtCfg.SessionSinkEndpoint != "http://session-hub.session-system.svc.cluster.local:8090/v1/sessions" {
		t.Fatalf("SessionSinkEndpoint = %q, want configured URL", rtCfg.SessionSinkEndpoint)
	}
	if rtCfg.Attribution["sessionKind"] != "x-session-kind" || rtCfg.Attribution["sessionName"] != "x-session-name" {
		t.Fatalf("missing session attribution headers in config.json: %+v", rtCfg.Attribution)
	}

	squidConf := bundle.EgressConfigMap["data"].(map[string]string)["squid.conf"]
	if !strings.Contains(squidConf, "acl allowed_domains dstdomain session-hub.session-system.svc.cluster.local\n") {
		t.Fatalf("squid.conf missing sessionSink domain:\n%s", squidConf)
	}

	egressRules := bundle.NetworkPolicy["spec"].(K8sObject)["egress"].([]K8sObject)
	ports := egressRules[1]["ports"].([]K8sObject)
	has8090 := false
	for _, p := range ports {
		if p["port"] == 8090 {
			has8090 = true
		}
	}
	if !has8090 {
		t.Fatalf("NetworkPolicy missing sessionSink port 8090: %#v", ports)
	}

	findAgentEnv := func(b *Bundle, name string) (K8sObject, bool) {
		inits := b.AutoscalingRunnerSet["spec"].(K8sObject)["template"].(K8sObject)["spec"].(K8sObject)["initContainers"].([]K8sObject)
		for _, ic := range inits {
			if ic["name"] != "agent" {
				continue
			}
			for _, e := range ic["env"].([]K8sObject) {
				if e["name"] == name {
					return e, true
				}
			}
		}
		return nil, false
	}

	if env, ok := findAgentEnv(bundle, "NO_PROXY"); !ok || !strings.Contains(env["value"].(string), "session-hub.session-system.svc.cluster.local") {
		t.Fatalf("NO_PROXY missing sessionSink host: %#v", env)
	}
	tokenEnv, ok := findAgentEnv(bundle, "SESSION_SINK_TOKEN")
	if !ok {
		t.Fatal("missing SESSION_SINK_TOKEN env var")
	}
	secRef := tokenEnv["valueFrom"].(K8sObject)["secretKeyRef"].(K8sObject)
	if secRef["name"] != "session-sink-secret" || secRef["key"] != "bearer" {
		t.Fatalf("unexpected SESSION_SINK_TOKEN secretKeyRef: %#v", secRef)
	}
}

func TestRenderMCPServers(t *testing.T) {
	dep, tc := sampleInputs()
	dep.Spec.Agent.MCPServerRefs = []v1alpha1.LocalRef{{Name: "internal-docs"}, {Name: "local-mcp"}}
	dep.Spec.Agent.MCPServers = []v1alpha1.InlineMCPServerSpec{
		{
			Name: "internal-docs", // override referenced server inline
			AgenticRunnerMCPServerSpec: v1alpha1.AgenticRunnerMCPServerSpec{
				URL:           "http://docs-mcp-v2.platform.svc.cluster.local:8095/mcp",
				AuthSecretRef: &v1alpha1.SecretRef{Name: "docs-mcp-v2-secret"},
				AgentsMD:      "Prefer internal-docs v2 endpoints.",
			},
		},
	}

	available := []*v1alpha1.AgenticRunnerMCPServer{
		{
			Metadata: v1alpha1.ObjectMeta{Name: "internal-docs", Namespace: "actions-runners"},
			Spec: v1alpha1.AgenticRunnerMCPServerSpec{
				URL: "http://docs-mcp-v1.platform.svc.cluster.local:8080/mcp",
			},
		},
		{
			Metadata: v1alpha1.ObjectMeta{Name: "local-mcp", Namespace: "actions-runners"},
			Spec: v1alpha1.AgenticRunnerMCPServerSpec{
				Command:         "/usr/local/bin/local-mcp",
				Args:            []string{"--readonly"},
				EgressAllowlist: []string{"mcp-helper.internal.example"},
			},
		},
	}

	bundle, err := Render(dep, tc, Options{MCPServers: available})
	if err != nil {
		t.Fatalf("Render failed: %v", err)
	}

	agentData := bundle.AgentConfigMap["data"].(map[string]string)
	if !strings.Contains(agentData["18-mcp-internal-docs.md"], "Prefer internal-docs v2 endpoints") {
		t.Fatalf("expected inline override agentsMd in 18-mcp-internal-docs.md, got %q", agentData["18-mcp-internal-docs.md"])
	}

	var rtCfg AgentRuntimeConfig
	if err := json.Unmarshal([]byte(agentData["config.json"]), &rtCfg); err != nil {
		t.Fatalf("unmarshal config.json: %v", err)
	}
	if len(rtCfg.MCPServers) != 2 {
		t.Fatalf("expected 2 resolved MCPServers, got %+v", rtCfg.MCPServers)
	}
	if rtCfg.MCPServers[0].Name != "internal-docs" || rtCfg.MCPServers[0].URL != "http://docs-mcp-v2.platform.svc.cluster.local:8095/mcp" ||
		rtCfg.MCPServers[0].AuthTokenEnv != "AGENTRUNNER_MCP_TOKEN_INTERNAL_DOCS" {
		t.Fatalf("unexpected inline-overridden MCPServer[0]: %+v", rtCfg.MCPServers[0])
	}
	if rtCfg.MCPServers[1].Name != "local-mcp" || rtCfg.MCPServers[1].Command != "/usr/local/bin/local-mcp" {
		t.Fatalf("unexpected MCPServer[1]: %+v", rtCfg.MCPServers[1])
	}

	squidConf := bundle.EgressConfigMap["data"].(map[string]string)["squid.conf"]
	for _, want := range []string{"docs-mcp-v2.platform.svc.cluster.local", "mcp-helper.internal.example"} {
		if !strings.Contains(squidConf, "acl allowed_domains dstdomain "+want+"\n") {
			t.Fatalf("squid.conf missing MCP domain %q:\n%s", want, squidConf)
		}
	}

	egressRules := bundle.NetworkPolicy["spec"].(K8sObject)["egress"].([]K8sObject)
	ports := egressRules[1]["ports"].([]K8sObject)
	has8095 := false
	for _, p := range ports {
		if p["port"] == 8095 {
			has8095 = true
		}
	}
	if !has8095 {
		t.Fatalf("NetworkPolicy missing MCP port 8095: %#v", ports)
	}

	findAgentEnv := func(name string) (K8sObject, bool) {
		inits := bundle.AutoscalingRunnerSet["spec"].(K8sObject)["template"].(K8sObject)["spec"].(K8sObject)["initContainers"].([]K8sObject)
		for _, ic := range inits {
			if ic["name"] != "agent" {
				continue
			}
			for _, e := range ic["env"].([]K8sObject) {
				if e["name"] == name {
					return e, true
				}
			}
		}
		return nil, false
	}
	if env, ok := findAgentEnv("NO_PROXY"); !ok || !strings.Contains(env["value"].(string), "docs-mcp-v2.platform.svc.cluster.local") {
		t.Fatalf("NO_PROXY missing MCP host: %#v", env)
	}
	tokEnv, ok := findAgentEnv("AGENTRUNNER_MCP_TOKEN_INTERNAL_DOCS")
	if !ok {
		t.Fatal("missing AGENTRUNNER_MCP_TOKEN_INTERNAL_DOCS")
	}
	sec := tokEnv["valueFrom"].(K8sObject)["secretKeyRef"].(K8sObject)
	if sec["name"] != "docs-mcp-v2-secret" || sec["key"] != "token" {
		t.Fatalf("unexpected MCP token secretKeyRef: %#v", sec)
	}
}
