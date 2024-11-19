package e2e

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
)

type idleConnectionTestConfig struct {
	namespace   string
	services    []*corev1.Service
	deployments []*appsv1.Deployment
	route       *routev1.Route
	testLabels  map[string]string
	httpClient  *http.Client
}

func idleConnectionTestSetup(t *testing.T, baseName string) (*corev1.Namespace, *idleConnectionTestConfig, error) {
	tc := &idleConnectionTestConfig{
		testLabels: map[string]string{
			"test": "idle-connection",
			"app":  "web-server",
		},
	}

	ns := createNamespace(t, baseName+"-"+rand.String(5))
	tc.namespace = ns.Name

	for i := 1; i <= 2; i++ {
		if err := idleConnectionCreateBackendService(t, tc, i); err != nil {
			return nil, nil, fmt.Errorf("failed to create backend %d: %v", i, err)
		}
	}

	var err error
	tc.route, err = idleConnectionCreateRoute(tc.namespace, "test", tc.services[0].Name, tc.testLabels)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create test route: %v", err)
	}

	return ns, tc, nil
}

func idleConnectionCreateBackendService(t *testing.T, tc *idleConnectionTestConfig, index int) error {
	labels := map[string]string{
		"app":      "web-server",
		"instance": fmt.Sprintf("%d", index),
	}
	for k, v := range tc.testLabels {
		labels[k] = v
	}

	deployment, err := idleConnectionCreateDeployment(tc.namespace, index, labels)
	if err != nil {
		return err
	}
	tc.deployments = append(tc.deployments, deployment)

	if err := waitForDeploymentComplete(t, kclient, deployment, 2*time.Minute); err != nil {
		return fmt.Errorf("deployment %d is not ready: %v", index, err)
	}

	svc, err := idleConnectionCreateService(t, tc.namespace, index, labels)
	if err != nil {
		return err
	}
	tc.services = append(tc.services, svc)

	return nil
}

func idleConnectionCreateDeployment(namespace string, index int, labels map[string]string) (*appsv1.Deployment, error) {
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

func idleConnectionCreateService(_ *testing.T, namespace string, index int, labels map[string]string) (*corev1.Service, error) {
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

func idleConnectionCreateRoute(namespace, name, serviceName string, labels map[string]string) (*routev1.Route, error) {
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

func fetchServiceResponse(t *testing.T, route *routev1.Route, client *http.Client) (string, error) {
	// Construct the URL from the route
	url := fmt.Sprintf("http://%s", route.Spec.Host)

	// Log the request being made
	t.Logf("Making single GET request", "url", url)

	// Perform the HTTP GET request
	resp, err := client.Get(url)
	if err != nil {
		t.Logf("Failed to GET response", "url", url, "error", err)
		return "", fmt.Errorf("failed to GET response from service: %w", err)
	}
	defer resp.Body.Close()

	// Check the HTTP status code
	if resp.StatusCode != http.StatusOK {
		t.Logf("Received non-200 status code", "url", url, "status", resp.StatusCode)
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Logf("Failed to read response body", "url", url, "error", err)
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	// Log the response received
	responseString := string(body)
	t.Logf("Received response", "url", url, "response", responseString)

	return responseString, nil
}

func switchRouteService(t *testing.T, ctx context.Context, tc *idleConnectionTestConfig, serviceIndex int) (*routev1.Route, error) {
	return nil, nil
}

func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	baseName := "idle-close-on-response-e2e"

	// Set up test resources
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, tc, err := idleConnectionTestSetup(t, baseName)
	if err != nil {
		t.Fatalf("failed to set up test resources: %v", err)
	}

	// Define IngressController name and namespace
	icName := types.NamespacedName{
		Name:      "default",
		Namespace: "openshift-ingress-operator",
	}

	// Step 1: Retrieve IdleConnectionTerminationPolicy
	ingressController, err := getIngressController(t, kclient, icName, 1*time.Minute)
	if err != nil {
		t.Fatalf("failed to retrieve IngressController: %v", err)
	}
	initialPolicy := ingressController.Spec.IdleConnectionTerminationPolicy
	t.Logf("Detected IdleConnectionTerminationPolicy: %s", initialPolicy)

	// Step 2: Define expected responses for each policy
	expectedResponses := map[operatorv1.IngressControllerConnectionTerminationPolicy][]string{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred:  {"Response from Service-A", "Response from Service-A", "Response from Service-B"},
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate: {"Response from Service-A", "Response from Service-B", "Response from Service-B"},
	}

	// Step 3: Function to switch IC policy
	switchPolicy := func(t *testing.T, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
		t.Helper()
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			ic, err := getIngressController(t, kclient, icName, 1*time.Minute)
			if err != nil {
				return fmt.Errorf("failed to get IngressController: %w", err)
			}

			ic.Spec.IdleConnectionTerminationPolicy = policy
			if err := kclient.Update(context.TODO(), ic); err != nil {
				t.Logf("Failed to update policy: %v, retrying...", err)
				return err
			}
			return nil
		})
		if err != nil {
			return fmt.Errorf("failed to switch policy to %s: %w", policy, err)
		}

		// Wait for IC policy update to propagate
		time.Sleep(30 * time.Second) // Adjust based on testing latency
		return nil
	}

	// Step 4: Define test actions for each policy
	for _, policy := range []operatorv1.IngressControllerConnectionTerminationPolicy{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
	} {
		t.Run(fmt.Sprintf("Testing policy: %s", policy), func(t *testing.T) {
			if err := switchPolicy(t, policy); err != nil {
				t.Fatalf("failed to switch to policy %s: %v", policy, err)
			}

			// Inline HTTP client creation for the policy
			tc.httpClient = &http.Client{
				Timeout: 10 * time.Second,
				Transport: &http.Transport{
					DisableKeepAlives: policy == operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
				},
			}

			actions := []func(ctx context.Context, tc *idleConnectionTestConfig) (string, error){
				func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
					return fetchServiceResponse(t, tc.route, tc.httpClient) // Initial GET
				},
				func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
					_, err := switchRouteService(t, ctx, tc, 1) // Switch to Service-B
					if err != nil {
						return "", err
					}
					return fetchServiceResponse(t, tc.route, tc.httpClient)
				},
				func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
					return fetchServiceResponse(t, tc.route, tc.httpClient) // Final GET
				},
			}

			for i, action := range actions {
				resp, err := action(ctx, tc)
				if err != nil {
					t.Fatalf("failed during step %d: %v", i+1, err)
				}

				if resp != expectedResponses[policy][i] {
					t.Fatalf("unexpected response at step %d for policy %s: got %s, want %s", i+1, policy, resp, expectedResponses[policy][i])
				}

				t.Logf("Response at step %d for policy %s matches expected: %s", i+1, policy, resp)
			}
		})
	}
}
