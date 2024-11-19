package e2e

import (
	"context"
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
	"k8s.io/client-go/util/retry"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	operatorv1 "github.com/openshift/api/operator/v1"
	routev1 "github.com/openshift/api/route/v1"
)

const (
	idleConnectionResponseServiceA = "Service A"
	idleConnectionResponseServiceB = "Service B"
)

type idleConnectionTestConfig struct {
	namespace   string
	services    []*corev1.Service
	deployments []*appsv1.Deployment
	route       *routev1.Route
	testLabels  map[string]string
	httpClient  *http.Client
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

func idleConnectionTestSetup(t *testing.T, namespace string) (*corev1.Namespace, *idleConnectionTestConfig, error) {
	tc := &idleConnectionTestConfig{
		testLabels: map[string]string{
			"test": "idle-connection",
			"app":  "web-server",
		},
	}

	ns := createNamespace(t, namespace)
	tc.namespace = ns.Name

	if err := idleConnectionCreateBackendService(t, tc, 1, idleConnectionResponseServiceA); err != nil {
		return nil, nil, fmt.Errorf("failed to create backend 1: %v", err)
	}

	if err := idleConnectionCreateBackendService(t, tc, 2, idleConnectionResponseServiceB); err != nil {
		return nil, nil, fmt.Errorf("failed to create backend 2: %v", err)
	}

	var err error
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

	// Wait for the deployment to complete
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
										Port:   intstr.FromInt(8080),
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
										Port:   intstr.FromInt(8080),
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
				TargetPort: intstr.FromInt(8080),
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

	return responseString, nil
}

func switchRouteService(t *testing.T, ctx context.Context, tc *idleConnectionTestConfig, serviceIndex int) (*routev1.Route, error) {
	return nil, nil
}

func Test_IdleConnectionTerminationPolicy(t *testing.T) {
	namespace := "idle-close-on-response-e2e-" + rand.String(5)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, tc, err := idleConnectionTestSetup(t, namespace)
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

	fmt.Println("setup complete")

	expectedResponses := map[operatorv1.IngressControllerConnectionTerminationPolicy][]string{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred: {
			idleConnectionResponseServiceA,
			idleConnectionResponseServiceA,
			idleConnectionResponseServiceB,
		},
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate: {
			idleConnectionResponseServiceA,
			idleConnectionResponseServiceB,
			idleConnectionResponseServiceB,
		},
	}

	switchPolicy := func(t *testing.T, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
		t.Helper()

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

		time.Sleep(30 * time.Second)
		return nil
	}

	actions := []func(ctx context.Context, tc *idleConnectionTestConfig) (string, error){
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			_, err := switchRouteService(t, ctx, tc, 1)
			if err != nil {
				return "", err
			}
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},
		func(ctx context.Context, tc *idleConnectionTestConfig) (string, error) {
			return fetchServiceResponse(t, tc.route, tc.httpClient)
		},
	}

	for _, policy := range []operatorv1.IngressControllerConnectionTerminationPolicy{
		operatorv1.IngressControllerConnectionTerminationPolicyDeferred,
		operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
	} {
		t.Run(string(policy), func(t *testing.T) {
			if err := switchPolicy(t, policy); err != nil {
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
