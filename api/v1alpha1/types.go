// Package v1alpha1 defines the Kubernetes CRD schema for AgenticRunnerToolchain
// and AgenticRunnerDeployment.
package v1alpha1

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	Group   = "agentic.example.io"
	Version = "v1alpha1"

	AgentModeReadOnly    = "read-only"
	AgentModeSuggest     = "suggest"
	AgentModeWriteBranch = "write-branch"
)

// TypeMeta mirrors Kubernetes metav1.TypeMeta.
type TypeMeta struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// ObjectMeta mirrors the subset of Kubernetes metav1.ObjectMeta needed for
// CRD composition and manifest rendering.
type ObjectMeta struct {
	Name        string            `json:"name"`
	Namespace   string            `json:"namespace,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`
}

// AgenticRunnerToolchain is a cluster-scoped resource defining a prebuilt
// container image, compute profile, egress allowlist, and AGENTS.md guidance.
type AgenticRunnerToolchain struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                 `json:"metadata"`
	Spec     AgenticRunnerToolchainSpec `json:"spec"`
}

// AgenticRunnerToolchainSpec configures a toolchain container image.
type AgenticRunnerToolchainSpec struct {
	Image           string               `json:"image"`
	Resources       ResourceRequirements `json:"resources,omitempty"`
	EgressAllowlist []string             `json:"egressAllowlist,omitempty"`
	AgentsMD        string               `json:"agentsMd,omitempty"`
}

// ResourceRequirements specifies CPU and memory requests and limits.
type ResourceRequirements struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}

// AgenticRunnerDeployment is a namespace-scoped resource composing an ARC v2
// AutoscalingRunnerSet with a toolchain and pi runtime configuration.
type AgenticRunnerDeployment struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                  `json:"metadata"`
	Spec     AgenticRunnerDeploymentSpec `json:"spec"`
}

// AgenticRunnerDeploymentSpec defines the composed runner and agent settings.
type AgenticRunnerDeploymentSpec struct {
	Runner       RunnerSpec `json:"runner"`
	ToolchainRef LocalRef   `json:"toolchainRef"`
	Agent        AgentSpec  `json:"agent"`
}

// RunnerSpec maps to ARC v2 AutoscalingRunnerSet configuration.
type RunnerSpec struct {
	GitHubConfigURL    string `json:"githubConfigUrl"`
	GitHubConfigSecret string `json:"githubConfigSecret,omitempty"`
	RunnerScaleSetName string `json:"runnerScaleSetName"`
	MinRunners         int    `json:"minRunners,omitempty"`
	MaxRunners         int    `json:"maxRunners,omitempty"`
	RunnerImage        string `json:"runnerImage,omitempty"`
	ServiceAccountName string `json:"serviceAccountName,omitempty"`
}

// LocalRef references another resource by name.
type LocalRef struct {
	Name string `json:"name"`
}

// AgentSpec configures the pi harness, mode, model gateway, execution limits, and prompts.
type AgentSpec struct {
	Harness               string                `json:"harness,omitempty"`
	Mode                  string                `json:"mode,omitempty"`
	Model                 string                `json:"model"`
	Provider              string                `json:"provider,omitempty"`
	Gateway               GatewaySpec           `json:"gateway"`
	Limits                LimitsSpec            `json:"limits,omitempty"`
	EgressAllowlist       []string              `json:"egressAllowlist,omitempty"`
	ProtectedBranches     *[]string             `json:"protectedBranches,omitempty"`
	SessionSink           *SessionSinkSpec      `json:"sessionSink,omitempty"`
	MCPServerRefs         []LocalRef            `json:"mcpServerRefs,omitempty"`
	MCPServers            []InlineMCPServerSpec `json:"mcpServers,omitempty"`
	SystemPromptRef       *ConfigMapKeyRef      `json:"systemPromptRef,omitempty"`
	AgentsMD              string                `json:"agentsMd,omitempty"`
	GitHubAppSecretRef    *SecretRef            `json:"githubAppSecretRef,omitempty"`
	GitHubAppConfigMapRef *ConfigMapKeyRef      `json:"githubAppConfigMapRef,omitempty"`
	GitHubAppID           string                `json:"githubAppId,omitempty"`
	GitHubInstallID       string                `json:"githubAppInstallationId,omitempty"`
}

// AgenticRunnerMCPServer is a namespace-scoped resource defining a reusable MCP server
// configuration (remote HTTP URL or pre-baked stdio command) referenced by deployments.
type AgenticRunnerMCPServer struct {
	TypeMeta `json:",inline"`
	Metadata ObjectMeta                 `json:"metadata"`
	Spec     AgenticRunnerMCPServerSpec `json:"spec"`
}

// AgenticRunnerMCPServerSpec configures either a remote HTTP MCP server (preferred)
// or a stdio subprocess command pre-baked into the toolchain image.
type AgenticRunnerMCPServerSpec struct {
	URL             string            `json:"url,omitempty"`
	Command         string            `json:"command,omitempty"`
	Args            []string          `json:"args,omitempty"`
	Headers         map[string]string `json:"headers,omitempty"`
	AuthSecretRef   *SecretRef        `json:"authSecretRef,omitempty"`
	EgressAllowlist []string          `json:"egressAllowlist,omitempty"`
	AgentsMD        string            `json:"agentsMd,omitempty"`
}

// InlineMCPServerSpec embeds a named MCP server configuration directly inside AgentSpec.
type InlineMCPServerSpec struct {
	Name                       string `json:"name"`
	AgenticRunnerMCPServerSpec `json:",inline"`
}

// GatewaySpec points pi at an external Model Gateway / Agent Router.
type GatewaySpec struct {
	Endpoint      string     `json:"endpoint"`
	AuthSecretRef *SecretRef `json:"authSecretRef,omitempty"`
}

// SessionSinkSpec configures an optional HTTP endpoint for fail-open session transcript archival.
type SessionSinkSpec struct {
	Endpoint      string     `json:"endpoint"`
	AuthSecretRef *SecretRef `json:"authSecretRef,omitempty"`
}

// SecretRef references a Kubernetes Secret.
type SecretRef struct {
	Name string `json:"name"`
	Key  string `json:"key,omitempty"`
}

// ConfigMapKeyRef references a ConfigMap containing base prompt files.
type ConfigMapKeyRef struct {
	ConfigMapName string `json:"configMapName"`
	Key           string `json:"key,omitempty"`
}

// LimitsSpec bounds agent execution per review step and model request.
type LimitsSpec struct {
	MaxTurns            int     `json:"maxTurns,omitempty"`
	MaxDuration         string  `json:"maxDuration,omitempty"`
	MaxCostUSD          float64 `json:"maxCostUsd,omitempty"`
	ContextWindow       int     `json:"contextWindow,omitempty"`
	MaxTokens           int     `json:"maxTokens,omitempty"`
	MaxTransportRetries *int    `json:"maxTransportRetries,omitempty"`
}

// Validate checks required fields and invariants on AgenticRunnerToolchain.
func (t *AgenticRunnerToolchain) Validate() error {
	var errs []error
	if strings.TrimSpace(t.Metadata.Name) == "" {
		errs = append(errs, errors.New("metadata.name is required"))
	}
	if strings.TrimSpace(t.Spec.Image) == "" {
		errs = append(errs, errors.New("spec.image is required"))
	}
	errs = append(errs, validateEgressAllowlist("spec.egressAllowlist", t.Spec.EgressAllowlist)...)
	return errors.Join(errs...)
}

func validateEgressAllowlist(field string, hosts []string) []error {
	var errs []error
	for _, host := range hosts {
		if strings.TrimSpace(host) == "" || strings.ContainsAny(host, " \t\n\r/") {
			errs = append(errs, fmt.Errorf("invalid %s entry %q", field, host))
		}
	}
	return errs
}

var dns1123SubdomainRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?(\.[a-z0-9]([-a-z0-9]*[a-z0-9])?)*$`)
var mcpServerNameRegex = regexp.MustCompile(`^[a-z0-9]([-a-z0-9_]*[a-z0-9])?$`)

func isDNS1123Subdomain(name string) bool {
	return len(name) > 0 && len(name) <= 253 && dns1123SubdomainRegex.MatchString(name)
}

func isValidMCPServerName(name string) bool {
	return len(name) > 0 && len(name) <= 63 && mcpServerNameRegex.MatchString(name)
}

func normalizeMCPServerKey(name string) string {
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(name), "-", "_"))
}

// Validate checks required fields and invariants on AgenticRunnerMCPServer.
func (m *AgenticRunnerMCPServer) Validate() error {
	var errs []error
	if !isValidMCPServerName(m.Metadata.Name) {
		errs = append(errs, fmt.Errorf("invalid metadata.name %q: must match [a-z0-9]([-a-z0-9_]*[a-z0-9])?", m.Metadata.Name))
	}
	errs = append(errs, validateMCPServerSpec("spec", m.Spec)...)
	return errors.Join(errs...)
}

func validateMCPServerSpec(field string, spec AgenticRunnerMCPServerSpec) []error {
	var errs []error
	hasURL := strings.TrimSpace(spec.URL) != ""
	hasCmd := strings.TrimSpace(spec.Command) != ""
	if hasURL == hasCmd {
		errs = append(errs, fmt.Errorf("%s must set exactly one of url or command", field))
	}
	if hasURL {
		if err := validateURL(spec.URL, field+".url"); err != nil {
			errs = append(errs, err)
		}
	}
	if hasCmd && strings.ContainsAny(strings.TrimSpace(spec.Command), " \t\n\r") {
		errs = append(errs, fmt.Errorf("%s.command must be a binary name or path without whitespace (use args)", field))
	}
	if spec.AuthSecretRef != nil {
		if strings.TrimSpace(spec.AuthSecretRef.Name) == "" {
			errs = append(errs, fmt.Errorf("%s.authSecretRef.name is required", field))
		}
		if !hasURL {
			errs = append(errs, fmt.Errorf("%s.authSecretRef is only valid when url is set", field))
		}
	}
	errs = append(errs, validateEgressAllowlist(field+".egressAllowlist", spec.EgressAllowlist)...)
	return errs
}

// Validate checks required fields and invariants on AgenticRunnerDeployment.
func (d *AgenticRunnerDeployment) Validate() error {
	var errs []error
	if strings.TrimSpace(d.Metadata.Name) == "" {
		errs = append(errs, errors.New("metadata.name is required"))
	}
	if err := validateURL(d.Spec.Runner.GitHubConfigURL, "spec.runner.githubConfigUrl"); err != nil {
		errs = append(errs, err)
	}
	if strings.TrimSpace(d.Spec.Runner.RunnerScaleSetName) == "" {
		errs = append(errs, errors.New("spec.runner.runnerScaleSetName is required"))
	}
	if sa := d.Spec.Runner.ServiceAccountName; sa != "" && !isDNS1123Subdomain(sa) {
		errs = append(errs, fmt.Errorf("invalid spec.runner.serviceAccountName %q: must be a lowercase RFC 1123 subdomain", sa))
	}
	if d.Spec.Runner.MinRunners < 0 {
		errs = append(errs, errors.New("spec.runner.minRunners must be >= 0"))
	}
	if d.Spec.Runner.MaxRunners < 0 {
		errs = append(errs, errors.New("spec.runner.maxRunners must be >= 0"))
	}
	if d.Spec.Runner.MaxRunners > 0 && d.Spec.Runner.MaxRunners < d.Spec.Runner.MinRunners {
		errs = append(errs, errors.New("spec.runner.maxRunners must be >= spec.runner.minRunners"))
	}
	if strings.TrimSpace(d.Spec.ToolchainRef.Name) == "" {
		errs = append(errs, errors.New("spec.toolchainRef.name is required"))
	}
	if h := d.Spec.Agent.Harness; h != "" && h != "pi" {
		errs = append(errs, fmt.Errorf("unsupported spec.agent.harness %q (only \"pi\" is supported)", h))
	}
	switch d.Spec.Agent.Mode {
	case "", AgentModeReadOnly, AgentModeSuggest, AgentModeWriteBranch:
	default:
		errs = append(errs, fmt.Errorf("unsupported spec.agent.mode %q", d.Spec.Agent.Mode))
	}
	errs = append(errs, validateEgressAllowlist("spec.agent.egressAllowlist", d.Spec.Agent.EgressAllowlist)...)
	if d.Spec.Agent.ProtectedBranches != nil {
		for _, b := range *d.Spec.Agent.ProtectedBranches {
			if strings.TrimSpace(b) == "" || strings.ContainsAny(b, " \t\n\r") {
				errs = append(errs, fmt.Errorf("invalid spec.agent.protectedBranches entry %q", b))
			}
		}
	}
	if strings.TrimSpace(d.Spec.Agent.Model) == "" {
		errs = append(errs, errors.New("spec.agent.model is required"))
	}
	if err := validateURL(d.Spec.Agent.Gateway.Endpoint, "spec.agent.gateway.endpoint"); err != nil {
		errs = append(errs, err)
	}
	if d.Spec.Agent.SessionSink != nil {
		if err := validateURL(d.Spec.Agent.SessionSink.Endpoint, "spec.agent.sessionSink.endpoint"); err != nil {
			errs = append(errs, err)
		}
	}
	normToName := map[string]string{}
	for i, ref := range d.Spec.Agent.MCPServerRefs {
		if !isValidMCPServerName(ref.Name) {
			errs = append(errs, fmt.Errorf("invalid spec.agent.mcpServerRefs[%d].name %q", i, ref.Name))
			continue
		}
		norm := normalizeMCPServerKey(ref.Name)
		if prev, dup := normToName[norm]; dup {
			if prev == ref.Name {
				errs = append(errs, fmt.Errorf("duplicate spec.agent.mcpServerRefs name %q", ref.Name))
			} else {
				errs = append(errs, fmt.Errorf("spec.agent.mcpServerRefs[%d].name %q collides with %q after token-env normalization", i, ref.Name, prev))
			}
			continue
		}
		normToName[norm] = ref.Name
	}
	seenInlineMCP := map[string]struct{}{}
	for i, srv := range d.Spec.Agent.MCPServers {
		if !isValidMCPServerName(srv.Name) {
			errs = append(errs, fmt.Errorf("invalid spec.agent.mcpServers[%d].name %q", i, srv.Name))
		} else if _, dup := seenInlineMCP[srv.Name]; dup {
			errs = append(errs, fmt.Errorf("duplicate spec.agent.mcpServers name %q", srv.Name))
		} else {
			norm := normalizeMCPServerKey(srv.Name)
			if prev, dup := normToName[norm]; dup && prev != srv.Name {
				errs = append(errs, fmt.Errorf("spec.agent.mcpServers[%d].name %q collides with %q after token-env normalization", i, srv.Name, prev))
			} else {
				seenInlineMCP[srv.Name] = struct{}{}
				normToName[norm] = srv.Name
			}
		}
		errs = append(errs, validateMCPServerSpec(fmt.Sprintf("spec.agent.mcpServers[%d]", i), srv.AgenticRunnerMCPServerSpec)...)
	}
	if d.Spec.Agent.Limits.MaxTurns < 0 {
		errs = append(errs, errors.New("spec.agent.limits.maxTurns must be >= 0"))
	}
	if d.Spec.Agent.Limits.MaxCostUSD < 0 {
		errs = append(errs, errors.New("spec.agent.limits.maxCostUsd must be >= 0"))
	}
	if d.Spec.Agent.Limits.ContextWindow < 0 {
		errs = append(errs, errors.New("spec.agent.limits.contextWindow must be >= 0"))
	}
	if d.Spec.Agent.Limits.MaxTokens < 0 {
		errs = append(errs, errors.New("spec.agent.limits.maxTokens must be >= 0"))
	}
	if d.Spec.Agent.Limits.ContextWindow > 0 && d.Spec.Agent.Limits.MaxTokens > d.Spec.Agent.Limits.ContextWindow {
		errs = append(errs, errors.New("spec.agent.limits.maxTokens must be <= spec.agent.limits.contextWindow"))
	}
	if d.Spec.Agent.Limits.MaxTransportRetries != nil && *d.Spec.Agent.Limits.MaxTransportRetries < 0 {
		errs = append(errs, errors.New("spec.agent.limits.maxTransportRetries must be >= 0"))
	}
	if dur := d.Spec.Agent.Limits.MaxDuration; dur != "" {
		parsed, err := time.ParseDuration(dur)
		if err != nil || parsed <= 0 {
			errs = append(errs, fmt.Errorf("invalid spec.agent.limits.maxDuration %q", dur))
		}
	}
	return errors.Join(errs...)
}

func validateURL(raw, field string) error {
	if strings.TrimSpace(raw) == "" {
		return fmt.Errorf("%s is required", field)
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("%s must be a valid http(s) URL: %q", field, raw)
	}
	return nil
}
