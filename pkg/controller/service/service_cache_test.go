package service

import (
	"fmt"
	"testing"
)

//All the testcases for ServiceCache uses a single cache, these below test cases should be run in order,
//as tc1 (addCache would add elements to the cache)
//and tc2 (delCache would remove element from the cache without it adding automatically)
//Please keep this in mind while adding new test cases.
func TestServiceCache(t *testing.T) {

	//ServiceCache a common service cache for all the test cases
	sc := &serviceCache{serviceMap: make(map[string]*cachedService)}

	testCases := []struct {
		testName     string
		setCacheFn   func()
		checkCacheFn func() error
	}{
		{
			testName: "Add",
			setCacheFn: func() {
				cS := sc.getOrCreate("addTest")
				cS.state = defaultExternalService()
			},
			checkCacheFn: func() error {
				//There must be exactly one element
				if len(sc.serviceMap) != 1 {
					return fmt.Errorf("Expected=1 Obtained=%d", len(sc.serviceMap))
				}
				return nil
			},
		},
		{
			testName: "Del",
			setCacheFn: func() {
				sc.delete("addTest")

			},
			checkCacheFn: func() error {
				//Now it should have no element
				if len(sc.serviceMap) != 0 {
					return fmt.Errorf("Expected=0 Obtained=%d", len(sc.serviceMap))
				}
				return nil
			},
		},
		{
			testName: "Set and Get",
			setCacheFn: func() {
				sc.set("addTest", &cachedService{state: defaultExternalService()})
			},
			checkCacheFn: func() error {
				//Now it should have one element
				Cs, bool := sc.get("addTest")
				if !bool {
					return fmt.Errorf("is Available Expected=true Obtained=%v", bool)
				}
				if Cs == nil {
					return fmt.Errorf("CachedService expected:non-nil Obtained=nil")
				}
				return nil
			},
		},
		{
			testName: "ListKeys",
			setCacheFn: func() {
				//Add one more entry here
				sc.set("addTest1", &cachedService{state: defaultExternalService()})
			},
			checkCacheFn: func() error {
				//It should have two elements
				keys := sc.ListKeys()
				if len(keys) != 2 {
					return fmt.Errorf("Elementes Expected=2 Obtained=%v", len(keys))
				}
				return nil
			},
		},
		{
			testName:   "GetbyKeys",
			setCacheFn: nil, //Nothing to set
			checkCacheFn: func() error {
				//It should have two elements
				svc, isKey, err := sc.GetByKey("addTest")
				if svc == nil || isKey == false || err != nil {
					return fmt.Errorf("Expected(non-nil, true, nil) Obtained(%v,%v,%v)", svc, isKey, err)
				}
				return nil
			},
		},
		{
			testName:   "allServices",
			setCacheFn: nil, //Nothing to set
			checkCacheFn: func() error {
				//It should return two elements
				svcArray := sc.allServices()
				if len(svcArray) != 2 {
					return fmt.Errorf("Expected(2) Obtained(%v)", len(svcArray))
				}
				return nil
			},
		},
	}

	for _, tc := range testCases {
		if tc.setCacheFn != nil {
			tc.setCacheFn()
		}
		if err := tc.checkCacheFn(); err != nil {
			t.Errorf("%v returned %v", tc.testName, err)
		}
	}
}
