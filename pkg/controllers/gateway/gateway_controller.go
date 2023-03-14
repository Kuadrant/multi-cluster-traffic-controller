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
	"reflect"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/conditions"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/traffic"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/source"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
)

const (
	ClusterSyncerAnnotation               = "clustersync.kuadrant.io"
	GatewayClusterLabelSelectorAnnotation = "kuadrant.io/gateway-cluster-label-selector"
)

type HostService interface {
	CreateDNSRecord(ctx context.Context, subDomain string, managedZone *v1alpha1.ManagedZone) (*v1alpha1.DNSRecord, error)
	GetDNSRecord(ctx context.Context, subDomain string, managedZone *v1alpha1.ManagedZone) (*v1alpha1.DNSRecord, error)
	GetManagedZoneForHost(ctx context.Context, domain string, t traffic.Interface) (*v1alpha1.ManagedZone, string, error)
	AddEndpoints(ctx context.Context, t traffic.Interface, dnsRecord *v1alpha1.DNSRecord) error
}

type CertificateService interface {
	EnsureCertificate(ctx context.Context, host string, owner metav1.Object) error
	GetCertificateSecret(ctx context.Context, host string, namespace string) (*corev1.Secret, error)
}

type GatewayHelper interface {
	GetGatewayStatuses(ctx context.Context) []gatewayv1beta1.GatewayStatus
	GetListenerByHost(host string) *gatewayv1beta1.Listener
	GetListenerStatusesByListenerName(ctx context.Context, listenerName string) []gatewayv1beta1.ListenerStatus
}

// GatewayReconciler reconciles a Gateway object
type GatewayReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Certificates CertificateService
	Host         HostService
}

type ManagedHost struct {
	host        string
	managedZone *v1alpha1.ManagedZone
	dnsRecord   *v1alpha1.DNSRecord
}

//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways/finalizers,verbs=update

func (r *GatewayReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	previous := &gatewayv1beta1.Gateway{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: req.Namespace, Name: req.Name}, previous)
	if err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			log.Error(err, "Unable to fetch Gateway")
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if previous.GetDeletionTimestamp() != nil && !previous.GetDeletionTimestamp().IsZero() {
		// TODO: Do we need to remove dns records and/or endpoints?
		//       Will ownerRefs be sufficient
		log.Info("Gateway is deleting", "gateway", previous.Name, "namespace", previous.Namespace)
		return ctrl.Result{}, nil
	}

	// Check if the class name is one of ours
	// TODO: If the gateway class is a supported class, but the GatewayClass resource doesn't exist,
	//       just create it anyways as we know we can support it.
	//       Con: Use case for an admin to only allow certain supported GatewayClasses to be used?
	gatewayClass := &gatewayv1beta1.GatewayClass{}
	err = r.Client.Get(ctx, client.ObjectKey{Name: string(previous.Spec.GatewayClassName)}, gatewayClass)
	if err != nil {
		if err := client.IgnoreNotFound(err); err != nil {
			log.Error(err, "Unable to fetch GatewayClass")
			return ctrl.Result{}, err
		}
		// Ignore as class can't be retrieved
		log.Info("GatewayClass not found", "gatewayclass", previous.Spec.GatewayClassName)
		return ctrl.Result{}, nil
	}

	gateway := previous.DeepCopy()
	acceptedStatus := metav1.ConditionTrue
	programmedStatus, clusters, requeue, reconcileErr := r.reconcileGateway(ctx, *previous, gateway)

	// Update gateway spec/metadata
	if !reflect.DeepEqual(gateway, previous) {
		log.Info("Updating Gateway Spec", "gatewaySpec", gateway.Spec, "previousSpec", previous.Spec)
		log.Info("Updating Gateway ObjectMeta", "gatewayObjectMeta", gateway.ObjectMeta, "previousObjectMeta", previous.ObjectMeta)
		err = r.Update(ctx, gateway)
		if err != nil {
			log.Error(err, "Error updating Gateway")
		}
	}

	// Update status
	gateway.Status.Conditions = buildStatusConditions(gateway.Status, previous.Generation, clusters, acceptedStatus, programmedStatus)
	if !reflect.DeepEqual(gateway.Status, previous.Status) {
		log.Info("Updating Gateway status", "gatewaystatus", gateway.Status, "previousstatus", previous.Status)
		err = r.Status().Update(ctx, gateway)
		if err != nil {
			log.Error(err, "Error updating Gateway status")
		}
	}

	if requeue {
		return ctrl.Result{Requeue: true, RequeueAfter: time.Second * 10}, reconcileErr
	}
	return ctrl.Result{}, reconcileErr
}

// Configures Gateway tls & dns for each cluster it targets.
// Returns the programmed status, a list of clusters that were programmed, if the gateway should be requeued, and any error
func (r *GatewayReconciler) reconcileGateway(ctx context.Context, previous gatewayv1beta1.Gateway, gateway *gatewayv1beta1.Gateway) (metav1.ConditionStatus, []string, bool, error) {
	log := log.FromContext(ctx)

	clusters := selectClusters(*gateway)
	// Don't do anything else until at least 1 cluster matches.
	if len(clusters) == 0 {
		// TODO: Handle any cleanup if there were previously clusters
		return metav1.ConditionFalse, clusters, false, nil
	}
	trafficAccessor := traffic.NewGateway(gateway)
	gatewayHelper := trafficAccessor.(GatewayHelper)
	allHosts := trafficAccessor.GetHosts()
	managedHosts := []ManagedHost{}

	// Create a list of managed hosts.
	// Find a suitable managed zone for each host, if one exists, ensure a DNSRecord exists for it.
	for _, host := range allHosts {
		log.V(2).Info("getting managed zone", "host", host)
		managedZone, subDomain, err := r.Host.GetManagedZoneForHost(ctx, host, trafficAccessor)
		if err != nil {
			return metav1.ConditionUnknown, clusters, false, err
		}
		if managedZone == nil {
			log.Info("no managed zone available to use")
			continue
		}
		// TODO: ownerRefs e.g.
		// err = controllerutil.SetControllerReference(parentZone, nsRecord, r.Scheme)

		// Get an existing DNSRecord or create a new one, but no endpoint yet.
		// Endpoints created later when routes are attached.
		dnsRecord, err := r.Host.GetDNSRecord(ctx, subDomain, managedZone)
		if err != nil {
			return metav1.ConditionUnknown, clusters, false, err
		}
		if dnsRecord == nil {
			dnsRecord, err = r.Host.CreateDNSRecord(ctx, subDomain, managedZone)
			if err != nil {
				log.Error(err, "failed to create DNSRecord", "subDomain", subDomain)
				return metav1.ConditionUnknown, clusters, false, err
			}
			log.Info("DNSRecord created", "dnsRecord", dnsRecord)
		}

		managedHost := ManagedHost{
			host:        host,
			managedZone: managedZone,
			dnsRecord:   dnsRecord,
		}

		managedHosts = append(managedHosts, managedHost)
	}

	// Generate certificates for managed hosts
	for _, mh := range managedHosts {

		// Only generate cert for https listeners
		listener := gatewayHelper.GetListenerByHost(mh.host)
		if listener.Protocol != gatewayv1beta1.HTTPSProtocolType {
			continue
		}

		// create certificate resource for assigned host
		if err := r.Certificates.EnsureCertificate(ctx, mh.host, gateway); err != nil && !k8serrors.IsAlreadyExists(err) {
			log.Error(err, "Error ensuring certificate")
			return metav1.ConditionUnknown, clusters, false, err
		}

		// Check if certificate secret is ready
		secret, err := r.Certificates.GetCertificateSecret(ctx, mh.host, trafficAccessor.GetNamespace())
		if err != nil && !k8serrors.IsNotFound(err) {
			log.Error(err, "Error getting certificate secret")
			return metav1.ConditionUnknown, clusters, false, err
		}
		if err != nil {
			log.Info("tls secret does not exist yet for host " + mh.host + " requeue")
			return metav1.ConditionUnknown, clusters, true, err
		}
		log.Info("certificate exists for host", "host", mh.host)

		//sync secret to clusters
		if secret != nil {
			updatedSecret := secret.DeepCopy()
			syncObjectToAllClusters(updatedSecret)
			if !reflect.DeepEqual(updatedSecret, secret) {
				log.Info("Updating Certificate secret annotations", "secret", secret.Name)
				err = r.Update(ctx, updatedSecret)
				if err != nil {
					return metav1.ConditionUnknown, clusters, false, err
				}
			}
			trafficAccessor.AddTLS(mh.host, secret)
		}
		// Secrets don't have a status, so we can't say for sure if it's synced OK. Optimism here.
		log.Info("certificate secret in place for host. Adding dns endpoints", "host", mh.host)
	}

	// Add endpoints for attached routes
	gatewayHasAttachedRoutes := false
	for _, mh := range managedHosts {
		// Only consider host for dns if there's at least 1 attached route to the listener for this host in *any* gateway
		listener := gatewayHelper.GetListenerByHost(mh.host)
		listenerStatuses := gatewayHelper.GetListenerStatusesByListenerName(ctx, string(listener.Name))
		hostHasAttachedRoutes := false
		for _, listenerStatus := range listenerStatuses {
			if listenerStatus.AttachedRoutes > 0 {
				hostHasAttachedRoutes = true
				break
			}
		}
		log.Info("hostHasAttachedRoutes", "host", mh.host, "hostHasAttachedRoutes", hostHasAttachedRoutes)

		if hostHasAttachedRoutes {
			gatewayHasAttachedRoutes = true
			err := r.Host.AddEndpoints(ctx, trafficAccessor, mh.dnsRecord)
			if err != nil {
				log.Error(err, "Error adding endpoints", "host", mh.host)
				return metav1.ConditionUnknown, clusters, false, err
			}
		}
	}
	syncObjectToAllClusters(gateway)

	if !gatewayHasAttachedRoutes {
		log.Info("no hosts have any attached routes in any gateway yet")
		return metav1.ConditionUnknown, clusters, true, nil
	}

	return metav1.ConditionTrue, clusters, false, nil
}

func buildStatusConditions(gatewayStatus gatewayv1beta1.GatewayStatus, generation int64, clusters []string, acceptedStatus metav1.ConditionStatus, programmedStatus metav1.ConditionStatus) []metav1.Condition {
	statusConditions := []metav1.Condition{}

	acceptedCondition := conditions.GetConditionByType(gatewayStatus.Conditions, string(gatewayv1beta1.GatewayConditionAccepted))
	if (acceptedCondition == nil) || (acceptedCondition.Status != acceptedStatus) || (acceptedCondition.ObservedGeneration != generation) {
		// State has changed
		statusConditions = conditions.SetCondition(statusConditions, generation, string(gatewayv1beta1.GatewayConditionAccepted), metav1.ConditionTrue, string(gatewayv1beta1.GatewayConditionAccepted), fmt.Sprintf("Handled by %s", ControllerName))
	} else {
		statusConditions = append(statusConditions, *acceptedCondition)
	}

	programmedCondition := conditions.GetConditionByType(gatewayStatus.Conditions, string(gatewayv1beta1.GatewayConditionProgrammed))
	if (programmedCondition == nil) || (programmedCondition.Status != programmedStatus) || (programmedCondition.ObservedGeneration != generation) {
		// State has changed
		if programmedStatus == metav1.ConditionFalse {
			statusConditions = conditions.SetCondition(statusConditions, generation, string(gatewayv1beta1.GatewayConditionProgrammed), metav1.ConditionFalse, string(gatewayv1beta1.GatewayReasonPending), "No clusters match selection")
		} else if programmedStatus == metav1.ConditionTrue {
			statusConditions = conditions.SetCondition(statusConditions, generation, string(gatewayv1beta1.GatewayConditionProgrammed), metav1.ConditionTrue, string(gatewayv1beta1.GatewayConditionProgrammed), fmt.Sprintf("Gateway configured in data plane cluster(s) - [%v]", strings.Join(clusters, ",")))
		} else {
			// assume condition unknown i.e. programming is pending
			statusConditions = conditions.SetCondition(statusConditions, generation, string(gatewayv1beta1.GatewayConditionProgrammed), metav1.ConditionUnknown, string(gatewayv1beta1.GatewayReasonPending), "Waiting for controller")
		}
	} else {
		statusConditions = append(statusConditions, *programmedCondition)
	}

	return statusConditions
}

// func syncObjectToClusters(obj metav1.Object, clusters []string) {
// 	annotations := obj.GetAnnotations()
// 	if len(annotations) == 0 {
// 		annotations = map[string]string{}
// 	}
// 	for _, cluster := range clusters {
// 		annotations[fmt.Sprintf("%s/%s", syncer.MCTC_SYNC_ANNOTATION_PREFIX, cluster)] = "true"
// 	}
// 	obj.SetAnnotations(annotations)
// }

// TODO: Remove. This is a hack to enable simple 'all' placement of a resource
//
//		 in lieu of cluster representation in control plane.
//	     Use the above commented function instead.
func syncObjectToAllClusters(obj metav1.Object) {
	annotations := obj.GetAnnotations()
	if len(annotations) == 0 {
		annotations = map[string]string{}
	}
	annotations[fmt.Sprintf("%s%s", syncer.MCTC_SYNC_ANNOTATION_PREFIX, syncer.MCTC_SYNC_ANNOTATION_WILDCARD)] = "true"
	obj.SetAnnotations(annotations)
}

func selectClusters(gateway gatewayv1beta1.Gateway) []string {
	if gateway.Annotations == nil {
		return []string{}
	}

	selector := gateway.Annotations[GatewayClusterLabelSelectorAnnotation]
	log.Log.Info("selectClusters", "selector", selector)

	// TODO: Lookup clusters and select based on gateway cluster label selector annotation
	// HARDCODED IMPLEMENTATION
	// Issue: https://github.com/Kuadrant/multi-cluster-traffic-controller/issues/52
	if selector == "type=test" {
		return []string{"test_cluster_one"}
	}
	return []string{}
}

// SetupWithManager sets up the controller with the Manager.
func (r *GatewayReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&gatewayv1beta1.Gateway{}).
		Watches(&source.Kind{
			Type: &corev1.Secret{},
		}, &ClusterEventHandler{client: r.Client}).
		WithEventFilter(predicate.NewPredicateFuncs(func(object client.Object) bool {
			gateway, ok := object.(*gatewayv1beta1.Gateway)
			if !ok {
				return true
			}

			return slice.ContainsString(getSupportedClasses(), string(gateway.Spec.GatewayClassName))
		})).
		Complete(r)
}
