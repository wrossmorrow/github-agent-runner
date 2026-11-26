// Package controller reconciles AgenticRunnerDeployment and AgenticRunnerToolchain
// resources into ARC v2 AutoscalingRunnerSet, ConfigMap, RBAC, and NetworkPolicy objects.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/wrossmorrow/agentrunner/api/v1alpha1"
	"github.com/wrossmorrow/agentrunner/pkg/render"
)

var (
	// DeploymentGVK is the GroupVersionKind for AgenticRunnerDeployment.
	DeploymentGVK = schema.GroupVersionKind{
		Group:   v1alpha1.Group,
		Version: v1alpha1.Version,
		Kind:    "AgenticRunnerDeployment",
	}
	// ToolchainGVK is the GroupVersionKind for AgenticRunnerToolchain.
	ToolchainGVK = schema.GroupVersionKind{
		Group:   v1alpha1.Group,
		Version: v1alpha1.Version,
		Kind:    "AgenticRunnerToolchain",
	}
	appsDeploymentListGVK = schema.GroupVersionKind{
		Group:   "apps",
		Version: "v1",
		Kind:    "DeploymentList",
	}
)

// DeploymentReconciler reconciles AgenticRunnerDeployment resources.
type DeploymentReconciler struct {
	Client               client.Client
	RenderOptions        render.Options
	AutoDetectARCVersion bool
}

// Reconcile resolves the referenced AgenticRunnerToolchain, renders the ARC v2
// and sidecar manifests, applies them via Server-Side Apply with ownerReferences,
// and updates the AgenticRunnerDeployment status.
func (r *DeploymentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	depObj := &unstructured.Unstructured{}
	depObj.SetGroupVersionKind(DeploymentGVK)
	if err := r.Client.Get(ctx, req.NamespacedName, depObj); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	var dep v1alpha1.AgenticRunnerDeployment
	if err := decodeUnstructured(depObj, &dep); err != nil {
		return ctrl.Result{}, fmt.Errorf("decode deployment: %w", err)
	}
	if err := dep.Validate(); err != nil {
		logger.Info("invalid AgenticRunnerDeployment spec", "error", err)
		_ = r.updateStatus(ctx, depObj, "", "", "", "False", "InvalidSpec", err.Error())
		return ctrl.Result{}, nil
	}

	tcObj := &unstructured.Unstructured{}
	tcObj.SetGroupVersionKind(ToolchainGVK)
	if err := r.Client.Get(ctx, types.NamespacedName{Name: dep.Spec.ToolchainRef.Name}, tcObj); err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("AgenticRunnerToolchain %q not found", dep.Spec.ToolchainRef.Name)
			logger.Info(msg)
			_ = r.updateStatus(ctx, depObj, "", "", "", "False", "ToolchainNotFound", msg)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	var tc v1alpha1.AgenticRunnerToolchain
	if err := decodeUnstructured(tcObj, &tc); err != nil {
		return ctrl.Result{}, fmt.Errorf("decode toolchain: %w", err)
	}

	opts := r.RenderOptions
	if opts.ARCVersion == "" && r.AutoDetectARCVersion {
		opts.ARCVersion = r.detectARCVersion(ctx, opts.ARCControllerNamespace)
	}

	bundle, err := render.Render(&dep, &tc, opts)
	if err != nil {
		_ = r.updateStatus(ctx, depObj, tc.Spec.Image, opts.ARCVersion, dep.Spec.Runner.RunnerScaleSetName, "False", "RenderFailed", err.Error())
		return ctrl.Result{}, nil
	}

	ownerRef := metav1.OwnerReference{
		APIVersion:         DeploymentGVK.GroupVersion().String(),
		Kind:               DeploymentGVK.Kind,
		Name:               depObj.GetName(),
		UID:                depObj.GetUID(),
		Controller:         boolPtr(true),
		BlockOwnerDeletion: boolPtr(true),
	}

	resolvedARC := opts.ARCVersion
	if resolvedARC == "" {
		resolvedARC = render.DefaultARCVersion
	}

	conflictMsg, err := r.checkOwnershipConflicts(ctx, bundle, ownerRef)
	if err != nil {
		return ctrl.Result{}, err
	}
	if conflictMsg != "" {
		logger.Info("scale set ownership conflict", "conflict", conflictMsg)
		_ = r.updateStatus(ctx, depObj, tc.Spec.Image, resolvedARC, dep.Spec.Runner.RunnerScaleSetName, "False", "ScaleSetNameConflict", conflictMsg)
		return ctrl.Result{}, nil
	}

	for _, obj := range bundle.Objects() {
		u := &unstructured.Unstructured{Object: deepCopyMap(obj)}
		u.SetOwnerReferences([]metav1.OwnerReference{ownerRef})
		if err := r.applyObject(ctx, u); err != nil {
			_ = r.updateStatus(ctx, depObj, tc.Spec.Image, resolvedARC, dep.Spec.Runner.RunnerScaleSetName, "False", "ApplyFailed", err.Error())
			return ctrl.Result{}, fmt.Errorf("applying %s/%s: %w", u.GetKind(), u.GetName(), err)
		}
	}

	if err := r.updateStatus(ctx, depObj, tc.Spec.Image, resolvedARC, dep.Spec.Runner.RunnerScaleSetName, "True", "Reconciled", "Rendered and applied ARC v2 bundle"); err != nil {
		return ctrl.Result{}, err
	}

	logger.Info("reconciled AgenticRunnerDeployment", "toolchain", tc.Metadata.Name, "scaleSet", dep.Spec.Runner.RunnerScaleSetName, "arcVersion", resolvedARC)
	return ctrl.Result{}, nil
}

// SetupWithManager registers watches on AgenticRunnerDeployment (primary) and
// AgenticRunnerToolchain (fan-out to referencing deployments).
func (r *DeploymentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	depWatch := &unstructured.Unstructured{}
	depWatch.SetGroupVersionKind(DeploymentGVK)

	tcWatch := &unstructured.Unstructured{}
	tcWatch.SetGroupVersionKind(ToolchainGVK)

	return ctrl.NewControllerManagedBy(mgr).
		Named("agenticrunnerdeployment").
		For(depWatch).
		Watches(tcWatch, handler.EnqueueRequestsFromMapFunc(r.deploymentsForToolchain)).
		Complete(r)
}

func (r *DeploymentReconciler) deploymentsForToolchain(ctx context.Context, obj client.Object) []reconcile.Request {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   DeploymentGVK.Group,
		Version: DeploymentGVK.Version,
		Kind:    "AgenticRunnerDeploymentList",
	})
	if err := r.Client.List(ctx, list); err != nil {
		log.FromContext(ctx).Error(err, "listing AgenticRunnerDeployments for toolchain", "toolchain", obj.GetName())
		return nil
	}

	var reqs []reconcile.Request
	for _, item := range list.Items {
		ref, found, _ := unstructured.NestedString(item.Object, "spec", "toolchainRef", "name")
		if found && ref == obj.GetName() {
			reqs = append(reqs, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Namespace: item.GetNamespace(),
					Name:      item.GetName(),
				},
			})
		}
	}
	return reqs
}

func (r *DeploymentReconciler) detectARCVersion(ctx context.Context, controllerNS string) string {
	if controllerNS == "" {
		controllerNS = render.DefaultARCControllerNamespace
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(appsDeploymentListGVK)
	if err := r.Client.List(ctx, list, client.InNamespace(controllerNS)); err != nil {
		return render.DefaultARCVersion
	}
	for _, d := range list.Items {
		labels := d.GetLabels()
		if labels["app.kubernetes.io/part-of"] == "gha-rs-controller" || d.GetName() == render.DefaultARCControllerSA {
			if v := labels["app.kubernetes.io/version"]; v != "" {
				return v
			}
		}
	}
	return render.DefaultARCVersion
}

func (r *DeploymentReconciler) updateStatus(
	ctx context.Context,
	depObj *unstructured.Unstructured,
	resolvedImage, arcVersion, scaleSetName, statusVal, reason, message string,
) error {
	updated := depObj.DeepCopy()
	statusMap := map[string]any{
		"observedGeneration": depObj.GetGeneration(),
		"conditions": []any{
			map[string]any{
				"type":               "Ready",
				"status":             statusVal,
				"reason":             reason,
				"message":            message,
				"lastTransitionTime": time.Now().UTC().Format(time.RFC3339),
			},
		},
	}
	if resolvedImage != "" {
		statusMap["resolvedImage"] = resolvedImage
	}
	if arcVersion != "" {
		statusMap["arcVersion"] = arcVersion
	}
	if scaleSetName != "" {
		statusMap["scaleSetName"] = scaleSetName
	}
	updated.Object["status"] = statusMap
	return r.Client.Status().Update(ctx, updated)
}

func (r *DeploymentReconciler) checkOwnershipConflicts(ctx context.Context, bundle *render.Bundle, ownerRef metav1.OwnerReference) (string, error) {
	objs := append([]render.K8sObject{bundle.AutoscalingRunnerSet}, bundle.Objects()...)
	for _, obj := range objs {
		if obj == nil {
			continue
		}
		desired := &unstructured.Unstructured{Object: deepCopyMap(obj)}
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(desired.GroupVersionKind())
		err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), existing)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		if msg, conflict := ownerConflict(existing, ownerRef); conflict {
			return msg, nil
		}
	}
	return "", nil
}

func ownerConflict(existing *unstructured.Unstructured, desired metav1.OwnerReference) (string, bool) {
	for _, ref := range existing.GetOwnerReferences() {
		isController := ref.Controller != nil && *ref.Controller
		sameType := ref.APIVersion == desired.APIVersion && ref.Kind == desired.Kind
		if !isController && !sameType {
			continue
		}
		if ref.UID != desired.UID || (desired.UID == "" && ref.Name != desired.Name) {
			return fmt.Sprintf("%s/%s is already owned by %s/%s", existing.GetKind(), existing.GetName(), ref.Kind, ref.Name), true
		}
	}
	return "", false
}

func (r *DeploymentReconciler) applyObject(ctx context.Context, desired *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(desired.GroupVersionKind())
	err := r.Client.Get(ctx, client.ObjectKeyFromObject(desired), existing)
	if apierrors.IsNotFound(err) {
		return r.Client.Create(ctx, desired)
	}
	if err != nil {
		return err
	}
	if refs := desired.GetOwnerReferences(); len(refs) > 0 {
		if msg, conflict := ownerConflict(existing, refs[0]); conflict {
			return fmt.Errorf("%s", msg)
		}
	}
	desired.SetResourceVersion(existing.GetResourceVersion())
	if fins := existing.GetFinalizers(); len(fins) > 0 {
		desired.SetFinalizers(fins)
	}
	return r.Client.Update(ctx, desired)
}

func decodeUnstructured(u *unstructured.Unstructured, dst any) error {
	raw, err := json.Marshal(u.Object)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, dst)
}

func deepCopyMap(in map[string]any) map[string]any {
	raw, _ := json.Marshal(in)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return out
}

func boolPtr(b bool) *bool { return &b }
