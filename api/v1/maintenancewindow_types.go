package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Day is a lowercase weekday name.
// +kubebuilder:validation:Enum=sunday;monday;tuesday;wednesday;thursday;friday;saturday
type Day string

// MaintenanceWindowSpec declares a recurring window in which maintenance may
// run. The window opens at Time on each day listed in DaysOfWeek, read as wall
// clock in TimeZone, and lasts Duration.
//
// Instances sharing a window are spread across its first half by an offset
// derived from the instance, so a window governing many instances does not
// start all of them at once.
type MaintenanceWindowSpec struct {
	// DaysOfWeek are the days a window starts on. A window running past
	// midnight ends on the following day, which need not be listed here.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +kubebuilder:validation:XValidation:rule="self.all(d, self.exists_one(x, x == d))",message="daysOfWeek must not contain duplicates"
	// +required
	DaysOfWeek []Day `json:"daysOfWeek"`

	// Time is the wall-clock start of the window in TimeZone, "HH:MM" in
	// 24-hour form.
	// +kubebuilder:validation:Pattern=`^([01][0-9]|2[0-3]):[0-5][0-9]$`
	// +kubebuilder:validation:XValidation:rule="self.matches('^([01][0-9]|2[0-3]):[0-5][0-9]$')",message="time must be HH:MM in 24-hour form, for example 22:00"
	// +required
	Time string `json:"time"`

	// Duration is how long the window lasts, measured as elapsed time rather
	// than wall clock. Across a daylight-saving change the window therefore
	// ends at a different wall-clock time than usual, and the instance keeps
	// its full duration either way.
	// +kubebuilder:validation:XValidation:rule="self.matches('^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$') && duration(self) >= duration('1h') && duration(self) <= duration('24h')",message="duration must be a positive duration between 1h and 24h, for example 6h"
	// +required
	Duration metav1.Duration `json:"duration"`

	// TimeZone is an IANA zone name, for example "Europe/Zurich". The API
	// server cannot check that the name is a real zone, so an unknown zone is
	// only rejected when the window is resolved.
	// +kubebuilder:default="UTC"
	// +kubebuilder:validation:MinLength=1
	// +optional
	TimeZone string `json:"timeZone,omitempty"`
}

// MaintenanceWindow is an operator-owned schedule that instances reference by
// name. It is cluster-scoped: instances across every namespace share a small
// set of named windows, which is what lets maintenance be batched.
// +kubebuilder:object:root=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Days",type=string,JSONPath=`.spec.daysOfWeek`
// +kubebuilder:printcolumn:name="Time",type=string,JSONPath=`.spec.time`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.spec.duration`
// +kubebuilder:printcolumn:name="Timezone",type=string,JSONPath=`.spec.timeZone`
type MaintenanceWindow struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec MaintenanceWindowSpec `json:"spec,omitempty"`
}

// MaintenanceWindowList contains a list of MaintenanceWindow.
// +kubebuilder:object:root=true
type MaintenanceWindowList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []MaintenanceWindow `json:"items"`
}

func init() { SchemeBuilder.Register(&MaintenanceWindow{}, &MaintenanceWindowList{}) }
