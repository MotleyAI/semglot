package dialect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/benchouse/semglot/ir"
	"gopkg.in/yaml.v3"
)

func emitSlayer(t *testing.T, m *ir.Model) (dir string, warnings []string) {
	t.Helper()
	dir = t.TempDir()
	w, err := (slayerDialect{Database: "ANALYTICS", Schema: "MAIN", DataSource: "warehouse"}).Emit(m, dir)
	if err != nil {
		t.Fatalf("Emit: %v", err)
	}
	return dir, w
}

func slayerFixture() *ir.Model {
	return &ir.Model{
		Tables: []ir.Table{
			{
				Name: "fct_orders", Description: "Orders.", Synonyms: []string{"purchases"},
				PrimaryKey: []string{"order_id"},
				Grain:      "order_date",
				Dimensions: []ir.Field{
					{Name: "order_id", Expr: "order_id", DataType: "number"},
					{Name: "status", Expr: "status", Enum: []ir.EnumValue{{Value: "open"}, {Value: "closed", Description: "Fulfilled"}}},
					{Name: "is_refunded", Expr: "is_refunded", DataType: "boolean", Synonyms: []string{"refund flag"}},
				},
				TimeDimensions: []ir.Field{{Name: "order_date", Expr: "ordered_at", DataType: "date"}},
				Measures: []ir.Measure{
					{Field: ir.Field{Name: "order_net_booked_amount", Expr: "order_net_booked", DataType: "float"}, Agg: "sum"},
					{Field: ir.Field{Name: "refund_flag_value", Expr: "case when is_refunded then 1 else 0 end", DataType: "float"}, Agg: "sum"},
					// Named after its own backing column: columns and measures
					// share a namespace in slayer, so this must rename.
					{Field: ir.Field{Name: "order_net_booked", Expr: "order_net_booked", DataType: "float"}, Agg: "avg"},
				},
				Metrics: []ir.Metric{
					{Name: "net_revenue", Label: "Net revenue", Description: "Net booked revenue.",
						Def: ir.Agg{Func: "sum", Table: "fct_orders", Arg: ir.Col{Table: "fct_orders", Name: "order_net_booked"}}},
					{Name: "orders", Def: ir.Agg{Func: "count", Table: "fct_orders"}},
					{Name: "large_orders", Def: ir.Agg{Func: "count", Table: "fct_orders", Arg: ir.Col{Table: "fct_orders", Name: "order_id"},
						Filter: ir.Binary{Op: ">", Left: ir.Col{Table: "fct_orders", Name: "order_net_booked"}, Right: ir.Lit{Value: "100"}}}},
					{Name: "refund_rate", Label: "Refund rate",
						Def: ir.Binary{Op: "/", Left: ir.Ref{Metric: "refund_flag_metric"}, Right: ir.Ref{Metric: "orders"}}},
					{Name: "refund_flag_metric",
						Def: ir.Agg{Func: "sum", Table: "fct_orders", Arg: ir.Raw{SQL: "case when is_refunded then 1 else 0 end", Columns: []string{"is_refunded"}}}},
					{Name: "cumulative_revenue", Def: ir.Window{Base: ir.Ref{Metric: "net_revenue"}, Grain: "month"}},
					{Name: "rolling_revenue", Def: ir.Window{Base: ir.Ref{Metric: "net_revenue"}, Window: "30 days"}},
					{Name: "first", Def: ir.Agg{Func: "min", Table: "fct_orders", Arg: ir.Col{Table: "fct_orders", Name: "order_date"}}},
					{Name: "restricted", Grain: "week", Dimensions: []string{"status"},
						Def: ir.Agg{Func: "avg", Table: "fct_orders", Arg: ir.Col{Table: "fct_orders", Name: "order_net_booked"}}},
				},
			},
			{
				Name: "dim_customer", Source: "PROD.CRM.customers", Description: "Customers.",
				PrimaryKey: []string{"customer_sk"},
				Dimensions: []ir.Field{{Name: "customer_sk", Expr: "customer_sk", DataType: "number"}},
			},
			{
				Name: "stg_events", Source: "SELECT * FROM raw.events WHERE kind = 'web'",
				Dimensions: []ir.Field{{Name: "event_id", Expr: "event_id"}},
			},
		},
		Relationships: []ir.Relationship{
			{Name: "order_customer", Synonyms: []string{"who ordered"},
				Left: "fct_orders", Right: "dim_customer",
				Columns: []ir.ColumnPair{{Left: "customer_sk", Right: "customer_sk"}}},
			{Left: "fct_missing", Right: "dim_customer",
				Columns: []ir.ColumnPair{{Left: "x", Right: "x"}}},
		},
	}
}

func TestSlayerEmit(t *testing.T) {
	dir, warnings := emitSlayer(t, slayerFixture())

	orders := readFile(t, filepath.Join(dir, "fct_orders.yaml"))
	for _, want := range []string{
		"version: 8",
		"name: fct_orders",
		"sql_table: ANALYTICS.MAIN.fct_orders", // no declared source -> profile reconstruction
		"data_source: warehouse",
		"default_time_dimension: order_date",
		"description: Orders.",
		"synonyms:", // table meta.synonyms
		"- purchases",
		"primary_key: true",
		"sql: ordered_at", // time dim with expr != name
		"type: DATE",
		"type: BOOLEAN",
		"Values: open; closed = Fulfilled.", // enum folded into description
		// identifier-backed measure: column + named measure over it
		"formula: order_net_booked:sum",
		"name: order_net_booked_amount",
		"type: DOUBLE",
		// compound measure expr -> synthesized hidden backing column
		"sql: case when is_refunded then 1 else 0 end",
		"hidden: true",
		"formula: refund_flag_value_expr:sum",
		// metrics
		"name: net_revenue",
		"label: Net revenue",
		"formula: '*:count'",
		"name: orders",
		// filtered aggregation -> hidden column with filter:
		"filter: order_net_booked > 100",
		"formula: large_orders_filtered:count",
		// ratio over refs -> bare measure-name arithmetic
		"formula: refund_flag_metric / orders",
		// unbounded window -> cumsum
		"formula: cumsum(net_revenue)",
		"name: cumulative_revenue",
		// reserved transform name renamed
		"name: first_measure",
		// measure named after its own backing column renamed
		"name: order_net_booked_measure",
		// join on the FK-holding model, with identity folded into description
		"target_model: dim_customer",
		"- - customer_sk",
		"join_type: left",
		"description: 'Join name: order_customer. Synonyms: who ordered.'",
	} {
		if !strings.Contains(orders, want) {
			t.Fatalf("fct_orders.yaml missing %q:\n%s", want, orders)
		}
	}

	customers := readFile(t, filepath.Join(dir, "dim_customer.yaml"))
	if !strings.Contains(customers, "sql_table: PROD.CRM.customers") {
		t.Fatalf("dim_customer.yaml should carry the declared source verbatim:\n%s", customers)
	}

	events := readFile(t, filepath.Join(dir, "stg_events.yaml"))
	if !strings.Contains(events, "sql: SELECT * FROM raw.events WHERE kind = 'web'") ||
		strings.Contains(events, "sql_table:") {
		t.Fatalf("stg_events.yaml should be a query-backed model (sql:, no sql_table:):\n%s", events)
	}

	notes := readFile(t, filepath.Join(dir, "NOTES.md"))
	for _, want := range []string{
		`metric "rolling_revenue" not emitted: a bounded "30 days" window is not expressible in slayer`,
		`metric "first": renamed to "first_measure"`,
		`measure "order_net_booked": renamed to "order_net_booked_measure"`,
		`metric "restricted": per-metric agg-time grain "week" not emitted`,
		`metric "restricted": slice-by dimension restriction [status] not emitted`,
		`relationship fct_missing -> dim_customer: not emitted to slayer at all`,
	} {
		if !strings.Contains(notes, want) {
			t.Fatalf("NOTES.md missing %q:\n%s", want, notes)
		}
	}
	if len(warnings) == 0 {
		t.Fatal("expected degrade warnings")
	}
}

// TestSlayerEmitValidYAML asserts every emitted model file decodes and carries
// the required SlayerModel fields with sane types.
func TestSlayerEmitValidYAML(t *testing.T) {
	dir, _ := emitSlayer(t, slayerFixture())
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, ent := range entries {
		if ent.Name() == "NOTES.md" {
			continue
		}
		var doc map[string]any
		b, err := os.ReadFile(filepath.Join(dir, ent.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := yaml.Unmarshal(b, &doc); err != nil {
			t.Fatalf("%s: invalid YAML: %v", ent.Name(), err)
		}
		if doc["version"] != 8 {
			t.Fatalf("%s: version = %v, want 8", ent.Name(), doc["version"])
		}
		if _, ok := doc["name"].(string); !ok {
			t.Fatalf("%s: missing name", ent.Name())
		}
		if _, ok := doc["columns"].([]any); !ok {
			t.Fatalf("%s: missing columns list", ent.Name())
		}
	}
}

func TestSlayerZeroValueUsable(t *testing.T) {
	m := &ir.Model{Tables: []ir.Table{{
		Name:       "t",
		Dimensions: []ir.Field{{Name: "id", Expr: "id"}},
	}}}
	dir := t.TempDir()
	if _, err := (slayerDialect{}).Emit(m, dir); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	got := readFile(t, filepath.Join(dir, "t.yaml"))
	if !strings.Contains(got, "sql_table: t") {
		t.Fatalf("zero-value emitter should fall back to the bare table name:\n%s", got)
	}
	if !strings.Contains(got, `data_source: ""`) {
		t.Fatalf("data_source must be present (empty) for SlayerModel defaults:\n%s", got)
	}
}
