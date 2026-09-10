package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const (
	Monday    Day = "monday"
	Tuesday   Day = "tuesday"
	Wednesday Day = "wednesday"
	Thursday  Day = "thursday"
	Friday    Day = "friday"
	Saturday  Day = "saturday"
	Sunday    Day = "sunday"
)

// Day is a lowercase weekday name.
// +kubebuilder:validation:Enum=sunday;monday;tuesday;wednesday;thursday;friday;saturday
type Day string

// MaintenanceWindowSpec declares a recurring window in which maintenance may
// start. The window opens at Time on each day listed in DaysOfWeek, read as wall
// clock in TimeZone, and stays open for Duration.
//
// Instances sharing a window are spread across the window by an offset derived
// from the instance, so a window governing many instances does not start all
// of them at once. Maintenance that has started is never interrupted by the
// window closing.
type MaintenanceWindowSpec struct {
	// DaysOfWeek are the days a window starts on, with no repeats. A window
	// running past midnight ends on the following day, which need not be
	// listed here.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=7
	// +required
	DaysOfWeek []Day `json:"daysOfWeek"`

	// Time is the wall-clock start of the window in TimeZone, "HH:MM" in
	// 24-hour form, for example "22:00".
	// +required
	Time string `json:"time"`

	// Duration is the time span during which maintenance may start, between 1h and
	// 24h. Instances sharing a window are spread across it so they do not all
	// start at once.
	//
	// It does not bound how long maintenance runs. A run that starts inside
	// the window keeps going until it finishes, which may be well after the window
	// has closed.
	// +kubebuilder:validation:XValidation:rule="self.matches('^([0-9]+([.][0-9]+)?(ns|us|ms|s|m|h))+$')",message="duration must be a valid duration, for example 6h"
	// +required
	Duration metav1.Duration `json:"duration"`

	// TimeZone is the IANA zone name the window's Time is read in, for example
	// "Europe/Zurich". Defaults to UTC.
	// +kubebuilder:default="UTC"
	// +kubebuilder:validation:MinLength=1
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// Default marks this window as the one instances take when they name no
	// window of their own. At most one window may be the default; a second
	// one claiming it is rejected.
	// +optional
	Default bool `json:"default,omitempty"`
}

// MaintenanceWindow is an operator-owned schedule that instances reference by
// name. It is cluster-scoped: instances across every namespace share a small
// set of named windows, which is what lets maintenance be batched.
// +kubebuilder:object:root=true
// +kubebuilder:ac:generate=true
// +kubebuilder:resource:scope=Cluster
// +kubebuilder:printcolumn:name="Days",type=string,JSONPath=`.spec.daysOfWeek`
// +kubebuilder:printcolumn:name="Time",type=string,JSONPath=`.spec.time`
// +kubebuilder:printcolumn:name="Duration",type=string,JSONPath=`.spec.duration`
// +kubebuilder:printcolumn:name="Timezone",type=string,JSONPath=`.spec.timeZone`
// +kubebuilder:printcolumn:name="Default",type=boolean,JSONPath=`.spec.default`
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
