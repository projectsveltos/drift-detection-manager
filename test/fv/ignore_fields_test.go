/*
Copyright 2024. projectsveltos.io. All rights reserved.

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

package fv_test

import (
	"context"
	"fmt"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/k8s_utils"
)

const (
	nginxDepl = `apiVersion: apps/v1
kind: Deployment
metadata:
  namespace: %s
  name: %s
spec:
  selector:
    matchLabels:
      app: nginx
  replicas: 2 # tells deployment to run 2 pods matching the template
  template:
    metadata:
      labels:
        app: nginx
    spec:
      containers:
      - name: nginx
        image: nginx:1.14.2
        ports:
        - containerPort: 80`
)

var _ = Describe("Start watcher", Label("FV"), func() {
	const (
		namePrefix = "ignore-fields-"
	)

	It("Mark ResourceSummary for reconciliation using ignore fields", func() {
		namespace := &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{
				Name: namePrefix + randomString(),
			},
		}

		By(fmt.Sprintf("Create namespace %s", namespace.Name))
		Expect(k8sClient.Create(context.TODO(), namespace))

		deplName := randomString()
		By(fmt.Sprintf("Create deployment %s/%s", namespace.Name, deplName))
		deployment, err := k8s_utils.GetUnstructured([]byte(fmt.Sprintf(nginxDepl, namespace.Name, deplName)))
		Expect(err).To(BeNil())
		Expect(k8sClient.Create(context.TODO(), deployment))
		Expect(addTypeInformationToObject(scheme, deployment)).To(Succeed())

		By(fmt.Sprintf("Create resourceSummary referencing deployment %s/%s",
			namespace.Name, deplName))
		By("deployment replicas is marked to be ignored for configuration drift detection")
		Expect(addTypeInformationToObject(scheme, namespace)).To(Succeed())
		resourceRef := corev1.ObjectReference{
			Namespace:  deployment.GetNamespace(),
			Name:       deployment.GetName(),
			Kind:       deployment.GetObjectKind().GroupVersionKind().Kind,
			APIVersion: deployment.GetAPIVersion(),
		}
		resourceSummary := getResourceSummary(&resourceRef, nil, false)
		resourceSummary.Spec.Patches = []libsveltosv1beta1.Patch{
			{
				Patch: `- op: remove
  path: /spec/replicas`,
				Target: &libsveltosv1beta1.PatchSelector{
					Kind:  "Deployment",
					Group: "apps",
				},
			},
		}

		Expect(k8sClient.Create(context.TODO(), resourceSummary)).To(Succeed())

		By(fmt.Sprintf("Modify deployment %s/%s replicas", namespace.Name, deplName))
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			newReplicas := int32(5)
			currentDeployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace.Name, Name: deplName},
				currentDeployment)).To(Succeed())
			currentDeployment.Spec.Replicas = &newReplicas
			return k8sClient.Update(context.TODO(), currentDeployment)
		})
		Expect(err).To(BeNil())

		By(fmt.Sprintf("Verify ResourceSummary %s is NOT marked for reconciliation", resourceSummary.Name))
		Consistently(func() bool {
			currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
				currentResourceSummary)
			return err == nil && !currentResourceSummary.Status.ResourcesChanged
		}, timeout/2, pollingInterval).Should(BeTrue())

		By(fmt.Sprintf("Modify deployment %s/%s annotations", namespace.Name, deplName))
		err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
			currentDeployment := &appsv1.Deployment{}
			Expect(k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: namespace.Name, Name: deplName},
				currentDeployment)).To(Succeed())
			currentDeployment.Annotations = map[string]string{randomString(): randomString()}
			return k8sClient.Update(context.TODO(), currentDeployment)
		})
		Expect(err).To(BeNil())

		By(fmt.Sprintf("Verify ResourceSummary %s is marked for reconciliation", resourceSummary.Name))
		Eventually(func() bool {
			currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
				currentResourceSummary)
			return err == nil && currentResourceSummary.Status.ResourcesChanged
		}, timeout/2, pollingInterval).Should(BeTrue())

		By(fmt.Sprintf("Delete deployment %s/%s", namespace.Name, deplName))
		currentDeployment := &appsv1.Deployment{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: namespace.Name, Name: deplName},
			currentDeployment)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentDeployment)).To(Succeed())

		By(fmt.Sprintf("Verify ResourceSummary %s is marked for reconciliation", resourceSummary.Name))
		Consistently(func() bool {
			currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
				currentResourceSummary)
			return err == nil && currentResourceSummary.Status.ResourcesChanged
		}, timeout/2, pollingInterval).Should(BeTrue())

		By("Delete ResourceSummary")
		currentResourceSummary := &libsveltosv1beta1.ResourceSummary{}
		Expect(k8sClient.Get(context.TODO(),
			types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
			currentResourceSummary)).To(Succeed())
		Expect(k8sClient.Delete(context.TODO(), currentResourceSummary)).To(Succeed())

		By("Verify ResourceSummary is gone")
		Eventually(func() bool {
			err := k8sClient.Get(context.TODO(),
				types.NamespacedName{Namespace: resourceSummary.Namespace, Name: resourceSummary.Name},
				currentResourceSummary)
			return err != nil && apierrors.IsNotFound(err)
		}, timeout, pollingInterval).Should(BeTrue())
	})
})
