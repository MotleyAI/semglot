package dialect

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/benchouse/semglot/ir"
	"gopkg.in/yaml.v3"
)

func init() { Register(slayerDialect{}) }

// slayerDialect emits one SLayer model YAML per table — the file shape
// `slayer models create <file>` reads and `slayer models show` prints
// (github.com/MotleyAI/slayer, SlayerModel schema version 8).
//
// SLayer chooses aggregations at query time (`revenue:sum` colon syntax), so
// the IR maps unusually directly: a dimension is a Column, a measure is a
// Column plus a named ModelMeasure `<col>:<agg>`, and a metric lowers to
// SLayer's formula DSL — arithmetic over colon references, bare references to
// other measures, and `cumsum(...)` for unbounded cumulative windows. A
// filtered aggregation becomes a synthesized hidden Column carrying SLayer's
// `filter:` (applied inside CASE WHEN at aggregation time), which is exactly
// the filtered-aggregate semantics.
//
// Zero value is usable; the build command sets identity from the profile.
type slayerDialect struct {
	// Database and Schema reconstruct a physical address for a table that
	// declares no Source of its own (dbt models resolve theirs through ref()).
	Database, Schema string
	// DataSource is the SLayer datasource name the emitted models are keyed
	// under ((data_source, name) is a model's identity in SLayer). Taken from
	// the profile's model-name option; "" is valid — `slayer import` flows
	// assign the datasource at import time.
	DataSource string
}

func (slayerDialect) Name() string { return "slayer" }

// WithOptions lets the CLI pass the profile's identity fields.
func (slayerDialect) WithOptions(o Options) Emitter {
	return slayerDialect{Database: o.Database, Schema: o.Schema, DataSource: o.Name}
}

// slayerVersion is the SlayerModel schema version emitted. Pinned, never computed.
const slayerVersion = 8

// ---- SLayer YAML shapes (SlayerModel, slayer/core/models.py) ----

type slModel struct {
	Version              int            `yaml:"version"`
	Name                 string         `yaml:"name"`
	SQLTable             string         `yaml:"sql_table,omitempty"`
	SQL                  string         `yaml:"sql,omitempty"`
	DataSource           string         `yaml:"data_source"`
	Columns              []slColumn     `yaml:"columns"`
	Measures             []slMeasure    `yaml:"measures,omitempty"`
	Joins                []slJoin       `yaml:"joins,omitempty"`
	DefaultTimeDimension string         `yaml:"default_time_dimension,omitempty"`
	Description          string         `yaml:"description,omitempty"`
	Meta                 map[string]any `yaml:"meta,omitempty"`
}

type slColumn struct {
	Name        string         `yaml:"name"`
	SQL         string         `yaml:"sql,omitempty"`
	Type        string         `yaml:"type,omitempty"`
	PrimaryKey  bool           `yaml:"primary_key,omitempty"`
	Description string         `yaml:"description,omitempty"`
	Hidden      bool           `yaml:"hidden,omitempty"`
	Filter      string         `yaml:"filter,omitempty"`
	Meta        map[string]any `yaml:"meta,omitempty"`
}

type slMeasure struct {
	Formula     string `yaml:"formula"`
	Name        string `yaml:"name"`
	Label       string `yaml:"label,omitempty"`
	Description string `yaml:"description,omitempty"`
	Type        string `yaml:"type,omitempty"`
}

type slJoin struct {
	TargetModel string     `yaml:"target_model"`
	JoinPairs   [][]string `yaml:"join_pairs"`
	JoinType    string     `yaml:"join_type"`
	Description string     `yaml:"description,omitempty"`
}

// slayerAggs is SLayer's built-in aggregation vocabulary
// (slayer/core/enums.py BUILTIN_AGGREGATIONS). A source aggregation outside it
// degrades loudly rather than emitting a formula SLayer would reject.
var slayerAggs = map[string]bool{
	"sum": true, "avg": true, "min": true, "max": true,
	"count": true, "count_distinct": true, "count_distinct_approx": true,
	"first": true, "last": true, "weighted_avg": true,
	"median": true, "percentile": true,
	"stddev_samp": true, "stddev_pop": true, "var_samp": true, "var_pop": true,
	"corr": true, "covar_samp": true, "covar_pop": true,
}

// slayerReservedMeasureNames are SLayer transform names a saved measure must
// not shadow (rejected at model construction; docs/concepts/formulas.md).
var slayerReservedMeasureNames = map[string]bool{
	"cumsum": true, "change": true, "time_shift": true, "lag": true,
	"lead": true, "rank": true, "percent_rank": true, "dense_rank": true,
	"ntile": true, "first": true, "last": true, "change_pct": true,
	"consecutive_periods": true,
}

// slayerType maps an IR/dbt data type string to SLayer's DataType vocabulary
// (TEXT/INT/DOUBLE/BOOLEAN/DATE/TIMESTAMP — sqlglot type names). An empty
// source type falls back by role: TIMESTAMP for a time dimension, TEXT
// otherwise (TEXT stays groupable, SLayer's own lenient default).
func slayerType(t string, isTime bool) string {
	switch strings.ToLower(strings.TrimSpace(t)) {
	case "int", "integer", "bigint", "smallint", "tinyint":
		return "INT"
	case "number", "float", "double", "double precision", "real", "numeric", "decimal":
		return "DOUBLE"
	case "boolean", "bool":
		return "BOOLEAN"
	case "date":
		return "DATE"
	case "timestamp", "datetime", "time", "timestamp_ntz", "timestamp_tz", "timestamp_ltz":
		return "TIMESTAMP"
	case "":
		if isTime {
			return "TIMESTAMP"
		}
		return "TEXT"
	default:
		return "TEXT"
	}
}

// synonymMeta returns the meta map carrying synonyms, or nil when there are
// none. SLayer's free-form meta dict is the structural home for them — the
// same meta.synonyms convention the IR sources them from in dbt.
func synonymMeta(syn []string) map[string]any {
	if len(syn) == 0 {
		return nil
	}
	return map[string]any{"synonyms": append([]string(nil), syn...)}
}

// unqualifyExpr strips table qualification from Col references owned by table,
// for rendering into SLayer's Mode-A SQL slots (Column.sql / Column.filter),
// where a same-model column is referenced bare (dots there denote join paths,
// not table qualifiers).
func unqualifyExpr(e ir.Expr, table string) ir.Expr {
	switch n := e.(type) {
	case ir.Col:
		if n.Table == table {
			return ir.Col{Name: n.Name}
		}
		return n
	case ir.Binary:
		return ir.Binary{Op: n.Op, Left: unqualifyExpr(n.Left, table), Right: unqualifyExpr(n.Right, table)}
	case ir.Agg:
		out := n
		if n.Arg != nil {
			out.Arg = unqualifyExpr(n.Arg, table)
		}
		if n.Filter != nil {
			out.Filter = unqualifyExpr(n.Filter, table)
		}
		return out
	default:
		return e
	}
}

// Emit does not mutate m; it reads m.Notes and accumulates its own degrade
// notes locally before writing the combined text to NOTES.md.
func (s slayerDialect) Emit(m *ir.Model, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	var degradeNotes []string
	resolve := metricResolver(m)

	models := map[string]bool{}
	for _, t := range m.Tables {
		models[t.Name] = true
	}
	// A SLayer join is written on the model that holds the foreign key (the
	// Left endpoint), from inside the table loop below, so a relationship
	// whose Left endpoint names no table is iterated by nothing and must be
	// reported here instead.
	for _, r := range m.Relationships {
		if !models[r.Left] {
			degradeNotes = append(degradeNotes, relNotEmittedWarning("slayer", r, relEndpointMissing(r.Left,
				"a slayer join is written on the model that holds the foreign key")))
		}
	}

	// Measure names must be project-unique for cross-table bare references to
	// resolve; rename ones SLayer would reject up front — a name that shadows
	// a transform, or one that collides with a column on the owning model
	// (columns and measures share one namespace per SLayer model, enforced at
	// model construction) — so Ref lowering below can target the emitted name.
	tableColumns := map[string]map[string]bool{}
	for _, t := range m.Tables {
		cols := map[string]bool{}
		for _, d := range t.Dimensions {
			cols[d.Name] = true
		}
		for _, d := range t.TimeDimensions {
			cols[d.Name] = true
		}
		for _, meas := range t.Measures {
			if isIdent(meas.Expr) {
				cols[meas.Expr] = true
			}
		}
		tableColumns[t.Name] = cols
	}
	// renameMeasure resolves what SLayer will accept name as on table owner,
	// with a degrade note explaining each forced rename.
	renameMeasure := func(kind, name, owner string) string {
		out := name
		if slayerReservedMeasureNames[strings.ToLower(out)] {
			out += "_measure"
			degradeNotes = append(degradeNotes, fmt.Sprintf(
				"%s %q: renamed to %q; its name shadows a slayer transform, which slayer rejects on a saved measure", kind, name, out))
		}
		for tableColumns[owner][out] {
			renamed := out + "_measure"
			degradeNotes = append(degradeNotes, fmt.Sprintf(
				"%s %q: renamed to %q; columns and measures share one namespace in a slayer model, and %q names a column on %q", kind, name, renamed, out, owner))
			out = renamed
		}
		return out
	}
	measureName := map[string]string{}
	metricOwner := map[string]string{}
	for _, t := range m.Tables {
		for _, mt := range t.Metrics {
			measureName[mt.Name] = renameMeasure("metric", mt.Name, t.Name)
			metricOwner[mt.Name] = t.Name
		}
	}

	for _, t := range m.Tables {
		model := slModel{
			Version:              slayerVersion,
			Name:                 t.Name,
			DataSource:           s.DataSource,
			Description:          t.Description,
			Meta:                 synonymMeta(t.Synonyms),
			DefaultTimeDimension: t.Grain,
		}

		// Prefer the source dialect's own declared physical address. SLayer's
		// sql_table holds one opaque dotted reference, so a genuine table
		// reference is used verbatim regardless of its dot-part count. A
		// QUERY source (which the OSI spec permits) has a first-class home
		// here that most targets lack: SLayer's query-backed models (`sql:`).
		switch {
		case t.Source != "" && looksLikeQuery(t.Source):
			model.SQL = t.Source
		case t.Source != "":
			model.SQLTable = t.Source
		default:
			parts := []string{}
			if s.Database != "" {
				parts = append(parts, s.Database)
			}
			if s.Schema != "" {
				parts = append(parts, s.Schema)
			}
			model.SQLTable = strings.Join(append(parts, t.Name), ".")
		}

		pk := map[string]bool{}
		for _, k := range t.PrimaryKey {
			pk[k] = true
		}
		colIndex := map[string]int{} // column name -> index in model.Columns
		addColumn := func(c slColumn) {
			if _, ok := colIndex[c.Name]; ok {
				return
			}
			colIndex[c.Name] = len(model.Columns)
			model.Columns = append(model.Columns, c)
		}
		fieldColumn := func(f ir.Field, isTime bool) slColumn {
			c := slColumn{
				Name:        f.Name,
				Type:        slayerType(f.DataType, isTime),
				PrimaryKey:  pk[f.Name] || pk[f.Expr],
				Description: appendClause(f.Description, enumClause(f.Enum)),
				Meta:        synonymMeta(f.Synonyms),
			}
			if f.Expr != "" && f.Expr != f.Name {
				c.SQL = f.Expr
			}
			return c
		}
		for _, d := range t.Dimensions {
			addColumn(fieldColumn(d, false))
		}
		for _, d := range t.TimeDimensions {
			addColumn(fieldColumn(d, true))
		}

		// mintColumn synthesizes a hidden expression-backed column (for a
		// compound or filtered aggregation arg) and returns its name. Two
		// mints with the same SQL and filter share one column — a dbt measure
		// and a metric over the same CASE expression are common — and a
		// synthesized column always aggregates, so an undeclared type
		// defaults to DOUBLE (TEXT would drive a wrong CAST at query time).
		minted := map[[2]string]string{}
		mintColumn := func(base string, c slColumn) string {
			sig := [2]string{c.SQL, c.Filter}
			if name, ok := minted[sig]; ok {
				return name
			}
			name := base
			for {
				if _, taken := colIndex[name]; !taken {
					break
				}
				name += "_expr"
			}
			c.Name = name
			c.Hidden = true
			if c.Type == "" {
				c.Type = "DOUBLE"
			}
			addColumn(c)
			minted[sig] = name
			return name
		}

		// IR measures: the backing column plus a named ModelMeasure over it.
		for _, meas := range t.Measures {
			agg := mapAgg(meas.Agg)
			if !slayerAggs[agg] {
				degradeNotes = append(degradeNotes, fmt.Sprintf(
					"measure %q not emitted: aggregation %q is not a slayer built-in aggregation", meas.Name, meas.Agg))
				continue
			}
			expr := meas.Expr
			if expr == "" {
				expr = meas.Name
			}
			var col string
			if isIdent(expr) {
				col = expr
				addColumn(fieldColumn(ir.Field{Name: expr, Expr: expr, DataType: meas.DataType, Description: meas.Description}, false))
			} else {
				typ := ""
				if meas.DataType != "" {
					typ = slayerType(meas.DataType, false)
				}
				col = mintColumn(meas.Name+"_expr", slColumn{SQL: expr, Type: typ})
			}
			name := renameMeasure("measure", meas.Name, t.Name)
			model.Measures = append(model.Measures, slMeasure{
				Formula:     col + ":" + agg,
				Name:        name,
				Description: appendClause(meas.Description, synonymClause(meas.Synonyms)),
				Type:        measureResultType(meas.DataType, agg),
			})
		}

		// Metrics lower to SLayer's formula DSL.
		for _, mt := range t.Metrics {
			formula, ok := s.lowerFormula(mt.Def, t.Name, measureName, metricOwner, resolve, &model, mintColumn, &degradeNotes, mt.Name)
			if !ok {
				continue
			}
			if mt.Grain != "" && mt.Grain != t.Grain {
				degradeNotes = append(degradeNotes, fmt.Sprintf(
					"metric %q: per-metric agg-time grain %q not emitted; slayer carries only a model-level default_time_dimension", mt.Name, mt.Grain))
			}
			if mt.Dimensions != nil {
				degradeNotes = append(degradeNotes, fmt.Sprintf(
					"metric %q: slice-by dimension restriction %v not emitted; slayer measures group by any dimension at query time", mt.Name, mt.Dimensions))
			}
			model.Measures = append(model.Measures, slMeasure{
				Formula:     formula,
				Name:        measureName[mt.Name],
				Label:       mt.Label,
				Description: appendClause(mt.Description, synonymClause(mt.Synonyms)),
			})
		}

		// Joins live on the FK-holding (Left) model. The join's declared name
		// and synonyms have no structural slot on a SLayer join, but its
		// description is a real prose home, so they fold there rather than
		// warn (the nao-yaml precedent). SLayer permits at most ONE join per
		// target model (its engine rejects a duplicate target with different
		// join_pairs), so a role-playing dimension keeps its first FK and
		// reports the rest — there is no alias construct to disambiguate with.
		joinTarget := map[string]bool{}
		for _, r := range m.Relationships {
			if r.Left != t.Name {
				continue
			}
			if joinTarget[r.Right] {
				degradeNotes = append(degradeNotes, relNotEmittedWarning("slayer", r, fmt.Sprintf(
					"model %q already joins %q, and a slayer model can declare at most one join per target model", t.Name, r.Right)))
				continue
			}
			if !relHasColumns(r) {
				degradeNotes = append(degradeNotes, relNotEmittedWarning("slayer", r,
					"it declares no column pairs, and a slayer join requires join_pairs"))
				continue
			}
			joinTarget[r.Right] = true
			pairs := make([][]string, len(r.Columns))
			for i, cp := range r.Columns {
				pairs[i] = []string{cp.Left, cp.Right}
			}
			model.Joins = append(model.Joins, slJoin{
				TargetModel: r.Right,
				JoinPairs:   pairs,
				JoinType:    "left",
				Description: relIdentityClause(r),
			})
		}

		// default_time_dimension must name a column on the model.
		if model.DefaultTimeDimension != "" {
			if _, ok := colIndex[model.DefaultTimeDimension]; !ok {
				degradeNotes = append(degradeNotes, fmt.Sprintf(
					"table %q: default time dimension %q not emitted; it names no column on the model", t.Name, model.DefaultTimeDimension))
				model.DefaultTimeDimension = ""
			}
		}

		var buf bytes.Buffer
		enc := yaml.NewEncoder(&buf)
		enc.SetIndent(2)
		if err := enc.Encode(model); err != nil {
			return degradeNotes, err
		}
		if err := enc.Close(); err != nil {
			return degradeNotes, err
		}
		if err := os.WriteFile(filepath.Join(dir, t.Name+".yaml"), buf.Bytes(), 0o644); err != nil {
			return degradeNotes, err
		}
	}

	allNotes := append(slices.Clone(m.Notes), degradeNotes...)
	if len(allNotes) > 0 {
		sort.Strings(allNotes)
		var sb strings.Builder
		sb.WriteString("# Not transpiled to slayer\n\n")
		for _, n := range allNotes {
			sb.WriteString("- " + n + "\n")
		}
		if err := os.WriteFile(filepath.Join(dir, "NOTES.md"), []byte(sb.String()), 0o644); err != nil {
			return degradeNotes, err
		}
	}
	return degradeNotes, nil
}

// measureResultType maps a measure's declared type through its aggregation:
// counts are INT regardless of input; otherwise the input type maps through
// slayerType, left empty (SLayer infers) when the source declared none.
func measureResultType(dataType, agg string) string {
	if agg == "count" || agg == "count_distinct" || agg == "count_distinct_approx" {
		return "INT"
	}
	if dataType == "" {
		return ""
	}
	return slayerType(dataType, false)
}

// lowerFormula lowers a metric-definition AST to SLayer's formula DSL:
// colon-aggregation references (`revenue:sum`, `*:count`, dotted for a joined
// model), bare references to other saved measures, arithmetic, and cumsum()
// for an unbounded cumulative window. Compound and filtered aggregation args
// synthesize hidden backing columns via mint. Returns ok=false (with a
// degrade note) for a definition SLayer cannot express.
func (s slayerDialect) lowerFormula(e ir.Expr, table string, measureName map[string]string,
	metricOwner map[string]string, resolve func(string) (ir.Expr, bool), model *slModel,
	mint func(string, slColumn) string, notes *[]string, metric string) (string, bool) {
	switch n := e.(type) {
	case ir.Lit:
		return n.Value, true
	case ir.Ref:
		// A bare measure name only resolves against the OWNING model's saved
		// measures, so it is emitted only for a same-model metric. A ref to a
		// metric on another model (or one outside the model) inlines its
		// definition instead: the inlined Aggs qualify their columns with the
		// owning table, which the Agg case below lowers to SLayer's dotted
		// join-path syntax.
		if name, ok := measureName[n.Metric]; ok && metricOwner[n.Metric] == table {
			return name, true
		}
		if def, ok := resolve(n.Metric); ok {
			return s.lowerFormula(def, table, measureName, metricOwner, resolve, model, mint, notes, metric)
		}
		*notes = append(*notes, fmt.Sprintf("metric %q not emitted: it references unknown metric %q", metric, n.Metric))
		return "", false
	case ir.Agg:
		agg := mapAgg(n.Func)
		if !slayerAggs[agg] {
			*notes = append(*notes, fmt.Sprintf("metric %q not emitted: aggregation %q is not a slayer built-in aggregation", metric, n.Func))
			return "", false
		}
		crossTable := n.Table != "" && n.Table != table
		var col string
		switch a := n.Arg.(type) {
		case nil:
			if agg != "count" {
				*notes = append(*notes, fmt.Sprintf("metric %q not emitted: %s(*) is not expressible in slayer", metric, agg))
				return "", false
			}
			if crossTable {
				*notes = append(*notes, fmt.Sprintf("metric %q not emitted: count(*) over joined model %q is not expressible in slayer", metric, n.Table))
				return "", false
			}
			if n.Filter == nil {
				return "*:count", true
			}
			// Filtered COUNT(*): count a synthesized always-1 column instead,
			// so the filter has a column to hang off.
			col = mint(metric+"_base", slColumn{SQL: "1", Type: "INT"})
		case ir.Col:
			if a.Table != "" && a.Table != table {
				// A joined model's column: SLayer's dotted join-path syntax,
				// resolvable when the emitted join graph connects the models.
				if n.Filter != nil {
					*notes = append(*notes, fmt.Sprintf("metric %q not emitted: a filtered aggregation over joined column %s.%s is not expressible in slayer", metric, a.Table, a.Name))
					return "", false
				}
				return a.Table + "." + a.Name + ":" + agg, true
			}
			col = a.Name
		case ir.Raw:
			// A raw fragment references its owning table's columns bare, so a
			// column synthesized from it is only correct on that same model.
			if crossTable {
				*notes = append(*notes, fmt.Sprintf("metric %q not emitted: an expression aggregation over joined model %q is not expressible in slayer", metric, n.Table))
				return "", false
			}
			col = mint(metric+"_expr", slColumn{SQL: a.SQL, Type: "DOUBLE"})
		default:
			*notes = append(*notes, fmt.Sprintf("metric %q not emitted: aggregation argument is not a column or expression", metric))
			return "", false
		}
		if n.Filter != nil {
			// SLayer's Column.filter applies inside CASE WHEN at aggregation
			// time — exactly a filtered aggregate. Synthesize a hidden column
			// carrying the base expression and the predicate.
			base := model.columnSQL(col)
			cond := renderSQL(unqualifyExpr(n.Filter, table), resolve)
			col = mint(metric+"_filtered", slColumn{SQL: base, Type: "DOUBLE", Filter: cond})
		}
		return col + ":" + agg, true
	case ir.Binary:
		l, ok := s.lowerFormula(n.Left, table, measureName, metricOwner, resolve, model, mint, notes, metric)
		if !ok {
			return "", false
		}
		r, ok := s.lowerFormula(n.Right, table, measureName, metricOwner, resolve, model, mint, notes, metric)
		if !ok {
			return "", false
		}
		if _, c := n.Left.(ir.Binary); c {
			l = "(" + l + ")"
		}
		if _, c := n.Right.(ir.Binary); c {
			r = "(" + r + ")"
		}
		return l + " " + n.Op + " " + r, true
	case ir.Window:
		if n.Window != "" {
			*notes = append(*notes, fmt.Sprintf("metric %q not emitted: a bounded %q window is not expressible in slayer", metric, n.Window))
			return "", false
		}
		base, ok := s.lowerFormula(n.Base, table, measureName, metricOwner, resolve, model, mint, notes, metric)
		if !ok {
			return "", false
		}
		return "cumsum(" + base + ")", true
	case ir.Conversion:
		*notes = append(*notes, fmt.Sprintf("metric %q not emitted: conversion/funnel metric is not expressible in slayer", metric))
		return "", false
	default:
		*notes = append(*notes, fmt.Sprintf("metric %q not emitted: definition is not expressible in slayer", metric))
		return "", false
	}
}

// columnSQL returns the SQL expression behind an emitted column name (the
// column's own name when it is a plain physical column), for re-wrapping into
// a synthesized filtered column.
func (m *slModel) columnSQL(name string) string {
	for _, c := range m.Columns {
		if c.Name == name && c.SQL != "" {
			return c.SQL
		}
	}
	return name
}
