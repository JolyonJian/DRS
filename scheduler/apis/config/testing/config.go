/*
Copyright 2021 The Kubernetes Authors.

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

package testing

import (
	"testing"

	"k8s.io/component-base/config/v1alpha1"
	"k8s.io/kube-scheduler/config/v1beta2"
	"k8s.io/kube-scheduler/config/v1beta3"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	"k8s.io/kubernetes/pkg/scheduler/apis/config/scheme"
)

// V1beta2ToInternalWithDefaults creates a v1beta2 default configuration.
func V1beta2ToInternalWithDefaults(t *testing.T, versionedCfg v1beta2.KubeSchedulerConfiguration) *config.KubeSchedulerConfiguration {
	versionedCfg.DebuggingConfiguration = *v1alpha1.NewRecommendedDebuggingConfiguration()

	scheme.Scheme.Default(&versionedCfg)
	cfg := config.KubeSchedulerConfiguration{}
	if err := scheme.Scheme.Convert(&versionedCfg, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	return &cfg
}

// V1beta3ToInternalWithDefaults creates a v1beta3 default configuration.
func V1beta3ToInternalWithDefaults(t *testing.T, versionedCfg v1beta3.KubeSchedulerConfiguration) *config.KubeSchedulerConfiguration {
	versionedCfg.DebuggingConfiguration = *v1alpha1.NewRecommendedDebuggingConfiguration()

	scheme.Scheme.Default(&versionedCfg)
	cfg := config.KubeSchedulerConfiguration{}
	if err := scheme.Scheme.Convert(&versionedCfg, &cfg, nil); err != nil {
		t.Fatal(err)
	}
	return &cfg
}
