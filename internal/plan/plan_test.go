package plan

import (
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/vt100io/molt/internal/project"
	"github.com/vt100io/molt/internal/shadow"
)

var base = map[string]string{
	"src":    "SELECT 1 AS id, 10.5 AS amount, 'paid' AS status",
	"mid":    "SELECT id, amount * 2 AS doubled, status FROM src WHERE status <> 'cancelled'",
	"top":    "SELECT status, sum(doubled) AS total FROM mid GROUP BY status",
	"ids":    "SELECT id FROM src",
	"other":  "SELECT 42 AS answer",
	"by_ref": "SELECT m.id, s.amount FROM mid m JOIN src s USING (id)",
}

func load(t *testing.T, models map[string]string) (*project.Project, *shadow.Catalog) {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "models"), 0755); err != nil {
		t.Fatal(err)
	}
	for name, sql := range models {
		if err := os.WriteFile(filepath.Join(dir, "models", name+".sql"), []byte(sql), 0644); err != nil {
			t.Fatal(err)
		}
	}
	p, err := project.LoadProject(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := shadow.Build(p, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return p, c
}

// plan builds base, then plans the project with the given models replaced
// (an empty string removes a model).
func plan(t *testing.T, changes map[string]string) *Plan {
	t.Helper()
	built := FromCatalog(load(t, base))
	models := map[string]string{}
	for k, v := range base {
		models[k] = v
	}
	for k, v := range changes {
		if v == "" {
			delete(models, k)
		} else {
			models[k] = v
		}
	}
	p, c := load(t, models)
	return Compare(built, p, c)
}

// summary renders a plan as model → status and changed columns.
func summary(p *Plan) map[string]string {
	out := map[string]string{}
	for _, m := range p.Models {
		if m.Status == Unchanged {
			continue
		}
		s := m.Status
		if m.Breaking {
			s += " breaking"
		}
		if m.RowsChanged {
			s += " rows"
		}
		var cols []string
		for _, c := range m.Columns {
			cols = append(cols, c.Change[:1]+c.Name)
		}
		sort.Strings(cols)
		for _, c := range cols {
			s += " " + c
		}
		out[m.Name] = s
	}
	return out
}

func TestPlan(t *testing.T) {
	tests := []struct {
		name    string
		changes map[string]string
		want    map[string]string
	}{
		{"nothing", nil, map[string]string{}},
		{
			name:    "formatting and comments only",
			changes: map[string]string{"mid": "-- doubled amounts\nselect id,\n  amount*2 as doubled, status\nfrom src where status != 'cancelled';"},
			want:    map[string]string{"main.mid": "modified"},
		},
		{
			name:    "expression change flows to dependent columns only",
			changes: map[string]string{"mid": "SELECT id, amount * 3 AS doubled, status FROM src WHERE status <> 'cancelled'"},
			want: map[string]string{
				"main.mid": "modified breaking mdoubled",
				"main.top": "affected breaking mtotal",
			},
		},
		{
			name:    "added column is non-breaking",
			changes: map[string]string{"mid": "SELECT id, amount * 2 AS doubled, status, amount FROM src WHERE status <> 'cancelled'"},
			want:    map[string]string{"main.mid": "modified aamount"},
		},
		{
			name:    "filter change changes every row downstream",
			changes: map[string]string{"mid": "SELECT id, amount * 2 AS doubled, status FROM src WHERE status = 'paid'"},
			want: map[string]string{
				// Values are unchanged; which rows exist is not.
				"main.mid": "modified breaking rows",
				// Downstream, mid's columns carry its rows in their fingerprints.
				"main.top":    "affected breaking rows mstatus mtotal",
				"main.by_ref": "affected breaking rows mid",
			},
		},
		{
			name:    "upstream source change",
			changes: map[string]string{"src": "SELECT 1 AS id, 11.5 AS amount, 'paid' AS status"},
			want: map[string]string{
				"main.src":    "modified breaking mamount",
				"main.mid":    "affected breaking mdoubled",
				"main.top":    "affected breaking mtotal",
				"main.by_ref": "affected breaking mamount",
			},
		},
		{
			name:    "removed column breaks a downstream model",
			changes: map[string]string{"mid": "SELECT id, status FROM src WHERE status <> 'cancelled'"},
			want: map[string]string{
				"main.mid": "modified breaking rdoubled",
				"main.top": "failed breaking",
			},
		},
		{
			name:    "added and removed models",
			changes: map[string]string{"other": "", "fresh": "SELECT 1 AS x"},
			want: map[string]string{
				"main.other": "removed breaking ranswer",
				"main.fresh": "added ax",
			},
		},
		{
			name:    "kind change only",
			changes: map[string]string{"ids": "-- @kind: view\nSELECT id FROM src"},
			want:    map[string]string{"main.ids": "modified"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := summary(plan(t, tt.changes)); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("plan = %v\nwant   %v", got, tt.want)
			}
		})
	}
}

func TestPlanExplainsAffectedColumns(t *testing.T) {
	p := plan(t, map[string]string{"mid": "SELECT id, amount * 3 AS doubled, status FROM src WHERE status <> 'cancelled'"})
	for _, m := range p.Models {
		if m.Name != "main.top" {
			continue
		}
		if len(m.Columns) != 1 || len(m.Columns[0].Because) != 1 || m.Columns[0].Because[0].String() != "main.mid.doubled" {
			t.Errorf("top columns = %+v, want total because of main.mid.doubled", m.Columns)
		}
		return
	}
	t.Fatal("main.top not in plan")
}
