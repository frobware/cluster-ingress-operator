package e2e

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/http"
	"sort"
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
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	routev1 "github.com/openshift/api/route/v1"
	routev1client "github.com/openshift/client-go/route/clientset/versioned/typed/route/v1"
	"github.com/openshift/cluster-ingress-operator/pkg/operator/controller/ingress"
)

func isRouteAdmitted(route *routev1.Route) bool {
	if len(route.Status.Ingress) == 0 {
		return false
	}

	for _, ingress := range route.Status.Ingress {
		if ingress.RouterCanonicalHostname != "" {
			return true
		}
	}

	return false
}

func waitForRouteAdmitted(t *testing.T, routeClient *routev1client.RouteV1Client, namespace, routeName string, timeout time.Duration) error {
	t.Helper()

	return wait.PollImmediate(5*time.Second, timeout, func() (bool, error) {
		route, err := routeClient.Routes(namespace).Get(context.TODO(), routeName, metav1.GetOptions{})
		if err != nil {
			t.Logf("Error fetching route %s/%s: %v", namespace, routeName, err)
			return false, err
		}

		admitted := isRouteAdmitted(route)
		if admitted {
			t.Logf("Route %s/%s has been admitted", namespace, routeName)
		} else {
			t.Logf("Waiting for route %s/%s to be admitted", namespace, routeName)
		}

		return admitted, nil
	})
}

func waitForAllRoutesAdmittedInNamespace(t *testing.T, namespace string, timeout time.Duration) error {
	t.Helper()

	return wait.PollImmediate(6*time.Second, timeout, func() (bool, error) {
		routeList := routev1.RouteList{}
		if err := kclient.List(context.TODO(), &routeList, []client.ListOption{client.InNamespace(namespace)}...); err != nil {
			return false, fmt.Errorf("failed to list routes in namespace %s: %v", namespace, err)
		}

		for _, route := range routeList.Items {
			if !isRouteAdmitted(&route) {
				t.Logf("Route %s/%s has not been admitted yet", namespace, route.Name)
				return false, nil
			}
		}

		return true, nil
	})
}

// createNamespaceWithSuffix creates a namespace with a random suffix.
func createNamespaceWithSuffix(t *testing.T, baseName string) *corev1.Namespace {
	t.Helper()

	ns := corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: fmt.Sprintf("%v-%v", baseName, rand.String(5)),
		},
	}

	if err := kclient.Create(context.TODO(), &ns); err != nil {
		t.Fatalf("Failed to create namespace: %v", err)
	}

	t.Cleanup(func() {
		t.Logf("Deleting namespace %v...", ns.Name)
		if err := kclient.Delete(context.TODO(), &ns); err != nil {
			t.Errorf("Failed to delete namespace %v: %v", ns.Name, err)
		}
	})

	t.Logf("Created namespace %v...", ns.Name)

	return &ns
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

	t.Logf("Created route %s/%s with termination %s", route.Name, route.Name, string(terminationType))

	return &route
}

func makeHTTPRequestToRoute(t *testing.T, url string, check func(*http.Response, error) error) error {
	t.Helper()

	// Create an HTTP client with a timeout
	client := &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}

	// Make the GET request
	t.Logf("Making GET request to: %s", url)

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

	return &deployment
}

func TestOCPBUGS48050(t *testing.T) {
	namespace := createNamespaceWithSuffix(t, "ocpbugs48050")
	service := createOCPBUGS48050Service(t, namespace.Name, "ocpbugs48050")
	deployment := createOCPBUGS48050Deployment(t, namespace.Name, "ocpbugs48050")

	if err := wait.PollImmediate(time.Second, 2*time.Minute, func() (bool, error) {
		id := types.NamespacedName{
			Namespace: deployment.Namespace,
			Name:      deployment.Name,
		}
		if err := kclient.Get(context.TODO(), id, deployment); err != nil {
			return false, err
		}
		if deployment.Status.AvailableReplicas == *deployment.Spec.Replicas {
			return true, nil
		}
		t.Logf("Waiting for deployment %s/%s to be ready...", deployment.Namespace, deployment.Name)
		return false, nil
	}); err != nil {
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
	for i := 1; i <= routeCount; i++ {
		for _, terminationType := range allTerminationTypes {
			routeName := fmt.Sprintf("%s-route-%02d", string(terminationType), i)
			targetPort := targetPorts[terminationType]
			createOCPBUGS48050Route(t, namespace.Name, routeName, service.Name, targetPort, terminationType)
		}
	}

	// Step 4: Wait for all routes to be admitted in the namespace
	if err := waitForAllRoutesAdmittedInNamespace(t, namespace.Name, 5*time.Minute); err != nil {
		t.Fatalf("Some routes in namespace %s were not admitted: %v", namespace.Name, err)
	}

	t.Logf("All routes in namespace %s have been admitted", namespace)

	// Step 8: List the routeList and hit each route's /single-te
	// endpoints and /duplicate-te for even-numbered routeList.
	routeList := routev1.RouteList{}
	if err := kclient.List(context.TODO(), &routeList, []client.ListOption{client.InNamespace(namespace.Name)}...); err != nil {
		t.Fatalf("Failed to list routes in namespace %s: %v", namespace.Name, err)
	}

	sort.Slice(routeList.Items, func(i, j int) bool {
		return routeList.Items[i].Name < routeList.Items[j].Name
	})

	for i, route := range routeList.Items {
		t.Logf("Processing route: %s (index: %d)", route.Name, i+1)

		if !isRouteAdmitted(&route) {
			t.Fatalf("Route %s/%s is not admitted", route.Namespace, route.Name)
		}

		// Get the canonical hostname from the Ingress status
		hostname := route.Status.Ingress[0].Host

		// Hit the /single-te endpoint for all routes.
		singleTeURL := fmt.Sprintf("https://%s/single-te", hostname)
		if err := makeHTTPRequestToRoute(t, singleTeURL, singleTransferEncodingResponseCheck); err != nil {
			t.Fatalf("GET request to /single-te for route %s/%s failed: %v", namespace.Name, route.Name, err)
		}

		// Only hit the /duplicate-te endpoint for
		// even-numbered routes so that we can assert in the
		// prometheus query that we get hits against known
		// routes and not necessarily against all routes
		// related to /duplicate-te.
		if (i+1)%2 == 0 {
			duplicateTeURL := fmt.Sprintf("https://%s/duplicate-te", hostname)
			for j := 0; j < i+1; j++ {
				if err := makeHTTPRequestToRoute(t, duplicateTeURL, duplicateTransferEncodingResponseCheck); err != nil {
					t.Fatalf("GET request to /duplicate-te for route %s/%s failed: %v", namespace.Name, route.Name, err)
				}
			}
		}
	}

	time.Sleep(time.Hour)
}
