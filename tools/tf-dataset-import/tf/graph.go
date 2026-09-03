package tf

import "sort"

// Graph is the dataset dependency graph read out of state: an edge runs from an input dataset to
// the dataset consuming it.
type Graph struct {
	// Dependents maps a normalized dataset oid to the oids of datasets consuming it.
	Dependents map[string][]string
}

// BuildGraph walks every managed dataset's `inputs` to build the reverse dependency edges.
func BuildGraph(s *State) *Graph {
	g := &Graph{Dependents: map[string][]string{}}

	for i := range s.Resources {
		r := &s.Resources[i]
		if r.Type != "observe_dataset" || r.Mode != "managed" {
			continue
		}
		attrs := r.attrs()
		if attrs == nil {
			continue
		}
		self := NormalizeOID(str(attrs, "oid"))
		if self == "" {
			continue
		}

		inputs, _ := attrs["inputs"].(map[string]any)
		for _, v := range inputs {
			input, ok := v.(string)
			if !ok {
				continue
			}
			from := NormalizeOID(input)
			if from == "" || from == self {
				continue
			}
			g.Dependents[from] = append(g.Dependents[from], self)
		}
	}
	return g
}

// TransitiveDependents returns every dataset reachable downstream of the given oids, excluding the
// seeds themselves.
//
// Any config change to a dataset recomputes its oid, which makes each dependent's `inputs` unknown
// at plan time and cascades the update onward. This is the size of that blast radius.
func (g *Graph) TransitiveDependents(seeds []string) []string {
	seen := map[string]bool{}
	isSeed := map[string]bool{}
	for _, s := range seeds {
		isSeed[NormalizeOID(s)] = true
	}

	queue := make([]string, 0, len(seeds))
	for _, s := range seeds {
		queue = append(queue, NormalizeOID(s))
	}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range g.Dependents[cur] {
			if seen[next] || isSeed[next] {
				continue
			}
			seen[next] = true
			queue = append(queue, next)
		}
	}

	out := make([]string, 0, len(seen))
	for oid := range seen {
		out = append(out, oid)
	}
	sort.Strings(out)
	return out
}
