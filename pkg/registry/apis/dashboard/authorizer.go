package dashboard

import (
	"context"

	"k8s.io/apiserver/pkg/authorization/authorizer"

	"github.com/grafana/authlib/types"
	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/guardian"
	"github.com/grafana/grafana/pkg/storage/legacysql/dualwrite"
)

func GetAuthorizer(dashboardService dashboards.DashboardService, dualWriter dualwrite.Service, l log.Logger) authorizer.Authorizer {
	return authorizer.AuthorizerFunc(
		func(ctx context.Context, attr authorizer.Attributes) (authorized authorizer.Decision, reason string, err error) {
			// Note that we will return Allow more than expected.
			// This is because we do NOT want to hit the RoleAuthorizer that would be evaluated afterwards.

			// Check if we're reading from legacy dashboards and folders
			isReadingLegacy := dualwrite.IsReadingLegacyDashboardsAndFolders(ctx, dualWriter)
			if !isReadingLegacy {
				return authorizer.DecisionAllow, "relying on unified storage for access control", nil
			}

			// This authorizer is only used for mode 0 to 2. Mode 3 onwards, unified storage handles access control.

			// Use the standard authorizer
			if !attr.IsResourceRequest() {
				// TODO: When is this used?
				return authorizer.DecisionNoOpinion, "", nil
			}

			user, err := identity.GetRequester(ctx)
			if err != nil {
				return authorizer.DecisionDeny, "", err
			}

			if attr.GetVerb() == "create" {
				// Permissions will be handled downstream
				return authorizer.DecisionAllow, "", nil
			}

			// Allow search and list requests
			if attr.GetResource() == "search" || attr.GetName() == "" {
				return authorizer.DecisionNoOpinion, "", nil
			}

			ns := attr.GetNamespace()
			if ns == "" {
				return authorizer.DecisionDeny, "expected namespace", nil
			}

			info, err := types.ParseNamespace(attr.GetNamespace())
			if err != nil {
				return authorizer.DecisionDeny, "error reading org from namespace", err
			}

			// expensive path to lookup permissions for a single dashboard
			dto, err := dashboardService.GetDashboard(ctx, &dashboards.GetDashboardQuery{
				UID:   attr.GetName(),
				OrgID: info.OrgID,
			})
			if err != nil {
				return authorizer.DecisionDeny, "error loading dashboard", err
			}

			ok := false
			guardian, err := guardian.NewByDashboard(ctx, dto, info.OrgID, user)
			if err != nil {
				return authorizer.DecisionDeny, "", err
			}

			switch attr.GetVerb() {
			case "get":
				ok, err = guardian.CanView()
				if !ok || err != nil {
					return authorizer.DecisionDeny, "can not view dashboard", err
				}
			case "update":
				ok, err = guardian.CanEdit() // vs Save
				if !ok || err != nil {
					return authorizer.DecisionDeny, "can not edit dashboard", err
				}
			case "delete":
				ok, err = guardian.CanDelete()
				if !ok || err != nil {
					return authorizer.DecisionDeny, "can not delete dashboard", err
				}
			default:
				l.Info("unknown verb", "verb", attr.GetVerb())
				return authorizer.DecisionNoOpinion, "unsupported verb", nil // Unknown verb
			}
			return authorizer.DecisionAllow, "", nil
		})
}
