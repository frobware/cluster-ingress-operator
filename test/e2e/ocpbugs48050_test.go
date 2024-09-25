package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingress"
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

// composeRoute constructs and returns a Route resource object.
func composeRoute(namespace, routeName, serviceName, portName string, tlsTerminationType routev1.TLSTerminationType) *routev1.Route {
	route := routev1.Route{
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
				Termination:                   tlsTerminationType,
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

	return &route
}

// performHTTPRequestWithExpectedError performs an HTTP GET request
// multiple times and verifies the response or error against the
// expected error.
func performHTTPRequestWithExpectedError(t *testing.T, routeClient routev1client.RouteV1Interface, namespace, routeName string, hits int, retryDelay time.Duration, expectedError string) {
	var routeHost string

	// Retrieve the route's host
	err := wait.PollImmediate(retryDelay, time.Duration(hits)*retryDelay, func() (bool, error) {
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

	// Perform HTTP GET requests and validate the response against the expected error
	for i := 0; i < hits; i++ {
		resp, err := httpClient.Get(url)
		if err != nil {
			// Check if the error matches the expected error
			if expectedError != "" && strings.Contains(err.Error(), expectedError) {
				t.Logf("Expected error encountered for route %s/%s: %v", namespace, routeName, expectedError)
				continue
			} else {
				t.Fatalf("Unexpected error for route %s/%s: %v", namespace, routeName, err)
			}
		}

		// If no error and we expect none
		if resp.StatusCode == http.StatusOK && expectedError == "" {
			t.Logf("Successfully reached route %s/%s", namespace, routeName)
		} else if expectedError != "" {
			t.Fatalf("Expected error %q for route %s/%s, but got status code %d", expectedError, namespace, routeName, resp.StatusCode)
		}

		time.Sleep(retryDelay)
	}
}

func waitForRouteAdmitted(t *testing.T, routeClient *routev1client.RouteV1Client, namespace, routeName string, timeout time.Duration) error {
	// Poll the route object and check if it has been admitted by the router
	return wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		route, err := routeClient.Routes(namespace).Get(context.TODO(), routeName, metav1.GetOptions{})
		if err != nil {
			t.Logf("Error fetching route %s/%s: %v", namespace, routeName, err)
			return false, err
		}

		// Check if the route has any Ingress status set
		if len(route.Status.Ingress) == 0 {
			t.Logf("Route %s/%s does not have Ingress yet", namespace, routeName)
			return false, nil
		}

		// Check if the routerCanonicalHostname is set, indicating the route is admitted
		for _, ingress := range route.Status.Ingress {
			if ingress.RouterCanonicalHostname != "" {
				t.Logf("Route %s/%s has been admitted with router %s", namespace, routeName, ingress.RouterCanonicalHostname)
				return true, nil
			}
		}

		t.Logf("Waiting for route %s/%s to be admitted", namespace, routeName)
		return false, nil
	})
}

func composeRouteWithPort(namespace, routeName, serviceName, targetPort string, terminationType routev1.TLSTerminationType) *routev1.Route {
	return &routev1.Route{
		ObjectMeta: metav1.ObjectMeta{
			Name:      routeName,
			Namespace: namespace,
		},
		Spec: routev1.RouteSpec{
			To: routev1.RouteTargetReference{
				Kind: "Service",
				Name: serviceName,
			},
			Port: &routev1.RoutePort{
				TargetPort: intstr.FromString(targetPort),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   terminationType,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
			},
			WildcardPolicy: routev1.WildcardPolicyNone,
		},
	}
}

func waitForAllRoutesAdmitted(t *testing.T, routeClient *routev1client.RouteV1Client, namespace string, timeout time.Duration) error {
	return wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		// Get all routes in the namespace
		routes, err := routeClient.Routes(namespace).List(context.TODO(), metav1.ListOptions{})
		if err != nil {
			return false, fmt.Errorf("failed to list routes in namespace %s: %v", namespace, err)
		}

		// Check if all routes have been admitted
		for _, route := range routes.Items {
			if len(route.Status.Ingress) == 0 {
				t.Logf("Route %s/%s does not have any Ingress yet", namespace, route.Name)
				return false, nil
			}
			admitted := false
			for _, ingress := range route.Status.Ingress {
				if ingress.RouterCanonicalHostname != "" {
					admitted = true
					break
				}
			}
			if !admitted {
				t.Logf("Route %s/%s has not been admitted yet", namespace, route.Name)
				return false, nil
			}
		}

		t.Logf("All routes in namespace %s have been admitted", namespace)
		return true, nil
	})
}

func makeHTTPRequestToRoute(t *testing.T, url string) error {
	// Create an HTTP client with a timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
		// You may want to disable SSL verification if using self-signed certs
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Make the GET request
	t.Logf("Making GET request to: %s", url)
	resp, err := client.Get(url)
	if err != nil {
		return fmt.Errorf("failed to make GET request: %v", err)
	}
	defer resp.Body.Close()

	// Check that the status code is 200 OK
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: got %d, expected 200", resp.StatusCode)
	}

	t.Logf("GET request to %s returned status %d", url, resp.StatusCode)
	return nil
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

	// Step 3: Define and create service
	serviceName := "ocpbugs48050"
	deploymentName := "ocpbugs48050"

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      serviceName,
			Namespace: namespace.Name,
			Annotations: map[string]string{
				ingress.ServingCertSecretAnnotation:                  "serving-cert-" + namespace.Name,
				"service.beta.openshift.io/serving-cert-secret-name": "serving-cert-" + namespace.Name,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": deploymentName},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt(8080)},
				{Name: "https", Port: 8443, TargetPort: intstr.FromInt(8443)},
			},
		},
	}

	if _, err := kubeClient.CoreV1().Services(namespace.Name).Create(context.TODO(), service, metav1.CreateOptions{}); err != nil {
		t.Fatalf("Failed to create service %s/%s: %v", namespace.Name, serviceName, err)
	}
	t.Logf("Created service %s/%s", namespace.Name, serviceName)

	// Step 4: Define and create deployment
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
							Name:            deploymentName,
							Image:           "quay.io/amcdermo/ocpbugs40850-server:latest",
							ImagePullPolicy: corev1.PullAlways,
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 8080},
								{Name: "https", ContainerPort: 8443},
							},
							Env: []corev1.EnvVar{
								{Name: "HTTP_PORT", Value: "8080"},
								{Name: "HTTPS_PORT", Value: "8443"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(8080),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path: "/healthz",
										Port: intstr.FromInt(8080),
									},
								},
								InitialDelaySeconds: 1,
								PeriodSeconds:       10,
							},
							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "cert",
									MountPath: "/etc/serving-cert",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "serving-cert-" + namespace.Name,
								},
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

	// Step 5: Define and create the single-te route using the helper function
	// singleTERouteName := "ocpbugs40850-single-te"
	// composeRoute(t, routeClient, namespace.Name, singleTERouteName, serviceName, "single-te")

	// Step 1: Define all termination types and corresponding target ports
	var targetPorts = map[routev1.TLSTerminationType]string{
		routev1.TLSTerminationEdge:        "http",  // Edge termination targets HTTP (8080)
		routev1.TLSTerminationReencrypt:   "https", // Reencrypt targets HTTPS (8443)
		routev1.TLSTerminationPassthrough: "https", // Passthrough targets HTTPS (8443)
	}

	var allTerminationTypes = []routev1.TLSTerminationType{
		routev1.TLSTerminationEdge,
		routev1.TLSTerminationReencrypt,
		routev1.TLSTerminationPassthrough,
	}

	// Step 2: Define the number of routes to create for each type (default 3)
	const routeCount = 3

	// Step 3: Create routes for each termination type with the mapped target ports
	for i := 1; i <= routeCount; i++ {
		for _, terminationType := range allTerminationTypes {
			routeName := fmt.Sprintf("%s-%d", string(terminationType), i)
			targetPort := targetPorts[terminationType] // Get the appropriate target port from the map

			// Generate the route object using your composeRouteWithPort function
			route := composeRouteWithPort(namespace.Name, routeName, serviceName, targetPort, terminationType)

			if _, err := routeClient.Routes(namespace.Name).Create(context.TODO(), route, metav1.CreateOptions{}); err != nil {
				t.Fatalf("Failed to create route %s/%s: %v", namespace.Name, routeName, err)
			}
			t.Logf("Created route %s/%s with termination %s", namespace.Name, routeName, string(terminationType))
		}
	}

	// Step 4: Wait for all routes to be admitted in the namespace
	if err := waitForAllRoutesAdmitted(t, routeClient, namespace.Name, 5*time.Minute); err != nil {
		t.Fatalf("Some routes in namespace %s were not admitted: %v", namespace.Name, err)
	}

	t.Logf("All routes in namespace %s have been admitted", namespace.Name)

	// Step 8: Make GET requests to each route
	routes, err := routeClient.Routes(namespace.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		t.Fatalf("Failed to list routes in namespace %s: %v", namespace.Name, err)
	}

	for _, route := range routes.Items {
		if len(route.Status.Ingress) == 0 {
			t.Fatalf("Route %s/%s does not have Ingress", namespace.Name, route.Name)
		}

		// Get the canonical hostname from the Ingress status
		hostname := route.Status.Ingress[0].Host
		url := fmt.Sprintf("https://%s/healthz", hostname)

		// Make GET request to the route
		if err := makeHTTPRequestToRoute(t, url); err != nil {
			t.Errorf("GET request to route %s/%s failed: %v", namespace.Name, route.Name, err)
		} else {
			t.Logf("GET request to route %s/%s succeeded", namespace.Name, route.Name)
		}
	}

	type testCase struct {
		routeName        string
		terminationTypes []routev1.TLSTerminationType
		hits             int
		expectedError    string // Specific expected error message
	}

	// Explicit test cases including both single-te and te-duplicate scenarios
	testCases := []testCase{
		// {
		// 	routeName:        "single-te",
		// 	terminationTypes: allTerminationTypes,
		// 	hits:             5,
		// 	expectedError:    "",
		// },
		// Test cases for te-duplicate, expecting specific errors
		{
			routeName:        "ocpbugs40850-duplicate-te1",
			terminationTypes: allTerminationTypes,
			hits:             5,
			expectedError:    `net/http: HTTP/1.x transport connection broken: too many transfer encodings: ["chunked" "chunked"]`,
		},
		{
			routeName:        "ocpbugs40850-duplicate-te2",
			terminationTypes: []routev1.TLSTerminationType{routev1.TLSTerminationEdge},
			hits:             1,
			expectedError:    `net/http: HTTP/1.x transport connection broken: too many transfer encodings: ["chunked" "chunked"]`,
		},
		{
			routeName:        "ocpbugs40850-duplicate-te3",
			terminationTypes: []routev1.TLSTerminationType{routev1.TLSTerminationReencrypt},
			hits:             1,
			expectedError:    `net/http: HTTP/1.x transport connection broken: too many transfer encodings: ["chunked" "chunked"]`,
		},
		{
			routeName:        "ocpbugs40850-duplicate-te4",
			terminationTypes: []routev1.TLSTerminationType{routev1.TLSTerminationPassthrough},
			hits:             1,
		},
	}

	// Loop through test cases and make requests
	for _, testCase := range testCases {
		for _, terminationType := range testCase.terminationTypes {
			routeName := fmt.Sprintf("%s-%s", testCase.routeName, string(terminationType))

			t.Logf("Testing route %s with termination type %s", routeName, terminationType)
			performHTTPRequestWithExpectedError(t, routeClient, namespace.Name, routeName, testCase.hits, 5*time.Second, testCase.expectedError)
		}
	}

	time.Sleep(time.Hour)
}
