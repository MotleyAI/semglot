package dialect

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/benchouse/semglot/ir"
)

// cube is an emit-only target writing Cube (cube.dev) data-model YAML: one
// `model/cubes/<table>.yml` per IR table.
//
//   - sql_table is the table's physical source; a query source becomes `sql`.
//   - Every field becomes a dimension; time-typed fields become `type: time`.
//     Column references are qualified as {CUBE}.col so they stay unambiguous
//     once Cube joins tables.
//   - The primary key becomes a `primary_key: true` dimension (Cube needs one
//     per cube for join de-duplication). A composite key becomes one hidden
//     dimension concatenating the key columns.
//   - Each relationship becomes a `many_to_one` join on its Left (many) cube.
//     Cube joins are directed; no reverse join is invented, matching the
//     source's declared direction.
//   - A metric that is one aggregation becomes a measure of that type (with
//     `filters` for a filtered aggregate). Arithmetic over aggregates becomes a
//     `type: number` measure over named measures ({name} or {cube.name}), via
//     hoistInlineAggs. Cumulative and conversion metrics are reported: Cube's
//     rolling_window has no portable mapping from this IR yet.
type cube struct{ opts Options }

func init() { Register(cube{}) }

func (cube) Name() string { return "cube" }

// WithOptions lets the CLI pass the profile's identity fields.
func (cube) WithOptions(o Options) Emitter { return cube{opts: o} }

type cubeFile struct {
	Cubes []cubeDef `yaml:"cubes"`
}

type cubeDef struct {
	Name        string         `yaml:"name"`
	SQLTable    string         `yaml:"sql_table,omitempty"`
	SQL         string         `yaml:"sql,omitempty"`
	Description string         `yaml:"description,omitempty"`
	Joins       []cubeJoin     `yaml:"joins,omitempty"`
	Dimensions  []cubeDim      `yaml:"dimensions,omitempty"`
	Measures    []cubeMeasure  `yaml:"measures,omitempty"`
	Meta        map[string]any `yaml:"meta,omitempty"`
}

type cubeJoin struct {
	Name         string `yaml:"name"`
	SQL          string `yaml:"sql"`
	Relationship string `yaml:"relationship"`
}

type cubeDim struct {
	Name        string         `yaml:"name"`
	SQL         string         `yaml:"sql"`
	Type        string         `yaml:"type"`
	PrimaryKey  bool           `yaml:"primary_key,omitempty"`
	Public      *bool          `yaml:"public,omitempty"`
	Description string         `yaml:"description,omitempty"`
	Meta        map[string]any `yaml:"meta,omitempty"`
}

type cubeMeasure struct {
	Name        string         `yaml:"name"`
	SQL         string         `yaml:"sql,omitempty"`
	Type        string         `yaml:"type"`
	Description string         `yaml:"description,omitempty"`
	Filters     []cubeFilter   `yaml:"filters,omitempty"`
	Public      *bool          `yaml:"public,omitempty"`
	Meta        map[string]any `yaml:"meta,omitempty"`
}

type cubeFilter struct {
	SQL string `yaml:"sql"`
}

func cubeDimType(f ir.Field, isTime bool) string {
	if isTime {
		return "time"
	}
	switch strings.ToLower(f.DataType) {
	case "integer", "int", "bigint", "decimal", "float", "double", "number", "numeric":
		return "number"
	case "boolean", "bool":
		return "boolean"
	case "date", "datetime", "timestamp", "datetimetz", "time":
		return "time"
	}
	return "string"
}

// cubeQualify rewrites an expression owned by table so column references read
// {CUBE}.col (own table) or {other}.col (another table of the model).
func cubeQualify(expr, table string, tables map[string]bool) string {
	if isIdent(expr) {
		return "{CUBE}." + expr
	}
	out := expr
	for t := range tables {
		re := regexp.MustCompile(`(?i)\b` + regexp.QuoteMeta(t) + `\.`)
		if strings.EqualFold(t, table) {
			out = re.ReplaceAllString(out, "{CUBE}.")
		} else {
			out = re.ReplaceAllString(out, "{"+t+"}.")
		}
	}
	return out
}

func cubeAggType(a ir.Agg) (string, bool) {
	switch strings.ToLower(a.Func) {
	case "sum":
		return "sum", true
	case "count":
		return "count", true
	case "count_distinct", "countdistinct":
		return "count_distinct", true
	case "avg", "average", "mean":
		return "avg", true
	case "min":
		return "min", true
	case "max":
		return "max", true
	}
	return "", false
}

func (e cube) Emit(m *ir.Model, dir string) ([]string, error) {
	var warnings []string
	tables := map[string]bool{}
	byName := map[string]ir.Table{}
	for _, t := range m.Tables {
		tables[t.Name] = true
		byName[t.Name] = t
	}
	hoist := hoistInlineAggs(m)
	// Every metric's owning cube, for {cube.measure} references.
	owner := map[string]string{}
	for _, t := range m.Tables {
		for _, mt := range hoist.metricsFor(t) {
			owner[mt.Name] = t.Name
		}
	}
	hidden := false
	// Relationships that reach no cube's joins at all are reported whole.
	for _, r := range m.Relationships {
		switch {
		case !tables[r.Left]:
			warnings = append(warnings, relNotEmittedWarning("cube", r, relEndpointMissing(r.Left,
				"a Cube join is declared on the cube the join leaves")))
		case len(r.Columns) == 0:
			warnings = append(warnings, relNotEmittedWarning("cube", r,
				"it declares no join columns, and a Cube join needs an ON condition"))
		}
	}

	outDir := filepath.Join(dir, "model", "cubes")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	for _, t := range m.Tables {
		c := cubeDef{Name: t.Name, Description: t.Description}
		src := strings.TrimSpace(t.Source)
		switch {
		case src == "":
			c.SQLTable = t.Name
			warnings = append(warnings, fmt.Sprintf("table %q has no physical source; sql_table set to %q", t.Name, t.Name))
		case strings.Contains(strings.ToLower(src), "select "):
			c.SQL = src
		default:
			c.SQLTable = src
		}
		if len(t.Synonyms) > 0 {
			c.Meta = map[string]any{"synonyms": t.Synonyms}
		}

		// Joins: many_to_one from this (many) side.
		for _, r := range m.Relationships {
			if r.Left != t.Name || len(r.Columns) == 0 {
				continue
			}
			if !tables[r.Right] {
				warnings = append(warnings, relNotEmittedWarning("cube", r, relEndpointMissing(r.Right,
					"a Cube join's ON condition references the cube it reaches")))
				continue
			}
			var conds []string
			for _, cp := range r.Columns {
				conds = append(conds, fmt.Sprintf("{CUBE}.%s = {%s}.%s", cp.Left, r.Right, cp.Right))
			}
			c.Joins = append(c.Joins, cubeJoin{Name: r.Right, SQL: strings.Join(conds, " AND "), Relationship: "many_to_one"})
			// A Cube join is named by its target cube; the relationship's own
			// declared name and synonyms have no slot.
			for _, w := range []string{relNameWarning("cube", r), relSynonymsWarning("cube", r.Name, r)} {
				if w != "" {
					warnings = append(warnings, w)
				}
			}
		}

		// Dimensions; mark or synthesise the primary key.
		names := map[string]bool{}
		pkDone := len(t.PrimaryKey) == 0
		addDim := func(f ir.Field, isTime bool) {
			d := cubeDim{Name: f.Name, SQL: cubeQualify(f.Expr, t.Name, tables), Type: cubeDimType(f, isTime), Description: f.Description}
			if len(f.Synonyms) > 0 {
				d.Meta = map[string]any{"synonyms": f.Synonyms}
			}
			if len(t.PrimaryKey) == 1 && strings.EqualFold(f.Expr, t.PrimaryKey[0]) && !pkDone {
				d.PrimaryKey = true
				pkDone = true
			}
			names[f.Name] = true
			c.Dimensions = append(c.Dimensions, d)
		}
		for _, f := range t.Dimensions {
			addDim(f, false)
		}
		for _, f := range t.TimeDimensions {
			addDim(f, true)
		}
		if !pkDone {
			name := "pk"
			if len(t.PrimaryKey) == 1 {
				name = t.PrimaryKey[0]
			}
			for names[name] {
				name = "_" + name
			}
			c.Dimensions = append(c.Dimensions, cubeDim{
				Name: name, SQL: cubeKeyExpr(t.PrimaryKey),
				Type: "string", PrimaryKey: true, Public: &hidden,
			})
			if len(t.PrimaryKey) == 1 {
				c.Dimensions[len(c.Dimensions)-1].Type = "number"
				c.Dimensions[len(c.Dimensions)-1].SQL = "{CUBE}." + t.PrimaryKey[0]
			}
		}

		// Measures from metrics.
		for _, mt := range hoist.metricsFor(t) {
			ms := cubeMeasure{Name: mt.Name, Description: mt.Description}
			if len(mt.Synonyms) > 0 {
				ms.Meta = map[string]any{"synonyms": mt.Synonyms}
			}
			if hoist.synthesisedName(mt.Name) {
				ms.Public = &hidden
			}
			switch def := mt.Def.(type) {
			case ir.Agg:
				typ, ok := cubeAggType(def)
				if !ok {
					warnings = append(warnings, fmt.Sprintf("metric %q not emitted: Cube has no measure type for aggregation %q", mt.Name, def.Func))
					continue
				}
				ms.Type = typ
				switch arg := def.Arg.(type) {
				case nil:
					// COUNT(*): Cube's count needs no sql.
				case ir.Col:
					home := t.Name
					if arg.Table != "" {
						home = arg.Table
					}
					if home != t.Name {
						warnings = append(warnings, fmt.Sprintf("metric %q on cube %q aggregates a column of %q; emitted as {%s}.%s", mt.Name, t.Name, home, home, arg.Name))
						ms.SQL = "{" + home + "}." + arg.Name
					} else {
						ms.SQL = "{CUBE}." + arg.Name
					}
				case ir.Raw:
					ms.SQL = cubeQualify(arg.SQL, t.Name, tables)
				default:
					ms.SQL = cubeQualify(renderSQL(arg, noMetricResolve), t.Name, tables)
				}
				if def.Filter != nil {
					ms.Filters = []cubeFilter{{SQL: cubeQualify(renderSQL(def.Filter, noMetricResolve), t.Name, tables)}}
				}
			case ir.Binary, ir.Ref:
				sql, ok := cubeArith(def, t.Name, owner)
				if !ok {
					warnings = append(warnings, fmt.Sprintf("metric %q not emitted: its arithmetic still inlines an expression that is not a named measure: %s", mt.Name, renderSQL(def, noMetricResolve)))
					continue
				}
				ms.Type = "number"
				ms.SQL = sql
			default:
				warnings = append(warnings, fmt.Sprintf("metric %q not emitted: cumulative/conversion metrics have no Cube mapping in this emitter", mt.Name))
				continue
			}
			c.Measures = append(c.Measures, ms)
		}

		if err := writeDBTFile(cubeFile{Cubes: []cubeDef{c}}, outDir, t.Name+".yml"); err != nil {
			return warnings, err
		}
	}
	return warnings, nil
}

// cubeArith renders a derived-metric tree as Cube number-measure SQL over named
// measures: {name} for the same cube, {cube.name} for another.
func cubeArith(e ir.Expr, table string, owner map[string]string) (string, bool) {
	switch n := e.(type) {
	case ir.Ref:
		home, ok := owner[n.Metric]
		if !ok {
			return "", false
		}
		if home == table {
			return "{" + n.Metric + "}", true
		}
		return "{" + home + "." + n.Metric + "}", true
	case ir.Lit:
		return n.Value, true
	case ir.Binary:
		l, lok := cubeArith(n.Left, table, owner)
		r, rok := cubeArith(n.Right, table, owner)
		if !lok || !rok {
			return "", false
		}
		if _, ok := n.Left.(ir.Binary); ok {
			l = "(" + l + ")"
		}
		if _, ok := n.Right.(ir.Binary); ok {
			r = "(" + r + ")"
		}
		return l + " " + n.Op + " " + r, true
	}
	return "", false
}

// cubeKeyExpr renders a composite key as one {CUBE}-qualified concatenation.
func cubeKeyExpr(cols []string) string {
	parts := make([]string, len(cols))
	for i, c := range cols {
		parts[i] = "CAST({CUBE}." + c + " AS VARCHAR)"
	}
	return strings.Join(parts, " || '|' || ")
}
