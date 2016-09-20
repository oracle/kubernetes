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

package e2e

import (
	"fmt"
	"time"

	"k8s.io/kubernetes/pkg/api"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/test/e2e/framework"

	. "github.com/onsi/ginkgo"
)

// returns maxRetries, sleepDuration
func readConfig() (int, time.Duration) {
	// Read in configuration settings, reasonable defaults.
	retry := framework.TestContext.Cadvisor.MaxRetries
	if framework.TestContext.Cadvisor.MaxRetries == 0 {
		retry = 6
		framework.Logf("Overriding default retry value of zero to %d", retry)
	}

	sleepDurationMS := framework.TestContext.Cadvisor.SleepDurationMS
	if sleepDurationMS == 0 {
		sleepDurationMS = 10000
		framework.Logf("Overriding default milliseconds value of zero to %d", sleepDurationMS)
	}

	return retry, time.Duration(sleepDurationMS) * time.Millisecond
}

var _ = framework.KubeDescribe("Cadvisor", func() {

	f := framework.NewDefaultFramework("cadvisor")

	It("should be healthy on every node.", func() {
		CheckCadvisorHealthOnAllNodes(f.Client, 5*time.Minute)
	})
})

func CheckCadvisorHealthOnAllNodes(c *client.Client, timeout time.Duration) {
	// It should be OK to list unschedulable Nodes here.
	By("getting list of nodes")
	nodeList, err := c.Nodes().List(api.ListOptions{})
	framework.ExpectNoError(err)
	var errors []error
<<<<<<< 9a92764704417b0ade9b569f59fc56e1632618e8
	maxRetries, sleepDuration := readConfig()
=======
	retries, sleepDuration := readConfig()
>>>>>>> e2e test docs
	for {
		errors = []error{}
		for _, node := range nodeList.Items {
			// cadvisor is not accessible directly unless its port (4194 by default) is exposed.
			// Here, we access '/stats/' REST endpoint on the kubelet which polls cadvisor internally.
			statsResource := fmt.Sprintf("api/v1/proxy/nodes/%s/stats/", node.Name)
			By(fmt.Sprintf("Querying stats from node %s using url %s", node.Name, statsResource))
			_, err = c.Get().AbsPath(statsResource).Timeout(timeout).Do().Raw()
			if err != nil {
				errors = append(errors, err)
			}
		}
		if len(errors) == 0 {
			return
		}
		if maxRetries--; maxRetries <= 0 {
			break
		}
		framework.Logf("failed to retrieve kubelet stats -\n %v", errors)
		time.Sleep(sleepDuration)
	}
	framework.Failf("Failed after retrying %d times for cadvisor to be healthy on all nodes. Errors:\n%v", maxRetries, errors)
}
