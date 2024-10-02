//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/pointer"

	"github.com/openshift/library-go/test/library/metrics"
	prometheusv1client "github.com/prometheus/client_golang/api/prometheus/v1"
	"github.com/prometheus/common/model"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingress"
)

func singleTransferEncodingResponseCheck(resp *http.Response, err error) error {
	if err != nil {
		return fmt.Errorf("unexpected error: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status code: got %d, expected %d", resp.StatusCode, http.StatusOK)
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

	expectedErrorMsg := `net/http: HTTP/1.x transport connection broken: too many transfer encodings: ["chunked" "chunked"]`
	if !strings.Contains(err.Error(), expectedErrorMsg) {
		return fmt.Errorf("unmatched error: %v; expected message to contain %q", err, expectedErrorMsg)
	}

	return nil
}

func createOCPBUGS48050Service(namespace, name string) (*corev1.Service, error) {
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
		return nil, err
	}

	return &service, nil
}

func createOCPBUGS48050Deployment(namespace, name, image string) (*appsv1.Deployment, error) {
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
							Image:           image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/usr/bin/ingress-operator"},
							Args:            []string{"serve-ocpbugs40850-test-server"},
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
		return nil, err
	}

	return &deployment, nil
}

func createOCPBUGS48050Route(namespace, routeName, serviceName, targetPort string, terminationType routev1.TLSTerminationType) (*routev1.Route, error) {
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
		return nil, fmt.Errorf("failed to create route %s/%s: %v", route.Namespace, route.Name, err)
	}

	return &route, nil
}

func setupOCPBUGS48050(t *testing.T, routeCount int) (*corev1.Namespace, error) {
	var targetPorts = map[routev1.TLSTerminationType]string{
		routev1.TLSTerminationEdge:        "http",
		routev1.TLSTerminationReencrypt:   "https",
		routev1.TLSTerminationPassthrough: "https",
	}

	var allTerminationTypes = []routev1.TLSTerminationType{
		routev1.TLSTerminationEdge,
		routev1.TLSTerminationReencrypt,
		routev1.TLSTerminationPassthrough,
	}

	baseName := "ocpbugs40850-e2e"
	ns := createNamespace(t, baseName+"-"+rand.String(5))

	service, err := createOCPBUGS48050Service(ns.Name, baseName)
	if err != nil {
		return nil, fmt.Errorf("failed to create service %s/%s: %v", ns.Name, baseName, err)
	}

	image, err := getCanaryImageFromIngressOperatorDeployment()
	if err != nil {
		return nil, fmt.Errorf("failed to get canary image: %v", err)
	}

	deployment, err := createOCPBUGS48050Deployment(ns.Name, baseName, image)
	if err != nil {
		return nil, fmt.Errorf("failed to create deployment %s/%s: %v", ns.Name, baseName, err)
	}

	if err := waitForDeploymentComplete(t, kclient, deployment, 5*time.Minute); err != nil {
		return nil, fmt.Errorf("deployment %s/%s not ready: %v", deployment.Namespace, deployment.Name, err)
	}

	makeRouteName := func(terminationType routev1.TLSTerminationType, i int) string {
		return fmt.Sprintf("%s-route-%02d", string(terminationType), i)
	}

	for i := 1; i <= routeCount; i++ {
		for _, terminationType := range allTerminationTypes {
			routeName := makeRouteName(terminationType, i)
			route, err := createOCPBUGS48050Route(ns.Name, routeName, service.Name, targetPorts[terminationType], terminationType)
			if err != nil {
				t.Fatalf("failed to create route %s/%s: %v", ns.Name, routeName, err)
			}
			t.Logf("Created route %s/%s", route.Namespace, route.Name)
		}
	}

	return ns, nil
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

func newPrometheusClient() (prometheusv1client.API, error) {
	kubeConfig, err := config.GetConfig()
	if err != nil {
		return nil, fmt.Errorf("failed to get Kubernetes configuration: %v", err)
	}

	kubeClient, err := kubernetes.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create Kubernetes client: %v", err)
	}

	routeClient, err := routev1client.NewForConfig(kubeConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to create route client: %v", err)
	}

	prometheusClient, err := metrics.NewPrometheusClient(context.TODO(), kubeClient, routeClient)
	if err != nil {
		return nil, fmt.Errorf("failed to create Prometheus client: %v", err)
	}

	return prometheusClient, nil
}

func queryPrometheus(client prometheusv1client.API, query string, timeout time.Duration) (model.Value, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	result, warnings, err := client.Query(ctx, query, time.Now())
	if err != nil {
		return nil, fmt.Errorf("error querying Prometheus: %v", err)
	}

	if len(warnings) > 0 {
		return nil, fmt.Errorf("warnings querying Prometheus: %v", warnings)
	}

	return result, nil
}

type podScrapeCounts map[string]float64

func getTotalScrapesForPods(promClient prometheusv1client.API, timeout time.Duration) (podScrapeCounts, error) {
	query := `sum by (pod) (haproxy_exporter_total_scrapes{pod=~"router-default-.*"})`

	result, err := queryPrometheus(promClient, query, timeout)
	if err != nil {
		return nil, err
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("expected result type model.Vector, got %T", result)
	}

	scrapeCounts := make(map[string]float64)

	for _, sample := range vector {
		podName := string(sample.Metric["pod"])
		scrapeCounts[podName] = float64(sample.Value)
	}

	return scrapeCounts, nil
}

func waitForDefaultRouterScrapesIncrement(promClient prometheusv1client.API, minNewScrapes int, timeout time.Duration, progress func(initialCounts, currentCounts podScrapeCounts, allIncremented bool)) error {
	initialCounts, err := getTotalScrapesForPods(promClient, timeout)
	if err != nil {
		return fmt.Errorf("failed to get initial scrape counts: %v", err)
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			currentCounts, err := getTotalScrapesForPods(promClient, timeout)
			if err != nil {
				return fmt.Errorf("failed to get current scrape counts: %v", err)
			}

			allIncremented := true
			for podName, initialCount := range initialCounts {
				if currentCount, exists := currentCounts[podName]; !exists || currentCount < initialCount+float64(minNewScrapes) {
					allIncremented = false
					break
				}
			}

			if progress != nil {
				progress(initialCounts, currentCounts, allIncremented)
			}

			if allIncremented {
				return nil
			}
		case <-time.After(deadline.Sub(time.Now())):
			return fmt.Errorf("timeout after %v waiting for all pods to increment total scrapes by at least %d", timeout, minNewScrapes)
		}
	}
}

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

func queryPrometheusForDuplicateTEHeaders(promClient prometheusv1client.API, namespace string, timeout time.Duration) (map[string]float64, error) {
	query := fmt.Sprintf(`sum by (route) (haproxy_backend_duplicate_te_header_total{exported_namespace="%s"})`, namespace)

	result, err := queryPrometheus(promClient, query, timeout)
	if err != nil {
		return nil, fmt.Errorf("failed to query Prometheus for duplicate TE headers: %w", err)
	}

	vector, ok := result.(model.Vector)
	if !ok {
		return nil, fmt.Errorf("expected result type model.Vector, got %T", result)
	}

	routeCounts := make(map[string]float64)
	for _, sample := range vector {
		routeName := string(sample.Metric["route"])
		value := float64(sample.Value)
		routeCounts[routeName] = value
	}

	return routeCounts, nil
}

// TestOCPBUGS48050 validates the new metric [1],
// duplicate_te_header_total, which detects duplicate
// Transfer-Encoding headers per backend response. This test addresses
// issues encountered when upgrading from HAProxy 2.2 in OpenShift
// 4.13 to HAProxy 2.6 in OpenShift 4.14, where responses are rejected
// due to RFC 7230 compliance.
//
// [1] https://github.com/openshift/router/pull/626.
func TestOCPBUGS48050(t *testing.T) {
	if err := waitForIngressControllerCondition(t, kclient, 5*time.Minute, defaultName, defaultAvailableConditions...); err != nil {
		t.Fatalf("failed to observe expected conditions: %v", err)
	}

	ns, err := setupOCPBUGS48050(t, 10)
	if err != nil {
		t.Fatalf("failed to setup test resources: %v", err)
	}

	routeList, err := waitForAllRoutesAdmitted(ns.Name, 2*time.Minute, func(admittedRoutes, totalRoutes int, pendingRoutes []string) {
		if len(pendingRoutes) > 0 {
			t.Logf("%d/%d routes admitted. Waiting for: %s", admittedRoutes, totalRoutes, strings.Join(pendingRoutes, ", "))
		} else {
			t.Logf("All %d routes in namespace %s have been admitted", totalRoutes, ns.Name)
		}
	})
	if err != nil {
		t.Fatalf("Error waiting for routes to be admitted: %v", err)
	}

	httpClient := &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	httpGet := func(url string, check func(*http.Response, error) error) error {
		t.Logf("GET %s", url)
		resp, err := httpClient.Get(url)
		if resp != nil {
			defer func(Body io.ReadCloser) {
				_ = Body.Close()
			}(resp.Body)
		}
		return check(resp, err)
	}

	routeIndex := func(name string) int {
		parts := strings.Split(name, "-")
		if len(parts) < 2 {
			t.Fatalf("Unexpected route name format: %q. Expected at least two parts separated by '-'", name)
		}

		index, err := strconv.Atoi(parts[len(parts)-1])
		if err != nil {
			t.Fatalf("Failed to convert route index to integer for route %q: %v", name, err)
		}

		return index
	}

	for _, route := range routeList.Items {
		host := route.Spec.Host

		// Retry only the /healthz endpoint because sometimes
		// we get a 503 status even though the routes have
		// been admitted.
		if err := wait.PollImmediate(time.Second, time.Minute, func() (bool, error) {
			healthzURL := fmt.Sprintf("http://%s/healthz", host)
			if err := httpGet(healthzURL, func(resp *http.Response, err error) error {
				if err != nil {
					t.Logf("Health check error for route %s: %v. Retrying...", host, err)
					return err
				}
				if resp == nil {
					t.Logf("Received a nil response for %s. Retrying...", healthzURL)
					return fmt.Errorf("nil response received")
				}
				if resp.StatusCode != http.StatusOK {
					t.Logf("Unexpected status code for %s: %d. Retrying...", healthzURL, resp.StatusCode)
					return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
				}
				return nil
			}); err != nil {
				return false, nil
			}
			return true, nil
		}); err != nil {
			t.Fatalf("/healthz checks failed for route %s: %v", host, err)
		}

		// Once /healthz passes, proceed with probing
		// /single-te and /duplicate-te endpoints.
		for _, tc := range []struct {
			path            string
			responseChecker func(*http.Response, error) error
		}{
			{"/single-te", singleTransferEncodingResponseCheck},
			{"/duplicate-te", duplicateTransferEncodingResponseCheck},
		} {
			url := fmt.Sprintf("http://%s%s", host, tc.path)
			for j := 1; j <= routeIndex(route.Name); j++ {
				if err := httpGet(url, tc.responseChecker); err != nil {
					t.Fatalf("GET request to %s failed: %v", url, err)
				}
			}
		}
	}

	promClient, err := newPrometheusClient()
	if err != nil {
		t.Fatalf("failed to create prometheus client: %v", err)
	}

	// Wait for > 1 metric scrapes from each router pod. While one
	// scrape is sometimes sufficient, relying on just one scrape
	// can make the overall test flaky.
	newScrapes := 5
	maxScrapeWaitDuration := 5 * time.Minute
	t.Logf("Waiting up to %.0f minutes for router pod metric scrape counts to increment by %d", maxScrapeWaitDuration.Minutes(), newScrapes)
	if err := waitForDefaultRouterScrapesIncrement(promClient, newScrapes, maxScrapeWaitDuration, func(initialCounts, currentCounts podScrapeCounts, allIncremented bool) {
		for podName, initialCount := range initialCounts {
			currentCount := currentCounts[podName]
			targetCount := initialCount + float64(newScrapes)
			status := "in progress"
			if currentCount >= targetCount {
				status = "complete"
			}
			t.Logf("Scrape count progress for pod %s: initial: %.0f, current: %.0f, target: %.0f (%s)",
				podName, initialCount, currentCount, targetCount, status)
		}
	}); err != nil {
		t.Fatalf("Error waiting for scrape counts to increment: %v", err)
	}

	duplicateTECounts, err := queryPrometheusForDuplicateTEHeaders(promClient, ns.Name, time.Minute)
	if err != nil {
		t.Fatalf("Failed to query Prometheus: %v", err)
	}

	t.Log("Detailed results:")
	t.Logf("%-20s %-15s %-15s %-15s %-15s", "Route", "Termination", "Actual Count", "Expected Count", "Match?")

	// For each route, verify that the actual count of duplicate
	// Transfer-Encoding headers matches the expected count. We
	// include all routes, even those with an expected count of
	// zero (i.e., only Passthrough routes), to ensure they do not
	// produce unexpected duplicate headers. Testing routes with
	// an expected count of zero helps us detect false positives
	// in our metrics. By asserting both the existence of the
	// route in the metrics and the correctness of the count, we
	// ensure the test accurately reflects the system's behaviour.
	for _, route := range routeList.Items {
		routeIndex := routeIndex(route.Name)
		var expectedCount float64

		if route.Spec.TLS.Termination != routev1.TLSTerminationPassthrough {
			expectedCount = float64(routeIndex)
		} else {
			expectedCount = 0
		}

		actualCount, exists := duplicateTECounts[route.Name]

		if !exists {
			t.Errorf("Route %s not found in Prometheus results", route.Name)
			continue
		}

		success := actualCount == expectedCount
		t.Logf("%-20s %-15s %-15.0f %-15.0f %-15v",
			route.Name, route.Spec.TLS.Termination, actualCount, expectedCount, success)

		if !success {
			t.Errorf("Mismatch for route %s: expected %.0f, got %.0f", route.Name, expectedCount, actualCount)
		}
	}
}
