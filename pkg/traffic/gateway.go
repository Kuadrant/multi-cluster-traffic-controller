package traffic

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/strings/slices"
	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/slice"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1alpha1"
	status "github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer/status"
)

func NewGateway(g *gatewayv1beta1.Gateway) Interface {
	return &Gateway{Gateway: g}
}

type Gateway struct {
	*gatewayv1beta1.Gateway
}

func (a *Gateway) GetKind() string {
	return "Gateway"
}

func (a *Gateway) GetHosts() []string {
	var hosts []string
	for _, listener := range a.Spec.Listeners {
		host := (*string)(listener.Hostname)
		if host == nil {
			continue
		}
		if !slices.Contains(hosts, *host) {
			hosts = append(hosts, *host)
		}
	}

	return hosts
}

func (a *Gateway) AddManagedHost(h string) error {
	// Not implemented for Gateways
	return nil
}

func (a *Gateway) HasTLS() bool {
	hasTLS := false
	for _, listener := range a.Spec.Listeners {
		if listener.TLS != nil {
			hasTLS = true
			break
		}
	}
	return hasTLS
}

func (a *Gateway) AddTLS(host string, secret *corev1.Secret) {
	listeners := []gatewayv1beta1.Listener{}

	gatewayNS := gatewayv1beta1.Namespace(a.Namespace)
	secretKind := gatewayv1beta1.Kind(secret.Kind)
	secretGroup := gatewayv1beta1.Group("")
	modeTerminate := gatewayv1beta1.TLSModeTerminate
	for _, listener := range a.Spec.Listeners {
		if *(*string)(listener.Hostname) == host {
			listener.TLS = &gatewayv1beta1.GatewayTLSConfig{
				Mode: &modeTerminate, // Ensure terminate mode as we're managing the cert
				CertificateRefs: []gatewayv1beta1.SecretObjectReference{
					{
						Group:     &secretGroup,
						Kind:      &secretKind,
						Name:      gatewayv1beta1.ObjectName(secret.Name),
						Namespace: &gatewayNS,
					},
				},
			}
		}
		listeners = append(listeners, listener)
	}

	a.Spec.Listeners = listeners
}

func (a *Gateway) RemoveTLS(hosts []string) {
	for _, listener := range a.Spec.Listeners {
		if slice.ContainsString(hosts, fmt.Sprint(listener.Hostname)) {
			listener.TLS = nil
		}
	}
}

func (a *Gateway) GetSpec() interface{} {
	return a.Spec
}

func (a *Gateway) GetNamespaceName() types.NamespacedName {
	return types.NamespacedName{
		Namespace: a.Namespace,
		Name:      a.Name,
	}
}

func (a *Gateway) GetCacheKey() string {
	key, _ := cache.MetaNamespaceKeyFunc(a)
	return key
}

func (a *Gateway) String() string {
	return fmt.Sprintf("kind: %v, namespace/name: %v", a.GetKind(), a.GetNamespaceName())
}

// GetDNSTargets will return the LB hosts and or IPs from the the Ingress object associated with the cluster they came from
func (a *Gateway) GetDNSTargets(ctx context.Context) ([]v1alpha1.Target, error) {
	dnsTargets := []v1alpha1.Target{}

	for _, gatewayStatus := range a.GetGatewayStatuses(ctx) {
		if len(gatewayStatus.Addresses) == 0 {
			continue
		}
		// TODO: Allow for more than 1 address
		ipAddress := gatewayStatus.Addresses[0].Value
		dnsTarget := v1alpha1.Target{
			TargetType: v1alpha1.TargetTypeIP,
			Value:      ipAddress,
		}
		dnsTargets = append(dnsTargets, dnsTarget)
	}

	return dnsTargets, nil
}

func (a *Gateway) GetGatewayStatuses(ctx context.Context) []gatewayv1beta1.GatewayStatus {
	log := log.FromContext(ctx)
	// Aggregated Gateway status from syncer
	statuses := []gatewayv1beta1.GatewayStatus{}
	for annotationName, annotationValue := range a.Annotations {
		if strings.HasPrefix(annotationName, status.SyncerClusterStatusAnnotationPrefix) {
			status := gatewayv1beta1.GatewayStatus{}
			err := json.Unmarshal([]byte(annotationValue), &status)
			if err != nil {
				log.Error(err, "Error unmarshalling gateway status from syncer annotation")
			} else {
				statuses = append(statuses, status)
			}
		}
	}
	return statuses
}

func (a *Gateway) GetListenerByHost(host string) *gatewayv1beta1.Listener {
	for _, listener := range a.Spec.Listeners {
		if *(*string)(listener.Hostname) == host {
			return &listener
		}
	}
	return nil
}

// Gather all listener statuses in all gateway statuses that match the given listener name
func (a *Gateway) GetListenerStatusesByListenerName(ctx context.Context, listenerName string) []gatewayv1beta1.ListenerStatus {
	listenerStatuses := []gatewayv1beta1.ListenerStatus{}
	for _, gatewayStatus := range a.GetGatewayStatuses(ctx) {
		for _, listenerStatus := range gatewayStatus.Listeners {
			if string(listenerStatus.Name) == listenerName {
				listenerStatuses = append(listenerStatuses, listenerStatus)
			}
		}
	}
	return listenerStatuses
}

func (a *Gateway) ExposesOwnController() bool {
	return false
}
