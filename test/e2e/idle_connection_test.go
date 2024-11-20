// XXXnothingXXX
// XXXmorenothingXXX

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/ptr"

	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
)

const (
	idleConnectionResponseServiceA = "Service A"
	idleConnectionResponseServiceB = "Service B"
)

type idleConnectionTestConfig struct {
	deployments   []*appsv1.Deployment
	httpClient    *http.Client
	kubeClientset *kubernetes.Clientset
	kubeConfig    *rest.Config
	namespace     string
	pods          []*corev1.Pod
	route         *routev1.Route
	services      []*corev1.Service
	testLabels    map[string]string
}

// haproxyBackend represents an HAProxy backend configuration section
// with its associated settings and servers.
type haproxyBackend struct {
	name     string   // Name of the backend as defined in HAProxy config.
	settings []string // Non-server settings.
	servers  []string // Server entries in this backend.
}

// waitWithTimeout is a test helper that wraps wait operations
// requiring a context deadline. Instead of manually creating contexts
// with timeouts throughout test code, which can lead to easy mistakes
// with deferred cancellations, this helper encapsulates the
// boilerplate.
//
// The helper is designed for use in tests where:
// - Multiple wait operations occur in sequence
// - Each wait needs its own timeout
// - Deferred cancellations would stack up
// - Context creation/cleanup would clutter test logic
// - Local variables would be needed just to hold intermediate state
//
// Example usage:
//
//	if err := waitWithTimeout(time.Minute, func(ctx context.Context) error {
//	    return waitForRouteAdmitted(t, ctx, "default", route)
//	}); err != nil {
//	    t.Fatalf("route not admitted: %v", err)
//	}
func waitWithTimeout(timeout time.Duration, waitFunc func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return waitFunc(ctx)
}

// getHAProxyConfigFromRouterPod retrieves the HAProxy configuration
// from a pod.
func getHAProxyConfigFromRouterPod(t *testing.T, pod *corev1.Pod) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := podExec(t, *pod, &stdout, &stderr, []string{"cat", "/var/lib/haproxy/conf/haproxy.config"}); err != nil {
		return "", fmt.Errorf("failed to get HAProxy config from pod %s/%s: %w\nstderr: %s", pod.Namespace, pod.Name, err, stderr.String())
	}

	return stdout.String(), nil
}

// parseHAProxyConfig parses raw HAProxy configuration content and
// extracts backend sections. Returns an error if the config is
// malformed or cannot be parsed.
func parseHAProxyConfig(content string) ([]haproxyBackend, error) {
	var (
		backends       []haproxyBackend
		currentBackend *haproxyBackend
	)

	scanner := bufio.NewScanner(strings.NewReader(content))
	lineNum := 0

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmedLine := strings.TrimSpace(line)

		if trimmedLine == "" {
			continue
		}

		if strings.HasPrefix(trimmedLine, "backend ") {
			if currentBackend != nil {
				backends = append(backends, *currentBackend)
			}

			name := strings.TrimSpace(strings.TrimPrefix(trimmedLine, "backend"))
			if name == "" {
				return nil, fmt.Errorf("empty backend name on line %d", lineNum)
			}

			currentBackend = &haproxyBackend{
				name:     name,
				settings: []string{},
				servers:  []string{},
			}

			continue
		}

		if currentBackend == nil {
			continue
		}

		if strings.HasPrefix(trimmedLine, "server ") {
			currentBackend.servers = append(currentBackend.servers, trimmedLine)
		} else {
			currentBackend.settings = append(currentBackend.settings, trimmedLine)
		}
	}

	if currentBackend != nil {
		backends = append(backends, *currentBackend)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading HAProxy config: %w", err)
	}

	if len(backends) == 0 {
		return nil, errors.New("no backends found in configuration")
	}

	return backends, nil
}

// getPodsWithLabels retrieves pods matching the specified label
// selector.
func getPodsWithLabels(ctx context.Context, kclient client.Client, namespace string, labelSelector string) ([]corev1.Pod, error) {
	selector, err := labels.Parse(labelSelector)
	if err != nil {
		return nil, fmt.Errorf("failed to parse label selector %q: %w", labelSelector, err)
	}

	var podList corev1.PodList
	if err := kclient.List(ctx, &podList, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: selector}); err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s with label selector %q: %w", namespace, labelSelector, err)
	}

	return podList.Items, nil
}

// findHAProxyBackendWithServiceServer searches for a specific backend
// name that contains a server referencing the given service name in
// the HAProxy config. Returns the matching backend and true if found,
// or an empty backend and false if not found.
func findHAProxyBackendWithServiceServer(backends []haproxyBackend, expectedBackendName, expectedServiceName string) (haproxyBackend, bool) {
	if expectedBackendName == "" || expectedServiceName == "" {
		return haproxyBackend{}, false
	}

	for _, b := range backends {
		if b.name == expectedBackendName {
			for _, server := range b.servers {
				if strings.Contains(server, expectedServiceName) {
					return b, true
				}
			}
		}
	}

	return haproxyBackend{}, false
}

// waitForHAProxyConfigUpdate polls until the HAProxy configuration
// matches the expected state across all router pods matching
// podSelector, or until the context is cancelled. The operation is
// retried every 7 seconds until success or timeout.
//
// For each poll iteration:
// - Lists pods matching the podSelector
// - For each pod, fetches and verifies HAProxy config
// - Checks if the config contains the expected backend and server entries
//
// Individual operation timeouts are kept short to avoid getting
// stuck, but failures will continue to retry until the parent context
// timeout.
//
// Parameters:
//   - ctx: parent context for overall timeout control
//   - t: testing context for logging
//   - kclient: Kubernetes client for pod operations
//   - restConfig: Kubernetes REST config for pod exec
//   - podSelector: label selector for finding router pods
//   - expectedBackendName: HAProxy backend name to match
//   - expectedServerName: HAProxy server entry to match
//
// Returns an error if:
//   - No matching pods are found
//   - Client creation fails
//   - Context is cancelled before success
//   - A fatal error occurs during polling
func waitForHAProxyConfigUpdate(ctx context.Context, t *testing.T, kclient client.Client, restConfig *rest.Config, podSelector string, expectedBackendName, expectedServerName string) error {
	_, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return wait.PollUntilContextCancel(ctx, 7*time.Second, true, func(ctx context.Context) (bool, error) {
		var pods []corev1.Pod
		if err := waitWithTimeout(15*time.Second, func(timeoutCtx context.Context) error {
			var err error
			pods, err = getPodsWithLabels(timeoutCtx, kclient, "openshift-ingress", podSelector)
			return err
		}); err != nil {
			t.Logf("Failed to get pods: %v", err)
			return false, nil // Return false to keep polling
		}

		if len(pods) == 0 {
			return false, fmt.Errorf("no pods found in namespace %s for selector %q", "openshift-ingress", podSelector)
		}

		allPodsMatch := true
		for i := range pods {
			pod := &pods[i]
			haproxyConfig, err := getHAProxyConfigFromRouterPod(t, pod)
			if err != nil {
				t.Logf("Failed to get HAProxy config from pod %s/%s (pod may be restarting): %v", pod.Namespace, pod.Name, err)
				allPodsMatch = false
				continue
			}

			backends, err := parseHAProxyConfig(haproxyConfig)
			if err != nil {
				t.Logf("Failed to parse HAProxy config from pod %s/%s: %v", pod.Namespace, pod.Name, err)
				allPodsMatch = false
				continue
			}

			backend, found := findHAProxyBackendWithServiceServer(backends, expectedBackendName, expectedServerName)
			if !found {
				allPodsMatch = false
				t.Logf("Waiting for backend %q in pod [#%d/%d] %s/%s", expectedBackendName, i+1, len(pods), pod.Namespace, pod.Name)
				continue
			}

			t.Logf("Found HAProxy backend in pod %s/%s:\nBackend: %s\nServers: %s", pod.Namespace, pod.Name, expectedBackendName, strings.Join(backend.servers, "\n  "))
		}

		return allPodsMatch, nil
	})
}

// routeStatusAdmitted returns true if a given route's status shows
// admitted by the Ingress Controller.
func routeStatusAdmitted(route routev1.Route, ingressControllerName string) bool {
	for _, ingress := range route.Status.Ingress {
		if ingress.RouterName == ingressControllerName {
			for _, cond := range ingress.Conditions {
				if cond.Type == routev1.RouteAdmitted && cond.Status == corev1.ConditionTrue {
					return true
				}
			}

			return false
		}
	}

	return false
}

func waitForRouteAdmitted(ctx context.Context, t *testing.T, ingressName string, route *routev1.Route) error {
	return wait.PollUntilContextCancel(ctx, 2*time.Second, true, func(ctx context.Context) (bool, error) {
		if err := kclient.Get(ctx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, route); err != nil {
			return false, fmt.Errorf("failed to get route %s/%s: %w", route.Namespace, route.Name, err)
		}

		if routeStatusAdmitted(*route, ingressName) {
			t.Logf("Route %s/%s has been admitted", route.Namespace, route.Name)
			return true, nil
		}

		t.Logf("Waiting for route %s/%s to be admitted", route.Namespace, route.Name)
		return false, nil
	})
}

func getCanaryImageFromIngressOperatorDeployment(ctx context.Context) (string, error) {
	ingressOperator := types.NamespacedName{Namespace: operatorNamespace, Name: "ingress-operator"}

	deployment := appsv1.Deployment{}
	if err := kclient.Get(ctx, ingressOperator, &deployment); err != nil {
		return "", fmt.Errorf("failed to get deployment %s/%s: %w", ingressOperator.Namespace, ingressOperator.Name, err)
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

func fetchPodsForServices(ctx context.Context, namespace string, service *corev1.Service) ([]*corev1.Pod, error) {
	podList := &corev1.PodList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels(service.Spec.Selector),
	}

	if err := kclient.List(ctx, podList, listOptions...); err != nil {
		return nil, fmt.Errorf("failed to list pods for service %s/%s: %w", service.Namespace, service.Name, err)
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found for service %s/%s", service.Namespace, service.Name)
	}

	pods := make([]*corev1.Pod, len(podList.Items))
	for i := range podList.Items {
		pods[i] = &podList.Items[i]
	}

	return pods, nil
}

func idleConnectionTestSetup(ctx context.Context, t *testing.T, namespace string) (*corev1.Namespace, *idleConnectionTestConfig, error) {
	tc := &idleConnectionTestConfig{
		testLabels: map[string]string{
			"test": "idle-connection",
			"app":  "web-server",
		},
	}

	cfg, err := config.GetConfig()
	if err != nil {
		return nil, nil, fmt.Errorf("failed to get config: %w", err)
	}

	kubeClientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	tc.kubeConfig = cfg
	tc.kubeClientset = kubeClientset

	ns := createNamespace(t, namespace)
	tc.namespace = ns.Name

	if err := idleConnectionCreateBackendService(ctx, t, tc, 1, idleConnectionResponseServiceA); err != nil {
		return nil, nil, fmt.Errorf("failed to create backend 1: %v", err)
	}

	if err := idleConnectionCreateBackendService(ctx, t, tc, 2, idleConnectionResponseServiceB); err != nil {
		return nil, nil, fmt.Errorf("failed to create backend 2: %v", err)
	}

	tc.route, err = idleConnectionCreateRoute(ctx, tc.namespace, "test", tc.services[0].Name, tc.testLabels)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create test route: %v", err)
	}

	if err := waitWithTimeout(time.Minute, func(ctx context.Context) error {
		return waitForRouteAdmitted(ctx, t, "default", tc.route)
	}); err != nil {
		return nil, nil, fmt.Errorf("error waiting for route to be admitted: %v", err)
	}

	for _, svc := range tc.services {
		pods, err := fetchPodsForServices(ctx, tc.namespace, svc)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to fetch pods for service %s: %v", svc.Name, err)
		}
		tc.pods = append(tc.pods, pods...)
	}

	return ns, tc, nil
}

func idleConnectionCreateBackendService(ctx context.Context, t *testing.T, tc *idleConnectionTestConfig, index int, serverResponse string) error {
	serviceLabels := map[string]string{
		"app":      "web-server",
		"instance": fmt.Sprintf("%d", index),
	}
	for k, v := range tc.testLabels {
		serviceLabels[k] = v
	}

	svc, err := idleConnectionCreateService(ctx, tc.namespace, index, serviceLabels)
	if err != nil {
		return fmt.Errorf("failed to create service %d: %v", index, err)
	}
	tc.services = append(tc.services, svc)

	deployment, err := idleConnectionCreateDeployment(ctx, tc.namespace, index, serviceLabels, serverResponse)
	if err != nil {
		return fmt.Errorf("failed to create deployment %d: %v", index, err)
	}
	tc.deployments = append(tc.deployments, deployment)

	if err := waitForDeploymentComplete(t, kclient, deployment, 2*time.Minute); err != nil {
		return fmt.Errorf("deployment %d is not ready: %v", index, err)
	}

	return nil
}

func idleConnectionCreateDeployment(ctx context.Context, namespace string, serviceNumber int, labels map[string]string, serverResponse string) (*appsv1.Deployment, error) {
	image, err := getCanaryImageFromIngressOperatorDeployment(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get canary image: %v", err)
	}

	name := fmt.Sprintf("web-server-%d", serviceNumber)
	secretName := fmt.Sprintf("serving-cert-%s-%s", namespace, name)

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
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
							Name:            name,
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Command:         []string{"/usr/bin/ingress-operator"},
							Args:            []string{"serve-http2-test-server"},
							Ports: []corev1.ContainerPort{
								{Name: "http", ContainerPort: 8080},
							},
							Env: []corev1.EnvVar{
								{Name: "CUSTOM_RESPONSE", Value: serverResponse},
								{Name: "PORT", Value: "8080"},
								{Name: "TLS_CERT", Value: "/etc/serving-cert/tls.crt"},
								{Name: "TLS_KEY", Value: "/etc/serving-cert/tls.key"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromInt32(8080),
										Scheme: corev1.URISchemeHTTP,
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									HTTPGet: &corev1.HTTPGetAction{
										Path:   "/healthz",
										Port:   intstr.FromInt32(8080),
										Scheme: corev1.URISchemeHTTP,
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
								TimeoutSeconds:      5,
							},

							VolumeMounts: []corev1.VolumeMount{
								{
									Name:      "serving-cert",
									MountPath: "/etc/serving-cert",
								},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "serving-cert",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: secretName,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := kclient.Create(ctx, deployment); err != nil {
		return nil, err
	}

	return deployment, nil
}

func idleConnectionCreateService(ctx context.Context, namespace string, serviceNumber int, serviceLabels map[string]string) (*corev1.Service, error) {
	name := fmt.Sprintf("web-server-%d", serviceNumber)
	secretName := fmt.Sprintf("serving-cert-%s-%s", namespace, name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    serviceLabels,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": secretName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: serviceLabels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8080,
				TargetPort: intstr.FromInt32(8080),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	if err := kclient.Create(ctx, service); err != nil {
		return nil, err
	}

	return service, nil
}

func idleConnectionCreateRoute(ctx context.Context, namespace, name, serviceName string, labels map[string]string) (*routev1.Route, error) {
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

	if err := kclient.Create(ctx, route); err != nil {
		return nil, err
	}

	return route, nil
}

func idleConnectionFetchResponse(t *testing.T, route *routev1.Route, client *http.Client) (string, error) {
	url := fmt.Sprintf("http://%s/custom-response", route.Spec.Host)

	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("failed to GET response from service: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read response body: %w", err)
	}

	responseString := string(body)

	t.Logf("GET %s RESPONSE: %s", url, responseString)

	return responseString, nil
}

func idleConnectionSwitchRouteService(ctx context.Context, t *testing.T, tc *idleConnectionTestConfig, serviceIndex int) (*routev1.Route, error) {
	if serviceIndex >= len(tc.services) {
		return nil, fmt.Errorf("service index %d out of range", serviceIndex)
	}

	service := tc.services[serviceIndex]
	route := tc.route

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		updatedRoute := &routev1.Route{}
		if err := kclient.Get(ctx, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, updatedRoute); err != nil {
			return fmt.Errorf("failed to get route %s/%s: %w", route.Namespace, route.Name, err)
		}

		updatedRoute.Spec.To.Name = service.Name
		if err := kclient.Update(ctx, updatedRoute); err != nil {
			t.Logf("Failed to update route %s/%s to point to service %s/%s: %v, retrying...", route.Namespace, route.Name, service.Namespace, service.Name, err)
			return err
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update route %s/%s to point to service %s/%s: %w", route.Namespace, route.Name, service.Namespace, service.Name, err)
	}

	t.Logf("Updated route %s/%s to point to service %s/%s", route.Namespace, route.Name, service.Namespace, service.Name)

	if err := waitWithTimeout(time.Minute, func(ctx context.Context) error {
		return waitForRouteAdmitted(ctx, t, "default", route)
	}); err != nil {
		return nil, fmt.Errorf("error waiting for route to be admitted: %v", err)
	}

	expectedBackendName := fmt.Sprintf("be_http:%s:%s", route.Namespace, route.Name)
	expectedServerName := fmt.Sprintf("pod:%s:%s:http:%s:%d", tc.pods[serviceIndex].Name, service.Name, tc.pods[serviceIndex].Status.PodIP, service.Spec.Ports[0].Port)

	podSelector := "ingresscontroller.operator.openshift.io/deployment-ingresscontroller=default"

	if err := waitWithTimeout(3*time.Minute, func(ctx context.Context) error {
		return waitForHAProxyConfigUpdate(ctx, t, kclient, tc.kubeConfig, podSelector, expectedBackendName, expectedServerName)
	}); err != nil {
		return nil, fmt.Errorf("error waiting for HAProxy configuration update for service %s/%s: %w", service.Namespace, service.Name, err)
	}

	t.Logf("HAProxy configuration updated for route %s/%s to point to service %s/%s", route.Namespace, route.Name, service.Namespace, service.Name)

	return route, nil
}

func idleConnectionSwitchTerminationPolicy(ctx context.Context, t *testing.T, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
	icName := types.NamespacedName{
		Name:      "default",
		Namespace: "openshift-ingress-operator",
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ic, err := getIngressController(t, kclient, icName, time.Minute)
		if err != nil {
			return fmt.Errorf("failed to get IngressController: %w", err)
		}

		ic.Spec.IdleConnectionTerminationPolicy = policy
		if err := kclient.Update(ctx, ic); err != nil {
			t.Logf("Failed to update IdleConnectionTerminationPolicy to %s: %v, retrying...", policy, err)
			return err
		}
		return nil
	}); err != nil {
		return fmt.Errorf("failed to switch IdleConnectionTerminationPolicy to %s: %w", policy, err)
	}

	t.Logf("Waiting for ingresscontroller to stabilise after policy switch to %s", policy)

	if err := waitForIngressControllerCondition(t, kclient, 5*time.Minute, icName, availableConditionsForIngressControllerWithHostNetwork...); err != nil {
		return fmt.Errorf("failed to observe expected conditions after switching policy to %s: %v", policy, err)
	}

	t.Logf("IngressController available after policy switch to %s", policy)

	routerDeployment := &appsv1.Deployment{}
	routerDeploymentName := types.NamespacedName{
		Namespace: "openshift-ingress",
		Name:      "router-default",
	}

	if err := kclient.Get(ctx, routerDeploymentName, routerDeployment); err != nil {
		return fmt.Errorf("failed to get router deployment: %v", err)
	}

	verifyRouterEnvVar := func(expectValue string) error {
		state := "unset"
		if expectValue != "" {
			state = fmt.Sprintf("set to %q", expectValue)
		}

		t.Logf("Waiting for router deployment to have environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE %s", state)

		if err := waitForDeploymentEnvVar(t, kclient, routerDeployment, 2*time.Minute, "ROUTER_IDLE_CLOSE_ON_RESPONSE", expectValue); err != nil {
			return fmt.Errorf("expected router deployment to have ROUTER_IDLE_CLOSE_ON_RESPONSE %s: %v", state, err)
		}

		t.Logf("Router deployment has environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE %s", state)
		return nil
	}

	switch policy {
	case operatorv1.IngressControllerConnectionTerminationPolicyDeferred:
		if err := verifyRouterEnvVar("true"); err != nil {
			return err
		}
	case operatorv1.IngressControllerConnectionTerminationPolicyImmediate:
		if err := verifyRouterEnvVar(""); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported idle connection termination policy: %q", policy)
	}

	return nil
}

func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	namespace := "idle-close-on-response-e2e-" + rand.String(5)

	_, tc, err := idleConnectionTestSetup(context.Background(), t, namespace)
	if err != nil {
		t.Fatalf("failed to set up test resources: %v", err)
	}

	icName := types.NamespacedName{
		Name:      "default",
		Namespace: "openshift-ingress-operator",
	}

	ingressController, err := getIngressController(t, kclient, icName, 1*time.Minute)
	if err != nil {
		t.Fatalf("failed to retrieve IngressController: %v", err)
	}
	initialPolicy := ingressController.Spec.IdleConnectionTerminationPolicy

	t.Logf("Detected IdleConnectionTerminationPolicy: %s", initialPolicy)

	defer func() {
		if err := idleConnectionSwitchTerminationPolicy(context.Background(), t, initialPolicy); err != nil {
			t.Fatalf("cleanup: failed to set policy back to %q: %v", initialPolicy, err)
		}
	}()

	expectedResponses := map[operatorv1.IngressControllerConnectionTerminationPolicy][]string{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred: {
			idleConnectionResponseServiceA, // Pre-step: Switch to Service-A
			idleConnectionResponseServiceA, // Step 1: Initial GET
			idleConnectionResponseServiceA, // Step 2: GET after switching to Service-B
			idleConnectionResponseServiceB, // Step 3: Final GET
		},
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate: {
			idleConnectionResponseServiceA, // Pre-step: Switch to Service-A
			idleConnectionResponseServiceA, // Step 1: Initial GET
			idleConnectionResponseServiceB, // Step 2: GET after switching to Service-B
			idleConnectionResponseServiceB, // Step 3: Final GET
		},
	}

	actions := []func(ctx context.Context, tc *idleConnectionTestConfig) (string, error){
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Pre-step: Set the route back to Service-A.
			if _, err := idleConnectionSwitchRouteService(ctx, t, tc, 0); err != nil {
				return "", fmt.Errorf("failed to switch route back to Service-A: %w", err)
			}
			return idleConnectionFetchResponse(t, tc.route, tc.httpClient)
		},
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Step 1: Verify the response from Service-A.
			return idleConnectionFetchResponse(t, tc.route, tc.httpClient)
		},
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Step 2: Switch the route to Service-B and fetch the response.
			_, err := idleConnectionSwitchRouteService(ctx, t, tc, 1)
			if err != nil {
				return "", fmt.Errorf("failed to switch route to Service-B: %w", err)
			}
			return idleConnectionFetchResponse(t, tc.route, tc.httpClient)
		},
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Step 3: Fetch the final response (expected to be from Service-B).
			return idleConnectionFetchResponse(t, tc.route, tc.httpClient)
		},
	}

	policiesToTest := []operatorv1.IngressControllerConnectionTerminationPolicy{
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
	}

	// If the initial policy doesn't match our first test policy,
	// reorder the tests.
	if initialPolicy == operatorv1.IngressControllerConnectionTerminationPolicyDeferred {
		t.Log("Reordering test cases to avoid initial policy switch")
		policiesToTest = []operatorv1.IngressControllerConnectionTerminationPolicy{
			operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
			operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
		}
	}

	for i, policy := range policiesToTest {
		t.Run(string(policy), func(t *testing.T) {
			// Only switch policy if it's not the first
			// test matching the initial policy.
			if i == 0 && policy == initialPolicy {
				t.Logf("Skipping policy switch as current policy already matches %s", policy)
			} else {
				if err := idleConnectionSwitchTerminationPolicy(context.Background(), t, policy); err != nil {
					t.Fatalf("failed to switch to policy %q: %v", policy, err)
				}
			}

			tc.httpClient = &http.Client{
				Timeout: 30 * time.Second,
				Transport: &http.Transport{
					IdleConnTimeout:     150 * time.Second,
					MaxIdleConns:        100,
					MaxIdleConnsPerHost: 10,
				},
			}

			for j, action := range actions {
				resp, err := action(context.Background(), tc)
				if err != nil {
					t.Fatalf("failed during step %d: %v", j+1, err)
				}

				if resp != expectedResponses[policy][j] {
					t.Fatalf("unexpected response at step %d for policy %s: got %s, want %s",
						j+1, policy, resp, expectedResponses[policy][j])
				}

				t.Logf("Response at step %d for policy %s matches expected: %s", j+1, policy, resp)
			}
		})
	}
}
