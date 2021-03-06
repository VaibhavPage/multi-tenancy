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
	"fmt"
	"time"

	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	clientset "k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog"

	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/constants"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/conversion"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/metrics"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/reconciler"
	"github.com/kubernetes-sigs/multi-tenancy/incubator/virtualcluster/pkg/syncer/utils"
)

func (c *controller) StartDWS(stopCh <-chan struct{}) error {
	if !cache.WaitForCacheSync(stopCh, c.podSynced, c.serviceSynced, c.nsSynced) {
		return fmt.Errorf("failed to wait for caches to sync before starting Pod dws")
	}
	return c.multiClusterPodController.Start(stopCh)
}

func (c *controller) Reconcile(request reconciler.Request) (reconciler.Result, error) {
	klog.Infof("reconcile pod %s/%s %s event for cluster %s", request.Namespace, request.Name, request.Event, request.Cluster.Name)

	var operation string
	switch request.Event {
	case reconciler.AddEvent:
		operation = "pod_add"
		defer recordOperation(operation, time.Now())
		err := c.reconcilePodCreate(request.Cluster.Name, request.Namespace, request.Name, request.Obj.(*v1.Pod))
		recordError(operation, err)
		if err != nil {
			klog.Errorf("failed reconcile pod %s/%s CREATE of cluster %s %v", request.Namespace, request.Name, request.Cluster.Name, err)
			return reconciler.Result{Requeue: true}, err
		}
	case reconciler.UpdateEvent:
		operation = "pod_update"
		defer recordOperation(operation, time.Now())
		err := c.reconcilePodUpdate(request.Cluster.Name, request.Namespace, request.Name, request.Obj.(*v1.Pod))
		recordError(operation, err)
		if err != nil {
			klog.Errorf("failed reconcile pod %s/%s UPDATE of cluster %s %v", request.Namespace, request.Name, request.Cluster.Name, err)
			return reconciler.Result{Requeue: true}, err
		}
	case reconciler.DeleteEvent:
		operation = "pod_delete"
		defer recordOperation(operation, time.Now())
		err := c.reconcilePodRemove(request.Cluster.Name, request.Namespace, request.Name, request.Obj.(*v1.Pod))
		recordError(operation, err)
		if err != nil {
			klog.Errorf("failed reconcile pod %s/%s DELETE of cluster %s %v", request.Namespace, request.Name, request.Cluster.Name, err)
			return reconciler.Result{Requeue: true}, err
		}
	}
	return reconciler.Result{}, nil
}

func (c *controller) reconcilePodCreate(cluster, namespace, name string, vPod *v1.Pod) error {
	// load deleting pod, don't create any pod on super master.
	if vPod.DeletionTimestamp != nil {
		return c.reconcilePodUpdate(cluster, namespace, name, vPod)
	}

	targetNamespace := conversion.ToSuperMasterNamespace(cluster, namespace)
	_, err := c.podLister.Pods(targetNamespace).Get(name)
	if err == nil {
		return c.reconcilePodUpdate(cluster, namespace, name, vPod)
	}

	newObj, err := conversion.BuildMetadata(cluster, targetNamespace, vPod)
	if err != nil {
		return err
	}

	pPod := newObj.(*v1.Pod)

	// check if the secret in super master is ready
	// we must create pod after sync the secret.
	saName := "default"
	if pPod.Spec.ServiceAccountName != "" {
		saName = pPod.Spec.ServiceAccountName
	}

	pSecret, err := utils.GetSecret(c.client, targetNamespace, saName)
	if err != nil {
		return fmt.Errorf("failed to get secret: %v", err)
	}

	if pSecret.Labels[constants.SyncStatusKey] != constants.SyncStatusReady {
		return fmt.Errorf("secret for pod is not ready")
	}

	var client *clientset.Clientset
	tenantCluster := c.multiClusterPodController.GetCluster(cluster)
	if tenantCluster == nil {
		klog.Infof("cluster %s is gone", cluster)
		return nil
	}
	client, err = tenantCluster.GetClient()
	if err != nil {
		return err
	}
	vSecret, err := utils.GetSecret(client.CoreV1(), namespace, saName)
	if err != nil {
		return fmt.Errorf("failed to get secret: %v", err)
	}

	services, err := c.getPodRelatedServices(cluster, pPod)
	if err != nil {
		return fmt.Errorf("failed to list services from cluster %s cache: %v", cluster, err)
	}

	if len(services) == 0 {
		return fmt.Errorf("service is not ready")
	}

	nameServer, err := c.getClusterNameServer(c.client, cluster)
	if err != nil {
		return fmt.Errorf("nameserver not found: %v", err)
	}

	err = conversion.VC(tenantCluster).Pod(pPod).Mutate(vPod, vSecret, pSecret, services, nameServer)
	if err != nil {
		return fmt.Errorf("failed to mutate pod: %v", err)
	}

	_, err = c.client.Pods(targetNamespace).Create(pPod)
	if errors.IsAlreadyExists(err) {
		klog.Infof("pod %s/%s of cluster %s already exist in super master", namespace, name, cluster)
		return nil
	}
	return err
}

func (c *controller) getClusterNameServer(client v1core.ServicesGetter, cluster string) (string, error) {
	svc, err := client.Services(conversion.ToSuperMasterNamespace(cluster, constants.TenantDNSServerNS)).Get(constants.TenantDNSServerServiceName, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}

	return svc.Spec.ClusterIP, nil
}

func (c *controller) getPodRelatedServices(cluster string, pPod *v1.Pod) ([]*v1.Service, error) {
	var services []*v1.Service
	list, err := c.informer.Services().Lister().Services(conversion.ToSuperMasterNamespace(cluster, metav1.NamespaceDefault)).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	services = append(services, list...)

	list, err = c.informer.Services().Lister().Services(pPod.Namespace).List(labels.Everything())
	if err != nil {
		return nil, err
	}
	services = append(services, list...)

	return services, nil
}

func (c *controller) reconcilePodUpdate(cluster, namespace, name string, vPod *v1.Pod) error {
	targetNamespace := conversion.ToSuperMasterNamespace(cluster, namespace)
	pPod, err := c.podLister.Pods(targetNamespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// if the pod on super master has been deleted and syncer has not
			// deleted virtual pod with 0 grace period second successfully.
			// we depends on periodic check to do gc.
			return nil
		}
		return err
	}

	if vPod.DeletionTimestamp != nil {
		if pPod.DeletionTimestamp != nil {
			// pPod is under deletion, waiting for UWS bock populate the pod status.
			return nil
		}
		deleteOptions := metav1.NewDeleteOptions(*vPod.DeletionGracePeriodSeconds)
		deleteOptions.Preconditions = metav1.NewUIDPreconditions(string(pPod.UID))
		err = c.client.Pods(targetNamespace).Delete(name, deleteOptions)
		if errors.IsNotFound(err) {
			return nil
		}
		return err
	}

	updatedPod := conversion.CheckPodEquality(pPod, vPod)
	if updatedPod != nil {
		pPod, err = c.client.Pods(targetNamespace).Update(updatedPod)
		if err != nil {
			return err
		}
	}

	// pod has been updated by tenant controller
	if !equality.Semantic.DeepEqual(vPod.Status, pPod.Status) {
		c.enqueuePod(pPod)
	}

	return nil
}

func (c *controller) reconcilePodRemove(cluster, namespace, name string, vPod *v1.Pod) error {
	targetNamespace := conversion.ToSuperMasterNamespace(cluster, namespace)
	opts := &metav1.DeleteOptions{
		PropagationPolicy: &constants.DefaultDeletionPolicy,
	}
	err := c.client.Pods(targetNamespace).Delete(name, opts)
	if errors.IsNotFound(err) {
		klog.Warningf("pod %s/%s of cluster (%s) is not found in super master", namespace, name, cluster)
		return nil
	}
	return err
}

func recordOperation(operation string, start time.Time) {
	metrics.PodOperations.WithLabelValues(operation).Inc()
	metrics.PodOperationsDuration.WithLabelValues(operation).Observe(metrics.SinceInSeconds(start))
}

func recordError(operation string, err error) {
	if err != nil {
		metrics.PodOperationsErrors.WithLabelValues(operation).Inc()
	}
}
