// XXXnothingXXX
// XXXmorenothingXXX

package e2e

import (
	"context"
	"fmt"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/utils/pointer"
)

func createDeployment(namespace, name string, serviceIndex int) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": name},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  name,
							Image: "path/to/your/image", // Update with actual image
							Env: []corev1.EnvVar{
								{
									Name:  "RESPONSE",
									Value: fmt.Sprintf("Service-%d", serviceIndex),
								},
							},
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									ContainerPort: 8080,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := kclient.Create(context.TODO(), deployment); err != nil {
		return nil, err
	}

	return deployment, nil
}

// func setupDeferredConnectionTest(t *testing.T, baseName string) (*corev1.Namespace, *routev1.Route, []*corev1.Service, error) {
// 	ns := createNamespace(t, baseName+"-"+rand.String(5))

// 	// Create two services that will return distinguishable responses
// 	services := make([]*corev1.Service, 2)
// 	for i := 1; i <= 2; i++ {
// 		svc, err := createService(ns.Name, fmt.Sprintf("%s-%d", baseName, i))
// 		if err != nil {
// 			return nil, nil, nil, fmt.Errorf("failed to create service %d: %v", i, err)
// 		}
// 		services[i-1] = svc

// 		// Create deployment for each service with unique response
// 		dep, err := createDeployment(ns.Name, svc.Name, i)
// 		if err != nil {
// 			return nil, nil, nil, fmt.Errorf("failed to create deployment %d: %v", i, err)
// 		}

// 		if err := waitForDeploymentComplete(t, kclient, dep, 5*time.Minute); err != nil {
// 			return nil, nil, nil, fmt.Errorf("deployment %s/%s not ready: %v", dep.Namespace, dep.Name, err)
// 		}
// 	}

// 	// Create route pointing to first service initially
// 	route, err := createRoute(ns.Name, baseName, services[0].Name)
// 	if err != nil {
// 		return nil, nil, nil, fmt.Errorf("failed to create route: %v", err)
// 	}

// 	// Wait for route admission
// 	if err := waitForRouteAdmission(context.TODO(), routeClient, route.Namespace, route.Name); err != nil {
// 		return nil, nil, nil, fmt.Errorf("route not admitted: %v", err)
// 	}

// 	return ns, route, services, nil
// }

// func setupIdleConnectionTest(t *testing.T, baseName string) (*corev1.Namespace, *routev1.Route, error) {
// 	ns := createNamespace(t, baseName+"-"+rand.String(5))

// 	service, err := createOCPBUGS48050Service(ns.Name, baseName)
// 	if err != nil {
// 		return nil, nil, fmt.Errorf("failed to create service %s/%s: %v", ns.Name, baseName, err)
// 	}

// 	image, err := getCanaryImageFromIngressOperatorDeployment()
// 	if err != nil {
// 		return nil, nil, fmt.Errorf("failed to get canary image: %v", err)
// 	}

// 	deployment, err := createOCPBUGS48050Deployment(ns.Name, baseName, image)
// 	if err != nil {
// 		return nil, nil, fmt.Errorf("failed to create deployment %s/%s: %v", ns.Name, baseName, err)
// 	}

// 	if err := waitForDeploymentComplete(t, kclient, deployment, 5*time.Minute); err != nil {
// 		return nil, nil, fmt.Errorf("deployment %s/%s not ready: %v", deployment.Namespace, deployment.Name, err)
// 	}

// 	route, err := createRoute(ns.Name, "test-route", service.Name, "http", routev1.TLSTerminationEdge)
// 	if err != nil {
// 		return nil, nil, fmt.Errorf("failed to create route: %v", err)
// 	}
// 	t.Logf("Created route %s/%s", route.Namespace, route.Name)

// 	if err := waitForPodsReadyAndLive(t, kclient, ns.Name, deployment.Spec.Template.Labels, 5*time.Minute); err != nil {
// 		return nil, nil, fmt.Errorf("pods failed readiness or liveness checks: %v", err)
// 	}

// 	return ns, route, nil
// }

// func waitForAllRoutesAdmitted(namespace string, timeout time.Duration, progress func(admittedRoutes, totalRoutes int, pendingRoutes []string)) (*routev1.RouteList, error) {
// 	isRouteAdmitted := func(route *routev1.Route) bool {
// 		for i := range route.Status.Ingress {
// 			if route.Status.Ingress[i].RouterCanonicalHostname != "" {
// 				return true
// 			}
// 		}
// 		return false
// 	}

// 	var routeList routev1.RouteList
// 	err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
// 		if err := kclient.List(context.TODO(), &routeList, client.InNamespace(namespace)); err != nil {
// 			return false, fmt.Errorf("failed to list routes in namespace %s: %v", namespace, err)
// 		}

// 		admittedRoutes := 0
// 		var pendingRoutes []string
// 		for i := range routeList.Items {
// 			if isRouteAdmitted(&routeList.Items[i]) {
// 				admittedRoutes++
// 			} else {
// 				pendingRoutes = append(pendingRoutes, fmt.Sprintf("%s/%s", routeList.Items[i].Namespace, routeList.Items[i].Name))
// 			}
// 		}

// 		totalRoutes := len(routeList.Items)
// 		if progress != nil {
// 			progress(admittedRoutes, totalRoutes, pendingRoutes)
// 		}

// 		if admittedRoutes == totalRoutes {
// 			return true, nil
// 		}

// 		return false, nil
// 	})

// 	if err != nil {
// 		return nil, fmt.Errorf("not all routes were admitted in namespace %s: %v", namespace, err)
// 	}

// 	return &routeList, nil
// }

// func waitForPodsReadyAndLive(t *testing.T, cl client.Client, namespace string, labelSelector map[string]string, timeout time.Duration) error {
// 	t.Helper()
// 	err := wait.PollImmediate(1*time.Second, timeout, func() (bool, error) {
// 		pods := &corev1.PodList{}
// 		if err := cl.List(context.TODO(), pods, client.InNamespace(namespace), client.MatchingLabels(labelSelector)); err != nil {
// 			t.Logf("error listing pods in namespace %s with selector %v: %v", namespace, labelSelector, err)
// 			return false, nil
// 		}

// 		for _, pod := range pods.Items {
// 			isReady := false
// 			for _, condition := range pod.Status.Conditions {
// 				if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
// 					isReady = true
// 					break
// 				}
// 			}
// 			if !isReady {
// 				t.Logf("pod %s/%s is not ready", pod.Namespace, pod.Name)
// 				return false, nil
// 			}

// 			for _, containerStatus := range pod.Status.ContainerStatuses {
// 				if containerStatus.State.Terminated != nil {
// 					t.Logf("pod %s/%s has a terminated container", pod.Namespace, pod.Name)
// 					return false, nil
// 				}
// 				if containerStatus.State.Waiting != nil && containerStatus.RestartCount > 0 {
// 					t.Logf("pod %s/%s is restarting (liveness probe failure)", pod.Namespace, pod.Name)
// 					return false, nil
// 				}
// 			}
// 		}

// 		// All pods are ready and live.
// 		return true, nil
// 	})

// 	if err != nil {
// 		return fmt.Errorf("failed to observe readiness and liveness for pods in namespace %s with selector %v", namespace, labelSelector)
// 	}

// 	return nil
// }

func getCanaryImageFromIngressOperatorDeployment() (string, error) {
	ingressOperator := types.NamespacedName{Namespace: operatorNamespace, Name: "ingress-operator"}

	deployment := appsv1.Deployment{}
	if err := kclient.Get(context.TODO(), ingressOperator, &deployment); err != nil {
		return "", fmt.Errorf("failed to get deployment %s/%s: %v", ingressOperator.Namespace, ingressOperator.Name, err)
	}

	for _, container := range deployment.Spec.Template.Spec.Containers {
		for _, env := range container.Env {
			if env.Name == "CANARY_IMAGE" {
				return env.Value, nil
			}
		}
	}

	return "", fmt.Errorf("CANARY_IMAGE environment variable not found in deployment %s/%s", ingressOperator.Namespace, ingressOperator.Name)
}

func setupIdleConnectionTerminationPolicyTest(t *testing.T, baseName string) (*corev1.Namespace, error) {
	ns := createNamespace(t, baseName+"-"+rand.String(5))

	_, err := getCanaryImageFromIngressOperatorDeployment()
	if err != nil {
		return nil, fmt.Errorf("failed to get canary image: %v", err)
	}

	return ns, nil
}

func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	baseName := "idle-close-on-response-e2e"

	_, err := setupIdleConnectionTerminationPolicyTest(t, baseName, 10)
	if err != nil {
		t.Fatalf("failed to setup test resources: %v", err)
	}
}
