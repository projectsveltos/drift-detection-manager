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
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
	"github.com/projectsveltos/libsveltos/lib/sveltos_upgrade"
)

// evaluateConfigurationDrift evaluates all resources awaiting evaluation for configuration drift
func (m *manager) evaluateConfigurationDrift(ctx context.Context, wg *sync.WaitGroup) {
	var once sync.Once

	for {
		// Sleep before next evaluation
		time.Sleep(m.interval)

		m.log.V(logs.LogDebug).Info("Evaluating Configuration drift")

		m.mu.RLock()
		// Get current queued resources
		resources := m.jobQueue.Items()
		// Reset current queue
		m.jobQueue = &libsveltosset.Set{}
		m.mu.RUnlock()

		failedEvaluations := &libsveltosset.Set{}

		for i := range resources {
			logger := m.log.WithValues("resource", fmt.Sprintf("%s/%s", resources[i].Namespace, resources[i].Name))
			logger = logger.WithValues("gvk", resources[i].GroupVersionKind())
			logger.V(logs.LogDebug).Info("Evaluating resource for configuration drift")
			err := m.evaluateResource(ctx, &resources[i])
			if err != nil {
				logger.V(logs.LogInfo).Error(err, "failed to evaluate resource")
				failedEvaluations.Insert(&resources[i])
			}
		}

		// Re-queue all resources whose evaluation failed
		resources = failedEvaluations.Items()
		for i := range failedEvaluations.Items() {
			logger := m.log.WithValues("resource", fmt.Sprintf("%s/%s", resources[i].Namespace, resources[i].Name))
			logger = logger.WithValues("gvk", resources[i].GroupVersionKind())
			logger.V(logs.LogDebug).Info("requeuing resource for evaluation")
			m.mu.Lock()
			m.checkForConfigurationDrift(&resources[i])
			m.mu.Unlock()
		}

		once.Do(func() {
			wg.Done()
		})
	}
}

// evaluateResource evaluates whether resource has drifted. If configuration drift is detected,
// request for Sveltos to reconcile is triggered.
func (m *manager) evaluateResource(ctx context.Context, resourceRef *corev1.ObjectReference) error {
	m.mu.RLock()
	hash, ok := m.resourceHashes[*resourceRef]
	m.mu.RUnlock()

	logger := m.log.WithValues("resource", fmt.Sprintf("%s/%s", resourceRef.Namespace, resourceRef.Name))
	logger = logger.WithValues("gvk", resourceRef.GroupVersionKind())

	if !ok {
		logger.V(logs.LogInfo).Info("resource is not tracked anymore")
		return nil
	}

	u, err := m.getUnstructured(ctx, resourceRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			logger.V(logs.LogInfo).Info("resource has been deleted. Request reconciliation.")
			m.updateResourceHash(resourceRef, nil)
			return m.requestReconciliations(ctx, resourceRef, nil)
		}
		if errors.Is(err, &meta.NoKindMatchError{}) {
			logger.V(logs.LogInfo).Info("CRD has been deleted. Request reconciliation.")
			m.updateResourceHash(resourceRef, nil)
			return m.requestReconciliations(ctx, resourceRef, nil)
		}
		return err
	}

	patches, err := m.getPatches(ctx, resourceRef)
	if err != nil {
		return err
	}

	currentHash, err := m.unstructuredHash(u, patches)
	if err != nil {
		return err
	}

	if !reflect.DeepEqual(hash, currentHash) {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("resource has been modified. Request reconciliation. Old %x -- Current %x",
			hash, currentHash))
		m.updateResourceHash(resourceRef, currentHash)
		return m.requestReconciliations(ctx, resourceRef, currentHash)
	}

	logger.V(logs.LogInfo).Info("no configuration drift detected.")
	return nil
}

func (m *manager) getResourceSummaries(resourceRef *corev1.ObjectReference) *libsveltosset.Set {
	m.mu.RLock()
	defer m.mu.RUnlock()

	rsList := &libsveltosset.Set{}

	resourceSummaries, ok := m.resources[*resourceRef]
	if ok {
		rsList.Append(resourceSummaries)
	}

	resourceSummaries, ok = m.helmResources[*resourceRef]
	if ok {
		rsList.Append(resourceSummaries)
	}

	resourceSummaries, ok = m.kustomizeResources[*resourceRef]
	if ok {
		rsList.Append(resourceSummaries)
	}

	return rsList
}

func (m *manager) getPatches(ctx context.Context, resourceRef *corev1.ObjectReference,
) ([]libsveltosv1beta1.Patch, error) {

	rs := m.getResourceSummaries(resourceRef)
	items := rs.Items()

	patches := make([]libsveltosv1beta1.Patch, 0)
	for i := range items {
		resourceSummaryRef := &items[i]
		resourceSummary := &libsveltosv1beta1.ResourceSummary{}
		err := m.Client.Get(ctx,
			types.NamespacedName{Namespace: resourceSummaryRef.Namespace, Name: resourceSummaryRef.Name},
			resourceSummary)
		if err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to get resourceSummary %s/%s",
				resourceSummaryRef.Namespace, resourceSummaryRef.Name))
			return nil, err
		}

		patches = append(patches, resourceSummary.Spec.Patches...)
	}

	return patches, nil
}

func (m *manager) updateResourceHash(resourceRef *corev1.ObjectReference, currentHash []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resourceHashes[*resourceRef] = currentHash
}

func (m *manager) requestReconciliations(ctx context.Context, resourceRef *corev1.ObjectReference,
	currentHash []byte) error {

	resourceTypes := []ResourceType{Resource, HelmResource, KustomizeResource}

	var resourceSummaries []corev1.ObjectReference
	for _, resourceType := range resourceTypes {
		var ok bool
		var rsList *libsveltosset.Set
		switch resourceType {
		case Resource:
			m.mu.RLock() // Lock only if necessary (see point 4)
			rsList, ok = m.resources[*resourceRef]
			m.mu.RUnlock()
		case HelmResource:
			m.mu.RLock() // Lock only if necessary
			rsList, ok = m.helmResources[*resourceRef]
			m.mu.RUnlock()
		case KustomizeResource:
			m.mu.RLock() // Lock only if necessary
			rsList, ok = m.kustomizeResources[*resourceRef]
			m.mu.RUnlock()
		}
		if !ok {
			continue // Early return if not found
		}
		resourceSummaries = append(resourceSummaries, rsList.Items()...)

		for i := range resourceSummaries {
			l := m.log.WithValues("resourceSummary", fmt.Sprintf("%s/%s",
				resourceSummaries[i].Namespace, resourceSummaries[i].Name))
			l.V(logs.LogDebug).Info("create reconciliation request")

			if err := m.requestReconciliationForResourceSummary(ctx, &resourceSummaries[i], resourceRef,
				currentHash, resourceType); err != nil {
				return err
			}
		}
	}

	return nil
}

// requestReconciliationForResourceSummary fetches ResourceSummary. If found, it updates
// ResourceSummary Status by:
// - marking ResourceSummary for reconciliation (ResourcesChanged, KustomizeResourcesChanged or HelmResourcesChanged
// is set to true based on resourceType input arg);
// - ResourceHashes for resourceRef is updated with current hash.
// Input args:
// - resourceSummaryRef is reference to the ResourceSummary;
// - resourceRef is reference to resource which has drifted;
// - currentHash is current hash of the resource that has drifted;
func (m *manager) requestReconciliationForResourceSummary(ctx context.Context,
	resourceSummaryRef, resourceRef *corev1.ObjectReference,
	currentHash []byte, resourceType ResourceType) error {

	logger := m.log.WithValues("resourceSummary", fmt.Sprintf("%s/%s",
		resourceSummaryRef.Namespace, resourceSummaryRef.Name))
	logger.V(logs.LogDebug).Info("requesting reconciliation")

	// fetch ResourceSummary
	u, err := m.getUnstructured(ctx, resourceSummaryRef)
	if err != nil {
		if apierrors.IsNotFound(err) {
			// If not found, there is nothing to do.
			logger.V(logs.LogInfo).Info("resourceSummary not found")
			return nil
		}
		return err
	}

	// Convert unstructured to typed ResourceSummary
	unstructured := u.UnstructuredContent()
	var resourceSummary libsveltosv1beta1.ResourceSummary
	err = runtime.DefaultUnstructuredConverter.
		FromUnstructured(unstructured, &resourceSummary)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to convert unstructured to ResourceSummary: %v",
			err))
		return err
	}

	// Mark resourceSummary for reconciliation
	switch resourceType {
	case HelmResource:
		resourceSummary.Status.HelmResourcesChanged = true
		resourceSummary.Status.HelmResourceHashes = m.updateResourceSummaryWithHash(
			resourceSummary.Status.HelmResourceHashes, resourceRef, currentHash)
	case KustomizeResource:
		resourceSummary.Status.KustomizeResourcesChanged = true
		resourceSummary.Status.KustomizeResourceHashes = m.updateResourceSummaryWithHash(
			resourceSummary.Status.KustomizeResourceHashes, resourceRef, currentHash)
	case Resource:
		resourceSummary.Status.ResourcesChanged = true
		resourceSummary.Status.ResourceHashes = m.updateResourceSummaryWithHash(
			resourceSummary.Status.ResourceHashes, resourceRef, currentHash)
	}

	return m.Status().Update(ctx, &resourceSummary)
}

func (m *manager) updateResourceSummaryWithHash(resourceHashes []libsveltosv1beta1.ResourceHash,
	resourceRef *corev1.ObjectReference, currentHash []byte) []libsveltosv1beta1.ResourceHash {

	for i := range resourceHashes {
		r := resourceHashes[i]
		objRef := m.getObjectRef(&r.Resource)
		if reflect.DeepEqual(objRef, resourceRef) {
			resourceHashes[i].Hash = string(currentHash)
			break
		}
	}

	return resourceHashes
}

func (m *manager) getObjectRef(resource *libsveltosv1beta1.Resource) *corev1.ObjectReference {
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

func (m *manager) storeVersionForCompatibilityChecks(ctx context.Context, version string, wg *sync.WaitGroup) {
	wg.Wait() // Wait for ResourceSummary to be processed once

	m.log.V(logs.LogInfo).Info(fmt.Sprintf("store version %s for compatibility checks", version))
	for {
		err := sveltos_upgrade.StoreDriftDetectionVersion(ctx, m.Client, version)
		if err == nil {
			break
		}
		m.log.V(logs.LogInfo).Info(fmt.Sprintf("failed to store version %s for compatibility checks: %v", version, err))
		time.Sleep(time.Second)
	}
}
