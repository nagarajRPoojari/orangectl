/*
Copyright 2025.

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

package controller

import (
	"context"
	"fmt"
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ctlv1alpha1 "orangectl/api/v1alpha1"
)

// OrangeCtlReconciler reconciles a OrangeCtl object
type OrangeCtlReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=ctl.orangectl.orange.db,resources=orangectls,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ctl.orangectl.orange.db,resources=orangectls/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ctl.orangectl.orange.db,resources=orangectls/finalizers,verbs=update

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// OrangeCtlReconciler spins up required number of shards with a router
// Components:
//   - Shard: a Statefulset with specified number of replicas, each hosting a orangedb instance.
//     check https://github.com/nagarajRPoojari/orange for more info.
//   - Router: a single Deployment of orange/gateway.
//     check https://github.com/nagarajRPoojari/orange/gateway for more info.
//   - Config: ConfigMap instance to store internal shard related info.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.21.0/pkg/reconcile
func (r *OrangeCtlReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var orangeCtl ctlv1alpha1.OrangeCtl
	if err := r.Get(ctx, req.NamespacedName, &orangeCtl); err != nil {
		if apierrors.IsNotFound(err) {
			log := logf.FromContext(ctx)
			log.Info("OrangeCtl resource not found (likely deleted), skipping")
			return ctrl.Result{}, nil
		}
		log.Error(err, "Failed to get OrangeCtl resource")
		return ctrl.Result{}, err
	}

	// Reconcile Shard StatefulSets
	err := r.reconcileShards(ctx, &orangeCtl)
	if err != nil {
		log.Error(err, "Failed to reconcile shards")
		return ctrl.Result{}, err
	}

	// Reconcile Router Deployment and Service
	if err := r.reconcileRouter(ctx, &orangeCtl); err != nil {
		log.Error(err, "Failed to reconcile router")
		return ctrl.Result{}, err
	}

	log.Info("Successfully reconciled OrangeCtl")
	return ctrl.Result{}, err
}

// reconcileRouter ensures that a Deployment and Service for the router component
// are created or updated to match the OrangeCtl spec. It sets appropriate labels,
// attaches the router to the given ConfigMap, and establishes controller ownership.
func (r *OrangeCtlReconciler) reconcileRouter(ctx context.Context, orangeCtl *ctlv1alpha1.OrangeCtl) error {

	routerSpec := orangeCtl.Spec.Router
	namespace := orangeCtl.Spec.Namespace
	serviceAccountName := "pod-watcher-sa"
	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceAccountName,
			Namespace: namespace,
		},
	}

	// create service account for router provding and attach controller reference to
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, sa, func() error {
		return controllerutil.SetControllerReference(orangeCtl, sa, r.Scheme)
	})

	if err != nil {
		return err
	}

	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-watcher-role",
			Namespace: namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, role, func() error {
		role.Rules = []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"pods"},
				Verbs:     []string{"get", "list", "watch"},
			},
		}
		return controllerutil.SetControllerReference(orangeCtl, role, r.Scheme)
	})
	if err != nil {
		return err
	}

	rb := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pod-watcher-binding",
			Namespace: namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, rb, func() error {
		rb.RoleRef = rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     role.Name,
		}
		rb.Subjects = []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      sa.Name,
				Namespace: sa.Namespace,
			},
		}
		return controllerutil.SetControllerReference(orangeCtl, rb, r.Scheme)
	})
	if err != nil {
		return err
	}

	labels := routerSpec.Labels
	if labels == nil {
		labels = map[string]string{}
	}
	labels["app"] = routerSpec.Name
	labels["orangectl"] = orangeCtl.Name

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerSpec.Name,
			Namespace: namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deploy, func() error {
		deploy.Labels = labels
		deploy.Spec = appsv1.DeploymentSpec{

			// @nagarajRPoojari: support multiple replica option for router
			Replicas: pointer.Int32(1),

			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: serviceAccountName,
					Containers: []corev1.Container{
						{
							Name:            routerSpec.Name,
							Image:           routerSpec.Image,
							ImagePullPolicy: corev1.PullNever,
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: routerSpec.Port,
								},
							},
							Env: []corev1.EnvVar{
								// __POD_SELECTOR__ will be used by router to identify pods to watch
								{Name: "__POD_SELECTOR__", Value: fmt.Sprintf("%s-shard-pod", orangeCtl.Name)},
							},
						},
					},
				},
			},
		}
		return controllerutil.SetControllerReference(orangeCtl, deploy, r.Scheme)
	})

	if err != nil {
		return fmt.Errorf("failed to create/update router Deployment: %w", err)
	}

	// Expose router service to client
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routerSpec.Name,
			Namespace: namespace,
		},
	}

	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		service.Labels = labels
		service.Spec = corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{
				{
					Port:       routerSpec.Port,
					TargetPort: intstr.FromInt(int(routerSpec.Port)),
				},
			},
		}
		return controllerutil.SetControllerReference(orangeCtl, service, r.Scheme)
	})

	if err != nil {
		return fmt.Errorf("failed to create/update router Service: %w", err)
	}

	return nil
}

// reconcileShards creates or updates a set of StatefulSets and headless Services
// for each shard defined in the OrangeCtl spec. It assembles the DNS addresses
// for all shard pods and returns a ConfigMap containing these addresses for proxy usage.
func (r *OrangeCtlReconciler) reconcileShards(ctx context.Context, orangeCtl *ctlv1alpha1.OrangeCtl) error {
	shardSpec := orangeCtl.Spec.Shard
	namespace := orangeCtl.Spec.Namespace

	for i := 0; i < shardSpec.Count; i++ {
		shardName := fmt.Sprintf("%s-%d", shardSpec.Name, i)

		ss := &appsv1.StatefulSet{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shardName,
				Namespace: namespace,
			},
		}

		_, err := controllerutil.CreateOrUpdate(ctx, r.Client, ss, func() error {
			// Set labels, adding shard and orangeCtl labels
			if ss.Labels == nil {
				ss.Labels = map[string]string{}
			}
			maps.Copy(ss.Labels, shardSpec.Labels)
			ss.Labels["orangectl"] = orangeCtl.Name
			ss.Labels["shard"] = shardName
			ss.Labels["pod-selector"] = fmt.Sprintf("%s-shard-pod", orangeCtl.Name)

			// StatefulSet spec
			ss.Spec.ServiceName = shardName
			ss.Spec.Replicas = pointer.Int32(shardSpec.Replicas)
			ss.Spec.Selector = &metav1.LabelSelector{
				MatchLabels: ss.Labels,
			}
			ss.Spec.Template = corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: ss.Labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:            "shard",
							Image:           shardSpec.Image,
							ImagePullPolicy: corev1.PullNever,
							Ports: []corev1.ContainerPort{
								{
									ContainerPort: shardSpec.Port,
								},
							},
							Env: []corev1.EnvVar{
								{
									Name: "__K8S_POD_NAME__",
									ValueFrom: &corev1.EnvVarSource{
										FieldRef: &corev1.ObjectFieldSelector{
											FieldPath: "metadata.name",
										},
									},
								},
								{Name: "__K8S_SERVICE_NAME__", Value: shardName},
								{Name: "__K8S_LEASE_NAMESAPCE__", Value: namespace},
								{Name: "__K8S_LEASE_NAME__", Value: shardName},
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "data",
									MountPath: "/app/data",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								EmptyDir: &corev1.EmptyDirVolumeSource{},
							},
						},
					},
				},
			}

			return controllerutil.SetControllerReference(orangeCtl, ss, r.Scheme)
		})
		if err != nil {
			return fmt.Errorf("failed to create or update StatefulSet %s: %w", shardName, err)
		}

		// existingSvc := &corev1.Service{}
		// svcKey := types.NamespacedName{Name: shardName, Namespace: namespace}
		// err = r.Client.Get(ctx, svcKey, existingSvc)

		// if err == nil && len(existingSvc.Spec.Selector) == 0 {
		// 	// Delete the service if selector is missing (immutable field cannot be updated)
		// 	if delErr := r.Client.Delete(ctx, existingSvc); delErr != nil {
		// 		return nil, fmt.Errorf("failed to delete service %s for selector fix: %w", shardName, delErr)
		// 	}
		// 	// Optionally wait a little or re-fetch if your controller needs to avoid race
		// }

		// Expose a shard level Service to distinguish pods from different shards
		// e.g: shard-0-1.shard-0.default.svc.cluster.local
		service := &corev1.Service{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shardName,
				Namespace: namespace,
			},
		}
		_, err = controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
			service.Spec = corev1.ServiceSpec{
				// Creating a headless service to get stable dns names
				ClusterIP: "None",
				Selector: map[string]string{
					// Selector labels should match with one being specified in
					// StatefulSet Pod template
					"shard":     shardName,
					"orangectl": orangeCtl.Name,
				},
				Ports: []corev1.ServicePort{
					{
						Port:       shardSpec.Port,
						TargetPort: intstr.FromInt(int(shardSpec.Port)),
					},
				},
			}
			return controllerutil.SetControllerReference(orangeCtl, service, r.Scheme)
		})

		if err != nil {
			return fmt.Errorf("failed to create or update StatefulSet service %s: %w", shardName, err)
		}

	}

	return nil
}

func toEnvVars(config map[string]string) []corev1.EnvVar {
	var envs []corev1.EnvVar
	for k, v := range config {
		envs = append(envs, corev1.EnvVar{
			Name:  k,
			Value: v,
		})
	}
	return envs
}

// SetupWithManager sets up the controller with the Manager.
func (r *OrangeCtlReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ctlv1alpha1.OrangeCtl{}).
		Named("orangectl").
		Complete(r)
}
