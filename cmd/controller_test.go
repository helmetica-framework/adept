package cmd

import (
	"testing"

	"github.com/stretchr/testify/assert"
	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	ritualsv1 "github.com/helmetica-framework/adept/api/v1"
)

// The manager's client can only read kinds its scheme knows. A controller
// reading one the scheme is missing still compiles and still passes its unit
// tests, because those build a scheme of their own; it fails only against a
// real API server.
func TestScheme_KnowsEveryKindTheControllersRead(t *testing.T) {
	scheme := newScheme()

	assert.True(t, scheme.Recognizes(ritualsv1.GroupVersion.WithKind("Maintenance")))
	assert.True(t, scheme.Recognizes(ritualsv1.GroupVersion.WithKind("MaintenanceWindow")))
	assert.True(t, scheme.Recognizes(ritualsv1.GroupVersion.WithKind("Action")))
	assert.True(t, scheme.Recognizes(ritualsv1.GroupVersion.WithKind("Definition")),
		"VersionManager lists CustomResourceDefinitions to find the newest version a claim allows")
	assert.True(t, scheme.Recognizes(apiextv1.SchemeGroupVersion.WithKind("CustomResourceDefinition")),
		"VersionManager lists CustomResourceDefinitions to find the newest version a claim allows")
}
