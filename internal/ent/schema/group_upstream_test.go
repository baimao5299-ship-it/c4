package schema

import (
	"testing"

	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"
	"github.com/stretchr/testify/require"
)

func TestGroupSchemaKeepsLegacyRoutingDefault(t *testing.T) {
	var routing, models map[string]any
	for _, f := range (Group{}).Fields() {
		d := f.Descriptor()
		switch d.Name {
		case "routing_mode":
			routing = map[string]any{"default": d.Default, "enums": d.Enums}
		case "allowed_models":
			models = map[string]any{"default": d.Default}
		}
	}
	require.Equal(t, "accounts", routing["default"])
	require.Len(t, routing["enums"], 2)
	require.NotNil(t, models)
}

func TestGroupUpstreamSchemaHasIndependentPolicyAndRuntimeFields(t *testing.T) {
	fields := make(map[string]*field.Descriptor)
	for _, f := range (GroupUpstream{}).Fields() {
		d := f.Descriptor()
		fields[d.Name] = d
	}
	for _, name := range []string{"group_id", "upstream_id", "weight", "priority", "max_concurrency", "enabled", "cooldown_until", "failure_streak", "last_error"} {
		require.Contains(t, fields, name)
	}
	require.Equal(t, 100, fields["weight"].Default)
	require.Equal(t, 0, fields["priority"].Default)
	require.Equal(t, 8, fields["max_concurrency"].Default)
	require.Equal(t, true, fields["enabled"].Default)
	require.Equal(t, 0, fields["failure_streak"].Default)
}

func TestGroupUpstreamSchemaUsesRequiredForeignKeyEdges(t *testing.T) {
	edges := make(map[string]*edge.Descriptor)
	for _, e := range (GroupUpstream{}).Edges() {
		d := e.Descriptor()
		edges[d.Name] = d
	}
	require.True(t, edges["group"].Required)
	require.Equal(t, "group_id", edges["group"].Field)
	require.True(t, edges["upstream"].Required)
	require.Equal(t, "upstream_id", edges["upstream"].Field)
}
