package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MaintenanceDefinitionSpec schedules one instance's maintenance. Every field
// is optional: an empty spec is a complete schedule, running the maintenance
// ritual in the operator's default window.
type MaintenanceDefinitionSpec struct {
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

// MaintenanceDefinitionStatus reports the schedule this instance resolved to.
type MaintenanceDefinitionStatus struct {
	// Schedule is the cron expression this instance runs on, including its
	// offset within the window.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// CronJobName is the CronJob executing the schedule, in this namespace.
	// +optional
	CronJobName string `json:"cronJobName,omitempty"`

	// Message explains why no schedule could be resolved.
	// +optional
	Message string `json:"message,omitempty"`
}

// MaintenanceDefinition is one instance's maintenance schedule. It lives in
// the instance namespace, names a MaintenanceWindow to run in and a Definition
// to run, and produces the CronJob that fires it.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Window",type=string,JSONPath=`.spec.window`
// +kubebuilder:printcolumn:name="Ritual",type=string,JSONPath=`.spec.ritual`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.status.schedule`
// +kubebuilder:printcolumn:name="Suspended",type=boolean,JSONPath=`.spec.suspend`
type MaintenanceDefinition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   MaintenanceDefinitionSpec   `json:"spec,omitempty"`
	Status MaintenanceDefinitionStatus `json:"status,omitempty"`
}

// MaintenanceDefinitionList contains a list of MaintenanceDefinition.
// +kubebuilder:object:root=true
type MaintenanceDefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MaintenanceDefinition `json:"items"`
}

func init() {
	SchemeBuilder.Register(&MaintenanceDefinition{}, &MaintenanceDefinitionList{})
}
