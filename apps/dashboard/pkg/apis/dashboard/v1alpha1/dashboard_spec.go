package v1alpha1

import common "github.com/grafana/grafana/pkg/apimachinery/apis/common/v0alpha1"

// +k8s:openapi-gen=true
type DashboardSpec = common.Unstructured

// TODO: Can we have a more specific type for the spec?
/*
type DashboardSpec struct {
	// The dashboard display name (shown in the UI)
	Title string `json:"title"`

	// The dashboard version
	Version int64 `json:"version,omitempty"`

	// The dashboard refresh interval
	RefreshInterval string `json:"refresh,omitempty"`
}
*/

// NewDashboardSpec creates a new Spec object.
func NewDashboardSpec() *DashboardSpec {
	return &DashboardSpec{}
}
