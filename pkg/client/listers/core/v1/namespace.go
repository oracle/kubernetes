/*
Copyright 2017 The Kubernetes Authors.

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

// This file was automatically generated by lister-gen

package v1

import (
	"k8s.io/apimachinery/pkg/api/errors"
	meta_v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/tools/cache"
	api "k8s.io/kubernetes/pkg/api"
	v1 "k8s.io/api/core/v1"
)

// NamespaceLister helps list Namespaces.
type NamespaceLister interface {
	// List lists all Namespaces in the indexer.
	List(selector labels.Selector) (ret []*v1.Namespace, err error)
	// Get retrieves the Namespace from the index for a given name.
	Get(name string) (*v1.Namespace, error)
	NamespaceListerExpansion
}

// namespaceLister implements the NamespaceLister interface.
type namespaceLister struct {
	indexer cache.Indexer
}

// NewNamespaceLister returns a new NamespaceLister.
func NewNamespaceLister(indexer cache.Indexer) NamespaceLister {
	return &namespaceLister{indexer: indexer}
}

// List lists all Namespaces in the indexer.
func (s *namespaceLister) List(selector labels.Selector) (ret []*v1.Namespace, err error) {
	err = cache.ListAll(s.indexer, selector, func(m interface{}) {
		ret = append(ret, m.(*v1.Namespace))
	})
	return ret, err
}

// Get retrieves the Namespace from the index for a given name.
func (s *namespaceLister) Get(name string) (*v1.Namespace, error) {
	key := &v1.Namespace{ObjectMeta: meta_v1.ObjectMeta{Name: name}}
	obj, exists, err := s.indexer.Get(key)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, errors.NewNotFound(api.Resource("namespace"), name)
	}
	return obj.(*v1.Namespace), nil
}
