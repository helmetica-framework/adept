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
	// Type names the Definition (in the instance namespace) to execute.
	// +kubebuilder:validation:MinLength=1
	// +required
	Type string `json:"type"`

	// Claim identifies the service instance; with ApiVersion and Kind it
	// derives the instance namespace when the Action lives in a claim
	// namespace.
	// +optional
	Claim string `json:"claim,omitempty"`

	// ApiVersion is the claim's apiVersion ("group/version"). Required
	// together with Kind when the Action lives in a claim namespace.
	// +optional
	ApiVersion string `json:"apiVersion,omitempty"`

	// Kind is the claim's kind. Required together with ApiVersion when the
	// Action lives in a claim namespace.
	// +optional
	Kind string `json:"kind,omitempty"`

	// Args are made available to the job. Stored, not injected for now.
	// +optional
	Args map[string]string `json:"args,omitempty"`
}

type ActionStatus struct {
	// +optional
	Phase ActionPhase `json:"phase,omitempty"`
	// +optional
	JobName string `json:"jobName,omitempty"`
	// Message explains a Failed phase caused by a spec or claim problem.
	// +optional
	Message string `json:"message,omitempty"`
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
