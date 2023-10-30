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
	"time"

	"github.com/gdexlab/go-render/render"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2/klogr"

	driftdetection "github.com/projectsveltos/drift-detection-manager/pkg/drift-detection"
	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
)

const (
	evaluateTimeout = 120 * 60 // high as we never want to see it
)

var _ = Describe("Manager: drift evaluation", func() {
	var watcherCtx context.Context
	var resource corev1.ServiceAccount
	var resourceSummary *libsveltosv1alpha1.ResourceSummary

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
		currentResourceSummaryList := &libsveltosv1alpha1.ResourceSummaryList{}
		Expect(testEnv.List(context.TODO(), currentResourceSummaryList)).To(Succeed())
		for i := range currentResourceSummaryList.Items {
			Expect(testEnv.Delete(watcherCtx, &currentResourceSummaryList.Items[i])).To(Succeed())
		}

		cancel()
	})

	It("evaluateResource: detects a configuration drift when resource is updated/deleted", func() {
		By(fmt.Sprintf("using resource %s:%s/%s", resource.Kind, resource.Namespace, resource.Name))
		Expect(driftdetection.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
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

		By(fmt.Sprintf("MGIANLUC %s", render.AsCode(u)))
		time.Sleep(30)

		// Prepare test. Store resource hash and store resourceSummary has one
		// of the ResourceSummary instances referencing resource
		hash := driftdetection.UnstructuredHash(manager, u)
		manager.SetResourceHashes(&resourceRef, hash)

		By("Prepare test: add ResourceSummary and mark it as referencing resource created above")
		resourceSummary = getResourceSummary(&resourceRef, nil)
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

		// MGIANLUC
		u, err = driftdetection.GetUnstructured(manager, watcherCtx, &resourceRef)
		Expect(err).To(BeNil())
		Expect(u).ToNot(BeNil())
		By(fmt.Sprintf("MGIANLUC %s", render.AsCode(u)))
		time.Sleep(60)

		By("Verify no drift is detected")
		// Since there has been no change, expect that ResourceSummary is not marked for reconciliation
		Expect(driftdetection.EvaluateResource(manager, watcherCtx, &resourceRef)).To(Succeed())
		verifyResourceSummary(resourceSummary, false, false)

		Expect(1).To(BeZero())

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
		verifyResourceSummary(resourceSummary, true, false)

		By("Reset ResourceSummary")
		// Reset ResourceSummary so it is not marked for reconciliation anymore
		resetResourceSummary(resourceSummary)

		By("Delete resource")
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
		verifyResourceSummary(resourceSummary, true, false)
	})

	It("requestReconciliationForResourceSummary updates ResourceSummary Status", func() {
		Expect(driftdetection.InitializeManager(watcherCtx, klogr.New(), testEnv.Config, testEnv.Client, scheme,
			randomString(), randomString(), libsveltosv1alpha1.ClusterTypeCapi, evaluateTimeout, false)).To(Succeed())
		manager, err := driftdetection.GetManager()
		Expect(err).To(BeNil())

		resourceRef := corev1.ObjectReference{
			Namespace:  resource.Namespace,
			Name:       resource.Name,
			Kind:       resource.Kind,
			APIVersion: resource.APIVersion,
		}

		By("Prepare test: add ResourceSummary and mark it as referencing resource created above")
		resourceSummary = getResourceSummary(nil, &resourceRef)
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
		currentResourceSummary := &libsveltosv1alpha1.ResourceSummary{}
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)).To(Succeed())
		currentResourceSummary.Status.ResourceHashes = []libsveltosv1alpha1.ResourceHash{
			{
				Hash: randomString(),
				Resource: libsveltosv1alpha1.Resource{
					Namespace: resource.Namespace,
					Name:      resource.Name,
					Group:     resource.GroupVersionKind().Group,
					Version:   resource.GroupVersionKind().Version,
					Kind:      resource.GroupVersionKind().Kind,
				},
			},
		}
		Expect(testEnv.Status().Update(watcherCtx, currentResourceSummary)).To(Succeed())
		// keep reading ServiceAccount till cache is synced
		Eventually(func() bool {
			err := testEnv.Get(context.TODO(),
				types.NamespacedName{Name: resourceSummary.Name, Namespace: resourceSummary.Namespace},
				currentResourceSummary)
			return err == nil && currentResourceSummary.Status.ResourceHashes != nil
		}, timeout, pollingInterval).Should(BeTrue())

		By("Call RequestReconciliationForResourceSummary")
		hash := []byte(randomString())
		Expect(driftdetection.RequestReconciliationForResourceSummary(manager, watcherCtx, resourceSummaryRef,
			&resourceRef, hash, true)).To(Succeed())

		By("Verify ResourceSummary is marked for reconciliation")
		verifyResourceSummary(resourceSummary, false, true)

		By("Verify ResourceSummary Status is updated")
		Expect(testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)).To(Succeed())
		Expect(currentResourceSummary.Status.ResourceHashes).ToNot(BeNil())
		Expect(len(currentResourceSummary.Status.ResourceHashes)).To(Equal(1))
		Expect(currentResourceSummary.Status.ResourceHashes[0].Hash).To(Equal(string(hash)))
		Expect(currentResourceSummary.Status.ResourceHashes[0].Resource.Name).To(Equal(resource.Name))
		Expect(currentResourceSummary.Status.ResourceHashes[0].Resource.Namespace).To(Equal(resource.Namespace))
		Expect(currentResourceSummary.Status.ResourceHashes[0].Resource.Kind).To(Equal(resource.Kind))
		Expect(currentResourceSummary.Status.ResourceHashes[0].Resource.Group).To(Equal(resource.GroupVersionKind().Group))
		Expect(currentResourceSummary.Status.ResourceHashes[0].Resource.Version).To(Equal(resource.GroupVersionKind().Version))
	})
})

func verifyResourceSummary(resourceSummary *libsveltosv1alpha1.ResourceSummary,
	shouldReconcileResources, shouldReconcileHelmResources bool) {

	currentResourceSummary := &libsveltosv1alpha1.ResourceSummary{}

	Eventually(func() bool {
		err := testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)
		if err != nil {
			return false
		}
		return currentResourceSummary.Status.HelmResourcesChanged == shouldReconcileHelmResources &&
			currentResourceSummary.Status.ResourcesChanged == shouldReconcileResources
	}, timeout, pollingInterval).Should(BeTrue())
}

func resetResourceSummary(resourceSummary *libsveltosv1alpha1.ResourceSummary) {
	currentResourceSummary := &libsveltosv1alpha1.ResourceSummary{}

	Expect(testEnv.Get(context.TODO(),
		types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
		currentResourceSummary)).To(Succeed())

	currentResourceSummary.Status.ResourcesChanged = false
	currentResourceSummary.Status.HelmResourcesChanged = false

	Expect(testEnv.Client.Status().Update(context.TODO(), currentResourceSummary)).To(Succeed())

	// wait for cache to sync
	Eventually(func() bool {
		err := testEnv.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)
		if err != nil {
			return false
		}
		return !currentResourceSummary.Status.HelmResourcesChanged &&
			!currentResourceSummary.Status.ResourcesChanged
	}, timeout, pollingInterval).Should(BeTrue())
}
