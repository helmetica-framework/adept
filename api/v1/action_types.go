package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type ActionPhase string

const (
	ActionPhasePending   ActionPhase = "Pending"
	ActionPhaseRunning   ActionPhase = "Running"
	ActionPhaseSucceeded ActionPhase = "Succeeded"
	ActionPhaseFailed    ActionPhase = "Failed"
)

// ActionSpec triggers a ritual Definition.
type ActionSpec struct {
	// Type names the Definition (same namespace) to execute.
	// +kubebuilder:validation:MinLength=1
	// +required
	Type string `json:"type"`

	// Claim identifies the service instance. Stored, unused for now.
	// +optional
	Claim string `json:"claim,omitempty"`

	// Args are made available to the job. Stored, not injected for now.
	// +optional
	Args map[string]string `json:"args,omitempty"`
}

type ActionStatus struct {
	// +optional
	Phase ActionPhase `json:"phase,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
}

// Action triggers a ritual Definition and tracks its Job to completion.
// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type Action struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ActionSpec   `json:"spec,omitempty"`
	Status ActionStatus `json:"status,omitempty"`
}

// ActionList contains a list of Action.
// +kubebuilder:object:root=true
type ActionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Action `json:"items"`
}

func init() { SchemeBuilder.Register(&Action{}, &ActionList{}) }
