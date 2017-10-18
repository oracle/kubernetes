package service

import (
	"fmt"
	"testing"

	"k8s.io/api/core/v1"
)

func TestGetNodeConditionPredicate(t *testing.T) {
	tests := []struct {
		node         v1.Node
		expectAccept bool
		name         string
	}{
		{
			node:         v1.Node{},
			expectAccept: false,
			name:         "empty",
		},
		{
			node: v1.Node{
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{Type: v1.NodeReady, Status: v1.ConditionTrue},
					},
				},
			},
			expectAccept: true,
			name:         "basic",
		},
		{
			node: v1.Node{
				Spec: v1.NodeSpec{Unschedulable: true},
				Status: v1.NodeStatus{
					Conditions: []v1.NodeCondition{
						{Type: v1.NodeReady, Status: v1.ConditionTrue},
					},
				},
			},
			expectAccept: false,
			name:         "unschedulable",
		},
	}
	pred := getNodeConditionPredicate()
	for _, test := range tests {
		accept := pred(&test.node)
		if accept != test.expectAccept {
			t.Errorf("Test failed for %s, expected %v, saw %v", test.name, test.expectAccept, accept)
		}
	}
}

//Test a utility functions as its not easy to unit test nodeSyncLoop directly
func TestNodeSlicesEqualForLB(t *testing.T) {
	numNodes := 10
	nArray := make([]*v1.Node, 10)

	for i := 0; i < numNodes; i++ {
		nArray[i] = &v1.Node{}
		nArray[i].Name = fmt.Sprintf("node1")
	}
	if !nodeSlicesEqualForLB(nArray, nArray) {
		t.Errorf("nodeSlicesEqualForLB() Expected=true Obtained=false")
	}
}
