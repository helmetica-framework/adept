package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaintenanceSpec schedules one instance's maintenance. Every field is
// optional: an empty spec is a complete schedule, running the maintenance
// ritual in the operator's default window.
type MaintenanceSpec struct {
	// Window names the MaintenanceWindow this instance's maintenance starts
	// in. Empty takes the window the operator marked as the default.
	// +optional
	Window string `json:"window,omitempty"`

	// Ritual names the Definition to run, in this namespace.
	// +kubebuilder:default="maintenance"
	// +kubebuilder:validation:MinLength=1
	// +optional
	Ritual string `json:"ritual,omitempty"`

	// Suspend stops maintenance for this instance. Its schedule stays in
	// place and stays visible.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// MaintenanceStatus reports the schedule this instance resolved to.
type MaintenanceStatus struct {
	// Schedule is the cron expression this instance runs on, including its
	// offset within the window.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// ObservedGeneration is the spec generation the schedule was computed
	// from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// CronJobName is the CronJob executing the schedule, in this namespace.
	// +optional
	CronJobName string `json:"cronJobName,omitempty"`

	// VersionUpdatedFor is the maintenance this instance last settled its
	// version for. A suspended instance settles without moving anything. It
	// stays empty while the instance pins its own version.
	// +optional
	VersionUpdatedFor *metav1.Time `json:"versionUpdatedFor,omitempty"`

	// ObservedBumpRequest is the value of the
	// rituals.helmetica.io/bump-now annotation this instance last acted on.
	// Set the annotation to any new value to move the version straight away
	// instead of waiting for the window.
	// +optional
	ObservedBumpRequest string `json:"observedBumpRequest,omitempty"`

	// Message explains why no schedule could be resolved.
	// +optional
	Message string `json:"message,omitempty"`
}

// Maintenance is one instance's maintenance schedule. It lives in the instance
// namespace, names a MaintenanceWindow to run in and a Definition to run, and
// produces the CronJob that fires it.
// +kubebuilder:object:root=true
// +kubebuilder:ac:generate=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.spec.window`
// +kubebuilder:printcolumn:name="Ritual",type=string,JSONPath=`.spec.ritual`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.status.schedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
type Maintenance struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaintenanceSpec   `json:"spec,omitempty"`
	Status MaintenanceStatus `json:"status,omitempty"`
}

// MaintenanceList contains a list of Maintenance.
// +kubebuilder:object:root=true
type MaintenanceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Maintenance `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Maintenance{}, &MaintenanceList{})
}
