// Package render compiles AgenticRunnerDeployment and AgenticRunnerToolchain
// resources into ARC v2 AutoscalingRunnerSet, ConfigMap, and NetworkPolicy
// Kubernetes manifests.
package render

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
)

const (
	DefaultRunnerImage     = "ghcr.io/actions/actions-runner:latest"
	DefaultProxyImage      = "ubuntu/squid:latest"
	DefaultGitHubAuthImage = "ghcr.io/agentrunner/github-auth:latest"
	DefaultAgentRunnerPort = 8788
	DefaultProxyPort       = 3128
	SharedWorkPath         = "/home/runner/_work"
	SharedFSGroup          = 1001
)

// Options customizes defaults during manifest rendering.
type Options struct {
	DefaultRunnerImage     string
	DefaultProxyImage      string
	DefaultGitHubAuthImage string
	AgentRunnerPort        int
	ProxyPort              int
	ClusterEgressAllowlist []string
}

func (o Options) withDefaults() Options {
	if o.DefaultRunnerImage == "" {
		o.DefaultRunnerImage = DefaultRunnerImage
	}
	if o.DefaultProxyImage == "" {
		o.DefaultProxyImage = DefaultProxyImage
	}
	if o.DefaultGitHubAuthImage == "" {
		o.DefaultGitHubAuthImage = DefaultGitHubAuthImage
	}
	if o.AgentRunnerPort == 0 {
		o.AgentRunnerPort = DefaultAgentRunnerPort
	}
	if o.ProxyPort == 0 {
		o.ProxyPort = DefaultProxyPort
	}
	if len(o.ClusterEgressAllowlist) == 0 {
		o.ClusterEgressAllowlist = []string{
			"github.com",
			"api.github.com",
			"objects.githubusercontent.com",
		}
	}
	return o
}

// K8sObject is a generic Kubernetes resource map suitable for JSON/YAML output
// and `kubectl apply`.
type K8sObject = map[string]any

// Bundle holds all Kubernetes manifests derived from one deployment + toolchain.
type Bundle struct {
	AgentConfigMap       K8sObject
	EgressConfigMap      K8sObject
	AutoscalingRunnerSet K8sObject
	NetworkPolicy        K8sObject
}

// Objects returns the bundle resources in recommended apply order.
func (b *Bundle) Objects() []K8sObject {
	return []K8sObject{
		b.AgentConfigMap,
		b.EgressConfigMap,
		b.NetworkPolicy,
		b.AutoscalingRunnerSet,
	}
}

// AgentRuntimeConfig is the JSON structure written to /etc/agentrunner/config.json
// and read by agentrunnerd inside the agent sidecar.
type AgentRuntimeConfig struct {
	Harness         string              `json:"harness"`
	Provider        string              `json:"provider,omitempty"`
	Model           string              `json:"model"`
	GatewayEndpoint string              `json:"gatewayEndpoint"`
	Limits          v1alpha1.LimitsSpec `json:"limits,omitempty"`
	Attribution     map[string]string   `json:"attributionHeaders,omitempty"`
}

// Render validates dep and tc and builds the complete Kubernetes resource bundle.
func Render(dep *v1alpha1.AgenticRunnerDeployment, tc *v1alpha1.AgenticRunnerToolchain, opts Options) (*Bundle, error) {
	if dep == nil || tc == nil {
		return nil, fmt.Errorf("deployment and toolchain must both be non-nil")
	}
	if err := dep.Validate(); err != nil {
		return nil, fmt.Errorf("invalid deployment: %w", err)
	}
	if err := tc.Validate(); err != nil {
		return nil, fmt.Errorf("invalid toolchain: %w", err)
	}
	if dep.Spec.ToolchainRef.Name != tc.Metadata.Name {
		return nil, fmt.Errorf("deployment references toolchain %q, got %q", dep.Spec.ToolchainRef.Name, tc.Metadata.Name)
	}

	opts = opts.withDefaults()
	ns := dep.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}

	agentCMName := dep.Metadata.Name + "-agent-config"
	egressCMName := dep.Metadata.Name + "-egress-config"

	agentCM, err := buildAgentConfigMap(dep, tc, ns, agentCMName)
	if err != nil {
		return nil, err
	}
	egressCM, gatewayPort := buildEgressConfigMap(dep, tc, ns, egressCMName, opts)
	runnerSet := buildAutoscalingRunnerSet(dep, tc, ns, agentCMName, egressCMName, opts)
	netPol := buildNetworkPolicy(dep, ns, gatewayPort)

	return &Bundle{
		AgentConfigMap:       agentCM,
		EgressConfigMap:      egressCM,
		AutoscalingRunnerSet: runnerSet,
		NetworkPolicy:        netPol,
	}, nil
}

func buildAgentConfigMap(dep *v1alpha1.AgenticRunnerDeployment, tc *v1alpha1.AgenticRunnerToolchain, ns, name string) (K8sObject, error) {
	harness := dep.Spec.Agent.Harness
	if harness == "" {
		harness = "pi"
	}
	cfg := AgentRuntimeConfig{
		Harness:         harness,
		Provider:        dep.Spec.Agent.Provider,
		Model:           dep.Spec.Agent.Model,
		GatewayEndpoint: dep.Spec.Agent.Gateway.Endpoint,
		Limits:          dep.Spec.Agent.Limits,
		Attribution: map[string]string{
			"sessionId": "x-session-id",
			"repo":      "x-github-repo",
			"runId":     "x-github-run-id",
			"pr":        "x-github-pr",
		},
	}
	cfgBytes, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal agent config: %w", err)
	}

	data := map[string]string{
		"config.json": string(cfgBytes) + "\n",
	}
	if strings.TrimSpace(tc.Spec.AgentsMD) != "" {
		data["15-toolchain-crd.md"] = strings.TrimSpace(tc.Spec.AgentsMD) + "\n"
	}
	if strings.TrimSpace(dep.Spec.Agent.AgentsMD) != "" {
		data["20-deployment.md"] = strings.TrimSpace(dep.Spec.Agent.AgentsMD) + "\n"
	}

	return K8sObject{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": K8sObject{
			"name":      name,
			"namespace": ns,
			"labels":    commonLabels(dep),
		},
		"data": data,
	}, nil
}

func buildEgressConfigMap(dep *v1alpha1.AgenticRunnerDeployment, tc *v1alpha1.AgenticRunnerToolchain, ns, name string, opts Options) (K8sObject, int) {
	seen := map[string]struct{}{}
	var domains []string
	addHost := func(h string) {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			return
		}
		if _, ok := seen[h]; ok {
			return
		}
		seen[h] = struct{}{}
		domains = append(domains, h)
	}

	for _, h := range opts.ClusterEgressAllowlist {
		addHost(h)
	}
	for _, h := range tc.Spec.EgressAllowlist {
		addHost(h)
	}

	gatewayPort := 443
	if u, err := url.Parse(dep.Spec.Agent.Gateway.Endpoint); err == nil {
		addHost(u.Hostname())
		if p := u.Port(); p != "" {
			if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
				gatewayPort = parsed
			}
		} else if u.Scheme == "http" {
			gatewayPort = 80
		}
	}

	sort.Strings(domains)

	var b strings.Builder
	fmt.Fprintf(&b, "http_port 127.0.0.1:%d\n", opts.ProxyPort)
	b.WriteString("access_log stdio:/dev/stdout\n")
	b.WriteString("cache_log stdio:/dev/stderr\n")
	b.WriteString("cache deny all\n")
	for _, d := range domains {
		fmt.Fprintf(&b, "acl allowed_domains dstdomain %s\n", d)
	}
	b.WriteString("http_access allow allowed_domains\n")
	b.WriteString("http_access deny all\n")

	return K8sObject{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": K8sObject{
			"name":      name,
			"namespace": ns,
			"labels":    commonLabels(dep),
		},
		"data": map[string]string{
			"squid.conf": b.String(),
		},
	}, gatewayPort
}

func buildAutoscalingRunnerSet(
	dep *v1alpha1.AgenticRunnerDeployment,
	tc *v1alpha1.AgenticRunnerToolchain,
	ns, agentCMName, egressCMName string,
	opts Options,
) K8sObject {
	runnerImage := dep.Spec.Runner.RunnerImage
	if runnerImage == "" {
		runnerImage = opts.DefaultRunnerImage
	}

	volumes := []K8sObject{
		{"name": "work", "emptyDir": K8sObject{}},
		{"name": "gh-token", "emptyDir": K8sObject{"medium": "Memory"}},
		{"name": "agent-config", "configMap": K8sObject{"name": agentCMName}},
		{"name": "egress-config", "configMap": K8sObject{"name": egressCMName}},
	}

	proxyAddr := fmt.Sprintf("http://127.0.0.1:%d", opts.ProxyPort)
	noProxy := "localhost,127.0.0.1,::1"
	agentEnv := []K8sObject{
		{"name": "HTTP_PROXY", "value": proxyAddr},
		{"name": "HTTPS_PROXY", "value": proxyAddr},
		{"name": "NO_PROXY", "value": noProxy},
		{"name": "http_proxy", "value": proxyAddr},
		{"name": "https_proxy", "value": proxyAddr},
		{"name": "no_proxy", "value": noProxy},
		{
			"name":  "JAVA_TOOL_OPTIONS",
			"value": fmt.Sprintf("-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=%d -Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=%d -Dhttp.nonProxyHosts=localhost|127.0.0.1", opts.ProxyPort, opts.ProxyPort),
		},
		{"name": "AGENTRUNNER_ADDR", "value": fmt.Sprintf("127.0.0.1:%d", opts.AgentRunnerPort)},
		{"name": "AGENTRUNNER_CONFIG", "value": "/etc/agentrunner/config.json"},
		{"name": "PI_CODING_AGENT_DIR", "value": "/home/node/.pi/agent"},
	}

	if dep.Spec.Agent.Gateway.AuthSecretRef != nil {
		key := dep.Spec.Agent.Gateway.AuthSecretRef.Key
		if key == "" {
			key = "token"
		}
		agentEnv = append(agentEnv, K8sObject{
			"name": "AGENT_GATEWAY_TOKEN",
			"valueFrom": K8sObject{
				"secretKeyRef": K8sObject{
					"name": dep.Spec.Agent.Gateway.AuthSecretRef.Name,
					"key":  key,
				},
			},
		})
	}

	agentMounts := []K8sObject{
		{"name": "work", "mountPath": SharedWorkPath},
		{"name": "gh-token", "mountPath": "/tokens", "readOnly": true},
		{"name": "agent-config", "mountPath": "/etc/agentrunner", "readOnly": true},
	}

	if dep.Spec.Agent.SystemPromptRef != nil {
		volumes = append(volumes, K8sObject{
			"name":      "system-prompt",
			"configMap": K8sObject{"name": dep.Spec.Agent.SystemPromptRef.ConfigMapName},
		})
		agentMounts = append(agentMounts, K8sObject{
			"name":      "system-prompt",
			"mountPath": "/etc/agentrunner/prompts",
			"readOnly":  true,
		})
	}

	agentSidecar := K8sObject{
		"name":          "agent",
		"image":         tc.Spec.Image,
		"restartPolicy": "Always",
		"command":       []string{"/usr/local/bin/agentrunnerd"},
		"env":           agentEnv,
		"volumeMounts":  agentMounts,
		"readinessProbe": K8sObject{
			"httpGet": K8sObject{
				"path": "/healthz",
				"port": opts.AgentRunnerPort,
			},
			"initialDelaySeconds": 1,
			"periodSeconds":       2,
		},
	}
	if len(tc.Spec.Resources.Requests) > 0 || len(tc.Spec.Resources.Limits) > 0 {
		res := K8sObject{}
		if len(tc.Spec.Resources.Requests) > 0 {
			res["requests"] = tc.Spec.Resources.Requests
		}
		if len(tc.Spec.Resources.Limits) > 0 {
			res["limits"] = tc.Spec.Resources.Limits
		}
		agentSidecar["resources"] = res
	}

	egressSidecar := K8sObject{
		"name":          "egress-proxy",
		"image":         opts.DefaultProxyImage,
		"restartPolicy": "Always",
		"command":       []string{"squid", "-N", "-f", "/etc/squid/conf.d/squid.conf"},
		"volumeMounts": []K8sObject{
			{"name": "egress-config", "mountPath": "/etc/squid/conf.d", "readOnly": true},
		},
	}

	ghAuthMounts := []K8sObject{
		{"name": "gh-token", "mountPath": "/tokens"},
	}
	if dep.Spec.Agent.GitHubAppSecretRef != nil {
		volumes = append(volumes, K8sObject{
			"name":   "gh-app-key",
			"secret": K8sObject{"secretName": dep.Spec.Agent.GitHubAppSecretRef.Name},
		})
		ghAuthMounts = append(ghAuthMounts, K8sObject{
			"name":      "gh-app-key",
			"mountPath": "/run/secrets/gh_private_key",
			"readOnly":  true,
		})
	}
	var ghAuthEnv []K8sObject
	if dep.Spec.Agent.GitHubAppID != "" {
		ghAuthEnv = append(ghAuthEnv, K8sObject{"name": "GITHUB_APP_ID", "value": dep.Spec.Agent.GitHubAppID})
	}
	if dep.Spec.Agent.GitHubInstallID != "" {
		ghAuthEnv = append(ghAuthEnv, K8sObject{"name": "GITHUB_APP_INSTALLATION_ID", "value": dep.Spec.Agent.GitHubInstallID})
	}

	ghAuthSidecar := K8sObject{
		"name":          "github-auth",
		"image":         opts.DefaultGitHubAuthImage,
		"restartPolicy": "Always",
		"volumeMounts":  ghAuthMounts,
	}
	if len(ghAuthEnv) > 0 {
		ghAuthSidecar["env"] = ghAuthEnv
	}

	runnerSpec := K8sObject{
		"githubConfigUrl":    dep.Spec.Runner.GitHubConfigURL,
		"runnerScaleSetName": dep.Spec.Runner.RunnerScaleSetName,
		"minRunners":         dep.Spec.Runner.MinRunners,
		"maxRunners":         dep.Spec.Runner.MaxRunners,
		"template": K8sObject{
			"metadata": K8sObject{
				"labels": commonLabels(dep),
			},
			"spec": K8sObject{
				"securityContext": K8sObject{
					"fsGroup": SharedFSGroup,
				},
				"initContainers": []K8sObject{
					egressSidecar,
					ghAuthSidecar,
					agentSidecar,
				},
				"containers": []K8sObject{
					{
						"name":  "runner",
						"image": runnerImage,
						"volumeMounts": []K8sObject{
							{"name": "work", "mountPath": SharedWorkPath},
						},
					},
				},
				"volumes": volumes,
			},
		},
	}
	if dep.Spec.Runner.GitHubConfigSecret != "" {
		runnerSpec["githubConfigSecret"] = dep.Spec.Runner.GitHubConfigSecret
	}

	return K8sObject{
		"apiVersion": "actions.github.com/v1alpha1",
		"kind":       "AutoscalingRunnerSet",
		"metadata": K8sObject{
			"name":      dep.Spec.Runner.RunnerScaleSetName,
			"namespace": ns,
			"labels":    commonLabels(dep),
		},
		"spec": runnerSpec,
	}
}

func buildNetworkPolicy(dep *v1alpha1.AgenticRunnerDeployment, ns string, gatewayPort int) K8sObject {
	ports := []K8sObject{
		{"protocol": "TCP", "port": 443},
	}
	if gatewayPort != 443 && gatewayPort > 0 {
		ports = append(ports, K8sObject{"protocol": "TCP", "port": gatewayPort})
	}

	return K8sObject{
		"apiVersion": "networking.k8s.io/v1",
		"kind":       "NetworkPolicy",
		"metadata": K8sObject{
			"name":      dep.Metadata.Name + "-egress",
			"namespace": ns,
			"labels":    commonLabels(dep),
		},
		"spec": K8sObject{
			"podSelector": K8sObject{
				"matchLabels": map[string]string{
					"agentic.example.io/deployment": dep.Metadata.Name,
				},
			},
			"policyTypes": []string{"Egress"},
			"egress": []K8sObject{
				{
					"ports": []K8sObject{
						{"protocol": "UDP", "port": 53},
						{"protocol": "TCP", "port": 53},
					},
				},
				{
					"ports": ports,
				},
			},
		},
	}
}

func commonLabels(dep *v1alpha1.AgenticRunnerDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":  "agentrunner",
		"agentic.example.io/deployment": dep.Metadata.Name,
		"agentic.example.io/toolchain":  dep.Spec.ToolchainRef.Name,
		"actions.github.com/scale-set":  dep.Spec.Runner.RunnerScaleSetName,
	}
}
