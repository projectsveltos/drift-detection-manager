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
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	memory "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"sigs.k8s.io/cluster-api/util/patch"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	"github.com/projectsveltos/drift-detection-manager/pkg/scope"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
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
	ClusterType      libsveltosv1beta1.ClusterType

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

	// key: ResourceSummary; value: set of resources referenced by ResourceSummary
	// This map is used efficiently stop tracking resources not referenced anynmore
	// by ResourceSummary.
	KustomizeResourceSummaryMap map[corev1.ObjectReference]*libsveltosset.Set

	mapper     *restmapper.DeferredDiscoveryRESTMapper
	MapperLock sync.Mutex
}

//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=resourcesummaries,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=resourcesummaries/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=lib.projectsveltos.io,resources=resourcesummaries/finalizers,verbs=update

func (r *ResourceSummaryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (_ ctrl.Result, reterr error) {
	logger := ctrl.LoggerFrom(ctx)
	logger.V(logs.LogInfo).Info("Reconciling")

	// Fecth the ResourceSummary instance
	resourceSummary := &libsveltosv1beta1.ResourceSummary{}
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
		return reconcile.Result{}, r.reconcileDelete(resourceSummaryScope, logger)
	}

	// Handle non-deleted resourceSummary
	return reconcile.Result{}, r.reconcileNormal(ctx, resourceSummaryScope, logger)
}

func (r *ResourceSummaryReconciler) reconcileDelete(
	resourceSummaryScope *scope.ResourceSummaryScope,
	logger logr.Logger,
) error {

	logger.V(logs.LogDebug).Info("reconcile delete")

	resourceSummary := resourceSummaryScope.ResourceSummary

	// stop tracking resources referenced by ResourceSummary in last iteration.
	// Updates internal maps by removing any entry for this ResourceSummary instance.
	if err := r.cleanMaps(resourceSummary, logger); err != nil {
		return err
	}

	if controllerutil.ContainsFinalizer(resourceSummary, libsveltosv1beta1.ResourceSummaryFinalizer) {
		controllerutil.RemoveFinalizer(resourceSummary, libsveltosv1beta1.ResourceSummaryFinalizer)
	}

	logger.V(logs.LogInfo).Info("reconciliation delete succeeded")
	return nil
}

func (r *ResourceSummaryReconciler) reconcileNormal(ctx context.Context,
	resourceSummaryScope *scope.ResourceSummaryScope,
	logger logr.Logger,
) error {

	logger.V(logs.LogDebug).Info("reconcile")

	resourceSummary := resourceSummaryScope.ResourceSummary

	if !controllerutil.ContainsFinalizer(resourceSummary, libsveltosv1beta1.ResourceSummaryFinalizer) {
		if err := r.addFinalizer(ctx, resourceSummary, logger); err != nil {
			logger.V(logs.LogInfo).Info("failed to update finalizer")
			return err
		}
	}

	// updates internal maps using resources currently referenced by ResourceSummary.
	// Start tracking all such resources.
	if err := r.updateMapsAndResourceSummaryStatus(ctx, resourceSummary, logger); err != nil {
		logger.V(logs.LogInfo).Info("failed to update maps")
		return err
	}

	logger.V(logs.LogInfo).Info("reconciliation succeeded")
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ResourceSummaryReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager) error {
	_, err := ctrl.NewControllerManagedBy(mgr).
		For(&libsveltosv1beta1.ResourceSummary{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 15,
		}).
		Build(r)
	if err != nil {
		return errors.Wrap(err, "error creating controller")
	}

	return nil
}

func (r *ResourceSummaryReconciler) addFinalizer(ctx context.Context, resourceSummary *libsveltosv1beta1.ResourceSummary,
	logger logr.Logger) error {

	// If the SveltosCluster doesn't have our finalizer, add it.
	controllerutil.AddFinalizer(resourceSummary, libsveltosv1beta1.ResourceSummaryFinalizer)

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
func (r *ResourceSummaryReconciler) getChartResource(ctx context.Context,
	helmResource *libsveltosv1beta1.HelmResources) ([]libsveltosv1beta1.Resource, error) {

	resources := make([]libsveltosv1beta1.Resource, len(helmResource.Resources))

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
		if resources[i].Namespace != "" {
			continue
		}
		gvk := schema.GroupVersionKind{
			Group:   resources[i].Group,
			Kind:    resources[i].Kind,
			Version: resources[i].Version,
		}

		mapper, err := r.getRESTMapper()
		if err != nil {
			return nil, err
		}
		var mapping *meta.RESTMapping
		mapping, err = mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return nil, err
		}
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resources[i].Namespace = helmResource.ReleaseNamespace
		}
	}

	return resources, nil
}

// getKustomizeResources gets all resources considering all the KustomizeResources in a ResourceSummary
func (r *ResourceSummaryReconciler) getKustomizeResources(resourceSummary *libsveltosv1beta1.ResourceSummary,
) []libsveltosv1beta1.Resource {

	resources := make([]libsveltosv1beta1.Resource, len(resourceSummary.Spec.KustomizeResources))
	copy(resources, resourceSummary.Spec.KustomizeResources)

	return resources
}

// getHelmResources gets all resources considering all the Helm charts in a ResourceSummary
func (r *ResourceSummaryReconciler) getHelmResources(ctx context.Context,
	resourceSummary *libsveltosv1beta1.ResourceSummary) ([]libsveltosv1beta1.Resource, error) {

	resources := make([]libsveltosv1beta1.Resource, 0)

	for i := range resourceSummary.Spec.ChartResources {
		chartResources, err := r.getChartResource(ctx, &resourceSummary.Spec.ChartResources[i])
		if err != nil {
			return nil, err
		}
		resources = append(resources, chartResources...)
	}

	return resources, nil
}

// getResources gets all resources in a ResourceSummary
func (r *ResourceSummaryReconciler) getResources(resourceSummary *libsveltosv1beta1.ResourceSummary,
) []libsveltosv1beta1.Resource {

	resources := make([]libsveltosv1beta1.Resource, len(resourceSummary.Spec.Resources))
	copy(resources, resourceSummary.Spec.Resources)

	return resources
}

func (r *ResourceSummaryReconciler) getObjectRef(resource *libsveltosv1beta1.Resource) *corev1.ObjectReference {
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

func (r *ResourceSummaryReconciler) unregisterResources(resourceSummary *libsveltosv1beta1.ResourceSummary,
	resources []corev1.ObjectReference, resourceType driftdetection.ResourceType) error {

	manager, err := driftdetection.GetManager()
	if err != nil {
		return err
	}

	for i := range resources {
		if err := manager.UnRegisterResource(&resources[i], resourceType, resourceSummary); err != nil {
			return err
		}
	}

	return nil
}

// cleanMaps using resources referenced in last reconciliation by ResourceSummary,
// clean internal maps and stops tracking such resources
func (r *ResourceSummaryReconciler) cleanMaps(resourceSummary *libsveltosv1beta1.ResourceSummary,
	logger logr.Logger) error {

	logger.V(logs.LogDebug).Info("unregister resources previously programmed")

	r.Mux.Lock()
	defer r.Mux.Unlock()

	resourceSet := r.getMapValue(resourceSummary, driftdetection.Resource)
	if resourceSet != nil {
		resources := resourceSet.Items()
		if err := r.unregisterResources(resourceSummary, resources, driftdetection.Resource); err != nil {
			return err
		}
	}
	r.removeMapEntry(resourceSummary, driftdetection.Resource)

	resourceSet = r.getMapValue(resourceSummary, driftdetection.KustomizeResource)
	if resourceSet != nil {
		resources := resourceSet.Items()
		if err := r.unregisterResources(resourceSummary, resources, driftdetection.KustomizeResource); err != nil {
			return err
		}
	}
	r.removeMapEntry(resourceSummary, driftdetection.KustomizeResource)

	resourceSet = r.getMapValue(resourceSummary, driftdetection.HelmResource)
	if resourceSet != nil {
		resources := resourceSet.Items()
		if err := r.unregisterResources(resourceSummary, resources, driftdetection.HelmResource); err != nil {
			return err
		}
	}
	r.removeMapEntry(resourceSummary, driftdetection.HelmResource)

	return nil
}

func (r *ResourceSummaryReconciler) updateMapsForResourceType(ctx context.Context, resourceSummary *libsveltosv1beta1.ResourceSummary,
	currentResources []libsveltosv1beta1.Resource, oldResources *libsveltosset.Set, resourceType driftdetection.ResourceType,
	logger logr.Logger) error {

	logger.V(logs.LogDebug).Info(fmt.Sprintf("register referenced resources for type %d", resourceType))
	resourceHashes, err := r.registerResources(ctx, currentResources, resourceSummary, resourceType, logger)
	if err != nil {
		return err
	}

	currentObjRefs := libsveltosset.Set{}
	for i := range currentResources {
		currentObjRefs.Insert(r.getObjectRef(&currentResources[i]))
	}

	logger.V(logs.LogDebug).Info(fmt.Sprintf("unregistered resources not referenced anymore for type %d", resourceType))

	if oldResources != nil {
		diff := oldResources.Difference(&currentObjRefs)
		if err := r.unregisterResources(resourceSummary, diff, resourceType); err != nil {
			return err
		}
	}

	// This update must happen after above code that cleans resources previously
	// referenced but not referenced anymore.
	switch resourceType {
	case driftdetection.Resource:
		resourceSummary.Status.ResourceHashes = resourceHashes
		r.setMapValue(resourceSummary, driftdetection.Resource, &currentObjRefs)
	case driftdetection.KustomizeResource:
		resourceSummary.Status.KustomizeResourceHashes = resourceHashes
		r.setMapValue(resourceSummary, driftdetection.KustomizeResource, &currentObjRefs)
	case driftdetection.HelmResource:
		resourceSummary.Status.HelmResourceHashes = resourceHashes
		r.setMapValue(resourceSummary, driftdetection.HelmResource, &currentObjRefs)
	}

	return nil
}

// updateMaps gets all resources referenced in a ResourceSummary.
// Updates map with all specific resource to watch and starts tracking those
func (r *ResourceSummaryReconciler) updateMapsAndResourceSummaryStatus(ctx context.Context,
	resourceSummary *libsveltosv1beta1.ResourceSummary, logger logr.Logger) error {

	// Get resources currently listed in ResourceSummary. Resources deployed because
	// of PolicyRefs, Kustomize and Helm charts.
	resources := r.getResources(resourceSummary)
	kustomizeResources := r.getKustomizeResources(resourceSummary)
	helmResources, err := r.getHelmResources(ctx, resourceSummary)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to get helm resources: %v", err))
		return err
	}

	r.Mux.Lock()
	defer r.Mux.Unlock()

	err = r.updateMapsForResourceType(ctx, resourceSummary, resources,
		r.getMapValue(resourceSummary, driftdetection.Resource), driftdetection.Resource, logger)
	if err != nil {
		return err
	}

	err = r.updateMapsForResourceType(ctx, resourceSummary, kustomizeResources,
		r.getMapValue(resourceSummary, driftdetection.KustomizeResource), driftdetection.KustomizeResource, logger)
	if err != nil {
		return err
	}

	err = r.updateMapsForResourceType(ctx, resourceSummary, helmResources,
		r.getMapValue(resourceSummary, driftdetection.HelmResource), driftdetection.HelmResource, logger)
	if err != nil {
		return err
	}

	return nil
}

func (r *ResourceSummaryReconciler) registerResources(ctx context.Context,
	resources []libsveltosv1beta1.Resource, resourceSummary *libsveltosv1beta1.ResourceSummary,
	resourceType driftdetection.ResourceType, logger logr.Logger) ([]libsveltosv1beta1.ResourceHash, error) {

	manager, err := driftdetection.GetManager()
	if err != nil {
		return nil, err
	}

	logger.V(logs.LogDebug).Info("register referenced resources")
	resourceHashes := make([]libsveltosv1beta1.ResourceHash, 0)
	for i := range resources {
		if resources[i].IgnoreForConfigurationDrift {
			l := logger.WithValues("kind", resources[i].Kind)
			l = l.WithValues("group", resources[i].Group)
			l = l.WithValues("resource", fmt.Sprintf("%s/%s", resources[i].Namespace, resources[i].Name))
			l.V(logs.LogInfo).Info("do not track resource for configuration drift")
			continue
		}

		objRef := r.getObjectRef(&resources[i])
		currentHash, err := manager.RegisterResource(ctx, objRef, resourceType, resourceSummary)
		if err != nil {
			if !apierrors.IsNotFound(err) {
				return nil, err
			}
			currentHash = []byte("")
		}
		resourceHashes = append(resourceHashes, libsveltosv1beta1.ResourceHash{
			Resource: resources[i],
			Hash:     fmt.Sprintf("%x", currentHash),
		})
	}

	return resourceHashes, nil
}

// getRESTMapper returns DeferredDiscoveryRESTMapper for a given GVK
func (r *ResourceSummaryReconciler) getRESTMapper() (*restmapper.DeferredDiscoveryRESTMapper, error) {
	if r.mapper == nil {
		r.MapperLock.Lock()
		defer r.MapperLock.Unlock()
		if r.mapper == nil {
			dc, err := discovery.NewDiscoveryClientForConfig(r.Config)
			if err != nil {
				return nil, err
			}
			r.mapper = restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(dc))
		}
	}

	return r.mapper, nil
}

func (r *ResourceSummaryReconciler) removeMapEntry(resourceSummary *libsveltosv1beta1.ResourceSummary,
	resourceType driftdetection.ResourceType) {

	resourceSummaryRef := driftdetection.GetObjectReference(r.Scheme, resourceSummary)

	switch resourceType {
	case driftdetection.Resource:
		delete(r.ResourceSummaryMap, *resourceSummaryRef)
	case driftdetection.KustomizeResource:
		delete(r.KustomizeResourceSummaryMap, *resourceSummaryRef)
	case driftdetection.HelmResource:
		delete(r.HelmResourceSummaryMap, *resourceSummaryRef)
	}
}

func (r *ResourceSummaryReconciler) getMapValue(resourceSummary *libsveltosv1beta1.ResourceSummary,
	resourceType driftdetection.ResourceType) *libsveltosset.Set {

	resourceSummaryRef := driftdetection.GetObjectReference(r.Scheme, resourceSummary)

	var value *libsveltosset.Set
	switch resourceType {
	case driftdetection.Resource:
		value = r.ResourceSummaryMap[*resourceSummaryRef]
	case driftdetection.KustomizeResource:
		value = r.KustomizeResourceSummaryMap[*resourceSummaryRef]
	case driftdetection.HelmResource:
		value = r.HelmResourceSummaryMap[*resourceSummaryRef]
	}

	return value
}

func (r *ResourceSummaryReconciler) setMapValue(resourceSummary *libsveltosv1beta1.ResourceSummary,
	resourceType driftdetection.ResourceType, value *libsveltosset.Set) {

	resourceSummaryRef := driftdetection.GetObjectReference(r.Scheme, resourceSummary)

	switch resourceType {
	case driftdetection.Resource:
		r.ResourceSummaryMap[*resourceSummaryRef] = value
	case driftdetection.KustomizeResource:
		r.KustomizeResourceSummaryMap[*resourceSummaryRef] = value
	case driftdetection.HelmResource:
		r.HelmResourceSummaryMap[*resourceSummaryRef] = value
	}
}
