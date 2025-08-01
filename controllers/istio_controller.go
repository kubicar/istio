/*
Copyright 2022.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"context"
	"fmt"
	"time"

	"github.com/pkg/errors"

	"github.com/kyma-project/istio/operator/internal/resources"

	"github.com/kyma-project/istio/operator/internal/restarter"
	"github.com/kyma-project/istio/operator/internal/restarter/predicates"
	"github.com/kyma-project/istio/operator/internal/validation"

	"github.com/kyma-project/istio/operator/pkg/lib/sidecars"
	"github.com/kyma-project/istio/operator/pkg/lib/sidecars/pods"
	"github.com/kyma-project/istio/operator/pkg/lib/sidecars/restart"

	"k8s.io/client-go/util/retry"

	"github.com/kyma-project/istio/operator/internal/describederrors"
	"github.com/kyma-project/istio/operator/internal/reconciliations/istio/configuration"
	"github.com/kyma-project/istio/operator/internal/reconciliations/istioresources"
	"github.com/kyma-project/istio/operator/internal/status"

	"github.com/kyma-project/istio/operator/internal/istiooperator"

	"golang.org/x/time/rate"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	operatorv1alpha2 "github.com/kyma-project/istio/operator/api/v1alpha2"
	"github.com/kyma-project/istio/operator/internal/reconciliations/istio"
)

const (
	namespace                        = "kyma-system"
	reconciliationRequeueTimeError   = 1 * time.Minute
	reconciliationRequeueTimeWarning = 1 * time.Hour
)

func NewController(mgr manager.Manager, reconciliationInterval time.Duration) *IstioReconciler {
	merger := istiooperator.NewDefaultIstioMerger()

	statusHandler := status.NewStatusHandler(mgr.GetClient())
	logger := mgr.GetLogger()
	podsLister := pods.NewPods(mgr.GetClient(), &logger)
	actionRestarter := restart.NewActionRestarter(mgr.GetClient(), &logger)
	restarters := []restarter.Restarter{
		restarter.NewIngressGatewayRestarter(mgr.GetClient(), []predicates.IngressGatewayPredicate{}, statusHandler),
		restarter.NewSidecarsRestarter(mgr.GetLogger(), mgr.GetClient(), &merger, sidecars.NewProxyRestarter(mgr.GetClient(), podsLister, actionRestarter, &logger), statusHandler),
	}
	userResources := resources.NewUserResources(mgr.GetClient())

	return &IstioReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		istioInstallation:      &istio.Installation{Client: mgr.GetClient(), IstioClient: istio.NewIstioClient(), Merger: &merger},
		istioResources:         istioresources.NewReconciler(mgr.GetClient()),
		userResources:          userResources,
		restarters:             restarters,
		log:                    mgr.GetLogger(),
		statusHandler:          statusHandler,
		reconciliationInterval: reconciliationInterval,
	}
}

//nolint:gocognit,funlen // cognitive complexity 30 of func `(*IstioReconciler).Reconcile` is high (> 20), Function 'Reconcile' has too many statements (58 > 50) TODO: refactor this function
func (r *IstioReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	r.log.Info("Was called to reconcile Kyma Istio Service Mesh")

	istioCR := operatorv1alpha2.Istio{}
	if err := r.Get(ctx, req.NamespacedName, &istioCR); err != nil {
		if apierrors.IsNotFound(err) {
			r.log.Info("Skipped reconciliation, because Istio CR was not found", "request object", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		r.log.Error(err, "Could not get Istio CR")
		return ctrl.Result{}, err
	}

	r.statusHandler.SetCondition(&istioCR, operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileUnknown))

	err := validation.ValidateAuthorizers(istioCR)
	if err != nil {
		return r.terminateReconciliation(ctx, &istioCR, err, operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonValidationFailed))
	}

	if istioCR.GetNamespace() != namespace {
		errWrongNS := fmt.Errorf("istio CR is not in %s namespace", namespace)
		return r.terminateReconciliation(ctx, &istioCR, describederrors.NewDescribedError(errWrongNS, "Stopped Istio CR reconciliation"),
			operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileFailed))
	}

	existingIstioCRs := &operatorv1alpha2.IstioList{}
	if err := r.List(ctx, existingIstioCRs, client.InNamespace(namespace)); err != nil {
		return r.requeueReconciliation(ctx, &istioCR, describederrors.NewDescribedError(err, "Unable to list Istio CRs"),
			operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileFailed),
			reconciliationRequeueTimeError)
	}

	if len(existingIstioCRs.Items) > 1 {
		oldestCr := r.getOldestCR(existingIstioCRs)
		if oldestCr == nil {
			errOldestCRNotFound := errors.New("no oldest Istio CR found")
			return r.terminateReconciliation(
				ctx,
				&istioCR,
				describederrors.NewDescribedError(errOldestCRNotFound, "Stopped Istio CR reconciliation").SetWarning(),
				operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonOldestCRNotFound),
			)
		}

		if istioCR.GetUID() != oldestCr.GetUID() {
			errNotOldestCR := fmt.Errorf("only Istio CR %s in %s reconciles the module", oldestCr.GetName(), oldestCr.GetNamespace())
			return r.terminateReconciliation(ctx, &istioCR, describederrors.NewDescribedError(errNotOldestCR, "Stopped Istio CR reconciliation").SetWarning(),
				operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonOlderCRExists))
		}
	}

	if istioCR.DeletionTimestamp.IsZero() {
		if err := r.statusHandler.UpdateToProcessing(ctx, &istioCR); err != nil {
			r.log.Error(err, "Update status to processing failed")
			// We don't update the status to error, because the status update already failed and to avoid another status update error we simply requeue the request.
			return ctrl.Result{}, err
		}
	} else {
		if err := r.statusHandler.UpdateToDeleting(ctx, &istioCR); err != nil {
			r.log.Error(err, "Update status to deleting failed")
			// We don't update the status to error, because the status update already failed and to avoid another status update error we simply requeue the request.
			return ctrl.Result{}, err
		}
	}

	istioImageVersion, installationErr := r.istioInstallation.Reconcile(ctx, &istioCR, r.statusHandler)
	if installationErr != nil {
		return r.requeueReconciliation(ctx, &istioCR, installationErr,
			operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonIstioInstallUninstallFailed),
			reconciliationRequeueTimeError)
	}

	// If there are no finalizers left, we must assume that the resource is deleted and therefore must stop the reconciliation
	// to prevent accidental read or write to the resource.
	if !istioCR.HasFinalizers() {
		r.log.Info("End reconciliation because all finalizers have been removed")
		return ctrl.Result{}, nil
	}

	resourcesErr := r.istioResources.Reconcile(ctx, istioCR)
	if resourcesErr != nil {
		return r.requeueReconciliation(ctx, &istioCR, resourcesErr,
			operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonCRsReconcileFailed),
			reconciliationRequeueTimeError)
	}

	err, requeue := restarter.Restart(ctx, &istioCR, r.restarters)
	reconciliationRequeueTime := reconciliationRequeueTimeError
	if err != nil {
		if err.Level() == describederrors.Warning {
			reconciliationRequeueTime = reconciliationRequeueTimeWarning
		}
		// We don't want to use the requeueReconciliation function here, since there is condition handling in this function, and we
		// need to clean this up, before we can use it here as conditions are already handled in the restarters.
		statusUpdateErr := r.statusHandler.UpdateToError(ctx, &istioCR, err, reconciliationRequeueTime)
		if statusUpdateErr != nil {
			r.log.Error(statusUpdateErr, "Error during updating status to error")
		}
		if err.Level() == describederrors.Warning {
			r.log.Info("Reconcile requeued")
			return ctrl.Result{RequeueAfter: reconciliationRequeueTime}, nil
		}
		r.log.Info("Reconcile failed")
		return ctrl.Result{}, err
	} else if requeue {
		r.statusHandler.SetCondition(&istioCR, operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileRequeued))
		return r.requeueReconciliationRestartNotFinished(ctx, &istioCR, reconciliationRequeueTime)
	}

	userResErr := r.userResources.DetectUserCreatedEfOnIngress(ctx)
	if userResErr != nil {
		if userResErr.Level() != describederrors.Warning {
			return r.requeueReconciliation(
				ctx,
				&istioCR,
				userResErr,
				operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonIngressTargetingUserResourceDetectionFailed),
				reconciliationRequeueTimeError,
			)
		}
		return r.requeueReconciliation(
			ctx,
			&istioCR,
			userResErr,
			operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonIngressTargetingUserResourceFound),
			reconciliationRequeueTimeWarning,
		)
	}

	return r.finishReconcile(ctx, &istioCR, istioImageVersion.Tag())
}

// requeueReconciliation cancels the reconciliation and requeues the request.
func (r *IstioReconciler) requeueReconciliation(ctx context.Context,
	istioCR *operatorv1alpha2.Istio, err describederrors.DescribedError,
	reason operatorv1alpha2.ReasonWithMessage, requeueAfter time.Duration) (ctrl.Result, error) {
	if err.ShouldSetCondition() {
		r.setConditionForError(istioCR, reason)
	}
	statusUpdateErr := r.statusHandler.UpdateToError(ctx, istioCR, err, requeueAfter)
	if statusUpdateErr != nil {
		r.log.Error(statusUpdateErr, "Error during updating status to error")
	}
	r.log.Info("Reconcile failed")
	return ctrl.Result{}, err
}

func (r *IstioReconciler) requeueReconciliationRestartNotFinished(ctx context.Context, istioCR *operatorv1alpha2.Istio, requeueAfter time.Duration) (ctrl.Result, error) {
	statusUpdateErr := r.statusHandler.UpdateToProcessing(ctx, istioCR)
	if statusUpdateErr != nil {
		r.log.Error(statusUpdateErr, "Error during updating status to processing")
	}
	r.log.Info("Reconcile requeued")
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

// terminateReconciliation stops the reconciliation and does not requeue the request.
func (r *IstioReconciler) terminateReconciliation(ctx context.Context, istioCR *operatorv1alpha2.Istio,
	err describederrors.DescribedError, reason operatorv1alpha2.ReasonWithMessage) (ctrl.Result, error) {
	if err.ShouldSetCondition() {
		r.setConditionForError(istioCR, reason)
	}
	statusUpdateErr := r.statusHandler.UpdateToError(ctx, istioCR, err)
	if statusUpdateErr != nil {
		r.log.Error(statusUpdateErr, "Error during updating status to error")
		// In case the update of the status fails we must requeue the request, because otherwise the Error state is never visible in the CR.
		return ctrl.Result{}, statusUpdateErr
	}

	r.log.Error(err, "Reconcile failed, but won't requeue")
	return ctrl.Result{}, nil
}

func (r *IstioReconciler) finishReconcile(ctx context.Context, istioCR *operatorv1alpha2.Istio, istioTag string) (ctrl.Result, error) {
	if err := r.updateLastAppliedConfiguration(ctx, client.ObjectKeyFromObject(istioCR), istioTag); err != nil {
		describedErr := describederrors.NewDescribedError(err, "Error updating LastAppliedConfiguration")
		return r.requeueReconciliation(ctx, istioCR, describedErr,
			operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileFailed),
			reconciliationRequeueTimeError)
	}

	r.statusHandler.SetCondition(istioCR, operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileSucceeded))
	r.statusHandler.SetCondition(istioCR, operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonIngressTargetingUserResourceNotFound))
	if err := r.validate(istioCR); err != nil {
		return ctrl.Result{}, r.statusHandler.UpdateToError(ctx, istioCR, err)
	}

	if err := r.statusHandler.UpdateToReady(ctx, istioCR); err != nil {
		r.log.Error(err, "Error during updating status to ready")
		return ctrl.Result{}, err
	}
	r.log.Info("Reconcile finished")
	return ctrl.Result{RequeueAfter: r.reconciliationInterval}, nil
}

// +kubebuilder:rbac:groups="",resources=namespaces,verbs=create;get;patch;update
// +kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=operator.kyma-project.io,resources=istios,verbs=create;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=operator.kyma-project.io,resources=istios/finalizers,verbs=update
// +kubebuilder:rbac:groups=operator.kyma-project.io,resources=istios/status,verbs=get;patch;update
// +kubebuilder:rbac:groups=authentication.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=config.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=install.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=networking.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=security.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=telemetry.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=extensions.istio.io,resources=*,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=mutatingwebhookconfigurations;validatingwebhookconfigurations,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions.apiextensions.k8s.io;customresourcedefinitions,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=apps;extensions,resources=daemonsets;deployments;deployments/finalizers;replicasets;statefulsets,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=k8s.cni.cncf.io,resources=networkattachmentdefinitions,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;create;update
// +kubebuilder:rbac:groups=policy,resources=poddisruptionbudgets,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings;roles;rolebindings,verbs=create;deletecollection;delete;get;list;patch;update;watch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterroles,verbs=*
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;create;update;patch
// +kubebuilder:rbac:groups="",resources=configmaps;endpoints;events;namespaces;pods;pods/proxy;pods/portforward;persistentvolumeclaims;secrets;services;serviceaccounts;resourcequotas,verbs=create;deletecollection;delete;get;list;patch;update;watch

//nolint:revive,staticcheck
func (r *IstioReconciler) SetupWithManager(mgr ctrl.Manager, rateLimiter RateLimiter) error {
	r.Config = mgr.GetConfig()

	if err := mgr.GetFieldIndexer().IndexField(context.TODO(), &corev1.Pod{}, "status.phase", func(rawObj client.Object) []string {
		pod, _ := rawObj.(*corev1.Pod)
		return []string{string(pod.Status.Phase)}
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&operatorv1alpha2.Istio{}).
		Watches(&corev1.ConfigMap{}, ElbConfigMapEventHandler{}).
		WithEventFilter(predicate.Or[client.Object](predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{})).
		WithOptions(controller.Options{
			RateLimiter: workqueue.NewTypedMaxOfRateLimiter[ctrl.Request](
				workqueue.NewTypedItemExponentialFailureRateLimiter[ctrl.Request](rateLimiter.BaseDelay,
					rateLimiter.FailureMaxDelay),
				&workqueue.TypedBucketRateLimiter[ctrl.Request]{
					Limiter: rate.NewLimiter(rate.Limit(rateLimiter.Frequency), rateLimiter.Burst),
				},
			),
		}).
		Complete(r)
}

// TemplateRateLimiter implements a rate limiter for a client-go.workqueue.  It has
// both an overall (token bucket) and per-item (exponential) rate limiting.
func TemplateRateLimiter(failureBaseDelay time.Duration, failureMaxDelay time.Duration,
	frequency int, burst int,
) workqueue.TypedRateLimiter[client.Object] {
	return workqueue.NewTypedMaxOfRateLimiter[client.Object](
		workqueue.NewTypedItemExponentialFailureRateLimiter[client.Object](failureBaseDelay, failureMaxDelay),
		&workqueue.TypedBucketRateLimiter[client.Object]{Limiter: rate.NewLimiter(rate.Limit(frequency), burst)})
}

func (r *IstioReconciler) getOldestCR(istioCRs *operatorv1alpha2.IstioList) *operatorv1alpha2.Istio {
	if len(istioCRs.Items) == 0 {
		return nil
	}

	oldest := istioCRs.Items[0]
	for _, item := range istioCRs.Items {
		timestamp := &item.CreationTimestamp
		if !(oldest.CreationTimestamp.Before(timestamp)) {
			oldest = item
		}
	}
	return &oldest
}

func (r *IstioReconciler) updateLastAppliedConfiguration(ctx context.Context, objectKey types.NamespacedName, istioTag string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		lacIstioCR := operatorv1alpha2.Istio{}
		if err := r.Get(ctx, objectKey, &lacIstioCR); err != nil {
			return err
		}
		lastAppliedErr := configuration.UpdateLastAppliedConfiguration(&lacIstioCR, istioTag)
		if lastAppliedErr != nil {
			return lastAppliedErr
		}
		return r.Update(ctx, &lacIstioCR)
	})
}

func (r *IstioReconciler) setConditionForError(istioCR *operatorv1alpha2.Istio, reason operatorv1alpha2.ReasonWithMessage) {
	if !operatorv1alpha2.IsReadyTypeCondition(reason) {
		r.statusHandler.SetCondition(istioCR, operatorv1alpha2.NewReasonWithMessage(operatorv1alpha2.ConditionReasonReconcileFailed))
	}
	r.statusHandler.SetCondition(istioCR, reason)
}
