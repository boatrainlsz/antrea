// Copyright 2020 Antrea Authors
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

package networkpolicystats

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metatable "k8s.io/apimachinery/pkg/api/meta/table"
	"k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"

	statsv1alpha1 "antrea.io/antrea/pkg/apis/stats/v1alpha1"
	"antrea.io/antrea/pkg/features"
)

var (
	tableColumnDefinitions = []metav1.TableColumnDefinition{
		{Name: "Name", Type: "string", Format: "name", Description: swaggerMetadataDescriptions["name"]},
		{Name: "Sessions", Type: "integer", Description: "The sessions count hit by the K8s NetworkPolicy."},
		{Name: "Packets", Type: "integer", Description: "The packets count hit by the K8s NetworkPolicy."},
		{Name: "Bytes", Type: "integer", Description: "The bytes count hit by the K8s NetworkPolicy."},
		{Name: "Created At", Type: "date", Description: swaggerMetadataDescriptions["creationTimestamp"]},
	}
)

type REST struct {
	statsProvider statsProvider
}

// NewREST returns a REST object that will work against API services.
func NewREST(p statsProvider) *REST {
	return &REST{p}
}

var (
	_ rest.Storage = &REST{}
	_ rest.Scoper  = &REST{}
	_ rest.Getter  = &REST{}
	_ rest.Lister  = &REST{}
)

type statsProvider interface {
	ListNetworkPolicyStats(namespace string) []statsv1alpha1.NetworkPolicyStats

	GetNetworkPolicyStats(namespace, name string) (*statsv1alpha1.NetworkPolicyStats, bool)
}

func (r *REST) New() runtime.Object {
	return &statsv1alpha1.NetworkPolicyStats{}
}

func (r *REST) Destroy() {
}

func (r *REST) NewList() runtime.Object {
	return &statsv1alpha1.NetworkPolicyStatsList{}
}

func (r *REST) List(ctx context.Context, options *internalversion.ListOptions) (runtime.Object, error) {
	if !features.DefaultFeatureGate.Enabled(features.NetworkPolicyStats) {
		return &statsv1alpha1.NetworkPolicyStatsList{}, nil
	}
	labelSelector := labels.Everything()
	if options != nil && options.LabelSelector != nil {
		labelSelector = options.LabelSelector
	}
	ns, _ := request.NamespaceFrom(ctx)
	stats := r.statsProvider.ListNetworkPolicyStats(ns)
	items := make([]statsv1alpha1.NetworkPolicyStats, 0, len(stats))
	for i := range stats {
		if labelSelector.Matches(labels.Set(stats[i].Labels)) {
			items = append(items, stats[i])
		}
	}
	metricList := &statsv1alpha1.NetworkPolicyStatsList{
		Items: items,
	}
	return metricList, nil
}

func (r *REST) Get(ctx context.Context, name string, options *metav1.GetOptions) (runtime.Object, error) {
	if !features.DefaultFeatureGate.Enabled(features.NetworkPolicyStats) {
		return nil, errors.NewBadRequest("feature NetworkPolicyStats disabled")
	}
	ns, ok := request.NamespaceFrom(ctx)
	if !ok || len(ns) == 0 {
		return nil, errors.NewBadRequest("Namespace parameter required.")
	}
	metric, exists := r.statsProvider.GetNetworkPolicyStats(ns, name)
	if !exists {
		return nil, errors.NewNotFound(statsv1alpha1.Resource("networkpolicystats"), name)
	}
	return metric, nil
}

var swaggerMetadataDescriptions = metav1.ObjectMeta{}.SwaggerDoc()

func (r *REST) ConvertToTable(ctx context.Context, obj runtime.Object, tableOptions runtime.Object) (*metav1.Table, error) {
	table := &metav1.Table{
		ColumnDefinitions: tableColumnDefinitions,
	}
	if m, err := meta.ListAccessor(obj); err == nil {
		table.ResourceVersion = m.GetResourceVersion()
		table.Continue = m.GetContinue()
		table.RemainingItemCount = m.GetRemainingItemCount()
	} else {
		if m, err := meta.CommonAccessor(obj); err == nil {
			table.ResourceVersion = m.GetResourceVersion()
		}
	}

	var err error
	table.Rows, err = metatable.MetaToTableRow(obj, func(obj runtime.Object, m metav1.Object, name, age string) ([]interface{}, error) {
		stats := obj.(*statsv1alpha1.NetworkPolicyStats)
		return []interface{}{name, stats.TrafficStats.Sessions, stats.TrafficStats.Packets, stats.TrafficStats.Bytes, m.GetCreationTimestamp().Time.UTC().Format(time.RFC3339)}, nil
	})
	return table, err
}

func (r *REST) NamespaceScoped() bool {
	return true
}
