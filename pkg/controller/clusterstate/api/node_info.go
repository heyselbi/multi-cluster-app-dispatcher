/*
Copyright 2017 The Kubernetes Authors.

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
/*
Copyright 2019, 2021 The Multi-Cluster App Dispatcher Authors.

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
package api

import (
	"fmt"

	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"
)

// NodeInfo is node level aggregated information.
type NodeInfo struct {
	Name string
	Node *v1.Node

	// The releasing resource on that node
	Releasing *Resource
	// The idle resource on that node
	Idle *Resource
	// The used resource on that node, including running and terminating
	// pods
	Used *Resource

	Allocatable *Resource
	Capability  *Resource

	// Track labels for potential filtering
	Labels map[string]string

	// Track Schedulable flag for potential filtering
	Unschedulable bool

	// Taints for potential filtering
	Taints []v1.Taint

	Tasks map[TaskID]*TaskInfo
}

func NewNodeInfo(node *v1.Node) *NodeInfo {
	if node == nil {
		return &NodeInfo{
			Releasing: EmptyResource(),
			Idle:      EmptyResource(),
			Used:      EmptyResource(),

			Allocatable: EmptyResource(),
			Capability:  EmptyResource(),

			Labels: make(map[string]string),
			Unschedulable: false,
			Taints: []v1.Taint{},

			Tasks: make(map[TaskID]*TaskInfo),
		}
	}

	return &NodeInfo{
		Name: node.Name,
		Node: node,

		Releasing: EmptyResource(),
		Idle:      NewResource(node.Status.Allocatable),
		Used:      EmptyResource(),

		Allocatable: NewResource(node.Status.Allocatable),
		Capability:  NewResource(node.Status.Capacity),

		Labels: node.GetLabels(),
		Unschedulable: node.Spec.Unschedulable,
		Taints: node.Spec.Taints,

		Tasks: make(map[TaskID]*TaskInfo),
	}
}

func (ni *NodeInfo) Clone() *NodeInfo {
	res := NewNodeInfo(ni.Node)

	for _, p := range ni.Tasks {
		res.AddTask(p)
	}

	return res
}

func (ni *NodeInfo) SetNode(node *v1.Node) {
	if ni.Node == nil {
		ni.Idle = NewResource(node.Status.Allocatable)

		for _, task := range ni.Tasks {
			if task.Status == Releasing {
				ni.Releasing.Add(task.Resreq)
			}

			_, err := ni.Idle.Sub(task.Resreq)
			if err != nil {
				klog.Warningf("[SetNode] Node idle amount subtraction err=%v", err)
			}

			ni.Used.Add(task.Resreq)
		}
	}

	ni.Name = node.Name
	ni.Node = node
	ni.Allocatable = NewResource(node.Status.Allocatable)
	ni.Capability = NewResource(node.Status.Capacity)
	ni.Labels = NewStringsMap(node.Labels)
	ni.Unschedulable = node.Spec.Unschedulable
	ni.Taints = NewTaints(node.Spec.Taints)
}

func (ni *NodeInfo) PipelineTask(task *TaskInfo) error {
	key := PodKey(task.Pod)
	if _, found := ni.Tasks[key]; found {
		return fmt.Errorf("task <%v/%v> already on node <%v>",
			task.Namespace, task.Name, ni.Name)
	}

	ti := task.Clone()

	if ni.Node != nil {
		_, err := ni.Releasing.Sub(ti.Resreq)
		if err != nil {
			klog.Warningf("[PipelineTask] Node release subtraction err=%v", err)
		}

		ni.Used.Add(ti.Resreq)
	}

	ni.Tasks[key] = ti

	return nil
}

func (ni *NodeInfo) AddTask(task *TaskInfo) error {
	key := PodKey(task.Pod)
	if _, found := ni.Tasks[key]; found {
		return fmt.Errorf("task <%v/%v> already on node <%v>",
			task.Namespace, task.Name, ni.Name)
	}

	// Node will hold a copy of task to make sure the status
	// change will not impact resource in node.
	ti := task.Clone()

	if ni.Node != nil {
		if ti.Status == Releasing {
			ni.Releasing.Add(ti.Resreq)
		}
		_, err := ni.Idle.Sub(ti.Resreq)
		if err != nil {
			klog.Warningf("[AddTask] Idle resource subtract err=%v", err)
		}

		ni.Used.Add(ti.Resreq)
	}

	ni.Tasks[key] = ti

	return nil
}

func (ni *NodeInfo) RemoveTask(ti *TaskInfo) error {
	klog.V(10).Infof("Attempting to remove task: %s on node: %s", ti.Name,  ni.Name)

	key := PodKey(ti.Pod)

	task, found := ni.Tasks[key]
	if !found {
		return fmt.Errorf("failed to find task <%v/%v> on host <%v>",
			ti.Namespace, ti.Name, ni.Name)
	}

	if ni.Node != nil {
		klog.V(10).Infof("Found node for task: %s, node: %s, task status: %v", task.Name,  ni.Name, task.Status)
		if task.Status == Releasing {
			_, err := ni.Releasing.Sub(task.Resreq)
			if err != nil {
				klog.Warningf("[RemoveTask] Node release subtraction err=%v", err)
			}
		}

		ni.Idle.Add(task.Resreq)
		_, err := ni.Used.Sub(task.Resreq)
		if err != nil {
			klog.Warningf("[RemoveTask] Node usage subtraction err=%v", err)
		}
	} else {
		klog.V(10).Infof("No node info found for task: %s, node: %s", task.Name,  ni.Name)
	}

	delete(ni.Tasks, key)

	return nil
}

func (ni *NodeInfo) UpdateTask(ti *TaskInfo) error {
	klog.V(10).Infof("Attempting to update task: %s on node: %s", ti.Name,  ni.Name)
	if err := ni.RemoveTask(ti); err != nil {
		return err
	}

	return ni.AddTask(ti)
}

func (ni NodeInfo) String() string {
	res := ""

	i := 0
	for _, task := range ni.Tasks {
		res = res + fmt.Sprintf("\n\t %d: %v", i, task)
		i++
	}

	return fmt.Sprintf("Node (%s): idle <%v>, used <%v>, releasing <%v>%s",
		ni.Name, ni.Idle, ni.Used, ni.Releasing, res)

}
