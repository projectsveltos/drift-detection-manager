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
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"

	"github.com/projectsveltos/libsveltos/lib/logsettings"
	logs "github.com/projectsveltos/libsveltos/lib/logsettings"
)

type ReactToNotification func(gvk *schema.GroupVersionKind, obj interface{}, logger logr.Logger)

// react gets called when an instance of passed in gvk has been modified.
// This method queues all Classifier currently using that gvk to be evaluated.
func (m *manager) react(gvk *schema.GroupVersionKind, obj interface{},
	logger logr.Logger) {

	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		logger.Info(fmt.Sprintf("failed to get namespace key: %v", err))
		return
	}
	logger = logger.WithValues("key", key)

	namespace, name, _ := cache.SplitMetaNamespaceKey(key)

	apiVersion, _ := gvk.ToAPIVersionAndKind()

	objRef := &corev1.ObjectReference{
		Kind:       gvk.Kind,
		APIVersion: apiVersion,
		Namespace:  namespace,
		Name:       name,
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if v, ok := m.resources[*objRef]; ok {
		resourceSummaries := v.Items()
		for i := range resourceSummaries {
			resourceSummary := resourceSummaries[i]

			logger.V(logsettings.LogInfo).Info(
				fmt.Sprintf("Resource in ResourceSummary %s potentially drifted (%s %s/%s)",
					resourceSummary.Name, objRef.Kind, objRef.Namespace, objRef.Name))

			// Potential configuration drift here. Queue resource (along with its hash and ResourceSummaries)
			// to be evaluated. This operation happens in a separate context where errors can be retried.

			// Even if pod restarts by the time operation is queued and before it is processed, all is ok.
			// on restarts, manager will rebuild its internal state by reading ResourceSummary status, fetching
			// tracked resources and detecting any resource whose hash has changed.

			m.checkForConfigurationDrift(objRef)

			// Queuing once is enough
			return
		}
	}

	if v, ok := m.helmResources[*objRef]; ok {
		resourceSummaries := v.Items()
		for i := range resourceSummaries {
			resourceSummary := resourceSummaries[i]

			logger.V(logsettings.LogInfo).Info(
				fmt.Sprintf("Resource in ResourceSummary %s potentially drifted (%s %s/%s)",
					resourceSummary.Name, objRef.Kind, objRef.Namespace, objRef.Name))

			// Potential configuration drift here. Queue resource (along with its hash and ResourceSummaries)
			// to be evaluated. This operation happens in a separate context where errors can be retried.

			// Even if pod restarts by the time operation is queued and before it is processed, all is ok.
			// on restarts, manager will rebuild its internal state by reading ResourceSummary status, fetching
			// tracked resources and detecting any resource whose hash has changed.

			m.checkForConfigurationDrift(objRef)
		}
	}
}

func (m *manager) stopWatcher(gvk schema.GroupVersionKind) {
	if cancel, ok := m.watchers[gvk]; ok {
		logger := m.log.WithValues("gvk", gvk.String())
		logger.V(logs.LogInfo).Info("stop watcher for gvk")
		cancel()
	}
}

func (m *manager) startWatcher(ctx context.Context, gvk *schema.GroupVersionKind,
	react ReactToNotification) error {

	logger := m.log.WithValues("gvk", gvk.String())

	if _, ok := m.watchers[*gvk]; ok {
		logger.V(logsettings.LogDebug).Info("watcher already present")
		return nil
	}

	// dynamic informer needs to be told which type to watch
	dcinformer, err := m.getDynamicInformer(gvk)
	if err != nil {
		logger.Error(err, "Failed to get informer")
		return err
	}

	logger.V(logsettings.LogInfo).Info(fmt.Sprintf("start watcher for gvk %s", gvk))
	watcherCtx, cancel := context.WithCancel(ctx)
	m.watchers[*gvk] = cancel
	go m.runInformer(watcherCtx.Done(), dcinformer.Informer(), gvk, react, logger)
	return nil
}

func (m *manager) getDynamicInformer(gvk *schema.GroupVersionKind) (informers.GenericInformer, error) {
	// Grab a dynamic interface that we can create informers from
	d, err := dynamic.NewForConfig(m.config)
	if err != nil {
		return nil, err
	}
	// Create a factory object that can generate informers for resource types
	factory := dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		d,
		0,
		corev1.NamespaceAll,
		nil,
	)

	dc := discovery.NewDiscoveryClientForConfigOrDie(m.config)
	groupResources, err := restmapper.GetAPIGroupResources(dc)
	if err != nil {
		return nil, err
	}
	mapper := restmapper.NewDiscoveryRESTMapper(groupResources)

	mapping, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		// getDynamicInformer is only called after verifying resource
		// is installed.
		return nil, err
	}

	resourceId := schema.GroupVersionResource{
		Group:    gvk.Group,
		Version:  gvk.Version,
		Resource: mapping.Resource.Resource,
	}

	informer := factory.ForResource(resourceId)
	return informer, nil
}

func (m *manager) runInformer(stopCh <-chan struct{}, s cache.SharedIndexInformer,
	gvk *schema.GroupVersionKind, react ReactToNotification, logger logr.Logger) {

	handlers := cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			// If an object is added, there is nothing to do
		},
		DeleteFunc: func(obj interface{}) {
			logger.V(logsettings.LogDebug).Info("got delete notification")
			react(gvk, obj, logger)
		},
		UpdateFunc: func(oldObj, newObj interface{}) {
			logger.V(logsettings.LogDebug).Info("got update notification")
			react(gvk, newObj, logger)
		},
	}
	s.AddEventHandler(handlers)
	s.Run(stopCh)
}
