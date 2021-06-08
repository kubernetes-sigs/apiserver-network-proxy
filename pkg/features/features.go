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

package features

import (
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/component-base/featuregate"
)

const (
	// NodeToMasterTraffic enables the traffic initiated in the agents side
	// to flow to the server side e.g. Kubelet to KAS and pods to KAS traffic
	// (KEP-2025).
	// TODO (#issues/232) Determine how to safely fix the feature gate name.
	NodeToMasterTraffic featuregate.Feature = "NodeToMasterTraffic"
)

var (
	// DefaultMutableFeatureGate is a mutable version of DefaultFeatureGate.
	DefaultMutableFeatureGate featuregate.MutableFeatureGate = featuregate.NewFeatureGate()
)

func init() {
	runtime.Must(DefaultMutableFeatureGate.Add(defaultFeatureGates))
}

var defaultFeatureGates = map[featuregate.Feature]featuregate.FeatureSpec{
	NodeToMasterTraffic: {Default: false, PreRelease: featuregate.Alpha},
}
