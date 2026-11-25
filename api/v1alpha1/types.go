// Package v1alpha1 defines the Kubernetes CRD schema for AgenticRunnerToolchain
// and AgenticRunnerDeployment.
package v1alpha1

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

const (
	Group   = "agentic.example.io"
	Version = "v1alpha1"
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
}

// LocalRef references another resource by name.
type LocalRef struct {
	Name string `json:"name"`
}

// AgentSpec configures the pi harness, model gateway, execution limits, and prompts.
type AgentSpec struct {
	Harness            string           `json:"harness,omitempty"`
	Model              string           `json:"model"`
	Provider           string           `json:"provider,omitempty"`
	Gateway            GatewaySpec      `json:"gateway"`
	Limits             LimitsSpec       `json:"limits,omitempty"`
	SystemPromptRef    *ConfigMapKeyRef `json:"systemPromptRef,omitempty"`
	AgentsMD           string           `json:"agentsMd,omitempty"`
	GitHubAppSecretRef *SecretRef       `json:"githubAppSecretRef,omitempty"`
	GitHubAppID        string           `json:"githubAppId,omitempty"`
	GitHubInstallID    string           `json:"githubAppInstallationId,omitempty"`
}

// GatewaySpec points pi at an external Model Gateway / Agent Router.
type GatewaySpec struct {
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

// LimitsSpec bounds agent execution per review step.
type LimitsSpec struct {
	MaxTurns    int     `json:"maxTurns,omitempty"`
	MaxDuration string  `json:"maxDuration,omitempty"`
	MaxCostUSD  float64 `json:"maxCostUsd,omitempty"`
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
	for _, host := range t.Spec.EgressAllowlist {
		if strings.TrimSpace(host) == "" || strings.ContainsAny(host, " \t\n\r/") {
			errs = append(errs, fmt.Errorf("invalid egressAllowlist entry %q", host))
		}
	}
	return errors.Join(errs...)
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
	if strings.TrimSpace(d.Spec.Agent.Model) == "" {
		errs = append(errs, errors.New("spec.agent.model is required"))
	}
	if err := validateURL(d.Spec.Agent.Gateway.Endpoint, "spec.agent.gateway.endpoint"); err != nil {
		errs = append(errs, err)
	}
	if d.Spec.Agent.Limits.MaxTurns < 0 {
		errs = append(errs, errors.New("spec.agent.limits.maxTurns must be >= 0"))
	}
	if d.Spec.Agent.Limits.MaxCostUSD < 0 {
		errs = append(errs, errors.New("spec.agent.limits.maxCostUsd must be >= 0"))
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
