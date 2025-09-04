// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package networkpolicy

import (
	"context"
	"fmt"

	"git.dolansoft.org/dolansoft/k8s-nft-npc/nftctrl"
	v1 "k8s.io/api/core/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/cache/synctrack"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/kubectl/pkg/scheme"

	"source.monogon.dev/go/logging"
	"source.monogon.dev/metropolis/node/allocs"
	"source.monogon.dev/osbase/supervisor"
)

type Service struct {
	Kubernetes kubernetes.Interface
}

type workItem struct {
	typ  string
	name cache.ObjectName
}

type updateEnqueuer struct {
	typ          string
	q            workqueue.TypedInterface[workItem]
	hasProcessed *synctrack.AsyncTracker[workItem]
	l            logging.Leveled
}

func (c *updateEnqueuer) OnAdd(obj interface{}, isInInitialList bool) {
	name, err := cache.ObjectToName(obj)
	if err != nil {
		c.l.Warningf("OnAdd name for type %q cannot be derived: %v", c.typ, err)
		return
	}
	item := workItem{typ: c.typ, name: name}
	if isInInitialList {
		c.hasProcessed.Start(item)
	}
	c.q.Add(item)
}

func (c *updateEnqueuer) OnUpdate(oldObj, newObj interface{}) {
	name, err := cache.ObjectToName(newObj)
	if err != nil {
		c.l.Warningf("OnUpdate name for type %q cannot be derived: %v", c.typ, err)
		return
	}
	c.q.Add(workItem{typ: c.typ, name: name})
}

func (c *updateEnqueuer) OnDelete(obj interface{}) {
	name, err := cache.DeletionHandlingObjectToName(obj)
	if err != nil {
		c.l.Warningf("OnDelete name for type %q cannot be derived: %v", c.typ, err)
		return
	}
	c.q.Add(workItem{typ: c.typ, name: name})
}

func (c *Service) Run(ctx context.Context) error {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&typedcorev1.EventSinkImpl{Interface: c.Kubernetes.CoreV1().Events("")})
	recorder := eventBroadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "npc"})

	nft, err := nftctrl.New(recorder, allocs.LinkGroupK8sPod)
	if err != nil {
		return fmt.Errorf("failed to create nftables controller: %w", err)
	}
	defer nft.Close()
	l := supervisor.Logger(ctx)

	informerFactory := informers.NewSharedInformerFactory(c.Kubernetes, 0)
	q := workqueue.NewTypedWithConfig(workqueue.TypedQueueConfig[workItem]{
		Name: "networkpolicy",
	})

	var hasProcessed synctrack.AsyncTracker[workItem]

	nsInformer := informerFactory.Core().V1().Namespaces()
	nsHandler, _ := nsInformer.Informer().AddEventHandler(&updateEnqueuer{q: q, typ: "ns", hasProcessed: &hasProcessed, l: l})
	podInformer := informerFactory.Core().V1().Pods()
	podHandler, _ := podInformer.Informer().AddEventHandler(&updateEnqueuer{q: q, typ: "pod", hasProcessed: &hasProcessed, l: l})
	nwpInformer := informerFactory.Networking().V1().NetworkPolicies()
	nwpHandler, _ := nwpInformer.Informer().AddEventHandler(&updateEnqueuer{q: q, typ: "nwp", hasProcessed: &hasProcessed, l: l})
	hasProcessed.UpstreamHasSynced = func() bool {
		return nsHandler.HasSynced() && podHandler.HasSynced() && nwpHandler.HasSynced()
	}
	informerFactory.Start(ctx.Done())

	go func() {
		<-ctx.Done()
		q.ShutDown()
		informerFactory.Shutdown()
	}()

	hasSynced := false
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		i, shut := q.Get()
		if shut {
			return ctx.Err()
		}
		switch i.typ {
		case "pod":
			pod, _ := podInformer.Lister().Pods(i.name.Namespace).Get(i.name.Name)
			nft.SetPod(i.name, pod)
		case "nwp":
			nwp, _ := nwpInformer.Lister().NetworkPolicies(i.name.Namespace).Get(i.name.Name)
			nft.SetNetworkPolicy(i.name, nwp)
		case "ns":
			ns, _ := nsInformer.Lister().Get(i.name.Name)
			nft.SetNamespace(i.name.Name, ns)
		}
		hasProcessed.Finished(i)
		if hasSynced {
			if err := nft.Flush(); err != nil {
				return fmt.Errorf("failed to flush after update of %s %v: %w", i.typ, i.name, err)
			}
		} else if hasProcessed.HasSynced() {
			if err := nft.Flush(); err != nil {
				return fmt.Errorf("initial flush failed: %w", err)
			}
			l.Info("Initial sync completed")
			supervisor.Signal(ctx, supervisor.SignalHealthy)
			hasSynced = true
		}
		q.Done(i)
	}
}
