// Copyright The Monogon Project Authors.
// SPDX-License-Identifier: Apache-2.0

package object

// Taken and modified from the Kubernetes plugin of CoreDNS, under Apache 2.0.

import (
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/cache"
)

// KeyFunc works like cache.DeletionHandlingMetaNamespaceKeyFunc
// but uses format "<name>.<namespace>" instead of "<namespace>/<name>".
// This makes lookup for a service slightly more efficient, because we can
// just use a slice of the query name instead of constructing a new string.
func KeyFunc(obj interface{}) (string, error) {
	if d, ok := obj.(cache.DeletedFinalStateUnknown); ok {
		return d.Key, nil
	}
	objMeta, err := meta.Accessor(obj)
	if err != nil {
		return "", fmt.Errorf("object has no meta: %w", err)
	}
	if len(objMeta.GetNamespace()) == 0 {
		return objMeta.GetName(), nil
	}
	return objMeta.GetName() + "." + objMeta.GetNamespace(), nil
}

// NewIndexerInformer is a copy of the cache.NewIndexerInformer function,
// but allows custom process function.
func NewIndexerInformer(lw cache.ListerWatcher, objType runtime.Object, h cache.ResourceEventHandler, indexers cache.Indexers, builder ProcessorBuilder) (cache.Indexer, cache.Controller) {
	clientState := cache.NewIndexer(KeyFunc, indexers)

	cfg := &cache.Config{
		Queue:            cache.NewDeltaFIFOWithOptions(cache.DeltaFIFOOptions{KeyFunction: KeyFunc, KnownObjects: clientState}),
		ListerWatcher:    lw,
		ObjectType:       objType,
		FullResyncPeriod: 0,
		Process:          builder(clientState, h),
	}
	return clientState, cache.New(cfg)
}

// DefaultProcessor is based on the Process function from
// cache.NewIndexerInformer except it does a conversion.
func DefaultProcessor(convert ToFunc) ProcessorBuilder {
	return func(clientState cache.Indexer, h cache.ResourceEventHandler) cache.ProcessFunc {
		return func(obj interface{}, isInitialList bool) error {
			for _, d := range obj.(cache.Deltas) {
				switch d.Type {
				case cache.Sync, cache.Added, cache.Updated:
					metaObj := d.Object.(metav1.Object)
					obj, err := convert(metaObj)
					if err != nil {
						return err
					}
					if old, exists, err := clientState.Get(obj); err == nil && exists {
						if err := clientState.Update(obj); err != nil {
							return err
						}
						h.OnUpdate(old, obj)
					} else {
						if err := clientState.Add(obj); err != nil {
							return err
						}
						h.OnAdd(obj, isInitialList)
					}
				case cache.Deleted:
					var obj interface{}
					obj, ok := d.Object.(cache.DeletedFinalStateUnknown)
					if !ok {
						var err error
						metaObj, ok := d.Object.(metav1.Object)
						if !ok {
							return fmt.Errorf("unexpected object %v", d.Object)
						}
						obj, err = convert(metaObj)
						if err != nil {
							return err
						}
					}

					if err := clientState.Delete(obj); err != nil {
						return err
					}
					h.OnDelete(obj)
				}
			}
			return nil
		}
	}
}
