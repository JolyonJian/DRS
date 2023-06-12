/*
Copyright 2020 The Kubernetes Authors.

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

package defaultpreemption

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	v1 "k8s.io/api/core/v1"
	policy "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/informers"
	clientsetfake "k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
	"k8s.io/client-go/tools/events"
	kubeschedulerconfigv1beta2 "k8s.io/kube-scheduler/config/v1beta2"
	extenderv1 "k8s.io/kube-scheduler/extender/v1"
	"k8s.io/kubernetes/pkg/scheduler/apis/config"
	configv1beta2 "k8s.io/kubernetes/pkg/scheduler/apis/config/v1beta2"
	"k8s.io/kubernetes/pkg/scheduler/framework"
	"k8s.io/kubernetes/pkg/scheduler/framework/parallelize"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/defaultbinder"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/feature"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/interpodaffinity"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/names"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/noderesources"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/podtopologyspread"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/queuesort"
	"k8s.io/kubernetes/pkg/scheduler/framework/plugins/tainttoleration"
	"k8s.io/kubernetes/pkg/scheduler/framework/preemption"
	frameworkruntime "k8s.io/kubernetes/pkg/scheduler/framework/runtime"
	internalcache "k8s.io/kubernetes/pkg/scheduler/internal/cache"
	internalqueue "k8s.io/kubernetes/pkg/scheduler/internal/queue"
	st "k8s.io/kubernetes/pkg/scheduler/testing"
)

var (
	negPriority, lowPriority, midPriority, highPriority, veryHighPriority = int32(-100), int32(0), int32(100), int32(1000), int32(10000)

	smallRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "100m",
		v1.ResourceMemory: "100",
	}
	mediumRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "200m",
		v1.ResourceMemory: "200",
	}
	largeRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "300m",
		v1.ResourceMemory: "300",
	}
	veryLargeRes = map[v1.ResourceName]string{
		v1.ResourceCPU:    "500m",
		v1.ResourceMemory: "500",
	}

	epochTime  = metav1.NewTime(time.Unix(0, 0))
	epochTime1 = metav1.NewTime(time.Unix(0, 1))
	epochTime2 = metav1.NewTime(time.Unix(0, 2))
	epochTime3 = metav1.NewTime(time.Unix(0, 3))
	epochTime4 = metav1.NewTime(time.Unix(0, 4))
	epochTime5 = metav1.NewTime(time.Unix(0, 5))
	epochTime6 = metav1.NewTime(time.Unix(0, 6))
)

func getDefaultDefaultPreemptionArgs() *config.DefaultPreemptionArgs {
	v1beta2dpa := &kubeschedulerconfigv1beta2.DefaultPreemptionArgs{}
	configv1beta2.SetDefaults_DefaultPreemptionArgs(v1beta2dpa)
	dpa := &config.DefaultPreemptionArgs{}
	configv1beta2.Convert_v1beta2_DefaultPreemptionArgs_To_config_DefaultPreemptionArgs(v1beta2dpa, dpa, nil)
	return dpa
}

var nodeResourcesFitFunc = frameworkruntime.FactoryAdapter(feature.Features{}, noderesources.NewFit)

// TestPlugin returns Error status when trying to `AddPod` or `RemovePod` on the nodes which have the {k,v} label pair defined on the nodes.
type TestPlugin struct {
	name string
}

func newTestPlugin(injArgs runtime.Object, f framework.Handle) (framework.Plugin, error) {
	return &TestPlugin{name: "test-plugin"}, nil
}

func (pl *TestPlugin) AddPod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod, podInfoToAdd *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node().GetLabels()["error"] == "true" {
		return framework.AsStatus(fmt.Errorf("failed to add pod: %v", podToSchedule.Name))
	}
	return nil
}

func (pl *TestPlugin) RemovePod(ctx context.Context, state *framework.CycleState, podToSchedule *v1.Pod, podInfoToRemove *framework.PodInfo, nodeInfo *framework.NodeInfo) *framework.Status {
	if nodeInfo.Node().GetLabels()["error"] == "true" {
		return framework.AsStatus(fmt.Errorf("failed to remove pod: %v", podToSchedule.Name))
	}
	return nil
}

func (pl *TestPlugin) Name() string {
	return pl.name
}

func (pl *TestPlugin) PreFilterExtensions() framework.PreFilterExtensions {
	return pl
}

func (pl *TestPlugin) PreFilter(ctx context.Context, state *framework.CycleState, p *v1.Pod) *framework.Status {
	return nil
}

func (pl *TestPlugin) Filter(ctx context.Context, state *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	return nil
}

func TestPostFilter(t *testing.T) {
	onePodRes := map[v1.ResourceName]string{v1.ResourcePods: "1"}
	nodeRes := map[v1.ResourceName]string{v1.ResourceCPU: "200m", v1.ResourceMemory: "400"}
	tests := []struct {
		name                  string
		pod                   *v1.Pod
		pods                  []*v1.Pod
		nodes                 []*v1.Node
		filteredNodesStatuses framework.NodeToStatusMap
		extender              framework.Extender
		wantResult            *framework.PostFilterResult
		wantStatus            *framework.Status
	}{
		{
			name: "pod with higher priority can be made schedulable",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(onePodRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode("node1"),
			wantStatus: framework.NewStatus(framework.Success),
		},
		{
			name: "pod with tied priority is still unschedulable",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(onePodRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode(""),
			wantStatus: framework.NewStatus(framework.Unschedulable, "0/1 nodes are available: 1 No victims found on node node1 for preemptor pod p."),
		},
		{
			name: "preemption should respect filteredNodesStatuses",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(onePodRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.UnschedulableAndUnresolvable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode(""),
			wantStatus: framework.NewStatus(framework.Unschedulable, "0/1 nodes are available: 1 Preemption is not helpful for scheduling."),
		},
		{
			name: "pod can be made schedulable on one node",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(midPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Priority(highPriority).Node("node1").Obj(),
				st.MakePod().Name("p2").UID("p2").Namespace(v1.NamespaceDefault).Priority(lowPriority).Node("node2").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(onePodRes).Obj(),
				st.MakeNode().Name("node2").Capacity(onePodRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
				"node2": framework.NewStatus(framework.Unschedulable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode("node2"),
			wantStatus: framework.NewStatus(framework.Success),
		},
		{
			name: "preemption result filtered out by extenders",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
				st.MakePod().Name("p2").UID("p2").Namespace(v1.NamespaceDefault).Node("node2").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(onePodRes).Obj(),
				st.MakeNode().Name("node2").Capacity(onePodRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
				"node2": framework.NewStatus(framework.Unschedulable),
			},
			extender:   &st.FakeExtender{Predicates: []st.FitPredicate{st.Node1PredicateExtender}},
			wantResult: framework.NewPostFilterResultWithNominatedNode("node1"),
			wantStatus: framework.NewStatus(framework.Success),
		},
		{
			name: "no candidate nodes found, no enough resource after removing low priority pods",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(largeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
				st.MakePod().Name("p2").UID("p2").Namespace(v1.NamespaceDefault).Node("node2").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(nodeRes).Obj(), // no enough CPU resource
				st.MakeNode().Name("node2").Capacity(nodeRes).Obj(), // no enough CPU resource
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
				"node2": framework.NewStatus(framework.Unschedulable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode(""),
			wantStatus: framework.NewStatus(framework.Unschedulable, "0/2 nodes are available: 2 Insufficient cpu."),
		},
		{
			name: "no candidate nodes found with mixed reasons, no lower priority pod and no enough CPU resource",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(largeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Priority(highPriority).Obj(),
				st.MakePod().Name("p2").UID("p2").Namespace(v1.NamespaceDefault).Node("node2").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(onePodRes).Obj(), // no pod will be preempted
				st.MakeNode().Name("node2").Capacity(nodeRes).Obj(),   // no enough CPU resource
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
				"node2": framework.NewStatus(framework.Unschedulable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode(""),
			wantStatus: framework.NewStatus(framework.Unschedulable, "0/2 nodes are available: 1 Insufficient cpu, 1 No victims found on node node1 for preemptor pod p."),
		},
		{
			name: "no candidate nodes found with mixed reason, 2 UnschedulableAndUnresolvable nodes and 2 nodes don't have enough CPU resource",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(largeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
				st.MakePod().Name("p2").UID("p2").Namespace(v1.NamespaceDefault).Node("node2").Obj(),
			},
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(nodeRes).Obj(),
				st.MakeNode().Name("node2").Capacity(nodeRes).Obj(),
				st.MakeNode().Name("node3").Capacity(nodeRes).Obj(),
				st.MakeNode().Name("node4").Capacity(nodeRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node3": framework.NewStatus(framework.UnschedulableAndUnresolvable),
				"node4": framework.NewStatus(framework.UnschedulableAndUnresolvable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode(""),
			wantStatus: framework.NewStatus(framework.Unschedulable, "0/4 nodes are available: 2 Insufficient cpu, 2 Preemption is not helpful for scheduling."),
		},
		{
			name: "only one node but failed with TestPlugin",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(largeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
			},
			// label the node with key as "error" so that the TestPlugin will fail with error.
			nodes:                 []*v1.Node{st.MakeNode().Name("node1").Capacity(largeRes).Label("error", "true").Obj()},
			filteredNodesStatuses: framework.NodeToStatusMap{"node1": framework.NewStatus(framework.Unschedulable)},
			wantResult:            nil,
			wantStatus:            framework.AsStatus(errors.New("running RemovePod on PreFilter plugin \"test-plugin\": failed to remove pod: p")),
		},
		{
			name: "one failed with TestPlugin and the other pass",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(largeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Namespace(v1.NamespaceDefault).Node("node1").Obj(),
				st.MakePod().Name("p2").UID("p2").Namespace(v1.NamespaceDefault).Node("node2").Req(mediumRes).Obj(),
			},
			// even though node1 will fail with error but node2 will still be returned as a valid nominated node.
			nodes: []*v1.Node{
				st.MakeNode().Name("node1").Capacity(largeRes).Label("error", "true").Obj(),
				st.MakeNode().Name("node2").Capacity(largeRes).Obj(),
			},
			filteredNodesStatuses: framework.NodeToStatusMap{
				"node1": framework.NewStatus(framework.Unschedulable),
				"node2": framework.NewStatus(framework.Unschedulable),
			},
			wantResult: framework.NewPostFilterResultWithNominatedNode("node2"),
			wantStatus: framework.NewStatus(framework.Success),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cs := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			podInformer.GetStore().Add(tt.pod)
			for i := range tt.pods {
				podInformer.GetStore().Add(tt.pods[i])
			}
			// As we use a bare clientset above, it's needed to add a reactor here
			// to not fail Victims deletion logic.
			cs.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				return true, nil, nil
			})
			// Register NodeResourceFit as the Filter & PreFilter plugin.
			registeredPlugins := []st.RegisterPluginFunc{
				st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
				st.RegisterPluginAsExtensions("test-plugin", newTestPlugin, "PreFilter"),
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}
			var extenders []framework.Extender
			if tt.extender != nil {
				extenders = append(extenders, tt.extender)
			}
			f, err := st.NewFramework(registeredPlugins, "",
				frameworkruntime.WithClientSet(cs),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithPodNominator(internalqueue.NewPodNominator(informerFactory.Core().V1().Pods().Lister())),
				frameworkruntime.WithExtenders(extenders),
				frameworkruntime.WithSnapshotSharedLister(internalcache.NewSnapshot(tt.pods, tt.nodes)),
			)
			if err != nil {
				t.Fatal(err)
			}
			p := DefaultPreemption{
				fh:        f,
				podLister: informerFactory.Core().V1().Pods().Lister(),
				pdbLister: getPDBLister(informerFactory, true),
				args:      *getDefaultDefaultPreemptionArgs(),
			}

			state := framework.NewCycleState()
			// Ensure <state> is populated.
			if status := f.RunPreFilterPlugins(context.Background(), state, tt.pod); !status.IsSuccess() {
				t.Errorf("Unexpected PreFilter Status: %v", status)
			}

			gotResult, gotStatus := p.PostFilter(context.TODO(), state, tt.pod, tt.filteredNodesStatuses)
			// As we cannot compare two errors directly due to miss the equal method for how to compare two errors, so just need to compare the reasons.
			if gotStatus.Code() == framework.Error {
				if diff := cmp.Diff(tt.wantStatus.Reasons(), gotStatus.Reasons()); diff != "" {
					t.Errorf("Unexpected status (-want, +got):\n%s", diff)
				}
			} else {
				if diff := cmp.Diff(tt.wantStatus, gotStatus); diff != "" {
					t.Errorf("Unexpected status (-want, +got):\n%s", diff)
				}
			}
			if diff := cmp.Diff(tt.wantResult, gotResult); diff != "" {
				t.Errorf("Unexpected postFilterResult (-want, +got):\n%s", diff)
			}
		})
	}
}

type candidate struct {
	victims *extenderv1.Victims
	name    string
}

func TestDryRunPreemption(t *testing.T) {
	tests := []struct {
		name                    string
		args                    *config.DefaultPreemptionArgs
		nodeNames               []string
		testPods                []*v1.Pod
		initPods                []*v1.Pod
		registerPlugins         []st.RegisterPluginFunc
		pdbs                    []*policy.PodDisruptionBudget
		fakeFilterRC            framework.Code // return code for fake filter plugin
		disableParallelism      bool
		expected                [][]candidate
		expectedNumFilterCalled []int32
	}{
		{
			name: "a pod that does not fit on any node",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterFilterPlugin("FalseFilter", st.NewFalseFilterPlugin),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Obj(),
			},
			expected:                [][]candidate{{}},
			expectedNumFilterCalled: []int32{2},
		},
		{
			name: "a pod that fits with no preemption",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterFilterPlugin("TrueFilter", st.NewTrueFilterPlugin),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Obj(),
			},
			expected:                [][]candidate{{}},
			fakeFilterRC:            framework.Unschedulable,
			expectedNumFilterCalled: []int32{2},
		},
		{
			name: "a pod that fits on one node with no preemption",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterFilterPlugin("MatchFilter", st.NewMatchFilterPlugin),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				// Name the pod as "node1" to fit "MatchFilter" plugin.
				st.MakePod().Name("node1").UID("node1").Priority(highPriority).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Obj(),
			},
			expected:                [][]candidate{{}},
			fakeFilterRC:            framework.Unschedulable,
			expectedNumFilterCalled: []int32{2},
		},
		{
			name: "a pod that fits on both nodes when lower priority pods are preempted",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj()},
						},
						name: "node1",
					},
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj()},
						},
						name: "node2",
					},
				},
			},
			expectedNumFilterCalled: []int32{4},
		},
		{
			name: "a pod that would fit on the nodes, but other pods running are higher priority, no preemption would happen",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(lowPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			expected:                [][]candidate{{}},
			expectedNumFilterCalled: []int32{0},
		},
		{
			name: "medium priority pod is preempted, but lower priority one stays as it is small",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(largeRes).Obj()},
						},
						name: "node1",
					},
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj()},
						},
						name: "node2",
					},
				},
			},
			expectedNumFilterCalled: []int32{5},
		},
		{
			name: "mixed priority pods are preempted",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(midPriority).Req(mediumRes).Obj(),
				st.MakePod().Name("p1.4").UID("p1.4").Node("node1").Priority(highPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(highPriority).Req(largeRes).Obj(),
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
								st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(midPriority).Req(mediumRes).Obj(),
							},
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{4},
		},
		{
			name: "mixed priority pods are preempted, pick later StartTime one when priorities are equal",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(lowPriority).Req(smallRes).StartTime(epochTime5).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(lowPriority).Req(smallRes).StartTime(epochTime4).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime3).Obj(),
				st.MakePod().Name("p1.4").UID("p1.4").Node("node1").Priority(highPriority).Req(smallRes).StartTime(epochTime2).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(highPriority).Req(largeRes).StartTime(epochTime1).Obj(),
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(lowPriority).Req(smallRes).StartTime(epochTime5).Obj(),
								st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime3).Obj(),
							},
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{4}, // no preemption would happen on node2 and no filter call is counted.
		},
		{
			name: "pod with anti-affinity is preempted",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
				st.RegisterPluginAsExtensions(interpodaffinity.Name, frameworkruntime.FactoryAdapter(feature.Features{}, interpodaffinity.New), "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Label("foo", "").Priority(highPriority).Req(smallRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("foo", "").Priority(lowPriority).Req(smallRes).
					PodAntiAffinityExists("foo", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(highPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(highPriority).Req(smallRes).Obj(),
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("foo", "").Priority(lowPriority).Req(smallRes).
									PodAntiAffinityExists("foo", "hostname", st.PodAntiAffinityWithRequiredReq).Obj(),
							},
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{3}, // no preemption would happen on node2 and no filter call is counted.
		},
		{
			name: "preemption to resolve pod topology spread filter failure",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(podtopologyspread.Name, podtopologyspread.New, "PreFilter", "Filter"),
			},
			nodeNames: []string{"node-a/zone1", "node-b/zone1", "node-x/zone2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Label("foo", "").Priority(highPriority).
					SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj()).
					SpreadConstraint(1, "hostname", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj()).
					Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("pod-a1").UID("pod-a1").Node("node-a").Label("foo", "").Priority(midPriority).Obj(),
				st.MakePod().Name("pod-a2").UID("pod-a2").Node("node-a").Label("foo", "").Priority(lowPriority).Obj(),
				st.MakePod().Name("pod-b1").UID("pod-b1").Node("node-b").Label("foo", "").Priority(lowPriority).Obj(),
				st.MakePod().Name("pod-x1").UID("pod-x1").Node("node-x").Label("foo", "").Priority(highPriority).Obj(),
				st.MakePod().Name("pod-x2").UID("pod-x2").Node("node-x").Label("foo", "").Priority(highPriority).Obj(),
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("pod-a2").UID("pod-a2").Node("node-a").Label("foo", "").Priority(lowPriority).Obj()},
						},
						name: "node-a",
					},
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("pod-b1").UID("pod-b1").Node("node-b").Label("foo", "").Priority(lowPriority).Obj()},
						},
						name: "node-b",
					},
				},
			},
			expectedNumFilterCalled: []int32{5}, // node-a (3), node-b (2), node-x (0)
		},

		{
			name: "get Unschedulable in the preemption phase when the filter plugins filtering the nodes",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			fakeFilterRC:            framework.Unschedulable,
			expected:                [][]candidate{{}},
			expectedNumFilterCalled: []int32{2},
		},
		{
			name: "preemption with violation of same pdb",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
			},
			pdbs: []*policy.PodDisruptionBudget{
				{
					Spec:   policy.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}}},
					Status: policy.PodDisruptionBudgetStatus{DisruptionsAllowed: 1},
				},
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
								st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
							},
							NumPDBViolations: 1,
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{3},
		},
		{
			name: "preemption with violation of the pdb with pod whose eviction was processed, the victim doesn't belong to DisruptedPods",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
			},
			pdbs: []*policy.PodDisruptionBudget{
				{
					Spec:   policy.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}}},
					Status: policy.PodDisruptionBudgetStatus{DisruptionsAllowed: 1, DisruptedPods: map[string]metav1.Time{"p2": {Time: time.Now()}}},
				},
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
								st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
							},
							NumPDBViolations: 1,
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{3},
		},
		{
			name: "preemption with violation of the pdb with pod whose eviction was processed, the victim belongs to DisruptedPods",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
			},
			pdbs: []*policy.PodDisruptionBudget{
				{
					Spec:   policy.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}}},
					Status: policy.PodDisruptionBudgetStatus{DisruptionsAllowed: 1, DisruptedPods: map[string]metav1.Time{"p1.2": {Time: time.Now()}}},
				},
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
								st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
							},
							NumPDBViolations: 0,
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{3},
		},
		{
			name: "preemption with violation of the pdb with pod whose eviction was processed, the victim which belongs to DisruptedPods is treated as 'nonViolating'",
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
			},
			pdbs: []*policy.PodDisruptionBudget{
				{
					Spec:   policy.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}}},
					Status: policy.PodDisruptionBudgetStatus{DisruptionsAllowed: 1, DisruptedPods: map[string]metav1.Time{"p1.3": {Time: time.Now()}}},
				},
			},
			expected: [][]candidate{
				{
					candidate{
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{
								st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
								st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
								st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Label("app", "foo").Priority(midPriority).Req(mediumRes).Obj(),
							},
							NumPDBViolations: 1,
						},
						name: "node1",
					},
				},
			},
			expectedNumFilterCalled: []int32{4},
		},
		{
			name: "all nodes are possible candidates, but DefaultPreemptionArgs limits to 2",
			args: &config.DefaultPreemptionArgs{MinCandidateNodesPercentage: 40, MinCandidateNodesAbsolute: 1},
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2", "node3", "node4", "node5"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p4").UID("p4").Node("node4").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p5").UID("p5").Node("node5").Priority(midPriority).Req(largeRes).Obj(),
			},
			disableParallelism: true,
			expected: [][]candidate{
				{
					// cycle=0 => offset=4 => node5 (yes), node1 (yes)
					candidate{
						name: "node1",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
					candidate{
						name: "node5",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p5").UID("p5").Node("node5").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
				},
			},
			expectedNumFilterCalled: []int32{4},
		},
		{
			name: "some nodes are not possible candidates, DefaultPreemptionArgs limits to 2",
			args: &config.DefaultPreemptionArgs{MinCandidateNodesPercentage: 40, MinCandidateNodesAbsolute: 1},
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2", "node3", "node4", "node5"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(veryHighPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p4").UID("p4").Node("node4").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p5").UID("p5").Node("node5").Priority(veryHighPriority).Req(largeRes).Obj(),
			},
			disableParallelism: true,
			expected: [][]candidate{
				{
					// cycle=0 => offset=4 => node5 (no), node1 (yes), node2 (no), node3 (yes)
					candidate{
						name: "node1",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
					candidate{
						name: "node3",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
				},
			},
			expectedNumFilterCalled: []int32{4},
		},
		{
			name: "preemption offset across multiple scheduling cycles and wrap around",
			args: &config.DefaultPreemptionArgs{MinCandidateNodesPercentage: 40, MinCandidateNodesAbsolute: 1},
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2", "node3", "node4", "node5"},
			testPods: []*v1.Pod{
				st.MakePod().Name("tp1").UID("tp1").Priority(highPriority).Req(largeRes).Obj(),
				st.MakePod().Name("tp2").UID("tp2").Priority(highPriority).Req(largeRes).Obj(),
				st.MakePod().Name("tp3").UID("tp3").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p4").UID("p4").Node("node4").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p5").UID("p5").Node("node5").Priority(midPriority).Req(largeRes).Obj(),
			},
			disableParallelism: true,
			expected: [][]candidate{
				{
					// cycle=0 => offset=4 => node5 (yes), node1 (yes)
					candidate{
						name: "node1",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
					candidate{
						name: "node5",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p5").UID("p5").Node("node5").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
				},
				{
					// cycle=1 => offset=1 => node2 (yes), node3 (yes)
					candidate{
						name: "node2",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
					candidate{
						name: "node3",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
				},
				{
					// cycle=2 => offset=3 => node4 (yes), node5 (yes)
					candidate{
						name: "node4",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p4").UID("p4").Node("node4").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
					candidate{
						name: "node5",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p5").UID("p5").Node("node5").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
				},
			},
			expectedNumFilterCalled: []int32{4, 4, 4},
		},
		{
			name: "preemption looks past numCandidates until a non-PDB violating node is found",
			args: &config.DefaultPreemptionArgs{MinCandidateNodesPercentage: 40, MinCandidateNodesAbsolute: 2},
			registerPlugins: []st.RegisterPluginFunc{
				st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			},
			nodeNames: []string{"node1", "node2", "node3", "node4", "node5"},
			testPods: []*v1.Pod{
				st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			},
			initPods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Label("app", "foo").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Label("app", "foo").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p4").UID("p4").Node("node4").Priority(midPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p5").UID("p5").Node("node5").Label("app", "foo").Priority(midPriority).Req(largeRes).Obj(),
			},
			pdbs: []*policy.PodDisruptionBudget{
				{
					Spec:   policy.PodDisruptionBudgetSpec{Selector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}}},
					Status: policy.PodDisruptionBudgetStatus{DisruptionsAllowed: 0},
				},
			},
			disableParallelism: true,
			expected: [][]candidate{
				{
					// Even though the DefaultPreemptionArgs constraints suggest that the
					// minimum number of candidates is 2, we get three candidates here
					// because we're okay with being a little over (in production, if a
					// non-PDB violating candidate isn't found close to the offset, the
					// number of additional candidates returned will be at most
					// approximately equal to the parallelism in dryRunPreemption).
					// cycle=0 => offset=4 => node5 (yes, pdb), node1 (yes, pdb), node2 (no, pdb), node3 (yes)
					candidate{
						name: "node1",
						victims: &extenderv1.Victims{
							Pods:             []*v1.Pod{st.MakePod().Name("p1").UID("p1").Node("node1").Label("app", "foo").Priority(midPriority).Req(largeRes).Obj()},
							NumPDBViolations: 1,
						},
					},
					candidate{
						name: "node3",
						victims: &extenderv1.Victims{
							Pods: []*v1.Pod{st.MakePod().Name("p3").UID("p3").Node("node3").Priority(midPriority).Req(largeRes).Obj()},
						},
					},
					candidate{
						name: "node5",
						victims: &extenderv1.Victims{
							Pods:             []*v1.Pod{st.MakePod().Name("p5").UID("p5").Node("node5").Label("app", "foo").Priority(midPriority).Req(largeRes).Obj()},
							NumPDBViolations: 1,
						},
					},
				},
			},
			expectedNumFilterCalled: []int32{8},
		},
	}

	labelKeys := []string{"hostname", "zone", "region"}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nodes := make([]*v1.Node, len(tt.nodeNames))
			fakeFilterRCMap := make(map[string]framework.Code, len(tt.nodeNames))
			for i, nodeName := range tt.nodeNames {
				nodeWrapper := st.MakeNode().Capacity(veryLargeRes)
				// Split node name by '/' to form labels in a format of
				// {"hostname": tpKeys[0], "zone": tpKeys[1], "region": tpKeys[2]}
				tpKeys := strings.Split(nodeName, "/")
				nodeWrapper.Name(tpKeys[0])
				for i, labelVal := range strings.Split(nodeName, "/") {
					nodeWrapper.Label(labelKeys[i], labelVal)
				}
				nodes[i] = nodeWrapper.Obj()
				fakeFilterRCMap[nodeName] = tt.fakeFilterRC
			}
			snapshot := internalcache.NewSnapshot(tt.initPods, nodes)

			// For each test, register a FakeFilterPlugin along with essential plugins and tt.registerPlugins.
			fakePlugin := st.FakeFilterPlugin{
				FailedNodeReturnCodeMap: fakeFilterRCMap,
			}
			registeredPlugins := append([]st.RegisterPluginFunc{
				st.RegisterFilterPlugin(
					"FakeFilter",
					func(_ runtime.Object, fh framework.Handle) (framework.Plugin, error) {
						return &fakePlugin, nil
					},
				)},
				st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			)
			registeredPlugins = append(registeredPlugins, tt.registerPlugins...)
			var objs []runtime.Object
			for _, p := range append(tt.testPods, tt.initPods...) {
				objs = append(objs, p)
			}
			for _, n := range nodes {
				objs = append(objs, n)
			}
			informerFactory := informers.NewSharedInformerFactory(clientsetfake.NewSimpleClientset(objs...), 0)
			parallelism := parallelize.DefaultParallelism
			if tt.disableParallelism {
				// We need disableParallelism because of the non-deterministic nature
				// of the results of tests that set custom minCandidateNodesPercentage
				// or minCandidateNodesAbsolute. This is only done in a handful of tests.
				parallelism = 1
			}
			fwk, err := st.NewFramework(
				registeredPlugins, "",
				frameworkruntime.WithPodNominator(internalqueue.NewPodNominator(informerFactory.Core().V1().Pods().Lister())),
				frameworkruntime.WithSnapshotSharedLister(snapshot),
				frameworkruntime.WithInformerFactory(informerFactory),
				frameworkruntime.WithParallelism(parallelism),
			)
			if err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			informerFactory.Start(ctx.Done())
			informerFactory.WaitForCacheSync(ctx.Done())

			nodeInfos, err := snapshot.NodeInfos().List()
			if err != nil {
				t.Fatal(err)
			}
			sort.Slice(nodeInfos, func(i, j int) bool {
				return nodeInfos[i].Node().Name < nodeInfos[j].Node().Name
			})

			if tt.args == nil {
				tt.args = getDefaultDefaultPreemptionArgs()
			}
			pl := &DefaultPreemption{
				fh:        fwk,
				podLister: informerFactory.Core().V1().Pods().Lister(),
				pdbLister: getPDBLister(informerFactory, true),
				args:      *tt.args,
			}

			// Using 4 as a seed source to test getOffsetAndNumCandidates() deterministically.
			// However, we need to do it after informerFactory.WaitforCacheSync() which might
			// set a seed.
			rand.Seed(4)
			var prevNumFilterCalled int32
			for cycle, pod := range tt.testPods {
				state := framework.NewCycleState()
				// Some tests rely on PreFilter plugin to compute its CycleState.
				if status := fwk.RunPreFilterPlugins(context.Background(), state, pod); !status.IsSuccess() {
					t.Errorf("cycle %d: Unexpected PreFilter Status: %v", cycle, status)
				}
				pe := preemption.Evaluator{
					PluginName: names.DefaultPreemption,
					Handler:    pl.fh,
					PodLister:  pl.podLister,
					PdbLister:  pl.pdbLister,
					State:      state,
					Interface:  pl,
				}
				offset, numCandidates := pl.GetOffsetAndNumCandidates(int32(len(nodeInfos)))
				got, _, _ := pe.DryRunPreemption(context.Background(), pod, nodeInfos, tt.pdbs, offset, numCandidates)
				// Sort the values (inner victims) and the candidate itself (by its NominatedNodeName).
				for i := range got {
					victims := got[i].Victims().Pods
					sort.Slice(victims, func(i, j int) bool {
						return victims[i].Name < victims[j].Name
					})
				}
				sort.Slice(got, func(i, j int) bool {
					return got[i].Name() < got[j].Name()
				})
				candidates := []candidate{}
				for i := range got {
					candidates = append(candidates, candidate{victims: got[i].Victims(), name: got[i].Name()})
				}
				if fakePlugin.NumFilterCalled-prevNumFilterCalled != tt.expectedNumFilterCalled[cycle] {
					t.Errorf("cycle %d: got NumFilterCalled=%d, want %d", cycle, fakePlugin.NumFilterCalled-prevNumFilterCalled, tt.expectedNumFilterCalled[cycle])
				}
				prevNumFilterCalled = fakePlugin.NumFilterCalled
				if diff := cmp.Diff(tt.expected[cycle], candidates, cmp.AllowUnexported(candidate{})); diff != "" {
					t.Errorf("cycle %d: unexpected candidates (-want, +got): %s", cycle, diff)
				}
			}
		})
	}
}

func TestSelectBestCandidate(t *testing.T) {
	tests := []struct {
		name           string
		registerPlugin st.RegisterPluginFunc
		nodeNames      []string
		pod            *v1.Pod
		pods           []*v1.Pod
		expected       []string // any of the items is valid
	}{
		{
			name:           "a pod that fits on both nodes when lower priority pods are preempted",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(largeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1").UID("p1").Node("node1").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2").UID("p2").Node("node2").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
			},
			expected: []string{"node1", "node2"},
		},
		{
			name:           "node with min highest priority pod is picked",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(lowPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(lowPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(lowPriority).Req(mediumRes).StartTime(epochTime).Obj(),
			},
			expected: []string{"node3"},
		},
		{
			name:           "when highest priorities are the same, minimum sum of priorities is picked",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(lowPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
			},
			expected: []string{"node2"},
		},
		{
			name:           "when highest priority and sum are the same, minimum number of pods is picked",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(midPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.4").UID("p1.4").Node("node1").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(negPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.3").UID("p3.3").Node("node3").Priority(lowPriority).Req(smallRes).StartTime(epochTime).Obj(),
			},
			expected: []string{"node2"},
		},
		{
			// pickOneNodeForPreemption adjusts pod priorities when finding the sum of the victims. This
			// test ensures that the logic works correctly.
			name:           "sum of adjusted priorities is considered",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(negPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.3").UID("p3.3").Node("node3").Priority(lowPriority).Req(smallRes).StartTime(epochTime).Obj(),
			},
			expected: []string{"node2"},
		},
		{
			name:           "non-overlapping lowest high priority, sum priorities, and number of pods",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3", "node4"},
			pod:            st.MakePod().Name("p").UID("p").Priority(veryHighPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(lowPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p1.3").UID("p1.3").Node("node1").Priority(lowPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(highPriority).Req(largeRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(lowPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.3").UID("p3.3").Node("node3").Priority(lowPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p3.4").UID("p3.4").Node("node3").Priority(lowPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p4.1").UID("p4.1").Node("node4").Priority(midPriority).Req(mediumRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p4.2").UID("p4.2").Node("node4").Priority(midPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p4.3").UID("p4.3").Node("node4").Priority(midPriority).Req(smallRes).StartTime(epochTime).Obj(),
				st.MakePod().Name("p4.4").UID("p4.4").Node("node4").Priority(negPriority).Req(smallRes).StartTime(epochTime).Obj(),
			},
			expected: []string{"node1"},
		},
		{
			name:           "same priority, same number of victims, different start time for each node's pod",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime2).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime2).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(mediumRes).StartTime(epochTime3).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(midPriority).Req(mediumRes).StartTime(epochTime3).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime1).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime1).Obj(),
			},
			expected: []string{"node2"},
		},
		{
			name:           "same priority, same number of victims, different start time for all pods",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime4).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime2).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(mediumRes).StartTime(epochTime5).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(midPriority).Req(mediumRes).StartTime(epochTime1).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime3).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime6).Obj(),
			},
			expected: []string{"node3"},
		},
		{
			name:           "different priority, same number of victims, different start time for all pods",
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			nodeNames:      []string{"node1", "node2", "node3"},
			pod:            st.MakePod().Name("p").UID("p").Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(lowPriority).Req(mediumRes).StartTime(epochTime4).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(midPriority).Req(mediumRes).StartTime(epochTime2).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(midPriority).Req(mediumRes).StartTime(epochTime6).Obj(),
				st.MakePod().Name("p2.2").UID("p2.2").Node("node2").Priority(lowPriority).Req(mediumRes).StartTime(epochTime1).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(lowPriority).Req(mediumRes).StartTime(epochTime3).Obj(),
				st.MakePod().Name("p3.2").UID("p3.2").Node("node3").Priority(midPriority).Req(mediumRes).StartTime(epochTime5).Obj(),
			},
			expected: []string{"node2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rand.Seed(4)
			nodes := make([]*v1.Node, len(tt.nodeNames))
			for i, nodeName := range tt.nodeNames {
				nodes[i] = st.MakeNode().Name(nodeName).Capacity(veryLargeRes).Obj()
			}

			var objs []runtime.Object
			objs = append(objs, tt.pod)
			for _, pod := range tt.pods {
				objs = append(objs, pod)
			}
			cs := clientsetfake.NewSimpleClientset(objs...)
			informerFactory := informers.NewSharedInformerFactory(cs, 0)
			snapshot := internalcache.NewSnapshot(tt.pods, nodes)
			fwk, err := st.NewFramework(
				[]st.RegisterPluginFunc{
					tt.registerPlugin,
					st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
					st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				},
				"",
				frameworkruntime.WithPodNominator(internalqueue.NewPodNominator(informerFactory.Core().V1().Pods().Lister())),
				frameworkruntime.WithSnapshotSharedLister(snapshot),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()
			// Some tests rely on PreFilter plugin to compute its CycleState.
			if status := fwk.RunPreFilterPlugins(context.Background(), state, tt.pod); !status.IsSuccess() {
				t.Errorf("Unexpected PreFilter Status: %v", status)
			}
			nodeInfos, err := snapshot.NodeInfos().List()
			if err != nil {
				t.Fatal(err)
			}

			pl := &DefaultPreemption{
				fh:        fwk,
				podLister: informerFactory.Core().V1().Pods().Lister(),
				pdbLister: getPDBLister(informerFactory, true),
				args:      *getDefaultDefaultPreemptionArgs(),
			}
			pe := preemption.Evaluator{
				PluginName: names.DefaultPreemption,
				Handler:    pl.fh,
				PodLister:  pl.podLister,
				PdbLister:  pl.pdbLister,
				State:      state,
				Interface:  pl,
			}
			offset, numCandidates := pl.GetOffsetAndNumCandidates(int32(len(nodeInfos)))
			candidates, _, _ := pe.DryRunPreemption(context.Background(), tt.pod, nodeInfos, nil, offset, numCandidates)
			s := pe.SelectCandidate(candidates)
			if s == nil || len(s.Name()) == 0 {
				return
			}
			found := false
			for _, nodeName := range tt.expected {
				if nodeName == s.Name() {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("expect any node in %v, but got %v", tt.expected, s.Name())
			}
		})
	}
}

func TestPodEligibleToPreemptOthers(t *testing.T) {
	tests := []struct {
		name                string
		pod                 *v1.Pod
		pods                []*v1.Pod
		nodes               []string
		nominatedNodeStatus *framework.Status
		expected            bool
	}{
		{
			name:                "Pod with nominated node",
			pod:                 st.MakePod().Name("p_with_nominated_node").UID("p").Priority(highPriority).NominatedNodeName("node1").Obj(),
			pods:                []*v1.Pod{st.MakePod().Name("p1").UID("p1").Priority(lowPriority).Node("node1").Terminating().Obj()},
			nodes:               []string{"node1"},
			nominatedNodeStatus: framework.NewStatus(framework.UnschedulableAndUnresolvable, tainttoleration.ErrReasonNotMatch),
			expected:            true,
		},
		{
			name:                "Pod with nominated node, but without nominated node status",
			pod:                 st.MakePod().Name("p_without_status").UID("p").Priority(highPriority).NominatedNodeName("node1").Obj(),
			pods:                []*v1.Pod{st.MakePod().Name("p1").UID("p1").Priority(lowPriority).Node("node1").Terminating().Obj()},
			nodes:               []string{"node1"},
			nominatedNodeStatus: nil,
			expected:            false,
		},
		{
			name:                "Pod without nominated node",
			pod:                 st.MakePod().Name("p_without_nominated_node").UID("p").Priority(highPriority).Obj(),
			pods:                []*v1.Pod{},
			nodes:               []string{},
			nominatedNodeStatus: nil,
			expected:            true,
		},
		{
			name:                "Pod with 'PreemptNever' preemption policy",
			pod:                 st.MakePod().Name("p_with_preempt_never_policy").UID("p").Priority(highPriority).PreemptionPolicy(v1.PreemptNever).Obj(),
			pods:                []*v1.Pod{},
			nodes:               []string{},
			nominatedNodeStatus: nil,
			expected:            false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var nodes []*v1.Node
			for _, n := range test.nodes {
				nodes = append(nodes, st.MakeNode().Name(n).Obj())
			}
			registeredPlugins := []st.RegisterPluginFunc{
				st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
				st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
			}
			f, err := st.NewFramework(registeredPlugins, "",
				frameworkruntime.WithSnapshotSharedLister(internalcache.NewSnapshot(test.pods, nodes)),
			)
			if err != nil {
				t.Fatal(err)
			}
			pl := DefaultPreemption{fh: f}
			if got := pl.PodEligibleToPreemptOthers(test.pod, test.nominatedNodeStatus); got != test.expected {
				t.Errorf("expected %t, got %t for pod: %s", test.expected, got, test.pod.Name)
			}
		})
	}
}
func TestPreempt(t *testing.T) {
	tests := []struct {
		name           string
		pod            *v1.Pod
		pods           []*v1.Pod
		extenders      []*st.FakeExtender
		nodeNames      []string
		registerPlugin st.RegisterPluginFunc
		want           *framework.PostFilterResult
		expectedPods   []string // list of preempted pods
	}{
		{
			name: "basic preemption logic",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).PreemptionPolicy(v1.PreemptLowerPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Node("node2").Priority(highPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Node("node3").Priority(midPriority).Req(mediumRes).Obj(),
			},
			nodeNames:      []string{"node1", "node2", "node3"},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			want:           framework.NewPostFilterResultWithNominatedNode("node1"),
			expectedPods:   []string{"p1.1", "p1.2"},
		},
		{
			name: "preemption for topology spread constraints",
			pod: st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Label("foo", "").Priority(highPriority).
				SpreadConstraint(1, "zone", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj()).
				SpreadConstraint(1, "hostname", v1.DoNotSchedule, st.MakeLabelSelector().Exists("foo").Obj()).
				Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p-a1").UID("p-a1").Namespace(v1.NamespaceDefault).Node("node-a").Label("foo", "").Priority(highPriority).Obj(),
				st.MakePod().Name("p-a2").UID("p-a2").Namespace(v1.NamespaceDefault).Node("node-a").Label("foo", "").Priority(highPriority).Obj(),
				st.MakePod().Name("p-b1").UID("p-b1").Namespace(v1.NamespaceDefault).Node("node-b").Label("foo", "").Priority(lowPriority).Obj(),
				st.MakePod().Name("p-x1").UID("p-x1").Namespace(v1.NamespaceDefault).Node("node-x").Label("foo", "").Priority(highPriority).Obj(),
				st.MakePod().Name("p-x2").UID("p-x2").Namespace(v1.NamespaceDefault).Node("node-x").Label("foo", "").Priority(highPriority).Obj(),
			},
			nodeNames:      []string{"node-a/zone1", "node-b/zone1", "node-x/zone2"},
			registerPlugin: st.RegisterPluginAsExtensions(podtopologyspread.Name, podtopologyspread.New, "PreFilter", "Filter"),
			want:           framework.NewPostFilterResultWithNominatedNode("node-b"),
			expectedPods:   []string{"p-b1"},
		},
		{
			name: "Scheduler extenders allow only node1, otherwise node3 would have been chosen",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).PreemptionPolicy(v1.PreemptLowerPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Namespace(v1.NamespaceDefault).Node("node1").Priority(midPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Namespace(v1.NamespaceDefault).Node("node3").Priority(midPriority).Req(largeRes).Obj(),
			},
			nodeNames: []string{"node1", "node2", "node3"},
			extenders: []*st.FakeExtender{
				{Predicates: []st.FitPredicate{st.TruePredicateExtender}},
				{Predicates: []st.FitPredicate{st.Node1PredicateExtender}},
			},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			want:           framework.NewPostFilterResultWithNominatedNode("node1"),
			expectedPods:   []string{"p1.1", "p1.2"},
		},
		{
			name: "Scheduler extenders do not allow any preemption",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).PreemptionPolicy(v1.PreemptLowerPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Namespace(v1.NamespaceDefault).Node("node1").Priority(midPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Namespace(v1.NamespaceDefault).Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			nodeNames: []string{"node1", "node2", "node3"},
			extenders: []*st.FakeExtender{
				{Predicates: []st.FitPredicate{st.FalsePredicateExtender}},
			},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			want:           nil,
			expectedPods:   []string{},
		},
		{
			name: "One scheduler extender allows only node1, the other returns error but ignorable. Only node1 would be chosen",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).PreemptionPolicy(v1.PreemptLowerPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Namespace(v1.NamespaceDefault).Node("node1").Priority(midPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Namespace(v1.NamespaceDefault).Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			nodeNames: []string{"node1", "node2", "node3"},
			extenders: []*st.FakeExtender{
				{Predicates: []st.FitPredicate{st.ErrorPredicateExtender}, Ignorable: true},
				{Predicates: []st.FitPredicate{st.Node1PredicateExtender}},
			},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			want:           framework.NewPostFilterResultWithNominatedNode("node1"),
			expectedPods:   []string{"p1.1", "p1.2"},
		},
		{
			name: "One scheduler extender allows only node1, but it is not interested in given pod, otherwise node1 would have been chosen",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).PreemptionPolicy(v1.PreemptLowerPriority).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Namespace(v1.NamespaceDefault).Node("node1").Priority(midPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Namespace(v1.NamespaceDefault).Node("node2").Priority(midPriority).Req(largeRes).Obj(),
			},
			nodeNames: []string{"node1", "node2"},
			extenders: []*st.FakeExtender{
				{Predicates: []st.FitPredicate{st.Node1PredicateExtender}, UnInterested: true},
				{Predicates: []st.FitPredicate{st.TruePredicateExtender}},
			},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			// sum of priorities of all victims on node1 is larger than node2, node2 is chosen.
			want:         framework.NewPostFilterResultWithNominatedNode("node2"),
			expectedPods: []string{"p2.1"},
		},
		{
			name: "no preempting in pod",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).PreemptionPolicy(v1.PreemptNever).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Namespace(v1.NamespaceDefault).Node("node2").Priority(highPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Namespace(v1.NamespaceDefault).Node("node3").Priority(midPriority).Req(mediumRes).Obj(),
			},
			nodeNames:      []string{"node1", "node2", "node3"},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			want:           nil,
			expectedPods:   nil,
		},
		{
			name: "PreemptionPolicy is nil",
			pod:  st.MakePod().Name("p").UID("p").Namespace(v1.NamespaceDefault).Priority(highPriority).Req(veryLargeRes).Obj(),
			pods: []*v1.Pod{
				st.MakePod().Name("p1.1").UID("p1.1").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p1.2").UID("p1.2").Namespace(v1.NamespaceDefault).Node("node1").Priority(lowPriority).Req(smallRes).Obj(),
				st.MakePod().Name("p2.1").UID("p2.1").Namespace(v1.NamespaceDefault).Node("node2").Priority(highPriority).Req(largeRes).Obj(),
				st.MakePod().Name("p3.1").UID("p3.1").Namespace(v1.NamespaceDefault).Node("node3").Priority(midPriority).Req(mediumRes).Obj(),
			},
			nodeNames:      []string{"node1", "node2", "node3"},
			registerPlugin: st.RegisterPluginAsExtensions(noderesources.FitName, nodeResourcesFitFunc, "Filter", "PreFilter"),
			want:           framework.NewPostFilterResultWithNominatedNode("node1"),
			expectedPods:   []string{"p1.1", "p1.2"},
		},
	}

	labelKeys := []string{"hostname", "zone", "region"}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := clientsetfake.NewSimpleClientset()
			informerFactory := informers.NewSharedInformerFactory(client, 0)
			podInformer := informerFactory.Core().V1().Pods().Informer()
			podInformer.GetStore().Add(test.pod)
			for i := range test.pods {
				podInformer.GetStore().Add(test.pods[i])
			}

			deletedPodNames := make(sets.String)
			client.PrependReactor("delete", "pods", func(action clienttesting.Action) (bool, runtime.Object, error) {
				deletedPodNames.Insert(action.(clienttesting.DeleteAction).GetName())
				return true, nil, nil
			})

			stop := make(chan struct{})
			defer close(stop)

			cache := internalcache.New(time.Duration(0), stop)
			for _, pod := range test.pods {
				cache.AddPod(pod)
			}
			cachedNodeInfoMap := map[string]*framework.NodeInfo{}
			nodes := make([]*v1.Node, len(test.nodeNames))
			for i, name := range test.nodeNames {
				node := st.MakeNode().Name(name).Capacity(veryLargeRes).Obj()
				// Split node name by '/' to form labels in a format of
				// {"hostname": node.Name[0], "zone": node.Name[1], "region": node.Name[2]}
				node.ObjectMeta.Labels = make(map[string]string)
				for i, label := range strings.Split(node.Name, "/") {
					node.ObjectMeta.Labels[labelKeys[i]] = label
				}
				node.Name = node.ObjectMeta.Labels["hostname"]
				cache.AddNode(node)
				nodes[i] = node

				// Set nodeInfo to extenders to mock extenders' cache for preemption.
				cachedNodeInfo := framework.NewNodeInfo()
				cachedNodeInfo.SetNode(node)
				cachedNodeInfoMap[node.Name] = cachedNodeInfo
			}
			var extenders []framework.Extender
			for _, extender := range test.extenders {
				// Set nodeInfoMap as extenders cached node information.
				extender.CachedNodeNameToInfo = cachedNodeInfoMap
				extenders = append(extenders, extender)
			}
			fwk, err := st.NewFramework(
				[]st.RegisterPluginFunc{
					test.registerPlugin,
					st.RegisterQueueSortPlugin(queuesort.Name, queuesort.New),
					st.RegisterBindPlugin(defaultbinder.Name, defaultbinder.New),
				},
				"",
				frameworkruntime.WithClientSet(client),
				frameworkruntime.WithEventRecorder(&events.FakeRecorder{}),
				frameworkruntime.WithExtenders(extenders),
				frameworkruntime.WithPodNominator(internalqueue.NewPodNominator(informerFactory.Core().V1().Pods().Lister())),
				frameworkruntime.WithSnapshotSharedLister(internalcache.NewSnapshot(test.pods, nodes)),
				frameworkruntime.WithInformerFactory(informerFactory),
			)
			if err != nil {
				t.Fatal(err)
			}

			state := framework.NewCycleState()
			// Some tests rely on PreFilter plugin to compute its CycleState.
			preFilterStatus := fwk.RunPreFilterPlugins(context.Background(), state, test.pod)
			if !preFilterStatus.IsSuccess() {
				t.Errorf("Unexpected preFilterStatus: %v", preFilterStatus)
			}
			// Call preempt and check the expected results.
			pl := DefaultPreemption{
				fh:        fwk,
				podLister: informerFactory.Core().V1().Pods().Lister(),
				pdbLister: getPDBLister(informerFactory, true),
				args:      *getDefaultDefaultPreemptionArgs(),
			}

			pe := preemption.Evaluator{
				PluginName: names.DefaultPreemption,
				Handler:    pl.fh,
				PodLister:  pl.podLister,
				PdbLister:  pl.pdbLister,
				State:      state,
				Interface:  &pl,
			}
			res, status := pe.Preempt(context.Background(), test.pod, make(framework.NodeToStatusMap))
			if !status.IsSuccess() && !status.IsUnschedulable() {
				t.Errorf("unexpected error in preemption: %v", status.AsError())
			}
			if diff := cmp.Diff(test.want, res); diff != "" {
				t.Errorf("Unexpected status (-want, +got):\n%s", diff)
			}
			if len(deletedPodNames) != len(test.expectedPods) {
				t.Errorf("expected %v pods, got %v.", len(test.expectedPods), len(deletedPodNames))
			}
			for victimName := range deletedPodNames {
				found := false
				for _, expPod := range test.expectedPods {
					if expPod == victimName {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("pod %v is not expected to be a victim.", victimName)
				}
			}
			if res != nil && res.NominatingInfo != nil {
				test.pod.Status.NominatedNodeName = res.NominatedNodeName
			}

			// Manually set the deleted Pods' deletionTimestamp to non-nil.
			for _, pod := range test.pods {
				if deletedPodNames.Has(pod.Name) {
					now := metav1.Now()
					pod.DeletionTimestamp = &now
					deletedPodNames.Delete(pod.Name)
				}
			}

			// Call preempt again and make sure it doesn't preempt any more pods.
			res, status = pe.Preempt(context.Background(), test.pod, make(framework.NodeToStatusMap))
			if !status.IsSuccess() && !status.IsUnschedulable() {
				t.Errorf("unexpected error in preemption: %v", status.AsError())
			}
			if res != nil && res.NominatingInfo != nil && len(deletedPodNames) > 0 {
				t.Errorf("didn't expect any more preemption. Node %v is selected for preemption.", res.NominatedNodeName)
			}
		})
	}
}
