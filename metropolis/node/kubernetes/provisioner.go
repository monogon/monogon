// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package kubernetes

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"time"

	"golang.org/x/sys/unix"
	v1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrs "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/client-go/informers"
	coreinformers "k8s.io/client-go/informers/core/v1"
	storageinformers "k8s.io/client-go/informers/storage/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	ref "k8s.io/client-go/tools/reference"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/utils/ptr"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/core/localstorage"
	"source.monogon.dev/osbase/fsquota"
	"source.monogon.dev/osbase/logtree"
	"source.monogon.dev/osbase/supervisor"
)

// inodeCapacityRatio describes the ratio between the byte capacity of a volume
// and its inode capacity. One inode on XFS is 512 bytes and by default 25%
// (1/4) of capacity can be used for metadata.
const inodeCapacityRatio = 4 * 512

// ONCHANGE(//metropolis/node/kubernetes/reconciler:resources_csi.go): needs to
// match csiProvisionerServerName declared.
const csiProvisionerServerName = "dev.monogon.metropolis.vfs"

// csiProvisionerServer is responsible for the provisioning and deprovisioning
// of CSI-based container volumes. It runs on all nodes and watches PVCs for
// ones assigned to the node it's running on and fulfills the provisioning
// request by creating a directory, applying a quota and creating the
// corresponding PV. When the PV is released and its retention policy is
// Delete, the directory and the PV resource are deleted.
type csiProvisionerServer struct {
	NodeName         string
	Kubernetes       kubernetes.Interface
	InformerFactory  informers.SharedInformerFactory
	VolumesDirectory *localstorage.DataVolumesDirectory

	claimQueue           workqueue.TypedDelayingInterface[string]
	claimRateLimiter     workqueue.TypedRateLimiter[string]
	claimNextTry         map[string]time.Time
	pvQueue              workqueue.TypedRateLimitingInterface[string]
	recorder             record.EventRecorder
	pvcInformer          coreinformers.PersistentVolumeClaimInformer
	pvInformer           coreinformers.PersistentVolumeInformer
	storageClassInformer storageinformers.StorageClassInformer
	pvcMutationCache     cache.MutationCache
	pvMutationCache      cache.MutationCache
	// processMutex ensures that the two workers (one for PVCs and one for PVs)
	// are not doing work concurrently.
	processMutex sync.Mutex
	logger       logging.Leveled
}

// runCSIProvisioner runs the main provisioning machinery. It consists of a
// bunch of informers which keep track of the events happening on the
// Kubernetes control plane and informs us when something happens. If anything
// happens to PVCs or PVs, we enqueue the identifier of that resource in a work
// queue. Queues are being worked on by only one worker to limit load and avoid
// complicated locking infrastructure. Failed items are requeued.
func (p *csiProvisionerServer) Run(ctx context.Context) error {
	p.logger = supervisor.Logger(ctx)

	// The recorder is used to log Kubernetes events for successful or failed
	// volume provisions. These events then show up in `kubectl describe pvc`
	// and can be used by admins to debug issues with this provisioner.
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: p.Kubernetes.CoreV1().Events("")})
	p.recorder = eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: csiProvisionerServerName, Host: p.NodeName})

	p.pvcInformer = p.InformerFactory.Core().V1().PersistentVolumeClaims()
	p.pvInformer = p.InformerFactory.Core().V1().PersistentVolumes()
	p.storageClassInformer = p.InformerFactory.Storage().V1().StorageClasses()

	klogger := logtree.NewKlogLogger(p.logger)
	p.pvcMutationCache = cache.NewIntegerResourceVersionMutationCache(klogger, p.pvcInformer.Informer().GetStore(), nil, time.Minute, false)
	p.pvMutationCache = cache.NewIntegerResourceVersionMutationCache(klogger, p.pvInformer.Informer().GetStore(), nil, time.Minute, false)

	p.claimQueue = workqueue.NewTypedDelayingQueue[string]()
	p.claimRateLimiter = workqueue.NewTypedItemExponentialFailureRateLimiter[string](time.Second, 5*time.Minute)
	p.claimNextTry = make(map[string]time.Time)
	p.pvQueue = workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[string]())

	p.pvcInformer.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		p.logger.Errorf("pvcInformer watch error: %v", err)
	})
	p.pvcInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: p.enqueueClaim,
		UpdateFunc: func(old, new interface{}) {
			p.enqueueClaim(new)
		},
		// We need to handle deletes to ensure that deleted keys are removed from
		// the rate limiter, because there are cases where we leave a key in the
		// rate limiter without scheduling a retry.
		DeleteFunc: p.enqueueClaim,
	})
	p.pvInformer.Informer().AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: p.enqueuePV,
		UpdateFunc: func(old, new interface{}) {
			p.enqueuePV(new)
		},
	})
	p.pvInformer.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		p.logger.Errorf("pvInformer watch error: %v", err)
	})

	p.storageClassInformer.Informer().SetWatchErrorHandler(func(_ *cache.Reflector, err error) {
		p.logger.Errorf("storageClassInformer watch error: %v", err)
	})

	go p.pvcInformer.Informer().Run(ctx.Done())
	go p.pvInformer.Informer().Run(ctx.Done())
	go p.storageClassInformer.Informer().Run(ctx.Done())

	// These will self-terminate once the queues are shut down
	go p.processQueueItems(p.claimQueue, func(key string) {
		p.processPVCRetryWrapper(ctx, key)
	})
	go p.processQueueItems(p.pvQueue, func(key string) {
		p.processPVRetryWrapper(ctx, key)
	})

	supervisor.Signal(ctx, supervisor.SignalHealthy)
	<-ctx.Done()
	p.claimQueue.ShutDown()
	p.pvQueue.ShutDown()
	return nil
}

// isOurPVC checks if the given PVC is is to be provisioned by this provisioner
// and has been scheduled onto this node
func (p *csiProvisionerServer) isOurPVC(pvc *v1.PersistentVolumeClaim) bool {
	if pvc.ObjectMeta.Annotations["volume.beta.kubernetes.io/storage-provisioner"] != csiProvisionerServerName {
		return false
	}
	if pvc.ObjectMeta.Annotations["volume.kubernetes.io/selected-node"] != p.NodeName {
		return false
	}
	return true
}

// isOurPV checks if the given PV has been provisioned by this provisioner and
// has been scheduled onto this node
func (p *csiProvisionerServer) isOurPV(pv *v1.PersistentVolume) bool {
	if pv.Spec.CSI == nil || pv.Spec.CSI.Driver != csiProvisionerServerName {
		return false
	}
	if pv.Spec.NodeAffinity.Required.NodeSelectorTerms[0].MatchExpressions[0].Values[0] != p.NodeName {
		return false
	}
	return true
}

// enqueueClaim adds an added/changed PVC to the work queue
func (p *csiProvisionerServer) enqueueClaim(obj interface{}) {
	key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
	if err != nil {
		p.logger.Errorf("Not queuing PVC because key could not be derived: %v", err)
		return
	}
	p.claimQueue.Add(key)
}

// enqueuePV adds an added/changed PV to the work queue
func (p *csiProvisionerServer) enqueuePV(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		p.logger.Errorf("Not queuing PV because key could not be derived: %v", err)
		return
	}
	p.pvQueue.Add(key)
}

// processQueueItems gets items from the given work queue and calls the process
// function for each of them. It self- terminates once the queue is shut down.
func (p *csiProvisionerServer) processQueueItems(queue workqueue.TypedInterface[string], process func(key string)) {
	for {
		obj, shutdown := queue.Get()
		if shutdown {
			return
		}

		func(obj string) {
			defer queue.Done(obj)

			p.processMutex.Lock()
			defer p.processMutex.Unlock()

			process(obj)
		}(obj)
	}
}

var errSkipRateLimitReset = errors.New("skip ratelimit reset")

func (p *csiProvisionerServer) processPVCRetryWrapper(ctx context.Context, key string) {
	err := p.processPVC(ctx, key)
	if errors.Is(err, errSkipRateLimitReset) {
		// ignore
	} else if err != nil {
		p.logger.Warningf("Failed processing PVC %s, requeueing (numrequeues: %d): %v", key, p.claimRateLimiter.NumRequeues(key), err)
		duration := p.claimRateLimiter.When(key)
		p.claimNextTry[key] = time.Now().Add(duration)
		p.claimQueue.AddAfter(key, duration)
	} else {
		p.claimRateLimiter.Forget(key)
		delete(p.claimNextTry, key)
	}
}

func (p *csiProvisionerServer) processPVRetryWrapper(ctx context.Context, key string) {
	if err := p.processPV(ctx, key); err != nil {
		p.logger.Warningf("Failed processing PV %s, requeueing (numrequeues: %d): %v", key, p.pvQueue.NumRequeues(key), err)
		p.pvQueue.AddRateLimited(key)
	} else {
		p.pvQueue.Forget(key)
	}
}

// volumePath gets the path where the volume is stored.
func (p *csiProvisionerServer) volumePath(volumeID string) string {
	return filepath.Join(p.VolumesDirectory.FullPath(), volumeID)
}

// processPVC looks at a single PVC item from the queue, determines if it needs
// to be provisioned and logs the provisioning result to the recorder
func (p *csiProvisionerServer) processPVC(ctx context.Context, key string) error {
	val, exists, err := p.pvcMutationCache.GetByKey(key)
	if err != nil {
		return fmt.Errorf("failed to get PVC for processing: %w", err)
	}
	if !exists {
		return nil // nothing to do, no error
	}
	pvc, ok := val.(*v1.PersistentVolumeClaim)
	if !ok {
		return fmt.Errorf("value in MutationCache is not a PVC: %+v", val)
	}

	if !p.isOurPVC(pvc) {
		return nil
	}

	if pvc.Spec.VolumeName == "" {
		// The claim is pending, so we may need to provision it.
		storageClass, err := p.storageClassInformer.Lister().Get(*pvc.Spec.StorageClassName)
		if err != nil {
			return fmt.Errorf("could not get storage class: %w", err)
		}

		if storageClass.Provisioner != csiProvisionerServerName {
			// We're not responsible for this PVC. Can only happen if
			// controller-manager makes a mistake setting the annotations, but
			// we're bailing here anyways for safety.
			return nil
		}

		err = p.provisionPVC(ctx, pvc, storageClass)

		if err != nil {
			p.recorder.Eventf(pvc, v1.EventTypeWarning, "ProvisioningFailed", "Failed to provision PV: %v", err)
			return err
		}
	} else if pvc.Status.Phase == v1.ClaimBound {
		// The claim is bound, so we may need to resize it.
		requestSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
		statusSize := pvc.Status.Capacity[v1.ResourceStorage]
		if requestSize.Cmp(statusSize) <= 0 {
			// No resize needed.
			return nil
		}

		val, exists, err := p.pvMutationCache.GetByKey(pvc.Spec.VolumeName)
		if err != nil {
			return fmt.Errorf("failed to get PV of PVC %s: %w", key, err)
		}
		if !exists {
			return nil
		}
		pv, ok := val.(*v1.PersistentVolume)
		if !ok {
			return fmt.Errorf("value in MutationCache is not a PV: %+v", val)
		}
		if pv.Status.Phase != v1.VolumeBound || pv.Spec.ClaimRef == nil || pv.Spec.ClaimRef.UID != pvc.UID {
			return nil
		}
		if !p.isOurPV(pv) {
			return nil
		}

		err = p.processResize(ctx, pvc, pv)
		if errors.Is(err, errSkipRateLimitReset) {
			return err
		} else if err != nil {
			p.recorder.Eventf(pvc, v1.EventTypeWarning, "VolumeResizeFailed", "Failed to resize PV: %v", err)
			return fmt.Errorf("failed to process resize of PVC %s: %w", key, err)
		}
	}
	return nil
}

// provisionPVC creates the directory where the volume lives, sets a quota for
// the requested amount of storage and creates the PV object representing this
// new volume
func (p *csiProvisionerServer) provisionPVC(ctx context.Context, pvc *v1.PersistentVolumeClaim, storageClass *storagev1.StorageClass) error {
	key := cache.MetaObjectToName(pvc).String()
	claimRef, err := ref.GetReference(scheme.Scheme, pvc)
	if err != nil {
		return fmt.Errorf("failed to get reference to PVC: %w", err)
	}

	storageReq := pvc.Spec.Resources.Requests[v1.ResourceStorage]
	capacity, err := quantityToBytes(storageReq)
	if err != nil {
		return err
	}
	newSize := *resource.NewQuantity(capacity, resource.BinarySI)

	volumeID := "pvc-" + string(pvc.ObjectMeta.UID)
	if _, err := p.pvInformer.Lister().Get(volumeID); err == nil {
		return nil // Volume already exists.
	}
	volumePath := p.volumePath(volumeID)
	volumeMode := ptr.Deref(pvc.Spec.VolumeMode, "")
	if volumeMode == "" {
		volumeMode = v1.PersistentVolumeFilesystem
	}

	p.logger.Infof("Creating persistent volume %s with mode %s and size %s for claim %s", volumeID, volumeMode, newSize.String(), key)

	switch volumeMode {
	case v1.PersistentVolumeFilesystem:
		if err := os.Mkdir(volumePath, 0644); err != nil && !os.IsExist(err) {
			return fmt.Errorf("failed to create volume directory: %w", err)
		}
		files, err := os.ReadDir(volumePath)
		if err != nil {
			return fmt.Errorf("failed to list files in newly-created volume: %w", err)
		}
		if len(files) > 0 {
			return errors.New("newly-created volume already contains data, bailing")
		}
		if err := fsquota.SetQuota(volumePath, uint64(capacity), uint64(capacity)/inodeCapacityRatio); err != nil {
			return fmt.Errorf("failed to update quota: %w", err)
		}
	case v1.PersistentVolumeBlock:
		imageFile, err := os.OpenFile(volumePath, os.O_CREATE|os.O_RDWR, 0644)
		if err != nil {
			return fmt.Errorf("failed to create volume image: %w", err)
		}
		defer imageFile.Close()
		if err := allocateBlockVolume(imageFile, capacity); err != nil {
			return fmt.Errorf("failed to allocate volume image: %w", err)
		}
	default:
		return fmt.Errorf("VolumeMode %q is unsupported", *pvc.Spec.VolumeMode)
	}

	vol := &v1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: volumeID,
			Annotations: map[string]string{
				"pv.kubernetes.io/provisioned-by": csiProvisionerServerName,
			},
		},
		Spec: v1.PersistentVolumeSpec{
			AccessModes: []v1.PersistentVolumeAccessMode{v1.ReadWriteOnce},
			Capacity: v1.ResourceList{
				v1.ResourceStorage: newSize,
			},
			PersistentVolumeSource: v1.PersistentVolumeSource{
				CSI: &v1.CSIPersistentVolumeSource{
					Driver:       csiProvisionerServerName,
					VolumeHandle: volumeID,
				},
			},
			ClaimRef:   claimRef,
			VolumeMode: pvc.Spec.VolumeMode,
			NodeAffinity: &v1.VolumeNodeAffinity{
				Required: &v1.NodeSelector{
					NodeSelectorTerms: []v1.NodeSelectorTerm{
						{
							MatchExpressions: []v1.NodeSelectorRequirement{
								{
									Key:      "kubernetes.io/hostname",
									Operator: v1.NodeSelectorOpIn,
									Values:   []string{p.NodeName},
								},
							},
						},
					},
				},
			},
			StorageClassName:              *pvc.Spec.StorageClassName,
			PersistentVolumeReclaimPolicy: *storageClass.ReclaimPolicy,
		},
	}

	_, err = p.Kubernetes.CoreV1().PersistentVolumes().Create(ctx, vol, metav1.CreateOptions{})
	if err != nil && !apierrs.IsAlreadyExists(err) {
		return fmt.Errorf("failed to create PV object: %w", err)
	}
	return nil
}

// See https://github.com/kubernetes-csi/external-resizer/blob/master/pkg/controller/expand_and_recover.go
func (p *csiProvisionerServer) processResize(ctx context.Context, pvc *v1.PersistentVolumeClaim, pv *v1.PersistentVolume) error {
	key := cache.MetaObjectToName(pvc).String()
	requestSize := pvc.Spec.Resources.Requests[v1.ResourceStorage]
	allocatedSize, hasAllocatedSize := pvc.Status.AllocatedResources[v1.ResourceStorage]
	pvSize := pv.Spec.Capacity[v1.ResourceStorage]
	resizeStatus := pvc.Status.AllocatedResourceStatuses[v1.ResourceStorage]

	newSize := requestSize
	if hasAllocatedSize {
		// Usually, we want to keep resizing to the same target size once we have
		// picked one and ignore changes in request size.
		newSize = allocatedSize
	}
	switch resizeStatus {
	case v1.PersistentVolumeClaimNodeResizePending,
		v1.PersistentVolumeClaimNodeResizeInProgress:
		// We are waiting for node resize. The PV should be large enough at this
		// point, which means we don't need to do anything here.
		if pvSize.Cmp(newSize) >= 0 || pvSize.Cmp(requestSize) >= 0 {
			// We don't need to do anything and don't need to schedule a retry, but we
			// still don't want to reset the rate limiter in case the node resize
			// fails repeatedly.
			return errSkipRateLimitReset
		}
	case "", v1.PersistentVolumeClaimControllerResizeInfeasible:
		// In this case, there is no ongoing or partially complete resize operation,
		// and we can be sure that the actually allocated size is equal to pvSize.
		// That means it's safe to pick a new target size.
		if pvSize.Cmp(requestSize) < 0 {
			newSize = requestSize
		}
	}
	capacity, err := quantityToBytes(newSize)
	if err != nil {
		return err
	}

	keepConditions := false
	if hasAllocatedSize && allocatedSize.Cmp(newSize) == 0 {
		now := time.Now()
		if p.claimNextTry[key].After(now) {
			// Not enough time has passed since the last attempt and the target size
			// is still the same.
			p.claimQueue.AddAfter(key, p.claimNextTry[key].Sub(now))
			return errSkipRateLimitReset
		}
		keepConditions = true
	}

	newPVC := pvc.DeepCopy()
	mapSet(&newPVC.Status.AllocatedResources, v1.ResourceStorage, newSize)
	mapSet(&newPVC.Status.AllocatedResourceStatuses, v1.ResourceStorage, v1.PersistentVolumeClaimControllerResizeInProgress)
	conditions := []v1.PersistentVolumeClaimCondition{{
		Type:               v1.PersistentVolumeClaimResizing,
		Status:             v1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
	}}
	mergeResizeConditionOnPVC(newPVC, conditions, keepConditions)
	pvc, err = p.patchPVCStatus(ctx, pvc, newPVC)
	if err != nil {
		return fmt.Errorf("failed to update PVC before resizing: %w", err)
	}

	expandedSize := *resource.NewQuantity(capacity, resource.BinarySI)
	p.logger.Infof("Resizing persistent volume %s to new size %s", pv.Spec.CSI.VolumeHandle, expandedSize.String())
	err = p.controllerExpandVolume(pv, capacity)
	if err != nil {
		// If the resize fails because the requested size is too large, then set
		// status to infeasible, which allows the user to change the request to a
		// smaller size.
		isInfeasible := errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.EDQUOT) || errors.Is(err, unix.EFBIG) || errors.Is(err, unix.EINVAL)
		newPVC = pvc.DeepCopy()
		if isInfeasible {
			mapSet(&newPVC.Status.AllocatedResourceStatuses, v1.ResourceStorage, v1.PersistentVolumeClaimControllerResizeInfeasible)
		}
		conditions = []v1.PersistentVolumeClaimCondition{{
			Type:               v1.PersistentVolumeClaimControllerResizeError,
			Status:             v1.ConditionTrue,
			LastTransitionTime: metav1.Now(),
			Message:            fmt.Sprintf("Failed to expand PV: %v", err),
		}}
		mergeResizeConditionOnPVC(newPVC, conditions, true)
		_, patchErr := p.patchPVCStatus(ctx, pvc, newPVC)
		if patchErr != nil {
			return fmt.Errorf("failed to update PVC after resizing: %w", patchErr)
		}
		return fmt.Errorf("failed to expand PV: %w", err)
	}

	newPV := pv.DeepCopy()
	newPV.Spec.Capacity[v1.ResourceStorage] = expandedSize
	pv, err = patchPV(ctx, p.Kubernetes, pv, newPV)
	if err != nil {
		return fmt.Errorf("failed to update PV with new capacity: %w", err)
	}
	p.pvMutationCache.Mutation(pv)

	newPVC = pvc.DeepCopy()
	mapSet(&newPVC.Status.AllocatedResourceStatuses, v1.ResourceStorage, v1.PersistentVolumeClaimNodeResizePending)
	conditions = []v1.PersistentVolumeClaimCondition{{
		Type:               v1.PersistentVolumeClaimFileSystemResizePending,
		Status:             v1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
	}}
	mergeResizeConditionOnPVC(newPVC, conditions, true)
	_, err = p.patchPVCStatus(ctx, pvc, newPVC)
	if err != nil {
		return fmt.Errorf("failed to update PVC after resizing: %w", err)
	}

	return nil
}

func (p *csiProvisionerServer) controllerExpandVolume(pv *v1.PersistentVolume, capacity int64) error {
	volumePath := p.volumePath(pv.Spec.CSI.VolumeHandle)
	switch ptr.Deref(pv.Spec.VolumeMode, "") {
	case "", v1.PersistentVolumeFilesystem:
		if err := fsquota.SetQuota(volumePath, uint64(capacity), uint64(capacity)/inodeCapacityRatio); err != nil {
			return fmt.Errorf("failed to update quota: %w", err)
		}
		return nil
	case v1.PersistentVolumeBlock:
		imageFile, err := os.OpenFile(volumePath, os.O_RDWR, 0)
		if err != nil {
			return fmt.Errorf("failed to open block volume backing file: %w", err)
		}
		defer imageFile.Close()
		if err := allocateBlockVolume(imageFile, capacity); err != nil {
			return fmt.Errorf("failed to allocate space: %w", err)
		}
		return nil
	default:
		return fmt.Errorf("VolumeMode %q is unsupported", *pv.Spec.VolumeMode)
	}
}

func allocateBlockVolume(imageFile *os.File, capacity int64) error {
	// On XFS, fallocate is not atomic: It allocates space in steps of around
	// 8 GB, and does not check upfront if there is enough space to satisfy the
	// entire allocation. As the last step, if allocation succeeded, it updates
	// the file size. This means that fallocate can fail and leave the file size
	// unchanged, but still allocate part of the requested capacity past EOF.
	//
	// To clean this up, we truncate the file to its current size, which leaves
	// the size unchanged but removes allocated space past EOF. We also do this if
	// fallocate succeeds, in case a previous allocation has left space past EOF
	// and was not cleaned up.
	allocErr := unix.Fallocate(int(imageFile.Fd()), 0, 0, capacity)
	info, err := imageFile.Stat()
	if err != nil {
		return err
	}
	err = imageFile.Truncate(info.Size())
	if err != nil {
		return err
	}
	if allocErr != nil {
		return fmt.Errorf("fallocate: %w", allocErr)
	}
	return nil
}

// processPV looks at a single PV item from the queue and checks if it has been
// released and needs to be deleted. If yes it deletes the associated quota,
// directory and the PV object and logs the result to the recorder.
func (p *csiProvisionerServer) processPV(ctx context.Context, key string) error {
	_, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return fmt.Errorf("invalid resource key: %s", key)
	}
	pv, err := p.pvInformer.Lister().Get(name)
	if apierrs.IsNotFound(err) {
		return nil // nothing to do, no error
	} else if err != nil {
		return fmt.Errorf("failed to get PV for processing: %w", err)
	}

	if !p.isOurPV(pv) {
		return nil
	}
	if pv.Status.Phase == v1.VolumeBound && pv.Spec.ClaimRef != nil {
		// Resize processing depends on both the PV and the claim. Instead of
		// directly retrieving the claim here and calling processResize, we add it
		// to the claimQueue. This ensures that all resize retries are handled by
		// the claimQueue.
		claimKey := cache.NewObjectName(pv.Spec.ClaimRef.Namespace, pv.Spec.ClaimRef.Name).String()
		p.claimQueue.Add(claimKey)
	}
	if pv.ObjectMeta.DeletionTimestamp != nil {
		return nil
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != v1.PersistentVolumeReclaimDelete || pv.Status.Phase != v1.VolumeReleased {
		return nil
	}
	volumePath := p.volumePath(pv.Spec.CSI.VolumeHandle)

	// Log deletes for auditing purposes
	p.logger.Infof("Deleting persistent volume %s", pv.Spec.CSI.VolumeHandle)
	switch ptr.Deref(pv.Spec.VolumeMode, "") {
	case "", v1.PersistentVolumeFilesystem:
		if err := fsquota.SetQuota(volumePath, 0, 0); err != nil && !os.IsNotExist(err) {
			// We record these here manually since a successful deletion
			// removes the PV we'd be attaching them to.
			p.recorder.Eventf(pv, v1.EventTypeWarning, "DeprovisioningFailed", "Failed to remove quota: %v", err)
			return fmt.Errorf("failed to remove quota: %w", err)
		}
		if err := os.RemoveAll(volumePath); err != nil {
			p.recorder.Eventf(pv, v1.EventTypeWarning, "DeprovisioningFailed", "Failed to delete volume: %v", err)
			return fmt.Errorf("failed to delete volume: %w", err)
		}
	case v1.PersistentVolumeBlock:
		if err := os.Remove(volumePath); err != nil && !os.IsNotExist(err) {
			p.recorder.Eventf(pv, v1.EventTypeWarning, "DeprovisioningFailed", "Failed to delete volume: %v", err)
			return fmt.Errorf("failed to delete volume: %w", err)
		}
	default:
		p.recorder.Eventf(pv, v1.EventTypeWarning, "DeprovisioningFailed", "Invalid volume mode \"%v\"", *pv.Spec.VolumeMode)
		return fmt.Errorf("invalid volume mode \"%v\"", *pv.Spec.VolumeMode)
	}

	err = p.Kubernetes.CoreV1().PersistentVolumes().Delete(ctx, pv.Name, metav1.DeleteOptions{})
	if err != nil && !apierrs.IsNotFound(err) {
		p.recorder.Eventf(pv, v1.EventTypeWarning, "DeprovisioningFailed", "Failed to delete PV object from K8s API: %v", err)
		return fmt.Errorf("failed to delete PV object: %w", err)
	}
	return nil
}

// quantityToBytes returns size rounded up to an integer amount.
// Based on Kubernetes staging/src/k8s.io/cloud-provider/volume/helpers/rounding.go
func quantityToBytes(size resource.Quantity) (int64, error) {
	if size.CmpInt64(math.MaxInt64) >= 0 {
		return 0, fmt.Errorf("quantity %s is too big, overflows int64", size.String())
	}
	val := size.Value()
	if val <= 0 {
		return 0, fmt.Errorf("invalid quantity %s, must be positive", size.String())
	}
	return val, nil
}

// patchPVCStatus, createPVCPatch, addResourceVersion, patchPV,
// mergeResizeConditionOnPVC are taken from Kubernetes
// pkg/volume/util/resize_util.go under Apache 2.0 and modified.

// patchPVCStatus updates a PVC using patch instead of update. Update should not
// be used because when the client is an older version, it will drop fields
// which it does not support. It's done this way in both kubelet and
// external-resizer.
func (p *csiProvisionerServer) patchPVCStatus(
	ctx context.Context,
	oldPVC *v1.PersistentVolumeClaim,
	newPVC *v1.PersistentVolumeClaim) (*v1.PersistentVolumeClaim, error) {
	patchBytes, err := createPVCPatch(oldPVC, newPVC, true /* addResourceVersionCheck */)
	if err != nil {
		return oldPVC, fmt.Errorf("failed to create PVC patch: %w", err)
	}

	updatedClaim, updateErr := p.Kubernetes.CoreV1().PersistentVolumeClaims(oldPVC.Namespace).
		Patch(ctx, oldPVC.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{}, "status")
	if updateErr != nil {
		return oldPVC, fmt.Errorf("failed to patch PVC object: %w", updateErr)
	}
	p.pvcMutationCache.Mutation(updatedClaim)
	return updatedClaim, nil
}

func createPVCPatch(
	oldPVC *v1.PersistentVolumeClaim,
	newPVC *v1.PersistentVolumeClaim, addResourceVersionCheck bool) ([]byte, error) {
	oldData, err := json.Marshal(oldPVC)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal old data: %w", err)
	}

	newData, err := json.Marshal(newPVC)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal new data: %w", err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, oldPVC)
	if err != nil {
		return nil, fmt.Errorf("failed to create 2 way merge patch: %w", err)
	}

	if addResourceVersionCheck {
		patchBytes, err = addResourceVersion(patchBytes, oldPVC.ResourceVersion)
		if err != nil {
			return nil, fmt.Errorf("failed to add resource version: %w", err)
		}
	}

	return patchBytes, nil
}

func addResourceVersion(patchBytes []byte, resourceVersion string) ([]byte, error) {
	var patchMap map[string]interface{}
	err := json.Unmarshal(patchBytes, &patchMap)
	if err != nil {
		return nil, fmt.Errorf("error unmarshalling patch: %w", err)
	}
	u := unstructured.Unstructured{Object: patchMap}
	u.SetResourceVersion(resourceVersion)
	versionBytes, err := json.Marshal(patchMap)
	if err != nil {
		return nil, fmt.Errorf("error marshalling json patch: %w", err)
	}
	return versionBytes, nil
}

func patchPV(
	ctx context.Context,
	kubeClient kubernetes.Interface,
	oldPV *v1.PersistentVolume,
	newPV *v1.PersistentVolume) (*v1.PersistentVolume, error) {
	oldData, err := json.Marshal(oldPV)
	if err != nil {
		return oldPV, fmt.Errorf("failed to marshal old data: %w", err)
	}

	newData, err := json.Marshal(newPV)
	if err != nil {
		return oldPV, fmt.Errorf("failed to marshal new data: %w", err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, oldPV)
	if err != nil {
		return oldPV, fmt.Errorf("failed to create 2 way merge patch: %w", err)
	}

	updatedPV, err := kubeClient.CoreV1().PersistentVolumes().
		Patch(ctx, oldPV.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		return oldPV, fmt.Errorf("failed to patch PV object: %w", err)
	}
	return updatedPV, nil
}

var knownResizeConditions = map[v1.PersistentVolumeClaimConditionType]bool{
	v1.PersistentVolumeClaimFileSystemResizePending: true,
	v1.PersistentVolumeClaimResizing:                true,
	v1.PersistentVolumeClaimControllerResizeError:   true,
	v1.PersistentVolumeClaimNodeResizeError:         true,
}

// mergeResizeConditionOnPVC updates pvc with requested resize conditions
// leaving other conditions untouched.
func mergeResizeConditionOnPVC(
	pvc *v1.PersistentVolumeClaim,
	resizeConditions []v1.PersistentVolumeClaimCondition,
	keepOldResizeConditions bool) {
	resizeConditionMap := map[v1.PersistentVolumeClaimConditionType]v1.PersistentVolumeClaimCondition{}
	for _, condition := range resizeConditions {
		resizeConditionMap[condition.Type] = condition
	}

	var newConditions []v1.PersistentVolumeClaimCondition
	for _, condition := range pvc.Status.Conditions {
		// If Condition is of not resize type, we keep it.
		if _, ok := knownResizeConditions[condition.Type]; !ok {
			newConditions = append(newConditions, condition)
			continue
		}

		if newCondition, ok := resizeConditionMap[condition.Type]; ok {
			newConditions = append(newConditions, newCondition)
			delete(resizeConditionMap, condition.Type)
		} else if keepOldResizeConditions {
			newConditions = append(newConditions, condition)
		}
	}

	for _, newCondition := range resizeConditionMap {
		newConditions = append(newConditions, newCondition)
	}
	pvc.Status.Conditions = newConditions
}

// mapSet is like `(*m)[key] = value` but also initializes *m if it is nil.
func mapSet[Map ~map[K]V, K comparable, V any](m *Map, key K, value V) {
	if *m == nil {
		*m = make(map[K]V)
	}
	(*m)[key] = value
}
