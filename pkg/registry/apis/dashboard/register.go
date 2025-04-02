package dashboard

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apiserver/pkg/admission"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	"k8s.io/apiserver/pkg/registry/rest"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/kube-openapi/pkg/common"
	"k8s.io/kube-openapi/pkg/spec3"
	"k8s.io/kube-openapi/pkg/validation/spec"

	"github.com/grafana/authlib/types"
	internal "github.com/grafana/grafana/apps/dashboard/pkg/apis/dashboard"
	"github.com/grafana/grafana/apps/dashboard/pkg/apis/dashboard/v0alpha1"
	"github.com/grafana/grafana/apps/dashboard/pkg/apis/dashboard/v1alpha1"
	"github.com/grafana/grafana/apps/dashboard/pkg/apis/dashboard/v2alpha1"
	"github.com/grafana/grafana/apps/dashboard/pkg/migration/conversion"
	"github.com/grafana/grafana/pkg/apimachinery/identity"
	"github.com/grafana/grafana/pkg/apimachinery/utils"
	grafanaregistry "github.com/grafana/grafana/pkg/apiserver/registry/generic"
	"github.com/grafana/grafana/pkg/infra/db"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/infra/tracing"
	"github.com/grafana/grafana/pkg/registry/apis/dashboard/legacy"
	"github.com/grafana/grafana/pkg/registry/apis/dashboard/legacysearcher"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/apiserver/builder"
	"github.com/grafana/grafana/pkg/services/apiserver/endpoints/request"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/provisioning"
	"github.com/grafana/grafana/pkg/services/quota"
	"github.com/grafana/grafana/pkg/services/search/sort"
	"github.com/grafana/grafana/pkg/services/user"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/storage/legacysql"
	"github.com/grafana/grafana/pkg/storage/legacysql/dualwrite"
	"github.com/grafana/grafana/pkg/storage/unified/apistore"
	"github.com/grafana/grafana/pkg/storage/unified/resource"

	folderv0alpha1 "github.com/grafana/grafana/pkg/apis/folder/v0alpha1"
	"github.com/grafana/grafana/pkg/services/apiserver"
	"github.com/grafana/grafana/pkg/services/apiserver/client"
)

var (
	_ builder.APIGroupBuilder          = (*DashboardsAPIBuilder)(nil)
	_ builder.APIGroupVersionsProvider = (*DashboardsAPIBuilder)(nil)
	_ builder.OpenAPIPostProcessor     = (*DashboardsAPIBuilder)(nil)
	_ builder.APIGroupRouteProvider    = (*DashboardsAPIBuilder)(nil)
)

const (
	DASHBOARD_SPEC_TITLE            = "title"
	DASHBOARD_SPEC_REFRESH_INTERVAL = "refresh"
)

// This is used just so wire has something unique to return
type DashboardsAPIBuilder struct {
	dashboardService dashboards.DashboardService
	features         featuremgmt.FeatureToggles

	accessControl                accesscontrol.AccessControl
	legacy                       *DashboardStorage
	unified                      resource.ResourceClient
	dashboardProvisioningService dashboards.DashboardProvisioningService
	scheme                       *runtime.Scheme
	search                       *SearchHandler
	dashStore                    dashboards.Store
	folderStore                  folder.FolderStore
	QuotaService                 quota.Service
	ProvisioningService          provisioning.ProvisioningService
	cfg                          *setting.Cfg
	accessClient                 types.AccessClient
	dualWriter                   dualwrite.Service
	folderClient                 client.K8sHandler

	log log.Logger
	reg prometheus.Registerer
}

func RegisterAPIService(
	cfg *setting.Cfg,
	features featuremgmt.FeatureToggles,
	apiregistration builder.APIRegistrar,
	dashboardService dashboards.DashboardService,
	provisioningDashboardService dashboards.DashboardProvisioningService,
	accessControl accesscontrol.AccessControl,
	provisioning provisioning.ProvisioningService,
	dashStore dashboards.Store,
	reg prometheus.Registerer,
	sql db.DB,
	tracing *tracing.TracingService,
	unified resource.ResourceClient,
	dual dualwrite.Service,
	sorter sort.Service,
	quotaService quota.Service,
	folderStore folder.FolderStore,
	accessClient types.AccessClient,
	restConfigProvider apiserver.RestConfigProvider,
	userService user.Service,
) *DashboardsAPIBuilder {
	softDelete := features.IsEnabledGlobally(featuremgmt.FlagDashboardRestore)
	dbp := legacysql.NewDatabaseProvider(sql)
	namespacer := request.GetNamespaceMapper(cfg)
	legacyDashboardSearcher := legacysearcher.NewDashboardSearchClient(dashStore, sorter)
	folderClient := client.NewK8sHandler(dual, request.GetNamespaceMapper(cfg), folderv0alpha1.FolderResourceInfo.GroupVersionResource(), restConfigProvider.GetRestConfig, dashStore, userService, unified, sorter)
	builder := &DashboardsAPIBuilder{
		log: log.New("grafana-apiserver.dashboards"),

		dashboardService:             dashboardService,
		features:                     features,
		accessControl:                accessControl,
		unified:                      unified,
		dashboardProvisioningService: provisioningDashboardService,
		search:                       NewSearchHandler(tracing, dual, legacyDashboardSearcher, unified, features),
		dashStore:                    dashStore,
		folderStore:                  folderStore,
		QuotaService:                 quotaService,
		ProvisioningService:          provisioning,
		cfg:                          cfg,
		accessClient:                 accessClient,
		dualWriter:                   dual,
		folderClient:                 folderClient,
		legacy: &DashboardStorage{
			Access:           legacy.NewDashboardAccess(dbp, namespacer, dashStore, provisioning, softDelete, sorter),
			DashboardService: &dashboardService,
		},
		reg: reg,
	}
	apiregistration.RegisterAPI(builder)
	return builder
}

func (b *DashboardsAPIBuilder) GetGroupVersions() []schema.GroupVersion {
	if featuremgmt.AnyEnabled(b.features, featuremgmt.FlagDashboardNewLayouts) {
		// If dashboards v2 is enabled, we want to use v2alpha1 as the default API version.
		return []schema.GroupVersion{
			v2alpha1.DashboardResourceInfo.GroupVersion(),
			v0alpha1.DashboardResourceInfo.GroupVersion(),
			v1alpha1.DashboardResourceInfo.GroupVersion(),
		}
	}

	return []schema.GroupVersion{
		v1alpha1.DashboardResourceInfo.GroupVersion(),
		v0alpha1.DashboardResourceInfo.GroupVersion(),
		v2alpha1.DashboardResourceInfo.GroupVersion(),
	}
}

func (b *DashboardsAPIBuilder) InstallSchema(scheme *runtime.Scheme) error {
	b.scheme = scheme
	if err := v0alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v2alpha1.AddToScheme(scheme); err != nil {
		return err
	}

	// Register the explicit conversions
	if err := conversion.RegisterConversions(scheme); err != nil {
		return err
	}

	return scheme.SetVersionPriority(b.GetGroupVersions()...)
}

func (b *DashboardsAPIBuilder) GetAuthorizer() authorizer.Authorizer {
	// TODO: This can be removed once we're fully switched to unified storage
	// as it will take care of authorizations
	return GetAuthorizer(b.dashboardService, b.dualWriter, b.log)
}

// Validate validates dashboard operations for the apiserver
func (b *DashboardsAPIBuilder) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	nsInfo, err := types.ParseNamespace(a.GetNamespace())
	if err != nil {
		return fmt.Errorf("failed to parse namespace: %w", err)
	}

	id, err := identity.GetRequester(ctx)
	if err != nil {
		return fmt.Errorf("error getting requester: %w", err)
	}

	// Validate organization access
	if id.GetOrgID() != nsInfo.OrgID {
		return apierrors.NewNotFound(a.GetResource().GroupResource(), a.GetName())
	}

	op := a.GetOperation()

	// Handle different operations
	switch op {
	case admission.Delete:
		return b.validateDelete(ctx, a)
	case admission.Create:
		return b.validateCreate(ctx, a, o)
	case admission.Update:
		return b.validateUpdate(ctx, a, o)
	case admission.Connect:
		return nil // TODO: What does this exactly mean?
	}

	return nil
}

// validateDelete checks if a dashboard can be deleted
func (b *DashboardsAPIBuilder) validateDelete(ctx context.Context, a admission.Attributes) error {
	obj := a.GetOperationOptions()
	deleteOptions, ok := obj.(*metav1.DeleteOptions)
	if !ok {
		return fmt.Errorf("expected v1.DeleteOptions")
	}

	// Skip validation for forced deletions (grace period = 0)
	if deleteOptions.GracePeriodSeconds != nil && *deleteOptions.GracePeriodSeconds == 0 {
		return nil
	}

	nsInfo, err := types.ParseNamespace(a.GetNamespace())
	if err != nil {
		return fmt.Errorf("%v: %w", "failed to parse namespace", err)
	}

	// The name of the resource is the dashboard UID
	dashboardUID := a.GetName()

	provisioningData, err := b.dashboardProvisioningService.GetProvisionedDashboardDataByDashboardUID(ctx, nsInfo.OrgID, dashboardUID)
	if err != nil {
		if errors.Is(err, dashboards.ErrProvisionedDashboardNotFound) ||
			errors.Is(err, dashboards.ErrDashboardNotFound) ||
			apierrors.IsNotFound(err) {
			return nil
		}

		return fmt.Errorf("%v: %w", "delete hook failed to check if dashboard is provisioned", err)
	}

	if provisioningData != nil {
		return apierrors.NewBadRequest(dashboards.ErrDashboardCannotDeleteProvisionedDashboard.Reason)
	}

	return nil
}

// validateCreate validates dashboard creation
func (b *DashboardsAPIBuilder) validateCreate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	// Get the dashboard object
	dashObj := a.GetObject()

	title, refresh, err := getDashboardProperties(dashObj)
	if err != nil {
		return fmt.Errorf("error extracting dashboard properties: %w", err)
	}

	accessor, err := utils.MetaAccessor(dashObj)
	if err != nil {
		return fmt.Errorf("error getting meta accessor: %w", err)
	}

	// Parse namespace for orgID
	nsInfo, err := types.ParseNamespace(a.GetNamespace())
	if err != nil {
		return fmt.Errorf("failed to parse namespace: %w", err)
	}

	// Basic validations
	if err := b.validateBasicProperties(title, accessor); err != nil {
		return err
	}

	// Check for UID uniqueness
	// Let the storage handle this.
	// if accessor.GetName() != "" {
	// 	existing, err := b.dashStore.GetDashboard(ctx, &dashboards.GetDashboardQuery{
	// 		UID:   accessor.GetName(),
	// 		OrgID: nsInfo.OrgID,
	// 	})
	// 	if err == nil && existing != nil {
	// 		return dashboards.ErrDashboardWithSameUIDExists
	// 	} else if err != nil && !errors.Is(err, dashboards.ErrDashboardNotFound) {
	// 		return fmt.Errorf("error checking dashboard UID uniqueness: %w", err)
	// 	}
	// }

	// Validate folder existence if specified
	if accessor.GetFolder() != "" {
		if err := b.validateFolderExists(ctx, accessor.GetFolder(), nsInfo.OrgID); err != nil {
			return err
		}
	}

	// Validate refresh interval
	if err := b.validateRefreshInterval(refresh); err != nil {
		return err
	}

	// Check if dashboard is provisioned and if it allows updates
	//mgr, hasMgr := accessor.GetManagerProperties()
	//if hasMgr && mgr.Identity != "" && !mgr.AllowsEdits {
	//	return dashboards.ErrDashboardCannotSaveProvisionedDashboard
	//}

	id, err := identity.GetRequester(ctx)
	if err != nil {
		return fmt.Errorf("error getting requester: %w", err)
	}

	internalId, err := id.GetInternalID()
	if err != nil {
		return fmt.Errorf("error getting internal ID: %w", err)
	}

	// Validate quota
	params := &quota.ScopeParameters{}
	params.OrgID = id.GetOrgID()
	params.UserID = internalId

	quotaReached, err := b.QuotaService.CheckQuotaReached(ctx, dashboards.QuotaTargetSrv, params)
	if err != nil {
		// Ignore quota disabled errors
		// TODO: Research
		if !strings.Contains(err.Error(), "quota.disabled") {
			return fmt.Errorf("error checking quota: %w", err)
		}
	}
	if quotaReached {
		return fmt.Errorf("dashboard quota reached") // TODO: Add a more specific error message as before
	}

	return nil
}

// validateUpdate validates dashboard updates
func (b *DashboardsAPIBuilder) validateUpdate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) error {
	// Get the new and old dashboards
	newDashObj := a.GetObject()
	oldDashObj := a.GetOldObject()

	title, refresh, err := getDashboardProperties(newDashObj)
	if err != nil {
		return fmt.Errorf("error extracting dashboard properties: %w", err)
	}

	oldAccessor, err := utils.MetaAccessor(oldDashObj)
	if err != nil {
		return fmt.Errorf("error getting meta accessor: %w", err)
	}

	newAccessor, err := utils.MetaAccessor(newDashObj)
	if err != nil {
		return fmt.Errorf("error getting meta accessor: %w", err)
	}

	// Parse namespace for old dashboard
	nsInfo, err := types.ParseNamespace(oldAccessor.GetNamespace())
	if err != nil {
		return fmt.Errorf("failed to parse namespace: %w", err)
	}

	// Validate requester's organization context
	id, err := identity.GetRequester(ctx)
	if err != nil {
		return fmt.Errorf("error getting requester: %w", err)
	}

	// Validate organization access
	if id.GetOrgID() != nsInfo.OrgID {
		return apierrors.NewNotFound(a.GetResource().GroupResource(), a.GetName())
	}

	// Basic validations
	if err := b.validateBasicProperties(title, newAccessor); err != nil {
		return err
	}

	// Validate folder existence if specified and changed
	if newAccessor.GetFolder() != "" && newAccessor.GetFolder() != oldAccessor.GetFolder() {
		if err := b.validateFolderExists(ctx, newAccessor.GetFolder(), nsInfo.OrgID); err != nil {
			return err
		}
	}

	// Validate refresh interval
	if err := b.validateRefreshInterval(refresh); err != nil {
		return err
	}

	// Check for provisioning - disallow updates to provisioned dashboards if not allowed
	if err := b.validateProvisionedDashboardUpdate(ctx, oldAccessor); err != nil {
		return err
	}

	// Validate generation conflicts
	if oldAccessor.GetGeneration() > newAccessor.GetGeneration() {
		return dashboards.ErrDashboardVersionMismatch // TODO: Check if this is the correct error to return
	}

	return nil
}

// validateBasicProperties validates basic dashboard properties
func (b *DashboardsAPIBuilder) validateBasicProperties(title string, accessor utils.GrafanaMetaAccessor) error {
	// Validate title
	if title == "" {
		return fmt.Errorf("dashboard title cannot be empty")
	}

	if len(title) > 5000 {
		return fmt.Errorf("dashboard title is too long (max 5000 characters)")
	}

	// Validate UID - get it from accesso
	uid := accessor.GetName()
	if uid != "" {
		// Check UID length
		if len(uid) > 40 { // TODO: Set correct length
			return fmt.Errorf("dashboard UID is too long (max 40 characters)")
		}

		// Check valid UID format using regex
		uidPattern := regexp.MustCompile(`^[a-zA-Z0-9\-\_]+$`) // TODO: Make const outside
		if !uidPattern.MatchString(uid) {
			return fmt.Errorf("dashboard UID can only contain alphanumeric characters, dashes and underscores")
		}
	}

	// Validate message
	if message := accessor.GetMessage(); message != "" && len(message) > 500 {
		return fmt.Errorf("dashboard update message is too long (max 500 characters)")
	}

	return nil
}

// validateFolderExists checks if a folder exists
func (b *DashboardsAPIBuilder) validateFolderExists(ctx context.Context, folderUID string, orgID int64) error {
	// Check if folder exists using the folder store
	_, err := b.folderClient.Get(ctx, folderUID, orgID, v1.GetOptions{})

	if err != nil {
		if errors.Is(err, dashboards.ErrFolderNotFound) {
			return err
		}
		return fmt.Errorf("error checking folder existence: %w", err)
	}

	return nil
}

// validateRefreshInterval validates dashboard refresh interval
func (b *DashboardsAPIBuilder) validateRefreshInterval(refresh string) error {
	if refresh == "" || refresh == "auto" {
		return nil
	}

	// Skip validation for disabled refresh
	if refresh == "0" || strings.ToLower(refresh) == "off" {
		return nil
	}

	minRefreshInterval := b.cfg.MinRefreshInterval
	if minRefreshInterval == "" {
		return nil
	}

	minInterval, err := time.ParseDuration(minRefreshInterval)
	if err != nil {
		return fmt.Errorf("invalid min refresh interval format: %s", minRefreshInterval)
	}

	refreshInterval, err := time.ParseDuration(refresh)
	if err != nil {
		return fmt.Errorf("invalid refresh interval format: %s", refresh)
	}

	if refreshInterval < minInterval {
		return dashboards.ErrDashboardRefreshIntervalTooShort
	}

	return nil
}

// validateProvisionedDashboardUpdate checks if a provisioned dashboard can be updated
func (b *DashboardsAPIBuilder) validateProvisionedDashboardUpdate(ctx context.Context, meta utils.GrafanaMetaAccessor) error {
	manager, ok := meta.GetManagerProperties()
	if !ok {
		return nil
	}

	if manager.Kind == "" {
		return nil
	}

	if !manager.AllowsEdits {
		return dashboards.ErrDashboardCannotSaveProvisionedDashboard
	}

	// TODO: Check overwrite flag

	return nil
}

// getDashboardProperties extracts title and refresh interval from any dashboard version
func getDashboardProperties(obj runtime.Object) (string, string, error) {
	var title, refresh string

	// Extract properties based on the object's type
	switch d := obj.(type) {
	case *v0alpha1.Dashboard:
		title = d.Spec.GetNestedString(DASHBOARD_SPEC_TITLE)
		refresh = d.Spec.GetNestedString(DASHBOARD_SPEC_REFRESH_INTERVAL)
	case *v1alpha1.Dashboard:
		title = d.Spec.GetNestedString(DASHBOARD_SPEC_TITLE)
		refresh = d.Spec.GetNestedString(DASHBOARD_SPEC_REFRESH_INTERVAL)
	case *v2alpha1.Dashboard:
		title = d.Spec.Title
		refresh = d.Spec.TimeSettings.AutoRefresh
	default:
		return "", "", fmt.Errorf("unsupported dashboard version: %T", obj)
	}

	return title, refresh, nil
}

func (b *DashboardsAPIBuilder) UpdateAPIGroupInfo(apiGroupInfo *genericapiserver.APIGroupInfo, opts builder.APIGroupOptions) error {
	storageOpts := apistore.StorageOptions{
		EnableFolderSupport:         true,
		RequireDeprecatedInternalID: true,
	}

	// Split dashboards when they are large
	var largeObjects apistore.LargeObjectSupport
	if b.features.IsEnabledGlobally(featuremgmt.FlagUnifiedStorageBigObjectsSupport) {
		largeObjects = NewDashboardLargeObjectSupport(opts.Scheme)
		storageOpts.LargeObjectSupport = largeObjects
	}

	opts.StorageOptions(v0alpha1.DashboardResourceInfo.GroupResource(), storageOpts)

	// v0alpha1
	if err := b.storageForVersion(apiGroupInfo, opts, largeObjects,
		v0alpha1.DashboardResourceInfo,
		v0alpha1.LibraryPanelResourceInfo,
		func(obj runtime.Object, access *internal.DashboardAccess) (v runtime.Object, err error) {
			dto := &v0alpha1.DashboardWithAccessInfo{}
			dash, ok := obj.(*v0alpha1.Dashboard)
			if ok {
				dto.Dashboard = *dash
			}
			if access != nil {
				err = b.scheme.Convert(access, &dto.Access, nil)
			}
			return dto, err
		}); err != nil {
		return err
	}

	// v1alpha1
	if err := b.storageForVersion(apiGroupInfo, opts, largeObjects,
		v1alpha1.DashboardResourceInfo,
		v1alpha1.LibraryPanelResourceInfo,
		func(obj runtime.Object, access *internal.DashboardAccess) (v runtime.Object, err error) {
			dto := &v1alpha1.DashboardWithAccessInfo{}
			dash, ok := obj.(*v1alpha1.Dashboard)
			if ok {
				dto.Dashboard = *dash
			}
			if access != nil {
				err = b.scheme.Convert(access, &dto.Access, nil)
			}
			return dto, err
		}); err != nil {
		return err
	}

	// v2alpha1
	if err := b.storageForVersion(apiGroupInfo, opts, largeObjects,
		v2alpha1.DashboardResourceInfo,
		v2alpha1.LibraryPanelResourceInfo,
		func(obj runtime.Object, access *internal.DashboardAccess) (v runtime.Object, err error) {
			dto := &v2alpha1.DashboardWithAccessInfo{}
			dash, ok := obj.(*v2alpha1.Dashboard)
			if ok {
				dto.Dashboard = *dash
			}
			if access != nil {
				err = b.scheme.Convert(access, &dto.Access, nil)
			}
			return dto, err
		}); err != nil {
		return err
	}

	return nil
}

func (b *DashboardsAPIBuilder) storageForVersion(
	apiGroupInfo *genericapiserver.APIGroupInfo,
	opts builder.APIGroupOptions,
	largeObjects apistore.LargeObjectSupport,
	dashboards utils.ResourceInfo,
	libraryPanels utils.ResourceInfo,
	newDTOFunc dtoBuilder,
) error {
	// Register the versioned storage
	storage := map[string]rest.Storage{}
	apiGroupInfo.VersionedResourcesStorageMap[dashboards.GroupVersion().Version] = storage

	legacyStore, err := b.legacy.NewStore(dashboards, opts.Scheme, opts.OptsGetter, b.reg)
	if err != nil {
		return err
	}

	store, err := grafanaregistry.NewRegistryStore(opts.Scheme, dashboards, opts.OptsGetter)
	if err != nil {
		return err
	}

	gr := dashboards.GroupResource()
	storage[dashboards.StoragePath()], err = opts.DualWriteBuilder(gr, legacyStore, store)
	if err != nil {
		return err
	}

	// Register the DTO endpoint that will consolidate all dashboard bits
	storage[dashboards.StoragePath("dto")], err = NewDTOConnector(
		storage[dashboards.StoragePath()].(rest.Getter),
		largeObjects,
		b.legacy.Access,
		b.unified,
		b.accessControl,
		opts.Scheme,
		newDTOFunc,
	)
	if err != nil {
		return err
	}

	// Expose read only library panels
	storage[libraryPanels.StoragePath()] = &LibraryPanelStore{
		Access:       b.legacy.Access,
		ResourceInfo: libraryPanels,
	}

	return nil
}

func (b *DashboardsAPIBuilder) GetOpenAPIDefinitions() common.GetOpenAPIDefinitions {
	return func(ref common.ReferenceCallback) map[string]common.OpenAPIDefinition {
		defs := v0alpha1.GetOpenAPIDefinitions(ref)
		maps.Copy(defs, v1alpha1.GetOpenAPIDefinitions(ref))
		maps.Copy(defs, v2alpha1.GetOpenAPIDefinitions(ref))
		return defs
	}
}

func (b *DashboardsAPIBuilder) PostProcessOpenAPI(oas *spec3.OpenAPI) (*spec3.OpenAPI, error) {
	// The plugin description
	oas.Info.Description = "Grafana dashboards as resources"

	for _, gv := range b.GetGroupVersions() {
		version := gv.Version
		// Hide cluster-scoped resources
		root := path.Join("/apis/", v0alpha1.GROUP, version)
		delete(oas.Paths.Paths, path.Join(root, "dashboards"))
		delete(oas.Paths.Paths, path.Join(root, "watch", "dashboards"))

		if version == v0alpha1.VERSION {
			sub := oas.Paths.Paths[path.Join(root, "search", "{name}")]
			oas.Paths.Paths[path.Join(root, "search")] = sub
			delete(oas.Paths.Paths, path.Join(root, "search", "{name}"))
		}
	}

	return oas, nil
}

func (b *DashboardsAPIBuilder) GetAPIRoutes() *builder.APIRoutes {
	defs := b.GetOpenAPIDefinitions()(func(path string) spec.Ref { return spec.Ref{} })
	return b.search.GetAPIRoutes(defs)
}
