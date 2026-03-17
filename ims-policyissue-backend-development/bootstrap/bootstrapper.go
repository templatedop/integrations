package bootstrap

import (
	"context"
	handler "policy-issue-service/handler"
	repo "policy-issue-service/repo/postgres"
	"policy-issue-service/workflows"
	"policy-issue-service/workflows/activities"

	config "gitlab.cept.gov.in/it-2.0-common/api-config"
	log "gitlab.cept.gov.in/it-2.0-common/n-api-log"
	serverHandler "gitlab.cept.gov.in/it-2.0-common/n-api-server/handler"
	"go.uber.org/fx"

	"go.temporal.io/sdk/client"
	"go.temporal.io/sdk/worker"
)

// FxRepo module provides all repository implementations
var FxRepo = fx.Module(
	"Repomodule",
	fx.Provide(
		repo.NewQuoteRepository,
		repo.NewProductRepository,
		repo.NewProposalRepository,
		repo.NewAadhaarRepository,
		repo.NewDocumentRepository,
		repo.NewBulkUploadRepository,
	),
)

// FxHandler module provides all HTTP handlers
var FxHandler = fx.Module(
	"Handlermodule",
	fx.Provide(
		fx.Annotate(
			handler.NewQuoteHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewProposalHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewAadhaarHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewApprovalHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewPolicyHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewLookupHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewValidationHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewCalculationHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewDocumentHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewStatusHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewWorkflowHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewBulkUploadHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
		fx.Annotate(
			handler.NewCustomerHandler,
			fx.As(new(serverHandler.Handler)),
			fx.ResultTags(serverHandler.ServerControllersGroupTag),
		),
	),
)

// FxTemporal module provides Temporal client and worker
var FxTemporal = fx.Module(
	"Temporalmodule",
	fx.Provide(
		// Provide Temporal client

		func(cfg *config.Config, ctx context.Context) (client.Client, error) {

			temporalHost := cfg.GetString("temporal.host")
			temporalPort := cfg.GetString("temporal.port")
			temporalNamespace := cfg.GetString("temporal.namespace")
			log.Info(ctx, "Connecting temporal at:", temporalHost+":"+temporalPort)
			return client.Dial(client.Options{
				HostPort:  temporalHost + ":" + temporalPort,
				Namespace: temporalNamespace,
			})
		},
		// Provide activity structs
		activities.NewProposalActivities,
		activities.NewAadhaarActivities,
	),
	fx.Invoke(
		// Register workflows and activities with worker
		func(c client.Client, proposalActivities *activities.ProposalActivities, aadhaarActivities *activities.AadhaarActivities) error {
			w := worker.New(c, "policy-issue-queue", worker.Options{})

			// Register workflows
			w.RegisterWorkflow(workflows.PolicyIssuanceWorkflow)
			w.RegisterWorkflow(workflows.InstantIssuanceWorkflow)

			// Register activities
			w.RegisterActivity(proposalActivities.ValidateProposalActivity)
			w.RegisterActivity(proposalActivities.CheckEligibilityActivity)
			w.RegisterActivity(proposalActivities.CalculatePremiumActivity)
			w.RegisterActivity(proposalActivities.SavePremiumToProposalActivity)
			w.RegisterActivity(proposalActivities.UpdateProposalStatusActivity)
			w.RegisterActivity(proposalActivities.SendNotificationActivity)
			w.RegisterActivity(proposalActivities.RequestMedicalReviewActivity)
			w.RegisterActivity(proposalActivities.RouteToApproverActivity)
			w.RegisterActivity(proposalActivities.GeneratePolicyNumberActivity)
			w.RegisterActivity(proposalActivities.GenerateBondActivity)
			w.RegisterActivity(proposalActivities.CreatePolicyIssuanceActivity)
			w.RegisterActivity(proposalActivities.UpdateBondDetailsActivity)
			w.RegisterActivity(aadhaarActivities.ValidateAndCalculatePremiumActivity)
			w.RegisterActivity(aadhaarActivities.CreateAadhaarProposalActivity)
			w.RegisterActivity(aadhaarActivities.CheckInstantIssuanceEligibilityActivity)
			w.RegisterActivity(aadhaarActivities.SendPolicyBondElectronicActivity)

			return w.Start()
		},
	),
)
