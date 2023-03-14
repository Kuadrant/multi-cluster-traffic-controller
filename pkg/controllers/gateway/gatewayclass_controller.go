/*
Copyright 2022 The MultiCluster Traffic Controller Authors.

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

package gateway

import (
	"context"
	"fmt"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/conditions"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
)

const (
	ControllerName = "kuadrant.io/mctc-gw-controller"
)

func getSupportedClasses() []string {
	return []string{"mctc-gw-istio-external-instance-per-cluster"}
}

// GatewayClassReconciler reconciles a GatewayClass object
type GatewayClassReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gatewayclasses/finalizers,verbs=update

func (r *GatewayClassReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrllog.FromContext(ctx)

	previous := &gatewayv1beta1.GatewayClass{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, previous)
	if err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			log.Error(err, "Unable to fetch GatewayClass")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if gatewayClassIsAccepted(previous) {
		log.Info("GatewayClass already Accepted")
		return ctrl.Result{}, nil
	}

	gatewayclass := previous.DeepCopy()
	supportedClasses := getSupportedClasses()
	if !slice.ContainsString(supportedClasses, previous.Name) {
		gatewayclass.Status = gatewayv1beta1.GatewayClassStatus{
			Conditions: []metav1.Condition{
				{
					LastTransitionTime: metav1.Now(),
					Message:            fmt.Sprintf("Invalid Parameters - Unsupported class name %s. Must be one of [%v]", previous.Name, strings.Join(supportedClasses, ",")),
					Reason:             string(gatewayv1beta1.GatewayClassReasonInvalidParameters),
					Status:             metav1.ConditionFalse,
					Type:               string(gatewayv1beta1.GatewayClassConditionStatusAccepted),
					ObservedGeneration: previous.Generation,
				},
			},
		}
	} else {
		gatewayclass.Status = gatewayv1beta1.GatewayClassStatus{
			Conditions: []metav1.Condition{
				{
					LastTransitionTime: metav1.Now(),
					Message:            fmt.Sprintf("Handled by %s", ControllerName),
					Reason:             string(gatewayv1beta1.GatewayClassConditionStatusAccepted),
					Status:             metav1.ConditionTrue,
					Type:               string(gatewayv1beta1.GatewayClassConditionStatusAccepted),
					ObservedGeneration: previous.Generation,
				},
			},
		}
	}

	log.Info("Updating GatewayClass", "status", gatewayclass.Status)
	err = r.Status().Update(ctx, gatewayclass)
	if err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func gatewayClassIsAccepted(gatewayClass *gatewayv1beta1.GatewayClass) bool {
	acceptedCondition := conditions.GetConditionByType(gatewayClass.Status.Conditions, string(gatewayv1beta1.GatewayConditionAccepted))
	return (acceptedCondition != nil && acceptedCondition.Status == metav1.ConditionTrue)
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayClassReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		// Uncomment the following line adding a pointer to an instance of the controlled resource as an argument
		For(&gatewayv1beta1.GatewayClass{}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			gatewayClass := object.(*gatewayv1beta1.GatewayClass)
			return gatewayClass.Spec.ControllerName == ControllerName
		})).
		Complete(r)
}
