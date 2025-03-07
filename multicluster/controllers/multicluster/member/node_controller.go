/*
Copyright 2022 Antrea Authors.

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

package member

import (
	"context"
	"fmt"
	"net"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	mcsv1alpha1 "antrea.io/antrea/multicluster/apis/multicluster/v1alpha1"
	"antrea.io/antrea/multicluster/controllers/multicluster/common"
)

var (
	ServiceCIDRDiscoverFn = common.DiscoverServiceCIDRByInvalidServiceCreation
)

type (
	// NodeReconciler is for member cluster only.
	NodeReconciler struct {
		client.Client
		Scheme            *runtime.Scheme
		namespace         string
		precedence        mcsv1alpha1.Precedence
		gatewayCandidates map[string]bool
		activeGateway     string
		serviceCIDR       string
		initialized       bool
	}
)

// NewNodeReconciler creates a NodeReconciler to watch Node resource changes.
// It's responsible for creating a Gateway for the first ready Node with
// annotation `multicluster.antrea.io/gateway:true` if there is no existing Gateway.
// It guarantees there is always only one Gateway CR when there are multiple Nodes
// with annotation `multicluster.antrea.io/gateway:true`.
func NewNodeReconciler(
	client client.Client,
	scheme *runtime.Scheme,
	namespace string,
	serviceCIDR string,
	precedence mcsv1alpha1.Precedence) *NodeReconciler {
	if string(precedence) == "" {
		precedence = mcsv1alpha1.PrecedenceInternal
	}
	reconciler := &NodeReconciler{
		Client:            client,
		Scheme:            scheme,
		namespace:         namespace,
		serviceCIDR:       serviceCIDR,
		precedence:        precedence,
		gatewayCandidates: make(map[string]bool),
	}
	return reconciler
}

//+kubebuilder:rbac:groups=multicluster.crd.antrea.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=nodes,verbs=get;list;watch;
//+kubebuilder:rbac:groups=multicluster.crd.antrea.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=multicluster.crd.antrea.io,resources=gateways/finalizers,verbs=update

func (r *NodeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	klog.V(2).InfoS("Reconciling Node", "node", req.Name)
	if !r.initialized {
		if err := r.initialize(); err != nil {
			return ctrl.Result{}, err
		}
		r.initialized = true
	}
	gw := &mcsv1alpha1.Gateway{
		ObjectMeta: metav1.ObjectMeta{
			Name:      req.Name,
			Namespace: r.namespace,
		},
	}

	noActiveGateway := r.activeGateway == ""
	isActiveGateway := r.activeGateway == req.Name
	stillGatewayNode := false

	node := &corev1.Node{}
	if err := r.Client.Get(ctx, req.NamespacedName, node); err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to get Node", "node", req.Name)
			return ctrl.Result{}, err
		}
	} else {
		_, hasGWAnnotation := node.Annotations[common.GatewayAnnotation]
		stillGatewayNode = hasGWAnnotation
	}

	if stillGatewayNode {
		r.gatewayCandidates[req.Name] = true
	} else {
		delete(r.gatewayCandidates, req.Name)
	}

	var err error
	var isValidGateway bool

	if stillGatewayNode {
		gw.ServiceCIDR = r.serviceCIDR
		gw.InternalIP, gw.GatewayIP, err = r.getGatawayNodeIP(node)
		if err != nil {
			klog.ErrorS(err, "There is no valid Gateway IP for Node", "node", node.Name)
		}
		isValidGateway = err == nil
	}

	if isActiveGateway {
		if !isValidGateway || !isReadyNode(node) {
			if err := r.recreateActiveGateway(ctx, gw); err != nil {
				return ctrl.Result{}, err
			}
		} else {
			if err := r.updateActiveGateway(ctx, gw); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	if noActiveGateway && isValidGateway && isReadyNode(node) {
		if err := r.createGateway(gw); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

// initialize initializes 'activeGateway' and 'gatewayCandidates' and removes
// stale Gateway during controller startup.
func (r *NodeReconciler) initialize() error {
	ctx := context.Background()
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList, &client.ListOptions{}); err != nil {
		return err
	}

	gwList := &mcsv1alpha1.GatewayList{}
	if err := r.Client.List(ctx, gwList, &client.ListOptions{}); err != nil {
		return err
	}
	// Gateway webhook guarantees that there is at most one Gateway in the member cluster.
	if len(gwList.Items) > 0 {
		existingGWName := gwList.Items[0].Name
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: existingGWName}, node); err != nil {
			if !apierrors.IsNotFound(err) {
				return err
			}
			staleGateway := &mcsv1alpha1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: r.namespace,
					Name:      existingGWName},
			}
			err := r.Client.Delete(ctx, staleGateway, &client.DeleteOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
		} else {
			r.activeGateway = existingGWName
		}
	}
	for _, n := range nodeList.Items {
		if _, isGW := n.Annotations[common.GatewayAnnotation]; isGW {
			r.gatewayCandidates[n.Name] = true
		}
	}
	return nil
}

func (r *NodeReconciler) updateActiveGateway(ctx context.Context, newGateway *mcsv1alpha1.Gateway) error {
	existingGW := &mcsv1alpha1.Gateway{}
	// TODO: cache might be stale. Need to revisit here and other reconcilers to
	// check if we can improve this with 'Owns' or other methods.
	if err := r.Client.Get(ctx, types.NamespacedName{Name: newGateway.Name, Namespace: r.namespace}, existingGW); err != nil {
		return err
	}
	if existingGW.GatewayIP == newGateway.GatewayIP && existingGW.InternalIP == newGateway.InternalIP &&
		existingGW.ServiceCIDR == newGateway.ServiceCIDR {
		return nil
	}
	existingGW.GatewayIP = newGateway.GatewayIP
	existingGW.InternalIP = newGateway.InternalIP
	existingGW.ServiceCIDR = newGateway.ServiceCIDR
	// If the Gateway version in the client cache is stale, the update operation will fail,
	// then the reconciler will retry with latest state again.
	if err := r.Client.Update(ctx, existingGW, &client.UpdateOptions{}); err != nil {
		return err
	}
	return nil
}

// recreateActiveGateway will delete the existing Gateway CR and create a new Gateway
// from the pool of Gateway candidates.
func (r *NodeReconciler) recreateActiveGateway(ctx context.Context, gateway *mcsv1alpha1.Gateway) error {
	err := r.Client.Delete(ctx, gateway, &client.DeleteOptions{})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	r.activeGateway = ""
	// Check remaining Gateway candidates and create a new Gateway.
	newGateway, err := r.getValidGatewayFromCandidates()
	if err != nil {
		return err
	}
	if newGateway != nil {
		return r.createGateway(newGateway)
	}
	return nil
}

// getValidGatewayFromCandidates picks a valid Node from Gateway candidates and
// creates a Gateway. It returns no error if no good Gateway candidate.
func (r *NodeReconciler) getValidGatewayFromCandidates() (*mcsv1alpha1.Gateway, error) {
	var activeGateway *mcsv1alpha1.Gateway
	var internalIP, gwIP string
	var err error

	gatewayNode := &corev1.Node{}
	for name := range r.gatewayCandidates {
		if err = r.Client.Get(context.Background(), types.NamespacedName{Name: name}, gatewayNode); err == nil {
			if !isReadyNode(gatewayNode) {
				continue
			}
			if internalIP, gwIP, err = r.getGatawayNodeIP(gatewayNode); err != nil {
				klog.V(2).ErrorS(err, "Node has no valid IP", "node", gatewayNode.Name)
				continue
			}

			activeGateway = &mcsv1alpha1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      gatewayNode.Name,
					Namespace: r.namespace,
				},
				GatewayIP:   gwIP,
				InternalIP:  internalIP,
				ServiceCIDR: r.serviceCIDR,
			}
			klog.InfoS("Found good Gateway candidate", "node", gatewayNode.Name)
			return activeGateway, nil
		}
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, nil
}

func (r *NodeReconciler) createGateway(gateway *mcsv1alpha1.Gateway) error {
	if err := r.Client.Create(context.Background(), gateway, &client.CreateOptions{}); err != nil {
		return err
	}
	r.activeGateway = gateway.Name
	return nil
}

func (r *NodeReconciler) getGatawayNodeIP(node *corev1.Node) (string, string, error) {
	var gatewayIP, internalIP string
	for _, addr := range node.Status.Addresses {
		if addr.Type == corev1.NodeInternalIP {
			if r.precedence == mcsv1alpha1.PrecedencePrivate || r.precedence == mcsv1alpha1.PrecedenceInternal {
				gatewayIP = addr.Address
			}
			internalIP = addr.Address
		}
		if (r.precedence == mcsv1alpha1.PrecedencePublic || r.precedence == mcsv1alpha1.PrecedenceExternal) &&
			addr.Type == corev1.NodeExternalIP {
			gatewayIP = addr.Address
		}
	}

	if ip, ok := node.Annotations[common.GatewayIPAnnotation]; ok {
		parsedIP := net.ParseIP(ip)
		if parsedIP == nil {
			return "", "", fmt.Errorf("the Gateway IP annotation %s on Node %s is not a valid IP address", ip, node.Name)
		}
		gatewayIP = ip
	}

	if gatewayIP == "" || internalIP == "" {
		return "", "", fmt.Errorf("no valid IP address for Gateway Node %s", node.Name)
	}
	return internalIP, gatewayIP, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.serviceCIDR == "" {
		var err error
		r.serviceCIDR, err = ServiceCIDRDiscoverFn(context.TODO(), r.Client, r.namespace)
		if err != nil {
			return err
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Node{}).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: 1,
		}).
		Complete(r)
}

func isReadyNode(node *corev1.Node) bool {
	var nodeIsReady bool
	for _, s := range node.Status.Conditions {
		if s.Type == corev1.NodeReady && s.Status == corev1.ConditionTrue {
			nodeIsReady = true
			break
		}
	}
	return nodeIsReady
}
