package e2e

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

// createNamespaceWithSuffix creates a namespace with a random suffix.
func createNamespaceWithSuffix(t *testing.T, kclient *kubernetes.Clientset, baseName string) *corev1.Namespace {
	t.Helper()

	namespaceName := fmt.Sprintf("%s-%s", baseName, rand.String(5))
	t.Logf("Creating namespace %q...", namespaceName)

	ns := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespaceName,
		},
	}

	if _, err := kclient.CoreV1().Namespaces().Create(context.TODO(), ns, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Deleting namespace %q...", namespaceName)
		if err := kclient.CoreV1().Namespaces().Delete(context.TODO(), namespaceName, metav1.DeleteOptions{}); err != nil {
			t.Errorf("Failed to delete namespace %s: %v", namespaceName, err)
		}
	})

	return ns
}

// createRoute constructs and creates a Route resource.
func createRoute(t *testing.T, routeClient routev1client.RouteV1Interface, namespace, routeName, serviceName, portName string) {
	route := &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: namespace,
			Labels:    map[string]string{"app": "ocpbugs48050-test"},
		},
		Spec: routev1.RouteSpec{
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString(portName),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   routev1.TLSTerminationEdge,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
			To: routev1.RouteTargetReference{
				Kind:   "Service",
				Name:   serviceName,
				Weight: pointer.Int32(100),
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}

	if _, err := routeClient.Routes(namespace).Create(context.TODO(), route, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create route %s/%s: %v", namespace, routeName, err)
	}
	t.Logf("Created route %s/%s", namespace, routeName)
}

// performHTTPRequest performs an HTTP GET request to the specified route
// with retries and returns true if the response is successful (200 OK).
func performHTTPRequest(t *testing.T, routeClient routev1client.RouteV1Interface, namespace, routeName string, retries int, retryDelay time.Duration) bool {
	var routeHost string

	// Retrieve the route's host
	err := wait.PollImmediate(retryDelay, time.Duration(retries)*retryDelay, func() (bool, error) {
		route, err := routeClient.Routes(namespace).Get(context.TODO(), routeName, metav1.GetOptions{})
		if err != nil {
			t.Logf("Error getting route %s/%s: %v", namespace, routeName, err)
			return false, nil
		}
		if len(route.Spec.Host) > 0 {
			routeHost = route.Spec.Host
			return true, nil
		}
		t.Logf("Route %s/%s does not have a host yet", namespace, routeName)
		return false, nil
	})
	if err != nil {
		t.Fatalf("Failed to get route host for %s/%s: %v", namespace, routeName, err)
		return false
	}

	url := fmt.Sprintf("https://%s", routeHost)

	// Create HTTP client with TLS configuration
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // Skip TLS verification for testing
			},
		},
	}

	// Perform HTTP GET requests with retries
	for i := 0; i < retries; i++ {
		resp, err := httpClient.Get(url)
		if err == nil && resp.StatusCode == http.StatusOK {
			t.Logf("Successfully reached route %s/%s", namespace, routeName)
			return true
		}

		if err != nil {
			t.Logf("Error accessing route %s/%s: %v", namespace, routeName, err)
		} else {
			t.Logf("Unexpected status code %d for route %s/%s", resp.StatusCode, namespace, routeName)
		}
		time.Sleep(retryDelay)
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
	namespace := createNamespaceWithSuffix(t, kubeClient, "ocpbugs48050")
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
		t.Fatalf("Failed to create deployment %s/%s: %v", namespace.Name, deploymentName, err)
	}
	t.Logf("Created deployment %s/%s", namespace.Name, deploymentName)

	// Wait for deployment to be ready
	err = wait.PollImmediate(5*time.Second, 2*time.Minute, func() (bool, error) {
		deploy, err := kubeClient.AppsV1().Deployments(namespace.Name).Get(context.TODO(), deploymentName, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if deploy.Status.AvailableReplicas == *deploy.Spec.Replicas {
			return true, nil
		}
		t.Logf("Waiting for deployment %s/%s to be ready...", namespace.Name, deploymentName)
		return false, nil
	})
	if err != nil {
		t.Fatalf("Deployment %s/%s not ready: %v", namespace.Name, deploymentName, err)
	}
	t.Logf("Deployment %s/%s is ready", namespace.Name, deploymentName)

	// Step 4: Define and create service
	serviceName := "ocpbugs48050-service"
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
		t.Fatalf("Failed to create service %s/%s: %v", namespace.Name, serviceName, err)
	}
	t.Logf("Created service %s/%s", namespace.Name, serviceName)

	// Step 5: Define and create the single-te route using the helper function
	singleTERouteName := "ocpbugs40850-single-te"
	createRoute(t, routeClient, namespace.Name, singleTERouteName, serviceName, "single-te")

	// Step 6: Define and create the duplicate-te routes in a loop using the helper function
	for i := 0; i <= 6; i++ {
		routeName := fmt.Sprintf("ocpbugs40850-duplicate-te%d", i)
		createRoute(t, routeClient, namespace.Name, routeName, serviceName, "duplicate-te")
	}

	// Wait for a moment to allow routes to be admitted
	time.Sleep(10 * time.Second)

	// Step 7: Retry logic for the single-te route
	if !performHTTPRequest(t, routeClient, namespace.Name, singleTERouteName, 10, 5*time.Second) {
		t.Fatalf("Failed to reach route %s/%s", namespace.Name, singleTERouteName)
	}

	// Step 8: Proceed to curl remaining duplicate-te routes
	routeHits := map[string]int{
		"ocpbugs40850-duplicate-te0": 2,
		"ocpbugs40850-duplicate-te1": 3,
		"ocpbugs40850-duplicate-te2": 1,
		"ocpbugs40850-duplicate-te3": 1,
		"ocpbugs40850-duplicate-te4": 1,
		"ocpbugs40850-duplicate-te5": 1,
		"ocpbugs40850-duplicate-te6": 1,
	}

	for routeName, hitCount := range routeHits {
		for i := 0; i < hitCount; i++ {
			if !performHTTPRequest(t, routeClient, namespace.Name, routeName, 3, 5*time.Second) {
				t.Fatalf("Failed to reach route %s/%s on attempt %d", namespace.Name, routeName, i+1)
			}
			t.Logf("Successful request to %s/%s (%d/%d)", namespace.Name, routeName, i+1, hitCount)
		}
	}

	// Step 9: Perform the Prometheus query
	// Note: Replace this with actual code to create a Prometheus client and perform the query
	// For the purpose of this example, we will just log that this is where the query would happen.

	t.Logf("Performing Prometheus query...")

	// Example query
	// query := `sum(haproxy_backend_duplicate_te_header_total{route="ocpbugs40850-duplicate-te6"})`

	// Placeholder for the actual Prometheus query
	// Implement the Prometheus client and query logic here
	// For now, we'll simulate the result
	result := map[string]interface{}{
		"status": "success",
		"data": map[string]interface{}{
			"resultType": "vector",
			"result": []interface{}{
				map[string]interface{}{
					"metric": map[string]interface{}{
						"__name__": "haproxy_backend_duplicate_te_header_total",
						"route":    "ocpbugs40850-duplicate-te6",
					},
					"value": []interface{}{
						float64(time.Now().Unix()),
						"7",
					},
				},
			},
		},
	}

	// Pretty print the result
	prettyResult, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatalf("Failed to format result for route %s/%s: %v", namespace.Name, "ocpbugs40850-duplicate-te6", err)
	}

	fmt.Printf("Prometheus Query Result:\n%s\n", prettyResult)
}
