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
	"sync"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	"github.com/projectsveltos/drift-detection-manager/pkg/scope"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
)

type Mode int64

const (
	SendUpdates Mode = iota
	DoNotSendUpdates
)

// ResourceSummaryReconciler reconciles a ResourceSummary object.
// The goal of this controller is to make sure none of the resources deployed by Sveltos are
// locally modified in the cluster. Logic to achieve such goal is following:
// - When a ClusterSummary deploys resources in a cluster, it creates a ResourceSummary instance
// containing all the resources deployed by the ClusterSummary.;
// - The resourceSummary controller watches for ResourceSummary instances and creates internal maps
// containing the list of resources deployed by Sveltos (various resource maps below) and the list of GVKs to
// watch (GVKClusterSummaries map below);
// - For any GVK a watcher is started. When an instance of the GVK is modified, if the resource is contained
// in the Resources/HelmResources map, ResourceSummary status is updated indicating resource modification (that
// is signal for sveltos to redeploy again to make sure there is no configuration drift)
type ResourceSummaryReconciler struct {
	client.Client
	Config           *rest.Config
	Scheme           *runtime.Scheme
	RunMode          Mode
	ClusterNamespace string
	ClusterName      string
	ClusterType      libsveltosv1alpha1.ClusterType

	// Used to update internal maps and sets
	Mux sync.RWMutex

	// key: ResourceSummary; value: set of resources referenced by ResourceSummary
	// This map is used efficiently stop tracking resources not referenced anynmore
	// by ResourceSummary.
	ResourceSummaryMap map[corev1.ObjectReference]*libsveltosset.Set

	// key: ResourceSummary; value: set of resources referenced by ResourceSummary
	// This map is used efficiently stop tracking resources not referenced anynmore
	// by ResourceSummary.
	HelmResourceSummaryMap map[corev1.ObjectReference]*libsveltosset.Set
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=resourcesummaries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=resourcesummaries/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=resourcesummaries/finalizers,verbs=update

func (r *ResourceSummaryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the ResourceSummary instance
	resourceSummary := &libsveltosv1alpha1.ResourceSummary{}
	err := r.Get(ctx, req.NamespacedName, resourceSummary)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return reconcile.Result{}, nil
		}
		logger.Error(err, "Failed to fetch ResourceSummary")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"Failed to fetch ResourceSummary %s",
			req.NamespacedName,
		)
	}

	resourceSummaryScope, err := scope.NewResourceSummaryScope(scope.ResourceSummaryScopeParams{
		Client:          r.Client,
		Logger:          logger,
		ResourceSummary: resourceSummary,
		ControllerName:  "resourceSummary",
	})
	if err != nil {
		logger.Error(err, "Failed to create resourceSummaryScope")
		return reconcile.Result{}, errors.Wrapf(
			err,
			"unable to create resourceSummary scope for %s",
			req.NamespacedName,
		)
	}

	// Always close the scope when exiting this function so we can persist any
	// ResourceSummary changes.
	defer func() {
		if err := resourceSummaryScope.Close(ctx); err != nil {
			reterr = err
		}
	}()

	// Handle deleted resourceSummary
	if !resourceSummary.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(resourceSummaryScope, logger)
	}

	// Handle non-deleted resourceSummary
	return r.reconcileNormal(ctx, resourceSummaryScope, logger)
}

func (r *ResourceSummaryReconciler) reconcileDelete(
	resourceSummaryScope *scope.ResourceSummaryScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.V(logs.LogDebug).Info("reconcile delete")

	resourceSummary := resourceSummaryScope.ResourceSummary

	// stop tracking resources referenced by ResourceSummary in last iteration.
	// Updates internal maps by removing any entry for this ResourceSummary instance.
	if err := r.cleanMaps(resourceSummary, logger); err != nil {
		return reconcile.Result{}, err
	}

	if controllerutil.ContainsFinalizer(resourceSummary, libsveltosv1alpha1.ResourceSummaryFinalizer) {
		controllerutil.RemoveFinalizer(resourceSummary, libsveltosv1alpha1.ResourceSummaryFinalizer)
	}

	logger.V(logs.LogInfo).Info("reconciliation delete succeeded")
	return ctrl.Result{}, nil
}

func (r *ResourceSummaryReconciler) reconcileNormal(ctx context.Context,
	resourceSummaryScope *scope.ResourceSummaryScope,
	logger logr.Logger,
) (reconcile.Result, error) {

	logger.V(logs.LogDebug).Info("reconcile")

	resourceSummary := resourceSummaryScope.ResourceSummary

	if !controllerutil.ContainsFinalizer(resourceSummary, libsveltosv1alpha1.ResourceSummaryFinalizer) {
		if err := r.addFinalizer(ctx, resourceSummary, logger); err != nil {
			logger.V(logs.LogInfo).Info("failed to update finalizer")
			return reconcile.Result{}, err
		}
	}

	// updates internal maps using resources currently referenced by ResourceSummary.
	// Start tracking all such resources.
	if err := r.updateMaps(ctx, resourceSummary, logger); err != nil {
		logger.V(logs.LogInfo).Info("failed to update maps")
		return reconcile.Result{}, err
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceSummaryReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1alpha1.ResourceSummary{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 15,
		}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	return nil
}

func (r *ResourceSummaryReconciler) addFinalizer(ctx context.Context, resourceSummary *libsveltosv1alpha1.ResourceSummary,
	logger logr.Logger) error {

	// If the SveltosCluster doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(resourceSummary, libsveltosv1alpha1.ResourceSummaryFinalizer)

	helper, err := patch.NewHelper(resourceSummary, r.Client)
	if err != nil {
		logger.Error(err, "failed to create patch Helper")
		return err
	}

	// Register the finalizer immediately to avoid orphaning resourcesummaries resources on delete
	if err := helper.Patch(ctx, resourceSummary); err != nil {
		return errors.Wrapf(
			err,
			"Failed to add finalizer for %s",
			resourceSummary.Name,
		)
	}

	logger.V(logs.LogDebug).Info("added finalizer")
	return nil
}

// getChartResource  gets all resources considering an Helm chart
func (r *ResourceSummaryReconciler) getChartResource(helmResource *libsveltosv1alpha1.HelmResources,
) []libsveltosv1alpha1.Resource {

	resources := make([]libsveltosv1alpha1.Resource, len(helmResource.Resources))

	copy(resources, helmResource.Resources)

	// Helm Resources are taken by addon-controller from helm manifest
	// and passed to drift-detection-manager. There are cases where
	// namespace is not set even for namespace resources. See this
	// for instance: https://github.com/projectsveltos/addon-controller/issues/363
	// Instead of changing addon-controller which would require querying the
	// api-server to understand if a resource is namespace or cluster wide,
	// we add namespace here.
	// From here on, returned slice will only be used in conjunction with
	// dynamic.ResourceInterface which ignores namespace for cluster wide
	// resources
	for i := range resources {
		if resources[i].Namespace == "" {
			resources[i].Namespace = helmResource.ReleaseNamespace
		}
	}

	return resources
}

// getHelmResources gets all resources considering all the Helm charts in a ResourceSummary
func (r *ResourceSummaryReconciler) getHelmResources(resourceSummary *libsveltosv1alpha1.ResourceSummary,
) []libsveltosv1alpha1.Resource {

	resources := make([]libsveltosv1alpha1.Resource, 0)

	for i := range resourceSummary.Spec.ChartResources {
		resources = append(resources, r.getChartResource(&resourceSummary.Spec.ChartResources[i])...)
	}

	return resources
}

// getResources gets all resources in a ResourceSummary
func (r *ResourceSummaryReconciler) getResources(resourceSummary *libsveltosv1alpha1.ResourceSummary,
) []libsveltosv1alpha1.Resource {

	resources := make([]libsveltosv1alpha1.Resource, len(resourceSummary.Spec.Resources))
	copy(resources, resourceSummary.Spec.Resources)

	return resources
}

func (r *ResourceSummaryReconciler) getObjectRef(resource *libsveltosv1alpha1.Resource) *corev1.ObjectReference {
	gvk := schema.GroupVersionKind{
		Group:   resource.Group,
		Version: resource.Version,
		Kind:    resource.Kind,
	}

	apiVersion, _ := gvk.ToAPIVersionAndKind()

	return &corev1.ObjectReference{
		Kind:       resource.Kind,
		Namespace:  resource.Namespace,
		Name:       resource.Name,
		APIVersion: apiVersion,
	}
}

// cleanMaps using resources referenced in last reconciliation by ResourceSummary,
// clean internal maps and stops tracking such resources
func (r *ResourceSummaryReconciler) cleanMaps(resourceSummary *libsveltosv1alpha1.ResourceSummary,
	logger logr.Logger) error {

	r.Mux.Lock()
	defer r.Mux.Unlock()

	policyRef := getKeyFromObject(r.Scheme, resourceSummary)
	manager, err := driftdetection.GetManager()
	if err != nil {
		return err
	}

	logger.V(logs.LogDebug).Info("unregister resources previously programmed")

	resourceSet, ok := r.ResourceSummaryMap[*policyRef]
	if ok {
		resources := resourceSet.Items()
		for i := range resources {
			if err := manager.UnRegisterResource(&resources[i], false, policyRef); err != nil {
				return err
			}
		}
		delete(r.ResourceSummaryMap, *policyRef)
	}

	resourceSet, ok = r.HelmResourceSummaryMap[*policyRef]
	if ok {
		resources := resourceSet.Items()
		for i := range resources {
			if err := manager.UnRegisterResource(&resources[i], true, policyRef); err != nil {
				return err
			}
		}
		delete(r.HelmResourceSummaryMap, *policyRef)
	}

	return nil
}

// updateMaps gets all resources referenced in a ResourceSummary.
// Updates map with all specific resource to watch and starts tracking those
func (r *ResourceSummaryReconciler) updateMaps(ctx context.Context,
	resourceSummary *libsveltosv1alpha1.ResourceSummary, logger logr.Logger) error {

	// Get resources currently listed in ResourceSummary. Both resources deployed because
	// of referenced ConfigMaps/Secrets and resources deployed because of helm charts.
	resources := r.getResources(resourceSummary)
	helmResources := r.getHelmResources(resourceSummary)

	r.Mux.Lock()
	defer r.Mux.Unlock()

	logger.V(logs.LogDebug).Info("register referenced resources")

	resourceHashes, err := r.registerResources(ctx, resources, resourceSummary, false, logger)
	if err != nil {
		return err
	}

	var helmResourceHashes []libsveltosv1alpha1.ResourceHash
	helmResourceHashes, err = r.registerResources(ctx, helmResources, resourceSummary, true, logger)
	if err != nil {
		return err
	}

	// Update current list of resources that needs to be tracked because of
	// ResourceSummary
	currentObjRefs := libsveltosset.Set{}
	for i := range resources {
		currentObjRefs.Insert(r.getObjectRef(&resources[i]))
	}

	currentHelmObjRefs := libsveltosset.Set{}
	for i := range helmResources {
		currentHelmObjRefs.Insert(r.getObjectRef(&helmResources[i]))
	}

	logger.V(logs.LogDebug).Info("unregistered resources not referenced anymore")
	// Stop tracking any resource which was previously referenced by this ResourceSummary
	// but it is not anymore
	policyRef := getKeyFromObject(r.Scheme, resourceSummary)
	oldObjRefs, ok := r.ResourceSummaryMap[*policyRef]
	if ok {
		diff := oldObjRefs.Difference(&currentObjRefs)
		if err := r.unregisterResources(diff, resourceSummary, false); err != nil {
			return err
		}
	}

	oldHelmObjRefs, ok := r.HelmResourceSummaryMap[*policyRef]
	if ok {
		diff := oldHelmObjRefs.Difference(&currentHelmObjRefs)
		if err := r.unregisterResources(diff, resourceSummary, true); err != nil {
			return err
		}
	}

	// Only here update new list of resources referenced by this ResourceSummary.
	// This update must happen after above code that cleans resources previously
	// referenced but not referenced anymore.
	if currentObjRefs.Len() > 0 {
		r.ResourceSummaryMap[*policyRef] = &currentObjRefs
	}
	if currentHelmObjRefs.Len() > 0 {
		r.HelmResourceSummaryMap[*policyRef] = &currentHelmObjRefs
	}

	resourceSummary.Status.ResourceHashes = resourceHashes
	resourceSummary.Status.HelmResourceHashes = helmResourceHashes

	return nil
}

func (r *ResourceSummaryReconciler) registerResources(ctx context.Context,
	resources []libsveltosv1alpha1.Resource, resourceSummary *libsveltosv1alpha1.ResourceSummary,
	isHelm bool, logger logr.Logger) ([]libsveltosv1alpha1.ResourceHash, error) {

	manager, err := driftdetection.GetManager()
	if err != nil {
		return nil, err
	}

	requestor := getKeyFromObject(r.Scheme, resourceSummary)

	logger.V(logs.LogDebug).Info("registered referenced resources")
	resourceHashes := make([]libsveltosv1alpha1.ResourceHash, len(resources))
	hashIndex := 0
	for i := range resources {
		objRef := r.getObjectRef(&resources[i])
		currentHash, err := manager.RegisterResource(ctx, objRef, isHelm, requestor)
		if err != nil {
			return nil, err
		}
		resourceHashes[hashIndex] = libsveltosv1alpha1.ResourceHash{
			Resource: resources[i],
			Hash:     fmt.Sprintf("%x", currentHash),
		}
		hashIndex++
	}

	return resourceHashes, nil
}

func (r *ResourceSummaryReconciler) unregisterResources(resources []corev1.ObjectReference,
	resourceSummary *libsveltosv1alpha1.ResourceSummary, isHelm bool) error {

	manager, err := driftdetection.GetManager()
	if err != nil {
		return err
	}

	requestor := getKeyFromObject(r.Scheme, resourceSummary)

	for i := range resources {
		if err := manager.UnRegisterResource(&resources[i], isHelm, requestor); err != nil {
			return err
		}
	}

	return nil
}
