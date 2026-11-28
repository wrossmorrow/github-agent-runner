// Package render compiles AgenticRunnerDeployment and AgenticRunnerToolchain
// resources into ARC v2 AutoscalingRunnerSet, ConfigMap, and NetworkPolicy
// Kubernetes manifests.
package render

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/wrossmorrow/github-agent-runner/api/v1alpha1"
)

const (
	DefaultRunnerImage            = "ghcr.io/actions/actions-runner:latest"
	DefaultProxyImage             = "ubuntu/squid:latest"
	DefaultGitHubAuthImage        = "ghcr.io/agentrunner/github-auth:latest"
	DefaultImagePullPolicy        = "IfNotPresent"
	DefaultARCVersion             = "0.15.1"
	DefaultARCControllerNamespace = "arc-systems"
	DefaultARCControllerSA        = "arc-gha-rs-controller"
	DefaultAgentRunnerPort        = 8788
	DefaultProxyPort              = 3128
	SharedWorkPath                = "/home/runner/_work"
	SharedFSGroup                 = 1001
)

// Options customizes defaults during manifest rendering.
type Options struct {
	DefaultRunnerImage          string
	DefaultProxyImage           string
	DefaultGitHubAuthImage      string
	ImagePullPolicy             string
	ARCVersion                  string
	ARCControllerNamespace      string
	ARCControllerServiceAccount string
	AgentRunnerPort             int
	ProxyPort                   int
	ClusterEgressAllowlist      []string
	MCPServers                  []*v1alpha1.AgenticRunnerMCPServer
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
	if o.ImagePullPolicy == "" {
		o.ImagePullPolicy = DefaultImagePullPolicy
	}
	if o.ARCVersion == "" {
		o.ARCVersion = DefaultARCVersion
	}
	if o.ARCControllerNamespace == "" {
		o.ARCControllerNamespace = DefaultARCControllerNamespace
	}
	if o.ARCControllerServiceAccount == "" {
		o.ARCControllerServiceAccount = DefaultARCControllerSA
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
	AgentConfigMap           K8sObject
	EgressConfigMap          K8sObject
	ARCControllerRole        K8sObject
	ARCControllerRoleBinding K8sObject
	AutoscalingRunnerSet     K8sObject
	NetworkPolicy            K8sObject
}

// Objects returns the bundle resources in recommended apply order.
func (b *Bundle) Objects() []K8sObject {
	return []K8sObject{
		b.AgentConfigMap,
		b.EgressConfigMap,
		b.NetworkPolicy,
		b.ARCControllerRole,
		b.ARCControllerRoleBinding,
		b.AutoscalingRunnerSet,
	}
}

// List wraps Objects in a Kubernetes v1 List suitable for `kubectl apply -f -`.
func (b *Bundle) List() K8sObject {
	return K8sObject{
		"apiVersion": "v1",
		"kind":       "List",
		"items":      b.Objects(),
	}
}

// AgentRuntimeConfig is the JSON structure written to /etc/agentrunner/config.json
// and read by agentrunnerd inside the agent sidecar.
type AgentRuntimeConfig struct {
	Harness             string              `json:"harness"`
	Mode                string              `json:"mode,omitempty"`
	Provider            string              `json:"provider,omitempty"`
	Model               string              `json:"model"`
	GatewayEndpoint     string              `json:"gatewayEndpoint"`
	SessionSinkEndpoint string              `json:"sessionSinkEndpoint,omitempty"`
	Limits              v1alpha1.LimitsSpec `json:"limits,omitempty"`
	ProtectedBranches   *[]string           `json:"protectedBranches,omitempty"`
	MCPServers          []RuntimeMCPServer  `json:"mcpServers,omitempty"`
	Attribution         map[string]string   `json:"attributionHeaders,omitempty"`
}

// RuntimeMCPServer describes a resolved MCP server written into config.json for agentrunnerd.
type RuntimeMCPServer struct {
	Name         string            `json:"name"`
	URL          string            `json:"url,omitempty"`
	Command      string            `json:"command,omitempty"`
	Args         []string          `json:"args,omitempty"`
	Headers      map[string]string `json:"headers,omitempty"`
	AuthTokenEnv string            `json:"authTokenEnv,omitempty"`
}

type resolvedMCPServer struct {
	runtime         RuntimeMCPServer
	authSecretRef   *v1alpha1.SecretRef
	egressAllowlist []string
	agentsMD        string
}

func mcpTokenEnvName(name string) string {
	normalized := strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
	return "AGENTRUNNER_MCP_TOKEN_" + normalized
}

func resolveMCPServers(dep *v1alpha1.AgenticRunnerDeployment, available []*v1alpha1.AgenticRunnerMCPServer) ([]resolvedMCPServer, error) {
	byName := make(map[string]*v1alpha1.AgenticRunnerMCPServer, len(available))
	for _, m := range available {
		if m == nil {
			continue
		}
		if err := m.Validate(); err != nil {
			return nil, fmt.Errorf("invalid AgenticRunnerMCPServer %q: %w", m.Metadata.Name, err)
		}
		byName[m.Metadata.Name] = m
	}

	toResolved := func(name string, spec v1alpha1.AgenticRunnerMCPServerSpec) resolvedMCPServer {
		var authEnv string
		if spec.AuthSecretRef != nil {
			authEnv = mcpTokenEnvName(name)
		}
		return resolvedMCPServer{
			runtime: RuntimeMCPServer{
				Name:         name,
				URL:          strings.TrimSpace(spec.URL),
				Command:      strings.TrimSpace(spec.Command),
				Args:         spec.Args,
				Headers:      spec.Headers,
				AuthTokenEnv: authEnv,
			},
			authSecretRef:   spec.AuthSecretRef,
			egressAllowlist: spec.EgressAllowlist,
			agentsMD:        spec.AgentsMD,
		}
	}

	var out []resolvedMCPServer
	idxByName := map[string]int{}
	upsert := func(item resolvedMCPServer) {
		if idx, exists := idxByName[item.runtime.Name]; exists {
			out[idx] = item
			return
		}
		idxByName[item.runtime.Name] = len(out)
		out = append(out, item)
	}

	for _, ref := range dep.Spec.Agent.MCPServerRefs {
		m, ok := byName[ref.Name]
		if !ok {
			return nil, fmt.Errorf("referenced AgenticRunnerMCPServer %q not provided", ref.Name)
		}
		upsert(toResolved(ref.Name, m.Spec))
	}
	for _, inline := range dep.Spec.Agent.MCPServers {
		upsert(toResolved(inline.Name, inline.AgenticRunnerMCPServerSpec))
	}
	seenTokenEnv := map[string]string{}
	for _, m := range out {
		envName := mcpTokenEnvName(m.runtime.Name)
		if prev, dup := seenTokenEnv[envName]; dup && prev != m.runtime.Name {
			return nil, fmt.Errorf("MCP servers %q and %q collide on token env var %s", prev, m.runtime.Name, envName)
		}
		seenTokenEnv[envName] = m.runtime.Name
	}
	return out, nil
}

func effectiveMode(dep *v1alpha1.AgenticRunnerDeployment) string {
	if dep.Spec.Agent.Mode != "" {
		return dep.Spec.Agent.Mode
	}
	return v1alpha1.AgentModeReadOnly
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

	mcps, err := resolveMCPServers(dep, opts.MCPServers)
	if err != nil {
		return nil, err
	}

	opts = opts.withDefaults()
	ns := dep.Metadata.Namespace
	if ns == "" {
		ns = "default"
	}

	agentCMName := dep.Metadata.Name + "-agent-config"
	egressCMName := dep.Metadata.Name + "-egress-config"

	agentCM, err := buildAgentConfigMap(dep, tc, mcps, ns, agentCMName)
	if err != nil {
		return nil, err
	}
	egressCM, endpointPorts := buildEgressConfigMap(dep, tc, mcps, ns, egressCMName, opts)
	arcRole, arcRoleBinding := buildARCRBAC(dep, ns, opts)
	runnerSet := buildAutoscalingRunnerSet(dep, tc, mcps, ns, agentCMName, egressCMName, opts)
	netPol := buildNetworkPolicy(dep, ns, endpointPorts)

	return &Bundle{
		AgentConfigMap:           agentCM,
		EgressConfigMap:          egressCM,
		ARCControllerRole:        arcRole,
		ARCControllerRoleBinding: arcRoleBinding,
		AutoscalingRunnerSet:     runnerSet,
		NetworkPolicy:            netPol,
	}, nil
}

func buildAgentConfigMap(dep *v1alpha1.AgenticRunnerDeployment, tc *v1alpha1.AgenticRunnerToolchain, mcps []resolvedMCPServer, ns, name string) (K8sObject, error) {
	harness := dep.Spec.Agent.Harness
	if harness == "" {
		harness = "pi"
	}
	var sessionSinkEndpoint string
	if dep.Spec.Agent.SessionSink != nil {
		sessionSinkEndpoint = dep.Spec.Agent.SessionSink.Endpoint
	}
	var runtimeMCPs []RuntimeMCPServer
	for _, m := range mcps {
		runtimeMCPs = append(runtimeMCPs, m.runtime)
	}
	cfg := AgentRuntimeConfig{
		Harness:             harness,
		Mode:                effectiveMode(dep),
		Provider:            dep.Spec.Agent.Provider,
		Model:               dep.Spec.Agent.Model,
		GatewayEndpoint:     dep.Spec.Agent.Gateway.Endpoint,
		SessionSinkEndpoint: sessionSinkEndpoint,
		Limits:              dep.Spec.Agent.Limits,
		ProtectedBranches:   dep.Spec.Agent.ProtectedBranches,
		MCPServers:          runtimeMCPs,
		Attribution: map[string]string{
			"sessionId":   "x-session-id",
			"sessionKind": "x-session-kind",
			"sessionName": "x-session-name",
			"repo":        "x-github-repo",
			"runId":       "x-github-run-id",
			"pr":          "x-github-pr",
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
	for _, m := range mcps {
		if strings.TrimSpace(m.agentsMD) != "" {
			data["18-mcp-"+m.runtime.Name+".md"] = strings.TrimSpace(m.agentsMD) + "\n"
		}
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

func buildEgressConfigMap(dep *v1alpha1.AgenticRunnerDeployment, tc *v1alpha1.AgenticRunnerToolchain, mcps []resolvedMCPServer, ns, name string, opts Options) (K8sObject, []int) {
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
	for _, h := range dep.Spec.Agent.EgressAllowlist {
		addHost(h)
	}
	for _, m := range mcps {
		for _, h := range m.egressAllowlist {
			addHost(h)
		}
	}

	var ports []int
	seenPorts := map[int]struct{}{}
	addEndpoint := func(rawURL string) {
		u, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil || u.Host == "" {
			return
		}
		addHost(u.Hostname())
		port := 443
		if p := u.Port(); p != "" {
			if parsed, err := strconv.Atoi(p); err == nil && parsed > 0 {
				port = parsed
			}
		} else if u.Scheme == "http" {
			port = 80
		}
		if _, ok := seenPorts[port]; !ok {
			seenPorts[port] = struct{}{}
			ports = append(ports, port)
		}
	}

	addEndpoint(dep.Spec.Agent.Gateway.Endpoint)
	if dep.Spec.Agent.SessionSink != nil {
		addEndpoint(dep.Spec.Agent.SessionSink.Endpoint)
	}
	for _, m := range mcps {
		if m.runtime.URL != "" {
			addEndpoint(m.runtime.URL)
		}
	}

	sort.Strings(domains)
	sort.Ints(ports)

	var b strings.Builder
	fmt.Fprintf(&b, "http_port 127.0.0.1:%d\n", opts.ProxyPort)
	b.WriteString("max_filedescriptors 1024\n")
	b.WriteString("pid_filename /tmp/squid.pid\n")
	b.WriteString("access_log stdio:/var/log/squid/access.log\n")
	b.WriteString("cache_log /var/log/squid/cache.log\n")
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
	}, ports
}

func buildAutoscalingRunnerSet(
	dep *v1alpha1.AgenticRunnerDeployment,
	tc *v1alpha1.AgenticRunnerToolchain,
	mcps []resolvedMCPServer,
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
	noProxyHosts := []string{"localhost", "127.0.0.1", "::1"}
	javaNonProxyHosts := []string{"localhost", "127.0.0.1"}
	seenNoProxy := map[string]struct{}{"localhost": {}, "127.0.0.1": {}, "::1": {}}
	addNoProxy := func(rawURL string) {
		h := gatewayHost(rawURL)
		if h == "" || !isInternalHost(h) {
			return
		}
		if _, ok := seenNoProxy[h]; ok {
			return
		}
		seenNoProxy[h] = struct{}{}
		noProxyHosts = append(noProxyHosts, h)
		javaNonProxyHosts = append(javaNonProxyHosts, h)
	}
	addNoProxy(dep.Spec.Agent.Gateway.Endpoint)
	if dep.Spec.Agent.SessionSink != nil {
		addNoProxy(dep.Spec.Agent.SessionSink.Endpoint)
	}
	for _, m := range mcps {
		if m.runtime.URL != "" {
			addNoProxy(m.runtime.URL)
		}
	}
	noProxy := strings.Join(noProxyHosts, ",")
	agentEnv := []K8sObject{
		{"name": "HTTP_PROXY", "value": proxyAddr},
		{"name": "HTTPS_PROXY", "value": proxyAddr},
		{"name": "NO_PROXY", "value": noProxy},
		{"name": "http_proxy", "value": proxyAddr},
		{"name": "https_proxy", "value": proxyAddr},
		{"name": "no_proxy", "value": noProxy},
		{
			"name":  "JAVA_TOOL_OPTIONS",
			"value": fmt.Sprintf("-Dhttp.proxyHost=127.0.0.1 -Dhttp.proxyPort=%d -Dhttps.proxyHost=127.0.0.1 -Dhttps.proxyPort=%d -Dhttp.nonProxyHosts=%s", opts.ProxyPort, opts.ProxyPort, strings.Join(javaNonProxyHosts, "|")),
		},
		{"name": "AGENTRUNNER_ADDR", "value": fmt.Sprintf("127.0.0.1:%d", opts.AgentRunnerPort)},
		{"name": "AGENTRUNNER_CONFIG", "value": "/etc/agentrunner/config.json"},
		{"name": "AGENTRUNNER_MODE", "value": effectiveMode(dep)},
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
	if dep.Spec.Agent.SessionSink != nil && dep.Spec.Agent.SessionSink.AuthSecretRef != nil {
		key := dep.Spec.Agent.SessionSink.AuthSecretRef.Key
		if key == "" {
			key = "token"
		}
		agentEnv = append(agentEnv, K8sObject{
			"name": "SESSION_SINK_TOKEN",
			"valueFrom": K8sObject{
				"secretKeyRef": K8sObject{
					"name": dep.Spec.Agent.SessionSink.AuthSecretRef.Name,
					"key":  key,
				},
			},
		})
	}
	for _, m := range mcps {
		if m.authSecretRef == nil {
			continue
		}
		key := m.authSecretRef.Key
		if key == "" {
			key = "token"
		}
		agentEnv = append(agentEnv, K8sObject{
			"name": m.runtime.AuthTokenEnv,
			"valueFrom": K8sObject{
				"secretKeyRef": K8sObject{
					"name": m.authSecretRef.Name,
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
		"name":            "agent",
		"image":           tc.Spec.Image,
		"imagePullPolicy": opts.ImagePullPolicy,
		"restartPolicy":   "Always",
		"command":         []string{"/usr/local/bin/agentrunnerd"},
		"env":             agentEnv,
		"volumeMounts":    agentMounts,
		"readinessProbe": K8sObject{
			"exec": K8sObject{
				"command": []string{"curl", "-fsS", fmt.Sprintf("http://127.0.0.1:%d/healthz", opts.AgentRunnerPort)},
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
		"name":            "egress-proxy",
		"image":           opts.DefaultProxyImage,
		"imagePullPolicy": opts.ImagePullPolicy,
		"restartPolicy":   "Always",
		"command":         []string{"squid", "-N", "-f", "/etc/squid/conf.d/squid.conf"},
		"volumeMounts": []K8sObject{
			{"name": "egress-config", "mountPath": "/etc/squid/conf.d", "readOnly": true},
		},
	}

	ghAuthMounts := []K8sObject{
		{"name": "gh-token", "mountPath": "/tokens"},
	}
	var ghAuthEnv []K8sObject
	if dep.Spec.Agent.GitHubAppSecretRef != nil {
		volumes = append(volumes, K8sObject{
			"name":   "gh-app-key",
			"secret": K8sObject{"secretName": dep.Spec.Agent.GitHubAppSecretRef.Name},
		})
		ghAuthMounts = append(ghAuthMounts, K8sObject{
			"name":      "gh-app-key",
			"mountPath": "/run/secrets/gh-app",
			"readOnly":  true,
		})
		keyFile := dep.Spec.Agent.GitHubAppSecretRef.Key
		if keyFile == "" {
			keyFile = "private-key.pem"
		}
		ghAuthEnv = append(ghAuthEnv, K8sObject{
			"name":  "GITHUB_APP_PRIVATE_KEY_PATH",
			"value": "/run/secrets/gh-app/" + keyFile,
		})
	}
	if envObj, ok := resolveGitHubAppEnv("GITHUB_APP_ID", dep.Spec.Agent.GitHubAppID, "github_app_id", dep.Spec.Agent); ok {
		ghAuthEnv = append(ghAuthEnv, envObj)
	}
	if envObj, ok := resolveGitHubAppEnv("GITHUB_APP_INSTALLATION_ID", dep.Spec.Agent.GitHubInstallID, "github_app_installation_id", dep.Spec.Agent); ok {
		ghAuthEnv = append(ghAuthEnv, envObj)
	}
	if repoName := extractRepoName(dep.Spec.Runner.GitHubConfigURL); repoName != "" {
		ghAuthEnv = append(ghAuthEnv, K8sObject{"name": "GITHUB_REPO_NAME", "value": repoName})
	}

	ghAuthSidecar := K8sObject{
		"name":            "github-auth",
		"image":           opts.DefaultGitHubAuthImage,
		"imagePullPolicy": opts.ImagePullPolicy,
		"restartPolicy":   "Always",
		"volumeMounts":    ghAuthMounts,
		"readinessProbe": K8sObject{
			"exec": K8sObject{
				"command": []string{"test", "-s", "/tokens/token"},
			},
			"initialDelaySeconds": 1,
			"periodSeconds":       2,
		},
	}
	if len(ghAuthEnv) > 0 {
		ghAuthSidecar["env"] = ghAuthEnv
	}

	podSpec := K8sObject{
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
				"name":    "runner",
				"image":   runnerImage,
				"command": []string{"/home/runner/run.sh"},
				"volumeMounts": []K8sObject{
					{"name": "work", "mountPath": SharedWorkPath},
				},
			},
		},
		"volumes": volumes,
	}
	if sa := strings.TrimSpace(dep.Spec.Runner.ServiceAccountName); sa != "" {
		podSpec["serviceAccountName"] = sa
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
			"spec": podSpec,
		},
	}
	if dep.Spec.Runner.GitHubConfigSecret != "" {
		runnerSpec["githubConfigSecret"] = dep.Spec.Runner.GitHubConfigSecret
	}

	labels := commonLabels(dep)
	labels["app.kubernetes.io/version"] = opts.ARCVersion
	labels["app.kubernetes.io/part-of"] = "gha-rs"
	labels["app.kubernetes.io/component"] = "runner-scale-set"

	return K8sObject{
		"apiVersion": "actions.github.com/v1alpha1",
		"kind":       "AutoscalingRunnerSet",
		"metadata": K8sObject{
			"name":      dep.Spec.Runner.RunnerScaleSetName,
			"namespace": ns,
			"labels":    labels,
		},
		"spec": runnerSpec,
	}
}

func resolveGitHubAppEnv(envName, inlineVal, key string, agent v1alpha1.AgentSpec) (K8sObject, bool) {
	if inlineVal != "" {
		return K8sObject{"name": envName, "value": inlineVal}, true
	}
	if agent.GitHubAppConfigMapRef != nil && agent.GitHubAppConfigMapRef.ConfigMapName != "" {
		return K8sObject{
			"name": envName,
			"valueFrom": K8sObject{
				"configMapKeyRef": K8sObject{
					"name":     agent.GitHubAppConfigMapRef.ConfigMapName,
					"key":      key,
					"optional": true,
				},
			},
		}, true
	}
	if agent.GitHubAppSecretRef != nil && agent.GitHubAppSecretRef.Name != "" {
		return K8sObject{
			"name": envName,
			"valueFrom": K8sObject{
				"secretKeyRef": K8sObject{
					"name":     agent.GitHubAppSecretRef.Name,
					"key":      key,
					"optional": true,
				},
			},
		}, true
	}
	return nil, false
}

func gatewayHost(endpoint string) string {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(u.Hostname()))
}

func isInternalHost(host string) bool {
	h := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(host), "."))
	if h == "" {
		return false
	}
	if addr, err := netip.ParseAddr(h); err == nil {
		return addr.IsLoopback() || addr.IsPrivate()
	}
	return !strings.Contains(h, ".") ||
		h == "host.docker.internal" ||
		strings.HasSuffix(h, ".svc.cluster.local") ||
		strings.HasSuffix(h, ".svc") ||
		strings.HasSuffix(h, ".internal")
}

func extractRepoName(configURL string) string {
	u, err := url.Parse(configURL)
	if err != nil {
		return ""
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

func buildNetworkPolicy(dep *v1alpha1.AgenticRunnerDeployment, ns string, endpointPorts []int) K8sObject {
	ports := []K8sObject{
		{"protocol": "TCP", "port": 443},
	}
	for _, p := range endpointPorts {
		if p != 443 && p > 0 {
			ports = append(ports, K8sObject{"protocol": "TCP", "port": p})
		}
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

func buildARCRBAC(dep *v1alpha1.AgenticRunnerDeployment, ns string, opts Options) (K8sObject, K8sObject) {
	name := dep.Spec.Runner.RunnerScaleSetName + "-arc-manager"
	labels := commonLabels(dep)

	role := K8sObject{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "Role",
		"metadata": K8sObject{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"rules": []K8sObject{
			{
				"apiGroups": []string{""},
				"resources": []string{"pods"},
				"verbs":     []string{"create", "delete", "get"},
			},
			{
				"apiGroups": []string{""},
				"resources": []string{"pods/status"},
				"verbs":     []string{"get"},
			},
			{
				"apiGroups": []string{""},
				"resources": []string{"secrets"},
				"verbs":     []string{"create", "delete", "get", "list", "patch", "update"},
			},
			{
				"apiGroups": []string{""},
				"resources": []string{"serviceaccounts"},
				"verbs":     []string{"create", "delete", "get", "patch", "update"},
			},
			{
				"apiGroups": []string{"rbac.authorization.k8s.io"},
				"resources": []string{"roles", "rolebindings"},
				"verbs":     []string{"create", "delete", "get", "list", "patch", "update", "watch"},
			},
		},
	}

	binding := K8sObject{
		"apiVersion": "rbac.authorization.k8s.io/v1",
		"kind":       "RoleBinding",
		"metadata": K8sObject{
			"name":      name,
			"namespace": ns,
			"labels":    labels,
		},
		"roleRef": K8sObject{
			"apiGroup": "rbac.authorization.k8s.io",
			"kind":     "Role",
			"name":     name,
		},
		"subjects": []K8sObject{
			{
				"kind":      "ServiceAccount",
				"name":      opts.ARCControllerServiceAccount,
				"namespace": opts.ARCControllerNamespace,
			},
		},
	}

	return role, binding
}

func commonLabels(dep *v1alpha1.AgenticRunnerDeployment) map[string]string {
	return map[string]string{
		"app.kubernetes.io/managed-by":  "agentrunner",
		"agentic.example.io/deployment": dep.Metadata.Name,
		"agentic.example.io/toolchain":  dep.Spec.ToolchainRef.Name,
		"actions.github.com/scale-set":  dep.Spec.Runner.RunnerScaleSetName,
	}
}
