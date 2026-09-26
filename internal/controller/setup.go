package controller

import (
	"context"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1alpha1 "github.com/sudharsanomprakash/kubevirt-vsphere-populator/api/v1alpha1"
)

// IndexSource indexes PVCs by the VSphereVolumeSource they reference.
const IndexSource = "vsphere-populator.source"

// SetupWithManager registers the reconciler and its watches.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrent int) error {
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &corev1.PersistentVolumeClaim{}, IndexSource, func(o client.Object) []string {
		pvc := o.(*corev1.PersistentVolumeClaim)
		if IsOurs(pvc) {
			return []string{pvc.Spec.DataSourceRef.Name}
		}
		return nil
	}); err != nil {
		return err
	}

	ours := predicate.NewPredicateFuncs(func(o client.Object) bool {
		pvc, ok := o.(*corev1.PersistentVolumeClaim)
		return ok && IsOurs(pvc)
	})

	// Prime PVCs, worker Jobs and worker pods map back to the target PVC
	// through their annotation; PVs through their claim reference.
	toTarget := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		if o.GetLabels()[LabelManagedBy] != ManagedByValue {
			return nil
		}
		name := o.GetAnnotations()[AnnPVCName]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: o.GetNamespace(), Name: name}}}
	})
	pvToTarget := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		pv := o.(*corev1.PersistentVolume)
		ref := pv.Spec.ClaimRef
		if ref == nil {
			return nil
		}
		if _, ok := pv.Annotations[AnnPopulatedFrom]; ok {
			return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}}}
		}
		var prime corev1.PersistentVolumeClaim
		if err := mgr.GetClient().Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, &prime); err != nil {
			return nil
		}
		if prime.Labels[LabelManagedBy] != ManagedByValue || prime.Annotations[AnnPVCName] == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: ref.Namespace, Name: prime.Annotations[AnnPVCName]}}}
	})
	srcToPVCs := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		var pvcs corev1.PersistentVolumeClaimList
		if err := mgr.GetClient().List(ctx, &pvcs, client.InNamespace(o.GetNamespace()), client.MatchingFields{IndexSource: o.GetName()}); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(pvcs.Items))
		for _, p := range pvcs.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: p.Namespace, Name: p.Name}})
		}
		return reqs
	})

	return ctrl.NewControllerManagedBy(mgr).
		Named("vsphere-populator").
		For(&corev1.PersistentVolumeClaim{}, builder.WithPredicates(ours)).
		Watches(&corev1.PersistentVolumeClaim{}, toTarget).
		Watches(&batchv1.Job{}, toTarget).
		Watches(&corev1.Pod{}, toTarget).
		Watches(&corev1.PersistentVolume{}, pvToTarget).
		Watches(&v1alpha1.VSphereVolumeSource{}, srcToPVCs, builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: maxConcurrent}).
		Complete(r)
}
