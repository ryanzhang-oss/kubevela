package workloads

import (
	"context"
	"fmt"

	"github.com/crossplane/crossplane-runtime/pkg/event"
	apps "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/klog/v2"
	"k8s.io/utils/pointer"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha2"
	"github.com/oam-dev/kubevela/apis/standard.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/pkg/controller/common"
	"github.com/oam-dev/kubevela/pkg/controller/utils"
	"github.com/oam-dev/kubevela/pkg/oam"
)

// DeploymentController is responsible for handle rollout deployment type of workloads
type DeploymentController struct {
	client           client.Client
	recorder         event.Recorder
	parentController oam.Object

	rolloutSpec          *v1alpha1.RolloutPlan
	rolloutStatus        *v1alpha1.RolloutStatus
	targetNamespacedName types.NamespacedName
	sourceNamespacedName types.NamespacedName
	sourceDeploy         apps.Deployment
	targetDeploy         apps.Deployment
}

// NewDeploymentController creates a new Deployment rollout controller
func NewDeploymentController(client client.Client, recorder event.Recorder, parentController oam.Object,
	rolloutSpec *v1alpha1.RolloutPlan, rolloutStatus *v1alpha1.RolloutStatus, sourceNamespacedName,
	targetNamespacedName types.NamespacedName) *DeploymentController {
	return &DeploymentController{
		client:               client,
		recorder:             recorder,
		parentController:     parentController,
		rolloutSpec:          rolloutSpec,
		rolloutStatus:        rolloutStatus,
		sourceNamespacedName: sourceNamespacedName,
		targetNamespacedName: targetNamespacedName,
	}
}

// VerifySpec verifies that the rollout resource is consistent with the rollout spec
func (c *DeploymentController) VerifySpec(ctx context.Context) (bool, error) {
	var verifyErr error

	defer func() {
		if verifyErr != nil {
			klog.Error(verifyErr)
			c.recorder.Event(c.parentController, event.Warning("VerifyFailed", verifyErr))
		}
	}()

	// check if the rollout spec is compatible with the current state
	totalReplicas, verifyErr := c.calculateTargetSize(ctx)
	if verifyErr != nil {
		// do not fail the rollout just because we can't get the resource
		c.rolloutStatus.RolloutRetry(verifyErr.Error())
		return false, nil
	}
	// record the size and we will use this value to drive the rest of the batches
	// we do not handle scale case in this controller
	c.rolloutStatus.RolloutTargetSize = totalReplicas

	// make sure that the updateRevision is different from what we have already done
	targetHash, verifyErr := utils.ComputeSpecHash(c.targetDeploy.Spec)
	if verifyErr != nil {
		// do not fail the rollout because we can't compute the hash value for some reason
		c.rolloutStatus.RolloutRetry(verifyErr.Error())
		return false, nil
	}
	if targetHash == c.rolloutStatus.LastAppliedPodTemplateIdentifier {
		return false, fmt.Errorf("there is no difference between the source and target, hash = %s", targetHash)
	}
	// record the new pod template hash
	c.rolloutStatus.NewPodTemplateIdentifier = targetHash

	// check if the rollout batch replicas added up to the Deployment replicas
	// we don't handle scale case in this controller
	if verifyErr = c.verifyRolloutBatchReplicaValue(totalReplicas); verifyErr != nil {
		return false, verifyErr
	}

	if !c.targetDeploy.Spec.Paused && c.targetDeploy.Spec.Replicas != pointer.Int32Ptr(0) {
		return false, fmt.Errorf("the Deployment %s is in the middle of updating, need to be paused or empty",
			c.sourceDeploy.GetName())
	}

	// check if the targetDeploy has any controller
	if controller := metav1.GetControllerOf(&c.targetDeploy); controller != nil {
		return false, fmt.Errorf("the target deployment %s has a controller owner %s",
			c.targetDeploy.GetName(), controller.String())
	}

	// check if the sourceDeploy has any controller
	if controller := metav1.GetControllerOf(&c.sourceDeploy); controller != nil {
		return false, fmt.Errorf("the source deployment %s has a controller owner %s",
			c.sourceDeploy.GetName(), controller.String())
	}

	// mark the rollout verified
	c.recorder.Event(c.parentController, event.Normal("Rollout Verified",
		"Rollout spec and the Deployment resource are verified"))
	return true, nil
}

// Initialize makes sure that the source and target deployment is under our control
func (c *DeploymentController) Initialize(ctx context.Context) (bool, error) {
	err := c.fetchDeployments(ctx)
	if err != nil {
		c.rolloutStatus.RolloutRetry(err.Error())
		return false, nil
	}
	err = c.claimDeployment(ctx, &c.sourceDeploy)
	if err != nil {
		c.rolloutStatus.RolloutRetry(err.Error())
		return false, nil
	}
	err = c.claimDeployment(ctx, &c.targetDeploy)
	if err != nil {
		c.rolloutStatus.RolloutRetry(err.Error())
		return false, nil
	}
	// mark the rollout initialized
	c.recorder.Event(c.parentController, event.Normal("Rollout Initialized", "Rollout resource are initialized"))
	return true, nil
}

// RolloutOneBatchPods calculates the number of pods we can upgrade once according to the rollout spec
// and then set the partition accordingly
func (c *DeploymentController) RolloutOneBatchPods(ctx context.Context) (bool, error) {
	err := c.fetchDeployments(ctx)
	if err != nil {
		// don't fail the rollout just because of we can't get the resource
		return false, nil
	}
	currentSizeSetting := *c.sourceDeploy.Spec.Replicas + *c.targetDeploy.Spec.Replicas
	// get the rollout strategy
	rolloutStrategy := v1alpha1.IncreaseFirstRolloutStrategyType
	if len(c.rolloutSpec.RolloutStrategy) != 0 {
		rolloutStrategy = c.rolloutSpec.RolloutStrategy
	}
	// Determine if we are the first or the second part of the current batch rollout
	if currentSizeSetting == c.rolloutStatus.RolloutTargetSize {
		// we need to finish the first part of the rollout,
		// will always return false to not move to the next phase
		return false, c.rolloutBatchFirstHalf(ctx, rolloutStrategy)
	}
	// we are at the second half
	targetSize := c.calculateCurrentTarget(c.rolloutStatus.RolloutTargetSize)
	if !c.rolloutBatchSecondHalf(ctx, rolloutStrategy, targetSize) {
		return false, nil
	}
	// record the finished upgrade action
	klog.InfoS("upgraded one batch", "current batch", c.rolloutStatus.CurrentBatch,
		"target deployment size", targetSize)
	c.recorder.Event(c.parentController, event.Normal("Batch Rollout",
		fmt.Sprintf("Submitted upgrade quest for batch %d", c.rolloutStatus.CurrentBatch)))
	c.rolloutStatus.UpgradedReplicas = targetSize
	return true, nil
}

// CheckOneBatchPods checks to see if the pods are all available according to the rollout plan
func (c *DeploymentController) CheckOneBatchPods(ctx context.Context) (bool, error) {
	err := c.fetchDeployments(ctx)
	if err != nil {
		// don't fail the rollout just because of we can't get the resource
		return false, nil
	}
	// get the number of ready pod from target
	readyTargetPodCount := c.targetDeploy.Status.ReadyReplicas
	sourcePodCount := c.sourceDeploy.Status.Replicas
	currentBatch := c.rolloutSpec.RolloutBatches[c.rolloutStatus.CurrentBatch]
	targetGoal := c.calculateCurrentTarget(c.rolloutStatus.RolloutTargetSize)
	sourceGoal := c.calculateCurrentSource(c.rolloutStatus.RolloutTargetSize)
	maxUnavail := 0
	if currentBatch.MaxUnavailable != nil {
		maxUnavail, _ = intstr.GetValueFromIntOrPercent(currentBatch.MaxUnavailable, int(targetGoal), true)
	}
	klog.InfoS("checking the rolling out progress", "current batch", c.rolloutStatus.CurrentBatch,
		"target pod ready count", readyTargetPodCount, "source pod count", sourcePodCount,
		"max unavailable pod allowed", maxUnavail, "target goal", targetGoal, "source goal", sourceGoal)

	// make sure that the source deployment has the correct pods before moving the target
	// and the total we could overshoot in revert cases
	if sourcePodCount != sourceGoal ||
		int32(maxUnavail)+readyTargetPodCount+sourcePodCount < c.rolloutStatus.RolloutTargetSize {
		// continue to verify
		klog.InfoS("the batch is not ready yet", "current batch", c.rolloutStatus.CurrentBatch)
		c.rolloutStatus.RolloutRetry(fmt.Sprintf(
			"the batch %d is not ready yet with %d target pods ready and %d source pods with %d unavailable allowed",
			c.rolloutStatus.CurrentBatch, readyTargetPodCount, sourcePodCount, maxUnavail))
		return false, nil
	}
	// record the successful upgrade
	c.rolloutStatus.UpgradedReadyReplicas = readyTargetPodCount
	klog.InfoS("all pods in current batch are ready", "current batch", c.rolloutStatus.CurrentBatch)
	c.recorder.Event(c.parentController, event.Normal("Batch Available",
		fmt.Sprintf("Batch %d is available", c.rolloutStatus.CurrentBatch)))
	c.rolloutStatus.LastAppliedPodTemplateIdentifier = c.rolloutStatus.NewPodTemplateIdentifier
	return true, nil
}

// FinalizeOneBatch makes sure that the rollout status are updated correctly
func (c *DeploymentController) FinalizeOneBatch(ctx context.Context) (bool, error) {
	// nothing to do for Deployment for now
	return false, nil
}

// Finalize makes sure the Deployment is all upgraded
func (c *DeploymentController) Finalize(ctx context.Context, succeed bool) bool {
	err := c.releaseDeployment(ctx, &c.sourceDeploy)
	if err != nil {
		c.rolloutStatus.RolloutRetry(err.Error())
		return false
	}
	err = c.releaseDeployment(ctx, &c.targetDeploy)
	if err != nil {
		c.rolloutStatus.RolloutRetry(err.Error())
		return false
	}
	// mark the resource finalized
	c.recorder.Event(c.parentController, event.Normal("Rollout Finalized",
		fmt.Sprintf("Rollout resource are finalized, succeed := %t", succeed)))
	return true
}

/* --------------------
The functions below are helper functions
--------------------- */

// calculateTargetSize fetches the Deployment and returns the replicas (not the actual number of pods)
func (c *DeploymentController) calculateTargetSize(ctx context.Context) (int32, error) {
	if err := c.fetchDeployments(ctx); err != nil {
		return -1, err
	}

	// source default is 1
	var sourceSize int32 = 1
	var targetSize int32 = 0
	if c.sourceDeploy.Spec.Replicas != nil {
		sourceSize = *c.sourceDeploy.Spec.Replicas
	}
	if c.targetDeploy.Spec.Replicas != nil {
		targetSize = *c.targetDeploy.Spec.Replicas
	}
	return sourceSize + targetSize, nil
}

// check if the replicas in all the rollout batches add up to the right number
func (c *DeploymentController) verifyRolloutBatchReplicaValue(totalReplicas int32) error {
	// the target size has to be the same as the Deployment size
	if c.rolloutSpec.TargetSize != nil && *c.rolloutSpec.TargetSize != totalReplicas {
		return fmt.Errorf("the rollout plan is attempting to scale the Deployment, target = %d, Deployment size = %d",
			*c.rolloutSpec.TargetSize, totalReplicas)
	}
	// use a common function to check if the sum of all the batches can match the Deployment size
	err := VerifySumOfBatchSizes(c.rolloutSpec, totalReplicas)
	if err != nil {
		return err
	}
	return nil
}

func (c *DeploymentController) fetchDeployments(ctx context.Context) error {
	var workload apps.Deployment
	err := c.client.Get(ctx, c.sourceNamespacedName, &workload)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.recorder.Event(c.parentController, event.Warning("Failed to get the Deployment", err))
		}
		return err
	}
	c.sourceDeploy = workload

	err = c.client.Get(ctx, c.targetNamespacedName, &workload)
	if err != nil {
		if !apierrors.IsNotFound(err) {
			c.recorder.Event(c.parentController, event.Warning("Failed to get the Deployment", err))
		}
		return err
	}
	c.targetDeploy = workload
	return nil
}

// add the parent controller to the owner of the Deployment
// before kicking start the update and start from every pod in the old version
func (c *DeploymentController) claimDeployment(ctx context.Context, deploy *apps.Deployment) error {
	deployPatch := client.MergeFrom(deploy.DeepCopyObject())
	ref := metav1.NewControllerRef(c.parentController, v1alpha2.AppRolloutKindVersionKind)
	deploy.SetOwnerReferences(append(deploy.GetOwnerReferences(), *ref))
	deploy.Spec.Paused = false
	// patch the Deployment
	if err := c.client.Patch(ctx, deploy, deployPatch, client.FieldOwner(c.parentController.GetUID())); err != nil {
		c.recorder.Event(c.parentController, event.Warning("Failed to the start the Deployment update", err))
		return err
	}
	return nil
}

func (c *DeploymentController) rolloutBatchFirstHalf(ctx context.Context, rolloutStrategy v1alpha1.RolloutStrategyType) error {
	var err error
	if rolloutStrategy == v1alpha1.IncreaseFirstRolloutStrategyType {
		// set the target replica first which should increase its size
		if err = c.patchDeployment(ctx, c.calculateCurrentTarget(c.rolloutStatus.RolloutTargetSize),
			&c.targetDeploy); err != nil {
			c.rolloutStatus.RolloutRetry(err.Error())
		}
		return nil
	}
	if rolloutStrategy == v1alpha1.DecreaseFirstRolloutStrategyType {
		// set the source replicas first which should shrink its size
		if err = c.patchDeployment(ctx, c.calculateCurrentSource(c.rolloutStatus.RolloutTargetSize),
			&c.sourceDeploy); err != nil {
			c.rolloutStatus.RolloutRetry(err.Error())
		}
		return nil
	}
	return fmt.Errorf("encountered an unknown rolloutStrategy `%s`", rolloutStrategy)
}

func (c *DeploymentController) rolloutBatchSecondHalf(ctx context.Context,
	rolloutStrategy v1alpha1.RolloutStrategyType, targetSize int32) bool {
	var err error
	if rolloutStrategy == v1alpha1.IncreaseFirstRolloutStrategyType {
		// calculate the max unavailable given the target size
		maxUnavail := 0
		currentBatch := c.rolloutSpec.RolloutBatches[c.rolloutStatus.CurrentBatch]
		if currentBatch.MaxUnavailable != nil {
			maxUnavail, _ = intstr.GetValueFromIntOrPercent(currentBatch.MaxUnavailable, int(targetSize), true)
		}
		// make sure that the target deployment has enough ready pods before reducing the source
		if c.targetDeploy.Status.ReadyReplicas+int32(maxUnavail) >= targetSize {
			// set the source replicas now which should shrink its size
			if err = c.patchDeployment(ctx, c.calculateCurrentSource(c.rolloutStatus.RolloutTargetSize),
				&c.sourceDeploy); err != nil {
				c.rolloutStatus.RolloutRetry(err.Error())
				return false
			}
		} else {
			// continue to verify
			klog.InfoS("the batch is not ready yet", "current batch", c.rolloutStatus.CurrentBatch,
				"target ready pod", c.targetDeploy.Status.ReadyReplicas)
			c.rolloutStatus.RolloutRetry(fmt.Sprintf("the batch %d is not ready yet with %d target pods ready",
				c.rolloutStatus.CurrentBatch, c.targetDeploy.Status.ReadyReplicas))
			return false
		}
	} else if rolloutStrategy == v1alpha1.DecreaseFirstRolloutStrategyType {
		// make sure that the source deployment has the correct pods before moving the target
		sourceSize := c.calculateCurrentSource(c.rolloutStatus.RolloutTargetSize)
		if c.sourceDeploy.Status.Replicas == sourceSize {
			// we can increase the target deployment as soon as the source deployment's replica is correct
			// no need to wait for them to be ready
			if err = c.patchDeployment(ctx, targetSize, &c.targetDeploy); err != nil {
				c.rolloutStatus.RolloutRetry(err.Error())
				return false
			}
		} else {
			// continue to verify
			klog.InfoS("the batch is not ready yet", "current batch", c.rolloutStatus.CurrentBatch,
				"source deploy pod", c.sourceDeploy.Status.Replicas)
			c.rolloutStatus.RolloutRetry(fmt.Sprintf("the batch %d is not ready yet with %d source pods",
				c.rolloutStatus.CurrentBatch, c.sourceDeploy.Status.Replicas))
			return false
		}
	}
	return true
}

// the target deploy size for the current batch
func (c *DeploymentController) calculateCurrentTarget(totalSize int32) int32 {
	currentBatch := int(c.rolloutStatus.CurrentBatch)
	var targetSize int32
	if currentBatch == len(c.rolloutSpec.RolloutBatches)-1 {
		targetSize = totalSize
		// special handle the last batch, we ignore the rest of the batch in case there are rounding errors
		klog.InfoS("Use the target size as the  for the last rolling batch",
			"current batch", currentBatch, "batch size", targetSize)
	} else {
		for i, r := range c.rolloutSpec.RolloutBatches {
			batchSize, _ := intstr.GetValueFromIntOrPercent(&r.Replicas, int(totalSize), true)
			if i <= currentBatch {
				targetSize += int32(batchSize)
			} else {
				break
			}
		}
		klog.InfoS("Calculated the number of pods in the target deployment after current batch",
			"current batch", currentBatch, "target deploy size", targetSize)
	}
	return targetSize
}

// the target deploy size for the current batch
func (c *DeploymentController) calculateCurrentSource(totalSize int32) int32 {
	currentBatch := int(c.rolloutStatus.CurrentBatch)
	var sourceSize int32
	for i, r := range c.rolloutSpec.RolloutBatches {
		batchSize, _ := intstr.GetValueFromIntOrPercent(&r.Replicas, int(totalSize), true)
		if i < currentBatch {
			sourceSize += int32(batchSize)
		} else {
			break
		}
	}
	klog.InfoS("Calculated the number of pods in the source deployment after current batch",
		"current batch", currentBatch, "source deploy size", sourceSize)
	return sourceSize
}

// patch the deployment's target, returns if succeeded
func (c *DeploymentController) patchDeployment(ctx context.Context, target int32, deploy *apps.Deployment) error {
	deployPatch := client.MergeFrom(deploy.DeepCopyObject())
	deploy.Spec.Replicas = pointer.Int32Ptr(target)
	// patch the Deployment
	if err := c.client.Patch(ctx, deploy, deployPatch, client.FieldOwner(c.parentController.GetUID())); err != nil {
		c.recorder.Event(c.parentController, event.Warning(event.Reason(fmt.Sprintf(
			"Failed to update the deployment %s to the correct target %d", deploy.GetName(), target)), err))
		return err
	}
	c.recorder.Event(c.parentController, event.Normal("Batch Rollout",
		fmt.Sprintf("Submitted upgrade quest for deployment %s to have %d pods for batch %d",
			deploy.GetName(), target, c.rolloutStatus.CurrentBatch)))
	return nil
}

func (c *DeploymentController) releaseDeployment(ctx context.Context, deploy *apps.Deployment) error {
	deployPatch := client.MergeFrom(deploy.DeepCopyObject())
	// remove the parent controller from the resources' owner list
	var newOwnerList []metav1.OwnerReference
	found := false
	for _, owner := range deploy.GetOwnerReferences() {
		if owner.Kind == v1alpha2.AppRolloutKind && owner.APIVersion == v1alpha2.SchemeGroupVersion.String() {
			found = true
			continue
		}
		newOwnerList = append(newOwnerList, owner)
	}
	if !found {
		klog.V(common.LogDebug).InfoS("the deployment is already released", "deploy", deploy.Name)
		return nil
	}
	deploy.SetOwnerReferences(newOwnerList)
	// patch the Deployment
	if err := c.client.Patch(ctx, deploy, deployPatch, client.FieldOwner(c.parentController.GetUID())); err != nil {
		c.recorder.Event(c.parentController, event.Warning("Failed to the finalize the Deployment", err))
		c.rolloutStatus.RolloutRetry(err.Error())
		return err
	}
	return nil
}
