/*
Copyright 2021 The Kubernetes Authors.

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

package controlplane

import (
	"context"

	"github.com/go-logr/logr"
	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1 "sigs.k8s.io/cluster-api-provider-nested/apis/controlplane/v1alpha4"
	controlplanev1alpha4 "sigs.k8s.io/cluster-api-provider-nested/apis/controlplane/v1alpha4"
)

// NestedControllerManagerReconciler reconciles a NestedControllerManager object
type NestedControllerManagerReconciler struct {
	client.Client
	Log    logr.Logger
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=nestedcontrollermanagers,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=nestedcontrollermanagers/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=nestedcontrollermanagers/finalizers,verbs=update
//+kubebuilder:rbac:groups=apps,resources=statefulset,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=apps,resources=statefulset/status,verbs=get;update;patch

func (r *NestedControllerManagerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("nestedcontrollermanager", req.NamespacedName)
	log.Info("Reconciling NestedControllerManager...")
	var nkcm clusterv1.NestedControllerManager
	if err := r.Get(ctx, req.NamespacedName, &nkcm); err != nil {
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}
	log.Info("creating NestedControllerManager",
		"namespace", nkcm.GetNamespace(),
		"name", nkcm.GetName())

	// 1. check if the ownerreference has been set by the
	// NestedControlPlane controller.
	owner := getOwner(nkcm.ObjectMeta)
	if owner == (metav1.OwnerReference{}) {
		// requeue the request if the owner NestedControlPlane has
		// not been set yet.
		log.Info("the owner has not been set yet, will retry later",
			"namespace", nkcm.GetNamespace(),
			"name", nkcm.GetName())
		return ctrl.Result{Requeue: true}, nil
	}

	// 2. create the NestedControllerManager StatefulSet if not found
	var nkcmSts appsv1.StatefulSet
	if err := r.Get(ctx, types.NamespacedName{
		Namespace: nkcm.GetNamespace(),
		Name:      nkcm.GetName(),
	}, &nkcmSts); err != nil {
		if apierrors.IsNotFound(err) {
			// as the statefulset is not found, mark the NestedControllerManager
			// as unready
			if IsComponentReady(nkcm.Status.CommonStatus) {
				nkcm.Status.Phase =
					string(clusterv1.Unready)
				log.V(5).Info("The corresponding statefulset is not found, " +
					"will mark the NestedControllerManager as unready")
				if err := r.Status().Update(ctx, &nkcm); err != nil {
					log.Error(err, "fail to update the status of the NestedControllerManager Object")
					return ctrl.Result{}, err
				}
			}
			// the statefulset is not found, create one
			if err := createNestedComponentSts(ctx,
				r.Client, nkcm.ObjectMeta, nkcm.Spec.NestedComponentSpec,
				clusterv1.ControllerManager, owner.Name, log); err != nil {
				log.Error(err, "fail to create NestedControllerManager StatefulSet")
				return ctrl.Result{}, err
			}
			log.Info("successfully create the NestedControllerManager StatefulSet")
			return ctrl.Result{}, nil
		}
		log.Error(err, "fail to get NestedControllerManager StatefulSet")
		return ctrl.Result{}, err
	}

	// 3. reconcile the NestedControllerManager based on the status of the StatefulSet.
	// Mark the NestedControllerManager as Ready if the StatefulSet is ready
	if nkcmSts.Status.ReadyReplicas == nkcmSts.Status.Replicas {
		log.Info("The NestedControllerManager StatefulSet is ready")
		if IsComponentReady(nkcm.Status.CommonStatus) {
			// As the NestedControllerManager StatefulSet is ready, update
			// NestedControllerManager status
			nkcm.Status.Phase = string(clusterv1.Ready)
			log.V(5).Info("The corresponding statefulset is ready, " +
				"will mark the NestedControllerManager as ready")
			if err := r.Status().Update(ctx, &nkcm); err != nil {
				log.Error(err, "fail to update NestedControllerManager Object")
				return ctrl.Result{}, err
			}
			log.Info("Successfully set the NestedControllerManager object to ready")
		}
		return ctrl.Result{}, nil
	}

	// mark the NestedControllerManager as unready, if the NestedControllerManager
	// StatefulSet is unready,
	if IsComponentReady(nkcm.Status.CommonStatus) {
		nkcm.Status.Phase = string(clusterv1.Unready)
		if err := r.Status().Update(ctx, &nkcm); err != nil {
			log.Error(err, "fail to update NestedControllerManager Object")
			return ctrl.Result{}, err
		}
		log.Info("Successfully set the NestedControllerManager object to unready")
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *NestedControllerManagerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if err := mgr.GetFieldIndexer().IndexField(context.TODO(),
		&appsv1.StatefulSet{},
		statefulsetOwnerKey,
		func(rawObj ctrlcli.Object) []string {
			// grab the statefulset object, extract the owner
			sts := rawObj.(*appsv1.StatefulSet)
			owner := metav1.GetControllerOf(sts)
			if owner == nil {
				return nil
			}
			// make sure it's a NestedAPIServer
			if owner.APIVersion != clusterv1.GroupVersion.String() ||
				owner.Kind != string(clusterv1.APIServer) {
				return nil
			}

			// and if so, return it
			return []string{owner.Name}
		}); err != nil {
		return err
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&controlplanev1alpha4.NestedControllerManager{}).
		Complete(r)
}
