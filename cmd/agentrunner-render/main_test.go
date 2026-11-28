package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestExampleAgentsMDMentionsGolangciLint(t *testing.T) {
	_, tc := exampleObjects()
	if tc == nil {
		t.Fatal("exampleObjects returned nil toolchain")
	}
	if !strings.Contains(tc.Spec.AgentsMD, "golangci-lint") {
		t.Errorf("example AgentsMD missing golangci-lint: %q", tc.Spec.AgentsMD)
	}
}

func TestExampleToolchainYAMLMentionsGolangciLint(t *testing.T) {
	raw, err := os.ReadFile("../../examples/toolchain-go.yaml")
	if err != nil {
		t.Fatalf("reading example toolchain yaml: %v", err)
	}
	if !strings.Contains(string(raw), "golangci-lint") {
		t.Errorf("examples/toolchain-go.yaml missing golangci-lint:\n%s", raw)
	}
}

type matrixEntry struct {
	Name      string   `json:"name"`
	Minor     string   `json:"minor"`
	SmokeCmd  string   `json:"smoke_cmd"`
	BuildArgs []string `json:"build_args"`
}

func TestGolangMatrixSmokeCmdsVerifyGolangciLint(t *testing.T) {
	raw, err := os.ReadFile("../../images/toolchains/matrix.json")
	if err != nil {
		t.Fatalf("reading matrix.json: %v", err)
	}
	var entries []matrixEntry
	if err := json.Unmarshal(raw, &entries); err != nil {
		t.Fatalf("matrix.json is not valid JSON: %v", err)
	}
	count := 0
	for _, e := range entries {
		if e.Name != "golang" {
			continue
		}
		count++
		if !strings.Contains(e.SmokeCmd, "golangci-lint --version") {
			t.Errorf("golang (%s) smoke_cmd missing golangci-lint --version: %q", e.Minor, e.SmokeCmd)
		}
	}
	if count == 0 {
		t.Fatal("matrix.json has no golang entries")
	}
}
