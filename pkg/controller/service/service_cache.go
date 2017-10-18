package service

import (
	"sync"
	"time"

	"k8s.io/api/core/v1"
)

type cachedService struct {
	// The cached state of the service
	state *v1.Service
	// Controls error back-off
	lastRetryDelay time.Duration
}

type serviceCache struct {
	mu         sync.Mutex // protects serviceMap
	serviceMap map[string]*cachedService
}

// ListKeys implements the interface required by DeltaFIFO to list the keys we
// already know about.
func (s *serviceCache) ListKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.serviceMap))
	for k := range s.serviceMap {
		keys = append(keys, k)
	}
	return keys
}

// GetByKey returns the value stored in the serviceMap under the given key
func (s *serviceCache) GetByKey(key string) (interface{}, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.serviceMap[key]; ok {
		return v, true, nil
	}
	return nil, false, nil
}

// ListKeys implements the interface required by DeltaFIFO to list the keys we
// already know about.
func (s *serviceCache) allServices() []*v1.Service {
	s.mu.Lock()
	defer s.mu.Unlock()
	services := make([]*v1.Service, 0, len(s.serviceMap))
	for _, v := range s.serviceMap {
		services = append(services, v.state)
	}
	return services
}

func (s *serviceCache) get(serviceName string) (*cachedService, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, ok := s.serviceMap[serviceName]
	return service, ok
}

func (s *serviceCache) getOrCreate(serviceName string) *cachedService {
	s.mu.Lock()
	defer s.mu.Unlock()
	service, ok := s.serviceMap[serviceName]
	if !ok {
		service = &cachedService{}
		s.serviceMap[serviceName] = service
	}
	return service
}

func (s *serviceCache) set(serviceName string, service *cachedService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serviceMap[serviceName] = service
}

func (s *serviceCache) delete(serviceName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.serviceMap, serviceName)
}

// Computes the next retry, using exponential backoff
// mutex must be held.
func (s *cachedService) nextRetryDelay() time.Duration {
	s.lastRetryDelay = s.lastRetryDelay * 2
	if s.lastRetryDelay < minRetryDelay {
		s.lastRetryDelay = minRetryDelay
	}
	if s.lastRetryDelay > maxRetryDelay {
		s.lastRetryDelay = maxRetryDelay
	}
	return s.lastRetryDelay
}

// Resets the retry exponential backoff.  mutex must be held.
func (s *cachedService) resetRetryDelay() {
	s.lastRetryDelay = time.Duration(0)
}
