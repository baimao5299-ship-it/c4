package schema

import (
	"time"

	"entgo.io/ent"
	"entgo.io/ent/dialect/entsql"
	"entgo.io/ent/schema/edge"
	"entgo.io/ent/schema/field"

	"github.com/is7qin/c3api/internal/domain"
)

// Upstream is a standalone management record for a provider endpoint. It keeps
// the existing account/template routing untouched while exposing cost and
// health metadata to the control plane.
type Upstream struct{ ent.Schema }

func (Upstream) Fields() []ent.Field {
	return []ent.Field{
		field.Int64("id"),
		field.String("name").Unique(),
		field.String("base_url"),
		// Null keeps existing rows in their historical newest-first order. A
		// manual drag stores a sparse rank without coupling presentation to ID or
		// routing priority.
		field.Int64("display_order").Optional().Nillable(),
		// The key is write-only at the management API boundary. It is retained here
		// so a health/balance adapter can authenticate without putting secrets in
		// the response model.
		field.String("upstream_key").Optional().Nillable(),
		// Bounded capability snapshot from the last /v1/models read. An empty
		// catalogue is distinguishable from never-read by models_checked_at.
		field.JSON("models", []string{}).
			Default([]string{}).
			Annotations(entsql.Default("[]")),
		// Per-model protocols verified by the last completed capability probe.
		// Existing rows migrate to an empty object, which deliberately means
		// unknown rather than "all protocols".
		field.JSON("model_formats", map[string][]domain.RequestFormat{}).
			Default(map[string][]domain.RequestFormat{}).
			Annotations(entsql.Default("{}")),
		field.Time("models_checked_at").Optional().Nillable(),
		field.String("models_error").Optional().Nillable(),
		field.Int("multiplier_bp").Default(10000),
		field.Bool("enabled").Default(true),
		field.String("note").Optional().Nillable(),
		field.String("balance_endpoint").Default(""),
		field.String("balance_method").Default(""),
		field.String("balance_auth").Default(""),
		field.String("balance_path").Default(""),
		field.String("balance_currency_path").Default(""),
		field.String("balance_amount").Optional().Nillable(),
		field.String("balance_currency").Optional().Nillable(),
		field.Enum("balance_status").Values("fresh", "stale", "unavailable", "unconfigured").Default("unconfigured"),
		field.Time("balance_checked_at").Optional().Nillable(),
		field.Int64("request_count").Default(0),
		field.Int64("success_count").Default(0),
		field.Int64("failure_count").Default(0),
		field.Int64("latency_total_ms").Default(0),
		field.Int64("latency_max_ms").Default(0),
		field.Time("last_checked_at").Optional().Nillable(),
		field.Time("last_success_at").Optional().Nillable(),
		field.Time("last_failure_at").Optional().Nillable(),
		field.String("last_error").Optional().Nillable(),
		field.Time("updated_at").Default(time.Now).UpdateDefault(time.Now),
		field.Time("deleted_at").Optional().Nillable(),
		field.Time("created_at").Default(time.Now),
	}
}

func (Upstream) Edges() []ent.Edge {
	return []ent.Edge{
		edge.To("accounts", Account.Type),
		edge.To("group_members", GroupUpstream.Type),
	}
}
