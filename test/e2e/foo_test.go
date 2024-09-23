package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned"
	"github.com/openshift/library-go/test/library/metrics"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

func TestFoo(t *testing.T) {
	kubeConfig, err := config.GetConfig()
	if err != nil {
		t.Fatalf("failed to get kube config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatalf("failed to create kube client: %v", err)
	}
	routeClient, err := routev1client.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatalf("failed to create route client: %v", err)
	}

	prometheusClient, err := metrics.NewPrometheusClient(context.TODO(), kubeClient, routeClient)
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}

	query := `sum(haproxy_backend_duplicate_te_header_total{route="ocpbugs40850-duplicate-te6"})`
	result, _, err := prometheusClient.Query(context.TODO(), query, time.Now())
	if err != nil {
		t.Fatalf("Prometheus query failed: %v", err)
	}

	// Pretty print the result
	prettyResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("failed to format result: %v", err)
	}

	fmt.Printf("Prometheus Query Result:\n%s\n", prettyResult)
}

func createNamespaceWithSuffix(t *testing.T, baseName string) *corev1.Namespace {
	// Create namespace with random suffix
	namespaceName := fmt.Sprintf("%s-%s", baseName, rand.String(5))
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespaceName}}

	// Create namespace
	if err := kclient.Create(context.TODO(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	// Register cleanup function with t.Cleanup
	t.Cleanup(func() {
		t.Logf("Deleting namespace %q...", namespaceName)
		if err := kclient.Delete(context.TODO(), ns); err != nil {
			t.Errorf("failed to delete namespace %s: %v", namespaceName, err)
		}
	})

	return ns
}

func TestOCPBUGS48050(t *testing.T) {
	// Step 1: Setup kubeConfig and clients
	kubeConfig, err := config.GetConfig()
	if err != nil {
		t.Fatalf("failed to get kube config: %v", err)
	}
	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatalf("failed to create kube client: %v", err)
	}
	routeClient, err := routev1client.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatalf("failed to create route client: %v", err)
	}

	// Step 2: Create namespace with random suffix
	namespace := createNamespaceWithSuffix(t, "ocpbugs48050")

	// Step 3: Define and create deployment
	deploymentName := "ocpbugs48050-test-" + namespace.Name
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      deploymentName,
			Namespace: namespace.Name,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: pointer.Int32(1),
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{"app": deploymentName},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": deploymentName},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{
							Name:  deploymentName,
							Image: "quay.io/amcdermo/ocpbugs40850-server:latest",
							Ports: []corev1.ContainerPort{
								{Name: "single-te", ContainerPort: 1030},
								{Name: "duplicate-te", ContainerPort: 1040},
								{Name: "health-port", ContainerPort: 1050},
							},
							Env: []corev1.EnvVar{
								{Name: "HEALTH_PORT", Value: "1050"},
								{Name: "SINGLE_TE_PORT", Value: "1030"},
								{Name: "DUPLICATE_TE_PORT", Value: "1040"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/",
										Port: intstr.FromInt(1050),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/",
										Port: intstr.FromInt(1050),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       10,
							},
						},
					},
				},
			},
		},
	}

	if _, err := kubeClient.AppsV1().Deployments(namespace.Name).Create(context.TODO(), deployment, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create deployment %s/%s: %v", namespace.Name, deploymentName, err)
	}
	defer func() {
		if err := kubeClient.AppsV1().Deployments(namespace.Name).Delete(context.TODO(), deployment.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("failed to delete deployment %s/%s: %v", namespace.Name, deploymentName, err)
		}
	}()

	// Step 4: Define and create service
	serviceName := "ocpbugs48050-service-" + namespace.Name
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace.Name,
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": deploymentName},
			Ports: []corev1.ServicePort{
				{Name: "single-te", Port: 1030, TargetPort: intstr.FromInt(1030)},
				{Name: "duplicate-te", Port: 1040, TargetPort: intstr.FromInt(1040)},
				{Name: "health-port", Port: 1050, TargetPort: intstr.FromInt(1050)},
			},
		},
	}

	if _, err := kubeClient.CoreV1().Services(namespace.Name).Create(context.TODO(), service, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create service %s/%s: %v", namespace.Name, serviceName, err)
	}
	defer func() {
		if err := kubeClient.CoreV1().Services(namespace.Name).Delete(context.TODO(), service.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("failed to delete service %s/%s: %v", namespace.Name, serviceName, err)
		}
	}()

	// Step 5: Define and create route
	routeName := "ocpbugs48050-duplicate-te-" + namespace.Name
	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: namespace.Name,
		},
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: service.Name,
			},
		},
	}

	if _, err := routeClient.RouteV1().Routes(namespace.Name).Create(context.TODO(), route, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create route %s/%s: %v", namespace.Name, routeName, err)
	}
	defer func() {
		if err := routeClient.RouteV1().Routes(namespace.Name).Delete(context.TODO(), route.Name, metav1.DeleteOptions{}); err != nil {
			t.Fatalf("failed to delete route %s/%s: %v", namespace.Name, routeName, err)
		}
	}()

	// Step 6: Perform the Prometheus query
	prometheusClient, err := metrics.NewPrometheusClient(context.TODO(), kubeClient, routeClient)
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}

	query := fmt.Sprintf(`sum(haproxy_backend_duplicate_te_header_total{route="%s"})`, routeName)
	result, _, err := prometheusClient.Query(context.TODO(), query, time.Now())
	if err != nil {
		t.Fatalf("Prometheus query failed for route %s/%s: %v", namespace.Name, routeName, err)
	}

	// Pretty print the result
	prettyResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("failed to format result for route %s/%s: %v", namespace.Name, routeName, err)
	}

	time.Sleep(time.Hour)
	fmt.Printf("Prometheus Query Result:\n%s\n", prettyResult)
}
