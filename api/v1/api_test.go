package v1

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestSchemeRegistration(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, AddToScheme(scheme))

	assert.True(t, scheme.Recognizes(GroupVersion.WithKind("Definition")))
	assert.True(t, scheme.Recognizes(GroupVersion.WithKind("DefinitionList")))
	assert.True(t, scheme.Recognizes(GroupVersion.WithKind("Action")))
	assert.True(t, scheme.Recognizes(GroupVersion.WithKind("ActionList")))
	assert.Equal(t, "rituals.helmetica.io", GroupVersion.Group)
	assert.Equal(t, "v1", GroupVersion.Version)
}

func TestActionDeepCopy(t *testing.T) {
	orig := &Action{
		Spec: ActionSpec{
			Type:  "restart",
			Claim: "my-claim",
			Args:  map[string]string{"key": "value"},
		},
		Status: ActionStatus{Phase: ActionPhaseRunning, JobName: "x-restart"},
	}
	cp := orig.DeepCopy()
	require.Equal(t, orig, cp)
	cp.Spec.Args["key"] = "mutated"
	assert.Equal(t, "value", orig.Spec.Args["key"], "deepcopy must not share the args map")
}
