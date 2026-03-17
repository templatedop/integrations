# Policy Issue → Policy Management Integration Report

**Date:** March 2026
**Scope:** `ims-policyissue-backend-development` integration with `policy-management-development`
**Requirement Source:** `req_Policy_Management_Orchestrator_v4_1.md` — §10.1, GAP-PM-010
**Status:** GAP IDENTIFIED — Action Required

---

## 1. Why This Integration Is Critical

Every policy operation (surrender, loan, revival, claim, commutation, conversion, FLC) is
gated behind the PM `PolicyLifecycleWorkflow` (PLW). **Without a running PLW, PM will reject
all requests for that policy** — no state gate check can run, no financial lock can be acquired.

The PLW is born exactly once per policy, spawned by Policy Issue Service immediately after
policy activation via a Temporal `SignalWithStart`. This is the **birth event** of the
entire PM lifecycle (req §10.1, GAP-PM-010: *"CRITICAL FIX — PLW won't spawn without this"*).

---

## 2. What PM Expects (The Contract)

### 2.1 Mechanism

| Field | Value |
|-------|-------|
| Temporal mechanism | `SignalWithStart` (atomic: workflow creation + signal) |
| Target task queue | `policy-management-tq` |
| Workflow ID | `plw-{policy_number}` (e.g. `plw-PLI/2026/000001`) |
| Workflow type | `PolicyLifecycleWorkflow` |
| Signal channel | `policy-created` |
| Workflow input type | `PolicyLifecycleState` (initial status = `FREE_LOOK_ACTIVE`) |
| Signal payload type | `PolicyCreatedSignal` |
| Idempotency | `SignalWithStart` is inherently idempotent — safe to retry |

### 2.2 Signal Payload (`PolicyCreatedSignal`)

```go
type PolicyCreatedSignal struct {
    RequestID    string         `json:"request_id"`    // UUID idempotency key
    PolicyID     string         `json:"policy_id"`     // PI's UUID (audit cross-ref)
    PolicyNumber string         `json:"policy_number"` // e.g. PLI/2026/000001
    Metadata     PolicyMetadata `json:"metadata"`      // full policy metadata
}
```

### 2.3 `PolicyMetadata` fields required at birth

| Field | Source in PI service |
|-------|---------------------|
| `CustomerID` (int64) | Parse from `input.CustomerID` (string → int64) |
| `ProductCode` | `input.ProductCode` |
| `ProductType` | Derived: `PLI` if `input.PolicyType == PLI*`, else `RPLI` |
| `SumAssured` | `input.SumAssured` |
| `CurrentPremium` | From `premiumResult.TotalPayable` (Step 3 of workflow) |
| `PremiumMode` | `input.PremiumPaymentFrequency` → canonical string |
| `BillingMethod` | From proposal record (`CASH` or `PAY_RECOVERY`) |
| `IssueDate` | `time.Now().UTC()` at issuance |
| `MaturityDate` | Computed in `CreatePolicyIssuanceActivity` as `ProposalDate + PolicyTerm years` |
| `PaidToDate` | First premium date (same as `IssueDate` at activation) |
| `AgentID` | From proposal record (nullable) |
| `NominationStatus` | `"PENDING"` at birth (nominees added later) |
| `IsDistanceMarketing` | Product config flag (30-day FLC vs 15-day) |
| `WorkflowID` | `fmt.Sprintf("plw-%s", policyNumber)` |

### 2.4 When to send

After policy activation is **complete** — specifically after:
- Step 7b: `CreatePolicyIssuanceActivity` (issuance record + maturity date created) ✅
- Step 8: `GenerateBondActivity` (bond generated) ✅
- Step 9: `SendNotificationActivity` ✅

The PM signal is Step 10 — the final step in `PolicyIssuanceWorkflow`.

---

## 3. Current State of Policy Issue Service

### 3.1 What exists

| Component | Status |
|-----------|--------|
| `PolicyIssuanceWorkflow` | Implemented — 9 steps, ends at status `"ISSUED"` |
| `InstantIssuanceWorkflow` | Present (separate flow, same gap) |
| `ProposalActivities` | Implemented: proposal repo + quote repo + product repo injected |
| Temporal worker | Running on task queue `policy-issue-queue` |
| `AadhaarActivities` | Present, separate struct |

### 3.2 What is missing

| Missing item | Where | Impact |
|-------------|-------|--------|
| Step 10: `signalPMToStartLifecycle()` call | `policy_issuance_workflow.go` | PLW never created — all downstream requests blocked |
| `StartPMLifecycleActivity` | New file needed | Activity that calls `client.SignalWithStartWorkflow` |
| `PMLifecycleActivities` struct | New file needed | Holds `temporalClient` for the activity |
| `NewPMLifecycleActivities` constructor | New file needed | Injected via fx |
| `bootstrapper.go` — provide `PMLifecycleActivities` | `bootstrap/bootstrapper.go` | Activity never registered with worker |
| `bootstrapper.go` — register `StartPMLifecycleActivity` | `bootstrap/bootstrapper.go` | Activity not found by worker |
| PM step in `InstantIssuanceWorkflow` | `instant_issuance_workflow.go` | Same gap — instant-issue path also skips PM |
| Config key `temporal.pm_task_queue` | `configs/config.yaml` | Task queue hardcoded instead of configurable |

---

## 4. Gap Analysis — File by File

### 4.1 `workflows/policy_issuance_workflow.go`

**Current last step (line 513–527):**
```go
// Step 9: Send notification
if err := workflow.ExecuteActivity(shortActivityOpts, "SendNotificationActivity", ...).Get(ctx, nil); err != nil {
    // Don't fail the workflow for notification failure
}
result.Status = "ISSUED"
return result, nil   // <— workflow ends here. PM is never signalled.
```

**Required addition — Step 10:**
```go
// Step 10: Signal PM to start PolicyLifecycleWorkflow
if err := workflow.ExecuteActivity(shortActivityOpts, "StartPMLifecycleActivity",
    StartPMLifecycleInput{
        PolicyNumber: policyNumberResult.PolicyNumber,
        InitialState: constructPMInitialState(input, policyNumberResult, premiumResult),
        Signal: PolicyCreatedSignal{
            RequestID:    workflow.GetInfo(ctx).WorkflowExecution.ID,
            PolicyID:     input.ProposalNumber, // PI's own identifier for audit
            PolicyNumber: policyNumberResult.PolicyNumber,
            Metadata:     <same as initial state metadata>,
        },
    }).Get(ctx, nil); err != nil {
    logger.Error("CRITICAL: Failed to signal PM — PLW not started", "error", err)
    result.Status = "PM_SIGNAL_FAILED"
    return result, err  // This IS a hard failure — PM must know about this policy
}
result.Status = "ISSUED"
return result, nil
```

> **Note:** The PM signal failure **must not be swallowed** (unlike notification failure).
> Without a PLW, the policy cannot be operated on at all.

### 4.2 New file: `workflows/activities/pm_lifecycle_activity.go`

```go
package activities

import (
    "context"
    "fmt"
    "go.temporal.io/sdk/client"
    "go.temporal.io/sdk/temporal"
    temporalEnums "go.temporal.io/api/enums/v1"
)

type PMLifecycleActivities struct {
    temporalClient client.Client
    pmTaskQueue    string  // "policy-management-tq" from config
}

func NewPMLifecycleActivities(c client.Client, pmTaskQueue string) *PMLifecycleActivities {
    return &PMLifecycleActivities{temporalClient: c, pmTaskQueue: pmTaskQueue}
}

// StartPMLifecycleInput — all data needed to create PLW via SignalWithStart.
// This struct is constructed by the workflow from its own accumulated state.
type StartPMLifecycleInput struct {
    PolicyNumber string
    InitialState PolicyLifecycleState  // matches PM's type exactly (shared contract pkg or duplicated)
    Signal       PolicyCreatedSignal   // matches PM's type exactly
}

// StartPMLifecycleActivity — atomically creates PM's PolicyLifecycleWorkflow
// and delivers the policy-created signal. Safe to retry (SignalWithStart is idempotent).
func (a *PMLifecycleActivities) StartPMLifecycleActivity(
    ctx context.Context, input StartPMLifecycleInput,
) error {
    workflowID := fmt.Sprintf("plw-%s", input.PolicyNumber)
    workflowOptions := client.StartWorkflowOptions{
        ID:        workflowID,
        TaskQueue: a.pmTaskQueue,
        WorkflowIDReusePolicy: temporalEnums.WORKFLOW_ID_REUSE_POLICY_ALLOW_DUPLICATE,
        RetryPolicy: &temporal.RetryPolicy{MaximumAttempts: 1},
        SearchAttributes: map[string]interface{}{
            "PolicyNumber":  input.PolicyNumber,
            "CurrentStatus": "FREE_LOOK_ACTIVE",
            "ProductType":   input.InitialState.Metadata.ProductType,
            "BillingMethod": input.InitialState.Metadata.BillingMethod,
            "IssueDate":     input.InitialState.Metadata.IssueDate,
        },
    }
    _, err := a.temporalClient.SignalWithStartWorkflow(
        ctx,
        workflowID,
        "policy-created",           // SignalPolicyCreated constant from PM contract
        input.Signal,
        workflowOptions,
        "PolicyLifecycleWorkflow",  // Registered workflow type name in PM
        input.InitialState,
    )
    if err != nil {
        return fmt.Errorf("SignalWithStartWorkflow failed for %s: %w", workflowID, err)
    }
    return nil
}
```

### 4.3 `bootstrap/bootstrapper.go`

**Current `FxTemporal` provides:**
```go
activities.NewProposalActivities,
activities.NewAadhaarActivities,
```

**Required additions:**
```go
// 1. Provide PMLifecycleActivities (needs temporalClient + config)
func(c client.Client, cfg *config.Config) *activities.PMLifecycleActivities {
    pmTQ := cfg.GetString("temporal.pm_task_queue")
    if pmTQ == "" {
        pmTQ = "policy-management-tq"  // fallback
    }
    return activities.NewPMLifecycleActivities(c, pmTQ)
},

// 2. Register activity with worker (inside the fx.Invoke function)
w.RegisterActivity(pmActivities.StartPMLifecycleActivity)
```

### 4.4 `configs/config.yaml`

Add under the `temporal` section:
```yaml
temporal:
  host: "localhost"
  port: "7233"
  namespace: "default"
  pm_task_queue: "policy-management-tq"   # <-- ADD THIS
```

### 4.5 `workflows/instant_issuance_workflow.go`

The instant issuance flow has the same gap. After policy activation in that workflow,
the same `StartPMLifecycleActivity` step must be added as the final step before returning.

---

## 5. Data Construction — `constructPMInitialState()`

This helper must be added to the workflow package. It assembles `PolicyLifecycleState`
from data already in the workflow at the time of Step 10:

```go
func constructPMInitialState(
    input     PolicyIssuanceInput,
    pnResult  GeneratePolicyNumberResult,
    premium   CalculatePremiumResult,
    issuance  CreatePolicyIssuanceResult,   // return this from CreatePolicyIssuanceActivity
) PolicyLifecycleState {
    workflowID := fmt.Sprintf("plw-%s", pnResult.PolicyNumber)
    now := workflow.Now(ctx).UTC()

    return PolicyLifecycleState{
        PolicyNumber:   pnResult.PolicyNumber,
        PolicyID:       input.ProposalNumber,  // PI UUID for audit
        CurrentStatus:  "FREE_LOOK_ACTIVE",
        PreviousStatus: "",
        Encumbrances:   EncumbranceFlags{AssignmentType: "NONE"},
        DisplayStatus:  "FREE_LOOK_ACTIVE",
        Version:        1,
        Metadata: PolicyMetadata{
            CustomerID:    parseCustomerID(input.CustomerID),
            ProductCode:   input.ProductCode,
            ProductType:   deriveProductType(input.PolicyType),  // PLI or RPLI
            SumAssured:    input.SumAssured,
            CurrentPremium: premium.TotalPayable,
            PremiumMode:   string(input.PremiumPaymentFrequency),
            BillingMethod: issuance.BillingMethod,  // needs to be returned by CreatePolicyIssuanceActivity
            IssueDate:     now,
            MaturityDate:  issuance.MaturityDate,   // already computed in Step 7b
            PaidToDate:    now,
            NominationStatus: "PENDING",
            WorkflowID:    workflowID,
        },
        PendingRequests:    []PendingRequest{},
        ProcessedSignalIDs: map[string]time.Time{},
        EventCount:         0,
    }
}
```

**Two small changes needed in `CreatePolicyIssuanceActivity`:**
- Return `MaturityDate` in its result struct (currently returns nothing)
- Include `BillingMethod` in result (needs to be read from proposal during the activity)

---

## 6. Shared Contract — Type Duplication Strategy

PM's `PolicyLifecycleState`, `PolicyCreatedSignal`, and `PolicyMetadata` types are defined
in `policy-management-development/.../workflows/signals.go`.

Policy Issue Service needs to use the **same struct shapes**. Options:

| Option | Pros | Cons |
|--------|------|------|
| **A — Duplicate the types in PI (with `pm_contract.go`)** | Zero cross-service dependency; same pattern as Surrender service | Must stay in sync manually |
| B — Shared Go module / library | Single source of truth | New module maintenance overhead |
| **Recommendation: Option A** | Follows existing surrender pattern; fastest | —

Create `workflows/pm_contract.go` in PI service (same approach used in surrender service
`temporal/workflows/pm_contract.go`). Copy just the 4 types needed:
`PolicyLifecycleState`, `PolicyMetadata`, `EncumbranceFlags`, `PolicyCreatedSignal`.

---

## 7. Implementation Checklist

| # | Task | File | Owner |
|---|------|------|-------|
| 1 | Create `pm_contract.go` with copied PM types | `workflows/pm_contract.go` | PI team |
| 2 | Create `pm_lifecycle_activity.go` with `StartPMLifecycleActivity` | `workflows/activities/pm_lifecycle_activity.go` | PI team |
| 3 | Update `CreatePolicyIssuanceActivity` to return `MaturityDate` + `BillingMethod` | `workflows/activities/proposal_activities.go` | PI team |
| 4 | Add `constructPMInitialState()` helper | `workflows/policy_issuance_workflow.go` | PI team |
| 5 | Add Step 10 (`StartPMLifecycleActivity`) to `PolicyIssuanceWorkflow` | `workflows/policy_issuance_workflow.go` | PI team |
| 6 | Add same Step 10 to `InstantIssuanceWorkflow` | `workflows/instant_issuance_workflow.go` | PI team |
| 7 | Provide `PMLifecycleActivities` via fx in bootstrapper | `bootstrap/bootstrapper.go` | PI team |
| 8 | Register `StartPMLifecycleActivity` with Temporal worker | `bootstrap/bootstrapper.go` | PI team |
| 9 | Add `temporal.pm_task_queue` to all config files | `configs/config.yaml` + others | PI team |
| 10 | Verify PM `PolicyLifecycleWorkflow` handler for `policy-created` (ready) | PM service | PM team (done) |

---

## 8. PM Service Readiness Confirmation

The PM service (`policy-management-tq`) is **ready to receive the signal**:

| Check | Status |
|-------|--------|
| `PolicyLifecycleWorkflow` registered on `policy-management-tq` | ✅ `bootstrapper.go:145` |
| `SignalPolicyCreated = "policy-created"` constant defined | ✅ `workflows/signals.go:21` |
| `PolicyCreatedSignal` struct defined | ✅ `workflows/signals.go:179` |
| `handlePolicyCreated()` signal handler implemented | ✅ `workflows/policy_lifecycle_workflow.go:816` |
| FLC timer goroutine spawned on receipt | ✅ Per req §10.1.6 |
| `RecordStateTransitionActivity` called on receipt | ✅ Persists `FREE_LOOK_ACTIVE` to DB |
| Duplicate signal detection (dedup by `RequestID`) | ✅ `isDuplicate()` check |

**Nothing to change in PM service for this integration.**

---

## 9. Sequence Diagram — After Integration

```
PolicyIssuanceWorkflow
├── Step 1:  ValidateProposalActivity
├── Step 2:  CheckEligibilityActivity
├── Step 3:  CalculatePremiumActivity
├── Step 3b: SavePremiumToProposalActivity
├── Step 4:  [QC Signal loop]
├── Step 5:  RequestMedicalReviewActivity + [Medical Signal]
├── Step 6:  RouteToApproverActivity + [Approver Signal]
├── Step 7:  GeneratePolicyNumberActivity
├── Step 7b: CreatePolicyIssuanceActivity  ← now returns MaturityDate + BillingMethod
├── Step 8:  GenerateBondActivity + UpdateBondDetailsActivity
├── Step 9:  SendNotificationActivity
└── Step 10: StartPMLifecycleActivity          ← NEW
              │
              │ client.SignalWithStartWorkflow(
              │   "plw-PLI/2026/000001",      WorkflowID
              │   "policy-created",            Signal channel
              │   PolicyCreatedSignal{...},    Signal payload
              │   "policy-management-tq",      Task queue
              │   PolicyLifecycleWorkflow,     Workflow type
              │   PolicyLifecycleState{        Workflow input
              │     CurrentStatus: "FREE_LOOK_ACTIVE",
              │     ...
              │   }
              │ )
              ▼
    PolicyLifecycleWorkflow (PM — plw-PLI/2026/000001)
              │
              ├── Receives "policy-created" signal
              ├── Records FREE_LOOK_ACTIVE → DB
              ├── Starts FLC timer (15d or 30d)
              │   └── On expiry → transitions to ACTIVE
              └── Enters signal-select loop
                  (all future policy requests now routable)
```

---

## 10. Risk If Not Implemented

| Risk | Impact |
|------|--------|
| No PLW running for issued policy | PM rejects ALL requests (surrender, loan, revival, claim) with "workflow not found" |
| No `FREE_LOOK_ACTIVE` state in PM | FLC handler (`flc-request` signal) has no target workflow — FLC cannot be processed |
| No FLC timer | Policy never automatically transitions to `ACTIVE` — stuck in `FREE_LOOK_ACTIVE` forever |
| No initial state persisted | PM cannot answer state-gate queries for CPC / portal |

**This is a blocking dependency. No downstream operation on any issued policy will work
until this integration is implemented.**
