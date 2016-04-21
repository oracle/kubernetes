/*
Copyright 2015 The Kubernetes Authors All rights reserved.

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

package rkt

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	appcschema "github.com/appc/spec/schema"
	appctypes "github.com/appc/spec/schema/types"
	rktapi "github.com/coreos/rkt/api/v1alpha"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/api/resource"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	containertesting "k8s.io/kubernetes/pkg/kubelet/container/testing"
	kubetesting "k8s.io/kubernetes/pkg/kubelet/container/testing"
	"k8s.io/kubernetes/pkg/kubelet/lifecycle"
	"k8s.io/kubernetes/pkg/kubelet/rkt/mock_os"
	"k8s.io/kubernetes/pkg/types"
	"k8s.io/kubernetes/pkg/util/errors"
	utiltesting "k8s.io/kubernetes/pkg/util/testing"
)

func mustMarshalPodManifest(man *appcschema.PodManifest) []byte {
	manblob, err := json.Marshal(man)
	if err != nil {
		panic(err)
	}
	return manblob
}

func mustMarshalImageManifest(man *appcschema.ImageManifest) []byte {
	manblob, err := json.Marshal(man)
	if err != nil {
		panic(err)
	}
	return manblob
}

func mustRktHash(hash string) *appctypes.Hash {
	h, err := appctypes.NewHash(hash)
	if err != nil {
		panic(err)
	}
	return h
}

func makeRktPod(rktPodState rktapi.PodState,
	rktPodID, podUID, podName, podNamespace,
	podIP string, podCreatedAt, podStartedAt int64,
	podRestartCount string, appNames, imgIDs, imgNames,
	containerHashes []string, appStates []rktapi.AppState,
	exitcodes []int32) *rktapi.Pod {

	podManifest := &appcschema.PodManifest{
		ACKind:    appcschema.PodManifestKind,
		ACVersion: appcschema.AppContainerVersion,
		Annotations: appctypes.Annotations{
			appctypes.Annotation{
				Name:  *appctypes.MustACIdentifier(k8sRktKubeletAnno),
				Value: k8sRktKubeletAnnoValue,
			},
			appctypes.Annotation{
				Name:  *appctypes.MustACIdentifier(k8sRktUIDAnno),
				Value: podUID,
			},
			appctypes.Annotation{
				Name:  *appctypes.MustACIdentifier(k8sRktNameAnno),
				Value: podName,
			},
			appctypes.Annotation{
				Name:  *appctypes.MustACIdentifier(k8sRktNamespaceAnno),
				Value: podNamespace,
			},
			appctypes.Annotation{
				Name:  *appctypes.MustACIdentifier(k8sRktRestartCountAnno),
				Value: podRestartCount,
			},
		},
	}

	appNum := len(appNames)
	if appNum != len(imgNames) ||
		appNum != len(imgIDs) ||
		appNum != len(containerHashes) ||
		appNum != len(appStates) {
		panic("inconsistent app number")
	}

	apps := make([]*rktapi.App, appNum)
	for i := range appNames {
		apps[i] = &rktapi.App{
			Name:  appNames[i],
			State: appStates[i],
			Image: &rktapi.Image{
				Id:      imgIDs[i],
				Name:    imgNames[i],
				Version: "latest",
				Manifest: mustMarshalImageManifest(
					&appcschema.ImageManifest{
						ACKind:    appcschema.ImageManifestKind,
						ACVersion: appcschema.AppContainerVersion,
						Name:      *appctypes.MustACIdentifier(imgNames[i]),
						Annotations: appctypes.Annotations{
							appctypes.Annotation{
								Name:  *appctypes.MustACIdentifier(k8sRktContainerHashAnno),
								Value: containerHashes[i],
							},
						},
					},
				),
			},
			ExitCode: exitcodes[i],
		}
		podManifest.Apps = append(podManifest.Apps, appcschema.RuntimeApp{
			Name:  *appctypes.MustACName(appNames[i]),
			Image: appcschema.RuntimeImage{ID: *mustRktHash("sha512-foo")},
			Annotations: appctypes.Annotations{
				appctypes.Annotation{
					Name:  *appctypes.MustACIdentifier(k8sRktContainerHashAnno),
					Value: containerHashes[i],
				},
			},
		})
	}

	return &rktapi.Pod{
		Id:        rktPodID,
		State:     rktPodState,
		Networks:  []*rktapi.Network{{Name: defaultNetworkName, Ipv4: podIP}},
		Apps:      apps,
		Manifest:  mustMarshalPodManifest(podManifest),
		StartedAt: podStartedAt,
		CreatedAt: podCreatedAt,
	}
}

func TestCheckVersion(t *testing.T) {
	fr := newFakeRktInterface()
	fs := newFakeSystemd()
	r := &Runtime{apisvc: fr, systemd: fs}

	fr.info = rktapi.Info{
		RktVersion:  "1.2.3+git",
		AppcVersion: "1.2.4+git",
		ApiVersion:  "1.2.6-alpha",
	}
	fs.version = "100"
	tests := []struct {
		minimumRktBinVersion     string
		recommendedRktBinVersion string
		minimumAppcVersion       string
		minimumRktApiVersion     string
		minimumSystemdVersion    string
		err                      error
		calledGetInfo            bool
		calledSystemVersion      bool
	}{
		// Good versions.
		{
			"1.2.3",
			"1.2.3",
			"1.2.4",
			"1.2.5",
			"99",
			nil,
			true,
			true,
		},
		// Good versions.
		{
			"1.2.3+git",
			"1.2.3+git",
			"1.2.4+git",
			"1.2.6-alpha",
			"100",
			nil,
			true,
			true,
		},
		// Requires greater binary version.
		{
			"1.2.4",
			"1.2.4",
			"1.2.4",
			"1.2.6-alpha",
			"100",
			fmt.Errorf("rkt: binary version is too old(%v), requires at least %v", fr.info.RktVersion, "1.2.4"),
			true,
			true,
		},
		// Requires greater Appc version.
		{
			"1.2.3",
			"1.2.3",
			"1.2.5",
			"1.2.6-alpha",
			"100",
			fmt.Errorf("rkt: appc version is too old(%v), requires at least %v", fr.info.AppcVersion, "1.2.5"),
			true,
			true,
		},
		// Requires greater API version.
		{
			"1.2.3",
			"1.2.3",
			"1.2.4",
			"1.2.6",
			"100",
			fmt.Errorf("rkt: API version is too old(%v), requires at least %v", fr.info.ApiVersion, "1.2.6"),
			true,
			true,
		},
		// Requires greater API version.
		{
			"1.2.3",
			"1.2.3",
			"1.2.4",
			"1.2.7",
			"100",
			fmt.Errorf("rkt: API version is too old(%v), requires at least %v", fr.info.ApiVersion, "1.2.7"),
			true,
			true,
		},
		// Requires greater systemd version.
		{
			"1.2.3",
			"1.2.3",
			"1.2.4",
			"1.2.7",
			"101",
			fmt.Errorf("rkt: systemd version(%v) is too old, requires at least %v", fs.version, "101"),
			false,
			true,
		},
	}

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)
		err := r.checkVersion(tt.minimumRktBinVersion, tt.recommendedRktBinVersion, tt.minimumAppcVersion, tt.minimumRktApiVersion, tt.minimumSystemdVersion)
		assert.Equal(t, tt.err, err, testCaseHint)

		if tt.calledGetInfo {
			assert.Equal(t, fr.called, []string{"GetInfo"}, testCaseHint)
		}
		if tt.calledSystemVersion {
			assert.Equal(t, fs.called, []string{"Version"}, testCaseHint)
		}
		if err == nil {
			assert.Equal(t, fr.info.RktVersion, r.versions.binVersion.String(), testCaseHint)
			assert.Equal(t, fr.info.AppcVersion, r.versions.appcVersion.String(), testCaseHint)
			assert.Equal(t, fr.info.ApiVersion, r.versions.apiVersion.String(), testCaseHint)
		}
		fr.CleanCalls()
		fs.CleanCalls()
	}
}

func TestListImages(t *testing.T) {
	fr := newFakeRktInterface()
	fs := newFakeSystemd()
	r := &Runtime{apisvc: fr, systemd: fs}

	tests := []struct {
		images   []*rktapi.Image
		expected []kubecontainer.Image
	}{
		{nil, []kubecontainer.Image{}},
		{
			[]*rktapi.Image{
				{
					Id:      "sha512-a2fb8f390702",
					Name:    "quay.io/coreos/alpine-sh",
					Version: "latest",
				},
			},
			[]kubecontainer.Image{
				{
					ID:       "sha512-a2fb8f390702",
					RepoTags: []string{"quay.io/coreos/alpine-sh:latest"},
				},
			},
		},
		{
			[]*rktapi.Image{
				{
					Id:      "sha512-a2fb8f390702",
					Name:    "quay.io/coreos/alpine-sh",
					Version: "latest",
					Size:    400,
				},
				{
					Id:      "sha512-c6b597f42816",
					Name:    "coreos.com/rkt/stage1-coreos",
					Version: "0.10.0",
					Size:    400,
				},
			},
			[]kubecontainer.Image{
				{
					ID:       "sha512-a2fb8f390702",
					RepoTags: []string{"quay.io/coreos/alpine-sh:latest"},
					Size:     400,
				},
				{
					ID:       "sha512-c6b597f42816",
					RepoTags: []string{"coreos.com/rkt/stage1-coreos:0.10.0"},
					Size:     400,
				},
			},
		},
	}

	for i, tt := range tests {
		fr.images = tt.images

		images, err := r.ListImages()
		if err != nil {
			t.Errorf("%v", err)
		}
		assert.Equal(t, tt.expected, images)
		assert.Equal(t, fr.called, []string{"ListImages"}, fmt.Sprintf("test case %d: unexpected called list", i))

		fr.CleanCalls()
	}
}

func TestGetPods(t *testing.T) {
	fr := newFakeRktInterface()
	fs := newFakeSystemd()
	r := &Runtime{apisvc: fr, systemd: fs}

	ns := func(seconds int64) int64 {
		return seconds * 1e9
	}

	tests := []struct {
		pods   []*rktapi.Pod
		result []*kubecontainer.Pod
	}{
		// No pods.
		{},
		// One pod.
		{
			[]*rktapi.Pod{
				makeRktPod(rktapi.PodState_POD_STATE_RUNNING,
					"uuid-4002", "42", "guestbook", "default",
					"10.10.10.42", ns(10), ns(10), "7",
					[]string{"app-1", "app-2"},
					[]string{"img-id-1", "img-id-2"},
					[]string{"img-name-1", "img-name-2"},
					[]string{"1001", "1002"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_RUNNING, rktapi.AppState_APP_STATE_EXITED},
					[]int32{0, 0},
				),
			},
			[]*kubecontainer.Pod{
				{
					ID:        "42",
					Name:      "guestbook",
					Namespace: "default",
					Containers: []*kubecontainer.Container{
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4002:app-1"),
							Name:    "app-1",
							Image:   "img-name-1:latest",
							Hash:    1001,
							Created: 10,
							State:   "running",
						},
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4002:app-2"),
							Name:    "app-2",
							Image:   "img-name-2:latest",
							Hash:    1002,
							Created: 10,
							State:   "exited",
						},
					},
				},
			},
		},
		// Multiple pods.
		{
			[]*rktapi.Pod{
				makeRktPod(rktapi.PodState_POD_STATE_RUNNING,
					"uuid-4002", "42", "guestbook", "default",
					"10.10.10.42", ns(10), ns(20), "7",
					[]string{"app-1", "app-2"},
					[]string{"img-id-1", "img-id-2"},
					[]string{"img-name-1", "img-name-2"},
					[]string{"1001", "1002"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_RUNNING, rktapi.AppState_APP_STATE_EXITED},
					[]int32{0, 0},
				),
				makeRktPod(rktapi.PodState_POD_STATE_EXITED,
					"uuid-4003", "43", "guestbook", "default",
					"10.10.10.43", ns(30), ns(40), "7",
					[]string{"app-11", "app-22"},
					[]string{"img-id-11", "img-id-22"},
					[]string{"img-name-11", "img-name-22"},
					[]string{"10011", "10022"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_EXITED, rktapi.AppState_APP_STATE_EXITED},
					[]int32{0, 0},
				),
				makeRktPod(rktapi.PodState_POD_STATE_EXITED,
					"uuid-4004", "43", "guestbook", "default",
					"10.10.10.44", ns(50), ns(60), "8",
					[]string{"app-11", "app-22"},
					[]string{"img-id-11", "img-id-22"},
					[]string{"img-name-11", "img-name-22"},
					[]string{"10011", "10022"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_RUNNING, rktapi.AppState_APP_STATE_RUNNING},
					[]int32{0, 0},
				),
			},
			[]*kubecontainer.Pod{
				{
					ID:        "42",
					Name:      "guestbook",
					Namespace: "default",
					Containers: []*kubecontainer.Container{
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4002:app-1"),
							Name:    "app-1",
							Image:   "img-name-1:latest",
							Hash:    1001,
							Created: 10,
							State:   "running",
						},
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4002:app-2"),
							Name:    "app-2",
							Image:   "img-name-2:latest",
							Hash:    1002,
							Created: 10,
							State:   "exited",
						},
					},
				},
				{
					ID:        "43",
					Name:      "guestbook",
					Namespace: "default",
					Containers: []*kubecontainer.Container{
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4003:app-11"),
							Name:    "app-11",
							Image:   "img-name-11:latest",
							Hash:    10011,
							Created: 30,
							State:   "exited",
						},
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4003:app-22"),
							Name:    "app-22",
							Image:   "img-name-22:latest",
							Hash:    10022,
							Created: 30,
							State:   "exited",
						},
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4004:app-11"),
							Name:    "app-11",
							Image:   "img-name-11:latest",
							Hash:    10011,
							Created: 50,
							State:   "running",
						},
						{
							ID:      kubecontainer.BuildContainerID("rkt", "uuid-4004:app-22"),
							Name:    "app-22",
							Image:   "img-name-22:latest",
							Hash:    10022,
							Created: 50,
							State:   "running",
						},
					},
				},
			},
		},
	}

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)
		fr.pods = tt.pods

		pods, err := r.GetPods(true)
		if err != nil {
			t.Errorf("test case #%d: unexpected error: %v", i, err)
		}

		assert.Equal(t, tt.result, pods, testCaseHint)
		assert.Equal(t, []string{"ListPods"}, fr.called, fmt.Sprintf("test case %d: unexpected called list", i))

		fr.CleanCalls()
	}
}

func TestGetPodsFilters(t *testing.T) {
	fr := newFakeRktInterface()
	fs := newFakeSystemd()
	r := &Runtime{apisvc: fr, systemd: fs}

	for _, test := range []struct {
		All             bool
		ExpectedFilters []*rktapi.PodFilter
	}{
		{
			true,
			[]*rktapi.PodFilter{
				{
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktKubeletAnno,
							Value: k8sRktKubeletAnnoValue,
						},
					},
				},
			},
		},
		{
			false,
			[]*rktapi.PodFilter{
				{
					States: []rktapi.PodState{rktapi.PodState_POD_STATE_RUNNING},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktKubeletAnno,
							Value: k8sRktKubeletAnnoValue,
						},
					},
				},
			},
		},
	} {
		_, err := r.GetPods(test.All)
		if err != nil {
			t.Errorf("%v", err)
		}
		assert.Equal(t, test.ExpectedFilters, fr.podFilters, "filters didn't match when all=%b", test.All)
	}
}

func TestGetPodStatus(t *testing.T) {
	fr := newFakeRktInterface()
	fs := newFakeSystemd()
	fos := &containertesting.FakeOS{}
	frh := &fakeRuntimeHelper{}
	r := &Runtime{
		apisvc:        fr,
		systemd:       fs,
		runtimeHelper: frh,
		os:            fos,
	}

	ns := func(seconds int64) int64 {
		return seconds * 1e9
	}

	tests := []struct {
		pods   []*rktapi.Pod
		result *kubecontainer.PodStatus
	}{
		// No pods.
		{
			nil,
			&kubecontainer.PodStatus{ID: "42", Name: "guestbook", Namespace: "default"},
		},
		// One pod.
		{
			[]*rktapi.Pod{
				makeRktPod(rktapi.PodState_POD_STATE_RUNNING,
					"uuid-4002", "42", "guestbook", "default",
					"10.10.10.42", ns(10), ns(20), "7",
					[]string{"app-1", "app-2"},
					[]string{"img-id-1", "img-id-2"},
					[]string{"img-name-1", "img-name-2"},
					[]string{"1001", "1002"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_RUNNING, rktapi.AppState_APP_STATE_EXITED},
					[]int32{0, 0},
				),
			},
			&kubecontainer.PodStatus{
				ID:        "42",
				Name:      "guestbook",
				Namespace: "default",
				IP:        "10.10.10.42",
				ContainerStatuses: []*kubecontainer.ContainerStatus{
					{
						ID:           kubecontainer.BuildContainerID("rkt", "uuid-4002:app-1"),
						Name:         "app-1",
						State:        kubecontainer.ContainerStateRunning,
						CreatedAt:    time.Unix(10, 0),
						StartedAt:    time.Unix(20, 0),
						FinishedAt:   time.Unix(0, 30),
						Image:        "img-name-1:latest",
						ImageID:      "rkt://img-id-1",
						Hash:         1001,
						RestartCount: 7,
					},
					{
						ID:           kubecontainer.BuildContainerID("rkt", "uuid-4002:app-2"),
						Name:         "app-2",
						State:        kubecontainer.ContainerStateExited,
						CreatedAt:    time.Unix(10, 0),
						StartedAt:    time.Unix(20, 0),
						FinishedAt:   time.Unix(0, 30),
						Image:        "img-name-2:latest",
						ImageID:      "rkt://img-id-2",
						Hash:         1002,
						RestartCount: 7,
						Reason:       "Completed",
					},
				},
			},
		},
		// Multiple pods.
		{
			[]*rktapi.Pod{
				makeRktPod(rktapi.PodState_POD_STATE_EXITED,
					"uuid-4002", "42", "guestbook", "default",
					"10.10.10.42", ns(10), ns(20), "7",
					[]string{"app-1", "app-2"},
					[]string{"img-id-1", "img-id-2"},
					[]string{"img-name-1", "img-name-2"},
					[]string{"1001", "1002"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_RUNNING, rktapi.AppState_APP_STATE_EXITED},
					[]int32{0, 0},
				),
				makeRktPod(rktapi.PodState_POD_STATE_RUNNING, // The latest pod is running.
					"uuid-4003", "42", "guestbook", "default",
					"10.10.10.42", ns(10), ns(20), "10",
					[]string{"app-1", "app-2"},
					[]string{"img-id-1", "img-id-2"},
					[]string{"img-name-1", "img-name-2"},
					[]string{"1001", "1002"},
					[]rktapi.AppState{rktapi.AppState_APP_STATE_RUNNING, rktapi.AppState_APP_STATE_EXITED},
					[]int32{0, 1},
				),
			},
			&kubecontainer.PodStatus{
				ID:        "42",
				Name:      "guestbook",
				Namespace: "default",
				IP:        "10.10.10.42",
				// Result should contain all containers.
				ContainerStatuses: []*kubecontainer.ContainerStatus{
					{
						ID:           kubecontainer.BuildContainerID("rkt", "uuid-4002:app-1"),
						Name:         "app-1",
						State:        kubecontainer.ContainerStateRunning,
						CreatedAt:    time.Unix(10, 0),
						StartedAt:    time.Unix(20, 0),
						FinishedAt:   time.Unix(0, 30),
						Image:        "img-name-1:latest",
						ImageID:      "rkt://img-id-1",
						Hash:         1001,
						RestartCount: 7,
					},
					{
						ID:           kubecontainer.BuildContainerID("rkt", "uuid-4002:app-2"),
						Name:         "app-2",
						State:        kubecontainer.ContainerStateExited,
						CreatedAt:    time.Unix(10, 0),
						StartedAt:    time.Unix(20, 0),
						FinishedAt:   time.Unix(0, 30),
						Image:        "img-name-2:latest",
						ImageID:      "rkt://img-id-2",
						Hash:         1002,
						RestartCount: 7,
						Reason:       "Completed",
					},
					{
						ID:           kubecontainer.BuildContainerID("rkt", "uuid-4003:app-1"),
						Name:         "app-1",
						State:        kubecontainer.ContainerStateRunning,
						CreatedAt:    time.Unix(10, 0),
						StartedAt:    time.Unix(20, 0),
						FinishedAt:   time.Unix(0, 30),
						Image:        "img-name-1:latest",
						ImageID:      "rkt://img-id-1",
						Hash:         1001,
						RestartCount: 10,
					},
					{
						ID:           kubecontainer.BuildContainerID("rkt", "uuid-4003:app-2"),
						Name:         "app-2",
						State:        kubecontainer.ContainerStateExited,
						CreatedAt:    time.Unix(10, 0),
						StartedAt:    time.Unix(20, 0),
						FinishedAt:   time.Unix(0, 30),
						Image:        "img-name-2:latest",
						ImageID:      "rkt://img-id-2",
						Hash:         1002,
						RestartCount: 10,
						ExitCode:     1,
						Reason:       "Error",
					},
				},
			},
		},
	}

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)
		fr.pods = tt.pods

		podTimes := map[string]time.Time{}
		for _, pod := range tt.pods {
			podTimes[podFinishedMarkerPath(r.runtimeHelper.GetPodDir(tt.result.ID), pod.Id)] = tt.result.ContainerStatuses[0].FinishedAt
		}

		r.os.(*containertesting.FakeOS).StatFn = func(name string) (os.FileInfo, error) {
			podTime, ok := podTimes[name]
			if !ok {
				t.Errorf("osStat called with %v, but only knew about %#v", name, podTimes)
			}
			mockFI := mock_os.NewMockFileInfo(ctrl)
			mockFI.EXPECT().ModTime().Return(podTime)
			return mockFI, nil
		}

		status, err := r.GetPodStatus("42", "guestbook", "default")
		if err != nil {
			t.Errorf("test case #%d: unexpected error: %v", i, err)
		}

		assert.Equal(t, tt.result, status, testCaseHint)
		assert.Equal(t, []string{"ListPods"}, fr.called, testCaseHint)
		fr.CleanCalls()
	}
}

func generateCapRetainIsolator(t *testing.T, caps ...string) appctypes.Isolator {
	retain, err := appctypes.NewLinuxCapabilitiesRetainSet(caps...)
	if err != nil {
		t.Fatalf("Error generating cap retain isolator: %v", err)
	}
	return retain.AsIsolator()
}

func generateCapRevokeIsolator(t *testing.T, caps ...string) appctypes.Isolator {
	revoke, err := appctypes.NewLinuxCapabilitiesRevokeSet(caps...)
	if err != nil {
		t.Fatalf("Error generating cap revoke isolator: %v", err)
	}
	return revoke.AsIsolator()
}

func generateCPUIsolator(t *testing.T, request, limit string) appctypes.Isolator {
	cpu, err := appctypes.NewResourceCPUIsolator(request, limit)
	if err != nil {
		t.Fatalf("Error generating cpu resource isolator: %v", err)
	}
	return cpu.AsIsolator()
}

func generateMemoryIsolator(t *testing.T, request, limit string) appctypes.Isolator {
	memory, err := appctypes.NewResourceMemoryIsolator(request, limit)
	if err != nil {
		t.Fatalf("Error generating memory resource isolator: %v", err)
	}
	return memory.AsIsolator()
}

func baseApp(t *testing.T) *appctypes.App {
	return &appctypes.App{
		Exec:              appctypes.Exec{"/bin/foo", "bar"},
		SupplementaryGIDs: []int{4, 5, 6},
		WorkingDirectory:  "/foo",
		Environment: []appctypes.EnvironmentVariable{
			{"env-foo", "bar"},
		},
		MountPoints: []appctypes.MountPoint{
			{Name: *appctypes.MustACName("mnt-foo"), Path: "/mnt-foo", ReadOnly: false},
		},
		Ports: []appctypes.Port{
			{Name: *appctypes.MustACName("port-foo"), Protocol: "TCP", Port: 4242},
		},
		Isolators: []appctypes.Isolator{
			generateCapRetainIsolator(t, "CAP_SYS_ADMIN"),
			generateCapRevokeIsolator(t, "CAP_NET_ADMIN"),
			generateCPUIsolator(t, "100m", "200m"),
			generateMemoryIsolator(t, "10M", "20M"),
		},
	}
}

func baseImageManifest(t *testing.T) *appcschema.ImageManifest {
	img := &appcschema.ImageManifest{App: baseApp(t)}
	entrypoint, err := json.Marshal([]string{"/bin/foo"})
	if err != nil {
		t.Fatal(err)
	}
	cmd, err := json.Marshal([]string{"bar"})
	if err != nil {
		t.Fatal(err)
	}
	img.Annotations.Set(*appctypes.MustACIdentifier(appcDockerEntrypoint), string(entrypoint))
	img.Annotations.Set(*appctypes.MustACIdentifier(appcDockerCmd), string(cmd))
	return img
}

func baseAppWithRootUserGroup(t *testing.T) *appctypes.App {
	app := baseApp(t)
	app.User, app.Group = "0", "0"
	return app
}

type envByName []appctypes.EnvironmentVariable

func (s envByName) Len() int           { return len(s) }
func (s envByName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s envByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type mountsByName []appctypes.MountPoint

func (s mountsByName) Len() int           { return len(s) }
func (s mountsByName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s mountsByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type portsByName []appctypes.Port

func (s portsByName) Len() int           { return len(s) }
func (s portsByName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s portsByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

type isolatorsByName []appctypes.Isolator

func (s isolatorsByName) Len() int           { return len(s) }
func (s isolatorsByName) Less(i, j int) bool { return s[i].Name < s[j].Name }
func (s isolatorsByName) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func sortAppFields(app *appctypes.App) {
	sort.Sort(envByName(app.Environment))
	sort.Sort(mountsByName(app.MountPoints))
	sort.Sort(portsByName(app.Ports))
	sort.Sort(isolatorsByName(app.Isolators))
}

type sortedStringList []string

func (s sortedStringList) Len() int           { return len(s) }
func (s sortedStringList) Less(i, j int) bool { return s[i] < s[j] }
func (s sortedStringList) Swap(i, j int)      { s[i], s[j] = s[j], s[i] }

func TestSetApp(t *testing.T) {
	tmpDir, err := utiltesting.MkTmpdir("rkt_test")
	if err != nil {
		t.Fatalf("error creating temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	rootUser := int64(0)
	nonRootUser := int64(42)
	runAsNonRootTrue := true
	fsgid := int64(3)

	tests := []struct {
		container *api.Container
		opts      *kubecontainer.RunContainerOptions
		ctx       *api.SecurityContext
		podCtx    *api.PodSecurityContext
		expect    *appctypes.App
		err       error
	}{
		// Nothing should change, but the "User" and "Group" should be filled.
		{
			container: &api.Container{},
			opts:      &kubecontainer.RunContainerOptions{},
			ctx:       nil,
			podCtx:    nil,
			expect:    baseAppWithRootUserGroup(t),
			err:       nil,
		},

		// error verifying non-root.
		{
			container: &api.Container{},
			opts:      &kubecontainer.RunContainerOptions{},
			ctx: &api.SecurityContext{
				RunAsNonRoot: &runAsNonRootTrue,
				RunAsUser:    &rootUser,
			},
			podCtx: nil,
			expect: nil,
			err:    fmt.Errorf("container has no runAsUser and image will run as root"),
		},

		// app's args should be changed.
		{
			container: &api.Container{
				Args: []string{"foo"},
			},
			opts:   &kubecontainer.RunContainerOptions{},
			ctx:    nil,
			podCtx: nil,
			expect: &appctypes.App{
				Exec:              appctypes.Exec{"/bin/foo", "foo"},
				User:              "0",
				Group:             "0",
				SupplementaryGIDs: []int{4, 5, 6},
				WorkingDirectory:  "/foo",
				Environment: []appctypes.EnvironmentVariable{
					{"env-foo", "bar"},
				},
				MountPoints: []appctypes.MountPoint{
					{Name: *appctypes.MustACName("mnt-foo"), Path: "/mnt-foo", ReadOnly: false},
				},
				Ports: []appctypes.Port{
					{Name: *appctypes.MustACName("port-foo"), Protocol: "TCP", Port: 4242},
				},
				Isolators: []appctypes.Isolator{
					generateCapRetainIsolator(t, "CAP_SYS_ADMIN"),
					generateCapRevokeIsolator(t, "CAP_NET_ADMIN"),
					generateCPUIsolator(t, "100m", "200m"),
					generateMemoryIsolator(t, "10M", "20M"),
				},
			},
			err: nil,
		},

		// app should be changed.
		{
			container: &api.Container{
				Command:    []string{"/bin/bar", "$(env-bar)"},
				WorkingDir: tmpDir,
				Resources: api.ResourceRequirements{
					Limits:   api.ResourceList{"cpu": resource.MustParse("50m"), "memory": resource.MustParse("50M")},
					Requests: api.ResourceList{"cpu": resource.MustParse("5m"), "memory": resource.MustParse("5M")},
				},
			},
			opts: &kubecontainer.RunContainerOptions{
				Envs: []kubecontainer.EnvVar{
					{Name: "env-bar", Value: "foo"},
				},
				Mounts: []kubecontainer.Mount{
					{Name: "mnt-bar", ContainerPath: "/mnt-bar", ReadOnly: true},
				},
				PortMappings: []kubecontainer.PortMapping{
					{Name: "port-bar", Protocol: api.ProtocolTCP, ContainerPort: 1234},
				},
			},
			ctx: &api.SecurityContext{
				Capabilities: &api.Capabilities{
					Add:  []api.Capability{"CAP_SYS_CHROOT", "CAP_SYS_BOOT"},
					Drop: []api.Capability{"CAP_SETUID", "CAP_SETGID"},
				},
				RunAsUser:    &nonRootUser,
				RunAsNonRoot: &runAsNonRootTrue,
			},
			podCtx: &api.PodSecurityContext{
				SupplementalGroups: []int64{1, 2},
				FSGroup:            &fsgid,
			},
			expect: &appctypes.App{
				Exec:              appctypes.Exec{"/bin/bar", "foo"},
				User:              "42",
				Group:             "0",
				SupplementaryGIDs: []int{1, 2, 3},
				WorkingDirectory:  tmpDir,
				Environment: []appctypes.EnvironmentVariable{
					{"env-foo", "bar"},
					{"env-bar", "foo"},
				},
				MountPoints: []appctypes.MountPoint{
					{Name: *appctypes.MustACName("mnt-foo"), Path: "/mnt-foo", ReadOnly: false},
					{Name: *appctypes.MustACName("mnt-bar"), Path: "/mnt-bar", ReadOnly: true},
				},
				Ports: []appctypes.Port{
					{Name: *appctypes.MustACName("port-foo"), Protocol: "TCP", Port: 4242},
					{Name: *appctypes.MustACName("port-bar"), Protocol: "TCP", Port: 1234},
				},
				Isolators: []appctypes.Isolator{
					generateCapRetainIsolator(t, "CAP_SYS_CHROOT", "CAP_SYS_BOOT"),
					generateCapRevokeIsolator(t, "CAP_SETUID", "CAP_SETGID"),
					generateCPUIsolator(t, "5m", "50m"),
					generateMemoryIsolator(t, "5M", "50M"),
				},
			},
		},

		// app should be changed. (env, mounts, ports, are overrided).
		{
			container: &api.Container{
				Name:       "hello-world",
				Command:    []string{"/bin/hello", "$(env-foo)"},
				Args:       []string{"hello", "world", "$(env-bar)"},
				WorkingDir: tmpDir,
				Resources: api.ResourceRequirements{
					Limits:   api.ResourceList{"cpu": resource.MustParse("50m")},
					Requests: api.ResourceList{"memory": resource.MustParse("5M")},
				},
			},
			opts: &kubecontainer.RunContainerOptions{
				Envs: []kubecontainer.EnvVar{
					{Name: "env-foo", Value: "foo"},
					{Name: "env-bar", Value: "bar"},
				},
				Mounts: []kubecontainer.Mount{
					{Name: "mnt-foo", ContainerPath: "/mnt-bar", ReadOnly: true},
				},
				PortMappings: []kubecontainer.PortMapping{
					{Name: "port-foo", Protocol: api.ProtocolTCP, ContainerPort: 1234},
				},
			},
			ctx: &api.SecurityContext{
				Capabilities: &api.Capabilities{
					Add:  []api.Capability{"CAP_SYS_CHROOT", "CAP_SYS_BOOT"},
					Drop: []api.Capability{"CAP_SETUID", "CAP_SETGID"},
				},
				RunAsUser:    &nonRootUser,
				RunAsNonRoot: &runAsNonRootTrue,
			},
			podCtx: &api.PodSecurityContext{
				SupplementalGroups: []int64{1, 2},
				FSGroup:            &fsgid,
			},
			expect: &appctypes.App{
				Exec:              appctypes.Exec{"/bin/hello", "foo", "hello", "world", "bar"},
				User:              "42",
				Group:             "0",
				SupplementaryGIDs: []int{1, 2, 3},
				WorkingDirectory:  tmpDir,
				Environment: []appctypes.EnvironmentVariable{
					{"env-foo", "foo"},
					{"env-bar", "bar"},
				},
				MountPoints: []appctypes.MountPoint{
					{Name: *appctypes.MustACName("mnt-foo"), Path: "/mnt-bar", ReadOnly: true},
				},
				Ports: []appctypes.Port{
					{Name: *appctypes.MustACName("port-foo"), Protocol: "TCP", Port: 1234},
				},
				Isolators: []appctypes.Isolator{
					generateCapRetainIsolator(t, "CAP_SYS_CHROOT", "CAP_SYS_BOOT"),
					generateCapRevokeIsolator(t, "CAP_SETUID", "CAP_SETGID"),
					generateCPUIsolator(t, "50m", "50m"),
					generateMemoryIsolator(t, "5M", "5M"),
				},
			},
		},
	}

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)
		img := baseImageManifest(t)
		err := setApp(img, tt.container, tt.opts, tt.ctx, tt.podCtx)
		if err == nil && tt.err != nil || err != nil && tt.err == nil {
			t.Errorf("%s: expect %v, saw %v", testCaseHint, tt.err, err)
		}
		if err == nil {
			sortAppFields(tt.expect)
			sortAppFields(img.App)
			assert.Equal(t, tt.expect, img.App, testCaseHint)
		}
	}
}

func TestGenerateRunCommand(t *testing.T) {
	hostName := "test-hostname"
	tests := []struct {
		pod  *api.Pod
		uuid string

		dnsServers  []string
		dnsSearches []string
		hostName    string
		err         error

		expect string
	}{
		// Case #0, returns error.
		{
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: "pod-name-foo",
				},
				Spec: api.PodSpec{},
			},
			"rkt-uuid-foo",
			[]string{},
			[]string{},
			"",
			fmt.Errorf("failed to get cluster dns"),
			"",
		},
		// Case #1, returns no dns, with private-net.
		{
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: "pod-name-foo",
				},
			},
			"rkt-uuid-foo",
			[]string{},
			[]string{},
			"pod-hostname-foo",
			nil,
			"/bin/rkt/rkt --insecure-options=image,ondisk --local-config=/var/rkt/local/data --dir=/var/data run-prepared --net=rkt.kubernetes.io --hostname=pod-hostname-foo rkt-uuid-foo",
		},
		// Case #2, returns no dns, with host-net.
		{
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: "pod-name-foo",
				},
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: true,
					},
				},
			},
			"rkt-uuid-foo",
			[]string{},
			[]string{},
			"",
			nil,
			fmt.Sprintf("/bin/rkt/rkt --insecure-options=image,ondisk --local-config=/var/rkt/local/data --dir=/var/data run-prepared --net=host --hostname=%s rkt-uuid-foo", hostName),
		},
		// Case #3, returns dns, dns searches, with private-net.
		{
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: "pod-name-foo",
				},
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: false,
					},
				},
			},
			"rkt-uuid-foo",
			[]string{"127.0.0.1"},
			[]string{"."},
			"pod-hostname-foo",
			nil,
			"/bin/rkt/rkt --insecure-options=image,ondisk --local-config=/var/rkt/local/data --dir=/var/data run-prepared --net=rkt.kubernetes.io --dns=127.0.0.1 --dns-search=. --dns-opt=ndots:5 --hostname=pod-hostname-foo rkt-uuid-foo",
		},
		// Case #4, returns no dns, dns searches, with host-network.
		{
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name: "pod-name-foo",
				},
				Spec: api.PodSpec{
					SecurityContext: &api.PodSecurityContext{
						HostNetwork: true,
					},
				},
			},
			"rkt-uuid-foo",
			[]string{"127.0.0.1"},
			[]string{"."},
			"pod-hostname-foo",
			nil,
			fmt.Sprintf("/bin/rkt/rkt --insecure-options=image,ondisk --local-config=/var/rkt/local/data --dir=/var/data run-prepared --net=host --hostname=%s rkt-uuid-foo", hostName),
		},
	}

	rkt := &Runtime{
		os: &kubetesting.FakeOS{HostName: hostName},
		config: &Config{
			Path:            "/bin/rkt/rkt",
			Stage1Image:     "/bin/rkt/stage1-coreos.aci",
			Dir:             "/var/data",
			InsecureOptions: "image,ondisk",
			LocalConfigDir:  "/var/rkt/local/data",
		},
	}

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)
		rkt.runtimeHelper = &fakeRuntimeHelper{tt.dnsServers, tt.dnsSearches, tt.hostName, "", tt.err}

		result, err := rkt.generateRunCommand(tt.pod, tt.uuid)
		assert.Equal(t, tt.err, err, testCaseHint)
		assert.Equal(t, tt.expect, result, testCaseHint)
	}
}

func TestPreStopHooks(t *testing.T) {
	runner := lifecycle.NewFakeHandlerRunner()
	fr := newFakeRktInterface()
	fs := newFakeSystemd()

	rkt := &Runtime{
		runner:              runner,
		apisvc:              fr,
		systemd:             fs,
		containerRefManager: kubecontainer.NewRefManager(),
	}

	tests := []struct {
		pod         *api.Pod
		runtimePod  *kubecontainer.Pod
		preStopRuns []string
		err         error
	}{
		{
			// Case 0, container without any hooks.
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
					UID:       "uid-1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{Name: "container-name-1"},
					},
				},
			},
			&kubecontainer.Pod{
				Containers: []*kubecontainer.Container{
					{ID: kubecontainer.BuildContainerID("rkt", "id-1")},
				},
			},
			[]string{},
			nil,
		},
		{
			// Case 1, containers with pre-stop hook.
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
					UID:       "uid-1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name: "container-name-1",
							Lifecycle: &api.Lifecycle{
								PreStop: &api.Handler{
									Exec: &api.ExecAction{},
								},
							},
						},
						{
							Name: "container-name-2",
							Lifecycle: &api.Lifecycle{
								PreStop: &api.Handler{
									HTTPGet: &api.HTTPGetAction{},
								},
							},
						},
					},
				},
			},
			&kubecontainer.Pod{
				Containers: []*kubecontainer.Container{
					{ID: kubecontainer.BuildContainerID("rkt", "id-1")},
					{ID: kubecontainer.BuildContainerID("rkt", "id-2")},
				},
			},
			[]string{
				"exec on pod: pod-1_ns-1(uid-1), container: container-name-1: rkt://id-1",
				"http-get on pod: pod-1_ns-1(uid-1), container: container-name-2: rkt://id-2",
			},
			nil,
		},
		{
			// Case 2, one container with invalid hooks.
			&api.Pod{
				ObjectMeta: api.ObjectMeta{
					Name:      "pod-1",
					Namespace: "ns-1",
					UID:       "uid-1",
				},
				Spec: api.PodSpec{
					Containers: []api.Container{
						{
							Name: "container-name-1",
							Lifecycle: &api.Lifecycle{
								PreStop: &api.Handler{},
							},
						},
					},
				},
			},
			&kubecontainer.Pod{
				Containers: []*kubecontainer.Container{
					{ID: kubecontainer.BuildContainerID("rkt", "id-1")},
				},
			},
			[]string{},
			errors.NewAggregate([]error{fmt.Errorf("Invalid handler: %v", &api.Handler{})}),
		},
	}

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)

		// Run pre-stop hooks.
		err := rkt.runPreStopHook(tt.pod, tt.runtimePod)
		assert.Equal(t, tt.err, err, testCaseHint)

		sort.Sort(sortedStringList(tt.preStopRuns))
		sort.Sort(sortedStringList(runner.HandlerRuns))

		assert.Equal(t, tt.preStopRuns, runner.HandlerRuns, testCaseHint)

		runner.Reset()
	}
}

func TestGarbageCollect(t *testing.T) {
	fr := newFakeRktInterface()
	fs := newFakeSystemd()
	cli := newFakeRktCli()
	fakeOS := kubetesting.NewFakeOS()
	getter := newFakePodGetter()

	rkt := &Runtime{
		os:                  fakeOS,
		cli:                 cli,
		apisvc:              fr,
		podGetter:           getter,
		systemd:             fs,
		containerRefManager: kubecontainer.NewRefManager(),
	}

	fakeApp := &rktapi.App{Name: "app-foo"}

	tests := []struct {
		gcPolicy             kubecontainer.ContainerGCPolicy
		apiPods              []*api.Pod
		pods                 []*rktapi.Pod
		expectedCommands     []string
		expectedServiceFiles []string
	}{
		// All running pods, should not be gc'd.
		// Dead, new pods should not be gc'd.
		// Dead, old pods should be gc'd.
		// Deleted pods should be gc'd.
		{
			kubecontainer.ContainerGCPolicy{
				MinAge:        0,
				MaxContainers: 0,
			},
			[]*api.Pod{
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-1"}},
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-2"}},
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-3"}},
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-4"}},
			},
			[]*rktapi.Pod{
				{
					Id:        "non-existing-foo",
					State:     rktapi.PodState_POD_STATE_EXITED,
					CreatedAt: time.Now().Add(time.Hour).UnixNano(),
					StartedAt: time.Now().Add(time.Hour).UnixNano(),
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-0",
						},
					},
				},
				{
					Id:        "running-foo",
					State:     rktapi.PodState_POD_STATE_RUNNING,
					CreatedAt: 0,
					StartedAt: 0,
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-1",
						},
					},
				},
				{
					Id:        "running-bar",
					State:     rktapi.PodState_POD_STATE_RUNNING,
					CreatedAt: 0,
					StartedAt: 0,
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-2",
						},
					},
				},
				{
					Id:        "dead-old",
					State:     rktapi.PodState_POD_STATE_EXITED,
					CreatedAt: 0,
					StartedAt: 0,
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-3",
						},
					},
				},
				{
					Id:        "dead-new",
					State:     rktapi.PodState_POD_STATE_EXITED,
					CreatedAt: time.Now().Add(time.Hour).UnixNano(),
					StartedAt: time.Now().Add(time.Hour).UnixNano(),
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-4",
						},
					},
				},
			},
			[]string{"rkt rm dead-old", "rkt rm non-existing-foo"},
			[]string{"/run/systemd/system/k8s_dead-old.service", "/run/systemd/system/k8s_non-existing-foo.service"},
		},
		// gcPolicy.MaxContainers should be enforced.
		// Oldest ones are removed first.
		{
			kubecontainer.ContainerGCPolicy{
				MinAge:        0,
				MaxContainers: 1,
			},
			[]*api.Pod{
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-0"}},
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-1"}},
				{ObjectMeta: api.ObjectMeta{UID: "pod-uid-2"}},
			},
			[]*rktapi.Pod{
				{
					Id:        "dead-2",
					State:     rktapi.PodState_POD_STATE_EXITED,
					CreatedAt: 2,
					StartedAt: 2,
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-2",
						},
					},
				},
				{
					Id:        "dead-1",
					State:     rktapi.PodState_POD_STATE_EXITED,
					CreatedAt: 1,
					StartedAt: 1,
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-1",
						},
					},
				},
				{
					Id:        "dead-0",
					State:     rktapi.PodState_POD_STATE_EXITED,
					CreatedAt: 0,
					StartedAt: 0,
					Apps:      []*rktapi.App{fakeApp},
					Annotations: []*rktapi.KeyValue{
						{
							Key:   k8sRktUIDAnno,
							Value: "pod-uid-0",
						},
					},
				},
			},
			[]string{"rkt rm dead-0", "rkt rm dead-1"},
			[]string{"/run/systemd/system/k8s_dead-0.service", "/run/systemd/system/k8s_dead-1.service"},
		},
	}

	for i, tt := range tests {
		testCaseHint := fmt.Sprintf("test case #%d", i)

		fr.pods = tt.pods
		for _, p := range tt.apiPods {
			getter.pods[p.UID] = p
		}

		err := rkt.GarbageCollect(tt.gcPolicy)
		assert.NoError(t, err, testCaseHint)

		sort.Sort(sortedStringList(tt.expectedCommands))
		sort.Sort(sortedStringList(cli.cmds))

		assert.Equal(t, tt.expectedCommands, cli.cmds, testCaseHint)

		sort.Sort(sortedStringList(tt.expectedServiceFiles))
		sort.Sort(sortedStringList(fakeOS.Removes))

		assert.Equal(t, tt.expectedServiceFiles, fakeOS.Removes, testCaseHint)

		// Cleanup after each test.
		cli.Reset()
		fakeOS.Removes = []string{}
		getter.pods = make(map[types.UID]*api.Pod)
	}
}
