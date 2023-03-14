/*
Copyright 2023 The MultiCluster Traffic Controller Authors.
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
	"path/filepath"
	"testing"
	"time"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/_internal/conditions"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/apis/v1alpha1"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/dns"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer/status"
	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/tls"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"

	certman "github.com/jetstack/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/log"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	gatewayv1beta1 "sigs.k8s.io/gateway-api/apis/v1beta1"
	//+kubebuilder:scaffold:imports
)

// These tests use Ginkgo (BDD-style Go testing framework). Refer to
// http://onsi.github.io/ginkgo/ to learn more about Ginkgo.

var (
	cfg       *rest.Config
	k8sClient client.Client
	testEnv   *envtest.Environment
	ctx       context.Context
	cancel    context.CancelFunc
)

const (
	TestTimeoutMedium       = time.Second * 10
	TestRetryIntervalMedium = time.Millisecond * 250
)

func TestAPIs(t *testing.T) {
	RegisterFailHandler(Fail)

	RunSpecs(t, "Controller Suite")
}

var _ = BeforeSuite(func() {
	logf.SetLogger(zap.New(zap.WriteTo(GinkgoWriter), zap.UseDevMode(true)))
	ctx, cancel = context.WithCancel(context.TODO())

	By("bootstrapping test environment")
	testEnv = &envtest.Environment{
		CRDDirectoryPaths: []string{
			filepath.Join("../../../", "config", "crd", "bases"),
			filepath.Join("../../../", "config", "gateway-api", "crd", "standard"),
			filepath.Join("../../../", "config", "cert-manager", "crd", "v1.7.1"),
		},
		ErrorIfCRDPathMissing: true,
	}

	var err error
	// cfg is defined in this file globally.
	cfg, err = testEnv.Start()
	Expect(err).NotTo(HaveOccurred())
	Expect(cfg).NotTo(BeNil())

	err = v1alpha1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = gatewayv1beta1.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	err = certman.AddToScheme(scheme.Scheme)
	Expect(err).NotTo(HaveOccurred())

	//+kubebuilder:scaffold:scheme

	k8sClient, err = client.New(cfg, client.Options{Scheme: scheme.Scheme})
	Expect(err).NotTo(HaveOccurred())
	Expect(k8sClient).NotTo(BeNil())

	k8sManager, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme.Scheme,
	})
	Expect(err).ToNot(HaveOccurred())

	err = (&GatewayClassReconciler{
		Client: k8sManager.GetClient(),
		Scheme: k8sManager.GetScheme(),
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	certificates := tls.NewService(k8sManager.GetClient(), "default", "glbc-ca")
	dns := dns.NewService(k8sManager.GetClient(), dns.NewSafeHostResolver(dns.NewDefaultHostResolver()), "default")
	err = (&GatewayReconciler{
		Client:       k8sManager.GetClient(),
		Scheme:       k8sManager.GetScheme(),
		Certificates: certificates,
		Host:         dns,
	}).SetupWithManager(k8sManager)
	Expect(err).ToNot(HaveOccurred())

	// TODO: can we avoid duplicate set code here that also in controller/main.go?
	err = k8sManager.GetFieldIndexer().IndexField(
		context.Background(),
		&v1alpha1.ManagedZone{},
		"spec.domainName",
		func(obj client.Object) []string {
			return []string{obj.(*v1alpha1.ManagedZone).Spec.DomainName}
		},
	)
	Expect(err).ToNot(HaveOccurred())

	go func() {
		defer GinkgoRecover()
		err = k8sManager.Start(ctx)
		Expect(err).ToNot(HaveOccurred(), "failed to run manager")
	}()
})

var _ = AfterSuite(func() {
	cancel()
	By("tearing down the test environment")
	err := testEnv.Stop()
	Expect(err).NotTo(HaveOccurred())
})

var _ = Describe("GatewayClassController", func() {
	Context("testing gatewayclass controller", func() {
		var gatewayclass *gatewayv1beta1.GatewayClass
		BeforeEach(func() {
			gatewayclass = &gatewayv1beta1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mctc-gw-istio-external-instance-per-cluster",
					Namespace: "default",
				},
				Spec: gatewayv1beta1.GatewayClassSpec{
					ControllerName: "kuadrant.io/mctc-gw-controller",
				},
			}
		})

		AfterEach(func() {
			// Clean up GatewayClasses
			gatewayclassList := &gatewayv1beta1.GatewayClassList{}
			err := k8sClient.List(ctx, gatewayclassList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())
			for _, gatewayclass := range gatewayclassList.Items {
				err = k8sClient.Delete(ctx, &gatewayclass)
				Expect(err).NotTo(HaveOccurred())
			}
		})

		It("should accept a gatewayclass for this controller", func() {
			Expect(k8sClient.Create(ctx, gatewayclass)).To(BeNil())
			createdGatewayclass := &gatewayv1beta1.GatewayClass{}
			gatewayclassType := types.NamespacedName{Name: gatewayclass.Name, Namespace: gatewayclass.Namespace}

			// Exists
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayclassType, createdGatewayclass)
				return err == nil
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())

			// Status Accepted
			var condition metav1.Condition
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayclassType, createdGatewayclass)
				return err == nil && createdGatewayclass.Status.Conditions[0].Status == metav1.ConditionTrue
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
			condition = createdGatewayclass.Status.Conditions[0]
			Expect(len(createdGatewayclass.Status.Conditions)).To(BeEquivalentTo(1))
			Expect(condition.Type).To(BeEquivalentTo(gatewayv1beta1.GatewayClassConditionStatusAccepted))
			Expect(condition.Reason).To(BeEquivalentTo(gatewayv1beta1.GatewayClassConditionStatusAccepted))
		})

		It("should NOT accept a gatewayclass for a different controller", func() {
			gatewayclass.Name = "some-other-gatewayclass"
			gatewayclass.Spec.ControllerName = "example.com/some-other-controller"
			Expect(k8sClient.Create(ctx, gatewayclass)).To(BeNil())
			createdGatewayclass := &gatewayv1beta1.GatewayClass{}
			gatewayclassType := types.NamespacedName{Name: gatewayclass.Name, Namespace: gatewayclass.Namespace}

			// Exists
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayclassType, createdGatewayclass)
				return err == nil
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())

			// Only 1
			gatewayclassList := &gatewayv1beta1.GatewayClassList{}
			err := k8sClient.List(ctx, gatewayclassList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())
			Expect(len(gatewayclassList.Items)).To(BeEquivalentTo(1))

			// Status stays Unknown
			var condition metav1.Condition
			Consistently(func() bool {
				err := k8sClient.Get(ctx, gatewayclassType, createdGatewayclass)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				condition = createdGatewayclass.Status.Conditions[0]
				return condition.Type == string(gatewayv1beta1.GatewayClassConditionStatusAccepted) && condition.Status == metav1.ConditionUnknown
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
		})

		It("should NOT accept a gatewayclass that is 'unsupported'", func() {
			gatewayclass.Name = "test-class-name-1"
			Expect(k8sClient.Create(ctx, gatewayclass)).To(BeNil())
			createdGatewayclass := &gatewayv1beta1.GatewayClass{}
			gatewayclassType := types.NamespacedName{Name: gatewayclass.Name, Namespace: gatewayclass.Namespace}

			// Exists
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayclassType, createdGatewayclass)
				return err == nil
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())

			// Status is false
			var condition metav1.Condition
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayclassType, createdGatewayclass)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				condition = createdGatewayclass.Status.Conditions[0]
				return condition.Type == string(gatewayv1beta1.GatewayClassConditionStatusAccepted) && condition.Status == metav1.ConditionFalse
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
			Expect(condition.Reason).To(BeEquivalentTo(gatewayv1beta1.GatewayClassReasonInvalidParameters))
			Expect(condition.Message).To(BeEquivalentTo("Invalid Parameters - Unsupported class name test-class-name-1. Must be one of [mctc-gw-istio-external-instance-per-cluster]"))
		})
	})
})

var _ = Describe("GatewayController", func() {
	Context("testing gateway controller", func() {
		var gateway *gatewayv1beta1.Gateway
		var gatewayclass *gatewayv1beta1.GatewayClass
		var managedZone *v1alpha1.ManagedZone
		// var cluster1 *corev1.Secret
		BeforeEach(func() {
			// Create a test GatewayClass for test Gateways to reference
			gatewayclass = &gatewayv1beta1.GatewayClass{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "mctc-gw-istio-external-instance-per-cluster",
					Namespace: "default",
				},
				Spec: gatewayv1beta1.GatewayClassSpec{
					ControllerName: "kuadrant.io/mctc-gw-controller",
				},
			}
			Expect(k8sClient.Create(ctx, gatewayclass)).To(BeNil())

			// Create a test ManagedZone for test Gateway listeners to use
			managedZone = &v1alpha1.ManagedZone{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "example.com",
					Namespace: "default",
				},
				Spec: v1alpha1.ManagedZoneSpec{
					ID:          "1234",
					DomainName:  "example.com",
					Description: "example.com",
				},
			}
			Expect(k8sClient.Create(ctx, managedZone)).To(BeNil())

			// Stub Gateway for tests
			hostname := gatewayv1beta1.Hostname("test.example.com")
			gateway = &gatewayv1beta1.Gateway{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test-gw-1",
					Namespace: "default",
					Annotations: map[string]string{
						"kuadrant.io/gateway-cluster-label-selector": "type=test",
					},
				},
				Spec: gatewayv1beta1.GatewaySpec{
					GatewayClassName: "mctc-gw-istio-external-instance-per-cluster",
					Listeners: []gatewayv1beta1.Listener{
						{
							Name:     "default",
							Port:     8443,
							Protocol: gatewayv1beta1.HTTPSProtocolType,
							Hostname: &hostname,
						},
					},
				},
			}
		})

		AfterEach(func() {
			// Clean up Gateways
			gatewayList := &gatewayv1beta1.GatewayList{}
			err := k8sClient.List(ctx, gatewayList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())
			for _, gateway := range gatewayList.Items {
				err = k8sClient.Delete(ctx, &gateway)
				Expect(err).NotTo(HaveOccurred())
			}

			// Clean up GatewayClasses
			gatewayclassList := &gatewayv1beta1.GatewayClassList{}
			err = k8sClient.List(ctx, gatewayclassList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())
			for _, gatewayclass := range gatewayclassList.Items {
				err = k8sClient.Delete(ctx, &gatewayclass)
				Expect(err).NotTo(HaveOccurred())
			}

			// Clean up ManagedZones
			managedZoneList := &v1alpha1.ManagedZoneList{}
			err = k8sClient.List(ctx, managedZoneList, client.InNamespace("default"))
			Expect(err).NotTo(HaveOccurred())
			for _, managedZone := range managedZoneList.Items {
				err = k8sClient.Delete(ctx, &managedZone)
				Expect(err).NotTo(HaveOccurred())
			}

		})

		It("should reconcile a gateway", func() {
			Expect(k8sClient.Create(ctx, gateway)).To(BeNil())
			createdGateway := &gatewayv1beta1.Gateway{}
			gatewayType := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

			// Exists
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				return err == nil
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())

			// Accepted is True
			var acceptedCondition *metav1.Condition
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				acceptedCondition = conditions.GetConditionByType(createdGateway.Status.Conditions, string(gatewayv1beta1.GatewayConditionAccepted))
				log.Log.Info("acceptedCondition", "acceptedCondition", acceptedCondition)
				Expect(acceptedCondition).ToNot(BeNil())
				return acceptedCondition.Status == metav1.ConditionTrue
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
			Expect(acceptedCondition.Message).To(BeEquivalentTo("Handled by kuadrant.io/mctc-gw-controller"))

			// Programmed is Unknown for now
			var programmedCondition *metav1.Condition
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				programmedCondition = conditions.GetConditionByType(createdGateway.Status.Conditions, string(gatewayv1beta1.GatewayConditionProgrammed))
				log.Log.Info("programmedCondition", "programmedCondition", programmedCondition)
				Expect(programmedCondition).ToNot(BeNil())
				return programmedCondition.Status == metav1.ConditionUnknown
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())

			// create a certificate secret, mimicking what cert-manager would do at this point
			tlsSecret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "test.example.com",
					Namespace: "default",
				},
				StringData: map[string]string{
					"tls.key": "some_value",
				},
			}
			err := k8sClient.Create(ctx, tlsSecret)
			if err != nil {
				// explicitly fail as we should be passed the point of any errors
				Fail("Error creating mock certificate secret")
			}

			// update syncer status annoation, mimicking the status from the data plane Gateway
			err = k8sClient.Get(ctx, gatewayType, createdGateway)
			if err != nil {
				Fail("Error reading gateway")
			}
			annotations := createdGateway.GetAnnotations()
			annotations[fmt.Sprintf("%s%s", status.SyncerClusterStatusAnnotationPrefix, "test_cluster_one")] = "{\"addresses\":[{\"type\":\"IPAddress\",\"value\":\"172.32.200.0\"}],\"listeners\":[{\"attachedRoutes\":1,\"name\":\"default\"}]}"
			createdGateway.SetAnnotations(annotations)
			err = k8sClient.Update(ctx, createdGateway)
			if err != nil {
				Fail("Error updating gateway")
			}

			// Programmed is True
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				programmedCondition = conditions.GetConditionByType(createdGateway.Status.Conditions, string(gatewayv1beta1.GatewayConditionProgrammed))
				log.Log.Info("programmedCondition", "programmedCondition", programmedCondition)
				Expect(programmedCondition).ToNot(BeNil())
				return programmedCondition.Status == metav1.ConditionTrue
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
			Expect(programmedCondition.Message).To(BeEquivalentTo("Gateway configured in data plane cluster(s) - [test_cluster_one]"))

			// sync annotation is set
			syncAnnotation := createdGateway.Annotations[fmt.Sprintf("%s%s", syncer.MCTC_SYNC_ANNOTATION_PREFIX, syncer.MCTC_SYNC_ANNOTATION_WILDCARD)]
			Expect(syncAnnotation).To(BeEquivalentTo("true"))

			// TLS config added to listener
			listener := createdGateway.Spec.Listeners[0]
			Expect(listener.Name).To(BeEquivalentTo("default"))
			Expect(listener.TLS).ToNot(BeNil())
			certificateRef := listener.TLS.CertificateRefs[0]
			Expect(certificateRef.Name).To(BeEquivalentTo("test.example.com"))
		})

		It("should NOT match any clusters", func() {
			gateway.Annotations["kuadrant.io/gateway-cluster-label-selector"] = "type=prod"
			Expect(k8sClient.Create(ctx, gateway)).To(BeNil())
			createdGateway := &gatewayv1beta1.Gateway{}
			gatewayType := types.NamespacedName{Name: gateway.Name, Namespace: gateway.Namespace}

			// Exists
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				return err == nil
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())

			// Accepted is True
			var acceptedCondition *metav1.Condition
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				acceptedCondition = conditions.GetConditionByType(createdGateway.Status.Conditions, string(gatewayv1beta1.GatewayConditionAccepted))
				log.Log.Info("acceptedCondition", "acceptedCondition", acceptedCondition)
				Expect(acceptedCondition).ToNot(BeNil())
				return acceptedCondition.Status == metav1.ConditionTrue
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
			Expect(acceptedCondition.Message).To(BeEquivalentTo("Handled by kuadrant.io/mctc-gw-controller"))

			// Pending is False
			var programmedCondition *metav1.Condition
			Eventually(func() bool {
				err := k8sClient.Get(ctx, gatewayType, createdGateway)
				if err != nil {
					// explicitly fail as we should be passed the point of any errors
					log.Log.Error(err, "No errors expected")
					Fail("No errors expected")
				}
				programmedCondition = conditions.GetConditionByType(createdGateway.Status.Conditions, string(gatewayv1beta1.GatewayConditionProgrammed))
				log.Log.Info("programmedCondition", "programmedCondition", programmedCondition)
				Expect(programmedCondition).ToNot(BeNil())
				return programmedCondition.Status == metav1.ConditionFalse
			}, TestTimeoutMedium, TestRetryIntervalMedium).Should(BeTrue())
			Expect(programmedCondition.Message).To(BeEquivalentTo("No clusters match selection"))
		})
	})
})
