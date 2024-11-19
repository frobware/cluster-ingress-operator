package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	routev1 "github.com/openshift/api/route/v1"
)

func setupIdleConnectionTerminationPolicyTest(t *testing.T, baseName string) (*corev1.Namespace, error) {
	ns := createNamespace(t, baseName+"-"+rand.String(5))

	for i := 1; i <= 2; i++ {
		if err := createBackendService(t, ns.Name, i); err != nil {
			return nil, fmt.Errorf("failed to create backend %d: %v", i, err)
		}
	}

	services, err := getServices(ns.Name)
	if err != nil {
		return nil, fmt.Errorf("failed to get services: %v", err)
	}

	if len(services) == 0 {
		return nil, fmt.Errorf("no services found")
	}

	_, err = createRoute(ns.Name, "test", services[0].Name, ns.Labels)
	if err != nil {
		return nil, fmt.Errorf("failed to create test route: %v", err)
	}

	return ns, nil
}

func createBackendService(t *testing.T, namespace string, index int) error {
	labels := map[string]string{
		"app":      "web-server",
		"instance": fmt.Sprintf("%d", index),
	}

	deployment, err := createDeployment(namespace, index, labels)
	if err != nil {
		return err
	}

	if err := waitForDeploymentComplete(t, kclient, deployment, 2*time.Minute); err != nil {
		return fmt.Errorf("deployment %d is not ready: %v", index, err)
	}

	_, err = createService(t, namespace, index, labels)
	if err != nil {
		return err
	}

	return nil
}

func createDeployment(namespace string, index int, labels map[string]string) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("web-server-%d", index),
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: labels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: labels,
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  "nginx",
							Image: "quay.io/openshifttest/nginx-alpine@sha256:04f316442d48ba60e3ea0b5a67eb89b0b667abf1c198a3d0056ca748736336a0",
							Ports: []corev1.ContainerPort{
								{
									Name:          "http",
									Protocol:      corev1.ProtocolTCP,
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

func createService(t *testing.T, namespace string, index int, labels map[string]string) (*corev1.Service, error) {
	svc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("service-%d", index),
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:       "http",
					Port:       8080,
					Protocol:   corev1.ProtocolTCP,
					TargetPort: intstr.FromInt32(8080),
				},
			},
			Selector: labels,
		},
	}

	if err := kclient.Create(context.TODO(), svc); err != nil {
		return nil, err
	}

	return svc, nil
}

func createRoute(namespace, name, serviceName string, labels map[string]string) (*routev1.Route, error) {
	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: serviceName,
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString("http"),
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}

	if err := kclient.Create(context.TODO(), route); err != nil {
		return nil, err
	}

	return route, nil
}

func getServices(namespace string) ([]*corev1.Service, error) {
	var serviceList corev1.ServiceList
	if err := kclient.List(context.TODO(), &serviceList, client.InNamespace(namespace), client.MatchingLabels{"app": "web-server"}); err != nil {
		return nil, fmt.Errorf("failed to list services: %v", err)
	}

	services := make([]*corev1.Service, 0, len(serviceList.Items))
	for i := range serviceList.Items {
		services = append(services, &serviceList.Items[i])
	}

	return services, nil
}

func waitForAllRoutesAdmitted(namespace string, timeout time.Duration, progress func(admittedRoutes, totalRoutes int, pendingRoutes []string)) (*routev1.RouteList, error) {
	isRouteAdmitted := func(route *routev1.Route) bool {
		for i := range route.Status.Ingress {
			if route.Status.Ingress[i].RouterCanonicalHostname != "" {
				return true
			}
		}
		return false
	}

	var routeList routev1.RouteList
	err := wait.PollImmediate(time.Second, timeout, func() (bool, error) {
		if err := kclient.List(context.TODO(), &routeList, client.InNamespace(namespace)); err != nil {
			return false, fmt.Errorf("failed to list routes in namespace %s: %v", namespace, err)
		}

		admittedRoutes := 0
		var pendingRoutes []string
		for i := range routeList.Items {
			if isRouteAdmitted(&routeList.Items[i]) {
				admittedRoutes++
			} else {
				pendingRoutes = append(pendingRoutes, fmt.Sprintf("%s/%s", routeList.Items[i].Namespace, routeList.Items[i].Name))
			}
		}

		totalRoutes := len(routeList.Items)
		if progress != nil {
			progress(admittedRoutes, totalRoutes, pendingRoutes)
		}

		if admittedRoutes == totalRoutes {
			return true, nil
		}

		return false, nil
	})

	if err != nil {
		return nil, fmt.Errorf("not all routes were admitted in namespace %s: %v", namespace, err)
	}

	return &routeList, nil
}

func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	baseName := "idle-close-on-response-e2e"

	ns, err := setupIdleConnectionTerminationPolicyTest(t, baseName)
	if err != nil {
		t.Fatalf("failed to setup test resources: %v", err)
	}

	_, err = waitForAllRoutesAdmitted(ns.Name, 2*time.Minute, func(admittedRoutes, totalRoutes int, pendingRoutes []string) {
		if len(pendingRoutes) > 0 {
			t.Logf("%d/%d routes admitted. Waiting for: %s", admittedRoutes, totalRoutes, strings.Join(pendingRoutes, ", "))
		} else {
			t.Logf("All %d routes in namespace %s have been admitted", totalRoutes, ns.Name)
		}
	})
	if err != nil {
		t.Fatalf("Error waiting for routes to be admitted: %v", err)
	}
}
