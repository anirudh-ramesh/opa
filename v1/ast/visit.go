// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package ast

// Visitor defines the interface for iterating AST elements. The Visit function
// can return a Visitor w which will be used to visit the children of the AST
// element v. If the Visit function returns nil, the children will not be
// visited.
// Deprecated: use GenericVisitor or another visitor implementation
type Visitor interface {
	Visit(v any) (w Visitor)
}

// BeforeAndAfterVisitor wraps Visitor to provide hooks for being called before
// and after the AST has been visited.
// Deprecated: use GenericVisitor or another visitor implementation
type BeforeAndAfterVisitor interface {
	Visitor
	Before(x any)
	After(x any)
}

// Walk iterates the AST by calling the Visit function on the Visitor
// v for x before recursing.
// Deprecated: use GenericVisitor.Walk
func Walk(v Visitor, x any) {
	if bav, ok := v.(BeforeAndAfterVisitor); !ok {
		walk(v, x)
	} else {
		bav.Before(x)
		defer bav.After(x)
		walk(bav, x)
	}
}

// WalkBeforeAndAfter iterates the AST by calling the Visit function on the
// Visitor v for x before recursing.
// Deprecated: use GenericVisitor.Walk
func WalkBeforeAndAfter(v BeforeAndAfterVisitor, x any) {
	Walk(v, x)
}

func walk(v Visitor, x any) {
	w := v.Visit(x)
	if w == nil {
		return
	}
	switch x := x.(type) {
	case *Module:
		Walk(w, x.Package)
		for i := range x.Imports {
			Walk(w, x.Imports[i])
		}
		for i := range x.Rules {
			Walk(w, x.Rules[i])
		}
		for i := range x.Annotations {
			Walk(w, x.Annotations[i])
		}
		for i := range x.Comments {
			Walk(w, x.Comments[i])
		}
	case *Package:
		Walk(w, x.Path)
	case *Import:
		Walk(w, x.Path)
		Walk(w, x.Alias)
	case *Rule:
		Walk(w, x.Head)
		Walk(w, x.Body)
		if x.Else != nil {
			Walk(w, x.Else)
		}
	case *Head:
		Walk(w, x.Name)
		Walk(w, x.Args)
		if x.Key != nil {
			Walk(w, x.Key)
		}
		if x.Value != nil {
			Walk(w, x.Value)
		}
	case Body:
		for i := range x {
			Walk(w, x[i])
		}
	case Args:
		for i := range x {
			Walk(w, x[i])
		}
	case *Expr:
		switch ts := x.Terms.(type) {
		case *Term, *SomeDecl, *Every:
			Walk(w, ts)
		case []*Term:
			for i := range ts {
				Walk(w, ts[i])
			}
		}
		for i := range x.With {
			Walk(w, x.With[i])
		}
	case *With:
		Walk(w, x.Target)
		Walk(w, x.Value)
	case *Term:
		Walk(w, x.Value)
	case Ref:
		for i := range x {
			Walk(w, x[i])
		}
	case *object:
		x.Foreach(func(k, vv *Term) {
			Walk(w, k)
			Walk(w, vv)
		})
	case *Array:
		x.Foreach(func(t *Term) {
			Walk(w, t)
		})
	case Set:
		x.Foreach(func(t *Term) {
			Walk(w, t)
		})
	case *ArrayComprehension:
		Walk(w, x.Term)
		Walk(w, x.Body)
	case *ObjectComprehension:
		Walk(w, x.Key)
		Walk(w, x.Value)
		Walk(w, x.Body)
	case *SetComprehension:
		Walk(w, x.Term)
		Walk(w, x.Body)
	case Call:
		for i := range x {
			Walk(w, x[i])
		}
	case *Every:
		if x.Key != nil {
			Walk(w, x.Key)
		}
		Walk(w, x.Value)
		Walk(w, x.Domain)
		Walk(w, x.Body)
	case *SomeDecl:
		for i := range x.Symbols {
			Walk(w, x.Symbols[i])
		}
	}
}

// WalkVars calls the function f on all vars under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkVars(x any, f func(Var) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if v, ok := x.(Var); ok {
			return f(v)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkClosures calls the function f on all closures under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkClosures(x any, f func(any) bool) {
	vis := &GenericVisitor{func(x any) bool {
		switch x := x.(type) {
		case *ArrayComprehension, *ObjectComprehension, *SetComprehension, *Every:
			return f(x)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkRefs calls the function f on all references under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkRefs(x any, f func(Ref) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if r, ok := x.(Ref); ok {
			return f(r)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkTerms calls the function f on all terms under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkTerms(x any, f func(*Term) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if term, ok := x.(*Term); ok {
			return f(term)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkWiths calls the function f on all with modifiers under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkWiths(x any, f func(*With) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if w, ok := x.(*With); ok {
			return f(w)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkExprs calls the function f on all expressions under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkExprs(x any, f func(*Expr) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if r, ok := x.(*Expr); ok {
			return f(r)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkBodies calls the function f on all bodies under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkBodies(x any, f func(Body) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if b, ok := x.(Body); ok {
			return f(b)
		}
		return false
	}}
	vis.Walk(x)
}

// WalkRules calls the function f on all rules under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkRules(x any, f func(*Rule) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if r, ok := x.(*Rule); ok {
			stop := f(r)
			// NOTE(tsandall): since rules cannot be embedded inside of queries
			// we can stop early if there is no else block.
			if stop || r.Else == nil {
				return true
			}
		}
		return false
	}}
	vis.Walk(x)
}

// WalkNodes calls the function f on all nodes under x. If the function f
// returns true, AST nodes under the last node will not be visited.
func WalkNodes(x any, f func(Node) bool) {
	vis := &GenericVisitor{func(x any) bool {
		if n, ok := x.(Node); ok {
			return f(n)
		}
		return false
	}}
	vis.Walk(x)
}

// GenericVisitor provides a utility to walk over AST nodes using a
// closure. If the closure returns true, the visitor will not walk
// over AST nodes under x.
type GenericVisitor struct {
	f func(x any) bool
}

// NewGenericVisitor returns a new GenericVisitor that will invoke the function
// f on AST nodes.
func NewGenericVisitor(f func(x any) bool) *GenericVisitor {
	return &GenericVisitor{f}
}

// Walk iterates the AST by calling the function f on the
// GenericVisitor before recursing. Contrary to the generic Walk, this
// does not require allocating the visitor from heap.
func (vis *GenericVisitor) Walk(x any) {
	if vis.f(x) {
		return
	}

	switch x := x.(type) {
	case *Module:
		vis.Walk(x.Package)
		for i := range x.Imports {
			vis.Walk(x.Imports[i])
		}
		for i := range x.Rules {
			vis.Walk(x.Rules[i])
		}
		for i := range x.Annotations {
			vis.Walk(x.Annotations[i])
		}
		for i := range x.Comments {
			vis.Walk(x.Comments[i])
		}
	case *Package:
		vis.Walk(x.Path)
	case *Import:
		vis.Walk(x.Path)
		vis.Walk(x.Alias)
	case *Rule:
		vis.Walk(x.Head)
		vis.Walk(x.Body)
		if x.Else != nil {
			vis.Walk(x.Else)
		}
	case *Head:
		vis.Walk(x.Name)
		vis.Walk(x.Args)
		if x.Key != nil {
			vis.Walk(x.Key)
		}
		if x.Value != nil {
			vis.Walk(x.Value)
		}
	case Body:
		for i := range x {
			vis.Walk(x[i])
		}
	case Args:
		for i := range x {
			vis.Walk(x[i])
		}
	case *Expr:
		switch ts := x.Terms.(type) {
		case *Term, *SomeDecl, *Every:
			vis.Walk(ts)
		case []*Term:
			for i := range ts {
				vis.Walk(ts[i])
			}
		}
		for i := range x.With {
			vis.Walk(x.With[i])
		}
	case *With:
		vis.Walk(x.Target)
		vis.Walk(x.Value)
	case *Term:
		vis.Walk(x.Value)
	case Ref:
		for i := range x {
			vis.Walk(x[i])
		}
	case *object:
		x.Foreach(func(k, _ *Term) {
			vis.Walk(k)
			vis.Walk(x.Get(k))
		})
	case Object:
		for _, k := range x.Keys() {
			vis.Walk(k)
			vis.Walk(x.Get(k))
		}
	case *Array:
		for i := range x.Len() {
			vis.Walk(x.Elem(i))
		}
	case Set:
		xSlice := x.Slice()
		for i := range xSlice {
			vis.Walk(xSlice[i])
		}
	case *ArrayComprehension:
		vis.Walk(x.Term)
		vis.Walk(x.Body)
	case *ObjectComprehension:
		vis.Walk(x.Key)
		vis.Walk(x.Value)
		vis.Walk(x.Body)
	case *SetComprehension:
		vis.Walk(x.Term)
		vis.Walk(x.Body)
	case Call:
		for i := range x {
			vis.Walk(x[i])
		}
	case *Every:
		if x.Key != nil {
			vis.Walk(x.Key)
		}
		vis.Walk(x.Value)
		vis.Walk(x.Domain)
		vis.Walk(x.Body)
	case *SomeDecl:
		for i := range x.Symbols {
			vis.Walk(x.Symbols[i])
		}
	}
}

// BeforeAfterVisitor provides a utility to walk over AST nodes using
// closures. If the before closure returns true, the visitor will not
// walk over AST nodes under x. The after closure is invoked always
// after visiting a node.
type BeforeAfterVisitor struct {
	before func(x any) bool
	after  func(x any)
}

// NewBeforeAfterVisitor returns a new BeforeAndAfterVisitor that
// will invoke the functions before and after AST nodes.
func NewBeforeAfterVisitor(before func(x any) bool, after func(x any)) *BeforeAfterVisitor {
	return &BeforeAfterVisitor{before, after}
}

// Walk iterates the AST by calling the functions on the
// BeforeAndAfterVisitor before and after recursing. Contrary to the
// generic Walk, this does not require allocating the visitor from
// heap.
func (vis *BeforeAfterVisitor) Walk(x any) {
	defer vis.after(x)
	if vis.before(x) {
		return
	}

	switch x := x.(type) {
	case *Module:
		vis.Walk(x.Package)
		for i := range x.Imports {
			vis.Walk(x.Imports[i])
		}
		for i := range x.Rules {
			vis.Walk(x.Rules[i])
		}
		for i := range x.Annotations {
			vis.Walk(x.Annotations[i])
		}
		for i := range x.Comments {
			vis.Walk(x.Comments[i])
		}
	case *Package:
		vis.Walk(x.Path)
	case *Import:
		vis.Walk(x.Path)
		vis.Walk(x.Alias)
	case *Rule:
		vis.Walk(x.Head)
		vis.Walk(x.Body)
		if x.Else != nil {
			vis.Walk(x.Else)
		}
	case *Head:
		if len(x.Reference) > 0 {
			vis.Walk(x.Reference)
		} else {
			vis.Walk(x.Name)
			if x.Key != nil {
				vis.Walk(x.Key)
			}
		}
		vis.Walk(x.Args)
		if x.Value != nil {
			vis.Walk(x.Value)
		}
	case Body:
		for i := range x {
			vis.Walk(x[i])
		}
	case Args:
		for i := range x {
			vis.Walk(x[i])
		}
	case *Expr:
		switch ts := x.Terms.(type) {
		case *Term, *SomeDecl, *Every:
			vis.Walk(ts)
		case []*Term:
			for i := range ts {
				vis.Walk(ts[i])
			}
		}
		for i := range x.With {
			vis.Walk(x.With[i])
		}
	case *With:
		vis.Walk(x.Target)
		vis.Walk(x.Value)
	case *Term:
		vis.Walk(x.Value)
	case Ref:
		for i := range x {
			vis.Walk(x[i])
		}
	case *object:
		x.Foreach(func(k, _ *Term) {
			vis.Walk(k)
			vis.Walk(x.Get(k))
		})
	case Object:
		x.Foreach(func(k, _ *Term) {
			vis.Walk(k)
			vis.Walk(x.Get(k))
		})
	case *Array:
		x.Foreach(func(t *Term) {
			vis.Walk(t)
		})
	case Set:
		xSlice := x.Slice()
		for i := range xSlice {
			vis.Walk(xSlice[i])
		}
	case *ArrayComprehension:
		vis.Walk(x.Term)
		vis.Walk(x.Body)
	case *ObjectComprehension:
		vis.Walk(x.Key)
		vis.Walk(x.Value)
		vis.Walk(x.Body)
	case *SetComprehension:
		vis.Walk(x.Term)
		vis.Walk(x.Body)
	case Call:
		for i := range x {
			vis.Walk(x[i])
		}
	case *Every:
		if x.Key != nil {
			vis.Walk(x.Key)
		}
		vis.Walk(x.Value)
		vis.Walk(x.Domain)
		vis.Walk(x.Body)
	case *SomeDecl:
		for i := range x.Symbols {
			vis.Walk(x.Symbols[i])
		}
	}
}

// VarVisitor walks AST nodes under a given node and collects all encountered
// variables. The collected variables can be controlled by specifying
// VarVisitorParams when creating the visitor.
type VarVisitor struct {
	params VarVisitorParams
	vars   VarSet
}

// VarVisitorParams contains settings for a VarVisitor.
type VarVisitorParams struct {
	SkipRefHead     bool
	SkipRefCallHead bool
	SkipObjectKeys  bool
	SkipClosures    bool
	SkipWithTarget  bool
	SkipSets        bool
}

// NewVarVisitor returns a new VarVisitor object.
func NewVarVisitor() *VarVisitor {
	return &VarVisitor{
		vars: NewVarSet(),
	}
}

// Clear resets the visitor to its initial state, and returns it for chaining.
func (vis *VarVisitor) Clear() *VarVisitor {
	vis.params = VarVisitorParams{}
	clear(vis.vars)

	return vis
}

// ClearOrNew returns a new VarVisitor if vis is nil, or else a cleared VarVisitor.
func (vis *VarVisitor) ClearOrNew() *VarVisitor {
	if vis == nil {
		return NewVarVisitor()
	}
	return vis.Clear()
}

// WithParams sets the parameters in params on vis.
func (vis *VarVisitor) WithParams(params VarVisitorParams) *VarVisitor {
	vis.params = params
	return vis
}

// Add adds a variable v to the visitor's set of variables.
func (vis *VarVisitor) Add(v Var) {
	if vis.vars == nil {
		vis.vars = NewVarSet(v)
	} else {
		vis.vars.Add(v)
	}
}

// Vars returns a VarSet that contains collected vars.
func (vis *VarVisitor) Vars() VarSet {
	return vis.vars
}

// visit determines if the VarVisitor will recurse into x: if it returns `true`,
// the visitor will _skip_ that branch of the AST
func (vis *VarVisitor) visit(v any) bool {
	if vis.params.SkipObjectKeys {
		if o, ok := v.(Object); ok {
			o.Foreach(func(_, v *Term) {
				vis.Walk(v)
			})
			return true
		}
	}
	if vis.params.SkipRefHead {
		if r, ok := v.(Ref); ok {
			rSlice := r[1:]
			for i := range rSlice {
				vis.Walk(rSlice[i])
			}
			return true
		}
	}
	if vis.params.SkipClosures {
		switch v := v.(type) {
		case *ArrayComprehension, *ObjectComprehension, *SetComprehension:
			return true
		case *Expr:
			if ev, ok := v.Terms.(*Every); ok {
				vis.Walk(ev.Domain)
				// We're _not_ walking ev.Body -- that's the closure here
				return true
			}
		}
	}
	if vis.params.SkipWithTarget {
		if v, ok := v.(*With); ok {
			vis.Walk(v.Value)
			return true
		}
	}
	if vis.params.SkipSets {
		if _, ok := v.(Set); ok {
			return true
		}
	}
	if vis.params.SkipRefCallHead {
		switch v := v.(type) {
		case *Expr:
			if terms, ok := v.Terms.([]*Term); ok {
				termSlice := terms[0].Value.(Ref)[1:]
				for i := range termSlice {
					vis.Walk(termSlice[i])
				}
				for i := 1; i < len(terms); i++ {
					vis.Walk(terms[i])
				}
				for i := range v.With {
					vis.Walk(v.With[i])
				}
				return true
			}
		case Call:
			operator := v[0].Value.(Ref)
			for i := 1; i < len(operator); i++ {
				vis.Walk(operator[i])
			}
			for i := 1; i < len(v); i++ {
				vis.Walk(v[i])
			}
			return true
		case *With:
			if ref, ok := v.Target.Value.(Ref); ok {
				refSlice := ref[1:]
				for i := range refSlice {
					vis.Walk(refSlice[i])
				}
			}
			if ref, ok := v.Value.Value.(Ref); ok {
				refSlice := ref[1:]
				for i := range refSlice {
					vis.Walk(refSlice[i])
				}
			} else {
				vis.Walk(v.Value)
			}
			return true
		}
	}
	if v, ok := v.(Var); ok {
		vis.Add(v)
	}
	return false
}

// Walk iterates the AST by calling the function f on the
// GenericVisitor before recursing. Contrary to the generic Walk, this
// does not require allocating the visitor from heap.
func (vis *VarVisitor) Walk(x any) {
	if vis.visit(x) {
		return
	}

	switch x := x.(type) {
	case *Module:
		vis.Walk(x.Package)
		for i := range x.Imports {
			vis.Walk(x.Imports[i])
		}
		for i := range x.Rules {
			vis.Walk(x.Rules[i])
		}
		for i := range x.Comments {
			vis.Walk(x.Comments[i])
		}
	case *Package:
		vis.WalkRef(x.Path)
	case *Import:
		vis.Walk(x.Path)
		if x.Alias != "" {
			vis.Add(x.Alias)
		}
	case *Rule:
		vis.Walk(x.Head)
		vis.WalkBody(x.Body)
		if x.Else != nil {
			vis.Walk(x.Else)
		}
	case *Head:
		if len(x.Reference) > 0 {
			vis.WalkRef(x.Reference)
		} else {
			vis.Add(x.Name)
			if x.Key != nil {
				vis.Walk(x.Key)
			}
		}
		vis.WalkArgs(x.Args)
		if x.Value != nil {
			vis.Walk(x.Value)
		}
	case Body:
		vis.WalkBody(x)
	case Args:
		vis.WalkArgs(x)
	case *Expr:
		switch ts := x.Terms.(type) {
		case *Term, *SomeDecl, *Every:
			vis.Walk(ts)
		case []*Term:
			for i := range ts {
				vis.Walk(ts[i].Value)
			}
		}
		for i := range x.With {
			vis.Walk(x.With[i])
		}
	case *With:
		vis.Walk(x.Target.Value)
		vis.Walk(x.Value.Value)
	case *Term:
		vis.Walk(x.Value)
	case Ref:
		for i := range x {
			vis.Walk(x[i].Value)
		}
	case *object:
		x.Foreach(func(k, _ *Term) {
			vis.Walk(k)
			vis.Walk(x.Get(k))
		})
	case *Array:
		x.Foreach(func(t *Term) {
			vis.Walk(t)
		})
	case Set:
		xSlice := x.Slice()
		for i := range xSlice {
			vis.Walk(xSlice[i])
		}
	case *ArrayComprehension:
		vis.Walk(x.Term.Value)
		vis.WalkBody(x.Body)
	case *ObjectComprehension:
		vis.Walk(x.Key.Value)
		vis.Walk(x.Value.Value)
		vis.WalkBody(x.Body)
	case *SetComprehension:
		vis.Walk(x.Term.Value)
		vis.WalkBody(x.Body)
	case Call:
		for i := range x {
			vis.Walk(x[i].Value)
		}
	case *Every:
		if x.Key != nil {
			vis.Walk(x.Key.Value)
		}
		vis.Walk(x.Value)
		vis.Walk(x.Domain)
		vis.WalkBody(x.Body)
	case *SomeDecl:
		for i := range x.Symbols {
			vis.Walk(x.Symbols[i])
		}
	}
}

// WalkArgs exists only to avoid the allocation cost of boxing Args to `any` in the VarVisitor.
// Use it when you know beforehand that the type to walk is Args.
func (vis *VarVisitor) WalkArgs(x Args) {
	for i := range x {
		vis.Walk(x[i].Value)
	}
}

// WalkRef exists only to avoid the allocation cost of boxing Ref to `any` in the VarVisitor.
// Use it when you know beforehand that the type to walk is a Ref.
func (vis *VarVisitor) WalkRef(ref Ref) {
	if vis.params.SkipRefHead {
		ref = ref[1:]
	}
	for _, term := range ref {
		vis.Walk(term.Value)
	}
}

// WalkBody exists only to avoid the allocation cost of boxing Body to `any` in the VarVisitor.
// Use it when you know beforehand that the type to walk is a Body.
func (vis *VarVisitor) WalkBody(body Body) {
	for _, expr := range body {
		vis.Walk(expr)
	}
}
