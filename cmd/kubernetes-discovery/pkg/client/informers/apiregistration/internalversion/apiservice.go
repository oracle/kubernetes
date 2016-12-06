/*
Copyright 2016 The Kubernetes Authors.

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

// This file was automatically generated by informer-gen with arguments: --input-dirs=[k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/apis/apiregistration,k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/apis/apiregistration/v1alpha1] --internal-clientset-package=k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/clientset_generated/internalclientset --listers-package=k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/listers --output-package=k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/informers --versioned-clientset-package=k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/clientset_generated/release_1_5

package internalversion

import (
	apiregistration "k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/apis/apiregistration"
	internalclientset "k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/clientset_generated/internalclientset"
	internal_interfaces "k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/informers/internal_interfaces"
	internalversion "k8s.io/kubernetes/cmd/kubernetes-discovery/pkg/client/listers/apiregistration/internalversion"
	api "k8s.io/kubernetes/pkg/api"
	v1 "k8s.io/kubernetes/pkg/api/v1"
	cache "k8s.io/kubernetes/pkg/client/cache"
	runtime "k8s.io/kubernetes/pkg/runtime"
	watch "k8s.io/kubernetes/pkg/watch"
	time "time"
)

// APIServiceInformer provides access to a shared informer and lister for
// APIServices.
type APIServiceInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() internalversion.APIServiceLister
}

type aPIServiceInformer struct {
	factory internal_interfaces.SharedInformerFactory
}

func newAPIServiceInformer(client internalclientset.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	sharedIndexInformer := cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				var internalOptions api.ListOptions
				if err := api.Scheme.Convert(&options, &internalOptions, nil); err != nil {
					return nil, err
				}
				return client.Apiregistration().APIServices().List(internalOptions)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				var internalOptions api.ListOptions
				if err := api.Scheme.Convert(&options, &internalOptions, nil); err != nil {
					return nil, err
				}
				return client.Apiregistration().APIServices().Watch(internalOptions)
			},
		},
		&apiregistration.APIService{},
		resyncPeriod,
		cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc},
	)

	return sharedIndexInformer
}

func (f *aPIServiceInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InternalInformerFor(&apiregistration.APIService{}, newAPIServiceInformer)
}

func (f *aPIServiceInformer) Lister() internalversion.APIServiceLister {
	return internalversion.NewAPIServiceLister(f.Informer().GetIndexer())
}
