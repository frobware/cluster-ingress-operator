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
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func waitForDeploymentCompleteAndNoOldPods(
	t *testing.T,
	deploymentName types.NamespacedName,
	timeout time.Duration,
) error {
	startTime := time.Now()
	t.Logf("[DEBUG] Starting to wait for deployment %s to complete (timeout: %v)", deploymentName, timeout)

	return wait.PollImmediate(time.Second*2, timeout, func() (bool, error) {
		elapsed := time.Since(startTime).Round(time.Second)

		// Get deployment
		deployment := &appsv1.Deployment{}
		if err := kclient.Get(context.Background(), deploymentName, deployment); err != nil {
			t.Logf("[DEBUG] Failed to get deployment: %v", err)
			return false, fmt.Errorf("failed to get deployment: %v", err)
		}
		t.Logf("[DEBUG] Found deployment with generation %d, observed generation %d",
			deployment.Generation, deployment.Status.ObservedGeneration)
		t.Logf("[DEBUG] Deployment conditions: %+v", deployment.Status.Conditions)

		// Get all pods matching deployment selector
		podList := &corev1.PodList{}
		if err := kclient.List(context.Background(), podList,
			client.InNamespace(deploymentName.Namespace),
			client.MatchingLabels(deployment.Spec.Selector.MatchLabels)); err != nil {
			t.Logf("[DEBUG] Failed to list pods: %v", err)
			return false, fmt.Errorf("failed to list pods: %v", err)
		}
		t.Logf("[DEBUG] Found %d pods matching deployment selector", len(podList.Items))

		// Get all replicasets
		rsList := &appsv1.ReplicaSetList{}
		if err := kclient.List(context.Background(), rsList,
			client.InNamespace(deploymentName.Namespace),
			client.MatchingLabels(deployment.Spec.Selector.MatchLabels)); err != nil {
			t.Logf("[DEBUG] Failed to list replicasets: %v", err)
			return false, fmt.Errorf("failed to list replicasets: %v", err)
		}
		t.Logf("[DEBUG] Found %d replicasets matching deployment selector", len(rsList.Items))

		// Find current revision by looking at the newest ReplicaSet
		var currentRevision string
		var newestTimestamp time.Time
		t.Logf("[DEBUG] Examining ReplicaSets to find current revision:")
		for _, rs := range rsList.Items {
			rsTimestamp := rs.CreationTimestamp.Time
			t.Logf("[DEBUG]   ReplicaSet %s: revision=%s created=%v",
				rs.Name,
				rs.Annotations["deployment.kubernetes.io/revision"],
				rsTimestamp)

			if currentRevision == "" || rsTimestamp.After(newestTimestamp) {
				currentRevision = rs.Annotations["deployment.kubernetes.io/revision"]
				newestTimestamp = rsTimestamp
				t.Logf("[DEBUG]     ^ This is now the newest ReplicaSet")
			}
		}
		t.Logf("[DEBUG] Current revision is %s (found from newest ReplicaSet)", currentRevision)

		// Log ReplicaSet details
		for _, rs := range rsList.Items {
			t.Logf("[DEBUG] ReplicaSet %s: revision=%s replicas=%d/%d ready=%d available=%d",
				rs.Name,
				rs.Annotations["deployment.kubernetes.io/revision"],
				rs.Status.Replicas,
				*rs.Spec.Replicas,
				rs.Status.ReadyReplicas,
				rs.Status.AvailableReplicas)
		}

		// Check each pod's owner references to ensure no pods from old replicasets exist
		t.Logf("[DEBUG] Checking %d pods for old revisions", len(podList.Items))
		foundOldPods := false
		for _, pod := range podList.Items {
			t.Logf("[DEBUG] Inspecting pod %s (namespace: %s)", pod.Name, pod.Namespace)
			t.Logf("[DEBUG]   Phase: %s", pod.Status.Phase)
			t.Logf("[DEBUG]   Owner references: %+v", pod.OwnerReferences)
			if pod.DeletionTimestamp != nil {
				t.Logf("[DEBUG]   Pod is terminating (deletion timestamp: %v)", pod.DeletionTimestamp)
				gracePeriod := pod.DeletionGracePeriodSeconds
				if gracePeriod != nil {
					t.Logf("[DEBUG]   Grace period: %ds", *gracePeriod)
				}
			}

			for _, ownerRef := range pod.OwnerReferences {
				if ownerRef.Kind == "ReplicaSet" {
					t.Logf("[DEBUG]   Pod %s is owned by ReplicaSet %s", pod.Name, ownerRef.Name)
					// Find the owner ReplicaSet
					for _, rs := range rsList.Items {
						if string(rs.UID) == string(ownerRef.UID) {
							rsRevision := rs.Annotations["deployment.kubernetes.io/revision"]
							t.Logf("[DEBUG]   Found owner ReplicaSet %s with revision %s", rs.Name, rsRevision)
							// Check if this RS is from an old revision
							if rsRevision != currentRevision {
								foundOldPods = true
								t.Logf("[DEBUG]   Pod %s from old revision %s (current: %s) still exists after %v",
									pod.Name, rsRevision, currentRevision, elapsed)
								if pod.DeletionTimestamp != nil {
									t.Logf("[DEBUG]   Pod is terminating, will wait for graceful deletion")
								}
							}
						}
					}
				}
			}
		}

		if foundOldPods {
			t.Logf("[DEBUG] Found pods from old revisions, waiting for them to terminate")
			return false, nil
		}

		// Check if deployment rollout is done
		if deployment.Status.ObservedGeneration != deployment.Generation {
			t.Logf("[DEBUG] [%v elapsed] Waiting for deployment generation (%d) to match observed generation (%d)",
				elapsed, deployment.Generation, deployment.Status.ObservedGeneration)
			return false, nil
		}

		for _, cond := range deployment.Status.Conditions {
			if cond.Type == appsv1.DeploymentProgressing {
				t.Logf("[DEBUG] Progressing condition: status=%s reason=%s message=%s",
					cond.Status, cond.Reason, cond.Message)

				if cond.Status == corev1.ConditionTrue && cond.Reason == "NewReplicaSetAvailable" {
					t.Logf("[DEBUG] [%v elapsed] Deployment is complete and no old pods exist", elapsed)
					return true, nil
				}
			}
		}

		t.Logf("[DEBUG] [%v elapsed] Still waiting for deployment to complete...", elapsed)
		t.Logf("[DEBUG] Current status: replicas=%d, updated=%d, ready=%d, available=%d",
			deployment.Status.Replicas,
			deployment.Status.UpdatedReplicas,
			deployment.Status.ReadyReplicas,
			deployment.Status.AvailableReplicas)
		return false, nil
	})
}
