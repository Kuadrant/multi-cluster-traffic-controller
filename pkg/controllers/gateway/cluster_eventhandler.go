package gateway

import (
	"context"
	"fmt"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/clusterSecret"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
	corev1 "k8s.io/api/core/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/workqueue"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

type ClusterEventHandler struct {
	client client.Client
}

var _ handler.EventHandler = &ClusterEventHandler{}

// Create implements handler.EventHandler
func (eh *ClusterEventHandler) Create(e event.CreateEvent, q workqueue.RateLimitingInterface) {
	eh.enqueueForObject(e.Object, q)
}

// Delete implements handler.EventHandler
func (eh *ClusterEventHandler) Delete(e event.DeleteEvent, q workqueue.RateLimitingInterface) {
	eh.enqueueForObject(e.Object, q)
}

// Generic implements handler.EventHandler
func (eh *ClusterEventHandler) Generic(e event.GenericEvent, q workqueue.RateLimitingInterface) {
	eh.enqueueForObject(e.Object, q)
}

// Update implements handler.EventHandler
func (eh *ClusterEventHandler) Update(e event.UpdateEvent, q workqueue.RateLimitingInterface) {
	eh.enqueueForObject(e.ObjectNew, q)
}

func (eh *ClusterEventHandler) enqueueForObject(obj v1.Object, q workqueue.RateLimitingInterface) {
	if !clusterSecret.IsClusterSecret(obj) {
		return
	}

	gateways, err := eh.getGatewaysFor(obj.(*corev1.Secret))
	if err != nil {
		log.Log.Error(err, "failed to get gateways when enqueueing from cluster secret")
		return
	}

	for _, gateway := range gateways {
		log.Log.Info(fmt.Sprintf("Enqueing reconciliation from secret update to gateway/%s", gateway.Name))
		q.Add(ctrl.Request{
			NamespacedName: client.ObjectKeyFromObject(&gateway),
		})
	}
}

func (eh *ClusterEventHandler) getGatewaysFor(secret *corev1.Secret) ([]gatewayv1beta1.Gateway, error) {
	clusterConfig, err := clusterSecret.ClusterConfigFromSecret(secret)
	if err != nil {
		return nil, err
	}

	gateways := &gatewayv1beta1.GatewayList{}
	if err := eh.client.List(context.TODO(), gateways); err != nil {
		return nil, err
	}

	return slice.Filter(gateways.Items, func(gateway gatewayv1beta1.Gateway) bool {
		return slice.ContainsString(selectClusters(gateway), clusterConfig.Name)
	}), nil
}
