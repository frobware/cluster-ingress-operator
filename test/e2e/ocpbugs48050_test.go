package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
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

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	prometheusv1client "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"

	"github.com/openshift/library-go/test/library/metrics"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingress"
)

func makeHTTPRequestToRoute(t *testing.T, url string, timeout time.Duration, check func(*http.Response, error) error) error {
	t.Helper()
	t.Logf("Making GET request to: %s", url)

	client := &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	resp, err := client.Get(url)
	if resp != nil {
		defer resp.Body.Close()
	}

	return check(resp, err)
}

func singleTransferEncodingResponseCheck(resp *http.Response, err error) error {
	if err != nil {
		return fmt.Errorf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: got %v, expected %v", resp.StatusCode, http.StatusOK)
	}

	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %v", err)
	}

	return nil
}

func duplicateTransferEncodingResponseCheck(resp *http.Response, err error) error {
	if err == nil {
		return fmt.Errorf("unexpected success")
	}

	if resp != nil {
		return fmt.Errorf("expected response to be nil")
	}

	expectedError := `net/http: HTTP/1.x transport connection broken: too many transfer encodings: ["chunked" "chunked"]`
	if !strings.Contains(err.Error(), expectedError) {
		return fmt.Errorf("unexpected error: %v; expected: %v", err, expectedError)
	}

	return nil
}

func createOCPBUGS48050Service(t *testing.T, namespace, name string) *corev1.Service {
	t.Helper()

	service := corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Annotations: map[string]string{
				ingress.ServingCertSecretAnnotation:                  "serving-cert-" + namespace,
				"service.beta.openshift.io/serving-cert-secret-name": "serving-cert-" + namespace,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": name},
			Ports: []corev1.ServicePort{
				{Name: "http", Port: 8080, TargetPort: intstr.FromInt(8080)},
				{Name: "https", Port: 8443, TargetPort: intstr.FromInt(8443)},
			},
		},
	}

	if err := kclient.Create(context.TODO(), &service); err != nil {
		t.Fatalf("Failed to create service %s/%s: %v", namespace, name, err)
	}

	t.Logf("Created service %s/%s", service.Namespace, service.Name)

	return &service
}

func createOCPBUGS48050Deployment(t *testing.T, namespace, name string) *appsv1.Deployment {
	t.Helper()

	deployment := appsv1.Deployment{
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
							Name:            name,
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
									SecretName: "serving-cert-" + namespace,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := kclient.Create(context.TODO(), &deployment); err != nil {
		t.Fatalf("Failed to create deployment %s/%s: %v", namespace, name, err)
	}

	t.Logf("Created deployment %s/%s", deployment.Namespace, deployment.Name)

	return &deployment
}

func createOCPBUGS48050Route(t *testing.T, namespace, routeName, serviceName, targetPort string, terminationType routev1.TLSTerminationType) *routev1.Route {
	t.Helper()

	route := routev1.Route{
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

	if err := kclient.Create(context.TODO(), &route); err != nil {
		t.Fatalf("Failed to create route %s/%s: %v", route.Namespace, route.Name, err)
	}

	t.Logf("Created route %s/%s with termination %s", route.Namespace, route.Name, string(terminationType))

	return &route
}

func waitForAllRoutesAdmitted(t *testing.T, kclient client.Client, namespace string, timeout time.Duration) error {
	isRouteAdmitted := func(route *routev1.Route) bool {
		for _, ingress := range route.Status.Ingress {
			if ingress.RouterCanonicalHostname != "" {
				return true
			}
		}
		return false
	}

	return wait.PollImmediate(6*time.Second, timeout, func() (bool, error) {
		var routeList routev1.RouteList
		if err := kclient.List(context.TODO(), &routeList, client.InNamespace(namespace)); err != nil {
			return false, fmt.Errorf("failed to list routes in namespace %s: %v", namespace, err)
		}

		for _, route := range routeList.Items {
			if !isRouteAdmitted(&route) {
				t.Logf("Route %s/%s has not been admitted yet, retrying...", route.Namespace, route.Name)
				return false, nil
			}
		}
		return true, nil
	})
}

func newPrometheusClient(t *testing.T) prometheusv1client.API {
	t.Helper()

	// Create a new prometheus client for fetching metrics.
	kubeConfig, err := config.GetConfig()
	if err != nil {
		t.Fatalf("failed to get kube config: %s", err)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatal(err)
	}

	routeClient, err := routev1client.NewForConfig(kubeConfig)
	if err != nil {
		t.Fatal(err)
	}

	prometheusClient, err := metrics.NewPrometheusClient(context.TODO(), kubeClient, routeClient)
	if err != nil {
		t.Fatal(err)
	}

	return prometheusClient
}

func queryPrometheus(t *testing.T, client prometheusv1client.API, query string, duration *time.Duration) (model.Value, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var result model.Value
	var warnings prometheusv1client.Warnings
	var err error

	if duration == nil {
		// Instant query
		result, warnings, err = client.Query(ctx, query, time.Now())
	} else {
		// Range query
		end := time.Now()
		start := end.Add(-*duration)
		step := time.Minute // Adjust step size as needed

		r := prometheusv1client.Range{
			Start: start,
			End:   end,
			Step:  step,
		}
		result, warnings, err = client.QueryRange(ctx, query, r)
	}

	if err != nil {
		return nil, fmt.Errorf("error querying Prometheus: %v", err)
	}
	if len(warnings) > 0 {
		t.Logf("Prometheus query warnings: %v", warnings)
	}

	return result, nil
}

func getTerminationType(routeName string) string {
	if strings.HasPrefix(routeName, "edge-") {
		return "edge"
	} else if strings.HasPrefix(routeName, "reencrypt-") {
		return "reencrypt"
	} else if strings.HasPrefix(routeName, "passthrough-") {
		return "passthrough"
	}
	return "unknown"

}

func TestOCPBUGS48050(t *testing.T) {
	baseName := "ocpbugs40850"
	namespace := createNamespace(t, fmt.Sprintf("%v-%v", baseName, rand.String(5)))
	service := createOCPBUGS48050Service(t, namespace.Name, baseName)
	deployment := createOCPBUGS48050Deployment(t, namespace.Name, baseName)

	if err := waitForDeploymentComplete(t, kclient, deployment, 3*time.Minute); err != nil {
		t.Fatalf("Deployment %s/%s not ready: %v", deployment.Namespace, deployment.Name, err)
	}

	t.Logf("Deployment %s/%s is ready", deployment.Namespace, deployment.Name)

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

	// Step 2: Define the number of routes to create for each type
	const routeCount = 10

	// Step 3: Create routes for each termination type with the mapped target ports
	for i := 0; i < routeCount; i++ {
		for _, terminationType := range allTerminationTypes {
			routeName := fmt.Sprintf("%s-route-%02d", string(terminationType), i)
			createOCPBUGS48050Route(t, namespace.Name, routeName, service.Name, targetPorts[terminationType], terminationType)
		}
	}

	if err := waitForAllRoutesAdmitted(t, kclient, namespace.Name, 3*time.Minute); err != nil {
		t.Fatalf("Some routes in namespace %s were not admitted: %v", namespace.Name, err)
	}

	t.Logf("All routes in namespace %s have been admitted", namespace.Name)

	// Step 8: List the routeList and hit each route's /single-te
	// endpoints and /duplicate-te for even-numbered routeList.
	routeList := routev1.RouteList{}
	if err := kclient.List(context.TODO(), &routeList, []client.ListOption{client.InNamespace(namespace.Name)}...); err != nil {
		t.Fatalf("Failed to list routes in namespace %s: %v", namespace.Name, err)
	}

	domainName := func(fqdn string) string {
		index := strings.Index(fqdn, ".")
		return fqdn[index+1:]
	}

	domain := domainName(routeList.Items[0].Status.Ingress[0].Host)

	for _, terminationType := range allTerminationTypes {
		for i := 0; i < routeCount; i++ {
			hostname := fmt.Sprintf("%s-route-%02d-%s.%s", string(terminationType), i, namespace.Name, domain)

			// Hit the /single-te endpoint for all routes.
			singleTeURL := fmt.Sprintf("http://%s/single-te", hostname)
			if err := makeHTTPRequestToRoute(t, singleTeURL, 30*time.Second, singleTransferEncodingResponseCheck); err != nil {
				t.Fatalf("GET request to /single-te for route %s/%s failed: %v", namespace.Name, hostname, err)
			}

			// Only hit the /duplicate-te endpoint for
			// odd-numbered routes so that we can assert
			// in the prometheus query that we get hits
			// against known routes and not necessarily
			// against all routes related to
			// /duplicate-te.
			if i%2 == 1 {
				duplicateTeURL := fmt.Sprintf("http://%s/duplicate-te", hostname)
				for j := 0; j < i; j++ {
					if err := makeHTTPRequestToRoute(t, duplicateTeURL, 30*time.Second, duplicateTransferEncodingResponseCheck); err != nil {
						t.Fatalf("GET request to /duplicate-te for route %s/%s failed: %v", namespace.Name, hostname, err)
					}
				}
			}

		}
	}

	promClient := newPrometheusClient(t)

	// Get initial total scrapes for all pods.
	initialCounts := getTotalScrapesForPods(t, promClient)

	// Wait for increments.
	waitForTotalScrapesIncrement(t, promClient, initialCounts, 5*time.Minute)

	for _, terminationType := range allTerminationTypes {
		query := fmt.Sprintf(`sum by (route) (haproxy_backend_duplicate_te_header_total{exported_namespace="%s", route=~"%s-route-.*"})`, namespace, string(terminationType))
		t.Logf("Prometheus query: %s", query)

		result, err := queryPrometheus(t, promClient, query, nil)
		if err != nil {
			t.Fatalf("Failed to query Prometheus for duplicate TE headers: %v", err)
		}

		vector, ok := result.(model.Vector)
		if !ok {
			t.Fatalf("Expected result type model.Vector, got %T", result)
		}

		routeCounts := make(map[string]float64)
		for _, sample := range vector {
			routeName := string(sample.Metric["route"])
			value := float64(sample.Value)
			routeCounts[routeName] = value
			t.Logf("Route %s: Sample Count %f", routeName, value)
		}

		for i := 0; i < routeCount; i++ {
			routeName := fmt.Sprintf("%s-route-%02d", string(terminationType), i)
			var expectedCount float64
			if terminationType == "passthrough" {
				expectedCount = 0
			} else if i%2 == 1 {
				expectedCount = float64(i)
			} else {
				expectedCount = 0
			}

			if count, exists := routeCounts[routeName]; exists {
				if count != expectedCount {
					t.Errorf("Expected count for route %s to be %f, got %f", routeName, expectedCount, count)
				}
			} else {
				t.Errorf("Route %s not found in results", routeName)
			}
		}
	}

	t.Logf("Prometheus metrics validation completed successfully")
}

func getTotalScrapesForPods(t *testing.T, promClient prometheusv1client.API) map[string]float64 {
	query := `sum by (pod) (haproxy_exporter_total_scrapes{pod=~"router-default-.*"})`
	result, err := queryPrometheus(t, promClient, query, nil)
	if err != nil {
		t.Fatalf("Failed to query Prometheus for total scrapes: %v", err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		t.Fatalf("Expected result type model.Vector, got %T", result)
	}

	scrapeCounts := make(map[string]float64)
	for _, sample := range vector {
		podName := string(sample.Metric["pod"])
		scrapeCounts[podName] = float64(sample.Value)
	}

	t.Logf("Current scrape counts: %+v", scrapeCounts) // Log current scrape counts
	return scrapeCounts
}

func waitForTotalScrapesIncrement(t *testing.T, promClient prometheusv1client.API, initialCounts map[string]float64, timeout time.Duration) {
	start := time.Now()
	for time.Since(start) < timeout {
		currentCounts := getTotalScrapesForPods(t, promClient)
		allIncremented := true

		for podName, initialCount := range initialCounts {
			if currentCount, exists := currentCounts[podName]; !exists || currentCount <= initialCount {
				allIncremented = false
				t.Logf("Waiting for pod %s to increment (initial: %f, current: %f)", podName, initialCount, currentCounts[podName]) // Log progress
				break
			}
		}

		if allIncremented {
			t.Log("All pods have incremented total scrapes.") // Log success
			return                                            // All pods have incremented
		}

		// Sleep before retrying
		time.Sleep(2 * time.Second)
	}

	t.Fatalf("Timeout waiting for all pods to increment total scrapes")
}

func TestFoo_old_old(t *testing.T) {
	namespace := corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "ocpbugs40850-jmz66"}}

	promClient := newPrometheusClient(t)

	query := fmt.Sprintf(`haproxy_backend_duplicate_te_header_total{exported_namespace="%s"}[1d]`, namespace.Name)
	query = `haproxy_backend_duplicate_te_header_total{exported_namespace="ocpbugs40850-jmz66"}[1d]`
	query = `haproxy_backend_duplicate_te_header_total{exported_namespace="ocpbugs40850-jmz66"}`

	result, err := queryPrometheus(t, promClient, query, nil)
	if err != nil {
		t.Fatalf("Failed to query Prometheus for duplicate TE headers: %v", err)
	}

	fmt.Println(result.Type())
	// // Check each route's value against our expectations
	// for _, sample := range result {
	// 	routeName := string(sample.Metric["route"])
	// 	actualValue := int(sample.Value)
	// 	//expectedValue, exists := expectedDuplicateTeHeadersByRoute[routeName]

	// 	// if !exists {
	// 	// 	t.Errorf("Unexpected route in Prometheus results: %s", routeName)
	// 	// 	continue
	// 	// }

	// 	// if actualValue != expectedValue {
	// 	// 	t.Errorf("Unexpected count for route %s. Got %d, expected %d", routeName, actualValue, expectedValue)
	// 	// }

	// 	fmt.Println(routeName, actualValue)
	// 	// Additional assertion for passthrough routes
	// 	if getTerminationType(routeName) == "passthrough" && actualValue != 0 {
	// 		t.Errorf("Passthrough route %s has non-zero value: %d", routeName, actualValue)
	// 	}
	// }
}

func TestFoo(t *testing.T) {
	namespace := "ocpbugs40850-jmz66"
	promClient := newPrometheusClient(t)
	query := fmt.Sprintf(`haproxy_backend_duplicate_te_header_total{exported_namespace="%s"}`, namespace)

	t.Logf("Prometheus query: %s", query)

	result, err := queryPrometheus(t, promClient, query, nil)
	if err != nil {
		t.Fatalf("Failed to query Prometheus for duplicate TE headers: %v", err)
	}

	fmt.Println(result)
	// Create a map to store expected values
	// expectedValues := make(map[string]int)

	// // Compute expected values
	// for i := 1; i <= 10; i++ {
	// 	for _, prefix := range []string{"reencrypt-route-", "edge-route-", "passthrough-route-"} {
	// 		routeName := fmt.Sprintf("%s%02d", prefix, i)
	// 		if prefix == "passthrough-route-" {
	// 			expectedValues[routeName] = 0
	// 		} else if i%2 == 0 {
	// 			if prefix == "reencrypt-route-" {
	// 				expectedValues[routeName] = i*2 + 2
	// 			} else {
	// 				expectedValues[routeName] = i
	// 			}
	// 		} else {
	// 			expectedValues[routeName] = 0
	// 		}
	// 	}
	// }

	// Verify results
	// for _, sample := range result {
	// 	routeName := string(sample.Metric["route"])
	// 	actualValue := int(sample.Value)
	// 	expectedValue, exists := expectedValues[routeName]

	// 	if !exists {
	// 		t.Errorf("Unexpected route in Prometheus results: %s", routeName)
	// 		continue
	// 	}

	// 	if actualValue != expectedValue {
	// 		t.Errorf("Unexpected count for route %s. Got %d, expected %d", routeName, actualValue, expectedValue)
	// 	}

	// 	// Additional assertion for passthrough routes
	// 	if strings.HasPrefix(routeName, "passthrough-route-") && actualValue != 0 {
	// 		t.Errorf("Passthrough route %s has non-zero value: %d", routeName, actualValue)
	// 	}

	// 	// For debugging: print each route and its value
	// 	t.Logf("%s %d", routeName, actualValue)

	// 	// Remove the route from expectedValues to track which routes we've seen
	// 	delete(expectedValues, routeName)
	// }

	// Check for any routes that were expected but not found in the results
	// for routeName, expectedValue := range expectedValues {
	// 	t.Errorf("Expected route %s with value %d not found in Prometheus results", routeName, expectedValue)
	// }
}

func TestFoo_old(t *testing.T) {
	namespace := "ocpbugs40850-gbfkv"

	promClient := newPrometheusClient(t)

	// Get initial total scrapes for all pods
	initialCounts := getTotalScrapesForPods(t, promClient)

	// Wait for increments
	waitForTotalScrapesIncrement(t, promClient, initialCounts, 5*time.Minute)

	var allTerminationTypes = []routev1.TLSTerminationType{
		routev1.TLSTerminationEdge,
		routev1.TLSTerminationReencrypt,
		routev1.TLSTerminationPassthrough,
	}

	for _, terminationType := range allTerminationTypes {
		query := fmt.Sprintf(`sum by (route) (haproxy_backend_duplicate_te_header_total{exported_namespace="%s", route=~"%s-route-.*"})`, namespace, string(terminationType))
		t.Logf("Prometheus query: %s", query)

		result, err := queryPrometheus(t, promClient, query, nil)
		if err != nil {
			t.Fatalf("Failed to query Prometheus for duplicate TE headers: %v", err)
		}

		vector, ok := result.(model.Vector)
		if !ok {
			t.Fatalf("Expected result type model.Vector, got %T", result)
		}

		routeCounts := make(map[string]float64)
		for _, sample := range vector {
			routeName := string(sample.Metric["route"])
			value := float64(sample.Value)
			routeCounts[routeName] = value
			t.Logf("Route %s: Sample Count %f", routeName, value)
		}

		for i := 0; i < 10; i++ {
			routeName := fmt.Sprintf("%s-route-%02d", string(terminationType), i)
			var expectedCount float64
			if terminationType == "passthrough" {
				expectedCount = 0
			} else if i%2 == 1 {
				expectedCount = float64(i)
			} else {
				expectedCount = 0
			}

			if count, exists := routeCounts[routeName]; exists {
				if count != expectedCount {
					t.Errorf("Expected count for route %s to be %f, got %f", routeName, expectedCount, count)
				}
			} else {
				t.Errorf("Route %s not found in results", routeName)
			}
		}
	}

	t.Logf("Prometheus metrics validation completed successfully")
}
