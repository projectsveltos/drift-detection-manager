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

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
)

func Reset() {
	managerInstance = nil
}

func (m *manager) GetResources() map[corev1.ObjectReference]*libsveltosset.Set {
	return m.resources
}

func (m *manager) AddResource(resource, requestor *corev1.ObjectReference) {
	m.resources[*resource] = &libsveltosset.Set{}
	m.resources[*resource].Insert(requestor)
}

func (m *manager) GetHelmResources() map[corev1.ObjectReference]*libsveltosset.Set {
	return m.helmResources
}

func (m *manager) GetKustomizeResources() map[corev1.ObjectReference]*libsveltosset.Set {
	return m.kustomizeResources
}

func (m *manager) AddKustomizeResource(resource, requestor *corev1.ObjectReference) {
	m.kustomizeResources[*resource] = &libsveltosset.Set{}
	m.kustomizeResources[*resource].Insert(requestor)
}

func (m *manager) GetResourceHashes() map[corev1.ObjectReference][]byte {
	return m.resourceHashes
}

func (m *manager) SetResourceHashes(resource *corev1.ObjectReference, hash []byte) {
	m.resourceHashes[*resource] = hash
}

func (m *manager) GetWatchers() map[schema.GroupVersionKind]context.CancelFunc {
	return m.watchers
}

func (m *manager) GetGVKResources() map[schema.GroupVersionKind]*libsveltosset.Set {
	return m.gvkResources
}

func (m *manager) GetJobQueue() *libsveltosset.Set {
	return m.jobQueue
}

var (
	React                                   = (*manager).react
	UnstructuredHash                        = (*manager).unstructuredHash
	GetUnstructured                         = (*manager).getUnstructured
	EvaluateResource                        = (*manager).evaluateResource
	RequestReconciliationForResourceSummary = (*manager).requestReconciliationForResourceSummary
	ReadResourceSummaries                   = (*manager).readResourceSummaries
)
