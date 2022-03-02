package reconcilers

import (
	"context"
	"fmt"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"opensearch.opster.io/opensearch-gateway/services"
	"opensearch.opster.io/pkg/builders"

	"github.com/banzaicloud/operator-tools/pkg/reconciler"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/tools/record"
	opsterv1 "opensearch.opster.io/api/v1"
	"opensearch.opster.io/pkg/helpers"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type ScalerReconciler struct {
	client.Client
	reconciler.ResourceReconciler
	ctx               context.Context
	recorder          record.EventRecorder
	reconcilerContext *ReconcilerContext
	instance          *opsterv1.OpenSearchCluster
}

func NewScalerReconciler(
	client client.Client,
	ctx context.Context,
	recorder record.EventRecorder,
	reconcilerContext *ReconcilerContext,
	instance *opsterv1.OpenSearchCluster,
	opts ...reconciler.ResourceReconcilerOption,
) *ScalerReconciler {
	return &ScalerReconciler{
		Client: client,
		ResourceReconciler: reconciler.NewReconcilerWith(client,
			append(opts, reconciler.WithLog(log.FromContext(ctx).WithValues("reconciler", "scaler")))...),
		ctx:               ctx,
		recorder:          recorder,
		reconcilerContext: reconcilerContext,
		instance:          instance,
	}
}

func (r *ScalerReconciler) Reconcile() (ctrl.Result, error) {
	for _, nodePool := range r.instance.Spec.NodePools {
		err := r.reconcileNodePool(&nodePool)
		if err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{}, nil
}

func (r *ScalerReconciler) reconcileNodePool(nodePool *opsterv1.NodePool) error {
	namespace := r.instance.Spec.General.ClusterName
	sts_name := builders.StsName(r.instance, nodePool)
	currentSts := appsv1.StatefulSet{}
	if err := r.Get(context.TODO(), client.ObjectKey{Name: sts_name, Namespace: namespace}, &currentSts); err != nil {
		return err
	}

	var desireReplicaDiff = *currentSts.Spec.Replicas - nodePool.Replicas
	if desireReplicaDiff == 0 {
		return nil
	}
	componentStatus := opsterv1.ComponentStatus{
		Component:   "Scaler",
		Description: nodePool.Component,
	}
	comp := r.instance.Status.ComponentsStatus
	currentStatus, found := helpers.FindFirstPartial(comp, componentStatus, getByDescriptionAndGroup)
	if !found {
		if desireReplicaDiff > 0 {
			if !r.instance.Spec.ConfMgmt.SmartScaler {
				status, err := r.decreaseOneNode(currentStatus, currentSts, nodePool.Component, r.instance.Spec.ConfMgmt.SmartScaler)
				helpers.Replace(currentStatus, *status, r.instance.Status.ComponentsStatus)
				return err
			}
			status, err := r.excludeNode(context.TODO(), currentStatus, currentSts, nodePool.Component)
			helpers.Replace(currentStatus, *status, r.instance.Status.ComponentsStatus)
			return err

		}
		if desireReplicaDiff < 0 {
			status, err := r.increaseOneNode(currentSts, nodePool.Component)
			helpers.Replace(currentStatus, *status, r.instance.Status.ComponentsStatus)
			return err
		}
	}
	if currentStatus.Status == "Excluded" {
		status, err := r.drainNode(context.TODO(), currentStatus, currentSts, nodePool.Component)
		helpers.Replace(currentStatus, *status, r.instance.Status.ComponentsStatus)
		return err
	}
	if currentStatus.Status == "Drained" {
		status, err := r.decreaseOneNode(currentStatus, currentSts, nodePool.Component, r.instance.Spec.ConfMgmt.SmartScaler)
		helpers.Replace(currentStatus, *status, r.instance.Status.ComponentsStatus)
		return err
	}
	return nil
}

func (r *ScalerReconciler) increaseOneNode(currentSts appsv1.StatefulSet, nodePoolGroupName string) (*opsterv1.ComponentStatus, error) {
	lg := log.FromContext(r.ctx)
	*currentSts.Spec.Replicas++
	lastReplicaNodeName := fmt.Sprintf("%s-%d", currentSts.ObjectMeta.Name, currentSts.Spec.Replicas)
	_, err := r.ReconcileResource(&currentSts, reconciler.StatePresent)
	if err != nil {
		r.recorder.Event(r.instance, "Normal", "failed to add node ", fmt.Sprintf("Group name-%s . Failed to add node %s", currentSts.Name, lastReplicaNodeName))
		return nil, err
	}
	lg.Info(fmt.Sprintf("Group-%s . added node %s", nodePoolGroupName, lastReplicaNodeName))
	return nil, nil
}

func (r *ScalerReconciler) decreaseOneNode(currentStatus opsterv1.ComponentStatus, currentSts appsv1.StatefulSet, nodePoolGroupName string, smartDecrease bool) (*opsterv1.ComponentStatus, error) {
	lg := log.FromContext(r.ctx)
	*currentSts.Spec.Replicas--
	lastReplicaNodeName := fmt.Sprintf("%s-%d", currentSts.ObjectMeta.Name, *currentSts.Spec.Replicas)
	_, err := r.ReconcileResource(&currentSts, reconciler.StatePresent)
	if err != nil {
		r.recorder.Event(r.instance, "Normal", "failed to remove node ", fmt.Sprintf("Group-%s . Failed to remove node %s", nodePoolGroupName, lastReplicaNodeName))
		lg.Error(err, fmt.Sprintf("failed to remove node %s", lastReplicaNodeName))
		return nil, err
	}
	lg.Info(fmt.Sprintf("Group-%s . removed node %s", nodePoolGroupName, lastReplicaNodeName))
	r.instance.Status.ComponentsStatus = helpers.RemoveIt(currentStatus, r.instance.Status.ComponentsStatus)
	if !smartDecrease {
		return nil, err
	}
	username, password := builders.UsernameAndPassword(r.instance)
	service, created, err := r.CreateNodePortServiceIfNotExists()
	if err != nil {
		return nil, err
	}
	clusterClient, err := services.NewOsClusterClient(builders.URLForCluster(r.instance), username, password)
	if err != nil {
		lg.Error(err, "failed to create os client")
		r.recorder.Event(r.instance, "WARN", "failed to remove node exclude", fmt.Sprintf("Group-%s . failed to remove node exclude %s", nodePoolGroupName, lastReplicaNodeName))
		if created {
			r.DeleteNodePortService(service)
		}
		return nil, err
	}
	success, err := services.RemoveExcludeNodeHost(clusterClient, lastReplicaNodeName)
	if !success || err != nil {
		lg.Error(err, fmt.Sprintf("failed to remove exclude node %s", lastReplicaNodeName))
		r.recorder.Event(r.instance, "WARN", "failed to remove node exclude", fmt.Sprintf("Group-%s . failed to remove node exclude %s", nodePoolGroupName, lastReplicaNodeName))
	}
	if created {
		r.DeleteNodePortService(service)
	}
	return nil, err
}

func (r *ScalerReconciler) excludeNode(ctx context.Context, currentStatus opsterv1.ComponentStatus, currentSts appsv1.StatefulSet, nodePoolGroupName string) (*opsterv1.ComponentStatus, error) {
	lg := log.FromContext(r.ctx)
	username, password := builders.UsernameAndPassword(r.instance)
	service, created, err := r.CreateNodePortServiceIfNotExists()
	if err != nil {
		return nil, err
	}
	clusterClient, err := services.NewOsClusterClient(fmt.Sprintf("https://localhost:%d", service.Spec.Ports[0].NodePort), username, password)
	if err != nil {
		lg.Error(err, "failed to create os client")
		if created {
			r.DeleteNodePortService(service)
		}
		return nil, err
	}
	// -----  Now start remove node ------
	lastReplicaNodeName := fmt.Sprintf("%s-%d", currentSts.ObjectMeta.Name, *currentSts.Spec.Replicas-1)

	excluded, err := services.AppendExcludeNodeHost(clusterClient, lastReplicaNodeName)
	if err != nil {
		lg.Error(err, fmt.Sprintf("failed to exclude node %s", lastReplicaNodeName))
		if created {
			r.DeleteNodePortService(service)
		}
		return nil, err
	}
	if err := r.Update(ctx, &currentSts); err != nil {
		return nil, err
	}
	if excluded {
		componentStatus := opsterv1.ComponentStatus{
			Component:   "Scaler",
			Status:      "Excluded",
			Description: nodePoolGroupName,
		}
		lg.Info(fmt.Sprintf("Group-%s .excluded node %s", nodePoolGroupName, lastReplicaNodeName))
		r.instance.Status.ComponentsStatus = helpers.Replace(currentStatus, componentStatus, r.instance.Status.ComponentsStatus)
		err = r.Status().Update(ctx, r.instance)
		if created {
			r.DeleteNodePortService(service)
		}
		return &componentStatus, err
	}

	componentStatus := opsterv1.ComponentStatus{
		Component:   "Scaler",
		Status:      "Running",
		Description: nodePoolGroupName,
	}
	lg.Info(fmt.Sprintf("Group-%s . Failed to exclude node %s", nodePoolGroupName, lastReplicaNodeName))
	r.instance.Status.ComponentsStatus = helpers.Replace(currentStatus, componentStatus, r.instance.Status.ComponentsStatus)
	err = r.Status().Update(ctx, r.instance)
	if created {
		r.DeleteNodePortService(service)
	}
	return &componentStatus, err
}

func (r *ScalerReconciler) drainNode(ctx context.Context, currentStatus opsterv1.ComponentStatus, currentSts appsv1.StatefulSet, nodePoolGroupName string) (*opsterv1.ComponentStatus, error) {
	lg := log.FromContext(r.ctx)
	lastReplicaNodeName := fmt.Sprintf("%s-%d", currentSts.ObjectMeta.Name, *currentSts.Spec.Replicas-1)

	username, password := builders.UsernameAndPassword(r.instance)
	service, created, err := r.CreateNodePortServiceIfNotExists()
	if err != nil {
		return nil, err
	}
	clusterClient, err := services.NewOsClusterClient(fmt.Sprintf("https://localhost:%d", service.Spec.Ports[0].NodePort), username, password)
	if err != nil {
		if created {
			r.DeleteNodePortService(service)
		}
		return nil, err
	}
	nodeNotEmpty, err := services.HasShardsOnNode(clusterClient, lastReplicaNodeName)
	if nodeNotEmpty {
		lg.Info(fmt.Sprintf("Group-%s . draining node %s", nodePoolGroupName, lastReplicaNodeName))
		return nil, err
	}
	success, err := services.RemoveExcludeNodeHost(clusterClient, lastReplicaNodeName)
	if !success {
		r.recorder.Event(r.instance, "Normal", "node is empty but node is still excluded from allocation", fmt.Sprintf("Group-%s . node %s node is empty but node is still excluded from allocation", nodePoolGroupName, lastReplicaNodeName))
		return nil, err
	}

	componentStatus := opsterv1.ComponentStatus{
		Component:   "Scaler",
		Status:      "Drained",
		Description: nodePoolGroupName,
	}
	lg.Info(fmt.Sprintf("Group-%s .node %s node is drained", nodePoolGroupName, lastReplicaNodeName))
	r.instance.Status.ComponentsStatus = helpers.Replace(currentStatus, componentStatus, r.instance.Status.ComponentsStatus)
	err = r.Status().Update(ctx, r.instance)
	if created {
		r.DeleteNodePortService(service)
	}
	return &componentStatus, err
}

func getByDescriptionAndGroup(left opsterv1.ComponentStatus, right opsterv1.ComponentStatus) (opsterv1.ComponentStatus, bool) {
	if left.Description == right.Description && left.Component == right.Component {
		return left, true
	}
	return right, false
}

func (r *ScalerReconciler) CreateNodePortServiceIfNotExists() (corev1.Service, bool, error) {
	lg := log.FromContext(r.ctx)
	namespace := r.instance.Spec.General.ClusterName
	targetService := builders.NewNodePortService(r.instance)
	existingService := corev1.Service{}
	if err := r.Get(context.TODO(), client.ObjectKey{Name: targetService.Name, Namespace: namespace}, &existingService); err != nil {
		err = r.Create(context.TODO(), targetService)
		if err != nil {
			if !errors.IsAlreadyExists(err) {
				lg.Error(err, "Cannot create service")
				r.recorder.Event(r.instance, "Warning", "Cannot create service", "Requeue - Fix the problem you have on main Opensearch Headless Service ")
				return *targetService, false, err
			}
		}
		lg.Info("service created successfully")
		return *targetService, true, nil
	}
	return existingService, false, nil
}

func (r *ScalerReconciler) DeleteNodePortService(service corev1.Service) {
	lg := log.FromContext(r.ctx)
	err := r.Delete(context.TODO(), &service)
	if err != nil {
		lg.Error(err, "Cannot delete service")
		r.recorder.Event(r.instance, "Warning", "Cannot delete service", "Requeue - Fix the problem you have on main Opensearch Headless Service ")
	}
}