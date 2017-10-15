/*
Copyright 2015 The Kubernetes Authors.

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

package service

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"reflect"

	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	coreinformers "k8s.io/client-go/informers/core/v1"
	clientset "k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	corelisters "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/workqueue"
	v1helper "k8s.io/kubernetes/pkg/api/v1/helper"
	"k8s.io/kubernetes/pkg/cloudprovider"
	"k8s.io/kubernetes/pkg/controller"
	"k8s.io/kubernetes/pkg/util/metrics"
)

const (
	// Interval of synchronizing service status from apiserver
	serviceSyncPeriod = 30 * time.Second
	// Interval of synchronizing node status from apiserver
	nodeSyncPeriod = 100 * time.Second

	// How long to wait before retrying the processing of a service change.
	// If this changes, the sleep in hack/jenkins/e2e.sh before downing a cluster
	// should be changed appropriately.
	minRetryDelay = 5 * time.Second
	maxRetryDelay = 300 * time.Second

	clientRetryCount    = 5
	clientRetryInterval = 5 * time.Second

	finalizerName = v1.FinalizerServiceController

	// LabelNodeRoleMaster specifies that a node is a master
	// It's copied over to kubeadm until it's merged in core: https://github.com/kubernetes/kubernetes/pull/39112
	LabelNodeRoleMaster = "node-role.kubernetes.io/master"

	// LabelNodeRoleExcludeBalancer specifies that the node should be
	// exclude from load balancers created by a cloud provider.
	LabelNodeRoleExcludeBalancer = "alpha.node.role.kubernetes.io/exclude-balancer"
)

// ServiceController keeps the cloud provider service resources
// like load balancers in sync with the registry.
type ServiceController struct {
	cloud               cloudprovider.Interface
	knownHosts          []*v1.Node
	kubeClient          clientset.Interface
	clusterName         string
	balancer            cloudprovider.LoadBalancer
	serviceLister       corelisters.ServiceLister
	serviceListerSynced cache.InformerSynced
	eventBroadcaster    record.EventBroadcaster
	eventRecorder       record.EventRecorder
	nodeLister          corelisters.NodeLister
	nodeListerSynced    cache.InformerSynced
	// services that need to be synced
	workingQueue workqueue.RateLimitingInterface
	// for testing
	patchService func(old *v1.Service, updated *v1.Service) error
}

// New returns a new service controller to keep cloud provider service resources
// (like load balancers) in sync with the registry.
func New(
	cloud cloudprovider.Interface,
	kubeClient clientset.Interface,
	serviceInformer coreinformers.ServiceInformer,
	nodeInformer coreinformers.NodeInformer,
	clusterName string,
) (*ServiceController, error) {
	broadcaster := record.NewBroadcaster()
	broadcaster.StartLogging(glog.Infof)
	broadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.Core().RESTClient()).Events("")})
	recorder := broadcaster.NewRecorder(scheme.Scheme, v1.EventSource{Component: "service-controller"})
	broadcaster.StartLogging(glog.Infof)

	if kubeClient != nil && kubeClient.Core().RESTClient().GetRateLimiter() != nil {
		metrics.RegisterMetricAndTrackRateLimiterUsage("service_controller", kubeClient.Core().RESTClient().GetRateLimiter())
	}

	s := &ServiceController{
		cloud:            cloud,
		knownHosts:       []*v1.Node{},
		kubeClient:       kubeClient,
		clusterName:      clusterName,
		eventBroadcaster: broadcaster,
		eventRecorder:    recorder,
		nodeLister:       nodeInformer.Lister(),
		nodeListerSynced: nodeInformer.Informer().HasSynced,
		workingQueue:     workqueue.NewNamedRateLimitingQueue(workqueue.NewItemExponentialFailureRateLimiter(minRetryDelay, maxRetryDelay), "service"),
	}

	s.patchService = s.patchServiceImpl

	serviceInformer.Informer().AddEventHandlerWithResyncPeriod(
		cache.ResourceEventHandlerFuncs{
			AddFunc: s.enqueueService,
			UpdateFunc: func(old, cur interface{}) {
				oldSvc, ok1 := old.(*v1.Service)
				curSvc, ok2 := cur.(*v1.Service)
				if ok1 && ok2 && s.needsUpdate(oldSvc, curSvc) {
					s.enqueueService(cur)
				}
			},
			DeleteFunc: s.enqueueService,
		},
		serviceSyncPeriod,
	)
	s.serviceLister = serviceInformer.Lister()
	s.serviceListerSynced = serviceInformer.Informer().HasSynced

	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

// obj could be an *v1.Service, or a DeletionFinalStateUnknown marker item.
func (s *ServiceController) enqueueService(obj interface{}) {
	key, err := controller.KeyFunc(obj)
	if err != nil {
		glog.Errorf("Couldn't get key for object %#v: %v", obj, err)
		return
	}
	s.workingQueue.Add(key)
}

// Run starts a background goroutine that watches for changes to services that
// have (or had) LoadBalancers=true and ensures that they have
// load balancers created and deleted appropriately.
// serviceSyncPeriod controls how often we check the cluster's services to
// ensure that the correct load balancers exist.
// nodeSyncPeriod controls how often we check the cluster's nodes to determine
// if load balancers need to be updated to point to a new set.
//
// It's an error to call Run() more than once for a given ServiceController
// object.
func (s *ServiceController) Run(stopCh <-chan struct{}, workers int) {
	defer runtime.HandleCrash()
	defer s.workingQueue.ShutDown()

	glog.Info("Starting service controller")
	defer glog.Info("Shutting down service controller")

	if !controller.WaitForCacheSync("service", stopCh, s.serviceListerSynced, s.nodeListerSynced) {
	}

	for i := 0; i < workers; i++ {
		go wait.Until(s.worker, time.Second, stopCh)
	}

	go wait.Until(s.nodeSyncLoop, nodeSyncPeriod, stopCh)

	<-stopCh
}

// worker runs a worker thread that just dequeues items, processes them, and marks them done.
// It enforces that the syncHandler is never invoked concurrently with the same key.
func (s *ServiceController) worker() {
	for s.processNextItem() {
	}
}

func (s *ServiceController) processNextItem() bool {
	key, quit := s.workingQueue.Get()
	if quit {
		return false
	}

	defer s.workingQueue.Done(key)

	err := s.syncService(key.(string))
	s.handleErr(err, key.(string))
	return true
}

func (s *ServiceController) handleErr(err error, key string) {
	if err == nil {
		s.workingQueue.Forget(key)
		return
	}

	runtime.HandleError(fmt.Errorf("Failed to process service %q (will retry): %v", key, err))
	s.workingQueue.AddRateLimited(key)
}

func (s *ServiceController) init() error {
	if s.cloud == nil {
		return fmt.Errorf("WARNING: no cloud provider provided, services of type LoadBalancer will fail")
	}

	balancer, ok := s.cloud.LoadBalancer()
	if !ok {
		return fmt.Errorf("the cloud provider does not support external load balancers")
	}
	s.balancer = balancer

	return nil
}

// Returns an error if processing the service update failed, along with a time.Duration
// indicating whether processing should be retried; zero means no-retry; otherwise
// we should retry in that Duration.
func (s *ServiceController) processServiceUpdate(service *v1.Service, key string) error {
	err := s.createLoadBalancerIfNeeded(key, service)
	if err != nil {
		s.eventRecorder.Eventf(service, v1.EventTypeWarning, "CreatingLoadBalancerFailed", "Error creating load balancer (will retry): %v", err)
		return err
	}

	return nil
}

// Returns whatever error occurred along with a boolean indicator of whether it should be retried.
func (s *ServiceController) createLoadBalancerIfNeeded(key string, service *v1.Service) error {
	// Note: It is safe to just call EnsureLoadBalancer.  But, on some clouds that requires a delete & create,
	// which may involve service interruption.  Also, we would like user-friendly events.

	// Save the state so we can avoid a write if it doesn't change
	previousFinalizers := service.Finalizers
	previousState := v1helper.LoadBalancerStatusDeepCopy(&service.Status.LoadBalancer)
	var newState *v1.LoadBalancerStatus
	var newFinalizers []string
	var err error

	if !wantsLoadBalancer(service) {
		_, exists, err := s.balancer.GetLoadBalancer(s.clusterName, service)
		if err != nil {
			return fmt.Errorf("error getting LB for service %s: %v", key, err)
		}
		if exists {
			glog.Infof("Deleting existing load balancer for service %s that no longer needs a load balancer.", key)
			s.eventRecorder.Event(service, v1.EventTypeNormal, "DeletingLoadBalancer", "Deleting load balancer")
			if err := s.balancer.EnsureLoadBalancerDeleted(s.clusterName, service); err != nil {
				return err
			}
			s.eventRecorder.Event(service, v1.EventTypeNormal, "DeletedLoadBalancer", "Deleted load balancer")
		}

		newState = &v1.LoadBalancerStatus{}
		newFinalizers = removeFinalizer(service)
	} else {
		glog.V(2).Infof("Ensuring LB for service %s", key)

		// TODO: We could do a dry-run here if wanted to avoid the spurious cloud-calls & events when we restart

		s.eventRecorder.Event(service, v1.EventTypeNormal, "EnsuringLoadBalancer", "Ensuring load balancer")
		newState, err = s.ensureLoadBalancer(service)
		if err != nil {
			// TOOD: add failure event like deletes?
			return fmt.Errorf("failed to ensure load balancer for service %s: %v", key, err)
		}
		s.eventRecorder.Event(service, v1.EventTypeNormal, "EnsuredLoadBalancer", "Ensured load balancer")

		newFinalizers = addFinalizer(service)
	}

	// If there are any changes to the status or finalizers then patch the service.
	if !v1helper.LoadBalancerStatusEqual(previousState, newState) || len(previousFinalizers) != len(newFinalizers) {
		// Make a copy so we don't mutate the shared informer cache
		updated := service.DeepCopy()
		updated.Status.LoadBalancer = *newState
		updated.Finalizers = newFinalizers

		err := s.patchService(service, updated)
		if err != nil {
			return fmt.Errorf("failed to persist updated status to apiserver: %v", err)
		}
		glog.V(2).Infof("Successfully updated load balancer service %q", key)
	} else {
		glog.V(4).Infof("Not persisting unchanged LoadBalancerStatus for service %s to registry.", key)
	}

	return nil
}

func (s *ServiceController) patchServiceImpl(old *v1.Service, updated *v1.Service) error {
	serviceName := updated.Name

	oldData, err := json.Marshal(old)
	if err != nil {
		return fmt.Errorf("failed to marshal old service %#v for serice %q: %v", old, serviceName, err)
	}

	// Reset spec to make sure only patch for Status or ObjectMeta is generated.
	// Note that we don't reset ObjectMeta here, because:
	// 1. This aligns with Services().UpdateStatus().
	// 2. Allows the finalizers to be updated.
	updated.Spec = old.Spec
	newData, err := json.Marshal(updated)
	if err != nil {
		return fmt.Errorf("failed to marshal new service %#v for service %q: %v", updated, serviceName, err)
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, v1.Service{})
	if err != nil {
		return fmt.Errorf("failed to create patch for service %q: %v", serviceName, err)
	}

	_, err = s.kubeClient.CoreV1().
		Services(updated.Namespace).
		Patch(serviceName, types.StrategicMergePatchType, patchBytes, "status")
	if err != nil {
		return fmt.Errorf("failed to patch status %q for service %q: %v", patchBytes, serviceName, err)
	}
	return nil
}

func (s *ServiceController) ensureLoadBalancer(service *v1.Service) (*v1.LoadBalancerStatus, error) {
	nodes, err := s.nodeLister.ListWithPredicate(getNodeConditionPredicate())
	if err != nil {
		return nil, err
	}

	// - Only one protocol supported per service
	// - Not all cloud providers support all protocols and the next step is expected to return
	//   an error for unsupported protocols
	return s.balancer.EnsureLoadBalancer(s.clusterName, service, nodes)
}

func (s *ServiceController) needsDeletion(service *v1.Service) bool {
	return service.DeletionTimestamp != nil &&
		!service.DeletionTimestamp.IsZero() &&
		hasFinalizer(service)
}

func (s *ServiceController) needsUpdate(oldService *v1.Service, newService *v1.Service) bool {
	if s.needsDeletion(newService) {
		return true
	}

	if !wantsLoadBalancer(oldService) && !wantsLoadBalancer(newService) {
		return false
	}
	if wantsLoadBalancer(oldService) != wantsLoadBalancer(newService) {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "Type", "%v -> %v",
			oldService.Spec.Type, newService.Spec.Type)
		return true
	}

	if wantsLoadBalancer(newService) && !reflect.DeepEqual(oldService.Spec.LoadBalancerSourceRanges, newService.Spec.LoadBalancerSourceRanges) {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "LoadBalancerSourceRanges", "%v -> %v",
			oldService.Spec.LoadBalancerSourceRanges, newService.Spec.LoadBalancerSourceRanges)
		return true
	}
	if !hasFinalizer(newService) {
		return true
	}
	if !portsEqualForLB(oldService, newService) || oldService.Spec.SessionAffinity != newService.Spec.SessionAffinity {
		return true
	}
	if !loadBalancerIPsAreEqual(oldService, newService) {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "LoadbalancerIP", "%v -> %v",
			oldService.Spec.LoadBalancerIP, newService.Spec.LoadBalancerIP)
		return true
	}
	if len(oldService.Spec.ExternalIPs) != len(newService.Spec.ExternalIPs) {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "ExternalIP", "Count: %v -> %v",
			len(oldService.Spec.ExternalIPs), len(newService.Spec.ExternalIPs))
		return true
	}
	for i := range oldService.Spec.ExternalIPs {
		if oldService.Spec.ExternalIPs[i] != newService.Spec.ExternalIPs[i] {
			s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "ExternalIP", "Added: %v",
				newService.Spec.ExternalIPs[i])
			return true
		}
	}
	if !reflect.DeepEqual(oldService.Annotations, newService.Annotations) {
		return true
	}
	if oldService.UID != newService.UID {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "UID", "%v -> %v",
			oldService.UID, newService.UID)
		return true
	}
	if oldService.Spec.ExternalTrafficPolicy != newService.Spec.ExternalTrafficPolicy {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "ExternalTrafficPolicy", "%v -> %v",
			oldService.Spec.ExternalTrafficPolicy, newService.Spec.ExternalTrafficPolicy)
		return true
	}
	if oldService.Spec.HealthCheckNodePort != newService.Spec.HealthCheckNodePort {
		s.eventRecorder.Eventf(newService, v1.EventTypeNormal, "HealthCheckNodePort", "%v -> %v",
			oldService.Spec.HealthCheckNodePort, newService.Spec.HealthCheckNodePort)
		return true
	}

	return false
}

func (s *ServiceController) loadBalancerName(service *v1.Service) string {
	return cloudprovider.GetLoadBalancerName(service)
}

func getPortsForLB(service *v1.Service) ([]*v1.ServicePort, error) {
	var protocol v1.Protocol

	ports := []*v1.ServicePort{}
	for i := range service.Spec.Ports {
		sp := &service.Spec.Ports[i]
		// The check on protocol was removed here.  The cloud provider itself is now responsible for all protocol validation
		ports = append(ports, sp)
		if protocol == "" {
			protocol = sp.Protocol
		} else if protocol != sp.Protocol && wantsLoadBalancer(service) {
			// TODO:  Convert error messages to use event recorder
			return nil, fmt.Errorf("mixed protocol external load balancers are not supported")
		}
	}
	return ports, nil
}

func portsEqualForLB(x, y *v1.Service) bool {
	xPorts, err := getPortsForLB(x)
	if err != nil {
		return false
	}
	yPorts, err := getPortsForLB(y)
	if err != nil {
		return false
	}
	return portSlicesEqualForLB(xPorts, yPorts)
}

func portSlicesEqualForLB(x, y []*v1.ServicePort) bool {
	if len(x) != len(y) {
		return false
	}

	for i := range x {
		if !portEqualForLB(x[i], y[i]) {
			return false
		}
	}
	return true
}

func portEqualForLB(x, y *v1.ServicePort) bool {
	// TODO: Should we check name?  (In theory, an LB could expose it)
	if x.Name != y.Name {
		return false
	}

	if x.Protocol != y.Protocol {
		return false
	}

	if x.Port != y.Port {
		return false
	}

	if x.NodePort != y.NodePort {
		return false
	}

	// We don't check TargetPort; that is not relevant for load balancing
	// TODO: Should we blank it out?  Or just check it anyway?

	return true
}

func nodeNames(nodes []*v1.Node) []string {
	ret := make([]string, len(nodes))
	for i, node := range nodes {
		ret[i] = node.Name
	}
	return ret
}

func nodeSlicesEqualForLB(x, y []*v1.Node) bool {
	if len(x) != len(y) {
		return false
	}
	return stringSlicesEqual(nodeNames(x), nodeNames(y))
}

func stringSlicesEqual(x, y []string) bool {
	if len(x) != len(y) {
		return false
	}
	if !sort.StringsAreSorted(x) {
		sort.Strings(x)
	}
	if !sort.StringsAreSorted(y) {
		sort.Strings(y)
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

func includeNodeFromNodeList(node *v1.Node) bool {
	return !node.Spec.Unschedulable
}

func getNodeConditionPredicate() corelisters.NodeConditionPredicate {
	return func(node *v1.Node) bool {
		// We add the master to the node list, but its unschedulable.  So we use this to filter
		// the master.
		if node.Spec.Unschedulable {
			return false
		}

		// As of 1.6, we will taint the master, but not necessarily mark it unschedulable.
		// Recognize nodes labeled as master, and filter them also, as we were doing previously.
		if _, hasMasterRoleLabel := node.Labels[LabelNodeRoleMaster]; hasMasterRoleLabel {
			return false
		}

		if _, hasExcludeBalancerLabel := node.Labels[LabelNodeRoleExcludeBalancer]; hasExcludeBalancerLabel {
			return false
		}

		// If we have no info, don't accept
		if len(node.Status.Conditions) == 0 {
			return false
		}
		for _, cond := range node.Status.Conditions {
			// We consider the node for load balancing only when its NodeReady condition status
			// is ConditionTrue
			if cond.Type == v1.NodeReady && cond.Status != v1.ConditionTrue {
				glog.V(4).Infof("Ignoring node %v with %v condition status %v", node.Name, cond.Type, cond.Status)
				return false
			}
		}
		return true
	}
}

// nodeSyncLoop handles updating the hosts pointed to by all load
// balancers whenever the set of nodes in the cluster changes.
func (s *ServiceController) nodeSyncLoop() {
	newHosts, err := s.nodeLister.ListWithPredicate(getNodeConditionPredicate())
	if err != nil {
		glog.Errorf("Failed to retrieve current set of nodes from node lister: %v", err)
		return
	}
	if nodeSlicesEqualForLB(newHosts, s.knownHosts) {
		// The set of nodes in the cluster hasn't changed, but we can retry
		// updating any services that we failed to update last time around.
		// TODO(horwitz): the return of this isn't useful... change the api??
		s.updateLoadBalancerHosts(s.servicesToUpdate, newHosts)
		return
	}

	glog.Infof("Detected change in list of current cluster nodes. New node set: %v", nodeNames(newHosts))

	// Try updating all services, and save the ones that fail to try again next
	// round.
	services, err := s.serviceLister.List(labels.Everything())
	if err != nil {
		glog.Errorf("Unable to list services to sync with updated nodes: %v", err)
		return
	}

	numServices := len(services)
	services = s.updateLoadBalancerHosts(services, newHosts)
	glog.Infof("Successfully updated %d out of %d load balancers to direct traffic to the updated set of nodes",
		numServices-len(services), numServices)

	s.knownHosts = newHosts
}

// updateLoadBalancerHosts updates all existing load balancers so that
// they will match the list of hosts provided.
// Returns the list of services that couldn't be updated.
func (s *ServiceController) updateLoadBalancerHosts(services []*v1.Service, hosts []*v1.Node) (servicesToRetry []*v1.Service) {
	for _, service := range services {
		func() {
			if service == nil {
				return
			}
			if err := s.lockedUpdateLoadBalancerHosts(service, hosts); err != nil {
				glog.Errorf("External error while updating load balancer: %v.", err)
				servicesToRetry = append(servicesToRetry, service)
			}
		}()
	}
	return servicesToRetry
}

// Updates the load balancer of a service, assuming we hold the mutex
// associated with the service.
func (s *ServiceController) lockedUpdateLoadBalancerHosts(service *v1.Service, hosts []*v1.Node) error {
	if !wantsLoadBalancer(service) {
		return nil
	}

	// This operation doesn't normally take very long (and happens pretty often), so we only record the final event
	err := s.balancer.UpdateLoadBalancer(s.clusterName, service, hosts)
	if err == nil {
		s.eventRecorder.Event(service, v1.EventTypeNormal, "UpdatedLoadBalancer", "Updated load balancer with new hosts")
		return nil
	}

	// It's only an actual error if the load balancer still exists.
	if _, exists, err := s.balancer.GetLoadBalancer(s.clusterName, service); err != nil {
		glog.Errorf("External error while checking if load balancer %q exists: name, %v", cloudprovider.GetLoadBalancerName(service), err)
	} else if !exists {
		return nil
	}

	s.eventRecorder.Eventf(service, v1.EventTypeWarning, "LoadBalancerUpdateFailed", "Error updating load balancer with new hosts %v: %v", nodeNames(hosts), err)
	return err
}

func wantsLoadBalancer(service *v1.Service) bool {
	return service.Spec.Type == v1.ServiceTypeLoadBalancer
}

func loadBalancerIPsAreEqual(oldService, newService *v1.Service) bool {
	return oldService.Spec.LoadBalancerIP == newService.Spec.LoadBalancerIP
}

// syncService will sync the Service with the given key if it has had its expectations fulfilled,
// meaning it did not expect to see any more of its pods created or deleted. This function is not meant to be
// invoked concurrently with the same key.
func (s *ServiceController) syncService(key string) error {
	startTime := time.Now()
	defer func() {
		glog.V(4).Infof("Finished syncing service %q (%v)", key, time.Now().Sub(startTime))
	}()

	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		return err
	}

	// NOTE: So with this refactor it means that during the initial deploy that if a service is deleted
	// before the finalizer is added that it would have leaked resources; however, this is already similar
	// to the case that if a delete happens and the controller is shutdown to uprade you'd have leaked resources.
	// I vote we put a note in the release notes to ensure that no load balancer services are deleted when upgrading the
	// CCM or controller manager.

	// service holds the latest service info from apiserver
	service, err := s.serviceLister.Services(namespace).Get(name)
	if errors.IsNotFound(err) {
		glog.Infof("Service %q has been deleted", key)
		return nil
	}

	if err != nil {
		glog.Infof("Unable to retrieve service %v from store: %v", key, err)
		return err
	}

	if s.needsDeletion(service) {
		return s.processServiceDeletion(service, key)
	}

	return s.processServiceUpdate(service, key)
}

// Returns an error if processing the service deletion failed, along with a time.Duration
// indicating whether processing should be retried; zero means no-retry; otherwise
// we should retry after that Duration.
func (s *ServiceController) processServiceDeletion(service *v1.Service, key string) error {
	return s.processLoadBalancerDelete(service, key)
}

func (s *ServiceController) processLoadBalancerDelete(service *v1.Service, key string) error {
	s.eventRecorder.Event(service, v1.EventTypeNormal, "DeletingLoadBalancer", "Deleting load balancer")
	err := s.balancer.EnsureLoadBalancerDeleted(s.clusterName, service)
	if err != nil {
		s.eventRecorder.Eventf(service, v1.EventTypeWarning, "DeletingLoadBalancerFailed", "Error deleting load balancer (will retry): %v", err)
		return err
	}
	s.eventRecorder.Event(service, v1.EventTypeNormal, "DeletedLoadBalancer", "Deleted load balancer")

	// Now that we've successfully cleaned up the load balancer resources we can remove the finalizer
	// and allow the service to be deleted.
	updatedFinalizers := removeFinalizer(service)
	if len(updatedFinalizers) != len(service.Finalizers) {
		updated := service.DeepCopy()
		updated.Finalizers = updatedFinalizers
		err = s.patchService(service, updated)
		if err != nil {
			return err
		}
	}

	return nil
}

// TODO(horwitz): test me
func removeFinalizer(service *v1.Service) []string {
	finalizers := sets.NewString(service.GetFinalizers()...)
	finalizers.Delete(finalizerName)
	return finalizers.List()
}

// TODO(horwitz): test me
func addFinalizer(service *v1.Service) []string {
	finalizers := sets.NewString(service.GetFinalizers()...)
	finalizers.Insert(finalizerName)
	return finalizers.List()
}

func hasFinalizer(service *v1.Service) bool {
	finalizers := sets.NewString(service.GetFinalizers()...)
	return finalizers.Has(finalizerName)
}
