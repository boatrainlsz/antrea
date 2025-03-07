// Copyright 2023 Antrea Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package networkpolicy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	admv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	crdv1alpha1 "antrea.io/antrea/pkg/apis/crd/v1alpha1"
)

func TestMutateAntreaClusterNetworkPolicy(t *testing.T) {
	tests := []struct {
		name        string
		policy      *crdv1alpha1.ClusterNetworkPolicy
		operation   admv1.Operation
		expectPatch []jsonPatch
	}{
		{
			name: "acnp-create-mutate",
			policy: &crdv1alpha1.ClusterNetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mutate-rule-name-tier",
				},
				Spec: crdv1alpha1.ClusterNetworkPolicySpec{
					AppliedTo: []crdv1alpha1.AppliedTo{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"foo": "bar"},
							},
						},
					},
					Ingress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo1": "bar1"},
									},
								},
							},
						},
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
					Egress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							To: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
				},
			},
			operation: admv1.Create,
			expectPatch: []jsonPatch{
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/0/name",
					Value: "ingress-allow-c4f16d3",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/1/name",
					Value: "ingress-allow-be87b58",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/egress/0/name",
					Value: "egress-allow-44b2575",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/tier",
					Value: "application",
				},
			},
		},
		{
			name: "acnp-update-mutate",
			policy: &crdv1alpha1.ClusterNetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mutate-tier-name",
				},
				Spec: crdv1alpha1.ClusterNetworkPolicySpec{
					AppliedTo: []crdv1alpha1.AppliedTo{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"foo": "bar"},
							},
						},
					},
					Ingress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo1": "bar1"},
									},
								},
							},
						},
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
					Egress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							To: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
				},
			},
			operation: admv1.Update,
			expectPatch: []jsonPatch{
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/0/name",
					Value: "ingress-allow-c4f16d3",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/1/name",
					Value: "ingress-allow-be87b58",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/egress/0/name",
					Value: "egress-allow-44b2575",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/tier",
					Value: "application",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, controller := newController(nil, nil)
			mutator := NewNetworkPolicyMutator(controller.NetworkPolicyController)
			_, _, patch := mutator.mutateAntreaPolicy(tt.operation, tt.policy.Spec.Ingress, tt.policy.Spec.Egress, tt.policy.Spec.Tier)
			marshalExpPatch, _ := json.Marshal(tt.expectPatch)
			assert.Equal(t, marshalExpPatch, patch)
		})
	}
}

func TestMutateAntreaNetworkPolicy(t *testing.T) {
	tests := []struct {
		name        string
		policy      *crdv1alpha1.NetworkPolicy
		operation   admv1.Operation
		expectPatch []jsonPatch
	}{
		{
			name: "anp-create-mutate",
			policy: &crdv1alpha1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mutate-rule-name-tier",
					Namespace: "x",
				},
				Spec: crdv1alpha1.NetworkPolicySpec{
					AppliedTo: []crdv1alpha1.AppliedTo{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"foo": "bar"},
							},
						},
					},
					Ingress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo1": "bar1"},
									},
								},
							},
						},
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
					Egress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							To: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
				},
			},
			operation: admv1.Create,
			expectPatch: []jsonPatch{
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/0/name",
					Value: "ingress-allow-c4f16d3",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/1/name",
					Value: "ingress-allow-be87b58",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/egress/0/name",
					Value: "egress-allow-44b2575",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/tier",
					Value: "application",
				},
			},
		},
		{
			name: "anp-update-mutate",
			policy: &crdv1alpha1.NetworkPolicy{
				ObjectMeta: metav1.ObjectMeta{
					Name: "mutate-rule-name-tier",
				},
				Spec: crdv1alpha1.NetworkPolicySpec{
					AppliedTo: []crdv1alpha1.AppliedTo{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{"foo": "bar"},
							},
						},
					},
					Ingress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo1": "bar1"},
									},
								},
							},
						},
						{
							Action: &allowAction,
							From: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
					Egress: []crdv1alpha1.Rule{
						{
							Action: &allowAction,
							To: []crdv1alpha1.NetworkPolicyPeer{
								{
									PodSelector: &metav1.LabelSelector{
										MatchLabels: map[string]string{"foo2": "bar2"},
									},
								},
							},
						},
					},
				},
			},
			operation: admv1.Update,
			expectPatch: []jsonPatch{
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/0/name",
					Value: "ingress-allow-c4f16d3",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/ingress/1/name",
					Value: "ingress-allow-be87b58",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/egress/0/name",
					Value: "egress-allow-44b2575",
				},
				{
					Op:    jsonPatchReplaceOp,
					Path:  "/spec/tier",
					Value: "application",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, controller := newController(nil, nil)
			mutator := NewNetworkPolicyMutator(controller.NetworkPolicyController)
			_, _, patch := mutator.mutateAntreaPolicy(tt.operation, tt.policy.Spec.Ingress, tt.policy.Spec.Egress, tt.policy.Spec.Tier)
			marshalExpPatch, _ := json.Marshal(tt.expectPatch)
			assert.Equal(t, marshalExpPatch, patch)
		})
	}
}
