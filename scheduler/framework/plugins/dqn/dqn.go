package dqn

import (
	"context"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	framework "k8s.io/kubernetes/pkg/scheduler/framework"
)

const (
	// Name is the name of the plugin used in the plugin registry and configurations.
	Name = "dqn-plugin"
	// ErrReason returned when node name doesn't match.
	ErrReason = "This node is not the result given by the RL scheudler"
)

type DQNPlugin struct {
	choose string
	handle framework.Handle
}

// var _ framework.PreFilterPlugin = DQNPlugin{}
var _ framework.FilterPlugin = DQNPlugin{}

func (dp DQNPlugin) Name() string {
	return Name
}

// func (dp DQNPlugin) PreFilter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod) *framework.Status {
// 	fmt.Printf("[INFO] Prefilter pod: %v\n\n", pod.Name)
// 	return framework.NewStatus(framework.Success, "")
// }

// func (dp DQNPlugin) PreFilterExtensions() framework.PreFilterExtensions {
// 	return nil
// }

func (dp DQNPlugin) Filter(ctx context.Context, cycleState *framework.CycleState, pod *v1.Pod, nodeInfo *framework.NodeInfo) *framework.Status {
	fmt.Printf("[INFO] Filter pod: %v\n\n", pod.Name)
	node := nodeInfo.Node()
	if node == nil {
		return framework.NewStatus(framework.Error, "node not find")
	}

	// get result from rl algorithm
	if dp.choose == "" {
		schedulerurl := "http://192.168.1.113:1234/choose"
		urlValues := url.Values{}
		urlValues.Add("podname", pod.Name)
		resp, err := http.PostForm(schedulerurl, urlValues)
		if err != nil {
			fmt.Printf("[ERROR] Get choose from %v failed!\n\n", schedulerurl)
			fmt.Printf("[INFO] RL scheduling failed, all node will pass the filter!\n\n")
			dp.choose = "error"
		}
		body, _ := ioutil.ReadAll(resp.Body)
		dp.choose = string(body)
	}

	if dp.choose == "error" {
		fmt.Printf("[INFO] Filter pod: %v, node: %v\n\n", pod.Name, nodeInfo.Node().Name)
		return framework.NewStatus(framework.Success, "")
	} else {
		if node.Name == dp.choose {
			fmt.Printf("[INFO] Filter pod: %v, node: %v\n\n", pod.Name, nodeInfo.Node().Name)
			return framework.NewStatus(framework.Success, "")
		} else {
			fmt.Printf("[INFO] Filter pod: %v, node: %v, reason: %v\n\n", pod.Name, nodeInfo.Node().Name, ErrReason)
			return framework.NewStatus(framework.UnschedulableAndUnresolvable, ErrReason)
		}
	}
}

func New(configuration runtime.Object, h framework.Handle) (framework.Plugin, error) {
	return &DQNPlugin{choose: "", handle: h}, nil
}
