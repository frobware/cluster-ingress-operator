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

func idleConnectionTestSetup(t *testing.T, baseName string) (*corev1.Namespace, *idleConnectionTestConfig, error) {
	tc := &idleConnectionTestConfig{
		testLabels: map[string]string{
			"test": "idle-connection",
			"app":  "web-server",
		},
	}

	ns := createNamespace(t, baseName+"-"+rand.String(5))
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
							Args:            []string{"serve-healthcheck"},
							Ports: []corev1.ContainerPort{
								{Name: "https", ContainerPort: 8443},
							},
							Env: []corev1.EnvVar{
								{Name: "CUSTOM_RESPONSE", Value: serverResponse},
								{Name: "PORT", Value: "8443"},
								{Name: "TLS_CERT", Value: "/etc/serving-cert/tls.crt"},
								{Name: "TLS_KEY", Value: "/etc/serving-cert/tls.key"},
							},
							ReadinessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(8443),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
							},
							LivenessProbe: &corev1.Probe{
								ProbeHandler: corev1.ProbeHandler{
									TCPSocket: &corev1.TCPSocketAction{
										Port: intstr.FromInt(8443),
									},
								},
								InitialDelaySeconds: 5,
								PeriodSeconds:       10,
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
				Name:       "https",
				Port:       8443,
				TargetPort: intstr.FromInt(8443),
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
				TargetPort: intstr.FromString("https"),
			},
			TLS: &routev1.TLSConfig{
				Termination:                   routev1.TLSTerminationReencrypt,
				InsecureEdgeTerminationPolicy: routev1.InsecureEdgeTerminationPolicyRedirect,
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
	url := fmt.Sprintf("http://%s", route.Spec.Host)

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
	baseName := "idle-close-on-response-e2e"

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	_, tc, err := idleConnectionTestSetup(t, baseName)
	if err != nil {
		t.Fatalf("failed to set up test resources: %v", err)
	}

	fmt.Println("setup complete")
	select {}

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

	// Step 2: Define expected responses for each policy.
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

	// Step 3: Function to switch IC policy
	switchPolicy := func(t *testing.T, policy operatorv1.IngressControllerConnectionTerminationPolicy) error {
		t.Helper()
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			ic, err := getIngressController(t, kclient, icName, 1*time.Minute)
			if err != nil {
				return fmt.Errorf("failed to get IngressController: %w", err)
			}

			ic.Spec.IdleConnectionTerminationPolicy = policy
			if err := kclient.Update(context.TODO(), ic); err != nil {
				t.Logf("Failed to update IdleConnectionTerminationPolicy: %v, retrying...", err)
				return err
			}
			return nil
		}); err != nil {
			return fmt.Errorf("failed to switch policy to %s: %w", policy, err)
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
		t.Run(fmt.Sprintf("Testing policy: %s", policy), func(t *testing.T) {
			if err := switchPolicy(t, policy); err != nil {
				t.Fatalf("failed to switch to policy %s: %v", policy, err)
			}

			tc.httpClient = &http.Client{
				Timeout: 10 * time.Second,
				Transport: &http.Transport{
					DisableKeepAlives: policy == operatorv1.IngressControllerConnectionTerminationPolicyImmediate,
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
