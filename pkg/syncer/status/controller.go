package status

import (
	"context"
	"fmt"
	"reflect"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/json"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/Kuadrant/multi-cluster-traffic-controller/pkg/syncer"
)

const (
	controllerName                      = "mctc-status-syncing-controller"
	SyncerClusterStatusAnnotationPrefix = "mctc-status-syncer-status-"
	NUM_THREADS                         = 8
)

type Controller struct {
	queue workqueue.RateLimitingInterface

	upstreamClient   dynamic.Interface
	downstreamClient dynamic.Interface

	upstreamNamespaces []string
	downstreamNS       string

	syncTargetName string
}

func NewStatusSyncer(syncTargetName string, upstreamClient dynamic.Interface, downstreamClient dynamic.Interface, cfg syncer.Config) (*Controller, error) {
	c := &Controller{
		queue:              workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), controllerName),
		upstreamClient:     upstreamClient,
		downstreamClient:   downstreamClient,
		syncTargetName:     syncTargetName,
		upstreamNamespaces: cfg.UpstreamNamespaces,
		downstreamNS:       cfg.DownstreamNS,
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

	if err := c.process(ctx, qk.gvr, qk.key); err != nil {
		utilruntime.HandleError(fmt.Errorf("%s failed to sync %q, err: %w", controllerName, key, err))
		c.queue.AddRateLimited(key)
		return true
	}

	c.queue.Forget(key)

	return true
}
func (c *Controller) process(ctx context.Context, gvr schema.GroupVersionResource, key string) error {
	logger := log.FromContext(ctx)
	logger.Info("status controller processing", "key", key, "gvr", gvr.String())

	// from upstream
	upstreamNamespace, upstreamName, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		logger.Error(err, "Invalid key")
		return nil
	}
	logger.Info("got upstream object reference", "namespace", upstreamNamespace, "name", upstreamName)

	obj, err := c.upstreamClient.Resource(gvr).Namespace(upstreamNamespace).Get(ctx, upstreamName, metav1.GetOptions{})

	if apierrors.IsNotFound(err) {
		logger.Error(err, "upstream resource not found")
		return err
	} else if err != nil {
		logger.Error(err, "error retrieving upstream object")
		return err
	}

	err = c.updateStatus(ctx, gvr, obj)
	if err != nil {
		logger.Error(err, "error updating downstream status")
	}
	return err
}

func (c *Controller) updateStatus(ctx context.Context, gvr schema.GroupVersionResource, upstreamObj *unstructured.Unstructured) error {
	logger := log.FromContext(ctx)

	upstreamStatus, statusExists, err := unstructured.NestedFieldCopy(upstreamObj.UnstructuredContent(), "status")
	if !statusExists {
		logger.Info("upstream resource doesn't contain a status. Skipping updating the status of downstream resource")
		return nil
	} else if err != nil {
		logger.Error(err, "error getting status from upstream")
		return err
	}

	existingObj, err := c.downstreamClient.Resource(gvr).Namespace(c.downstreamNS).Get(ctx, upstreamObj.GetName(), metav1.GetOptions{})
	if err != nil {
		logger.Error(err, "Error getting downstream resource")
		return err
	}

	newDownstream := existingObj.DeepCopy()

	statusAnnotationValue, err := json.Marshal(upstreamStatus)
	if err != nil {
		logger.Error(err, "error formatting status to JSON")
		return err
	}
	newDownstreamAnnotations := newDownstream.GetAnnotations()
	if newDownstreamAnnotations == nil {
		newDownstreamAnnotations = make(map[string]string)
	}
	newDownstreamAnnotations[SyncerClusterStatusAnnotationPrefix+c.syncTargetName] = string(statusAnnotationValue)
	newDownstream.SetAnnotations(newDownstreamAnnotations)

	if reflect.DeepEqual(existingObj, newDownstream) {
		logger.Info("No need to update the status annotation of downstream resource")
		return nil
	}

	_, err = c.downstreamClient.Resource(gvr).Namespace(c.downstreamNS).Update(ctx, newDownstream, metav1.UpdateOptions{})

	if err != nil {
		logger.Error(err, "Failed updating the status annotation of downstream resource")
		return err
	}
	logger.Info("Updated the status annotation of downstream resource")
	return nil

}
