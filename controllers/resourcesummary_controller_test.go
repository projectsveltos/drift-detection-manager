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
	"k8s.io/klog/v2/textlogger"

	"github.com/projectsveltos/drift-detection-manager/controllers"
	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
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
		helmResources, err := controllers.GetHelmResources(reconciler, context.TODO(), resourceSummary)
		Expect(err).To(BeNil())
		Expect(len(helmResources)).To(Equal(0))
	})

	It("getHelmResources returns resources", func() {
		resourceSummary := getResourceSummary(nil, &resourceRef)

		reconciler := &controllers.ResourceSummaryReconciler{
			Config:                 testEnv.Config,
			Client:                 testEnv.Client,
			Scheme:                 scheme,
			Mux:                    sync.RWMutex{},
			ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
			HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		resources := controllers.GetResources(reconciler, resourceSummary)
		Expect(len(resources)).To(Equal(0))
		helmResources, err := controllers.GetHelmResources(reconciler, context.TODO(), resourceSummary)
		Expect(err).To(BeNil())
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

		logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		// Manager initialization is done within SetupManager. So test calls it directly here.
		Expect(driftdetection.InitializeManager(watcherCtx, logger, testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1beta1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())

		Expect(controllers.UpdateMaps(reconciler, context.TODO(), resourceSummary, logger)).To(Succeed())
		Expect(len(reconciler.ResourceSummaryMap)).To(Equal(1))
		Expect(len(reconciler.HelmResourceSummaryMap)).To(Equal(0))

		Expect(controllers.CleanMaps(reconciler, resourceSummary, logger)).To(Succeed())
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

		currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}
		err = c.Get(context.TODO(), resourceSummaryName, currentResourceSummary)
		Expect(err).ToNot(HaveOccurred())
		Expect(
			controllerutil.ContainsFinalizer(
				currentResourceSummary,
				libsveltosv1beta1.ResourceSummaryFinalizer,
			),
		).Should(BeTrue())
	})

	It("getChartResource collects resources", func() {
		reconciler := &controllers.ResourceSummaryReconciler{
			Config:                 testEnv.Config,
			Client:                 testEnv.Client,
			Scheme:                 scheme,
			Mux:                    sync.RWMutex{},
			ResourceSummaryMap:     make(map[corev1.ObjectReference]*libsveltosset.Set),
			HelmResourceSummaryMap: make(map[corev1.ObjectReference]*libsveltosset.Set),
		}

		resource1 := libsveltosv1beta1.Resource{
			Name:      randomString(),
			Namespace: randomString(),
			Group:     "apps",
			Kind:      "Deployment",
			Version:   "v1",
		}

		resource2 := libsveltosv1beta1.Resource{
			Name:    randomString(),
			Group:   "",
			Kind:    "Service",
			Version: "v1",
		}

		resource3 := libsveltosv1beta1.Resource{
			Name:    randomString(),
			Group:   "apiextensions.k8s.io",
			Kind:    "CustomResourceDefinition",
			Version: "v1",
		}

		helmResources := &libsveltosv1beta1.HelmResources{
			ChartName:        randomString(),
			ReleaseName:      randomString(),
			ReleaseNamespace: randomString(),
			Resources: []libsveltosv1beta1.Resource{
				resource1, resource2, resource3,
			},
		}
		resources, err := controllers.GetChartResource(reconciler, context.TODO(), helmResources)
		Expect(err).To(BeNil())

		// When namespace is set, Resource is taken as it is
		Expect(resources).To(ContainElement(resource1))

		// When namespace is not set, helm chart namespace is used
		tmpResource2 := libsveltosv1beta1.Resource{
			Name:      resource2.Name,
			Namespace: helmResources.ReleaseNamespace,
			Group:     resource2.Group,
			Kind:      resource2.Kind,
			Version:   resource2.Version,
		}
		Expect(resources).To(ContainElement(tmpResource2))

		// Namespace is not set on cluster wide resources
		Expect(resources).To(ContainElement(resource3))

	})
})
