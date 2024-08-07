/*
Copyright 2022. projectsveltos.io. All rights reserved.

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

package scope_test

import (
	"github.com/go-logr/logr"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2/textlogger"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/projectsveltos/drift-detection-manager/pkg/scope"
	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

const (
	resourceSummaryNamePrefix = "scope-"
	failedToDeploy            = "failed to deploy"
	apiserverNotReachable     = "apiserver not reachable"
)

var _ = Describe("ResourceSummaryScope", func() {
	var resourceSummary *libsveltosv1beta1.ResourceSummary
	var c client.Client
	var logger logr.Logger

	BeforeEach(func() {
		logger = textlogger.NewLogger(textlogger.NewConfig(textlogger.Verbosity(1)))

		resourceSummary = &libsveltosv1beta1.ResourceSummary{
			ObjectMeta: metav1.ObjectMeta{
				Name: resourceSummaryNamePrefix + randomString(),
			},
		}

		scheme := setupScheme()
		initObjects := []client.Object{resourceSummary}
		c = fake.NewClientBuilder().WithScheme(scheme).WithObjects(initObjects...).Build()
	})

	It("Return nil,error if ResourceSummary is not specified", func() {
		params := scope.ResourceSummaryScopeParams{
			Client: c,
			Logger: logger,
		}

		scope, err := scope.NewResourceSummaryScope(params)
		Expect(err).To(HaveOccurred())
		Expect(scope).To(BeNil())
	})

	It("Return nil,error if client is not specified", func() {
		params := scope.ResourceSummaryScopeParams{
			ResourceSummary: resourceSummary,
			Logger:          logger,
		}

		scope, err := scope.NewResourceSummaryScope(params)
		Expect(err).To(HaveOccurred())
		Expect(scope).To(BeNil())
	})

	It("Name returns ResourceSummary Name", func() {
		params := scope.ResourceSummaryScopeParams{
			Client:          c,
			ResourceSummary: resourceSummary,
			Logger:          logger,
		}

		scope, err := scope.NewResourceSummaryScope(params)
		Expect(err).ToNot(HaveOccurred())
		Expect(scope).ToNot(BeNil())

		Expect(scope.Name()).To(Equal(resourceSummary.Name))
	})
})
