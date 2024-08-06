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
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/textlogger"
)

const (
	evaluateTimeout = 120 * 60 // high as we never want to see it
)

var _ = Describe("Manager: drift evaluation", func() {
	var watcherCtx context.Context
	var resource corev1.ServiceAccount
	var resourceSummary *libsveltosv1beta1.ResourceSummary

	BeforeEach(func() {
		driftdetection.Reset()
		watcherCtx, cancel = context.WithCancel(context.Background())

		resource = corev1.ServiceAccount{
			ObjectMeta: metav1.ObjectMeta{
				Name:      randomString(),
				Namespace: randomString(),
			},
		}

		ns := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: resource.Namespace,
			},
		}

		Expect(testEnv.Create(watcherCtx, ns)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, ns)).To(Succeed())

		Expect(testEnv.Create(watcherCtx, &resource)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, &resource)).To(Succeed())

		Expect(addTypeInformationToObject(scheme, &resource)).To(Succeed())
	})

	AfterEach(func() {
		currentResourceSummaryList := &libsveltosv1beta1.ResourceSummaryList{}
		Expect(testEnv.List(context.TODO(), currentResourceSummaryList)).To(Succeed())
		for i := range currentResourceSummaryList.Items {
			Expect(testEnv.Delete(watcherCtx, &currentResourceSummaryList.Items[i])).To(Succeed())
		}

		cancel()
	})

	It("evaluateResource: detects a configuration drift when resource is updated", func() {
		logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		Expect(driftdetection.InitializeManager(watcherCtx, logger, testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1beta1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		resourceRef := corev1.ObjectReference{
			Namespace:  resource.Namespace,
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		By("Prepare test: start tracking resource")
		u, err := driftdetection.GetUnstructured(manager, watcherCtx, &resourceRef)
		Expect(err).To(BeNil())
		Expect(u).ToNot(BeNil())

		// Prepare test. Store resource hash and a resourceSummary referencing one resource
		// (via Resource)
		hash, err := driftdetection.UnstructuredHash(manager, u, nil)
		manager.SetResourceHashes(&resourceRef, hash)
		Expect(err).To(BeNil())

		// Test Patch removing field for purpose of drift detection
		patches2 := []libsveltosv1beta1.Patch{
			{
				Patch: `[{ "op": "remove", "path": "/spec/replicas" }]`,
				Target: &libsveltosv1beta1.PatchSelector{
					Version: "v1",
					Group:   "apps",
					Kind:    "Deployment",
					Name:    "test1",
				},
			}}

		By("Prepare test: create 2 deployments with same metadata but differing replica counts to ignore replica field for drift detection.")
		depl1 := unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "apps/v1",
				"spec": map[string]interface{}{
					"replicas": 3,
				},
				"kind": "Deployment",
				"metadata": map[string]interface{}{
					"name": "test1",
				},
			},
		}
		depl2 := unstructured.Unstructured{
			Object: map[string]interface{}{
				"apiVersion": "apps/v1",
				"spec": map[string]interface{}{
					"replicas": 6,
				},
				"kind": "Deployment",
				"metadata": map[string]interface{}{
					"name": "test1",
				},
			},
		}

		h1, err := driftdetection.UnstructuredHash(manager, &depl1, patches2)
		Expect(err).To(BeNil())
		Expect(h1).ToNot(BeNil())
		h2, err2 := driftdetection.UnstructuredHash(manager, &depl2, patches2)
		Expect(err2).To(BeNil())
		Expect(h2).ToNot(BeNil())
		Expect(h1).To(BeEquivalentTo(h2))

		By("Prepare test: add ResourceSummary and mark it as referencing resource created above")
		resourceSummary = getResourceSummary(nil, &resourceRef, nil)
		resourceSummaryNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSummary.Namespace,
			},
		}
		Expect(testEnv.Create(watcherCtx, resourceSummaryNs)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummaryNs)).To(Succeed())
		Expect(testEnv.Create(watcherCtx, resourceSummary)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummary)).To(Succeed())
		resourceSummaryRef := getObjRefFromResourceSummary(resourceSummary)
		manager.AddKustomizeResource(&resourceRef, resourceSummaryRef)
		By(fmt.Sprintf("Using ResourceSummary %s/%s", resourceSummary.Namespace, resourceSummary.Name))

		By("Verify no drift is detected")
		// Since there has been no change, expect that ResourceSummary is not marked for reconciliation
		Expect(driftdetection.EvaluateResource(manager, watcherCtx, &resourceRef)).To(Succeed())
		verifyResourceSummary(resourceSummary, false, false, false)

		By("Modify resource")
		// Modify resource
		currentSA := &corev1.ServiceAccount{}
		Expect(testEnv.Get(watcherCtx,
			types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name}, currentSA)).To(Succeed())
		currentSA.Labels = map[string]string{randomString(): randomString()}
		Expect(testEnv.Update(watcherCtx, currentSA)).To(Succeed())

		// keep reading ServiceAccount till cache is synced
		Eventually(func() bool {
			err = testEnv.Get(context.TODO(),
				types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name},
				currentSA)
			return err == nil && currentSA.Labels != nil
		}, timeout, pollingInterval).Should(BeTrue())

		By("Verify drift is detected")
		// Since resource has now changed, evaluateResource marks ResourceSummary for reconciliation
		Expect(driftdetection.EvaluateResource(manager, watcherCtx, &resourceRef)).To(Succeed())
		verifyResourceSummary(resourceSummary, false, true, false)
	})

	It("evaluateResource: detects a configuration drift when resource is deleted", func() {
		logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		Expect(driftdetection.InitializeManager(watcherCtx, logger, testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1beta1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		resourceRef := corev1.ObjectReference{
			Namespace:  resource.Namespace,
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		By("Prepare test: start tracking resource")
		u, err := driftdetection.GetUnstructured(manager, watcherCtx, &resourceRef)
		Expect(err).To(BeNil())
		Expect(u).ToNot(BeNil())

		// Prepare test. Store resource hash and a resourceSummary referencing one resource
		// (via Resource)
		hash, err := driftdetection.UnstructuredHash(manager, u, nil)
		Expect(err).To(BeNil())
		manager.SetResourceHashes(&resourceRef, hash)

		By("Prepare test: add ResourceSummary and mark it as referencing resource created above")
		resourceSummary = getResourceSummary(&resourceRef, nil, nil)
		resourceSummaryNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSummary.Namespace,
			},
		}
		Expect(testEnv.Create(watcherCtx, resourceSummaryNs)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummaryNs)).To(Succeed())
		Expect(testEnv.Create(watcherCtx, resourceSummary)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummary)).To(Succeed())
		resourceSummaryRef := getObjRefFromResourceSummary(resourceSummary)
		manager.AddResource(&resourceRef, resourceSummaryRef)
		By(fmt.Sprintf("Using ResourceSummary %s/%s", resourceSummary.Namespace, resourceSummary.Name))

		By("Verify no drift is detected")
		// Since there has been no change, expect that ResourceSummary is not marked for reconciliation
		Expect(driftdetection.EvaluateResource(manager, watcherCtx, &resourceRef)).To(Succeed())
		verifyResourceSummary(resourceSummary, false, false, false)

		By("Delete resource")
		currentSA := &corev1.ServiceAccount{}
		// Delete resource
		Expect(testEnv.Get(watcherCtx,
			types.NamespacedName{Namespace: resource.Namespace, Name: resource.Name},
			currentSA)).To(Succeed())
		Expect(testEnv.Client.Delete(watcherCtx, currentSA)).To(Succeed())

		// keep reading ServiceAccount till cache is synced
		Eventually(func() bool {
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Name: resource.Name}, currentSA)
			return err != nil && apierrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())

		By("Verify drift is detected")
		// Since resource has now changed, evaluateResource marks ResourceSummary for reconciliation
		Expect(driftdetection.EvaluateResource(manager, watcherCtx, &resourceRef)).To(Succeed())
		verifyResourceSummary(resourceSummary, true, false, false)
	})

	It("requestReconciliationForResourceSummary updates ResourceSummary Status", func() {
		logger := textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		Expect(driftdetection.InitializeManager(watcherCtx, logger, testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1beta1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		resourceRef := corev1.ObjectReference{
			Namespace:  resource.Namespace,
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		By("Prepare test: add ResourceSummary and mark it as referencing resource created above")
		resourceSummary = getResourceSummary(nil, nil, &resourceRef)
		resourceSummaryNs := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSummary.Namespace,
			},
		}
		Expect(testEnv.Create(watcherCtx, resourceSummaryNs)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummaryNs)).To(Succeed())
		Expect(testEnv.Create(watcherCtx, resourceSummary)).To(Succeed())
		Expect(waitForObject(watcherCtx, testEnv.Client, resourceSummary)).To(Succeed())
		resourceSummaryRef := getObjRefFromResourceSummary(resourceSummary)
		manager.AddResource(&resourceRef, resourceSummaryRef)
		By(fmt.Sprintf("Using ResourceSummary %s/%s", resourceSummary.Namespace, resourceSummary.Name))

		By("Prepare test: prepare ResourceSummary Status")
		currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)).To(Succeed())
		currentResourceSummary.Status.HelmResourceHashes = []libsveltosv1beta1.ResourceHash{
			{
				Hash: randomString(),
				Resource: libsveltosv1beta1.Resource{
					Namespace: resource.Namespace,
					Name:      resource.Name,
					Group:     resource.GroupVersionKind().Group,
					Version:   resource.GroupVersionKind().Version,
					Kind:      resource.GroupVersionKind().Kind,
				},
			},
		}
		Expect(testEnv.Status().Update(watcherCtx, currentResourceSummary)).To(Succeed())
		// keep reading ResourceSummary till cache is synced
		Eventually(func() bool {
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Name: resourceSummary.Name, Namespace: resourceSummary.Namespace},
				currentResourceSummary)
			return err == nil && currentResourceSummary.Status.HelmResourceHashes != nil
		}, timeout, pollingInterval).Should(BeTrue())

		By("Call RequestReconciliationForResourceSummary")
		hash := []byte(randomString())
		Expect(driftdetection.RequestReconciliationForResourceSummary(manager, watcherCtx, resourceSummaryRef,
			&resourceRef, hash, driftdetection.HelmResource)).To(Succeed())

		By("Verify ResourceSummary is marked for reconciliation")
		verifyResourceSummary(resourceSummary, false, false, true)

		By("Verify ResourceSummary Status is updated")
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)).To(Succeed())
		Expect(currentResourceSummary.Status.HelmResourceHashes).ToNot(BeNil())
		Expect(len(currentResourceSummary.Status.HelmResourceHashes)).To(Equal(1))
		Expect(currentResourceSummary.Status.HelmResourceHashes[0].Hash).To(Equal(string(hash)))
		Expect(currentResourceSummary.Status.HelmResourceHashes[0].Resource.Name).To(Equal(resource.Name))
		Expect(currentResourceSummary.Status.HelmResourceHashes[0].Resource.Namespace).To(Equal(resource.Namespace))
		Expect(currentResourceSummary.Status.HelmResourceHashes[0].Resource.Kind).To(Equal(resource.Kind))
		Expect(currentResourceSummary.Status.HelmResourceHashes[0].Resource.Group).To(Equal(resource.GroupVersionKind().Group))
		Expect(currentResourceSummary.Status.HelmResourceHashes[0].Resource.Version).To(Equal(resource.GroupVersionKind().Version))
	})
})

func verifyResourceSummary(resourceSummary *libsveltosv1beta1.ResourceSummary,
	shouldReconcileResources, shouldReconcileKustomizeResources, shouldReconcileHelmResources bool) {

	currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}

	Eventually(func() bool {
		err := testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)
		if err != nil {
			return false
		}
		return currentResourceSummary.Status.HelmResourcesChanged == shouldReconcileHelmResources &&
			currentResourceSummary.Status.ResourcesChanged == shouldReconcileResources &&
			currentResourceSummary.Status.KustomizeResourcesChanged == shouldReconcileKustomizeResources
	}, timeout, pollingInterval).Should(BeTrue())
}
