// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/component-base/featuregate"
	"k8s.io/kubernetes/pkg/features"
)

type featureGates map[featuregate.Feature]bool

// AsFlag returns the feature gates as a --feature-gate flag.
func (fgs featureGates) AsFlag() string {
	var strb strings.Builder
	strb.WriteString("--feature-gates=")
	features := make([]string, 0, len(fgs))
	for f := range fgs {
		features = append(features, string(f))
	}
	// Ensure deterministic output by sorting the map keys
	sort.Strings(features)
	for i, f := range features {
		fmt.Fprintf(&strb, "%s=%v", f, fgs[featuregate.Feature(f)])
		if i+1 != len(features) {
			strb.WriteByte(',')
		}
	}
	return strb.String()
}

// AsConfigObject returns the feature gates as a plain map for K8s configs.
func (fgs featureGates) AsMap() map[string]bool {
	out := make(map[string]bool)
	for f, en := range fgs {
		out[string(f)] = en
	}
	return out
}

var extraFeatureGates = featureGates{
	features.UserNamespacesPodSecurityStandards: true,
}
