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

package pod

import (
	"context"
	"sync"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog"

	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/conversion"
	mc "github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/mccontroller"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/reconciler"
)

const (
	// Default grace period in seconds
	minimumGracePeriodInSeconds = 30
)

// StartPeriodChecker starts the period checker for data consistency check. Checker is
// blocking so should be called via a goroutine.
func (c *controller) StartPeriodChecker(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()

	// Start a loop to periodically check if pods keep consistency between super
	// master and tenant masters.
	wait.Until(c.checkPods, c.periodCheckerPeriod, stopCh)
}

// checkPods checks to see if pods in super master informer cache and tenant master
// keep consistency.
func (c *controller) checkPods() {
	clusterNames := c.multiClusterPodController.GetClusterNames()
	if len(clusterNames) == 0 {
		klog.Infof("tenant masters has no clusters, give up period checker")
		return
	}

	wg := sync.WaitGroup{}

	for _, clusterName := range clusterNames {
		cluster := c.multiClusterPodController.GetCluster(clusterName)
		if cluster == nil {
			klog.Errorf("failed to locate cluster %s", clusterName)
			continue
		}

		wg.Add(1)
		go func(clusterName string, cluster mc.ClusterInterface) {
			defer wg.Done()
			c.checkPodsOfCluster(clusterName, cluster)
		}(clusterName, cluster)
	}
	wg.Wait()

	pPods, err := c.podLister.List(labels.Everything())
	if err != nil {
		klog.Errorf("error listing pods from super master informer cache: %v", err)
		return
	}

	for _, pPod := range pPods {
		clusterName, vNamespace := conversion.GetVirtualOwner(pPod)
		if len(clusterName) == 0 || len(vNamespace) == 0 {
			continue
		}

		vPodObj, err := c.multiClusterPodController.Get(clusterName, vNamespace, pPod.Name)
		if err != nil {
			if errors.IsNotFound(err) {
				if pPod.DeletionTimestamp != nil {
					// pPod is under deletion, waiting for UWS bock populate the pod status.
					continue
				}
				// vPod not found and pPod not under deletion, we need to delete pPod manually
				gracePeriod := int64(minimumGracePeriodInSeconds)
				if pPod.Spec.TerminationGracePeriodSeconds != nil {
					gracePeriod = *pPod.Spec.TerminationGracePeriodSeconds
				}
				deleteOptions := metav1.NewDeleteOptions(gracePeriod)
				deleteOptions.Preconditions = metav1.NewUIDPreconditions(string(pPod.UID))
				if err = c.client.Pods(pPod.Namespace).Delete(pPod.Name, deleteOptions); err != nil {
					klog.Errorf("error deleting pPod %v/%v in super master: %v", pPod.Namespace, pPod.Name, err)
				}
			}
		}
		vPod := vPodObj.(*v1.Pod)

		// pod has been updated by super master
		if !equality.Semantic.DeepEqual(vPod.Status, pPod.Status) {
			klog.Warningf("status of pod %v/%v diff in super&tenant master", pPod.Namespace, pPod.Name)
		}
	}
}

// checkPodsOfCluster checks to see if pods in specific cluster keeps consistency.
func (c *controller) checkPodsOfCluster(clusterName string, cluster mc.ClusterInterface) {
	clusterInformerCache, err := cluster.GetCache()
	if err != nil {
		klog.Errorf("failed to get informer cache for cluster %s", clusterName)
		return
	}
	podList := &v1.PodList{}
	err = clusterInformerCache.List(context.TODO(), podList)
	if err != nil {
		klog.Errorf("error listing pods from cluster informer cache: %v", err)
		return
	}

	klog.Infof("check pods consistency in cluster %s", clusterName)
	for _, vPod := range podList.Items {
		targetNamespace := conversion.ToSuperMasterNamespace(clusterName, vPod.Namespace)
		pPod, err := c.podLister.Pods(targetNamespace).Get(vPod.Name)
		if errors.IsNotFound(err) {
			// pPod not found and vPod is under deletion, we need to delete vPod manually
			if vPod.DeletionTimestamp != nil {
				clusterClient, err := cluster.GetClient()
				if err != nil {
					klog.Errorf("error getting cluster %s clientset: %v", clusterName, err)
					continue
				}
				// since pPod not found in super master, we can force delete vPod
				deleteOptions := metav1.NewDeleteOptions(0)
				deleteOptions.Preconditions = metav1.NewUIDPreconditions(string(vPod.UID))
				if err = clusterClient.CoreV1().Pods(vPod.Namespace).Delete(vPod.Name, deleteOptions); err != nil {
					klog.Errorf("error deleting pod %v/%v in cluster %s: %v", vPod.Namespace, vPod.Name, clusterName, err)
				}
			} else {
				// pPod not found and vPod still exists, we need to create pPod again
				c.multiClusterPodController.RequeueObject(clusterName, vPod, reconciler.AddEvent)
			}
			continue
		}

		if err != nil {
			klog.Errorf("error getting pPod %s/%s from super master cache: %v", targetNamespace, vPod.Name, err)
			continue
		}

		updatedPod := conversion.CheckPodEquality(pPod, &vPod)
		if updatedPod != nil {
			klog.Warningf("spec of pod %v/%v diff in super&tenant master", vPod.Namespace, vPod.Name)
		}
	}
}
