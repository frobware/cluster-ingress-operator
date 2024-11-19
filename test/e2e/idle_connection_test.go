//go:build e2e
// +build e2e

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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
	operatorcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller"
)

const (
	idleConnectionServer1Response = "web server 1"
	idleConnectionServer2Response = "web server 2"
)

type idleConnectionTestConfig struct {
	deployments []*appsv1.Deployment
	httpClient  *http.Client
	pods        []*corev1.Pod
	routeName   types.NamespacedName
	services    []*corev1.Service
	testLabels  map[string]string
}

// haproxyBackend represents an HAProxy backend configuration section
// with its associated settings and servers.
type haproxyBackend struct {
	name     string   // Name of the backend as defined in HAProxy config.
	settings []string // Non-server settings.
	servers  []string // Server entries in this backend.
}

// getHAProxyConfigFromRouterPod retrieves the HAProxy configuration
// from a pod.
func getHAProxyConfigFromRouterPod(t *testing.T, pod *corev1.Pod) (string, error) {
	var stdout, stderr bytes.Buffer
	if err := podExec(t, *pod, &stdout, &stderr, []string{"cat", "/var/lib/haproxy/conf/haproxy.config"}); err != nil {
		return "", fmt.Errorf("%s/%s: cat /var/lib/haproxy/conf/haproxy.config: %w (stderr=%q)", pod.Namespace, pod.Name, err, stderr.String())
	}

	return stdout.String(), nil
}

// parseHAProxyConfigBackends parses raw HAProxy configuration content
// and extracts backend sections. Returns an error if the config is
// malformed or cannot be parsed.
func parseHAProxyConfigBackends(content string) ([]haproxyBackend, error) {
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
		return nil, errors.New("no backends found in HAProxy configuration")
	}

	return backends, nil
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

// waitForHAProxyConfigUpdate ensures the HAProxy configuration in all
// router pods matches the expected backend and server entries. It
// repeatedly polls the HAProxy configuration of pods selected by the
// given label selector, checking for consistency across all pods. The
// function continues polling until the configuration is verified or
// the provided context is cancelled.
func waitForHAProxyConfigUpdate(ctx context.Context, t *testing.T, ic *operatorv1.IngressController, backendName, serverName string) error {
	return wait.PollUntilContextCancel(ctx, 6*time.Second, true, func(ctx context.Context) (bool, error) {
		deploymentName := operatorcontroller.RouterDeploymentName(ic)
		deployment, err := getDeployment(t, kclient, deploymentName, time.Minute)
		if err != nil {
			t.Logf("Failed to get deployment %s: %v, retrying...", deploymentName, err)
			return false, nil
		}

		podList, err := getPods(t, kclient, deployment)
		if err != nil {
			t.Logf("Failed to get pods for deployment %s: %v, retrying...", deploymentName, err)
			return false, nil
		}

		if len(podList.Items) == 0 {
			return false, fmt.Errorf("no router pods found for deployment %s", deploymentName)
		}

		allPodsMatch := true
		for _, pod := range podList.Items {
			if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
				t.Logf("Skipping terminated pod %s/%s (phase: %v)", pod.Namespace, pod.Name, pod.Status.Phase)
				continue
			}

			haproxyConfig, err := getHAProxyConfigFromRouterPod(t, &pod)
			if err != nil {
				t.Logf("Failed to get HAProxy config from pod %s/%s: %v, retrying...", pod.Namespace, pod.Name, err)
				allPodsMatch = false
				continue
			}

			backends, err := parseHAProxyConfigBackends(haproxyConfig)
			if err != nil {
				t.Logf("Failed to parse HAProxy config from pod %s/%s: %v", pod.Namespace, pod.Name, err)
				allPodsMatch = false
				continue
			}

			backend, found := findHAProxyBackendWithServiceServer(backends, backendName, serverName)
			if !found {
				allPodsMatch = false
				t.Logf("Waiting for HAProxy backend %q in pod %s/%s", backendName, pod.Namespace, pod.Name)
				continue
			}

			t.Logf("Found HAProxy backend in pod %s/%s:\nBackend: %s\nServers: %s", pod.Namespace, pod.Name, backendName, strings.Join(backend.servers, "\n  "))
		}

		return allPodsMatch, nil
	})
}

func idleConnectionTestSetup(ctx context.Context, t *testing.T, ns *corev1.Namespace, ic *operatorv1.IngressController) (*idleConnectionTestConfig, error) {
	canaryImageReference := func(t *testing.T) (string, error) {
		ingressOperatorName := types.NamespacedName{
			Name:      "ingress-operator",
			Namespace: operatorNamespace,
		}

		deployment, err := getDeployment(t, kclient, ingressOperatorName, 1*time.Minute)
		if err != nil {
			return "", fmt.Errorf("failed to get deployment %s/%s: %w", ingressOperatorName.Namespace, ingressOperatorName.Name, err)
		}

		for _, container := range deployment.Spec.Template.Spec.Containers {
			for _, env := range container.Env {
				if env.Name == "CANARY_IMAGE" {
					return env.Value, nil
				}
			}
		}

		return "", fmt.Errorf("CANARY_IMAGE environment variable not found in deployment %s/%s", ingressOperatorName.Namespace, ingressOperatorName.Name)
	}

	tc := &idleConnectionTestConfig{
		testLabels: map[string]string{
			"ingress-controller": ic.Name,
		},
	}

	image, err := canaryImageReference(t)
	if err != nil {
		return nil, fmt.Errorf("failed to get canary image: %w", err)
	}

	if err := idleConnectionCreateBackendService(ctx, t, ns, tc, 1, idleConnectionServer1Response, image); err != nil {
		return nil, fmt.Errorf("failed to create backend 1: %w", err)
	}

	if err := idleConnectionCreateBackendService(ctx, t, ns, tc, 2, idleConnectionServer2Response, image); err != nil {
		return nil, fmt.Errorf("failed to create backend 2: %w", err)
	}

	for _, deployment := range tc.deployments {
		t.Logf("Waiting for deployment %s/%s to be ready...", deployment.Namespace, deployment.Name)

		if err := waitForDeploymentComplete(t, kclient, deployment, 2*time.Minute); err != nil {
			return nil, fmt.Errorf("deployment %s/%s is not ready: %w", deployment.Namespace, deployment.Name, err)
		}

		podList, err := getPods(t, kclient, deployment)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch pods for deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
		}

		if len(podList.Items) == 0 {
			return nil, fmt.Errorf("no pods in  deployment %s/%s", deployment.Namespace, deployment.Name)
		}

		for i := range podList.Items {
			tc.pods = append(tc.pods, &podList.Items[i])
		}
	}

	route, err := idleConnectionCreateRoute(ctx, ns.Name, "test", tc.services[0].Name, tc.testLabels)
	if err != nil {
		return nil, fmt.Errorf("failed to create test route: %w", err)
	}

	routeAdmittedCondition := routev1.RouteIngressCondition{
		Type:   routev1.RouteAdmitted,
		Status: corev1.ConditionTrue,
	}

	if err := waitForRouteIngressConditions(t, kclient, types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, ic.Name, routeAdmittedCondition); err != nil {
		return nil, fmt.Errorf("error waiting for route to be admitted: %w", err)
	}

	t.Logf("Route %s/%s admitted by ingresscontroller %s", route.Namespace, route.Name, ic.Name)

	if len(tc.deployments) != 2 {
		return nil, fmt.Errorf("expected 2 deployments, but got %d", len(tc.deployments))
	}

	if len(tc.services) != 2 {
		return nil, fmt.Errorf("expected 2 services, but got %d", len(tc.services))
	}

	if len(tc.pods) != 2 {
		return nil, fmt.Errorf("expected 2 pods, but got %d", len(tc.pods))
	}

	tc.routeName = types.NamespacedName{Namespace: route.Namespace, Name: route.Name}

	return tc, nil
}

func idleConnectionCreateBackendService(ctx context.Context, t *testing.T, ns *corev1.Namespace, tc *idleConnectionTestConfig, index int, serverResponse, image string) error {
	svc, err := idleConnectionCreateService(ctx, ns.Name, index)
	if err != nil {
		return fmt.Errorf("failed to create service %d: %w", index, err)
	}
	tc.services = append(tc.services, svc)

	deployment, err := idleConnectionCreateDeployment(ctx, ns.Name, index, serverResponse, image)
	if err != nil {
		return fmt.Errorf("failed to create deployment %d: %w", index, err)
	}
	tc.deployments = append(tc.deployments, deployment)

	if err := waitForDeploymentComplete(t, kclient, deployment, 2*time.Minute); err != nil {
		return fmt.Errorf("deployment %d is not ready: %w", index, err)
	}

	return nil
}

func idleConnectionCreateDeployment(ctx context.Context, namespace string, serviceNumber int, serverResponse, image string) (*appsv1.Deployment, error) {
	name := fmt.Sprintf("web-server-%d", serviceNumber)
	secretName := fmt.Sprintf("serving-cert-%s-%s", namespace, name)

	selectorLabels := map[string]string{
		"app":      "web-server",
		"instance": fmt.Sprintf("%d", serviceNumber),
	}

	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    selectorLabels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To[int32](1),
			Selector: &metav1.LabelSelector{
				MatchLabels: selectorLabels,
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: selectorLabels,
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
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
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
								InitialDelaySeconds: 1,
								PeriodSeconds:       1,
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
		return nil, fmt.Errorf("failed to create deployment %s/%s: %w", deployment.Namespace, deployment.Name, err)
	}

	return deployment, nil
}

func idleConnectionCreateService(ctx context.Context, namespace string, serviceNumber int) (*corev1.Service, error) {
	name := fmt.Sprintf("web-server-%d", serviceNumber)
	secretName := fmt.Sprintf("serving-cert-%s-%s", namespace, name)
	selectorLabels := map[string]string{
		"app":      "web-server",
		"instance": fmt.Sprintf("%d", serviceNumber),
	}

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    selectorLabels,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": secretName,
			},
		},
		Spec: corev1.ServiceSpec{
			Selector: selectorLabels,
			Ports: []corev1.ServicePort{{
				Name:       "http",
				Port:       8080,
				TargetPort: intstr.FromInt32(8080),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}

	if err := kclient.Create(ctx, service); err != nil {
		return nil, fmt.Errorf("failed to create service %s/%s: %w", service.Namespace, service.Name, err)
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
			Subdomain: name,
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
		return nil, fmt.Errorf("failed to create route %s/%s: %w", route.Namespace, route.Name, err)
	}

	return route, nil
}

func idleConnectionSwitchRouteService(t *testing.T, ic *operatorv1.IngressController, tc *idleConnectionTestConfig, routeName types.NamespacedName, serviceIndex int) (*routev1.Route, error) {
	if serviceIndex >= len(tc.services) {
		return nil, fmt.Errorf("service index %d out of range", serviceIndex)
	}

	service := tc.services[serviceIndex]

	var updatedRoute *routev1.Route
	if err := updateRouteWithRetryOnConflict(t, routeName, time.Minute, func(route *routev1.Route) {
		route.Spec.To.Name = service.Name
		updatedRoute = route
	}); err != nil {
		return nil, fmt.Errorf("failed to update route %s to point to service %s/%s: %w", routeName, service.Namespace, service.Name, err)
	}

	t.Logf("Switched route %s to service %s/%s", routeName, service.Namespace, service.Name)

	routeAdmittedCondition := routev1.RouteIngressCondition{
		Type:   routev1.RouteAdmitted,
		Status: corev1.ConditionTrue,
	}
	if err := waitForRouteIngressConditions(t, kclient, routeName, ic.Name, routeAdmittedCondition); err != nil {
		return nil, fmt.Errorf("error waiting for route to be admitted: %w", err)
	}

	t.Logf("Route %s admitted by ingresscontroller %s", routeName, ic.Name)

	expectedBackendName := fmt.Sprintf("be_http:%s:%s", routeName.Namespace, routeName.Name)
	expectedServerName := fmt.Sprintf("pod:%s:%s:http:%s:%d", tc.pods[serviceIndex].Name, service.Name, tc.pods[serviceIndex].Status.PodIP, service.Spec.Ports[0].Port)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := waitForHAProxyConfigUpdate(ctx, t, ic, expectedBackendName, expectedServerName); err != nil {
		return nil, fmt.Errorf("error waiting for HAProxy configuration update for route %s to point to service %s/%s: %w", routeName, service.Namespace, service.Name, err)
	}

	t.Logf("HAProxy configuration updated for route %s to point to service %s/%s", routeName, service.Namespace, service.Name)

	return updatedRoute, nil
}

func idleConnectionFetchResponse(t *testing.T, client *http.Client, elbHostname, hostname string) (string, error) {
	url := fmt.Sprintf("http://%s/custom-response", elbHostname)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Host = hostname
	t.Logf("GET %s with Host %s", url, req.Host)

	var responseBody string
	compareFunc := func(resp *http.Response) bool {
		if resp.StatusCode == http.StatusServiceUnavailable {
			t.Logf("Got %v Service Unavailable, retrying...", resp.StatusCode)
			return false
		}
		if resp.StatusCode != http.StatusOK {
			t.Logf("Got unexpected status code: %d", resp.StatusCode)
			return false
		}

		bodyBytes, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Logf("Failed to read response body: %v", err)
			return false
		}
		responseBody = string(bodyBytes)
		return true
	}

	if err := waitForHTTPClientCondition(t, client, req, 6*time.Second, time.Minute, compareFunc); err != nil {
		return "", fmt.Errorf("failed waiting for successful response: %w", err)
	}

	return responseBody, nil
}

func idleConnectionSwitchIdleTerminationPolicy(t *testing.T, ic *operatorv1.IngressController, icName types.NamespacedName, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
	if err := updateIngressControllerWithRetryOnConflict(t, icName, 5*time.Minute, func(ic *operatorv1.IngressController) {
		ic.Spec.IdleConnectionTerminationPolicy = policy
	}); err != nil {
		return fmt.Errorf("failed to update IdleConnectionTerminationPolicy to %q for ingresscontroller %s: %w", policy, icName, err)
	}

	t.Logf("Updated IdleConnectionTerminationPolicy from %q to %q for ingresscontroller %s", ic.Spec.IdleConnectionTerminationPolicy, policy, icName)

	if err := waitForDeploymentCompleteWithOldPodTermination(t, kclient, operatorcontroller.RouterDeploymentName(ic), 3*time.Minute); err != nil {
		return fmt.Errorf("failed to observe router deployment completion: %w", err)
	}

	t.Logf("Waiting for ingresscontroller to stabilise after policy switch to %q", policy)

	if err := waitForIngressControllerCondition(t, kclient, 5*time.Minute, icName, availableConditionsForIngressControllerWithLoadBalancer...); err != nil {
		return fmt.Errorf("failed to observe expected conditions after switching policy to %q: %w", policy, err)
	}

	t.Logf("IngressController available after policy switch to %q", policy)

	routerDeployment := appsv1.Deployment{}
	if err := kclient.Get(context.TODO(), operatorcontroller.RouterDeploymentName(ic), &routerDeployment); err != nil {
		t.Fatalf("failed to get ingresscontroller deployment: %v", err)
	}

	verifyRouterEnvVar := func(expectValue string) error {
		state := "unset"
		if expectValue != "" {
			state = fmt.Sprintf("set to %q", expectValue)
		}

		t.Logf("Waiting for router deployment to have environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE %s", state)

		if err := waitForDeploymentEnvVar(t, kclient, &routerDeployment, 2*time.Minute, "ROUTER_IDLE_CLOSE_ON_RESPONSE", expectValue); err != nil {
			return fmt.Errorf("expected router deployment to have ROUTER_IDLE_CLOSE_ON_RESPONSE %s: %w", state, err)
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

// Test_IdleConnectionTerminationPolicy verifies that the
// IngressController correctly handles backend switching under
// different IdleConnectionTerminationPolicy settings.
//
// This test:
//  1. Deploys two backend services (`web-server-1` and `web-server-2`).
//  2. Alternates a Route between the backends.
//  3. Validates that HAProxy routes requests to the correct backend
//     according to the policy (`Immediate` or `Deferred`).
//  4. Ensures router pods correctly apply the expected environment
//     variable (`ROUTER_IDLE_CLOSE_ON_RESPONSE`) for each policy.
//
// Note: In the `Deferred` policy case, due to keep-alive behaviour,
// the first request after switching backends will still be routed to
// the previously active backend. The test accounts for this expected
// behaviour and validates subsequent requests route correctly to the
// new backend.
func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	t.Parallel()
	testName := "idle-close-on-response-" + rand.String(5)
	icName := types.NamespacedName{Namespace: operatorNamespace, Name: testName}
	ns := createNamespace(t, icName.Name)
	ic := newLoadBalancerController(icName, icName.Name+"."+dnsConfig.Spec.BaseDomain)
	ic.Spec.EndpointPublishingStrategy.LoadBalancer = &operatorv1.LoadBalancerStrategy{
		Scope:               operatorv1.ExternalLoadBalancer,
		DNSManagementPolicy: operatorv1.ManagedLoadBalancerDNS,
	}
	if err := kclient.Create(context.TODO(), ic); err != nil {
		t.Fatalf("failed to create ingresscontroller: %v", err)
	}
	defer assertIngressControllerDeleted(t, kclient, ic)

	if err := waitForIngressControllerCondition(t, kclient, 5*time.Minute, icName, availableConditionsForIngressControllerWithLoadBalancer...); err != nil {
		t.Fatalf("failed to observe expected conditions: %v", err)
	}

	ic, err := getIngressController(t, kclient, icName, 1*time.Minute)
	if err != nil {
		t.Fatalf("failed to get ingresscontroller: %v", err)
	}

	elbHostname := getIngressControllerLBAddress(t, ic)
	externalTestPodName := types.NamespacedName{Name: icName.Name + "-external-verify", Namespace: icName.Namespace}
	verifyExternalIngressController(t, externalTestPodName, "apps."+ic.Spec.Domain, elbHostname)

	lbService := &corev1.Service{}
	if err := kclient.Get(context.TODO(), operatorcontroller.LoadBalancerServiceName(ic), lbService); err != nil {
		t.Fatalf("failed to get LoadBalancer service for ingresscontroller %s: %v", icName, err)
	}

	currentIdleTerminationPolicy := ic.Spec.IdleConnectionTerminationPolicy
	t.Logf("IngressController %s initial IdleConnectionTerminationPolicy=%q", ic.Name, currentIdleTerminationPolicy)

	tc, err := idleConnectionTestSetup(context.Background(), t, ns, ic)
	if err != nil {
		t.Fatalf("test setup failed: %v", err)
	}

	var route routev1.Route
	if err := kclient.Get(context.TODO(), tc.routeName, &route); err != nil {
		t.Fatalf("failed to get route %s: %v", tc.routeName, err)
	}

	routeHost := getRouteHost(&route, ic.Name)
	if routeHost == "" {
		t.Fatalf("Route %s has no host assigned by ingresscontroller %s", tc.routeName, ic.Name)
	}

	expectedResponses := map[operatorv1.IngressControllerConnectionTerminationPolicy][]string{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred: {
			idleConnectionServer1Response, // Step 1: Switch to web-server-1 and GET response.
			idleConnectionServer1Response, // Step 2: GET response.
			idleConnectionServer1Response, // Step 3: Switch to web-server-2 and GET response.
			idleConnectionServer2Response, // Step 4: GET response.
		},
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate: {
			idleConnectionServer1Response, // Step 1: Switch to web-server-1 and GET response.
			idleConnectionServer1Response, // Step 2: GET response.
			idleConnectionServer2Response, // Step 3: Switch to web-server-2 and GET response.
			idleConnectionServer2Response, // Step 4: GET response.
		},
	}

	actions := []func() (string, error){
		func() (string, error) {
			// Step 1: Set the route back to service web-server-1 and fetch the response.
			if _, err := idleConnectionSwitchRouteService(t, ic, tc, tc.routeName, 0); err != nil {
				return "", fmt.Errorf("failed to switch route back to web-server-1: %w", err)
			}
			return idleConnectionFetchResponse(t, tc.httpClient, elbHostname, routeHost)
		},
		func() (string, error) {
			// Step 2: Verify the response from web-server-1.
			return idleConnectionFetchResponse(t, tc.httpClient, elbHostname, routeHost)
		},
		func() (string, error) {
			// Step 3: Switch the route to service web-server-2 and fetch the response.
			if _, err := idleConnectionSwitchRouteService(t, ic, tc, tc.routeName, 1); err != nil {
				return "", fmt.Errorf("failed to switch route to web-server-2: %w", err)
			}
			return idleConnectionFetchResponse(t, tc.httpClient, elbHostname, routeHost)
		},
		func() (string, error) {
			// Step 4: Fetch the final response (expected to be from web-server-2).
			return idleConnectionFetchResponse(t, tc.httpClient, elbHostname, routeHost)
		},
	}

	policiesToTest := []operatorv1.IngressControllerConnectionTerminationPolicy{
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
	}

	// If the current policy is Deferred, reorder the test cases
	// to start with Deferred. Later in the test, we skip updating
	// the policy when it matches our test case. This way, if the
	// IngressController starts with Deferred policy, we avoid an
	// unnecessary rollout in the beginning of our test.
	if currentIdleTerminationPolicy == operatorv1.IngressControllerConnectionTerminationPolicyDeferred {
		t.Log("Reordering test cases to avoid initial policy switch")
		policiesToTest = []operatorv1.IngressControllerConnectionTerminationPolicy{
			operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
			operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
		}
	}

	tc.httpClient = &http.Client{
		Timeout: time.Minute,
		Transport: &http.Transport{
			IdleConnTimeout: 300 * time.Second,
		},
	}

	for i, policy := range policiesToTest {
		if i == 0 && policy == currentIdleTerminationPolicy {
			t.Logf("Skipping initial policy switch as current policy %q already matches %q", currentIdleTerminationPolicy, policy)
		} else {
			if err := idleConnectionSwitchIdleTerminationPolicy(t, ic, icName, policy); err != nil {
				t.Fatalf("failed to switch to policy %q: %v", policy, err)
			}
		}

		for j, action := range actions {
			resp, err := action()
			if err != nil {
				t.Fatalf("Step %d failed: %v", j+1, err)
			}

			if resp != expectedResponses[policy][j] {
				t.Fatalf("unexpected response at step %d for policy %q: got %q, want %q",
					j+1, policy, resp, expectedResponses[policy][j])
			}

			t.Logf("Step %d response for policy %q matches expected value %q", j+1, policy, resp)
		}
	}
}
