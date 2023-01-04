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

package scope

import (
	"context"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"
	"sigs.k8s.io/cluster-api/util/patch"
	"sigs.k8s.io/controller-runtime/pkg/client"

	libsveltosv1alpha1 "github.com/projectsveltos/libsveltos/api/v1alpha1"
)

// ResourceSummaryScopeParams defines the input parameters used to create a new ResourceSummary Scope.
type ResourceSummaryScopeParams struct {
	Client          client.Client
	Logger          logr.Logger
	ResourceSummary *libsveltosv1alpha1.ResourceSummary
	ControllerName  string
}

// NewResourceSummaryScope creates a new ResourceSummary Scope from the supplied parameters.
// This is meant to be called for each reconcile iteration.
func NewResourceSummaryScope(params ResourceSummaryScopeParams) (*ResourceSummaryScope, error) {
	if params.Client == nil {
		return nil, errors.New("client is required when creating a ResourceSummaryScope")
	}
	if params.ResourceSummary == nil {
		return nil, errors.New("failed to generate new scope from nil ResourceSummary")
	}

	helper, err := patch.NewHelper(params.ResourceSummary, params.Client)
	if err != nil {
		return nil, errors.Wrap(err, "failed to init patch helper")
	}
	return &ResourceSummaryScope{
		Logger:          params.Logger,
		client:          params.Client,
		ResourceSummary: params.ResourceSummary,
		patchHelper:     helper,
		controllerName:  params.ControllerName,
	}, nil
}

// ResourceSummaryScope defines the basic context for an actuator to operate upon.
type ResourceSummaryScope struct {
	logr.Logger
	client          client.Client
	patchHelper     *patch.Helper
	ResourceSummary *libsveltosv1alpha1.ResourceSummary
	controllerName  string
}

// PatchObject persists the feature configuration and status.
func (s *ResourceSummaryScope) PatchObject(ctx context.Context) error {
	return s.patchHelper.Patch(
		ctx,
		s.ResourceSummary,
	)
}

// Close closes the current scope persisting the ResourceSummary configuration and status.
func (s *ResourceSummaryScope) Close(ctx context.Context) error {
	return s.PatchObject(ctx)
}

// Name returns the ResourceSummary name.
func (s *ResourceSummaryScope) Name() string {
	return s.ResourceSummary.Name
}
