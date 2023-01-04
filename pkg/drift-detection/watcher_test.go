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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/klogr"

	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
)

var _ = Describe("Manager: react", func() {
	var watcherCtx context.Context
	var resource corev1.Namespace

	BeforeEach(func() {
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

	It("react: queue resource to be evaluated for configuration drift", func() {
		Expect(driftdetection.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())

		resourceRef := corev1.ObjectReference{
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		gvk := resourceRef.GroupVersionKind()
		driftdetection.React(manager, &gvk, &resource, klogr.New())

		// Since there is no resource of this GVK tracked, resource is queued
		// for configuration drift evaluation
		jobs := manager.GetJobQueue()
		resourceQueued := jobs.Items()
		Expect(resourceQueued).ToNot(ContainElement(resourceRef))

		resourceSummary := getResourceSummary(&resourceRef, nil)
		resourceSummaryRef := getObjRefFromResourceSummary(resourceSummary)
		manager.AddResource(&resourceRef, resourceSummaryRef)
		driftdetection.React(manager, &gvk, &resource, klogr.New())

		// Since resource is now tracked, resource is queued
		// for configuration drift evaluation
		jobs = manager.GetJobQueue()
		resourceQueued = jobs.Items()
		Expect(resourceQueued).To(ContainElement(resourceRef))
	})
})
