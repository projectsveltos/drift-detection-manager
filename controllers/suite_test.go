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
	"fmt"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	"github.com/pkg/errors"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/wait"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/klog/v2"
	"sigs.k8s.io/cluster-api/util"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/projectsveltos/drift-detection-manager/internal/test/helpers"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
	"github.com/projectsveltos/libsveltos/lib/crd"
	"github.com/projectsveltos/libsveltos/lib/utils"
)

var (
	testEnv *helpers.TestEnvironment
	cancel  context.CancelFunc
	ctx     context.Context
	scheme  *runtime.Scheme
)

var (
	cacheSyncBackoff = wait.Backoff{
		Duration: 100 * time.Millisecond,
		Factor:   1.5,
		Steps:    8,
		Jitter:   0.4,
	}
)

func TestControllers(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Classification Suite")
}

var _ = BeforeSuite(func() {
	By("bootstrapping test environment")

	ctrl.SetLogger(klog.Background())

	ctx, cancel = context.WithCancel(context.TODO())

	var err error
	scheme, err = setupScheme()
	Expect(err).To(BeNil())

	testEnvConfig := helpers.NewTestEnvironmentConfiguration([]string{}, scheme)
	testEnv, err = testEnvConfig.Build(scheme)
	if err != nil {
		panic(err)
	}

	go func() {
		By("Starting the manager")
		err = testEnv.StartManager(ctx)
		if err != nil {
			panic(fmt.Sprintf("Failed to start the envtest manager: %v", err))
		}
	}()

	resourceSummaryCRD, err := utils.GetUnstructured(crd.GetResourceSummaryCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, resourceSummaryCRD)).To(Succeed())
	Expect(waitForObject(ctx, testEnv.Client, resourceSummaryCRD)).To(Succeed())

	dcCRD, err := utils.GetUnstructured(crd.GetDebuggingConfigurationCRDYAML())
	Expect(err).To(BeNil())
	Expect(testEnv.Create(ctx, dcCRD)).To(Succeed())
	Expect(waitForObject(ctx, testEnv.Client, dcCRD)).To(Succeed())

	time.Sleep(time.Second)

	if synced := testEnv.GetCache().WaitForCacheSync(ctx); !synced {
		time.Sleep(time.Second)
	}
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).ToNot(HaveOccurred())
})

func randomString() string {
	const length = 10
	return util.RandomString(length)
}

func setupScheme() (*runtime.Scheme, error) {
	s := runtime.NewScheme()
	if err := libsveltosv1beta1.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		return nil, err
	}
	if err := apiextensionsv1.AddToScheme(s); err != nil {
		return nil, err
	}
	return s, nil
}

// waitForObject waits for the cache to be updated helps in preventing test flakes due to the cache sync delays.
func waitForObject(ctx context.Context, c client.Client, obj client.Object) error {
	// Makes sure the cache is updated with the new object
	objCopy := obj.DeepCopyObject().(client.Object)
	key := client.ObjectKeyFromObject(obj)
	if err := wait.ExponentialBackoff(
		cacheSyncBackoff,
		func() (done bool, err error) {
			if err := c.Get(ctx, key, objCopy); err != nil {
				if apierrors.IsNotFound(err) {
					return false, nil
				}
				return false, err
			}
			return true, nil
		}); err != nil {
		return errors.Wrapf(err, "object %s, %s is not being added to the testenv client cache",
			obj.GetObjectKind().GroupVersionKind().String(), key)
	}
	return nil
}

func getResourceSummary(resource, kustomizeResource, helmResource *corev1.ObjectReference,
) *libsveltosv1beta1.ResourceSummary {

	rs := &libsveltosv1beta1.ResourceSummary{
		ObjectMeta: metav1.ObjectMeta{
			Name:      randomString(),
			Namespace: randomString(),
		},
	}

	if resource != nil {
		rs.Spec.Resources = []libsveltosv1beta1.Resource{
			{
				Name:      resource.Name,
				Namespace: resource.Namespace,
				Kind:      resource.Kind,
				Group:     resource.GroupVersionKind().Group,
				Version:   resource.GroupVersionKind().Version,
			},
		}
	}

	if kustomizeResource != nil {
		rs.Spec.KustomizeResources = []libsveltosv1beta1.Resource{
			{
				Name:      kustomizeResource.Name,
				Namespace: kustomizeResource.Namespace,
				Kind:      kustomizeResource.Kind,
				Group:     kustomizeResource.GroupVersionKind().Group,
				Version:   kustomizeResource.GroupVersionKind().Version,
			},
		}
	}

	if helmResource != nil {
		rs.Spec.ChartResources = []libsveltosv1beta1.HelmResources{
			{
				ChartName:        randomString(),
				ReleaseName:      randomString(),
				ReleaseNamespace: randomString(),
				Resources: []libsveltosv1beta1.Resource{
					{
						Name:      helmResource.Name,
						Namespace: helmResource.Namespace,
						Kind:      helmResource.Kind,
						Group:     helmResource.GroupVersionKind().Group,
						Version:   helmResource.GroupVersionKind().Version,
					},
				},
			},
		}
	}

	Expect(addTypeInformationToObject(scheme, rs)).To(Succeed())

	return rs
}

func addTypeInformationToObject(scheme *runtime.Scheme, obj client.Object) error {
	gvks, _, err := scheme.ObjectKinds(obj)
	if err != nil {
		return fmt.Errorf("missing apiVersion or kind and cannot assign it; %w", err)
	}

	for _, gvk := range gvks {
		if gvk.Kind == "" {
			continue
		}
		if gvk.Version == "" || gvk.Version == runtime.APIVersionInternal {
			continue
		}
		obj.GetObjectKind().SetGroupVersionKind(gvk)
		break
	}

	return nil
}

func getObjRefFromResourceSummary(resourceSummary *libsveltosv1beta1.ResourceSummary) *corev1.ObjectReference {
	gvk := schema.GroupVersionKind{
		Group:   libsveltosv1beta1.GroupVersion.Group,
		Version: libsveltosv1beta1.GroupVersion.Version,
		Kind:    libsveltosv1beta1.ResourceSummaryKind,
	}

	apiVersion, kind := gvk.ToAPIVersionAndKind()

	return &corev1.ObjectReference{
		Namespace:  resourceSummary.Namespace,
		Name:       resourceSummary.Name,
		APIVersion: apiVersion,
		Kind:       kind,
	}
}
