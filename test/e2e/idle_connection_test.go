//go:build e2e
// +build e2e

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

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
	operatorcontroller "github.com/openshift/cluster-ingress-operator/pkg/operator/controller"
)

func idleConnectionCreateBackendService(ctx context.Context, t *testing.T, namespace, name, image string) error {
	labels := map[string]string{
		"instance": name,
	}

	_, err := idleConnectionCreateService(ctx, namespace, name, labels)
	if err != nil {
		return fmt.Errorf("failed to create service %s/%s: %w", namespace, name, err)
	}

	pod, err := idleConnectionCreatePod(ctx, namespace, name, image, labels)
	if err != nil {
		return fmt.Errorf("failed to create pod %s: %w", name, err)
	}

	if err := waitForPodReady(t, kclient, pod, 2*time.Minute); err != nil {
		return fmt.Errorf("pod %s is not ready: %w", name, err)
	}

	return nil
}

func idleConnectionCreateService(ctx context.Context, namespace, name string, labels map[string]string) (*corev1.Service, error) {
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Selector: labels,
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

func idleConnectionCreatePod(ctx context.Context, namespace, name, image string, labels map[string]string) (*corev1.Pod, error) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
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
						{
							Name:          "http",
							ContainerPort: 8080,
						},
					},
					Env: []corev1.EnvVar{
						{
							Name:  "TEST_SERVER_ENABLE_HTTPS_LISTENER",
							Value: "false",
						},
						{
							Name: "POD_NAME",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "metadata.name",
								},
							},
						},
						{
							Name: "POD_NAMESPACE",
							ValueFrom: &corev1.EnvVarSource{
								FieldRef: &corev1.ObjectFieldSelector{
									FieldPath: "metadata.namespace",
								},
							},
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/healthz",
								Port:   intstr.FromInt32(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
					},
					LivenessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{
								Path:   "/healthz",
								Port:   intstr.FromInt32(8080),
								Scheme: corev1.URISchemeHTTP,
							},
						},
					},
					SecurityContext: generateUnprivilegedSecurityContext(),
				},
			},
		},
	}

	if err := kclient.Create(ctx, pod); err != nil {
		return nil, fmt.Errorf("failed to create pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	return pod, nil
}

func idleConnectionSwitchRouteService(t *testing.T, routeName types.NamespacedName, routerName, serviceName string) error {
	if err := updateRouteWithRetryOnConflict(t, routeName, time.Minute, func(route *routev1.Route) {
		route.Spec.To.Name = serviceName
	}); err != nil {
		return fmt.Errorf("failed to update route %s to point to service %q: %w", routeName, serviceName, err)
	}

	routeAdmittedCondition := routev1.RouteIngressCondition{
		Type:   routev1.RouteAdmitted,
		Status: corev1.ConditionTrue,
	}

	if err := waitForRouteIngressConditions(t, kclient, routeName, routerName, routeAdmittedCondition); err != nil {
		return fmt.Errorf("error waiting for route %s to be admitted: %w", routeName, err)
	}

	// Wait for the router deployment to update the HAProxy
	// configuration and perform a soft-reload.
	time.Sleep(20 * time.Second)

	return nil
}

func idleConnectionFetchResponse(client *http.Client, elbHostname, hostname string) (string, error) {
	url := fmt.Sprintf("http://%s", elbHostname)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("failed to create request: %w", err)
	}
	req.Host = hostname

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}

	defer resp.Body.Close()

	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read body: %w", err)
	}

	return resp.Header.Get("X-Pod-Name"), nil
}

func idleConnectionValidateRouterEnvVar(t *testing.T, routerDeployment *appsv1.Deployment, expectValue string) error {
	state := "unset"
	if expectValue != "" {
		state = fmt.Sprintf("set to %q", expectValue)
	}

	if err := waitForDeploymentEnvVar(t, kclient, routerDeployment, 2*time.Minute, "ROUTER_IDLE_CLOSE_ON_RESPONSE", expectValue); err != nil {
		return fmt.Errorf("expected router deployment to have ROUTER_IDLE_CLOSE_ON_RESPONSE %s: %w", state, err)
	}

	return nil
}

func idleConnectionSwitchIdleTerminationPolicy(t *testing.T, ic *operatorv1.IngressController, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
	icName := types.NamespacedName{Namespace: ic.Namespace, Name: ic.Name}
	if err := updateIngressControllerWithRetryOnConflict(t, icName, 5*time.Minute, func(ic *operatorv1.IngressController) {
		ic.Spec.IdleConnectionTerminationPolicy = policy
	}); err != nil {
		return fmt.Errorf("failed to update IdleConnectionTerminationPolicy to %q for ingresscontroller %s: %w", policy, icName, err)
	}

	if err := waitForDeploymentCompleteWithOldPodTermination(t, kclient, operatorcontroller.RouterDeploymentName(ic), 3*time.Minute); err != nil {
		return fmt.Errorf("failed to observe router deployment completion for %s: %w", operatorcontroller.RouterDeploymentName(ic), err)
	}

	routerDeployment := appsv1.Deployment{}
	if err := kclient.Get(context.Background(), operatorcontroller.RouterDeploymentName(ic), &routerDeployment); err != nil {
		return fmt.Errorf("failed to get ingresscontroller deployment: %w", err)
	}

	switch policy {
	case operatorv1.IngressControllerConnectionTerminationPolicyDeferred:
		return idleConnectionValidateRouterEnvVar(t, &routerDeployment, "true")
	case operatorv1.IngressControllerConnectionTerminationPolicyImmediate:
		return idleConnectionValidateRouterEnvVar(t, &routerDeployment, "")
	default:
		return fmt.Errorf("unsupported idle connection termination policy: %q", policy)
	}
}

// IngressController correctly handles backend switching under
// different IdleConnectionTerminationPolicy settings.
//
// This test:
//  1. Deploys two backend services (`web-service-1` and `web-service-2`).
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

	const (
		webService1 = "web-service-1"
		webService2 = "web-service-2"
	)

	testName := "idle-close-on-response-" + rand.String(5)

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

	podImage, err := canaryImageReference(t)
	if err != nil {
		t.Fatalf("failed to get canary image reference: %v", err)
	}

	icName := types.NamespacedName{Namespace: operatorNamespace, Name: testName}
	ns := createNamespace(t, icName.Name)

	if err := idleConnectionCreateBackendService(context.Background(), t, ns.Name, webService1, podImage); err != nil {
		t.Fatalf("failed to create backend service 1: %v", err)
	}

	if err := idleConnectionCreateBackendService(context.Background(), t, ns.Name, webService2, podImage); err != nil {
		t.Fatalf("failed to create backend service 2: %v", err)
	}

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

	if ic, err = getIngressController(t, kclient, icName, 1*time.Minute); err != nil {
		t.Fatalf("failed to get ingresscontroller: %v", err)
	}

	initialIdleTerminationPolicy := ic.Spec.IdleConnectionTerminationPolicy
	elbHostname := getIngressControllerLBAddress(t, ic)
	externalTestPodName := types.NamespacedName{Name: icName.Name + "-external-verify", Namespace: icName.Namespace}

	verifyExternalIngressController(t, externalTestPodName, "apps."+ic.Spec.Domain, elbHostname)

	routeName := types.NamespacedName{Namespace: testName, Name: "test"}
	route := buildRoute(routeName.Name, routeName.Namespace, webService1)
	if err := kclient.Create(context.Background(), route); err != nil {
		t.Fatalf("failed to create route %s: %v", routeName, err)
	}

	routeAdmittedCondition := routev1.RouteIngressCondition{
		Type:   routev1.RouteAdmitted,
		Status: corev1.ConditionTrue,
	}

	if err := waitForRouteIngressConditions(t, kclient, routeName, ic.Name, routeAdmittedCondition); err != nil {
		t.Fatalf("error waiting for route %s to be admitted: %v", routeName, err)
	}

	if err := kclient.Get(context.TODO(), routeName, route); err != nil {
		t.Fatalf("failed to get route %s: %v", routeName, err)
	}

	routeHost := getRouteHost(route, ic.Name)
	if routeHost == "" {
		t.Fatalf("route %s has no host assigned by ingresscontroller %s", routeName, ic.Name)
	}

	testPolicies := []operatorv1.IngressControllerConnectionTerminationPolicy{
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
	}

	// If the current policy is Deferred, reorder the test cases
	// to start with Deferred. This ensures we avoid an
	// unnecessary policy switch and the associated
	// IngressController rollout at the beginning of the test. By
	// starting with the current policy, we can skip applying it
	// again in the first subtest, improving efficiency. In 4.19+
	// the default is Immediate.
	if initialIdleTerminationPolicy == operatorv1.IngressControllerConnectionTerminationPolicyDeferred {
		t.Log("Reordering test cases to avoid initial policy switch")
		testPolicies = []operatorv1.IngressControllerConnectionTerminationPolicy{
			operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
			operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
		}
	}

	actions := []struct {
		description      string
		action           func(*http.Client) (string, error)
		expectedResponse func(policy operatorv1.IngressControllerConnectionTerminationPolicy) string
	}{
		{
			description: "Switch to web-service-1 and fetch response",
			action: func(httpClient *http.Client) (string, error) {
				if err := idleConnectionSwitchRouteService(t, routeName, ic.Name, webService1); err != nil {
					return "", err
				}
				return idleConnectionFetchResponse(httpClient, elbHostname, routeHost)
			},
			expectedResponse: func(policy operatorv1.IngressControllerConnectionTerminationPolicy) string {
				return webService1
			},
		},
		{
			description: "Verify response is initially from web-service-1",
			action: func(httpClient *http.Client) (string, error) {
				return idleConnectionFetchResponse(httpClient, elbHostname, routeHost)
			},
			expectedResponse: func(policy operatorv1.IngressControllerConnectionTerminationPolicy) string {
				return webService1
			},
		},
		{
			description: "Switch to web-service-2 and fetch response",
			action: func(httpClient *http.Client) (string, error) {
				if err := idleConnectionSwitchRouteService(t, routeName, ic.Name, webService2); err != nil {
					return "", err
				}
				return idleConnectionFetchResponse(httpClient, elbHostname, routeHost)
			},
			expectedResponse: func(policy operatorv1.IngressControllerConnectionTerminationPolicy) string {
				return map[operatorv1.IngressControllerConnectionTerminationPolicy]string{
					operatorv1.IngressControllerConnectionTerminationPolicyImmediate: webService2,
					operatorv1.IngressControllerConnectionTerminationPolicyDeferred:  webService1,
				}[policy]
			},
		},
		{
			description: "Check final response is still from web-service-2",
			action: func(httpClient *http.Client) (string, error) {
				return idleConnectionFetchResponse(httpClient, elbHostname, routeHost)
			},
			expectedResponse: func(policy operatorv1.IngressControllerConnectionTerminationPolicy) string {
				return map[operatorv1.IngressControllerConnectionTerminationPolicy]string{
					operatorv1.IngressControllerConnectionTerminationPolicyImmediate: webService2,
					operatorv1.IngressControllerConnectionTerminationPolicyDeferred:  webService2,
				}[policy]
			},
		},
	}

	for i, policy := range testPolicies {
		if i == 0 && policy == initialIdleTerminationPolicy {
			t.Logf("[%s] skipping policy update for ingresscontroller %s: current policy %q matches desired policy %q", policy, icName, initialIdleTerminationPolicy, policy)
		} else {
			if err := idleConnectionSwitchIdleTerminationPolicy(t, ic, policy); err != nil {
				t.Fatalf("failed to set ingresscontroller %s idle termination policy to %q: %v", icName, policy, err)
			}
			t.Logf("ingresscontroller %s available after policy switch to %q", icName, policy)
		}

		httpClient := http.Client{
			Timeout: time.Minute,
			Transport: &http.Transport{
				DisableKeepAlives: false,
				IdleConnTimeout:   300 * time.Second,
			},
		}

		for step, action := range actions {
			t.Logf("[%s] step %d: %s", policy, step+1, action.description)

			response, err := action.action(&httpClient)
			if err != nil {
				t.Fatalf("[%s] step %d: failed: %v", policy, step+1, err)
			}

			expectedResponse := action.expectedResponse(policy)
			if response != expectedResponse {
				t.Fatalf("[%s] step %d: unexpected response: got %q, want %q", policy, step+1, response, expectedResponse)
			}
		}
	}
}
