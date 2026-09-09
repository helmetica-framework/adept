package controllers

import (
	"context"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

// +kubebuilder:webhook:path=/validate-rituals-helmetica-io-v1-maintenancewindow,mutating=false,failurePolicy=fail,sideEffects=None,groups=rituals.helmetica.io,resources=maintenancewindows,verbs=create;update,versions=v1,name=vmaintenancewindow.rituals.helmetica.io,admissionReviewVersions=v1

// MaintenanceWindowValidator rejects windows the schedule package cannot use.
type MaintenanceWindowValidator struct{}

var _ admission.Validator[*ritualsv1.MaintenanceWindow] = &MaintenanceWindowValidator{}

func (v *MaintenanceWindowValidator) ValidateCreate(_ context.Context, window *ritualsv1.MaintenanceWindow) (admission.Warnings, error) {
	return nil, schedule.Validate(window.Spec)
}

func (v *MaintenanceWindowValidator) ValidateUpdate(_ context.Context, _, window *ritualsv1.MaintenanceWindow) (admission.Warnings, error) {
	return nil, schedule.Validate(window.Spec)
}

// ValidateDelete is required by the interface but not really applicable.
func (v *MaintenanceWindowValidator) ValidateDelete(_ context.Context, _ *ritualsv1.MaintenanceWindow) (admission.Warnings, error) {
	return nil, nil
}

// SetupWithManager registers the validator on the manager's webhook server.
func (v *MaintenanceWindowValidator) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &ritualsv1.MaintenanceWindow{}).
		WithValidator(v).
		Complete()
}
