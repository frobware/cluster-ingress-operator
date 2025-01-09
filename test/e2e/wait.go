//go:build e2e
// +build e2e

package e2e

import (
	"context"
	"fmt"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// waitForDeploymentCompleteAndNoOldPods waits for a deployment to
// complete a roll-out by watching for the deployment's generation to
// advance beyond a known starting point. This avoids races that could
// occur if we started watching after the roll-out had already begun or
// completed.
//
// The function takes a startingGeneration parameter which represents
// the deployment's generation before any changes were made. It then
// waits until:
//
//  1. The deployment generation advances beyond startingGeneration
//     (indicating a change was detected).
//
//  2. The number of pods exactly matches the deployment's desired
//     replica count, with all pods running and ready.
//
//  3. No pods are in a terminating state.
//
// This ensures we see both the start of the roll-out (generation
// advancing) and its completion (all old pods gone, exact number of
// new pods ready and running).
//
// For cases involving pod termination with grace periods, this
// function will continue to wait until the terminating pods are fully
// removed from the API server.
func waitForDeploymentCompleteAndNoOldPods(
	t *testing.T,
	deploymentName types.NamespacedName,
	startingGeneration int64,
	interval, timeout time.Duration,
) error {
	t.Helper()

	startTime := time.Now()
	t.Logf("[%s] Waiting for deployment %s to move past generation %d (timeout: %v)",
		deploymentName.String(), deploymentName, startingGeneration, timeout)

	return wait.PollUntilContextTimeout(context.Background(), interval, timeout, false, func(ctx context.Context) (bool, error) {
		deployment := &appsv1.Deployment{}
		if err := kclient.Get(context.Background(), deploymentName, deployment); err != nil {
			return false, fmt.Errorf("failed to get deployment %s: %v", deploymentName, err)
		}

		// If spec.replicas is null, the default value is 1,
		// per the API spec.
		expectedReplicas := ptr.Deref(deployment.Spec.Replicas, 1)

		podList := &corev1.PodList{}
		if err := kclient.List(context.Background(), podList,
			client.InNamespace(deploymentName.Namespace),
			client.MatchingLabels(deployment.Spec.Selector.MatchLabels)); err != nil {
			return false, fmt.Errorf("failed to list pods for deployment %s: %v", deploymentName, err)
		}

		elapsed := time.Since(startTime).Round(time.Second)
		t.Logf("[%v elapsed] Deployment %s status:", elapsed, deploymentName.String())
		t.Logf("  Generation: %d/%d (starting at %d)",
			deployment.Status.ObservedGeneration,
			deployment.Generation,
			startingGeneration)
		t.Logf("  Pods: %d current, %d desired",
			len(podList.Items),
			expectedReplicas)

		// Wait until the deployment moves past our starting
		// generation.
		if deployment.Generation <= startingGeneration {
			t.Logf("Waiting for deployment %s to move past generation %d (currently %d)",
				deploymentName, startingGeneration, deployment.Generation)
			return false, nil
		}

		var (
			readyAndRunning int32
			terminatingPods int32
		)

		for _, pod := range podList.Items {
			if pod.DeletionTimestamp != nil {
				terminatingPods++
				t.Logf("  Pod %s in deployment %s is terminating (grace period: %ds)",
					pod.Name, deploymentName, ptr.Deref(pod.DeletionGracePeriodSeconds, 0))
				continue
			}

			isReady := false
			if pod.Status.Phase == corev1.PodRunning {
				for _, condition := range pod.Status.Conditions {
					if condition.Type == corev1.PodReady && condition.Status == corev1.ConditionTrue {
						readyAndRunning++
						isReady = true
						break
					}
				}
			}

			t.Logf("  Pod %s in deployment %s is %s (ready: %v)",
				pod.Name, deploymentName, pod.Status.Phase, isReady)
		}

		if readyAndRunning != expectedReplicas || terminatingPods > 0 {
			t.Logf("Deployment %s: Waiting for pods to be ready and running (%d ready+running, %d terminating, %d desired)",
				deploymentName, readyAndRunning, terminatingPods, expectedReplicas)
			return false, nil
		}

		t.Logf("Deployment %s complete in %s: Moved from generation %d to %d with %d pods ready and running",
			deploymentName, elapsed.Round(time.Second), startingGeneration, deployment.Generation, readyAndRunning)
		return true, nil
	})
}
