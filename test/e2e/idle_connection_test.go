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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
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
	name     string   // Name of the backend as defined in HAProxy config
	settings []string // Raw config settings (mode, balance, etc)
	servers  []string // Server entries in this backend
}

// getHAProxyConfigFromRouterPod retrieves the HAProxy configuration
// from a pod.
func getHAProxyConfigFromRouterPod(ctx context.Context, kubeClient *kubernetes.Clientset, restConfig *rest.Config, pod *corev1.Pod) (string, error) {
	stdout, stderr, err := executeCommandInPod(ctx, kubeClient, restConfig, pod.Name, pod.Namespace, "router", []string{"cat", "/var/lib/haproxy/conf/haproxy.config"})
	if err != nil {
		return "", fmt.Errorf("failed to get HAProxy config from pod %s/%s: %w\nstderr: %s",
			pod.Namespace, pod.Name, err, stderr)
	}
	return stdout, nil
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

// executeCommandInPod executes a command in a specific pod container.
func executeCommandInPod(ctx context.Context, kubeClient *kubernetes.Clientset, restConfig *rest.Config, podName, namespace, container string, command []string) (string, string, error) {
	req := kubeClient.CoreV1().RESTClient().
		Post().
		Resource("pods").
		Name(podName).
		Namespace(namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdin:     false,
			Stdout:    true,
			Stderr:    true,
			TTY:       false,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(restConfig, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("failed to create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer

	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", "", fmt.Errorf("failed to execute command: %w", err)
	}

	return stdout.String(), stderr.String(), nil
}

// getPodsWithLabels retrieves pods matching the specified label selector
func getPodsWithLabels(kclient client.Client, namespace string, labelSelector string) ([]corev1.Pod, error) {
	var podList corev1.PodList
	err := kclient.List(context.Background(), &podList,
		client.InNamespace(namespace),
		client.HasLabels{labelSelector},
	)
	if err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s with label selector %q: %w",
			namespace, labelSelector, err)
	}

	return podList.Items, nil
}

// findBackend searches for a specific backend and server combination
// in the HAProxy config. Returns the matching backend and true if
// found, or an empty backend and false if not found.
func findBackend(backends []haproxyBackend, expectedBackendName, expectedServiceName string) (haproxyBackend, bool) {
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
// labelSelector or the context is cancelled.
func waitForHAProxyConfigUpdate(
	t *testing.T,
	ctx context.Context,
	timeout time.Duration,
	kclient client.Client,
	restConfig *rest.Config,
	namespace string,
	labelSelector string,
	expectedBackendName, expectedServerName string,
) error {
	kubeClient, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return fmt.Errorf("failed to create kubernetes client: %w", err)
	}

	return wait.PollUntilContextTimeout(ctx, 10*time.Second, timeout, true, func(ctx context.Context) (bool, error) {
		pods, err := getPodsWithLabels(kclient, namespace, labelSelector)
		if err != nil {
			t.Logf("Failed to get pods: %v", err)
			return false, nil
		}

		if len(pods) == 0 {
			t.Log("No pods found")
			return false, nil
		}

		allPodsMatch := true
		for i := range pods {
			pod := &pods[i]
			haproxyConfig, err := getHAProxyConfigFromRouterPod(ctx, kubeClient, restConfig, pod)
			if err != nil {
				t.Logf("Failed to get HAProxy config from pod %s/%s (pod may be restarting): %v",
					pod.Namespace, pod.Name, err)
				allPodsMatch = false
				continue
			}

			backends, err := parseHAProxyConfig(haproxyConfig)
			if err != nil {
				t.Logf("Failed to parse HAProxy config from pod %s/%s: %v",
					pod.Namespace, pod.Name, err)
				allPodsMatch = false
				continue
			}

			backend, found := findBackend(backends, expectedBackendName, expectedServerName)
			if !found {
				allPodsMatch = false
				t.Logf("Waiting for backend %q in pod [#%d/%d] %s/%s",
					expectedBackendName, i, len(pods), pod.Namespace, pod.Name)
				continue
			}

			t.Logf("Found HAProxy backend in pod %s/%s:\nBackend: %s\nServers: %s",
				pod.Namespace, pod.Name,
				expectedBackendName, strings.Join(backend.servers, "\n  "))
		}

		return allPodsMatch, nil
	})
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

func fetchPodsForServices(ctx context.Context, namespace string, service *corev1.Service) ([]*corev1.Pod, error) {
	podList := &corev1.PodList{}
	listOptions := []client.ListOption{
		client.InNamespace(namespace),
		client.MatchingLabels(service.Spec.Selector),
	}

	if err := kclient.List(ctx, podList, listOptions...); err != nil {
		return nil, fmt.Errorf("failed to list pods for service %s: %w", service.Name, err)
	}

	if len(podList.Items) == 0 {
		return nil, fmt.Errorf("no pods found for service %s", service.Name)
	}

	pods := make([]*corev1.Pod, len(podList.Items))
	for i := range podList.Items {
		pods[i] = &podList.Items[i]
	}

	return pods, nil
}

func idleConnectionTestSetup(t *testing.T, namespace string) (*corev1.Namespace, *idleConnectionTestConfig, error) {
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

	if err := idleConnectionCreateBackendService(t, tc, 1, idleConnectionResponseServiceA); err != nil {
		return nil, nil, fmt.Errorf("failed to create backend 1: %v", err)
	}

	if err := idleConnectionCreateBackendService(t, tc, 2, idleConnectionResponseServiceB); err != nil {
		return nil, nil, fmt.Errorf("failed to create backend 2: %v", err)
	}

	tc.route, err = idleConnectionCreateRoute(tc.namespace, "test", tc.services[0].Name, tc.testLabels)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create test route: %v", err)
	}

	if _, err := waitForAllRoutesAdmitted(ns.Name, time.Minute, func(admittedRoutes, totalRoutes int, pendingRoutes []string) {
		if len(pendingRoutes) > 0 {
			t.Logf("%d/%d routes admitted. Waiting for: %s", admittedRoutes, totalRoutes, strings.Join(pendingRoutes, ", "))
		} else {
			t.Logf("All %d routes in namespace %s have been admitted", totalRoutes, ns.Name)
		}
	}); err != nil {
		return nil, nil, fmt.Errorf("not all routes admitted in namespace %s: %v", namespace, err)
	}

	for _, svc := range tc.services {
		pods, err := fetchPodsForServices(context.Background(), tc.namespace, svc)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to fetch pods for service %s: %v", svc.Name, err)
		}
		tc.pods = append(tc.pods, pods...)
	}

	return ns, tc, nil
}

func idleConnectionCreateBackendService(t *testing.T, tc *idleConnectionTestConfig, index int, serverResponse string) error {
	labels := map[string]string{
		"app":      "web-server",
		"instance": fmt.Sprintf("%d", index),
	}
	for k, v := range tc.testLabels {
		labels[k] = v
	}

	svc, err := idleConnectionCreateService(tc.namespace, index, labels)
	if err != nil {
		return fmt.Errorf("failed to create service %d: %v", index, err)
	}
	tc.services = append(tc.services, svc)

	deployment, err := idleConnectionCreateDeployment(tc.namespace, index, labels, serverResponse)
	if err != nil {
		return fmt.Errorf("failed to create deployment %d: %v", index, err)
	}
	tc.deployments = append(tc.deployments, deployment)

	if err := waitForDeploymentComplete(t, kclient, deployment, 2*time.Minute); err != nil {
		return fmt.Errorf("deployment %d is not ready: %v", index, err)
	}

	return nil
}

func idleConnectionCreateDeployment(namespace string, serviceNumber int, labels map[string]string, serverResponse string) (*appsv1.Deployment, error) {
	image, err := getCanaryImageFromIngressOperatorDeployment()
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

	if err := kclient.Create(context.TODO(), deployment); err != nil {
		return nil, err
	}

	return deployment, nil
}

func idleConnectionCreateService(namespace string, serviceNumber int, labels map[string]string) (*corev1.Service, error) {
	name := fmt.Sprintf("web-server-%d", serviceNumber)
	secretName := fmt.Sprintf("serving-cert-%s-%s", namespace, name)

	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels:    labels,
			Annotations: map[string]string{
				"service.beta.openshift.io/serving-cert-secret-name": secretName,
			},
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

	if err := kclient.Create(context.TODO(), service); err != nil {
		return nil, err
	}

	return service, nil
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
	url := fmt.Sprintf("http://%s/custom-response", route.Spec.Host)

	t.Logf("GET %s", url)

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

func switchRouteService(
	t *testing.T,
	ctx context.Context,
	tc *idleConnectionTestConfig,
	serviceIndex int,
) (*routev1.Route, error) {
	t.Helper()

	if serviceIndex >= len(tc.services) {
		return nil, fmt.Errorf("service index %d out of range", serviceIndex)
	}

	service := tc.services[serviceIndex]
	route := tc.route

	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		updatedRoute := &routev1.Route{}
		if err := kclient.Get(context.TODO(), types.NamespacedName{Name: route.Name, Namespace: route.Namespace}, updatedRoute); err != nil {
			return fmt.Errorf("failed to get route %s/%s: %w", route.Namespace, route.Name, err)
		}

		updatedRoute.Spec.To.Name = service.Name
		if err := kclient.Update(context.TODO(), updatedRoute); err != nil {
			t.Logf("Failed to update route %s/%s to point to service %s: %v, retrying...", route.Namespace, route.Name, service.Name, err)
			return err
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to update route %s/%s to point to service %s: %w", route.Namespace, route.Name, service.Name, err)
	}

	t.Logf("Updated route %s/%s to point to service %s", route.Namespace, route.Name, service.Name)

	if _, err := waitForAllRoutesAdmitted(tc.route.Namespace, time.Minute, func(admittedRoutes, totalRoutes int, pendingRoutes []string) {
		if len(pendingRoutes) > 0 {
			t.Logf("%d/%d routes admitted. Waiting for: %s", admittedRoutes, totalRoutes, strings.Join(pendingRoutes, ", "))
		} else {
			t.Logf("All %d routes in namespace %s have been admitted", totalRoutes, tc.route.Namespace)
		}
	}); err != nil {
		t.Fatalf("Error waiting for routes to be admitted: %v", err)
	}

	expectedBackendName := fmt.Sprintf("be_http:%s:%s", route.Namespace, route.Name)
	expectedServerName := fmt.Sprintf("pod:%s:%s:http:%s:%d", tc.pods[serviceIndex].Name, service.Name, tc.pods[serviceIndex].Status.PodIP, service.Spec.Ports[0].Port)

	podSelector := "ingresscontroller.operator.openshift.io/deployment-ingresscontroller: default"

	if err := waitForHAProxyConfigUpdate(t, ctx, 5*time.Minute, kclient, tc.kubeConfig, "openshift-ingress", podSelector, expectedBackendName, expectedServerName); err != nil {
		return nil, fmt.Errorf("failed waiting for HAProxy configuration update for service %s: %w", service.Name, err)
	}

	t.Logf("HAProxy configuration updated for route %s/%s to point to service %s", route.Namespace, route.Name, service.Name)

	return route, nil
}

func idleConnectionSwitchTerminationPolicy(t *testing.T, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
	t.Helper()

	icName := types.NamespacedName{
		Name:      "default",
		Namespace: "openshift-ingress-operator",
	}

	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		ic, err := getIngressController(t, kclient, icName, 1*time.Minute)
		if err != nil {
			return fmt.Errorf("failed to get IngressController: %w", err)
		}

		ic.Spec.IdleConnectionTerminationPolicy = policy
		if err := kclient.Update(context.TODO(), ic); err != nil {
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

	if err := kclient.Get(context.TODO(), routerDeploymentName, routerDeployment); err != nil {
		return fmt.Errorf("failed to get router deployment: %v", err)
	}

	if policy == operatorv1.IngressControllerConnectionTerminationPolicyDeferred {
		t.Logf("Waiting for router deployment to have environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE set to true")

		if err := waitForDeploymentEnvVar(t, kclient, routerDeployment, 2*time.Minute, "ROUTER_IDLE_CLOSE_ON_RESPONSE", "true"); err != nil {
			return fmt.Errorf("expected router deployment to have ROUTER_IDLE_CLOSE_ON_RESPONSE set to true: %v", err)
		}

		t.Logf("Router deployment has environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE set to true")
	} else if policy == operatorv1.IngressControllerConnectionTerminationPolicyImmediate {
		t.Logf("Waiting for router deployment to have environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE unset")

		if err := waitForDeploymentEnvVar(t, kclient, routerDeployment, 2*time.Minute, "ROUTER_IDLE_CLOSE_ON_RESPONSE", ""); err != nil {
			return fmt.Errorf("expected router deployment to have ROUTER_IDLE_CLOSE_ON_RESPONSE unset: %v", err)
		}

		t.Logf("Router deployment has environment variable ROUTER_IDLE_CLOSE_ON_RESPONSE unset")
	}

	return nil
}

func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	namespace := "idle-close-on-response-e2e-" + rand.String(5)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, tc, err := idleConnectionTestSetup(t, namespace)
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

	fmt.Println("setup complete")

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
			if _, err := switchRouteService(t, ctx, tc, 0); err != nil {
				return "", fmt.Errorf("failed to switch route back to Service-A: %w", err)
			}
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},

		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Step 1: Verify the response from Service-A.
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},

		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Step 2: Switch the route to Service-B and
			// fetch the response.
			_, err := switchRouteService(t, ctx, tc, 1)
			if err != nil {
				return "", fmt.Errorf("failed to switch route to Service-B: %w", err)
			}
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},

		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			// Step 3: Fetch the final response (expected to be
			// from Service-B).
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},
	}

	for _, policy := range []operatorv1.IngressControllerConnectionTerminationPolicy{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
	} {
		t.Run(string(policy), func(t *testing.T) {
			if err := idleConnectionSwitchTerminationPolicy(t, policy); err != nil {
				t.Fatalf("failed to switch to policy %s: %v", policy, err)
			}

			tc.httpClient = &http.Client{
				Timeout: 30 * time.Second,
				Transport: &http.Transport{
					IdleConnTimeout:     90 * time.Second,
					MaxIdleConns:        100,
					MaxIdleConnsPerHost: 10,
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
