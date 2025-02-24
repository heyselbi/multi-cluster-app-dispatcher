// +build private
// ------------------------------------------------------ {COPYRIGHT-TOP} ---
// Copyright 2022 The Multi-Cluster App Dispatcher Authors.
// 
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// 
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
// ------------------------------------------------------ {COPYRIGHT-END} ---

package quotamanager

import (
	"bytes"
	"fmt"
	"github.com/project-codeflare/multi-cluster-app-dispatcher/cmd/kar-controllers/app/options"
	arbv1 "github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/apis/controller/v1beta1"
	listersv1 "github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/client/listers/controller/v1"
	clusterstateapi "github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/controller/clusterstate/api"
	"github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/controller/quota"
	rpmanager "github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/controller/quota/quotamanager/qm_lib_backend_with_resplan_mgr/resplanmgr"
	"github.com/project-codeflare/multi-cluster-app-dispatcher/pkg/controller/quota/quotamanager/util"
	qmbackend "github.ibm.com/ai-foundation/quota-manager/quota"
	qmbackendutils "github.ibm.com/ai-foundation/quota-manager/quota/utils"
	"k8s.io/client-go/rest"
	"strings"

	"k8s.io/klog/v2"
	"math"
	"reflect"
)

const (
	// Quota Manager Forest Name
	QuotaManagerForestName string = "MCAD-CONTROLLER-FOREST"

	// Default Tree Name
	DefaultQuotaTreeName = "UNKNOWNTREENAME"

	// Default Tree Node Name
	DefaultQuotaNodeName = "UNKNOWNTREENODENAME"

	MaxInt = int(^uint(0) >> 1)

)

// QuotaManager implements a QuotaManagerInterface.
type QuotaManager struct {
	url                 string
	appwrapperLister    listersv1.AppWrapperLister
	preemptionEnabled   bool
	quotaManagerBackend *qmbackend.Manager
	resourcePlanManager *rpmanager.ResourcePlanManager
	initializationDone  bool
}

type QuotaGroup struct {
	GroupContext string  `json:"groupcontext"`
	GroupId	     string  `json:"groupid"`
}

type Request struct {
	Id          string       `json:"id"`
	Groups      []QuotaGroup `json:"groups"`
	Demand      []int        `json:"demand"`
	Priority    int          `json:"priority"`
	Preemptable bool         `json:"preemptable"`
}

type QuotaResponse struct {
	Id          string       `json:"id"`
	Groups      []QuotaGroup `json:"groups"`
	Demand      []int        `json:"demand"`
	Priority    int          `json:"priority"`
	Preemptable bool         `json:"preemptable"`
	PreemptIds  []string     `json:"preemptedIds"`
	CreateDate  string       `json:"dateCreated"`
}

type TreeNode struct {
	Allocation	string     `json:"allocation"`
	Quota		string  `json:"quota"`
	Name		string   `json:"name"`
	Hard		bool     `json:"hard"`
	Children	[]TreeNode   `json:"children"`
	Parent 		string `json:"parent"`
}

// Making sure that QuotaManager implements QuotaManager.
var _ = quota.QuotaManagerInterface(&QuotaManager{})

func getDispatchedAppWrapper(dispatchedAWs map[string]*arbv1.AppWrapper, awId string) *arbv1.AppWrapper {
	// Find Appwrapper that is run (runnable)
	aw := dispatchedAWs[awId]
	if aw != nil && aw.Status.CanRun == true {
		return aw
	}
	return nil
}

func NewQuotaManager(dispatchedAWDemands map[string]*clusterstateapi.Resource, dispatchedAWs map[string]*arbv1.AppWrapper,
			awJobLister listersv1.AppWrapperLister, config *rest.Config, serverOptions *options.ServerOption) (*QuotaManager, error) {

	if serverOptions.QuotaEnabled == false {
		klog.
			Infof("[NewQuotaManager] Quota management is not enabled.")
		return nil, nil
	}

	qm := &QuotaManager{
		url:                 serverOptions.QuotaRestURL,
		appwrapperLister:    awJobLister,
		preemptionEnabled:   serverOptions.Preemption,
		quotaManagerBackend: qmbackend.NewManager(),
		initializationDone:  false,
	}

	// Set the name of the forest in the backend
	qm.quotaManagerBackend.AddForest(QuotaManagerForestName)
	klog.V(10).Infof("[NewQuotaManager] Before initialization ResourcePlan informer - %s", qm.quotaManagerBackend.String())

	// Create a resource plan manager
	qm.resourcePlanManager, _ = rpmanager.NewResourcePlanManager(config, qm.quotaManagerBackend)

	// Initialize Forest/Trees if new resource plan manager added to the cache
	err := qm.updateForestFromCache()
	if err != nil {
		klog.Errorf("[dispatchedAWDemands] Failure during Quota Manager Backend Cache refresh, err=%#v", err)
	}

	// Add AppWrappers that have been evaluated as runnable to QuotaManager
	err2 := qm.loadDispatchedAWs(dispatchedAWDemands, dispatchedAWs)
	if err2 != nil {
		klog.Errorf("[dispatchedAWDemands] Failure during Quota Manager Backend Cache refresh, err=%#v",
														err2)
		// Combine errors for function return
		if err != nil {
			err = fmt.Errorf("%w; Next error %s", err, err2.Error())
		} else {
			err = err2
		}
	}
	// Set mode of quota manager
	qm.quotaManagerBackend.SetMode(qmbackend.Normal)

	treeNames := qm.quotaManagerBackend.GetTreeNames()

	for _, treeName := range treeNames {
		klog.V(4).Infof("[NewQuotaManager] Quota Manager Backend tree %s processing completed.", treeName)
	}

	qm.initializationDone = true
	return qm, err
}

func (qm *QuotaManager) loadDispatchedAWs(dispatchedAWDemands map[string]*clusterstateapi.Resource,
						dispatchedAWs map[string]*arbv1.AppWrapper) error {

	// Nothing to do
	if dispatchedAWDemands == nil || len(dispatchedAWDemands) <= 0 {
		klog.V(4).Infof("[loadDispatchedAWs] No dispatched AppWrappers found to preload.")
		return nil
	}


	// Process list of AppWrappers that are already dispatched
	var err error
	err = nil

	for k, v := range dispatchedAWDemands{
		aw := getDispatchedAppWrapper(dispatchedAWs, k)
		if aw != nil {

			doesFit, preemptionIds, err2:= qm.Fits(aw, v, nil)
			if doesFit == false {
				klog.Errorf("[loadDispatchedAWs] Loading of AppWrapper %s/%s failed.",
										aw.Namespace, aw.Name)
				err = fmt.Errorf("Loading of AppWrapper %s/%s failed, %#v \n",
										aw.Namespace, aw.Name, err2)
			}

			if preemptionIds != nil && len(preemptionIds) > 0 {
				klog.Errorf("[loadDispatchedAWs] Loading of AppWrapper %s/%s caused invalid preemptions: %v.  Quota Manager is in inconsistent state.",
					aw.Namespace, aw.Name, preemptionIds)
				if err == nil {
					err = fmt.Errorf("Loading of AppWrapper %s/%s caused invalid preemptions: %v.  Quota Manager is in inconsistent state. \n",
						aw.Namespace, aw.Name, preemptionIds)
				} else {
					err = fmt.Errorf("%w; Next error %s Loading of AppWrapper %s/%s caused invalid preemptions: %v.  Quota Manager is in inconsistent state. \n",
						err, aw.Namespace, aw.Name, preemptionIds)
				}
			}
			klog.V(4).Infof("[loadDispatchedAWs] Dispatched AppWrappers %s/%s found to preload.", aw.Namespace, aw.Name)
		} else {
			klog.Warningf("[loadDispatchedAWs] Unable to obtain AppWrapper from key: %s.  Loading of AppWrapper will be skipped.",
				k)
		}
	}

	return err
}

func (qm *QuotaManager) updateForestFromCache() error {
	unallocatedConsumers, treeCacheCreateResponse, err := qm.quotaManagerBackend.UpdateForest(QuotaManagerForestName)

	if treeCacheCreateResponse != nil {
		for k, v := range treeCacheCreateResponse {
			danglingNodeNames := v.DanglingNodeNames
			if danglingNodeNames != nil {
				for _, danglingNodeName := range danglingNodeNames {
					klog.Errorf("[updateForestFromCache] Failure to link node %s to tree %s after Quota Manager Backend Cache refresh.", danglingNodeName, k)
				}
			}
			klog.V(10).Infof("[updateForestFromCache] %s", qm.quotaManagerBackend.String())
		}
	}

	if unallocatedConsumers != nil && len(unallocatedConsumers) > 0 {
		for _, unallocatedConsumer := range unallocatedConsumers {
			klog.Errorf("[updateForestFromCache] Failure to allocate %s after Quota Manager Backend Cache refresh.", unallocatedConsumer)
		}
	}

	return err
}

// Recrusive call to add names of Tree
func (qm *QuotaManager) addChildrenNodes(parentNode TreeNode, treeIDs []string) ([]string) {
	if len(parentNode.Children) > 0 {
		for _, childNode := range parentNode.Children {
			klog.V(10).Infof("[getQuotaTreeIDs] Quota tree response child node from quota mananger: %s", childNode.Name)
			treeIDs = qm.addChildrenNodes(childNode, treeIDs)
		}
	}
	treeIDs = append(treeIDs, parentNode.Name)
	return treeIDs
}

func isValidQuota(quotaGroup QuotaGroup, qmTreeIDs []string) bool {
	for _, treeNodeID := range qmTreeIDs {
		if strings.Compare(treeNodeID, quotaGroup.GroupContext) == 0 {
			return true
		}
	}
	return false
}

func (qm *QuotaManager) getQuotaDesignation(aw *arbv1.AppWrapper) ([]QuotaGroup, map[string][]string, error) {
	var groups []QuotaGroup
	treeNameToResourceTypes := make(map[string][]string)

	// Get list of quota management tree IDs
	qmTreeIDs := qm.quotaManagerBackend.GetTreeNames()
	if len(qmTreeIDs) <= 0 {
		klog.Warningf("[getQuotaDesignation] No quota management IDs defined for quota evalution of for AppWrapper Job: %s/%s",
			aw.Namespace, aw.Name)
		return groups, treeNameToResourceTypes, nil
	}

	labels := aw.GetLabels()
	if ( labels != nil) {
		keys := reflect.ValueOf(labels).MapKeys()
		for _,  key := range keys {
			strkey := key.String()
			quotaGroup := QuotaGroup{
				GroupContext: strkey,
				GroupId: labels[strkey],
			}
			if isValidQuota(quotaGroup, qmTreeIDs) {
				// Save the quota designation ain return var
				groups = append(groups, quotaGroup)
				klog.V(8).Infof("[getQuotaDesignation] AppWrapper: %s/%s quota label: %v found.",
					aw.Namespace, aw.Name, quotaGroup)
				// Save the related resource types in return var
				treeNameToResourceTypes[quotaGroup.GroupContext] = qm.quotaManagerBackend.GetTreeCache(quotaGroup.GroupContext).GetResourceNames()

			} else {
				klog.V(10).Infof("[getQuotaDesignation] AppWrapper: %s/%s label: %v ignored.  Not a valid quota ID from Quota Tree list: %v.",
					aw.Namespace, aw.Name, quotaGroup, qmTreeIDs)
			}
		}
	} else {
		klog.V(4).Infof("[getQuotaDesignation] AppWrapper: %s/%s does not any context quota labels.",
										aw.Namespace, aw.Name)
	}

	// Figure out which quota tree allocation is missing and produce an error
	if len(groups) < len(qmTreeIDs) {
		var allocationMessage bytes.Buffer
		fmt.Fprintf(&allocationMessage, "Missing required quota designation: ")

		numMissingTreesCt := 0
		for _, treeName := range qmTreeIDs {
			treeFound := false
			for _, quotaGroup := range groups {
				if strings.Compare(treeName, quotaGroup.GroupContext) == 0 {
					treeFound = true
					break
				}
			}
			if treeFound {
				continue
			} else {
				if numMissingTreesCt < 1 {
					fmt.Fprintf(&allocationMessage, "%s", treeName)
				} else {
					fmt.Fprintf(&allocationMessage, ", %s", treeName)
				}
				numMissingTreesCt++
			}
		}

		// Produce an error
		var err error
		err = nil
		if len(allocationMessage.String()) > 0 {
			fmt.Fprintf(&allocationMessage, ".")
			err = fmt.Errorf(allocationMessage.String())
		} else {
			err = fmt.Errorf("Unknown error verifying quota designations.")
		}
		klog.V(6).Infof("[getQuotaDesignation] No valid quota management IDs found for AppWrapper Job: %s/%s, err=%#v",
			aw.Namespace, aw.Name, err)
		return groups, treeNameToResourceTypes, err
	}

	if len(groups) > 0 {
		klog.V(6).Infof("[getQuotaDesignation] AppWrapper: %s/%s quota labels: %v.", aw.Namespace,
			aw.Name, groups)
	} else {
		klog.Warningf("[getQuotaDesignation] AppWrapper: %s/%s does not have any quota labels.",
			aw.Namespace, aw.Name)
	}

	return groups, treeNameToResourceTypes, nil
}

func (qm *QuotaManager) convertInt64Demand (int64Demand int64) (int, error) {
	var err error
	err = nil
	if int64Demand > int64(MaxInt) {
		err = fmt.Errorf("demand %d is larger than Max Quota Management Backend size, resetting demand to %d",
			int64Demand, MaxInt)
		return MaxInt, err
	} else {
		return int(int64Demand), err
	}
}

func (qm *QuotaManager) convertFloat64Demand (floatDemand float64) (int, error) {
	var err error
	err = nil
	if floatDemand > float64(MaxInt) {
		err = fmt.Errorf("demand %f is larger than Max Quota Management Backend size, resetting demand to %d",
			floatDemand, MaxInt)
		return MaxInt, err
	} else {
		return int(math.Trunc(floatDemand)), err
	}
}

func (qm *QuotaManager) getQuotaTreeResourceTypesDemands(awResDemands *clusterstateapi.Resource, treeToResourceTypes []string)  (map[string]int, error) {
	demands := map[string]int{}
	var err error
	err = nil
	var processedResourceTypes []string

	for _, treeResourceType := range treeToResourceTypes {

		// CPU Demands
		if strings.Contains(strings.ToLower(treeResourceType), "cpu") {
			// Handle type conversions
			demand, converErr := qm.convertFloat64Demand(awResDemands.MilliCPU)
			if converErr != nil {
				if err == nil {
					err = fmt.Errorf("resource type: %s %s",
						treeResourceType, converErr.Error())
				} else {
					err = fmt.Errorf("%w; next error resource type: %s %s",
						err, treeResourceType, converErr.Error())
				}
			}
			demands[treeResourceType] = demand
			processedResourceTypes = append(processedResourceTypes, treeResourceType)
		}

		// Memory Demands
		if strings.Contains(strings.ToLower(treeResourceType), "memory") {
			// Handle type conversions
			demand, converErr := qm.convertFloat64Demand(awResDemands.Memory/1000000)
			if converErr != nil {
				if err == nil {
					err = fmt.Errorf("resource type: %s %s",
						treeResourceType, converErr.Error())
				} else {
					err = fmt.Errorf("%w; next error resource type: %s %s",
						err, treeResourceType, converErr.Error())
				}
			}
			demands[treeResourceType] = demand
			processedResourceTypes = append(processedResourceTypes, treeResourceType)

		}

		// GPU Demands
		if strings.Contains(strings.ToLower(treeResourceType), "gpu") {
			// Handle type conversions
			demand, converErr := qm.convertInt64Demand(awResDemands.GPU)

			if converErr != nil {
				if err == nil {
					err = fmt.Errorf("resource type: %s %s",
						treeResourceType, converErr.Error())
				} else {
					err = fmt.Errorf("%w; next error resource type: %s %s",
						err, treeResourceType, converErr.Error())
				}
			}
			demands[treeResourceType] = demand
			processedResourceTypes = append(processedResourceTypes, treeResourceType)
		}
	}

	if len(processedResourceTypes) < len(treeToResourceTypes) {
		if err == nil {
			err = fmt.Errorf("resource type processed %v less than expected %v",
				processedResourceTypes, treeToResourceTypes)
		} else {
			err = fmt.Errorf("%w; next error resource type processed %v less than expected %v",
				err, processedResourceTypes, treeToResourceTypes)
		}
	}

	klog.V(10).Infof("[getQuotaTreeResourceTypesDemands] Quota resource demands: %#v.", demands)
	return demands, err
}

func (qm *QuotaManager) buildRequest(aw *arbv1.AppWrapper,
			awResDemands *clusterstateapi.Resource) (*qmbackend.ConsumerInfo, error) {
	awId := util.CreateId(aw.Namespace, aw.Name)
	if len(awId) <= 0 {
		err := fmt.Errorf("[buildRequest] Request failed due to invalid AppWrapper due to empty namespace: %s or name:%s.", aw.Namespace, aw.Name)
		return nil, err
	}

	var consumerTrees []qmbackendutils.JConsumerTreeSpec

	// Get quota tree designations and associated resource demands from AW labels
	quotaTreeDesignations, treeNameToResourceTypes, err := qm.getQuotaDesignation(aw)

	if err != nil {
		return nil, err
	}

	for _, quotaTreeDesignation := range quotaTreeDesignations {
		unPreemptable := !qm.preemptionEnabled

		quotaTreeName := quotaTreeDesignation.GroupContext
		demands, err := qm.getQuotaTreeResourceTypesDemands(awResDemands, treeNameToResourceTypes[quotaTreeName])
		if err != nil {
			klog.Errorf("[buildRequest] Failure building quota resource demands for AppWrapper %s/%s, err=%#v",
				aw.Namespace, aw.Name, err)
		}

		priority := int(aw.Spec.Priority)

		consumerTreeSpec := &qmbackendutils.JConsumerTreeSpec {
			ID:            awId,
			TreeName:      quotaTreeDesignation.GroupContext,
			GroupID:       quotaTreeDesignation.GroupId,
			Request:       demands,
			Priority:      priority,
			CType:         0,
			UnPreemptable: unPreemptable,
		}

		consumerTrees = append(consumerTrees, *consumerTreeSpec)

	}

	// Add quota demands per tree to quota allocation request
	consumerSpec := &qmbackendutils.JConsumerSpec  {
		ID:	awId,
		Trees:	consumerTrees,
	}

	// JConsumer : JSON consumer
	consumer := &qmbackendutils.JConsumer  {
		Kind: qmbackendutils.DefaultConsumerKind,
		Spec: *consumerSpec,
	}

	consumerInfo, err := qmbackend.NewConsumerInfo(*consumer)

	return consumerInfo, err
}

func (qm *QuotaManager) refreshQuotaDefiniions() error {
	// Initialize Forest/Trees if new resource plan manager added to the cache
	err := qm.updateForestFromCache()

	return err
}

func (qm *QuotaManager) Fits(aw *arbv1.AppWrapper, awResDemands *clusterstateapi.Resource,
					proposedPreemptions []*arbv1.AppWrapper) (bool, []*arbv1.AppWrapper, string) {

	doesFit := false

	// If a Quota Manager Backend instance does not exists then assume quota failed
	if qm.quotaManagerBackend == nil {
		klog.V(4).Infof("[Fits] No quota manager backend exists, %#v fails quota by default.",
													awResDemands)
		return doesFit, nil, "No quota manager backend exists"
	}

	// If Quota Manager initialization is complete but quota manager backend is in maintenance mode assume quota
	// Processing quota requests is allow during initialization and backend is in maitenace mode for recovery purposes
	if qm.quotaManagerBackend.GetMode() == qmbackend.Maintenance && qm.initializationDone {
		klog.Warningf("[Fits] Quota Manager backend in maintenance mode.  Unable to process request for AppWrapper: %s/%s",
			aw.Namespace, aw.Name)
		return doesFit, nil, "Quota Manager backend in maintenance mode"
	}

	// Refresh Quota Manager Backend Cache and Tree(s) if detected change in ResourcePlans
	if qm.resourcePlanManager.IsResplanChanged() {
		// Load ResourcePlan Cache into Quoto Management Backend Cache
		qm.resourcePlanManager.LoadResourcePlansIntoBackend()
		// Realize new Quoto Management tree(s) from Backend Cache
		err := qm.updateForestFromCache()
		if err != nil {
			klog.Errorf("[Fits] Failure during refresh of quota tree(s), err=%#v.", err)
		}
	}

	// Create a consumer
	consumerInfo, err := qm.buildRequest(aw, awResDemands)
	if err != nil {
		klog.Errorf("[Fits] Creation of quota request failed: %s/%s, err=%#v.", aw.Namespace, aw.Name, err)
		return doesFit, nil, err.Error()
	}

	var preemptIds []*arbv1.AppWrapper

	qm.quotaManagerBackend.AddConsumer(consumerInfo)

	consumerID := consumerInfo.GetID()
	klog.V(4).Infof("[Fits] Sending quota allocation request: %#v ", consumerInfo)
	allocResponse, err := qm.quotaManagerBackend.AllocateForest(QuotaManagerForestName, consumerID)

	if err != nil {
		if allocResponse != nil && len(allocResponse.GetMessage()) > 0 {
			klog.Errorf("[Fits] Error allocating consumer: %s/%s, msg=%s, err=%#v.",
				aw.Namespace, aw.Name, allocResponse.GetMessage(), err)
			return 	doesFit, nil, allocResponse.GetMessage()
		} else {
			klog.Errorf("[Fits] Error allocating consumer: %s/%s, err=%#v.",
				aw.Namespace, aw.Name, err)
			return 	doesFit, nil, err.Error()

		}
	}

	doesFit = allocResponse.IsAllocated()
	if len(allocResponse.GetMessage()) > 0 {
		klog.Warningf("[Fits] Response from Quota Management backend: %s",
			allocResponse.GetMessage())
	}
	preemptIds = qm.getAppWrappers(allocResponse.GetPreemptedIds())

	return doesFit, preemptIds, allocResponse.GetMessage()
}


func  (qm *QuotaManager) getAppWrappers(preemptIds []string) []*arbv1.AppWrapper{
	var aws []*arbv1.AppWrapper
	if len(preemptIds) <= 0 {
		return nil
	}

	for _, preemptId := range preemptIds {
		awNamespace, awName := util.ParseId(preemptId)
		if len(awNamespace) <= 0 || len(awName) <= 0 {
			klog.Errorf("[getAppWrappers] Failed to parse AppWrapper id from quota manager, parse string: %s.  Preemption of this Id will be ignored.", preemptId)
			continue
		}
		aw, e := qm.appwrapperLister.AppWrappers(awNamespace).Get(awName)
		if e != nil {
			klog.Errorf("[getAppWrappers] Failed to get AppWrapper from API Cache %s/%s, err=%v.  Preemption of this Id will be ignored.",
				awNamespace, awName, e)
			continue
		}
		aws = append(aws, aw)
	}

	// Final validation check
	if len(preemptIds) != len(aws) {
		klog.Warningf("[getAppWrappers] Preemption list size of %d from quota manager does not match size of generated list of AppWrapper: %d", len(preemptIds), len(aws))
	}
	return aws
}
func (qm *QuotaManager) Release(aw *arbv1.AppWrapper) bool {

	released := false

	// Handle uninitialized quota manager
	if qm.quotaManagerBackend == nil {
		klog.Errorf("[Release] No quota manager backend exists, Quota release %s/%s fails quota by default.",
								aw.Name, aw.Namespace)
		return released
	}

	awId := util.CreateId(aw.Namespace, aw.Name)
	if len(awId) <= 0 {
		klog.Errorf("[Release] Request failed due to invalid AppWrapper due to empty namespace: %s or name:%s.", aw.Namespace, aw.Name)
		return released
	}

	released = qm.quotaManagerBackend.DeAllocateForest(QuotaManagerForestName, awId)

	if !released {
		klog.Errorf("[Release] Quota release for %s/%s failed.",
			aw.Namespace, aw.Name)
	} else {
		klog.V(8).Infof("[Release] Quota release for %s/%s successful.",
			aw.Namespace, aw.Name)
	}

	// Remove Consumer Request
	success, err := qm.quotaManagerBackend.RemoveConsumer(awId)
	if err != nil {
		klog.Errorf("[Release] Error removing Quota request definition id: %s for AppWrapper %s/%s, err=%#v.",
			awId, aw.Namespace, aw.Name, err)
	}

	if success {
		klog.V(8).Infof("[Release] Quota request definition for %s/%s successful.",
			aw.Namespace, aw.Name)

	} else {
		klog.Warningf("[Release] Removing Quota request definition for %s/%s unsuccessful.",
			aw.Namespace, aw.Name)
	}

	return released
}
