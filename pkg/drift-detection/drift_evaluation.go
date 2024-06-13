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
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
)

// evaluateConfigurationDrift evaluates all resources awaiting evaluation for configuration drift
func (m *manager) evaluateConfigurationDrift(ctx context.Context) {
	for {
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

		// Sleep before next evaluation
		time.Sleep(m.interval)
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

	currentHash := m.unstructuredHash(u)

	if !reflect.DeepEqual(hash, currentHash) {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("resource has been modified. Request reconciliation. Old %x -- Current %x",
			hash, currentHash))
		m.updateResourceHash(resourceRef, currentHash)
		return m.requestReconciliations(ctx, resourceRef, currentHash)
	}

	logger.V(logs.LogInfo).Info("no configuration drift detected.")
	return nil
}

func (m *manager) updateResourceHash(resourceRef *corev1.ObjectReference, currentHash []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.resourceHashes[*resourceRef] = currentHash
}

func (m *manager) requestReconciliations(ctx context.Context, resourceRef *corev1.ObjectReference,
	currentHash []byte) error {

	var resourceSummaries []corev1.ObjectReference

	// Consider resources
	m.mu.RLock()
	rsList, ok := m.resources[*resourceRef]
	if ok {
		resourceSummaries = rsList.Items()
	}
	m.mu.RUnlock()

	for i := range resourceSummaries {
		l := m.log.WithValues("resourceSummary", fmt.Sprintf("%s/%s",
			resourceSummaries[i].Namespace, resourceSummaries[i].Name))
		l.V(logs.LogDebug).Info("create reconciliation request")
		if err := m.requestReconciliationForResourceSummary(ctx, &resourceSummaries[i],
			resourceRef, currentHash, false); err != nil {
			return err
		}
	}

	resourceSummaries = nil

	// Consider helm resources
	m.mu.RLock()
	rsList, ok = m.helmResources[*resourceRef]
	if ok {
		resourceSummaries = rsList.Items()
	}
	m.mu.RUnlock()

	for i := range resourceSummaries {
		l := m.log.WithValues("resourceSummary", fmt.Sprintf("%s/%s",
			resourceSummaries[i].Namespace, resourceSummaries[i].Name))
		l.V(logs.LogDebug).Info("create reconciliation request")
		if err := m.requestReconciliationForResourceSummary(ctx, &resourceSummaries[i],
			resourceRef, currentHash, true); err != nil {
			return err
		}
	}

	return nil
}

// requestReconciliationForResourceSummary fetches ResourceSummary. If found, it updates
// ResourceSummary Status by:
// - marking ResourceSummary for reconciliation (either ResourcesChanged or HelmResourcesChanged
// is set to true based on isHelm input arg);
// - ResourceHashes for resourceRef is updated with current hash.
// Input args:
// - resourceSummaryRef is reference to the ResourceSummary;
// - resourceRef is reference to resource which has drifted;
// - currentHash is current hash of the resource that has drifted;
func (m *manager) requestReconciliationForResourceSummary(ctx context.Context,
	resourceSummaryRef, resourceRef *corev1.ObjectReference,
	currentHash []byte, isHelm bool) error {

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
	var resourceSummary libsveltosv1alpha1.ResourceSummary
	err = runtime.DefaultUnstructuredConverter.
		FromUnstructured(unstructured, &resourceSummary)
	if err != nil {
		logger.V(logs.LogInfo).Info(fmt.Sprintf("failed to convert unstructured to ResourceSummary: %v",
			err))
		return err
	}

	// Mark resourceSummary for reconciliation
	if isHelm {
		resourceSummary.Status.HelmResourcesChanged = true
	} else {
		resourceSummary.Status.ResourcesChanged = true
	}

	// Update resource hash in ResourceSummary Status
	for i := range resourceSummary.Status.ResourceHashes {
		r := resourceSummary.Status.ResourceHashes[i]
		objRef := m.getObjectRef(&r.Resource)
		if reflect.DeepEqual(objRef, resourceRef) {
			resourceSummary.Status.ResourceHashes[i].Hash = string(currentHash)
			break
		}
	}

	return m.Status().Update(ctx, &resourceSummary)
}

func (m *manager) getObjectRef(resource *libsveltosv1alpha1.Resource) *corev1.ObjectReference {
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
