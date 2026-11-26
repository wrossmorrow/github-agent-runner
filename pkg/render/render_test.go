package render

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
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
					MaxTurns:    25,
					MaxDuration: "10m",
					MaxCostUSD:  2.0,
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
	if rtCfg.Model != "claude-sonnet-4-5" || rtCfg.GatewayEndpoint != "https://agent-gateway.internal:8443/v1" {
		t.Fatalf("unexpected runtime config: %+v", rtCfg)
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
