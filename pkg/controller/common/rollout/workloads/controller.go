package workloads

import (
	"context"
)

// WorkloadController is the interface that all type of cloneSet controller implements
type WorkloadController interface {
	// VerifySpec makes sure that the resources can be upgraded according to the rollout plan
	// it returns if the verification succeeded/failed or should retry
	VerifySpec(ctx context.Context) (bool, error)

	// Initialize make sure that the resource is ready to be upgraded
	// this function is tasked to do any initialization work on the resources
	// it returns if the initialization succeeded/failed or should retry
	Initialize(ctx context.Context) (bool, error)

	// RolloutOneBatchPods tries to upgrade pods in the resources following the rollout plan
	// it will upgrade pods as the rollout plan allows at once
	// it returns if the upgrade actionable items succeeded/failed or should continue
	RolloutOneBatchPods(ctx context.Context) (bool, error)

	// CheckOneBatchPods checks how many pods are ready to serve requests in the current batch
	// it returns whether the number of pods upgraded in this round satisfies the rollout plan
	CheckOneBatchPods(ctx context.Context) (bool, error)

	// FinalizeOneBatch makes sure that the rollout can start the next batch
	// it returns if the finalization of this batch succeeded/failed or should retry
	FinalizeOneBatch(ctx context.Context) (bool, error)

	// Finalize makes sure the resources are in a good final state.
	// It might depend on if the rollout succeeded or not.
	// For example, we may remove the source object to prevent scalar traits to ever work
	// and the finalize rollout web hooks will be called after this call succeeds
	Finalize(ctx context.Context, succeed bool) bool
}
