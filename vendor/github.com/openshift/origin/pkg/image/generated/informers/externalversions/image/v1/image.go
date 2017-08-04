// This file was automatically generated by informer-gen

package v1

import (
	image_v1 "github.com/openshift/origin/pkg/image/apis/image/v1"
	clientset "github.com/openshift/origin/pkg/image/generated/clientset"
	internalinterfaces "github.com/openshift/origin/pkg/image/generated/informers/externalversions/internalinterfaces"
	v1 "github.com/openshift/origin/pkg/image/generated/listers/image/v1"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
	time "time"
)

// ImageInformer provides access to a shared informer and lister for
// Images.
type ImageInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1.ImageLister
}

type imageInformer struct {
	factory internalinterfaces.SharedInformerFactory
}

func newImageInformer(client clientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	sharedIndexInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options meta_v1.ListOptions) (runtime.Object, error) {
				return client.ImageV1().Images().List(options)
			},
			WatchFunc: func(options meta_v1.ListOptions) (watch.Interface, error) {
				return client.ImageV1().Images().Watch(options)
			},
		},
		&image_v1.Image{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	return sharedIndexInformer
}

func (f *imageInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&image_v1.Image{}, newImageInformer)
}

func (f *imageInformer) Lister() v1.ImageLister {
	return v1.NewImageLister(f.Informer().GetIndexer())
}
