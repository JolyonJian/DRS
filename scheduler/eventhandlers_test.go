/*
Copyright 2019 The Kubernetes Authors.

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

package scheduler

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/api/core/v1"
	storagev1beta1 "k8s.io/api/storage/v1beta1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic/dynamicinformer"
	dyfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/internal/cache"
	"k8s.io/kubernetes/pkg/scheduler/internal/queue"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

func TestNodeAllocatableChanged(t *testing.T) {
	newQuantity := func(value int64) resource.Quantity {
		return *resource.NewQuantity(value, resource.BinarySI)
	}
	for _, test := range []struct {
		Name           string
		Changed        bool
		OldAllocatable v1.ResourceList
		NewAllocatable v1.ResourceList
	}{
		{
			Name:           "no allocatable resources changed",
			Changed:        false,
			OldAllocatable: v1.ResourceList{v1.ResourceMemory: newQuantity(1024)},
			NewAllocatable: v1.ResourceList{v1.ResourceMemory: newQuantity(1024)},
		},
		{
			Name:           "new node has more allocatable resources",
			Changed:        true,
			OldAllocatable: v1.ResourceList{v1.ResourceMemory: newQuantity(1024)},
			NewAllocatable: v1.ResourceList{v1.ResourceMemory: newQuantity(1024), v1.ResourceStorage: newQuantity(1024)},
		},
	} {
		t.Run(test.Name, func(t *testing.T) {
			oldNode := &v1.Node{Status: v1.NodeStatus{Allocatable: test.OldAllocatable}}
			newNode := &v1.Node{Status: v1.NodeStatus{Allocatable: test.NewAllocatable}}
			changed := nodeAllocatableChanged(newNode, oldNode)
			if changed != test.Changed {
				t.Errorf("nodeAllocatableChanged should be %t, got %t", test.Changed, changed)
			}
		})
	}
}

func TestNodeLabelsChanged(t *testing.T) {
	for _, test := range []struct {
		Name      string
		Changed   bool
		OldLabels map[string]string
		NewLabels map[string]string
	}{
		{
			Name:      "no labels changed",
			Changed:   false,
			OldLabels: map[string]string{"foo": "bar"},
			NewLabels: map[string]string{"foo": "bar"},
		},
		// Labels changed.
		{
			Name:      "new node has more labels",
			Changed:   true,
			OldLabels: map[string]string{"foo": "bar"},
			NewLabels: map[string]string{"foo": "bar", "test": "value"},
		},
	} {
		t.Run(test.Name, func(t *testing.T) {
			oldNode := &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: test.OldLabels}}
			newNode := &v1.Node{ObjectMeta: metav1.ObjectMeta{Labels: test.NewLabels}}
			changed := nodeLabelsChanged(newNode, oldNode)
			if changed != test.Changed {
				t.Errorf("Test case %q failed: should be %t, got %t", test.Name, test.Changed, changed)
			}
		})
	}
}

func TestNodeTaintsChanged(t *testing.T) {
	for _, test := range []struct {
		Name      string
		Changed   bool
		OldTaints []v1.Taint
		NewTaints []v1.Taint
	}{
		{
			Name:      "no taint changed",
			Changed:   false,
			OldTaints: []v1.Taint{{Key: "key", Value: "value"}},
			NewTaints: []v1.Taint{{Key: "key", Value: "value"}},
		},
		{
			Name:      "taint value changed",
			Changed:   true,
			OldTaints: []v1.Taint{{Key: "key", Value: "value1"}},
			NewTaints: []v1.Taint{{Key: "key", Value: "value2"}},
		},
	} {
		t.Run(test.Name, func(t *testing.T) {
			oldNode := &v1.Node{Spec: v1.NodeSpec{Taints: test.OldTaints}}
			newNode := &v1.Node{Spec: v1.NodeSpec{Taints: test.NewTaints}}
			changed := nodeTaintsChanged(newNode, oldNode)
			if changed != test.Changed {
				t.Errorf("Test case %q failed: should be %t, not %t", test.Name, test.Changed, changed)
			}
		})
	}
}

func TestNodeConditionsChanged(t *testing.T) {
	nodeConditionType := reflect.TypeOf(v1.NodeCondition{})
	if nodeConditionType.NumField() != 6 {
		t.Errorf("NodeCondition type has changed. The nodeConditionsChanged() function must be reevaluated.")
	}

	for _, test := range []struct {
		Name          string
		Changed       bool
		OldConditions []v1.NodeCondition
		NewConditions []v1.NodeCondition
	}{
		{
			Name:          "no condition changed",
			Changed:       false,
			OldConditions: []v1.NodeCondition{{Type: v1.NodeDiskPressure, Status: v1.ConditionTrue}},
			NewConditions: []v1.NodeCondition{{Type: v1.NodeDiskPressure, Status: v1.ConditionTrue}},
		},
		{
			Name:          "only LastHeartbeatTime changed",
			Changed:       false,
			OldConditions: []v1.NodeCondition{{Type: v1.NodeDiskPressure, Status: v1.ConditionTrue, LastHeartbeatTime: metav1.Unix(1, 0)}},
			NewConditions: []v1.NodeCondition{{Type: v1.NodeDiskPressure, Status: v1.ConditionTrue, LastHeartbeatTime: metav1.Unix(2, 0)}},
		},
		{
			Name:          "new node has more healthy conditions",
			Changed:       true,
			OldConditions: []v1.NodeCondition{},
			NewConditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
		},
		{
			Name:          "new node has less unhealthy conditions",
			Changed:       true,
			OldConditions: []v1.NodeCondition{{Type: v1.NodeDiskPressure, Status: v1.ConditionTrue}},
			NewConditions: []v1.NodeCondition{},
		},
		{
			Name:          "condition status changed",
			Changed:       true,
			OldConditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionFalse}},
			NewConditions: []v1.NodeCondition{{Type: v1.NodeReady, Status: v1.ConditionTrue}},
		},
	} {
		t.Run(test.Name, func(t *testing.T) {
			oldNode := &v1.Node{Status: v1.NodeStatus{Conditions: test.OldConditions}}
			newNode := &v1.Node{Status: v1.NodeStatus{Conditions: test.NewConditions}}
			changed := nodeConditionsChanged(newNode, oldNode)
			if changed != test.Changed {
				t.Errorf("Test case %q failed: should be %t, got %t", test.Name, test.Changed, changed)
			}
		})
	}
}

func TestUpdatePodInCache(t *testing.T) {
	ttl := 10 * time.Second
	nodeName := "node"

	tests := []struct {
		name   string
		oldObj interface{}
		newObj interface{}
	}{
		{
			name:   "pod updated with the same UID",
			oldObj: withPodName(podWithPort("oldUID", nodeName, 80), "pod"),
			newObj: withPodName(podWithPort("oldUID", nodeName, 8080), "pod"),
		},
		{
			name:   "pod updated with different UIDs",
			oldObj: withPodName(podWithPort("oldUID", nodeName, 80), "pod"),
			newObj: withPodName(podWithPort("newUID", nodeName, 8080), "pod"),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			schedulerCache := cache.New(ttl, ctx.Done())
			schedulerQueue := queue.NewTestQueue(ctx, nil)
			sched := &Scheduler{
				SchedulerCache:  schedulerCache,
				SchedulingQueue: schedulerQueue,
			}
			sched.addPodToCache(tt.oldObj)
			sched.updatePodInCache(tt.oldObj, tt.newObj)
			pod, err := sched.SchedulerCache.GetPod(tt.newObj.(*v1.Pod))
			if err != nil {
				t.Errorf("Failed to get pod from scheduler: %v", err)
			}
			if pod.UID != tt.newObj.(*v1.Pod).UID {
				t.Errorf("Want pod UID %v, got %v", tt.newObj.(*v1.Pod).UID, pod.UID)
			}
		})
	}
}

func withPodName(pod *v1.Pod, name string) *v1.Pod {
	pod.Name = name
	return pod
}

func TestPreCheckForNode(t *testing.T) {
	cpu4 := map[v1.ResourceName]string{v1.ResourceCPU: "4"}
	cpu8 := map[v1.ResourceName]string{v1.ResourceCPU: "8"}
	cpu16 := map[v1.ResourceName]string{v1.ResourceCPU: "16"}
	tests := []struct {
		name               string
		nodeFn             func() *v1.Node
		existingPods, pods []*v1.Pod
		want               []bool
	}{
		{
			name: "regular node, pods with a single constraint",
			nodeFn: func() *v1.Node {
				return st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p").HostPort(80).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).Obj(),
				st.MakePod().Name("p2").Req(cpu16).Obj(),
				st.MakePod().Name("p3").Req(cpu4).Req(cpu8).Obj(),
				st.MakePod().Name("p4").NodeAffinityIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p5").NodeAffinityNotIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p6").Obj(),
				st.MakePod().Name("p7").Node("invalid-node").Obj(),
				st.MakePod().Name("p8").HostPort(8080).Obj(),
				st.MakePod().Name("p9").HostPort(80).Obj(),
			},
			want: []bool{true, false, false, true, false, true, false, true, false},
		},
		{
			name: "tainted node, pods with a single constraint",
			nodeFn: func() *v1.Node {
				node := st.MakeNode().Name("fake-node").Obj()
				node.Spec.Taints = []v1.Taint{
					{Key: "foo", Effect: v1.TaintEffectNoSchedule},
					{Key: "bar", Effect: v1.TaintEffectPreferNoSchedule},
				}
				return node
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Obj(),
				st.MakePod().Name("p2").Toleration("foo").Obj(),
				st.MakePod().Name("p3").Toleration("bar").Obj(),
				st.MakePod().Name("p4").Toleration("bar").Toleration("foo").Obj(),
			},
			want: []bool{false, true, false, true},
		},
		{
			name: "regular node, pods with multiple constraints",
			nodeFn: func() *v1.Node {
				return st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
			},
			existingPods: []*v1.Pod{
				st.MakePod().Name("p").HostPort(80).Obj(),
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).NodeAffinityNotIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p2").Req(cpu16).NodeAffinityIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p3").Req(cpu8).NodeAffinityIn("hostname", []string{"fake-node"}).Obj(),
				st.MakePod().Name("p4").HostPort(8080).Node("invalid-node").Obj(),
				st.MakePod().Name("p5").Req(cpu4).NodeAffinityIn("hostname", []string{"fake-node"}).HostPort(80).Obj(),
			},
			want: []bool{false, false, true, false, false},
		},
		{
			name: "tainted node, pods with multiple constraints",
			nodeFn: func() *v1.Node {
				node := st.MakeNode().Name("fake-node").Label("hostname", "fake-node").Capacity(cpu8).Obj()
				node.Spec.Taints = []v1.Taint{
					{Key: "foo", Effect: v1.TaintEffectNoSchedule},
					{Key: "bar", Effect: v1.TaintEffectPreferNoSchedule},
				}
				return node
			},
			pods: []*v1.Pod{
				st.MakePod().Name("p1").Req(cpu4).Toleration("bar").Obj(),
				st.MakePod().Name("p2").Req(cpu4).Toleration("bar").Toleration("foo").Obj(),
				st.MakePod().Name("p3").Req(cpu16).Toleration("foo").Obj(),
				st.MakePod().Name("p3").Req(cpu16).Toleration("bar").Obj(),
			},
			want: []bool{false, true, false, false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodeInfo := framework.NewNodeInfo(tt.existingPods...)
			nodeInfo.SetNode(tt.nodeFn())
			preCheckFn := preCheckForNode(nodeInfo)

			var got []bool
			for _, pod := range tt.pods {
				got = append(got, preCheckFn(pod))
			}

			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}

// test for informers of resources we care about is registered
func TestAddAllEventHandlers(t *testing.T) {
	tests := []struct {
		name                   string
		gvkMap                 map[framework.GVK]framework.ActionType
		expectStaticInformers  map[reflect.Type]bool
		expectDynamicInformers map[schema.GroupVersionResource]bool
	}{
		{
			name:   "default handlers in framework",
			gvkMap: map[framework.GVK]framework.ActionType{},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):  true,
				reflect.TypeOf(&v1.Node{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{},
		},
		{
			name: "add GVKs handlers defined in framework dynamically",
			gvkMap: map[framework.GVK]framework.ActionType{
				"Pod":                               framework.Add | framework.Delete,
				"PersistentVolume":                  framework.Delete,
				"storage.k8s.io/CSIStorageCapacity": framework.Update,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):                            true,
				reflect.TypeOf(&v1.Node{}):                           true,
				reflect.TypeOf(&v1.PersistentVolume{}):               true,
				reflect.TypeOf(&storagev1beta1.CSIStorageCapacity{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{},
		},
		{
			name: "add GVKs handlers defined in plugins dynamically",
			gvkMap: map[framework.GVK]framework.ActionType{
				"daemonsets.v1.apps": framework.Add | framework.Delete,
				"cronjobs.v1.batch":  framework.Delete,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):  true,
				reflect.TypeOf(&v1.Node{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{
				{Group: "apps", Version: "v1", Resource: "daemonsets"}: true,
				{Group: "batch", Version: "v1", Resource: "cronjobs"}:  true,
			},
		},
		{
			name: "add GVKs handlers defined in plugins dynamically, with one illegal GVK form",
			gvkMap: map[framework.GVK]framework.ActionType{
				"daemonsets.v1.apps":    framework.Add | framework.Delete,
				"custommetrics.v1beta1": framework.Update,
			},
			expectStaticInformers: map[reflect.Type]bool{
				reflect.TypeOf(&v1.Pod{}):  true,
				reflect.TypeOf(&v1.Node{}): true,
			},
			expectDynamicInformers: map[schema.GroupVersionResource]bool{
				{Group: "apps", Version: "v1", Resource: "daemonsets"}: true,
			},
		},
	}

	scheme := runtime.NewScheme()
	var localSchemeBuilder = runtime.SchemeBuilder{
		appsv1.AddToScheme,
		batchv1.AddToScheme,
	}
	localSchemeBuilder.AddToScheme(scheme)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stopCh := make(chan struct{})
			testSched := Scheduler{
				StopEverything:  stopCh,
				SchedulingQueue: queue.NewTestQueue(context.Background(), nil),
			}

			client := fake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			dynclient := dyfake.NewSimpleDynamicClient(scheme)
			dynInformerFactory := dynamicinformer.NewDynamicSharedInformerFactory(dynclient, 0)

			addAllEventHandlers(&testSched, informerFactory, dynInformerFactory, tt.gvkMap)

			informerFactory.Start(testSched.StopEverything)
			dynInformerFactory.Start(testSched.StopEverything)
			staticInformers := informerFactory.WaitForCacheSync(testSched.StopEverything)
			dynamicInformers := dynInformerFactory.WaitForCacheSync(testSched.StopEverything)

			if diff := cmp.Diff(tt.expectStaticInformers, staticInformers); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.expectDynamicInformers, dynamicInformers); diff != "" {
				t.Errorf("Unexpected diff (-want, +got):\n%s", diff)
			}
		})
	}
}
