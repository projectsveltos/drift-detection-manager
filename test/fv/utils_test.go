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

package fv_test

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1beta1 "github.com/projectsveltos/libsveltos/api/v1beta1"
)

func getResourceSummary(resource, helmResource *corev1.ObjectReference, ignore bool) *libsveltosv1beta1.ResourceSummary {
	rs := &libsveltosv1beta1.ResourceSummary{
		ObjectMeta: metav1.ObjectMeta{
			Name:      randomString(),
			Namespace: "projectsveltos",
		},
	}

	if resource != nil {
		rs.Spec.Resources = []libsveltosv1beta1.Resource{
			{
				Name:                        resource.Name,
				Namespace:                   resource.Namespace,
				Kind:                        resource.Kind,
				Group:                       resource.GroupVersionKind().Group,
				Version:                     resource.GroupVersionKind().Version,
				IgnoreForConfigurationDrift: ignore,
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
						Name:                        helmResource.Name,
						Namespace:                   helmResource.Namespace,
						Kind:                        helmResource.Kind,
						Group:                       helmResource.GroupVersionKind().Group,
						Version:                     helmResource.GroupVersionKind().Version,
						IgnoreForConfigurationDrift: ignore,
					},
				},
			},
		}
	}

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
