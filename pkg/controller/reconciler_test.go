package controller

import (
	"context"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/wrossmorrow/github-agent-runner/api/v1alpha1"
	"github.com/wrossmorrow/github-agent-runner/pkg/render"
)

func newUnstructuredDeployment(ns, name, toolchainName string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": v1alpha1.Group + "/" + v1alpha1.Version,
			"kind":       "AgenticRunnerDeployment",
			"metadata": map[string]any{
				"name":       name,
				"namespace":  ns,
				"uid":        "dep-uid-123",
				"generation": int64(2),
			},
			"spec": map[string]any{
				"runner": map[string]any{
					"githubConfigUrl":    "https://github.com/my-org/my-repo",
					"githubConfigSecret": "arc-github-auth",
					"runnerScaleSetName": "agentic-go-review",
					"minRunners":         int64(0),
					"maxRunners":         int64(3),
				},
				"toolchainRef": map[string]any{"name": toolchainName},
				"agent": map[string]any{
					"harness": "pi",
					"model":   "flint",
					"gateway": map[string]any{
						"endpoint": "https://gateway.internal/v1",
					},
				},
			},
		},
	}
	return u
}

func newUnstructuredToolchain(name, image string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": v1alpha1.Group + "/" + v1alpha1.Version,
			"kind":       "AgenticRunnerToolchain",
			"metadata": map[string]any{
				"name": name,
			},
			"spec": map[string]any{
				"image":           image,
				"egressAllowlist": []any{"proxy.golang.org"},
				"agentsMd":        "Run `go test ./...`.",
			},
		},
	}
}

func newFakeARCDeployment(version string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": "apps/v1",
			"kind":       "Deployment",
			"metadata": map[string]any{
				"name":      render.DefaultARCControllerSA,
				"namespace": render.DefaultARCControllerNamespace,
				"labels": map[string]any{
					"app.kubernetes.io/part-of": "gha-rs-controller",
					"app.kubernetes.io/version": version,
				},
			},
		},
	}
}

func TestReconcileHappyPath(t *testing.T) {
	dep := newUnstructuredDeployment("actions-runners", "go-reviewer", "go-1.23")
	tc := newUnstructuredToolchain("go-1.23", "agentrunner-golang:latest")
	arcDep := newFakeARCDeployment("0.16.0")

	statusTarget := &unstructured.Unstructured{}
	statusTarget.SetGroupVersionKind(DeploymentGVK)

	cl := fake.NewClientBuilder().
		WithObjects(dep, tc, arcDep).
		WithStatusSubresource(statusTarget).
		Build()

	r := &DeploymentReconciler{
		Client:               cl,
		AutoDetectARCVersion: true,
	}

	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "actions-runners", Name: "go-reviewer"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify AutoscalingRunnerSet created with detected ARC version & ownerReference
	ars := &unstructured.Unstructured{}
	ars.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "actions.github.com",
		Version: "v1alpha1",
		Kind:    "AutoscalingRunnerSet",
	})
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "agentic-go-review"}, ars); err != nil {
		t.Fatalf("expected AutoscalingRunnerSet: %v", err)
	}
	if ars.GetLabels()["app.kubernetes.io/version"] != "0.16.0" {
		t.Fatalf("expected auto-detected ARC version 0.16.0, got %q", ars.GetLabels()["app.kubernetes.io/version"])
	}
	if refs := ars.GetOwnerReferences(); len(refs) != 1 || refs[0].Name != "go-reviewer" || refs[0].UID != "dep-uid-123" {
		t.Fatalf("unexpected ownerReferences: %#v", refs)
	}

	// Verify ConfigMap created
	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "go-reviewer-agent-config"}, cm); err != nil {
		t.Fatalf("expected agent ConfigMap: %v", err)
	}

	// Verify Status updated
	gotDep := &unstructured.Unstructured{}
	gotDep.SetGroupVersionKind(DeploymentGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "go-reviewer"}, gotDep); err != nil {
		t.Fatalf("get updated deployment: %v", err)
	}
	resolvedImg, _, _ := unstructured.NestedString(gotDep.Object, "status", "resolvedImage")
	arcVer, _, _ := unstructured.NestedString(gotDep.Object, "status", "arcVersion")
	conds, _, _ := unstructured.NestedSlice(gotDep.Object, "status", "conditions")
	if resolvedImg != "agentrunner-golang:latest" || arcVer != "0.16.0" || len(conds) != 1 {
		t.Fatalf("unexpected status: %#v", gotDep.Object["status"])
	}
	if conds[0].(map[string]any)["status"] != "True" {
		t.Fatalf("expected Ready=True, got %#v", conds[0])
	}
}

func TestReconcileMissingToolchainSetsCondition(t *testing.T) {
	dep := newUnstructuredDeployment("actions-runners", "go-reviewer", "missing-tc")
	statusTarget := &unstructured.Unstructured{}
	statusTarget.SetGroupVersionKind(DeploymentGVK)

	cl := fake.NewClientBuilder().
		WithObjects(dep).
		WithStatusSubresource(statusTarget).
		Build()

	r := &DeploymentReconciler{Client: cl}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "actions-runners", Name: "go-reviewer"},
	}); err != nil {
		t.Fatalf("expected nil error on missing toolchain, got %v", err)
	}

	gotDep := &unstructured.Unstructured{}
	gotDep.SetGroupVersionKind(DeploymentGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "go-reviewer"}, gotDep); err != nil {
		t.Fatal(err)
	}
	conds, _, _ := unstructured.NestedSlice(gotDep.Object, "status", "conditions")
	if len(conds) != 1 || conds[0].(map[string]any)["reason"] != "ToolchainNotFound" {
		t.Fatalf("expected ToolchainNotFound condition, got %#v", gotDep.Object["status"])
	}
}

func TestDeploymentsForToolchainFanOut(t *testing.T) {
	d1 := newUnstructuredDeployment("ns1", "rev-go-1", "go-1.23")
	d2 := newUnstructuredDeployment("ns2", "rev-go-2", "go-1.23")
	d3 := newUnstructuredDeployment("ns1", "rev-py", "python-3.12")
	tc := newUnstructuredToolchain("go-1.23", "agentrunner-golang:latest")

	cl := fake.NewClientBuilder().WithObjects(d1, d2, d3, tc).Build()
	r := &DeploymentReconciler{Client: cl}

	reqs := r.deploymentsForToolchain(context.Background(), client.Object(tc))
	if len(reqs) != 2 {
		t.Fatalf("expected 2 requests for go-1.23, got %d: %+v", len(reqs), reqs)
	}
}

func TestReconcileScaleSetConflict(t *testing.T) {
	dep1 := newUnstructuredDeployment("actions-runners", "dep1", "go-1.23")
	dep1.SetUID("dep-uid-1")
	dep2 := newUnstructuredDeployment("actions-runners", "dep2", "go-1.23")
	dep2.SetUID("dep-uid-2")
	tc := newUnstructuredToolchain("go-1.23", "agentrunner-golang:latest")

	statusTarget := &unstructured.Unstructured{}
	statusTarget.SetGroupVersionKind(DeploymentGVK)

	cl := fake.NewClientBuilder().
		WithObjects(dep1, dep2, tc).
		WithStatusSubresource(statusTarget).
		Build()

	r := &DeploymentReconciler{Client: cl}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "actions-runners", Name: "dep1"},
	}); err != nil {
		t.Fatalf("Reconcile dep1 failed: %v", err)
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "actions-runners", Name: "dep2"},
	}); err != nil {
		t.Fatalf("expected nil error on scale-set conflict, got %v", err)
	}

	gotDep2 := &unstructured.Unstructured{}
	gotDep2.SetGroupVersionKind(DeploymentGVK)
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "dep2"}, gotDep2); err != nil {
		t.Fatal(err)
	}
	conds, _, _ := unstructured.NestedSlice(gotDep2.Object, "status", "conditions")
	if len(conds) != 1 {
		t.Fatalf("expected 1 condition on dep2, got %#v", gotDep2.Object["status"])
	}
	cond := conds[0].(map[string]any)
	if cond["status"] != "False" || cond["reason"] != "ScaleSetNameConflict" {
		t.Fatalf("expected Ready=False / ScaleSetNameConflict, got %#v", cond)
	}
	msg, _ := cond["message"].(string)
	if !strings.Contains(msg, "AutoscalingRunnerSet/agentic-go-review") || !strings.Contains(msg, "dep1") {
		t.Fatalf("expected conflict message mentioning AutoscalingRunnerSet/agentic-go-review and dep1, got %q", msg)
	}

	ars := &unstructured.Unstructured{}
	ars.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   "actions.github.com",
		Version: "v1alpha1",
		Kind:    "AutoscalingRunnerSet",
	})
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "agentic-go-review"}, ars); err != nil {
		t.Fatal(err)
	}
	if refs := ars.GetOwnerReferences(); len(refs) != 1 || refs[0].UID != "dep-uid-1" {
		t.Fatalf("expected AutoscalingRunnerSet to remain owned by dep1 (dep-uid-1), got %#v", refs)
	}

	cm := &unstructured.Unstructured{}
	cm.SetGroupVersionKind(schema.GroupVersionKind{Version: "v1", Kind: "ConfigMap"})
	err := cl.Get(context.Background(), types.NamespacedName{Namespace: "actions-runners", Name: "dep2-agent-config"}, cm)
	if !apierrors.IsNotFound(err) {
		t.Fatalf("expected dep2-agent-config not to be created on conflict, got err=%v", err)
	}
}
