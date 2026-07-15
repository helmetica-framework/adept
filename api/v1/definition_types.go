package v1

import (
	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// DefinitionSpec defines a ritual: a job template executed on demand.
type DefinitionSpec struct {
	// Description of what this ritual does.
	// +optional
	Description string `json:"description,omitempty"`

	// JobTemplate is instantiated as a Job each time the ritual runs.
	// +required
	JobTemplate batchv1.JobTemplateSpec `json:"jobTemplate"`
}

// Definition is a ritual: a named, on-demand job template.
// +kubebuilder:object:root=true
type Definition struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec DefinitionSpec `json:"spec,omitempty"`
}

// DefinitionList contains a list of Definition.
// +kubebuilder:object:root=true
type DefinitionList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`

	Items []Definition `json:"items"`
}

func init() { SchemeBuilder.Register(&Definition{}, &DefinitionList{}) }
