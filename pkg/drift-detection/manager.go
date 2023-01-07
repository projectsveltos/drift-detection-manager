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

package driftdetection

import (
	"context"
	"crypto/sha256"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/gdexlab/go-render/render"
	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/libsveltos/lib/utils"
)

var (
	getManagerLock  = &sync.Mutex{}
	managerInstance *manager
)

// manager is used to detect configuration drift.
// - Manager is notified about any resource deployed by Sveltos (via RegisterResource method
// and the counterpart UnRegisterResource);
// - Manager starts a watcher for any GVK with at least a resource to watch (corresponding stops
// watcher when there are no more registered resources for a given GVK);
// - When a change happens in a register resource, manager understands if the change is a configuration
// drifit or not (only label/annotation/spec changes are considered configuration drift).
// - In case there is a configuration drift, manager updates ResourceSummary status to indicate that.
type manager struct {
	log logr.Logger

	client.Client
	config *rest.Config
	scheme *runtime.Scheme

	sendUpdates      bool
	clusterNamespace string
	clusterName      string
	clusterType      libsveltosv1alpha1.ClusterType

	mu *sync.RWMutex

	// jobQueue contains name of all Resources instances that need to be evaluated
	// for drift
	jobQueue *libsveltosset.Set

	// interval is the interval at which queued resources are evaluated for configuration
	// drift
	interval time.Duration

	// Contains hash for a resource. This hash is used to detect where a configuration
	// drift has happened.
	// Set first time a resource starts being watched for change and any time a configuration
	// drift is detected and reported.
	resourceHashes map[corev1.ObjectReference][]byte

	// Key: resource to watch, Value: list of ResourceSummary referencing it
	resources map[corev1.ObjectReference]*libsveltosset.Set

	// Key: resource to watch, Value: list of ResourceSummary referencing it
	helmResources map[corev1.ObjectReference]*libsveltosset.Set

	// List of gvk with a watcher
	// Key: GroupResourceVersion currently being watched
	// Value: stop channel
	watchers map[schema.GroupVersionKind]context.CancelFunc

	// key: GVK, Value: list of tracked resources in that GVK
	// GVKs are all the ones to watch.
	gvkResources map[schema.GroupVersionKind]*libsveltosset.Set
}

// InitializeManager initializes a manager
func InitializeManager(ctx context.Context, l logr.Logger, config *rest.Config, c client.Client,
	scheme *runtime.Scheme, clusterNamespace, clusterName string, cluserType libsveltosv1alpha1.ClusterType,
	intervalInSecond uint, sendUpdates bool) error {

	if managerInstance == nil {
		getManagerLock.Lock()
		defer getManagerLock.Unlock()
		if managerInstance == nil {
			l.V(logs.LogInfo).Info("Creating manager now.")
			managerInstance = &manager{log: l, Client: c, config: config, scheme: scheme}
			managerInstance.jobQueue = &libsveltosset.Set{}
			managerInstance.mu = &sync.RWMutex{}

			managerInstance.interval = time.Duration(intervalInSecond) * time.Second

			managerInstance.watchers = make(map[schema.GroupVersionKind]context.CancelFunc)

			managerInstance.sendUpdates = sendUpdates

			managerInstance.clusterNamespace = clusterNamespace
			managerInstance.clusterName = clusterName
			managerInstance.clusterType = cluserType

			managerInstance.resourceHashes = make(map[corev1.ObjectReference][]byte)
			managerInstance.resources = make(map[corev1.ObjectReference]*libsveltosset.Set)
			managerInstance.helmResources = make(map[corev1.ObjectReference]*libsveltosset.Set)
			managerInstance.gvkResources = make(map[schema.GroupVersionKind]*libsveltosset.Set)

			if err := managerInstance.readResourceSummaries(ctx); err != nil {
				managerInstance = nil
				return err
			}

			go managerInstance.evaluateConfigurationDrift(ctx)
		}
	}

	return nil
}

// GetManager returns the manager instance implementing the ClassifierInterface.
// Returns nil if manager has not been initialized yet
func GetManager() (*manager, error) {
	if managerInstance != nil {
		return managerInstance, nil
	}
	return nil, fmt.Errorf("manager not initialized yet")
}

// RegisterResource requests manager to track a resource for possible drift changes.
// isHelmResource indicates whether such resource is deployed by Sveltos because of an helm chart
// (other reason Sveltos deploys a resource is because of referenced ConfigMaps/Secrets)
// Returns resource current hash or an error if any occurs.
func (m *manager) RegisterResource(ctx context.Context, resourceRef *corev1.ObjectReference, isHelmResource bool,
	requestor *corev1.ObjectReference) ([]byte, error) {

	logger := m.log.WithValues("resource", fmt.Sprintf("%s/%s", resourceRef.Namespace, resourceRef.Name))
	logger = logger.WithValues("gvk", resourceRef.GroupVersionKind().String())
	logger = logger.WithValues("requestor", requestor.Name)

	logger.V(logs.LogDebug).Info("track resource")

	m.mu.Lock()
	defer m.mu.Unlock()

	m.trackResource(resourceRef, isHelmResource, requestor)

	v, ok := m.resourceHashes[*resourceRef]
	if ok {
		return v, nil
	}

	u, err := m.getUnstructured(ctx, resourceRef)
	if err != nil {
		return nil, err
	}

	currentHash := m.unstructuredHash(u)
	m.resourceHashes[*resourceRef] = currentHash
	if err := m.updateGVKMapAndStartWatcher(ctx, resourceRef); err != nil {
		return nil, err
	}
	return currentHash, nil
}

func (m *manager) UnRegisterResource(resourceRef *corev1.ObjectReference, isHelmResource bool,
	requestor *corev1.ObjectReference) error {

	logger := m.log.WithValues("resource", fmt.Sprintf("%s/%s", resourceRef.Namespace, resourceRef.Name))
	logger = logger.WithValues("gvk", resourceRef.GroupVersionKind().String())
	logger = logger.WithValues("requestor", requestor.Name)

	logger.V(logs.LogDebug).Info("stop tracking resource")

	m.mu.Lock()
	defer m.mu.Unlock()

	if isHelmResource {
		if _, ok := m.helmResources[*resourceRef]; !ok {
			return nil
		}
		m.helmResources[*resourceRef].Erase(requestor)
		if m.helmResources[*resourceRef].Len() == 0 {
			delete(m.helmResources, *resourceRef)
		}
	} else {
		if _, ok := m.resources[*resourceRef]; !ok {
			return nil
		}
		m.resources[*resourceRef].Erase(requestor)
		if m.resources[*resourceRef].Len() == 0 {
			delete(m.resources, *resourceRef)
		}
	}

	// check if resource is not tracked anymore
	if !m.stillTrackingResource(resourceRef) {
		logger.V(logs.LogInfo).Info("not tracked anymore")
		m.stopTrackingResource(resourceRef)
	}

	return nil
}

func (m *manager) trackResource(resourceRef *corev1.ObjectReference, isHelmResource bool,
	requestor *corev1.ObjectReference) {

	if isHelmResource {
		if _, ok := m.helmResources[*resourceRef]; !ok {
			m.helmResources[*resourceRef] = &libsveltosset.Set{}
		}
		m.helmResources[*resourceRef].Insert(requestor)
		return
	}

	if _, ok := m.resources[*resourceRef]; !ok {
		m.resources[*resourceRef] = &libsveltosset.Set{}
	}
	m.resources[*resourceRef].Insert(requestor)
}

// stillTrackingResource returns true if resource is still
// being tracked. False otherwise
func (m *manager) stillTrackingResource(resourceRef *corev1.ObjectReference) bool {
	if _, ok := m.helmResources[*resourceRef]; ok {
		return true
	}

	_, ok := m.resources[*resourceRef]
	return ok
}

// stopTrackingResource stops tracking a resource.
// If no other resource of the same GVK is being tracked, GVK watcher is also stopped
func (m *manager) stopTrackingResource(resourceRef *corev1.ObjectReference) {
	delete(m.resourceHashes, *resourceRef)

	gvk := resourceRef.GroupVersionKind()
	if _, ok := m.gvkResources[gvk]; ok {
		m.gvkResources[gvk].Erase(resourceRef)
		if m.gvkResources[gvk].Len() == 0 {
			logger := m.log.WithValues("gvk", gvk.String())
			logger.V(logs.LogInfo).Info("stop tracking gvk")
			delete(m.gvkResources, gvk)
			m.stopWatcher(gvk)
			delete(m.watchers, gvk)
		}
	}
}

// updateGVKMapAndStartWatcher updates gvkResources map. For any new GVK, a watcher is started.
func (m *manager) updateGVKMapAndStartWatcher(ctx context.Context, resourceRef *corev1.ObjectReference) error {
	gvk := resourceRef.GroupVersionKind()

	_, ok := m.gvkResources[gvk]
	if !ok {
		m.gvkResources[gvk] = &libsveltosset.Set{}
		if err := m.startWatcher(ctx, &gvk, m.react); err != nil {
			return err
		}
	}
	m.gvkResources[gvk].Insert(resourceRef)
	return nil
}

func (m *manager) getUnstructured(ctx context.Context, resourceRef *corev1.ObjectReference,
) (*unstructured.Unstructured, error) {

	gvk := resourceRef.GroupVersionKind()

	dr, err := utils.GetDynamicResourceInterface(m.config, gvk, resourceRef.Namespace)
	if err != nil {
		return nil, err
	}

	u, err := dr.Get(ctx, resourceRef.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	return u, nil
}

// unstructuredHash returns hash considering *only*:
// - labels
// - annotations
// - spec section
func (m *manager) unstructuredHash(u *unstructured.Unstructured) []byte {
	h := sha256.New()
	var config string

	labels := u.GetLabels()
	if labels != nil {
		config += render.AsCode(labels)
	}

	annotations := u.GetAnnotations()
	if labels != nil {
		config += render.AsCode(annotations)
	}

	content := u.UnstructuredContent()
	if _, ok := content["spec"]; ok {
		config += render.AsCode(content["spec"])
	}

	if _, ok := content["rules"]; ok {
		config += render.AsCode(content["rules"])
	}

	h.Write([]byte(config))
	return h.Sum(nil)
}

// checkForConfigurationDrift queue resource to be evaluated for configuration drift
func (m *manager) checkForConfigurationDrift(resourceRef *corev1.ObjectReference) {
	m.jobQueue.Insert(resourceRef)
}

// readResourceSummaries reads all ResourceSummary and rebuilds internal maps.
//
// Rebuilding hash is needed for this scenario:
// 1. ResourceSummary reconciler register deployment;
// 2. manager fetches deployment, evaluates hash and start tracking Deployment. Manager returns
// hash. ResourceSummary reconciler stores current hash in ResourceSummary Status;
// 3. later on deployment changes, manager queues deployment to be evaluated for configuration
// drift;
// 4. before deployment drift evaluation happens, pod is restarted;
// 5. on restarts, manager fetches last known deployment status (#2). It fetches current deployment
// and detects a configuration drift => queue deployment for evaluation;
// 6. when deployment is evaluated for configuration drift and one is found, only then
// ResourceSummary Status is marked for reconciliation and ResourceSummary Status is updated
// with current deployment hash.
func (m *manager) readResourceSummaries(ctx context.Context) error {
	list := &libsveltosv1alpha1.ResourceSummaryList{}

	if err := m.List(ctx, list); err != nil {
		return err
	}

	for i := range list.Items {
		resourceSummary := list.Items[i]
		if !resourceSummary.DeletionTimestamp.IsZero() {
			continue
		}
		if err := m.readResourceSummary(ctx, &resourceSummary); err != nil {
			return err
		}
	}

	return nil
}

func (m *manager) readResourceSummary(ctx context.Context, resourceSummary *libsveltosv1alpha1.ResourceSummary,
) error {

	if err := m.processResourceHashes(ctx, resourceSummary.Status.ResourceHashes,
		false, resourceSummary); err != nil {
		return err
	}

	if err := m.processResourceHashes(ctx, resourceSummary.Status.HelmResourceHashes,
		true, resourceSummary); err != nil {
		return err
	}

	return nil
}

func (m *manager) processResourceHashes(ctx context.Context, resourceHashes []libsveltosv1alpha1.ResourceHash,
	isHelm bool, resourceSummary *libsveltosv1alpha1.ResourceSummary) error {

	resourceSummaryDef := m.getObjectReference(resourceSummary)

	for i := range resourceHashes {
		resource := resourceHashes[i].Resource
		resourceRef := m.getObjectRef(&resource)
		lastKnownHash := resourceHashes[i].Hash

		currentHash, err := m.RegisterResource(ctx, resourceRef, isHelm, resourceSummaryDef)
		// Override with last known hash
		m.resourceHashes[*resourceRef] = []byte(resourceHashes[i].Hash)

		if err != nil {
			if apierrors.IsNotFound(err) {
				m.log.V(logs.LogInfo).Info(fmt.Sprintf("resource %s/%s not found",
					resourceRef.Namespace, resourceRef.Name))
				m.checkForConfigurationDrift(resourceRef)
				continue
			}
			return err
		}

		if !reflect.DeepEqual(currentHash, lastKnownHash) {
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("resource %s/%s found with different hash",
				resourceRef.Namespace, resourceRef.Name))
			m.checkForConfigurationDrift(resourceRef)
		}
	}

	return nil
}

// getKeyFromObject returns the Key that can be used in the internal reconciler maps.
func (m *manager) getObjectReference(obj client.Object) *corev1.ObjectReference {
	m.addTypeInformationToObject(m.Scheme(), obj)

	return &corev1.ObjectReference{
		Namespace:  obj.GetNamespace(),
		Name:       obj.GetName(),
		Kind:       obj.GetObjectKind().GroupVersionKind().Kind,
		APIVersion: obj.GetObjectKind().GroupVersionKind().String(),
	}
}

func (m *manager) addTypeInformationToObject(scheme *runtime.Scheme, obj client.Object) {
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		panic(1)
	}

	for _, gvk := range gvks {
		if gvk.Kind == "" {
			continue
		}
		if gvk.Version == "" || gvk.Version == runtime.APIVersionInternal {
			continue
		}
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		break
	}
}
