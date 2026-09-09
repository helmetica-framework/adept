package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	k8syaml "k8s.io/apimachinery/pkg/util/yaml"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

// Guards the generated webhook manifest. controller-gen writes it from the
// marker in controllers/maintenancewindow_webhook.go, but the path in that
// marker is hand-typed while the path the manager actually serves is derived
// from the GroupVersionKind. If the two drift apart, every MaintenanceWindow
// write is rejected, because failurePolicy is Fail and the API server is
// calling a path nothing answers on. Nothing surfaces that until a deploy.

const webhookManifest = "config/webhook/manifests.yaml"

func TestValidatingWebhookPathMatchesTheServedPath(t *testing.T) {
	webhook := maintenanceWindowWebhook(t)

	// Mirrors generateValidatePath in controller-runtime's webhook builder:
	// "/validate-" + group with dots as dashes + "-" + version + "-" + lowercase kind.
	want := "/validate-" +
		strings.ReplaceAll(ritualsv1.GroupVersion.Group, ".", "-") + "-" +
		ritualsv1.GroupVersion.Version + "-" +
		strings.ToLower("MaintenanceWindow")

	require.NotNil(t, webhook.ClientConfig.Service)
	assert.Equal(t, want, *webhook.ClientConfig.Service.Path)
}

func TestValidatingWebhookFailsClosed(t *testing.T) {
	// Ignore would admit exactly the windows this webhook exists to reject
	// whenever adept is unreachable. Changing this is a decision, not a tweak.
	webhook := maintenanceWindowWebhook(t)

	require.NotNil(t, webhook.FailurePolicy)
	assert.Equal(t, admissionv1.Fail, *webhook.FailurePolicy)
}

func TestValidatingWebhookDoesNotInterceptDeletes(t *testing.T) {
	// An invalid window has to stay deletable, including one stored before
	// this webhook existed.
	webhook := maintenanceWindowWebhook(t)

	require.Len(t, webhook.Rules, 1)
	assert.ElementsMatch(t,
		[]admissionv1.OperationType{admissionv1.Create, admissionv1.Update},
		webhook.Rules[0].Operations)
	assert.Equal(t, []string{"maintenancewindows"}, webhook.Rules[0].Resources)
}

// maintenanceWindowWebhook returns the one webhook registered for
// maintenancewindows in the generated configuration.
func maintenanceWindowWebhook(t *testing.T) admissionv1.ValidatingWebhook {
	t.Helper()

	raw, err := os.ReadFile(webhookManifest)
	require.NoError(t, err, "run `just manifests` to generate %s", webhookManifest)

	// The file starts with a document separator, so decode until EOF rather
	// than assuming the first document is the configuration.
	decoder := k8syaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var config admissionv1.ValidatingWebhookConfiguration
		err := decoder.Decode(&config)
		if errors.Is(err, io.EOF) {
			break
		}
		require.NoError(t, err)

		for _, webhook := range config.Webhooks {
			for _, rule := range webhook.Rules {
				if len(rule.Resources) > 0 && rule.Resources[0] == "maintenancewindows" {
					return webhook
				}
			}
		}
	}

	t.Fatalf("no ValidatingWebhook for maintenancewindows in %s", webhookManifest)
	return admissionv1.ValidatingWebhook{}
}
