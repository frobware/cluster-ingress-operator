package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
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

// func TestFoo(t *testing.T) {
// 	kubeConfig, err := config.GetConfig()
// 	if err != nil {
// 		t.Fatalf("failed to get kube config: %v", err)
// 	}
// 	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
// 	if err != nil {
// 		t.Fatalf("failed to create kube client: %v", err)
// 	}
// 	routeClient, err := routev1client.NewForConfig(kubeConfig)
// 	if err != nil {
// 		t.Fatalf("failed to create route client: %v", err)
// 	}

// 	prometheusClient, err := metrics.NewPrometheusClient(context.TODO(), kubeClient, routeClient)
// 	if err != nil {
// 		t.Fatalf("failed to create prometheus client: %v", err)
// 	}

// 	query := `sum(haproxy_backend_duplicate_te_header_total{route="ocpbugs40850-duplicate-te6"})`
// 	result, _, err := prometheusClient.Query(context.TODO(), query, time.Now())
// 	if err != nil {
// 		t.Fatalf("Prometheus query failed: %v", err)
// 	}

// 	// Pretty print the result
// 	prettyResult, err := json.MarshalIndent(result, "", "  ")
// 	if err != nil {
// 		t.Fatalf("failed to format result: %v", err)
// 	}

// 	fmt.Printf("Prometheus Query Result:\n%s\n", prettyResult)
// }

func createNamespaceWithSuffix(t *testing.T, baseName string) *corev1.Namespace {
	t.Helper()

	namespaceName := fmt.Sprintf("%s-%s", baseName, rand.String(5))
	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
		},
	}

	if err := kclient.Create(context.TODO(), ns); err != nil {
		t.Fatalf("failed to create namespace: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Deleting namespace %q...", namespaceName)
		if err := kclient.Delete(context.TODO(), ns); err != nil {
			t.Errorf("failed to delete namespace %s: %v", namespaceName, err)
		}
	})

	return ns
}

// performHTTPRequest performs an HTTP GET request to the specified route host
// with retries and returns true if the response is successful (200 OK).
func performHTTPRequest(t *testing.T, routeName, namespace, clusterDomain string, retryCount int, retryInterval time.Duration) bool {
	httpClient := http.Client{Timeout: 5 * time.Second}
	routeHost := fmt.Sprintf("%s-%s.apps.%s", routeName, namespace, clusterDomain)

	for i := 0; i < retryCount; i++ {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s", routeHost), nil)
		if err != nil {
			t.Fatalf("failed to create request for route %s: %v", routeName, err)
		}
		req.Host = routeName

		resp, err := httpClient.Do(req)
		if err != nil {
			t.Logf("failed to reach route %s (attempt %d/%d): %v", routeName, i+1, retryCount, err)
			time.Sleep(retryInterval)
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode == http.StatusOK {
			t.Logf("Successfully reached route %s after %d attempt(s)", routeName, i+1)
			return true
		} else {
			t.Logf("unexpected status code %d for route %s (attempt %d/%d)", resp.StatusCode, routeName, i+1, retryCount)
			if i == retryCount-1 {
				t.Fatalf("failed to reach route %s after %d attempts", routeName, retryCount)
			}
			time.Sleep(retryInterval)
		}
	}
	return false
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
	t.Logf("Created namespace %s", namespace.Name)

	// Step 3: Define and create deployment
	deploymentName := "ocpbugs48050-test"
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
	t.Logf("Created deployment %s/%s", namespace.Name, deploymentName)

	// Step 4: Define and create service
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ocpbugs48050-service",
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
		t.Fatalf("failed to create service %s/%s: %v", namespace.Name, service.Name, err)
	}
	t.Logf("Created service %s/%s", namespace.Name, service.Name)

	// Step 4: Define and create the single-te route first
	singleTERoute := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ocpbugs40850-single-te",
			Namespace: namespace.Name,
			Labels:    map[string]string{"app": "ocpbugs48050-test"},
		},
		Spec: routev1.RouteSpec{
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString("single-te"),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   routev1.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   service.Name,
				Weight: pointer.Int32(100),
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}

	if _, err := routeClient.RouteV1().Routes(namespace.Name).Create(context.TODO(), singleTERoute, metav1.CreateOptions{}); err != nil {
		t.Fatalf("failed to create single-te route %s/%s: %v", namespace.Name, singleTERoute.Name, err)
	}
	t.Logf("Created route %s/%s", namespace.Name, singleTERoute.Name)

	// Step 5: Define and create the duplicate-te routes in a loop
	for i := 0; i <= 6; i++ {
		routeName := fmt.Sprintf("ocpbugs40850-duplicate-te%d", i)
		route := &routev1.Route{
			ObjectMeta: metav1.ObjectMeta{
				Name:      routeName,
				Namespace: namespace.Name,
				Labels:    map[string]string{"app": "ocpbugs48050-test"},
			},
			Spec: routev1.RouteSpec{
				Port: &routev1.RoutePort{
					TargetPort: intstr.FromString("duplicate-te"),
				},
				TLS: &routev1.TLSConfig{
					Termination:                   routev1.TLSTerminationEdge,
					InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
				},
				To: routev1.RouteTargetReference{
					Kind:   "Service",
					Name:   service.Name,
					Weight: pointer.Int32(100),
				},
				WildcardPolicy: routev1.WildcardPolicyNone,
			},
		}

		if _, err := routeClient.RouteV1().Routes(namespace.Name).Create(context.TODO(), route, metav1.CreateOptions{}); err != nil {
			t.Fatalf("failed to create duplicate-te route %s/%s: %v", namespace.Name, routeName, err)
		}
		t.Logf("Created route %s/%s", namespace.Name, routeName)
	}

	// Step 6: Retry logic for the single-te route
	singleTERoute := "ocpbugs40850-single-te"
	clusterDomain := "your-cluster-domain" // Replace with actual cluster domain
	if !performHTTPRequest(t, singleTERoute, namespace.Name, clusterDomain, 10, 5*time.Second) {
		t.Fatalf("failed to reach route %s/%s", namespace.Name, singleTERoute)
	}

	// Proceed to curl remaining duplicate-te routes
	routeHits := map[string]int{
		"ocpbugs40850-duplicate-te0": 2,
		"ocpbugs40850-duplicate-te1": 3,
		"ocpbugs40850-duplicate-te2": 1,
		// Add more duplicate-te routes here as needed
	}

	for routeName, hitCount := range routeHits {
		for i := 0; i < hitCount; i++ {
			if !performHTTPRequest(t, routeName, namespace.Name, clusterDomain, 3, 5*time.Second) {
				t.Fatalf("failed to reach route %s/%s on attempt %d", namespace.Name, routeName, i+1)
			}
			t.Logf("Successful request to %s/%s (%d/%d)", namespace.Name, routeName, i+1, hitCount)
		}
	}

	// Step 6: Perform the Prometheus query
	prometheusClient, err := metrics.NewPrometheusClient(context.TODO(), kubeClient, routeClient)
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}

	query := fmt.Sprintf(`sum(haproxy_backend_duplicate_te_header_total{route="%s"})`, "ocpbugs40850-duplicate-te6")
	result, _, err := prometheusClient.Query(context.TODO(), query, time.Now())
	if err != nil {
		t.Fatalf("Prometheus query failed for route %s/%s: %v", namespace.Name, "ocpbugs40850-duplicate-te6", err)
	}

	// Pretty print the result
	prettyResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("failed to format result for route %s/%s: %v", namespace.Name, "ocpbugs40850-duplicate-te6", err)
	}

	fmt.Printf("Prometheus Query Result:\n%s\n", prettyResult)
}
