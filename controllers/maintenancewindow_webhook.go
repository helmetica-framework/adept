package controllers

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
	"github.com/helmetica-framework/adept/schedule"
)

// +kubebuilder:webhook:path=/validate-rituals-helmetica-io-v1-maintenancewindow,mutating=false,failurePolicy=fail,sideEffects=None,groups=rituals.helmetica.io,resources=maintenancewindows,verbs=create;update,versions=v1,name=vmaintenancewindow.rituals.helmetica.io,admissionReviewVersions=v1
// +kubebuilder:rbac:groups=rituals.helmetica.io,resources=maintenancewindows,verbs=get;list;watch

// MaintenanceWindowValidator rejects windows the schedule package cannot use,
// and windows claiming a default another window already holds. The uniqueness
// check reads other windows, which is why the validator holds a client;
// schedule.Validate stays pure and judges one window on its own.
type MaintenanceWindowValidator struct {
	client.Client
}

var _ admission.Validator[*ritualsv1.MaintenanceWindow] = &MaintenanceWindowValidator{}

// ValidateCreate admits a window that is usable and does not claim a default
// another window holds.
func (v *MaintenanceWindowValidator) ValidateCreate(ctx context.Context, window *ritualsv1.MaintenanceWindow) (admission.Warnings, error) {
	if err := schedule.Validate(window.Spec); err != nil {
		return nil, err
	}

	return nil, v.rejectSecondDefault(ctx, window)
}

// ValidateUpdate judges the window being written, not the one it replaces, so
// an edit that repairs a stored window is admitted.
func (v *MaintenanceWindowValidator) ValidateUpdate(ctx context.Context, _, window *ritualsv1.MaintenanceWindow) (admission.Warnings, error) {
	if err := schedule.Validate(window.Spec); err != nil {
		return nil, err
	}

	return nil, v.rejectSecondDefault(ctx, window)
}

// rejectSecondDefault refuses a window claiming spec.default while another one
// holds it, naming the holder. If default is false, the list is skipped.
func (v *MaintenanceWindowValidator) rejectSecondDefault(ctx context.Context, mw *ritualsv1.MaintenanceWindow) error {
	if !mw.Spec.Default {
		return nil
	}

	list := &ritualsv1.MaintenanceWindowList{}

	err := v.List(ctx, list)
	if err != nil {
		return fmt.Errorf("listing maintenance windows: %w", err)
	}

	for _, window := range list.Items {
		if mw.GetName() != window.GetName() && window.Spec.Default {
			return fmt.Errorf("window %q is already the default", window.GetName())
		}
	}

	return nil
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
