package cluster

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/karmada-io/karmada/pkg/apis/cluster/v1alpha1"
	"github.com/karmada-io/karmada/pkg/util"
	"github.com/karmada-io/karmada/pkg/util/names"
)

const (
	// ControllerName is the controller name that will be used when reporting events.
	ControllerName           = "cluster-controller"
	executionSpaceLabelKey   = "karmada.io/executionspace"
	executionSpaceLabelValue = ""
)

// Controller is to sync Cluster.
type Controller struct {
	client.Client                      // used to operate Cluster resources.
	KubeClientSet kubernetes.Interface // used to get kubernetes resources.
	EventRecorder record.EventRecorder
}

// Reconcile performs a full reconciliation for the object referred to by the Request.
// The Controller will requeue the Request to be processed again if an error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
func (c *Controller) Reconcile(req controllerruntime.Request) (controllerruntime.Result, error) {
	klog.V(4).Infof("Reconciling cluster %s", req.NamespacedName.Name)

	cluster := &v1alpha1.Cluster{}
	if err := c.Client.Get(context.TODO(), req.NamespacedName, cluster); err != nil {
		// The resource may no longer exist, in which case we stop processing.
		if errors.IsNotFound(err) {
			return controllerruntime.Result{}, nil
		}

		return controllerruntime.Result{Requeue: true}, err
	}

	if !cluster.DeletionTimestamp.IsZero() {
		return c.removeCluster(cluster)
	}

	return c.syncCluster(cluster)
}

// SetupWithManager creates a controller and register to controller manager.
func (c *Controller) SetupWithManager(mgr controllerruntime.Manager) error {
	return controllerruntime.NewControllerManagedBy(mgr).For(&v1alpha1.Cluster{}).Complete(c)
}

func (c *Controller) syncCluster(cluster *v1alpha1.Cluster) (controllerruntime.Result, error) {
	// create execution space
	err := c.createExecutionSpace(cluster)
	if err != nil {
		return controllerruntime.Result{Requeue: true}, err
	}

	// ensure finalizer
	return c.ensureFinalizer(cluster)
}

func (c *Controller) removeCluster(cluster *v1alpha1.Cluster) (controllerruntime.Result, error) {
	err := c.removeExecutionSpace(cluster)
	if apierrors.IsNotFound(err) {
		return c.removeFinalizer(cluster)
	}
	if err != nil {
		klog.Errorf("Failed to remove execution space %v, err is %v", cluster.Name, err)
		return controllerruntime.Result{Requeue: true}, err
	}

	// make sure the given execution space has been deleted
	existES, err := c.ensureRemoveExecutionSpace(cluster)
	if err != nil {
		klog.Errorf("Failed to check weather the execution space exist in the given member cluster or not, error is: %v", err)
		return controllerruntime.Result{Requeue: true}, err
	} else if existES {
		return controllerruntime.Result{Requeue: true}, fmt.Errorf("requeuing operation until the execution space %v deleted, ", cluster.Name)
	}

	return c.removeFinalizer(cluster)
}

// removeExecutionSpace delete the given execution space
func (c *Controller) removeExecutionSpace(cluster *v1alpha1.Cluster) error {
	executionSpace, err := names.GenerateExecutionSpaceName(cluster.Name)
	if err != nil {
		klog.Errorf("Failed to generate execution space name for member cluster %s, err is %v", cluster.Name, err)
		return err
	}

	if err := c.KubeClientSet.CoreV1().Namespaces().Delete(context.TODO(), executionSpace, v1.DeleteOptions{}); err != nil {
		klog.Errorf("Error while deleting namespace %s: %s", executionSpace, err)
		return err
	}
	return nil
}

// ensureRemoveExecutionSpace make sure the given execution space has been deleted
func (c *Controller) ensureRemoveExecutionSpace(cluster *v1alpha1.Cluster) (bool, error) {
	executionSpace, err := names.GenerateExecutionSpaceName(cluster.Name)
	if err != nil {
		klog.Errorf("Failed to generate execution space name for member cluster %s, err is %v", cluster.Name, err)
		return false, err
	}

	_, err = c.KubeClientSet.CoreV1().Namespaces().Get(context.TODO(), executionSpace, v1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		klog.Errorf("Failed to get execution space %v, err is %v ", executionSpace, err)
		return false, err
	}
	return true, nil
}

func (c *Controller) removeFinalizer(cluster *v1alpha1.Cluster) (controllerruntime.Result, error) {
	if !controllerutil.ContainsFinalizer(cluster, util.ClusterControllerFinalizer) {
		return controllerruntime.Result{}, nil
	}

	controllerutil.RemoveFinalizer(cluster, util.ClusterControllerFinalizer)
	err := c.Client.Update(context.TODO(), cluster)
	if err != nil {
		return controllerruntime.Result{Requeue: true}, err
	}

	return controllerruntime.Result{}, nil
}

func (c *Controller) ensureFinalizer(cluster *v1alpha1.Cluster) (controllerruntime.Result, error) {
	if controllerutil.ContainsFinalizer(cluster, util.ClusterControllerFinalizer) {
		return controllerruntime.Result{}, nil
	}

	controllerutil.AddFinalizer(cluster, util.ClusterControllerFinalizer)
	err := c.Client.Update(context.TODO(), cluster)
	if err != nil {
		return controllerruntime.Result{Requeue: true}, err
	}

	return controllerruntime.Result{}, nil
}

// createExecutionSpace create member cluster execution space when member cluster joined
func (c *Controller) createExecutionSpace(cluster *v1alpha1.Cluster) error {
	executionSpace, err := names.GenerateExecutionSpaceName(cluster.Name)
	if err != nil {
		klog.Errorf("Failed to generate execution space name for member cluster %s, err is %v", cluster.Name, err)
		return err
	}

	// create member cluster execution space when member cluster joined
	_, err = c.KubeClientSet.CoreV1().Namespaces().Get(context.TODO(), executionSpace, v1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			clusterES := &corev1.Namespace{
				ObjectMeta: v1.ObjectMeta{
					Name:   executionSpace,
					Labels: map[string]string{executionSpaceLabelKey: executionSpaceLabelValue},
				},
			}
			_, err = c.KubeClientSet.CoreV1().Namespaces().Create(context.TODO(), clusterES, v1.CreateOptions{})
			if err != nil {
				klog.Errorf("Failed to create execution space for cluster %v", cluster.Name)
				return err
			}
		} else {
			klog.Errorf("Could not get %s namespace: %v", executionSpace, err)
			return err
		}
	}
	return nil
}
