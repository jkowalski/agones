// Copyright 2018 Google Inc. All Rights Reserved.
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

package gameserversets

import (
	"encoding/json"
	"sync"
	"time"

	"agones.dev/agones/pkg/apis/stable"
	"agones.dev/agones/pkg/apis/stable/v1alpha1"
	"agones.dev/agones/pkg/client/clientset/versioned"
	getterv1alpha1 "agones.dev/agones/pkg/client/clientset/versioned/typed/stable/v1alpha1"
	"agones.dev/agones/pkg/client/informers/externalversions"
	listerv1alpha1 "agones.dev/agones/pkg/client/listers/stable/v1alpha1"
	"agones.dev/agones/pkg/util/crd"
	"agones.dev/agones/pkg/util/runtime"
	"agones.dev/agones/pkg/util/webhooks"
	"agones.dev/agones/pkg/util/workerqueue"
	"github.com/heptiolabs/healthcheck"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	admv1beta1 "k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	extclientset "k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset"
	"k8s.io/apiextensions-apiserver/pkg/client/clientset/clientset/typed/apiextensions/v1beta1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
)

var (
	// ErrNoGameServerSetOwner is returned when a GameServerSet can't be found as an owner
	// for a GameServer
	ErrNoGameServerSetOwner = errors.New("No GameServerSet owner for this GameServer")
)

const (
	maxCreationParalellism         = 8
	maxGameServerCreationsPerBatch = 16

	maxDeletionParallelism         = 8
	maxGameServerDeletionsPerBatch = 16
)

// Controller is a the GameServerSet controller
type Controller struct {
	logger              *logrus.Entry
	crdGetter           v1beta1.CustomResourceDefinitionInterface
	gameServerGetter    getterv1alpha1.GameServersGetter
	gameServerLister    listerv1alpha1.GameServerLister
	gameServerSynced    cache.InformerSynced
	gameServerSetGetter getterv1alpha1.GameServerSetsGetter
	gameServerSetLister listerv1alpha1.GameServerSetLister
	gameServerSetSynced cache.InformerSynced
	workerqueue         *workerqueue.WorkerQueue
	allocationMutex     *sync.Mutex
	stop                <-chan struct{}
	recorder            record.EventRecorder
}

// NewController returns a new gameserverset crd controller
func NewController(
	wh *webhooks.WebHook,
	health healthcheck.Handler,
	allocationMutex *sync.Mutex,
	kubeClient kubernetes.Interface,
	extClient extclientset.Interface,
	agonesClient versioned.Interface,
	agonesInformerFactory externalversions.SharedInformerFactory) *Controller {

	gameServers := agonesInformerFactory.Stable().V1alpha1().GameServers()
	gsInformer := gameServers.Informer()
	gameServerSets := agonesInformerFactory.Stable().V1alpha1().GameServerSets()
	gsSetInformer := gameServerSets.Informer()

	c := &Controller{
		crdGetter:           extClient.ApiextensionsV1beta1().CustomResourceDefinitions(),
		gameServerGetter:    agonesClient.StableV1alpha1(),
		gameServerLister:    gameServers.Lister(),
		gameServerSynced:    gsInformer.HasSynced,
		gameServerSetGetter: agonesClient.StableV1alpha1(),
		gameServerSetLister: gameServerSets.Lister(),
		gameServerSetSynced: gsSetInformer.HasSynced,
		allocationMutex:     allocationMutex,
	}

	c.logger = runtime.NewLoggerWithType(c)
	c.workerqueue = workerqueue.NewWorkerQueueWithRateLimiter(c.syncGameServerSet, c.logger, stable.GroupName+".GameServerSetController",
		workqueue.NewItemFastSlowRateLimiter(100*time.Millisecond, 5*time.Second, 10))
	health.AddLivenessCheck("gameserverset-workerqueue", healthcheck.Check(c.workerqueue.Healthy))

	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(c.logger.Infof)
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: kubeClient.CoreV1().Events("")})
	c.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{Component: "gameserverset-controller"})

	wh.AddHandler("/validate", v1alpha1.Kind("GameServerSet"), admv1beta1.Update, c.updateValidationHandler)

	gsSetInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.workerqueue.Enqueue,
		UpdateFunc: func(oldObj, newObj interface{}) {
			oldGss := oldObj.(*v1alpha1.GameServerSet)
			newGss := newObj.(*v1alpha1.GameServerSet)
			if oldGss.Spec.Replicas != newGss.Spec.Replicas {
				c.workerqueue.EnqueueImmediately(newGss)
			}
		},
	})

	gsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: c.gameServerEventHandler,
		UpdateFunc: func(oldObj, newObj interface{}) {
			gs := newObj.(*v1alpha1.GameServer)
			// ignore if already being deleted
			if gs.ObjectMeta.DeletionTimestamp == nil {
				c.gameServerEventHandler(gs)
			}
		},
		DeleteFunc: c.gameServerEventHandler,
	})

	return c
}

// Run the GameServerSet controller. Will block until stop is closed.
// Runs threadiness number workers to process the rate limited queue
func (c *Controller) Run(workers int, stop <-chan struct{}) error {
	c.stop = stop

	err := crd.WaitForEstablishedCRD(c.crdGetter, "gameserversets."+stable.GroupName, c.logger)
	if err != nil {
		return err
	}

	c.logger.Info("Wait for cache sync")
	if !cache.WaitForCacheSync(stop, c.gameServerSynced, c.gameServerSetSynced) {
		return errors.New("failed to wait for caches to sync")
	}

	c.workerqueue.Run(workers, stop)
	return nil
}

// updateValidationHandler that validates a GameServerSet when is updated
// Should only be called on gameserverset update operations.
func (c *Controller) updateValidationHandler(review admv1beta1.AdmissionReview) (admv1beta1.AdmissionReview, error) {
	c.logger.WithField("review", review).Info("updateValidationHandler")

	newGss := &v1alpha1.GameServerSet{}
	oldGss := &v1alpha1.GameServerSet{}

	newObj := review.Request.Object
	if err := json.Unmarshal(newObj.Raw, newGss); err != nil {
		return review, errors.Wrapf(err, "error unmarshalling new GameServerSet json: %s", newObj.Raw)
	}

	oldObj := review.Request.OldObject
	if err := json.Unmarshal(oldObj.Raw, oldGss); err != nil {
		return review, errors.Wrapf(err, "error unmarshalling old GameServerSet json: %s", oldObj.Raw)
	}

	ok, causes := oldGss.ValidateUpdate(newGss)
	if !ok {
		review.Response.Allowed = false
		details := metav1.StatusDetails{
			Name:   review.Request.Name,
			Group:  review.Request.Kind.Group,
			Kind:   review.Request.Kind.Kind,
			Causes: causes,
		}
		review.Response.Result = &metav1.Status{
			Status:  metav1.StatusFailure,
			Message: "GameServer update is invalid",
			Reason:  metav1.StatusReasonInvalid,
			Details: &details,
		}

		c.logger.WithField("review", review).Info("Invalid GameServerSet update")
		return review, nil
	}

	return review, nil
}

func (c *Controller) gameServerEventHandler(obj interface{}) {
	gs, ok := obj.(*v1alpha1.GameServer)
	if !ok {
		return
	}

	ref := metav1.GetControllerOf(gs)
	if ref == nil {
		return
	}
	gsSet, err := c.gameServerSetLister.GameServerSets(gs.ObjectMeta.Namespace).Get(ref.Name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("ref", ref).Info("Owner GameServerSet no longer available for syncing")
		} else {
			runtime.HandleError(c.logger.WithField("gs", gs.ObjectMeta.Name).WithField("ref", ref),
				errors.Wrap(err, "error retrieving GameServer owner"))
		}
		return
	}
	c.workerqueue.EnqueueImmediately(gsSet)
}

// syncGameServer synchronises the GameServers for the Set,
// making sure there are aways as many GameServers as requested
func (c *Controller) syncGameServerSet(key string) error {
	c.logger.WithField("key", key).Info("syncGameServerSet")
	defer c.logger.WithField("key", key).Info("syncGameServerSet finished")

	// Convert the namespace/name string into a distinct namespace and name
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		// don't return an error, as we don't want this retried
		runtime.HandleError(c.logger.WithField("key", key), errors.Wrapf(err, "invalid resource key"))
		return nil
	}

	gsSet, err := c.gameServerSetLister.GameServerSets(namespace).Get(name)
	if err != nil {
		if k8serrors.IsNotFound(err) {
			c.logger.WithField("key", key).Info("GameServerSet is no longer available for syncing")
			return nil
		}
		return errors.Wrapf(err, "error retrieving GameServerSet %s from namespace %s", name, namespace)
	}

	list, err := ListGameServersByGameServerSetOwner(c.gameServerLister, gsSet)
	if err != nil {
		return err
	}
	if err := c.syncUnhealthyGameServers(gsSet, list); err != nil {
		return err
	}

	diff := gsSet.Spec.Replicas - int32(len(list))

	c.logger.WithField("key", key).Info("synchronizing more game servers")
	if err := c.syncMoreGameServers(gsSet, diff); err != nil {
		return err
	}
	c.logger.WithField("key", key).Info("removing excessive game servers")
	if err := c.removeExcessiveGameServers(gsSet, diff); err != nil {
		return err
	}
	c.logger.WithField("key", key).Info("syncing game server state")
	if err := c.syncGameServerSetState(gsSet, list); err != nil {
		return err
	}

	return nil
}

// syncUnhealthyGameServers deletes any unhealthy game servers (that are not already being deleted)
func (c *Controller) syncUnhealthyGameServers(gsSet *v1alpha1.GameServerSet, list []*v1alpha1.GameServer) error {
	for _, gs := range list {
		if gs.Status.State == v1alpha1.GameServerStateUnhealthy && gs.ObjectMeta.DeletionTimestamp.IsZero() {
			c.allocationMutex.Lock()
			err := c.gameServerGetter.GameServers(gs.ObjectMeta.Namespace).Delete(gs.ObjectMeta.Name, nil)
			c.allocationMutex.Unlock()
			if err != nil {
				return errors.Wrapf(err, "error deleting gameserver %s", gs.ObjectMeta.Name)
			}
			c.recorder.Eventf(gsSet, corev1.EventTypeNormal, "UnhealthyDelete", "Deleted gameserver: %s", gs.ObjectMeta.Name)
		}
	}

	return nil
}

// syncMoreGameServers adds diff more GameServers to the set
func (c *Controller) syncMoreGameServers(gsSet *v1alpha1.GameServerSet, diff int32) error {
	if diff <= 0 {
		return nil
	}
	c.logger.WithField("diff", diff).WithField("gameserverset", gsSet.ObjectMeta.Name).Info("Adding more gameservers")

	batchSize := int(diff)
	haveMoreItems := false
	if batchSize > maxGameServerCreationsPerBatch {
		batchSize = maxGameServerCreationsPerBatch
		haveMoreItems = true
	}
	if err := parallelize(generateNGameServers(batchSize, gsSet), maxCreationParalellism, func(gs *v1alpha1.GameServer) error {
		gs, err := c.gameServerGetter.GameServers(gs.Namespace).Create(gs)
		if err != nil {
			return errors.Wrapf(err, "error creating gameserver for gameserverset %s", gsSet.ObjectMeta.Name)
		}

		c.recorder.Eventf(gsSet, corev1.EventTypeNormal, "SuccessfulCreate", "Created gameserver: %s", gs.ObjectMeta.Name)
		return nil
	}); err != nil {
		return err
	}

	if haveMoreItems {
		c.workerqueue.EnqueueImmediately(gsSet)
	}
	return nil
}

func generateNGameServers(n int, gsSet *v1alpha1.GameServerSet) chan *v1alpha1.GameServer {
	gameServers := make(chan *v1alpha1.GameServer)
	go func() {
		defer close(gameServers)

		for i := 0; i < n; i++ {
			gameServers <- gsSet.GameServer()
		}
	}()

	return gameServers
}

// parallelize processes a channel of game server objects, invoking the provided callback for items in the channel with the specified degree of parallelism up to a limit.
// Returns nil if all callbacks returned nil or one of the error responses, not necessarily the first one.
func parallelize(gameServers chan *v1alpha1.GameServer, parallelism int, work func(gs *v1alpha1.GameServer) error) error {
	errch := make(chan error, parallelism)

	var wg sync.WaitGroup

	for i := 0; i < parallelism; i++ {
		wg.Add(1)

		go func() {
			defer wg.Done()
			for it := range gameServers {
				err := work(it)
				if err != nil {
					errch <- err
					break
				}
			}
		}()
	}
	wg.Wait()
	close(errch)

	for range gameServers {
		// drain any remaining game servers in the channel, in case we did not consume them all
	}

	// return first error from the channel, or nil if all successful.
	return <-errch
}

// removeExcessiveGameServers removes Ready GameServers from the set of GameServers
func (c *Controller) removeExcessiveGameServers(gsSet *v1alpha1.GameServerSet, diff int32) error {
	if diff >= 0 {
		return nil
	}
	// easier to manage positive numbers
	diff = -diff
	c.logger.WithField("diff", diff).WithField("gameserverset", gsSet.ObjectMeta.Name).Info("Deleting gameservers")

	// don't allow allocation state for GameServers to change
	c.allocationMutex.Lock()
	defer c.allocationMutex.Unlock()

	// make sure we are up to date with GameServer state
	if !cache.WaitForCacheSync(c.stop, c.gameServerSynced) {
		// if we can't sync the cache, then exit, and try and scale down
		// again, and then we aren't blocking allocation at this time.
		return errors.New("could not sync gameservers cache")
	}

	list, err := ListGameServersByGameServerSetOwner(c.gameServerLister, gsSet)
	if err != nil {
		return err
	}

	// // count anything that is already being deleted
	// for _, gs := range list {
	// 	if !gs.ObjectMeta.DeletionTimestamp.IsZero() {
	// 		diff--
	// 	}
	// }

	if gsSet.Spec.Scheduling == v1alpha1.Packed {
		list = filterGameServersOnLeastFullNodes(list, diff)
	}

	// prepare a channel of game servers to be delete, ignoring the ones that are allocated or in the process of being deleted.
	batchSize := maxGameServerDeletionsPerBatch
	haveMoreItems := false
	if batchSize > int(diff) {
		batchSize = int(diff)
	}

	// create a buffered channel for game servers to delete and add them up to a batch size limit.
	ch := make(chan *v1alpha1.GameServer, batchSize)
	cnt := 0
	for _, gs := range list {
		if gs.Status.State == v1alpha1.GameServerStateAllocated || !gs.ObjectMeta.DeletionTimestamp.IsZero() {
			// ignore
			continue
		}
		cnt++
		if cnt > batchSize {
			// no more room in the batch, but there are more items in the 'list'.
			// we will return error at the end of the batch to have the work retried.
			haveMoreItems = true
			break
		}
		ch <- gs
	}
	close(ch)

	if err := parallelize(ch, maxDeletionParallelism, func(gs *v1alpha1.GameServer) error {
		err := c.gameServerGetter.GameServers(gs.Namespace).Delete(gs.ObjectMeta.Name, nil)
		if err != nil {
			return errors.Wrapf(err, "error deleting gameserver for gameserverset %s", gsSet.ObjectMeta.Name)
		}

		c.recorder.Eventf(gsSet, corev1.EventTypeNormal, "SuccessfulDelete", "Deleted GameServer: %s", gs.ObjectMeta.Name)
		return nil
	}); err != nil {
		return err
	}

	if haveMoreItems {
		c.workerqueue.EnqueueImmediately(gsSet)
	}

	return nil
}

// syncGameServerSetState synchronises the GameServerSet State with active GameServer counts
func (c *Controller) syncGameServerSetState(gsSet *v1alpha1.GameServerSet, list []*v1alpha1.GameServer) error {
	rc := int32(0)
	ac := int32(0)
	for _, gs := range list {
		if gs.ObjectMeta.DeletionTimestamp.IsZero() {
			switch gs.Status.State {
			case v1alpha1.GameServerStateReady:
				rc++
			case v1alpha1.GameServerStateAllocated:
				ac++
			}
		}
	}

	status := v1alpha1.GameServerSetStatus{
		Replicas:          int32(len(list)),
		ReadyReplicas:     rc,
		AllocatedReplicas: ac,
	}
	if gsSet.Status != status {
		gsSetCopy := gsSet.DeepCopy()
		gsSetCopy.Status = status
		_, err := c.gameServerSetGetter.GameServerSets(gsSet.ObjectMeta.Namespace).Update(gsSetCopy)
		if err != nil {
			return errors.Wrapf(err, "error updating status on GameServerSet %s", gsSet.ObjectMeta.Name)
		}
	}
	return nil
}
