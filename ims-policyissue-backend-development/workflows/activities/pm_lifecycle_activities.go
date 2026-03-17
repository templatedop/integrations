package activities

import (
	"context"
	"fmt"
	"time"

	"policy-issue-service/repo/postgres"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/temporal"
)

// PMLifecycleActivities contains activities for signalling the Policy Manager (PM) service.
type PMLifecycleActivities struct {
	proposalRepo   *postgres.ProposalRepository
	temporalClient client.Client
	// pmTaskQueue is the Temporal task queue that PM's lifecycle worker listens on.
	pmTaskQueue string
}

// NewPMLifecycleActivities creates a PMLifecycleActivities instance.
func NewPMLifecycleActivities(
	proposalRepo *postgres.ProposalRepository,
	temporalClient client.Client,
	pmTaskQueue string,
) *PMLifecycleActivities {
	return &PMLifecycleActivities{
		proposalRepo:   proposalRepo,
		temporalClient: temporalClient,
		pmTaskQueue:    pmTaskQueue,
	}
}

// StartPMLifecycleInput is the input for StartPMLifecycleActivity.
type StartPMLifecycleInput struct {
	// PolicyNumber uniquely identifies the issued policy (e.g. "PLI/2026/GJ/000001").
	PolicyNumber string `json:"policy_number"`
	// PolicyType is "PLI" or "RPLI", used to build the PM workflow ID.
	PolicyType string `json:"policy_type"`
}

// PMCreatedSignal is the signal payload sent to PM's lifecycle workflow.
type PMCreatedSignal struct {
	PolicyNumber string    `json:"policy_number"`
	PolicyType   string    `json:"policy_type"`
	IssuedAt     time.Time `json:"issued_at"`
}

// StartPMLifecycleActivity sends a SignalWithStart to the PM service for the given policy.
//
// Behaviour:
//   - Increments pm_signal_attempts in proposal_issuance before the call.
//   - On success: writes pm_signal_status = 'SENT' and pm_plw_workflow_id.
//   - On failure: writes pm_signal_status = 'FAILED' and pm_signal_last_error,
//     then returns the error so Temporal will retry this activity according to
//     the caller's RetryPolicy.
//
// Idempotency: SignalWithStart is idempotent by workflowID — PM will ignore a
// duplicate start if the workflow is already running.
func (a *PMLifecycleActivities) StartPMLifecycleActivity(ctx context.Context, input StartPMLifecycleInput) error {
	if input.PolicyNumber == "" {
		return temporal.NewNonRetryableApplicationError("policy_number is required", "INVALID_INPUT", nil)
	}

	workflowID := fmt.Sprintf("plw-%s", input.PolicyNumber)

	// Record that we are about to attempt the signal (best-effort).
	// MarkPMSignalFailed increments the counter; we call it here with an empty
	// error to bump the attempt count before the actual call.
	_ = a.proposalRepo.MarkPMSignalFailed(ctx, input.PolicyNumber, "attempt in progress")

	signal := PMCreatedSignal{
		PolicyNumber: input.PolicyNumber,
		PolicyType:   input.PolicyType,
		IssuedAt:     time.Now().UTC(),
	}

	startOpts := client.StartWorkflowOptions{
		ID:        workflowID,
		TaskQueue: a.pmTaskQueue,
	}

	_, err := a.temporalClient.SignalWithStartWorkflow(
		ctx,
		workflowID,
		"policy-created", // signal name PM's lifecycle workflow listens on
		signal,
		startOpts,
		"PolicyLifecycleWorkflow", // PM's workflow function name (registered on PM worker)
		signal,                    // initial input if the workflow isn't running yet
	)
	if err != nil {
		// Persist failure so the reconciliation worker can find it.
		_ = a.proposalRepo.MarkPMSignalFailed(ctx, input.PolicyNumber, err.Error())
		return fmt.Errorf("SignalWithStart for %s failed: %w", workflowID, err)
	}

	// Persist success — reconciliation worker will skip this row.
	if dbErr := a.proposalRepo.MarkPMSignalSent(ctx, input.PolicyNumber, workflowID); dbErr != nil {
		// The signal reached PM; a DB write failure here is non-critical.
		// Log and continue — the reconciliation worker will see status still
		// PENDING/FAILED and re-attempt SignalWithStart, which is idempotent.
		return fmt.Errorf("signal sent but failed to persist SENT status for %s: %w", input.PolicyNumber, dbErr)
	}

	return nil
}

// FindUnsignalledPoliciesInput is the input for FindUnsignalledPoliciesActivity.
type FindUnsignalledPoliciesInput struct {
	// GracePeriodMinutes is how old a PENDING record must be before reconciliation
	// considers it stuck (prevents interfering with in-progress workflows).
	GracePeriodMinutes int `json:"grace_period_minutes"`
	// MaxAttempts caps how many times a policy will be retried before giving up.
	MaxAttempts int `json:"max_attempts"`
}

// FindUnsignalledPoliciesActivity queries the PI DB for policies that need
// PM signal retry and returns them as StartPMLifecycleInput slices.
func (a *PMLifecycleActivities) FindUnsignalledPoliciesActivity(
	ctx context.Context,
	input FindUnsignalledPoliciesInput,
) ([]StartPMLifecycleInput, error) {
	gracePeriod := time.Duration(input.GracePeriodMinutes) * time.Minute
	maxAttempts := input.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 20
	}
	if gracePeriod <= 0 {
		gracePeriod = 30 * time.Minute
	}

	targets, err := a.proposalRepo.FindUnsignalledPolicies(ctx, gracePeriod, maxAttempts)
	if err != nil {
		return nil, fmt.Errorf("FindUnsignalledPoliciesActivity: %w", err)
	}

	result := make([]StartPMLifecycleInput, 0, len(targets))
	for _, t := range targets {
		result = append(result, StartPMLifecycleInput{
			PolicyNumber: t.PolicyNumber,
			PolicyType:   t.PolicyType,
		})
	}
	return result, nil
}
