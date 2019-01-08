package controller

import (
	"encoding/json"
	"fmt"
	"sort"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	patchtypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/klog"
	"k8s.io/kubernetes/pkg/controller"

	"github.com/argoproj/rollout-controller/pkg/apis/rollouts/v1alpha1"
	"github.com/argoproj/rollout-controller/utils/annotations"
	"github.com/argoproj/rollout-controller/utils/conditions"
	"github.com/argoproj/rollout-controller/utils/defaults"
	replicasetutil "github.com/argoproj/rollout-controller/utils/replicaset"
)

// rolloutBlueGreen implements the logic for rolling a new replica set.
func (c *Controller) rolloutBlueGreen(r *v1alpha1.Rollout, rsList []*appsv1.ReplicaSet) error {
	newRS, oldRSs, err := c.getAllReplicaSetsAndSyncRevision(r, rsList, true)
	if err != nil {
		return err
	}
	previewSvc, activeSvc, err := c.getPreviewAndActiveServices(r)
	if err != nil {
		return err
	}
	allRSs := append(oldRSs, newRS)

	// Scale up, if we can.
	scaledUp, err := c.reconcileNewReplicaSet(allRSs, newRS, r)
	if err != nil {
		return err
	}
	if scaledUp {
		return c.syncRolloutStatus(allRSs, newRS, previewSvc, activeSvc, r)
	}

	if previewSvc != nil {
		switchPreviewSvc, err := c.reconcilePreviewService(r, newRS, previewSvc, activeSvc)
		if err != nil {
			return err
		}
		if switchPreviewSvc {
			return c.syncRolloutStatus(allRSs, newRS, previewSvc, activeSvc, r)
		}

		verfyingPreview := c.reconcileVerifyingPreview(activeSvc, r)
		if verfyingPreview {
			return c.syncRolloutStatus(allRSs, newRS, previewSvc, activeSvc, r)
		}
	}

	switchActiveSvc, err := c.reconcileActiveService(r, newRS, previewSvc, activeSvc)
	if err != nil {
		return err
	}
	if switchActiveSvc {
		return c.syncRolloutStatus(allRSs, newRS, previewSvc, activeSvc, r)
	}
	// Scale down, if we can.
	scaledDown, err := c.reconcileOldReplicaSets(allRSs, controller.FilterActiveReplicaSets(oldRSs), newRS, r)
	if err != nil {
		return err
	}
	if scaledDown {
		return c.syncRolloutStatus(allRSs, newRS, previewSvc, activeSvc, r)
	}

	if conditions.RolloutComplete(r, &r.Status) {
		if err := c.cleanupRollouts(oldRSs, r); err != nil {
			return err
		}
	}

	return c.syncRolloutStatus(allRSs, newRS, previewSvc, activeSvc, r)
}

func (c *Controller) reconcileVerifyingPreview(activeSvc *corev1.Service, rollout *v1alpha1.Rollout) bool {
	if rollout.Spec.Strategy.BlueGreenStrategy.PreviewService == "" {
		return false
	}
	if _, ok := activeSvc.Spec.Selector[v1alpha1.DefaultRolloutUniqueLabelKey]; !ok {
		return false
	}

	if rollout.Status.VerifyingPreview == nil {
		return false
	}

	return *rollout.Status.VerifyingPreview
}

func (c *Controller) reconcileNewReplicaSet(allRSs []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet, rollout *v1alpha1.Rollout) (bool, error) {
	rolloutReplicas := defaults.GetRolloutReplicasOrDefault(rollout)
	if *(newRS.Spec.Replicas) == rolloutReplicas {
		// Scaling not required.
		return false, nil
	}
	if *(newRS.Spec.Replicas) > rolloutReplicas {
		// Scale down.
		scaled, _, err := c.scaleReplicaSetAndRecordEvent(newRS, rolloutReplicas, rollout)
		return scaled, err
	}
	newReplicasCount, err := replicasetutil.NewRSNewReplicas(rollout, allRSs, newRS)
	if err != nil {
		return false, err
	}
	scaled, _, err := c.scaleReplicaSetAndRecordEvent(newRS, newReplicasCount, rollout)
	return scaled, err
}

func (c *Controller) reconcileOldReplicaSets(allRSs []*appsv1.ReplicaSet, oldRSs []*appsv1.ReplicaSet, newRS *appsv1.ReplicaSet, rollout *v1alpha1.Rollout) (bool, error) {
	oldPodsCount := replicasetutil.GetReplicaCountForReplicaSets(oldRSs)
	if oldPodsCount == 0 {
		// Can't scale down further
		return false, nil
	}

	klog.V(4).Infof("New replica set %s/%s has %d available pods.", newRS.Namespace, newRS.Name, newRS.Status.AvailableReplicas)
	if !annotations.IsSaturated(rollout, newRS) {
		return false, nil
	}
	// Add check for active service

	// Clean up unhealthy replicas first, otherwise unhealthy replicas will block deployment
	// and cause timeout. See https://github.com/kubernetes/kubernetes/issues/16737
	oldRSs, cleanupCount, err := c.cleanupUnhealthyReplicas(oldRSs, rollout)
	if err != nil {
		return false, nil
	}
	klog.V(4).Infof("Cleaned up unhealthy replicas from old RSes by %d", cleanupCount)

	// Scale down old replica sets, need check replicasToKeep to ensure we can scale down
	allRSs = append(oldRSs, newRS)
	scaledDownCount, err := c.scaleDownOldReplicaSetsForBlueGreen(allRSs, oldRSs, rollout)
	if err != nil {
		return false, nil
	}
	klog.V(4).Infof("Scaled down old RSes of deployment %s by %d", rollout.Name, scaledDownCount)

	totalScaledDown := cleanupCount + scaledDownCount
	return totalScaledDown > 0, nil
}

// cleanupUnhealthyReplicas will scale down old replica sets with unhealthy replicas, so that all unhealthy replicas will be deleted.
func (c *Controller) cleanupUnhealthyReplicas(oldRSs []*appsv1.ReplicaSet, rollout *v1alpha1.Rollout) ([]*appsv1.ReplicaSet, int32, error) {
	sort.Sort(controller.ReplicaSetsByCreationTimestamp(oldRSs))
	// Safely scale down all old replica sets with unhealthy replicas. Replica set will sort the pods in the order
	// such that not-ready < ready, unscheduled < scheduled, and pending < running. This ensures that unhealthy replicas will
	// been deleted first and won't increase unavailability.
	totalScaledDown := int32(0)
	for i, targetRS := range oldRSs {
		if *(targetRS.Spec.Replicas) == 0 {
			// cannot scale down this replica set.
			continue
		}
		klog.V(4).Infof("Found %d available pods in old RS %s/%s", targetRS.Status.AvailableReplicas, targetRS.Namespace, targetRS.Name)
		if *(targetRS.Spec.Replicas) == targetRS.Status.AvailableReplicas {
			// no unhealthy replicas found, no scaling required.
			continue
		}

		scaledDownCount := *(targetRS.Spec.Replicas) - targetRS.Status.AvailableReplicas
		newReplicasCount := targetRS.Status.AvailableReplicas
		if newReplicasCount > *(targetRS.Spec.Replicas) {
			return nil, 0, fmt.Errorf("when cleaning up unhealthy replicas, got invalid request to scale down %s/%s %d -> %d", targetRS.Namespace, targetRS.Name, *(targetRS.Spec.Replicas), newReplicasCount)
		}
		_, updatedOldRS, err := c.scaleReplicaSetAndRecordEvent(targetRS, newReplicasCount, rollout)
		if err != nil {
			return nil, totalScaledDown, err
		}
		totalScaledDown += scaledDownCount
		oldRSs[i] = updatedOldRS
	}
	return oldRSs, totalScaledDown, nil
}

// scaleDownOldReplicaSetsForBlueGreen scales down old replica sets when rollout strategy is "Blue Green".
func (c *Controller) scaleDownOldReplicaSetsForBlueGreen(allRSs []*appsv1.ReplicaSet, oldRSs []*appsv1.ReplicaSet, rollout *v1alpha1.Rollout) (int32, error) {
	availablePodCount := replicasetutil.GetAvailableReplicaCountForReplicaSets(allRSs)
	if availablePodCount <= defaults.GetRolloutReplicasOrDefault(rollout) {
		// Cannot scale down.
		return 0, nil
	}
	klog.V(4).Infof("Found %d available pods in rollout %s, scaling down old RSes", availablePodCount, rollout.Name)

	sort.Sort(controller.ReplicaSetsByCreationTimestamp(oldRSs))

	totalScaledDown := int32(0)
	for _, targetRS := range oldRSs {
		if *(targetRS.Spec.Replicas) == 0 {
			// cannot scale down this ReplicaSet.
			continue
		}
		scaleDownCount := *(targetRS.Spec.Replicas)
		// Scale down.
		newReplicasCount := int32(0)
		_, _, err := c.scaleReplicaSetAndRecordEvent(targetRS, newReplicasCount, rollout)
		if err != nil {
			return totalScaledDown, err
		}

		totalScaledDown += scaleDownCount
	}

	return totalScaledDown, nil
}

func (c *Controller) setVerifyingPreview(r *v1alpha1.Rollout) error {
	verifyPreview := true
	patch := v1alpha1.Rollout{
		Status: v1alpha1.RolloutStatus{
			VerifyingPreview: &verifyPreview,
		},
	}
	patchBytes, err := json.Marshal(patch)
	if err != nil {
		return err
	}

	_, err = c.rolloutsclientset.ArgoprojV1alpha1().Rollouts(r.Namespace).Patch(r.Name, patchtypes.MergePatchType, patchBytes)
	return err
}