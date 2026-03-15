package workflows

import (
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"gitlab.cept.gov.in/it-2.0-policy/surrender-service/temporal/activities"
)

// VoluntarySurrenderWorkflowInput defines the input for voluntary surrender workflow
// TEMP-001: Voluntary Surrender Processing Workflow
type VoluntarySurrenderWorkflowInput struct {
	SurrenderRequestID string
	PolicyID           string
	RequestNumber      string
	RequestedBy        string
}

// VoluntarySurrenderWorkflowResult defines the result of voluntary surrender workflow
type VoluntarySurrenderWorkflowResult struct {
	SurrenderRequestID string
	Status             string
	CompletedAt        time.Time
	PolicyStatus       string
	PaymentReference   string
	Error              string
}

// VoluntarySurrenderWorkflow orchestrates the complete voluntary surrender process
// TEMP-001: Voluntary Surrender Processing Workflow
// Business Flow:
// 1. Validate eligibility
// 2. Wait for Data Entry signal → Execute SubmitDEActivity
// 3. Wait for Quality Check signal → Execute SubmitQCActivity
// 4. Wait for Approval signal → Execute SubmitApprovalActivity
// 5. Calculate surrender value
// 6. Process payment
// 7. Update policy status
func VoluntarySurrenderWorkflow(ctx workflow.Context, input VoluntarySurrenderWorkflowInput) (*VoluntarySurrenderWorkflowResult, error) {
	logger := workflow.GetLogger(ctx)
	logger.Info("Starting Voluntary Surrender Workflow", "SurrenderRequestID", input.SurrenderRequestID)

	result := &VoluntarySurrenderWorkflowResult{
		SurrenderRequestID: input.SurrenderRequestID,
		CompletedAt:        workflow.Now(ctx),
	}

	// Configure activity options
	activityOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 5 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    3,
			InitialInterval:    1 * time.Second,
			BackoffCoefficient: 2.0,
		},
	}
	ctx = workflow.WithActivityOptions(ctx, activityOptions)

	// Step 1: Validate Eligibility
	logger.Info("Step 1: Validating eligibility")
	var eligibilityResult activities.ValidateEligibilityResult
	err := workflow.ExecuteActivity(ctx, activities.ValidateEligibilityActivity, activities.ValidateEligibilityInput{
		PolicyID: input.PolicyID,
	}).Get(ctx, &eligibilityResult)

	if err != nil {
		logger.Error("Failed to validate eligibility", "error", err)
		result.Status = "FAILED_ELIGIBILITY"
		result.Error = err.Error()
		return result, err
	}

	if !eligibilityResult.Eligible {
		logger.Info("Policy not eligible for surrender", "reasons", eligibilityResult.Reasons)
		result.Status = "NOT_ELIGIBLE"
		result.Error = fmt.Sprintf("Policy not eligible: %v", eligibilityResult.Reasons)
		return result, fmt.Errorf("policy not eligible for surrender")
	}

	// Step 2: Wait for Data Entry Completion (Signal-based)
	logger.Info("Step 2: Waiting for Data Entry completion")
	var deInput activities.SubmitDEInput
	selector := workflow.NewSelector(ctx)

	deChannel := workflow.GetSignalChannel(ctx, "de-completed")
	selector.AddReceive(deChannel, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, &deInput)
		logger.Info("Received Data Entry completion signal")
	})

	// Timeout after 7 days
	deTimeout := workflow.NewTimer(ctx, 7*24*time.Hour)
	selector.AddFuture(deTimeout, func(f workflow.Future) {
		logger.Warn("Data Entry timeout reached")
	})

	selector.Select(ctx)

	// Check if signal was received (empty struct check)
	if deInput.SurrenderRequestID == "" {
		logger.Error("Data Entry timeout")
		result.Status = "TIMEOUT_DE"
		result.Error = "Data Entry timeout (7 days)"
		return result, fmt.Errorf("data entry timeout")
	}

	// Execute SubmitDEActivity
	logger.Info("Executing SubmitDEActivity")
	var deResult activities.SubmitDEResult
	err = workflow.ExecuteActivity(ctx, activities.SubmitDEActivity, deInput).Get(ctx, &deResult)
	if err != nil {
		logger.Error("Failed to execute SubmitDEActivity", "error", err)
		result.Status = "FAILED_DE"
		result.Error = err.Error()
		return result, err
	}

	if !deResult.Success {
		logger.Error("SubmitDEActivity failed", "message", deResult.Message)
		result.Status = "FAILED_DE"
		result.Error = deResult.Message
		return result, fmt.Errorf("submit DE failed: %s", deResult.Message)
	}

	logger.Info("DE completed successfully", "message", deResult.Message)

	// Step 3: Wait for Quality Check Completion (Signal-based)
	logger.Info("Step 3: Waiting for Quality Check completion")
	var qcInput activities.SubmitQCInput
	selector2 := workflow.NewSelector(ctx)

	qcChannel := workflow.GetSignalChannel(ctx, "qc-completed")
	selector2.AddReceive(qcChannel, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, &qcInput)
		logger.Info("Received Quality Check completion signal")
	})

	// Timeout after 7 days
	qcTimeout := workflow.NewTimer(ctx, 7*24*time.Hour)
	selector2.AddFuture(qcTimeout, func(f workflow.Future) {
		logger.Warn("Quality Check timeout reached")
	})

	selector2.Select(ctx)

	// Check if signal was received (empty struct check)
	if qcInput.SurrenderRequestID == "" {
		logger.Error("Quality Check timeout")
		result.Status = "TIMEOUT_QC"
		result.Error = "Quality Check timeout (7 days)"
		return result, fmt.Errorf("quality check timeout")
	}

	// Execute SubmitQCActivity
	logger.Info("Executing SubmitQCActivity")
	var qcResult activities.SubmitQCResult
	err = workflow.ExecuteActivity(ctx, activities.SubmitQCActivity, qcInput).Get(ctx, &qcResult)
	if err != nil {
		logger.Error("Failed to execute SubmitQCActivity", "error", err)
		result.Status = "FAILED_QC"
		result.Error = err.Error()
		return result, err
	}

	if !qcResult.Success {
		logger.Error("SubmitQCActivity failed", "message", qcResult.Message)
		result.Status = "FAILED_QC"
		result.Error = qcResult.Message
		return result, fmt.Errorf("submit QC failed: %s", qcResult.Message)
	}

	logger.Info("QC completed successfully", "message", qcResult.Message)

	// Step 4: Wait for Approval Completion (Signal-based)
	logger.Info("Step 4: Waiting for Approval completion")
	var approvalInput activities.SubmitApprovalInput
	selector3 := workflow.NewSelector(ctx)

	approvalChannel := workflow.GetSignalChannel(ctx, "approval-completed")
	selector3.AddReceive(approvalChannel, func(c workflow.ReceiveChannel, more bool) {
		c.Receive(ctx, &approvalInput)
		logger.Info("Received Approval completion signal")
	})

	// Timeout after 30 days
	approvalTimeout := workflow.NewTimer(ctx, 30*24*time.Hour)
	selector3.AddFuture(approvalTimeout, func(f workflow.Future) {
		logger.Warn("Approval timeout reached")
	})

	selector3.Select(ctx)

	// Check if signal was received (empty struct check)
	if approvalInput.SurrenderRequestID == "" {
		logger.Error("Approval timeout")
		result.Status = "TIMEOUT_APPROVAL"
		result.Error = "Approval timeout (30 days)"
		return result, fmt.Errorf("approval timeout")
	}

	// Execute SubmitApprovalActivity
	logger.Info("Executing SubmitApprovalActivity")
	var approvalResult activities.SubmitApprovalResult
	err = workflow.ExecuteActivity(ctx, activities.SubmitApprovalActivity, approvalInput).Get(ctx, &approvalResult)
	if err != nil {
		logger.Error("Failed to execute SubmitApprovalActivity", "error", err)
		result.Status = "FAILED_APPROVAL"
		result.Error = err.Error()
		return result, err
	}

	if !approvalResult.Success {
		logger.Error("SubmitApprovalActivity failed", "message", approvalResult.Message)
		result.Status = "FAILED_APPROVAL"
		result.Error = approvalResult.Message
		return result, fmt.Errorf("submit approval failed: %s", approvalResult.Message)
	}

	logger.Info("Approval completed successfully", "message", approvalResult.Message, "status", approvalResult.Status)

	// Check approval status
	if approvalResult.Status != "APPROVED" {
		logger.Info("Surrender request rejected", "status", approvalResult.Status)
		result.Status = "REJECTED"
		result.Error = fmt.Sprintf("Request was rejected with status: %s", approvalResult.Status)
		return result, nil
	}

	// Step 5: Calculate Surrender Value
	logger.Info("Step 5: Calculating surrender value")
	var calculationResult activities.CalculateSurrenderValueResult
	err = workflow.ExecuteActivity(ctx, activities.CalculateSurrenderValueActivity, activities.CalculateSurrenderValueInput{
		SurrenderRequestID: input.SurrenderRequestID,
		PolicyID:           input.PolicyID,
	}).Get(ctx, &calculationResult)

	if err != nil {
		logger.Error("Failed to calculate surrender value", "error", err)
		result.Status = "FAILED_CALCULATION"
		result.Error = err.Error()
		return result, err
	}

	logger.Info("Surrender value calculated", "NSV", calculationResult.NetSurrenderValue)

	// Step 6: Process Payment
	logger.Info("Step 6: Processing payment")
	var paymentResult activities.ProcessPaymentResult

	// Use longer timeout for payment processing
	paymentOptions := workflow.ActivityOptions{
		StartToCloseTimeout: 10 * time.Minute,
		RetryPolicy: &temporal.RetryPolicy{
			MaximumAttempts:    5,
			InitialInterval:    2 * time.Second,
			BackoffCoefficient: 2.0,
		},
	}
	paymentCtx := workflow.WithActivityOptions(ctx, paymentOptions)

	err = workflow.ExecuteActivity(paymentCtx, activities.ProcessPaymentActivity, activities.ProcessPaymentInput{
		SurrenderRequestID: input.SurrenderRequestID,
		Amount:             calculationResult.NetSurrenderValue,
	}).Get(paymentCtx, &paymentResult)

	if err != nil {
		logger.Error("Failed to process payment", "error", err)
		result.Status = "FAILED_PAYMENT"
		result.Error = err.Error()
		return result, err
	}

	result.PaymentReference = paymentResult.PaymentReference

	// Step 7: Update Policy Status
	logger.Info("Step 7: Updating policy status")
	var policyUpdateResult activities.UpdatePolicyStatusResult
	err = workflow.ExecuteActivity(ctx, activities.UpdatePolicyStatusActivity, activities.UpdatePolicyStatusInput{
		PolicyID:           input.PolicyID,
		SurrenderRequestID: input.SurrenderRequestID,
		NewStatus:          calculationResult.PredictedDisposition,
	}).Get(ctx, &policyUpdateResult)

	if err != nil {
		logger.Error("Failed to update policy status", "error", err)
		// Don't fail workflow - payment already processed
		result.Status = "COMPLETED_POLICY_UPDATE_FAILED"
		result.Error = err.Error()
		return result, nil
	}

	result.PolicyStatus = policyUpdateResult.NewStatus

	// Workflow completed successfully
	logger.Info("Voluntary Surrender Workflow completed successfully")
	result.Status = "COMPLETED"
	result.CompletedAt = workflow.Now(ctx)

	return result, nil
}
