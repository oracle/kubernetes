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

package rest

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang/glog"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/rest"
	"k8s.io/kubernetes/pkg/api/unversioned"
	"k8s.io/kubernetes/pkg/apimachinery/registered"
	"k8s.io/kubernetes/pkg/apiserver"
	policyclient "k8s.io/kubernetes/pkg/client/clientset_generated/internalclientset/typed/policy/internalversion"
	"k8s.io/kubernetes/pkg/client/restclient"
	"k8s.io/kubernetes/pkg/genericapiserver"
	kubeletclient "k8s.io/kubernetes/pkg/kubelet/client"
	"k8s.io/kubernetes/pkg/master/ports"
	"k8s.io/kubernetes/pkg/registry/core/componentstatus"
	configmapstore "k8s.io/kubernetes/pkg/registry/core/configmap/storage"
	controllerstore "k8s.io/kubernetes/pkg/registry/core/controller/storage"
	"k8s.io/kubernetes/pkg/registry/core/endpoint"
	endpointstore "k8s.io/kubernetes/pkg/registry/core/endpoint/storage"
	eventstore "k8s.io/kubernetes/pkg/registry/core/event/storage"
	limitrangestore "k8s.io/kubernetes/pkg/registry/core/limitrange/storage"
	"k8s.io/kubernetes/pkg/registry/core/namespace"
	namespacestore "k8s.io/kubernetes/pkg/registry/core/namespace/storage"
	"k8s.io/kubernetes/pkg/registry/core/node"
	nodestore "k8s.io/kubernetes/pkg/registry/core/node/storage"
	pvstore "k8s.io/kubernetes/pkg/registry/core/persistentvolume/storage"
	pvcstore "k8s.io/kubernetes/pkg/registry/core/persistentvolumeclaim/storage"
	podstore "k8s.io/kubernetes/pkg/registry/core/pod/storage"
	podtemplatestore "k8s.io/kubernetes/pkg/registry/core/podtemplate/storage"
	"k8s.io/kubernetes/pkg/registry/core/rangeallocation"
	resourcequotastore "k8s.io/kubernetes/pkg/registry/core/resourcequota/storage"
	secretstore "k8s.io/kubernetes/pkg/registry/core/secret/storage"
	"k8s.io/kubernetes/pkg/registry/core/service"
	"k8s.io/kubernetes/pkg/registry/core/service/allocator"
	allocatorstore "k8s.io/kubernetes/pkg/registry/core/service/allocator/storage"
	ipallocator "k8s.io/kubernetes/pkg/registry/core/service/ipallocator"
	"k8s.io/kubernetes/pkg/registry/core/service/portallocator"
	servicestore "k8s.io/kubernetes/pkg/registry/core/service/storage"
	serviceaccountstore "k8s.io/kubernetes/pkg/registry/core/serviceaccount/storage"
	etcdutil "k8s.io/kubernetes/pkg/storage/etcd/util"
	utilnet "k8s.io/kubernetes/pkg/util/net"
)

// LegacyRESTStorageProvider provides information needed to build RESTStorage for core, but
// does NOT implement the "normal" RESTStorageProvider (yet!)
type LegacyRESTStorageProvider struct {
	StorageFactory genericapiserver.StorageFactory
	// Used for custom proxy dialing, and proxy TLS options
	ProxyTransport      http.RoundTripper
	KubeletClientConfig kubeletclient.KubeletClientConfig
	EventTTL            time.Duration

	// ServiceIPRange is used to build cluster IPs for discovery.
	ServiceIPRange       net.IPNet
	ServiceNodePortRange utilnet.PortRange

	LoopbackClientConfig *restclient.Config
}

// LegacyRESTStorage returns stateful information about particular instances of REST storage to
// master.go for wiring controllers.
// TODO remove this by running the controller as a poststarthook
type LegacyRESTStorage struct {
	NodeRegistry              node.Registry
	NamespaceRegistry         namespace.Registry
	ServiceRegistry           service.Registry
	EndpointRegistry          endpoint.Registry
	ServiceClusterIPAllocator rangeallocation.RangeRegistry
	ServiceNodePortAllocator  rangeallocation.RangeRegistry
}

func (c LegacyRESTStorageProvider) NewLegacyRESTStorage(restOptionsGetter genericapiserver.RESTOptionsGetter) (LegacyRESTStorage, genericapiserver.APIGroupInfo, error) {
	apiGroupInfo := genericapiserver.APIGroupInfo{
		GroupMeta:                    *registered.GroupOrDie(api.GroupName),
		VersionedResourcesStorageMap: map[string]map[string]rest.Storage{},
		Scheme:                      api.Scheme,
		ParameterCodec:              api.ParameterCodec,
		NegotiatedSerializer:        api.Codecs,
		SubresourceGroupVersionKind: map[string]unversioned.GroupVersionKind{},
	}
	if autoscalingGroupVersion := (unversioned.GroupVersion{Group: "autoscaling", Version: "v1"}); registered.IsEnabledVersion(autoscalingGroupVersion) {
		apiGroupInfo.SubresourceGroupVersionKind["replicationcontrollers/scale"] = autoscalingGroupVersion.WithKind("Scale")
	}

	var podDisruptionClient policyclient.PodDisruptionBudgetsGetter
	if policyGroupVersion := (unversioned.GroupVersion{Group: "policy", Version: "v1beta1"}); registered.IsEnabledVersion(policyGroupVersion) {
		apiGroupInfo.SubresourceGroupVersionKind["pods/eviction"] = policyGroupVersion.WithKind("Eviction")

		var err error
		podDisruptionClient, err = policyclient.NewForConfig(c.LoopbackClientConfig)
		if err != nil {
			return LegacyRESTStorage{}, genericapiserver.APIGroupInfo{}, err
		}
	}
	restStorage := LegacyRESTStorage{}

	podTemplateStorage := podtemplatestore.NewREST(restOptionsGetter(api.Resource("podTemplates")))

	eventStorage := eventstore.NewREST(restOptionsGetter(api.Resource("events")), uint64(c.EventTTL.Seconds()))
	limitRangeStorage := limitrangestore.NewREST(restOptionsGetter(api.Resource("limitRanges")))

	resourceQuotaStorage, resourceQuotaStatusStorage := resourcequotastore.NewREST(restOptionsGetter(api.Resource("resourceQuotas")))
	secretStorage := secretstore.NewREST(restOptionsGetter(api.Resource("secrets")))
	serviceAccountStorage := serviceaccountstore.NewREST(restOptionsGetter(api.Resource("serviceAccounts")))
	persistentVolumeStorage, persistentVolumeStatusStorage := pvstore.NewREST(restOptionsGetter(api.Resource("persistentVolumes")))
	persistentVolumeClaimStorage, persistentVolumeClaimStatusStorage := pvcstore.NewREST(restOptionsGetter(api.Resource("persistentVolumeClaims")))
	configMapStorage := configmapstore.NewREST(restOptionsGetter(api.Resource("configMaps")))

	namespaceStorage, namespaceStatusStorage, namespaceFinalizeStorage := namespacestore.NewREST(restOptionsGetter(api.Resource("namespaces")))
	restStorage.NamespaceRegistry = namespace.NewRegistry(namespaceStorage)

	endpointsStorage := endpointstore.NewREST(restOptionsGetter(api.Resource("endpoints")))
	restStorage.EndpointRegistry = endpoint.NewRegistry(endpointsStorage)

	nodeStorage, err := nodestore.NewStorage(restOptionsGetter(api.Resource("nodes")), c.KubeletClientConfig, c.ProxyTransport)
	if err != nil {
		return LegacyRESTStorage{}, genericapiserver.APIGroupInfo{}, err
	}
	restStorage.NodeRegistry = node.NewRegistry(nodeStorage.Node)

	podStorage := podstore.NewStorage(
		restOptionsGetter(api.Resource("pods")),
		nodeStorage.KubeletConnectionInfo,
		c.ProxyTransport,
		podDisruptionClient,
	)

	serviceRESTStorage, serviceStatusStorage := servicestore.NewREST(restOptionsGetter(api.Resource("services")))
	restStorage.ServiceRegistry = service.NewRegistry(serviceRESTStorage)

	var serviceClusterIPRegistry rangeallocation.RangeRegistry
	serviceClusterIPRange := c.ServiceIPRange
	if serviceClusterIPRange.IP == nil {
		return LegacyRESTStorage{}, genericapiserver.APIGroupInfo{}, fmt.Errorf("service clusterIPRange is missing")
	}

	serviceStorageConfig, err := c.StorageFactory.NewConfig(api.Resource("services"))
	if err != nil {
		return LegacyRESTStorage{}, genericapiserver.APIGroupInfo{}, err
	}

	ServiceClusterIPAllocator := ipallocator.NewAllocatorCIDRRange(&serviceClusterIPRange, func(max int, rangeSpec string) allocator.Interface {
		mem := allocator.NewAllocationMap(max, rangeSpec)
		// TODO etcdallocator package to return a storage interface via the storageFactory
		etcd := allocatorstore.NewEtcd(mem, "/ranges/serviceips", api.Resource("serviceipallocations"), serviceStorageConfig)
		serviceClusterIPRegistry = etcd
		return etcd
	})
	restStorage.ServiceClusterIPAllocator = serviceClusterIPRegistry

	var serviceNodePortRegistry rangeallocation.RangeRegistry
	ServiceNodePortAllocator := portallocator.NewPortAllocatorCustom(c.ServiceNodePortRange, func(max int, rangeSpec string) allocator.Interface {
		mem := allocator.NewAllocationMap(max, rangeSpec)
		// TODO etcdallocator package to return a storage interface via the storageFactory
		etcd := allocatorstore.NewEtcd(mem, "/ranges/servicenodeports", api.Resource("servicenodeportallocations"), serviceStorageConfig)
		serviceNodePortRegistry = etcd
		return etcd
	})
	restStorage.ServiceNodePortAllocator = serviceNodePortRegistry

	controllerStorage := controllerstore.NewStorage(restOptionsGetter(api.Resource("replicationControllers")))

	serviceRest := service.NewStorage(restStorage.ServiceRegistry, restStorage.EndpointRegistry, ServiceClusterIPAllocator, ServiceNodePortAllocator, c.ProxyTransport)

	restStorageMap := map[string]rest.Storage{
		"pods":             podStorage.Pod,
		"pods/attach":      podStorage.Attach,
		"pods/status":      podStorage.Status,
		"pods/log":         podStorage.Log,
		"pods/exec":        podStorage.Exec,
		"pods/portforward": podStorage.PortForward,
		"pods/proxy":       podStorage.Proxy,
		"pods/binding":     podStorage.Binding,
		"bindings":         podStorage.Binding,

		"podTemplates": podTemplateStorage,

		"replicationControllers":        controllerStorage.Controller,
		"replicationControllers/status": controllerStorage.Status,

		"services":        serviceRest.Service,
		"services/proxy":  serviceRest.Proxy,
		"services/status": serviceStatusStorage,

		"endpoints": endpointsStorage,

		"nodes":        nodeStorage.Node,
		"nodes/status": nodeStorage.Status,
		"nodes/proxy":  nodeStorage.Proxy,

		"events": eventStorage,

		"limitRanges":                   limitRangeStorage,
		"resourceQuotas":                resourceQuotaStorage,
		"resourceQuotas/status":         resourceQuotaStatusStorage,
		"namespaces":                    namespaceStorage,
		"namespaces/status":             namespaceStatusStorage,
		"namespaces/finalize":           namespaceFinalizeStorage,
		"secrets":                       secretStorage,
		"serviceAccounts":               serviceAccountStorage,
		"persistentVolumes":             persistentVolumeStorage,
		"persistentVolumes/status":      persistentVolumeStatusStorage,
		"persistentVolumeClaims":        persistentVolumeClaimStorage,
		"persistentVolumeClaims/status": persistentVolumeClaimStatusStorage,
		"configMaps":                    configMapStorage,

		"componentStatuses": componentstatus.NewStorage(componentStatusStorage{c.StorageFactory}.serversToValidate),
	}
	if registered.IsEnabledVersion(unversioned.GroupVersion{Group: "autoscaling", Version: "v1"}) {
		restStorageMap["replicationControllers/scale"] = controllerStorage.Scale
	}
	if registered.IsEnabledVersion(unversioned.GroupVersion{Group: "policy", Version: "v1beta1"}) {
		restStorageMap["pods/eviction"] = podStorage.Eviction
	}
	apiGroupInfo.VersionedResourcesStorageMap["v1"] = restStorageMap

	return restStorage, apiGroupInfo, nil
}

func (p LegacyRESTStorageProvider) GroupName() string {
	return api.GroupName
}

type componentStatusStorage struct {
	storageFactory genericapiserver.StorageFactory
}

func (s componentStatusStorage) serversToValidate() map[string]apiserver.Server {
	serversToValidate := map[string]apiserver.Server{
		"controller-manager": {Addr: "127.0.0.1", Port: ports.ControllerManagerPort, Path: "/healthz"},
		"scheduler":          {Addr: "127.0.0.1", Port: ports.SchedulerPort, Path: "/healthz"},
	}

	for ix, machine := range s.storageFactory.Backends() {
		etcdUrl, err := url.Parse(machine)
		if err != nil {
			glog.Errorf("Failed to parse etcd url for validation: %v", err)
			continue
		}
		var port int
		var addr string
		if strings.Contains(etcdUrl.Host, ":") {
			var portString string
			addr, portString, err = net.SplitHostPort(etcdUrl.Host)
			if err != nil {
				glog.Errorf("Failed to split host/port: %s (%v)", etcdUrl.Host, err)
				continue
			}
			port, _ = strconv.Atoi(portString)
		} else {
			addr = etcdUrl.Host
			port = 2379
		}
		// TODO: etcd health checking should be abstracted in the storage tier
		serversToValidate[fmt.Sprintf("etcd-%d", ix)] = apiserver.Server{
			Addr:        addr,
			EnableHTTPS: etcdUrl.Scheme == "https",
			Port:        port,
			Path:        "/health",
			Validate:    etcdutil.EtcdHealthCheck,
		}
	}
	return serversToValidate
}
