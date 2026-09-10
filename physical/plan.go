// Package plan metadata describes the bound operator without opening inputs.
package physical

import "strings"

// PlanNode is descriptive, not a second executable plan. Nil estimates mean unknown.
// Future optimizers and EXPLAIN ANALYZE may populate the optional fields.
type PlanNode struct {
	Kind          string
	Attributes    map[string]string
	Children      []*PlanNode
	EstimatedRows *float64
	EstimatedCost *float64
	ActualRows    *uint64
}
type PlanProvider interface{ PlanNode() *PlanNode }

func Describe(op any) *PlanNode {
	if p, ok := op.(PlanProvider); ok {
		return p.PlanNode()
	}
	return &PlanNode{Kind: "ExternalSource"}
}
func (p *PlanNode) String() string {
	if p == nil {
		return "Unknown"
	}
	if len(p.Children) == 0 {
		return p.Kind
	}
	parts := make([]string, len(p.Children))
	for i, c := range p.Children {
		parts[i] = c.String()
	}
	return p.Kind + "(" + strings.Join(parts, ", ") + ")"
}
func node(kind string, inputs ...any) *PlanNode {
	p := &PlanNode{Kind: kind}
	for _, input := range inputs {
		p.Children = append(p.Children, Describe(input))
	}
	return p
}
func (s Scan[T]) PlanNode() *PlanNode {
	if s.Plan != nil {
		return s.Plan
	}
	return node("Scan")
}
func (s Source[T]) PlanNode() *PlanNode        { return node("ExternalSource") }
func (f Filter[T]) PlanNode() *PlanNode        { return node("Filter", f.Input) }
func (p Projection[A, B]) PlanNode() *PlanNode { return node("Projection", p.Input) }
func (j Join3[L, R, O]) PlanNode() *PlanNode {
	p := node("Join", j.Left)
	r := j.RightPlan
	if r == nil {
		r = &PlanNode{Kind: "DynamicScan"}
	}
	p.Children = append(p.Children, r)
	return p
}
func (j Join[T]) PlanNode() *PlanNode         { return Join3[T, T, T](j).PlanNode() }
func (a Aggregate[A, B]) PlanNode() *PlanNode { return node("Aggregate", a.Input) }
func (s Sort[T]) PlanNode() *PlanNode         { return node("Sort", s.Input) }
func (d Distinct[T]) PlanNode() *PlanNode     { return node("Distinct", d.Input) }
func (l Limit[T]) PlanNode() *PlanNode        { return node("Limit", l.Input) }
func (t TopN[T]) PlanNode() *PlanNode         { return node("TopN", t.Sort) }
func (m Materialize[T]) PlanNode() *PlanNode  { return node("Materialize", m.Input) }
func (w Window[A, B]) PlanNode() *PlanNode    { return node("Window", w.Input) }
func (m Modify[A, B]) PlanNode() *PlanNode    { return node("Modify", m.Input) }
func (u Union[T]) PlanNode() *PlanNode {
	p := node("Union")
	for _, i := range u.Inputs {
		p.Children = append(p.Children, Describe(i))
	}
	return p
}
