package build

import (
	"fmt"
	"strings"

	"github.com/vt100io/molt/internal/project"
)

// TopoSort returns the model names in topological order (dependencies first).
// Ties are broken by name, so the order is deterministic.
func TopoSort(p *project.Project) ([]string, error) {
	var order []string
	visited := make(map[string]bool)
	inProgress := make(map[string]bool)

	var visit func(name string) error
	visit = func(name string) error {
		if inProgress[name] {
			return fmt.Errorf("circular dependency detected involving %s", name)
		}
		if visited[name] {
			return nil
		}
		inProgress[name] = true

		m, ok := p.Models[name]
		if !ok {
			return fmt.Errorf("model %s not found", name)
		}

		// Dependencies are sorted, so visiting them in order keeps the result deterministic.
		for _, dep := range m.Dependencies {
			if err := visit(dep); err != nil {
				return err
			}
		}

		inProgress[name] = false
		visited[name] = true
		order = append(order, name)
		return nil
	}

	for _, name := range p.ModelNames() {
		if err := visit(name); err != nil {
			return nil, err
		}
	}

	return order, nil
}

// Select returns the models matched by selectors, in topological order.
// A selector is a model name (selects that model) or a model name followed by
// "+" (selects that model and everything downstream of it). No selectors
// selects every model.
func Select(p *project.Project, selectors []string) ([]string, error) {
	order, err := TopoSort(p)
	if err != nil {
		return nil, err
	}
	if len(selectors) == 0 {
		return order, nil
	}

	selected := make(map[string]bool)
	for _, sel := range selectors {
		name, downstream := strings.CutSuffix(sel, "+")
		m, err := p.Resolve(name)
		if err != nil {
			return nil, err
		}
		selected[m.Name] = true
		if !downstream {
			continue
		}
		// In topological order every model comes after its dependencies, so one
		// pass finds everything that depends on m, directly or transitively.
		reached := map[string]bool{m.Name: true}
		for _, n := range order {
			for _, dep := range p.Models[n].Dependencies {
				if reached[dep] {
					reached[n] = true
					selected[n] = true
					break
				}
			}
		}
	}

	var result []string
	for _, n := range order {
		if selected[n] {
			result = append(result, n)
		}
	}
	return result, nil
}
