package spec

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
)

const (
	controllerName                      = "mctc-spec-syncing-controller"
	SyncerFinalizerNamePrefix           = "mctc-spec-syncer-finalizer/"
	SyncerDeletionAnnotationPrefix      = "mctc-spec-syncer-deletion-timestamp-"
	SyncerClusterStatusAnnotationPrefix = "mctc-spec-syncer-status-"
	syncerApplyManager                  = "syncer"
	NUM_THREADS                         = 8
)

type Controller struct {
	queue workqueue.RateLimitingInterface

	upstreamClient   dynamic.Interface
	downstreamClient dynamic.Interface

	syncTargetName     string
	upstreamNamespaces []string
	downstreamNS       string

	mutators []syncer.Mutator
}

func NewSpecSyncer(syncTargetName string, upstreamClient dynamic.Interface, downstreamClient dynamic.Interface, cfg syncer.Config) (*Controller, error) {
	c := &Controller{
		queue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		upstreamClient:     upstreamClient,
		downstreamClient:   downstreamClient,
		syncTargetName:     syncTargetName,
		upstreamNamespaces: cfg.UpstreamNamespaces,
		downstreamNS:       cfg.DownstreamNS,
		mutators:           cfg.Mutators,
	}

	return c, nil
}

type queueKey struct {
	gvr schema.GroupVersionResource
	key string // meta namespace key
}

func (c *Controller) AddToQueue(gvr schema.GroupVersionResource, obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		utilruntime.HandleError(err)
		return
	}

	c.queue.Add(
		queueKey{
			gvr: gvr,
			key: key,
		},
	)
}

// Start starts N worker processes each processing work items.
func (c *Controller) Start(ctx context.Context) {
	defer utilruntime.HandleCrash()
	defer c.queue.ShutDown()

	for i := 0; i < NUM_THREADS; i++ {
		go wait.UntilWithContext(ctx, c.startWorker, time.Second)
	}

	<-ctx.Done()
}

// startWorker processes work items until stopCh is closed.
func (c *Controller) startWorker(ctx context.Context) {
	for c.processNextWorkItem(ctx) {
	}
}

func (c *Controller) processNextWorkItem(ctx context.Context) bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	qk := key.(queueKey)

	// No matter what, tell the queue we're done with this key, to unblock
	// other workers.
	defer c.queue.Done(key)

	retryAfter, err := c.process(ctx, qk.gvr, qk.key)
	if err != nil {
		utilruntime.HandleError(fmt.Errorf("%s failed to sync %q, err: %w", controllerName, key, err))
		c.queue.AddRateLimited(key)
		return true
	} else if retryAfter != nil {
		c.queue.AddAfter(key, *retryAfter)
		return true
	}

	c.queue.Forget(key)

	return true
}

func (c *Controller) process(ctx context.Context, gvr schema.GroupVersionResource, key string) (retryAfter *time.Duration, err error) {
	logger := log.FromContext(ctx)
	logger.Info("spec controller process", "key", key, "gvr", gvr.String())
	// from upstream
	upstreamNamespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error(err, "Invalid key")
		return nil, nil
	}

	// get the upstream object
	upstreamUnstructuredObject, err := c.upstreamClient.Resource(gvr).Namespace(upstreamNamespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		logger.Error(err, "error finding upstream object")
		return nil, err
	} else if errors.IsNotFound(err) {
		logger.Info("upstream object not found.", "gvr", gvr, "namespace", upstreamNamespace, "name", name)
		// deleted upstream => delete downstream
		logger.Info("Deleting downstream object for upstream object")
		err = c.downstreamClient.Resource(gvr).Namespace(c.downstreamNS).Delete(ctx, name, metav1.DeleteOptions{})
		if err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
		//TODO If the resource is namespaced, let's plan the cleanup of it's namespace.
		return nil, nil
	}

	for _, mutator := range c.mutators {
		err := mutator.Mutate(syncer.MutatorConfig{ClusterID: c.syncTargetName, Logger: logger}, upstreamUnstructuredObject)
		if err != nil {
			return nil, fmt.Errorf("error from %v mutator: %v", mutator.GetName(), err.Error())
		}
	}

	// upsert downstream
	if err := c.ensureDownstreamNamespaceExists(ctx, c.downstreamNS); err != nil {
		return nil, err
	}

	if added, err := c.ensureSyncerFinalizer(ctx, gvr, upstreamUnstructuredObject); added {
		// The successful update of the upstream resource finalizer will trigger a new reconcile
		return nil, nil
	} else if err != nil {
		return nil, err
	}

	return nil, c.applyToDownstream(ctx, gvr, c.downstreamNS, upstreamUnstructuredObject)
}

func (c *Controller) ensureDownstreamNamespaceExists(ctx context.Context, downstreamNamespace string) error {
	logger := log.FromContext(ctx)

	namespaces := c.downstreamClient.Resource(schema.GroupVersionResource{
		Group:    "",
		Version:  "v1",
		Resource: "namespaces",
	})

	newNamespace := &unstructured.Unstructured{}
	newNamespace.SetAPIVersion("v1")
	newNamespace.SetKind("Namespace")
	newNamespace.SetName(downstreamNamespace)

	_, err := namespaces.Create(ctx, newNamespace, metav1.CreateOptions{})
	if err == nil || !errors.IsAlreadyExists(err) {
		logger.Info("Created downstream namespace for upstream namespace")
		return nil
	}

	return nil
}

func (c *Controller) ensureSyncerFinalizer(ctx context.Context, gvr schema.GroupVersionResource, upstreamObj *unstructured.Unstructured) (bool, error) {
	logger := log.FromContext(ctx)

	upstreamFinalizers := upstreamObj.GetFinalizers()
	hasFinalizer := false
	for _, finalizer := range upstreamFinalizers {
		if finalizer == SyncerFinalizerNamePrefix+c.syncTargetName {
			hasFinalizer = true
		}
	}

	intendedToBeRemovedFromLocation := upstreamObj.GetAnnotations()[SyncerDeletionAnnotationPrefix+c.syncTargetName] != ""

	stillOwnedByExternalActorForLocation := upstreamObj.GetAnnotations()[SyncerFinalizerNamePrefix+c.syncTargetName] != ""

	if !hasFinalizer && (!intendedToBeRemovedFromLocation || stillOwnedByExternalActorForLocation) {
		upstreamObjCopy := upstreamObj.DeepCopy()
		namespace := upstreamObjCopy.GetNamespace()

		upstreamFinalizers = append(upstreamFinalizers, SyncerFinalizerNamePrefix+c.syncTargetName)
		upstreamObjCopy.SetFinalizers(upstreamFinalizers)
		if _, err := c.upstreamClient.Resource(gvr).Namespace(namespace).Update(ctx, upstreamObjCopy, metav1.UpdateOptions{}); err != nil {
			logger.Error(err, "Failed adding finalizer on upstream resource")
			return false, err
		}
		logger.Info("Updated upstream resource with syncer finalizer")
		return true, nil
	}

	return false, nil
}

func (c *Controller) applyToDownstream(ctx context.Context, gvr schema.GroupVersionResource, downstreamNamespace string, upstreamObj *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)

	downstreamObj := upstreamObj.DeepCopy()

	intendedToBeRemovedFromLocation := upstreamObj.GetAnnotations()[SyncerDeletionAnnotationPrefix+c.syncTargetName] != ""

	stillOwnedByExternalActorForLocation := upstreamObj.GetAnnotations()[SyncerFinalizerNamePrefix+c.syncTargetName] != ""

	if intendedToBeRemovedFromLocation && !stillOwnedByExternalActorForLocation {
		var err error
		if downstreamNamespace != "" {
			err = c.downstreamClient.Resource(gvr).Namespace(downstreamNamespace).Delete(ctx, downstreamObj.GetName(), metav1.DeleteOptions{})
		} else {
			err = c.downstreamClient.Resource(gvr).Delete(ctx, downstreamObj.GetName(), metav1.DeleteOptions{})
		}
		if err != nil {
			if errors.IsNotFound(err) {
				// That's not an error.
				// Just think about removing the finalizer from the KCP location-specific resource:
				return c.EnsureUpstreamFinalizerRemoved(ctx, gvr, upstreamObj.GetNamespace(), upstreamObj.GetName())
			}
			logger.Error(err, "Error deleting upstream resource from downstream")
			return err
		}
		//TODO clean up namespace
		logger.Info("Deleted upstream resource from downstream")
		return nil
	}

	//TODO jsonpatch applied
	// HARDCODED PATCHES - REMOVE THESE
	if upstreamObj.GetKind() == "Gateway" {
		// Patch gatewayClassName
		downstreamObj.Object["spec"].(map[string]interface{})["gatewayClassName"] = "istio"

		// Patch certificateRefs namespace
		// listeners:
		// - tls:
		// 	   certificateRefs:
		// 	   - group: ""
		// 	     kind: Secret
		// 	     name: test.dev.hcpapps.net
		// 	     namespace: multi-cluster-traffic-controller-system
		listeners := downstreamObj.Object["spec"].(map[string]interface{})["listeners"].([]interface{})
		for _, listener := range listeners {
			tlsConfig := listener.(map[string]interface{})["tls"]
			if tlsConfig != nil {
				certificateRefs := tlsConfig.(map[string]interface{})["certificateRefs"].([]interface{})
				for _, certificateRef := range certificateRefs {
					certificateRef.(map[string]interface{})["namespace"] = downstreamNamespace
				}
			}
		}
	}

	downstreamObj.SetUID("")
	downstreamObj.SetResourceVersion("")
	downstreamObj.SetNamespace(downstreamNamespace)
	downstreamObj.SetManagedFields(nil)

	// Strip cluster name annotation
	downstreamAnnotations := downstreamObj.GetAnnotations()
	delete(downstreamAnnotations, SyncerClusterStatusAnnotationPrefix+c.syncTargetName)

	// If we're left with 0 annotations, nil out the map so it's not included in the patch
	if len(downstreamAnnotations) == 0 {
		downstreamAnnotations = nil
	}
	downstreamObj.SetAnnotations(downstreamAnnotations)

	// Deletion fields are immutable and set by the downstream API server
	downstreamObj.SetDeletionTimestamp(nil)
	downstreamObj.SetDeletionGracePeriodSeconds(nil)
	// Strip owner references, to avoid orphaning by broken references,
	// and make sure cascading deletion is only performed once upstream.
	downstreamObj.SetOwnerReferences(nil)
	// Strip finalizers to avoid the deletion of the downstream resource from being blocked.
	downstreamObj.SetFinalizers(nil)

	// replace upstream state label with downstream cluster label. We don't want to leak upstream state machine
	// state to downstream, and also we don't need downstream updates every time the upstream state machine changes.
	labels := downstreamObj.GetLabels()
	downstreamObj.SetLabels(labels)

	// Marshalling the unstructured object is good enough as SSA patch
	data, err := json.Marshal(downstreamObj)
	if err != nil {
		return err
	}

	_, err = c.downstreamClient.Resource(gvr).Namespace(downstreamNamespace).Patch(ctx, downstreamObj.GetName(), types.ApplyPatchType, data, metav1.PatchOptions{FieldManager: syncerApplyManager, Force: pointer.Bool(true)})

	if err != nil {
		logger.Error(err, "Error upserting upstream resource to downstream")
		return err
	}
	logger.Info("Upserted upstream resource to downstream")

	return nil
}

func (c *Controller) EnsureUpstreamFinalizerRemoved(ctx context.Context, gvr schema.GroupVersionResource, upstreamNamespace string, resourceName string) error {
	logger := log.FromContext(ctx)

	upstreamObj, err := c.upstreamClient.Resource(gvr).Namespace(upstreamNamespace).Get(ctx, resourceName, metav1.GetOptions{})
	if err != nil && !errors.IsNotFound(err) {
		return err
	}
	if errors.IsNotFound(err) {
		return nil
	}

	if upstreamObj.GetAnnotations()[SyncerDeletionAnnotationPrefix+c.syncTargetName] == "" {
		// Do nothing: the object should not be deleted anymore for this location on the KCP side
		return nil
	}

	upstreamObj = upstreamObj.DeepCopy()

	// Remove the syncer finalizer.
	currentFinalizers := upstreamObj.GetFinalizers()
	var desiredFinalizers []string
	for _, finalizer := range currentFinalizers {
		if finalizer != SyncerFinalizerNamePrefix+c.syncTargetName {
			desiredFinalizers = append(desiredFinalizers, finalizer)
		}
	}
	upstreamObj.SetFinalizers(desiredFinalizers)
	annotations := upstreamObj.GetAnnotations()
	delete(annotations, SyncerClusterStatusAnnotationPrefix+c.syncTargetName)
	delete(annotations, SyncerDeletionAnnotationPrefix+c.syncTargetName)
	upstreamObj.SetAnnotations(annotations)

	_, err = c.upstreamClient.Resource(gvr).Namespace(upstreamObj.GetNamespace()).Update(ctx, upstreamObj, metav1.UpdateOptions{})
	if err != nil {
		logger.Error(err, "Failed updating upstream resource after removing the syncer finalizer")
		return err
	}
	logger.Info("Updated upstream resource to remove the syncer finalizer")
	return nil
}
