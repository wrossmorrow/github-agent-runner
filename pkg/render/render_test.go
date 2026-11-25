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

	if got := len(bundle.Objects()); got != 4 {
		t.Fatalf("expected 4 objects, got %d", got)
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
	for _, want := range []string{"api.github.com", "proxy.golang.org", "agent-gateway.internal", "http_access deny all"} {
		if !strings.Contains(squidConf, want) {
			t.Fatalf("squid.conf missing %q:\n%s", want, squidConf)
		}
	}

	// Verify AutoscalingRunnerSet PodSpec
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
		byName[ic["name"].(string)] = ic
	}

	agent := byName["agent"]
	if agent["image"] != tc.Spec.Image {
		t.Fatalf("agent image = %v, want %v", agent["image"], tc.Spec.Image)
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
