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

package controllers_test

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/klogr"

	"github.com/projectsveltos/drift-detection-manager/controllers"
	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
	libsveltosset "github.com/projectsveltos/libsveltos/lib/set"
)

const (
	evaluateTimeout = 120 * 60 // high as we never want to see it
)

var _ = Describe("ResourceSummary Reconciler", func() {
	var resource corev1.Namespace
	var resourceRef corev1.ObjectReference
	var watcherCtx context.Context
	var watcherCancel context.CancelFunc

	BeforeEach(func() {
		resource = corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: randomString(),
			},
		}

		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())

		resourceRef = corev1.ObjectReference{
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		watcherCtx, watcherCancel = context.WithCancel(ctx)
	})

	AfterEach(func() {
		watcherCancel()
	})

	It("getResources returns resources", func() {
		resourceSummary := getResourceSummary(&resourceRef, nil)

		reconciler := &controllers.ResourceSummaryReconciler{
			Client:                 testEnv.Client,
			Scheme:                 scheme,
			Mux:                    sync.RWMutex{},
			ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
			HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		resources := controllers.GetResources(reconciler, resourceSummary)
		Expect(len(resources)).To(Equal(1))
		helmResources := controllers.GetHelmResources(reconciler, resourceSummary)
		Expect(len(helmResources)).To(Equal(0))
	})

	It("getHelmResources returns resources", func() {
		resourceSummary := getResourceSummary(nil, &resourceRef)

		reconciler := &controllers.ResourceSummaryReconciler{
			Client:                 testEnv.Client,
			Scheme:                 scheme,
			Mux:                    sync.RWMutex{},
			ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
			HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		resources := controllers.GetResources(reconciler, resourceSummary)
		Expect(len(resources)).To(Equal(0))
		helmResources := controllers.GetHelmResources(reconciler, resourceSummary)
		Expect(len(helmResources)).To(Equal(1))
	})

	It("updateMaps updates internal maps/cleanMaps removes ResourceSummary from internal data structures", func() {
		Expect(testEnv.Create(watcherCtx, &resource)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, &resource)).To(Succeed())
		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())

		resourceRef := corev1.ObjectReference{
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}
		resourceSummary := getResourceSummary(&resourceRef, nil)

		reconciler := &controllers.ResourceSummaryReconciler{
			Client:                 testEnv.Client,
			Scheme:                 scheme,
			Mux:                    sync.RWMutex{},
			ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
			HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		// Manager initialization is done within SetupManager. So test calls it directly here.
		Expect(driftdetection.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())

		Expect(controllers.UpdateMaps(reconciler, context.TODO(), resourceSummary, klogr.New())).To(Succeed())
		Expect(len(reconciler.ResourceSummaryMap)).To(Equal(1))
		Expect(len(reconciler.HelmResourceSummaryMap)).To(Equal(0))

		Expect(controllers.CleanMaps(reconciler, resourceSummary, klogr.New())).To(Succeed())
		Expect(len(reconciler.ResourceSummaryMap)).To(Equal(0))
		Expect(len(reconciler.HelmResourceSummaryMap)).To(Equal(0))
	})

	It("Adds finalizer", func() {
		resourceSummary := getResourceSummary(nil, nil)

		initObjects := []client.Object{
			resourceSummary,
		}

		c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjects...).Build()

		reconciler := &controllers.ResourceSummaryReconciler{
			Client:                 c,
			Scheme:                 scheme,
			Mux:                    sync.RWMutex{},
			ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
			HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		resourceSummaryName := client.ObjectKey{
			Name:      resourceSummary.Name,
			Namespace: resourceSummary.Namespace,
		}
		_, err := reconciler.Reconcile(context.TODO(), ctrl.Request{
			NamespacedName: resourceSummaryName,
		})
		Expect(err).ToNot(HaveOccurred())

		currentResourceSummary := &libsveltosv1alpha1.ResourceSummary{}
		err = c.Get(context.TODO(), resourceSummaryName, currentResourceSummary)
		Expect(err).ToNot(HaveOccurred())
		Expect(
			controllerutil.ContainsFinalizer(
				currentResourceSummary,
				libsveltosv1alpha1.ResourceSummaryFinalizer,
			),
		).Should(BeTrue())
	})
})
