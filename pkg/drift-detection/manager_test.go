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

package driftdetection_test

import (
	"context"
	"reflect"

	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/textlogger"

	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
)

var _ = Describe("Manager: registration", func() {
	var watcherCtx context.Context
	var resource corev1.Namespace
	var logger logr.Logger

	BeforeEach(func() {
		logger = textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		driftdetection.Reset()
		watcherCtx, cancel = context.WithCancel(context.Background())

		resource = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
		}
	})

	AfterEach(func() {
		cancel()
	})

	It("RegisterResource: start tracking a resource. UnRegisterResource stop tracking a resource", func() {
		Expect(testEnv.Create(watcherCtx, &resource)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, &resource)).To(Succeed())

		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())

		Expect(driftdetection.InitializeManager(watcherCtx, logger, testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		resourceRef := corev1.ObjectReference{
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		resourceSummary := getResourceSummary(&resourceRef, nil)
		resourceSummaryRef := getObjRefFromResourceSummary(resourceSummary)
		hash, err := manager.RegisterResource(watcherCtx, &resourceRef, false, resourceSummaryRef)
		Expect(err).To(BeNil())

		resources := manager.GetResources()
		Expect(len(resources)).To(Equal(1))
		consumers := resources[resourceRef]
		Expect(consumers.Len()).To(Equal(1))

		resources = manager.GetHelmResources()
		Expect(len(resources)).To(Equal(0))

		hashes := manager.GetResourceHashes()
		Expect(len(hashes)).To(Equal(1))
		currentHash, ok := hashes[resourceRef]
		Expect(ok).To(BeTrue())
		Expect(reflect.DeepEqual(currentHash, hash)).To(BeTrue())

		watchers := manager.GetWatchers()
		Expect(len(watchers)).To(Equal(1))
		gvk := schema.GroupVersionKind{
			Group:   resource.GroupVersionKind().Group,
			Version: resource.GroupVersionKind().Version,
			Kind:    resource.Kind,
		}
		_, ok = watchers[gvk]
		Expect(ok).To(BeTrue())

		gvks := manager.GetGVKResources()
		Expect(len(gvks)).To(Equal(1))
		gvkResources := gvks[resourceRef.GroupVersionKind()]
		Expect(gvkResources.Len()).To(Equal(1))
		Expect(gvkResources.Items()).To(ContainElement(resourceRef))

		Expect(manager.UnRegisterResource(&resourceRef, false, resourceSummaryRef)).To(Succeed())
		resources = manager.GetResources()
		Expect(len(resources)).To(Equal(0))
		resources = manager.GetHelmResources()
		Expect(len(resources)).To(Equal(0))
		hashes = manager.GetResourceHashes()
		Expect(len(hashes)).To(Equal(0))
		watchers = manager.GetWatchers()
		Expect(len(watchers)).To(Equal(0))
		gvks = manager.GetGVKResources()
		Expect(len(gvks)).To(Equal(0))
	})

	It("readResourceSummaries processes all existing ResourceSummaries", func() {
		Expect(driftdetection.InitializeManager(watcherCtx, logger, testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())

		resourceRef := corev1.ObjectReference{
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		resourceSummary := getResourceSummary(&resourceRef, &resourceRef)

		resourceSummaryNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSummary.Namespace,
			},
		}
		Expect(testEnv.Create(watcherCtx, resourceSummaryNs)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummaryNs)).To(Succeed())

		Expect(testEnv.Create(watcherCtx, resourceSummary)).To(Succeed())
		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummary)).To(Succeed())

		// Update ResourceSummary status
		currentResourceSummary := &libsveltosv1alpha1.ResourceSummary{}
		Expect(testEnv.Get(watcherCtx,
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)).To(Succeed())
		hash := randomString()
		currentResourceSummary.Status.ResourceHashes = []libsveltosv1alpha1.ResourceHash{
			{
				Hash: hash,
				Resource: libsveltosv1alpha1.Resource{
					Kind:      resource.Kind,
					Group:     resource.GroupVersionKind().Group,
					Version:   resource.GroupVersionKind().Version,
					Name:      resource.Name,
					Namespace: resource.Namespace,
				},
			},
		}
		currentResourceSummary.Status.HelmResourceHashes = []libsveltosv1alpha1.ResourceHash{
			{
				Hash: hash,
				Resource: libsveltosv1alpha1.Resource{
					Kind:      resource.Kind,
					Group:     resource.GroupVersionKind().Group,
					Version:   resource.GroupVersionKind().Version,
					Name:      resource.Name,
					Namespace: resource.Namespace,
				},
			},
		}
		Expect(testEnv.Status().Update(watcherCtx, currentResourceSummary)).To(Succeed())

		// wait for cache to sync
		Eventually(func() bool {
			err := testEnv.Get(watcherCtx,
				types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
				currentResourceSummary)
			return err == nil && currentResourceSummary.Status.ResourceHashes != nil &&
				currentResourceSummary.Status.HelmResourceHashes != nil
		}, timeout, pollingInterval).Should(BeTrue())

		Expect(driftdetection.ReadResourceSummaries(manager, watcherCtx)).To(Succeed())

		Expect(len(manager.GetResources())).To(Equal(1))
		Expect(len(manager.GetHelmResources())).To(Equal(1))
		// ResourceSummary Status was set with a random hash for resource.
		// Resource has not been created by this test. So manager needs to detect that condition
		// (resource missing) as potential configuration drift which needs evaluation.
		Expect(manager.GetJobQueue().Len()).To(Equal(1))
	})
})

func getObjRefFromResourceSummary(resourceSummary *libsveltosv1alpha1.ResourceSummary) *corev1.ObjectReference {
	gvk := schema.GroupVersionKind{
		Group:   libsveltosv1alpha1.GroupVersion.Group,
		Version: libsveltosv1alpha1.GroupVersion.Version,
		Kind:    libsveltosv1alpha1.ResourceSummaryKind,
	}

	apiVersion, kind := gvk.ToAPIVersionAndKind()

	return &corev1.ObjectReference{
		Namespace:  resourceSummary.Namespace,
		Name:       resourceSummary.Name,
		APIVersion: apiVersion,
		Kind:       kind,
	}
}
