// Copyright (C) 2022 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package plan

import (
	"errors"
	"fmt"

	"golang.org/x/exp/slices"

	"github.com/SnellerInc/sneller/expr"
	"github.com/SnellerInc/sneller/ion"
	"github.com/SnellerInc/sneller/ion/blockfmt"
	"github.com/SnellerInc/sneller/plan/pir"
	"github.com/SnellerInc/sneller/vm"
)

var (
	ErrNotSupported = errors.New("plan: query not supported")
)

// reject produces an ErrNotSupported error message
func reject(msg string) error {
	return fmt.Errorf("%w: %s", ErrNotSupported, msg)
}

func lowerIterValue(in *pir.IterValue, from Op) (Op, error) {
	if in.Wildcard() {
		return nil, reject("cannot project '*' from a cross-join")
	}
	if pivot, ok := in.Value.(*expr.Path); ok {
		return &Unnest{
			Nonterminal: Nonterminal{
				From: from,
			},
			PivotField:   pivot,
			InnerProject: vm.Selection(in.InnerBind()),
			OuterProject: vm.Selection(in.OuterBind()),
			InnerMatch:   in.Filter,
		}, nil
	} else {
		if _ /*unpivot*/, ok := in.Value.(*expr.Unpivot); ok {
			return nil, reject("UNPIVOT is not supported yet")
		} else {
			return nil, reject("cross-join on non-path nor UNPIVOT expression")
		}
	}
}

func lowerFilter(in *pir.Filter, from Op) (Op, error) {
	return &Filter{
		Nonterminal: Nonterminal{From: from},
		Expr:        in.Where,
	}, nil
}

func lowerDistinct(in *pir.Distinct, from Op) (Op, error) {
	return &Distinct{
		Nonterminal: Nonterminal{From: from},
		Fields:      in.Columns,
	}, nil
}

func lowerLimit(in *pir.Limit, from Op) (Op, error) {
	if in.Count == 0 {
		return NoOutput{}, nil
	}

	// some operations accept Limit natively
	switch f := from.(type) {
	case *HashAggregate:
		f.Limit = int(in.Count)
		if in.Offset != 0 {
			return nil, reject("non-zero OFFSET of hash aggregate result")
		}
		return f, nil
	case *OrderBy:
		f.Limit = int(in.Count)
		f.Offset = int(in.Offset)
		return f, nil
	case *Distinct:
		if in.Offset != 0 {
			return nil, reject("non-zero OFFSET of distinct result")
		}
		f.Limit = in.Count
		return f, nil
	}
	if in.Offset != 0 {
		return nil, reject("OFFSET without GROUP BY/ORDER BY not implemented")
	}
	return &Limit{
		Nonterminal: Nonterminal{From: from},
		Num:         in.Count,
	}, nil
}

func iscountstar(a vm.Aggregation) bool {
	if len(a) != 1 {
		return false
	}

	agg := a[0]
	if agg.Expr.Filter != nil {
		return false
	}

	_, isstar := agg.Expr.Inner.(expr.Star)
	return isstar
}

func lowerAggregate(in *pir.Aggregate, from Op) (Op, error) {
	if in.GroupBy == nil {
		// simple aggregate; check for COUNT(*) first
		if iscountstar(in.Agg) {
			return &CountStar{
				Nonterminal: Nonterminal{From: from},
				As:          in.Agg[0].Result,
			}, nil
		}
		return &SimpleAggregate{
			Nonterminal: Nonterminal{From: from},
			Outputs:     in.Agg,
		}, nil
	}

	return &HashAggregate{
		Nonterminal: Nonterminal{From: from},
		Agg:         in.Agg,
		By:          in.GroupBy,
	}, nil
}

func lowerOrder(in *pir.Order, from Op) (Op, error) {
	if ha, ok := from.(*HashAggregate); ok {
		// hash aggregates can accept ORDER BY directly
	outer:
		for i := range in.Columns {
			ex := in.Columns[i].Column
			for col := range ha.Agg {
				if expr.IsIdentifier(ex, ha.Agg[col].Result) {
					ha.OrderBy = append(ha.OrderBy, HashOrder{
						Column:    col,
						Desc:      in.Columns[i].Desc,
						NullsLast: in.Columns[i].NullsLast,
					})
					continue outer
				}
			}
			for col := range ha.By {
				if expr.IsIdentifier(ex, ha.By[col].Result()) {
					ha.OrderBy = append(ha.OrderBy, HashOrder{
						Column:    len(ha.Agg) + col,
						Desc:      in.Columns[i].Desc,
						NullsLast: in.Columns[i].NullsLast,
					})
					continue outer
				}
			}
			return nil, fmt.Errorf("cannot ORDER BY expression %q", ex)
		}
		return ha, nil
	}

	// ordinary Order node
	columns := make([]OrderByColumn, 0, len(in.Columns))
	for i := range in.Columns {
		switch in.Columns[i].Column.(type) {
		case expr.Bool, expr.Integer, *expr.Rational, expr.Float, expr.String:
			// skip constant columns; they do not meaningfully apply a sort
			continue
		}

		columns = append(columns, OrderByColumn{
			Node:      in.Columns[i].Column,
			Desc:      in.Columns[i].Desc,
			NullsLast: in.Columns[i].NullsLast,
		})
	}

	// if we had ORDER BY "foo" or something like that,
	// then we don't need to do any ordering at all
	if len(columns) == 0 {
		return from, nil
	}

	// find possible duplicates
	for i := range columns {
		for j := i + 1; j < len(columns); j++ {
			if expr.Equivalent(columns[i].Node, columns[j].Node) {
				return nil, fmt.Errorf("duplicate order by expression %q", expr.ToString(columns[j].Node))
			}
		}
	}

	return &OrderBy{
		Nonterminal: Nonterminal{From: from},
		Columns:     columns,
	}, nil
}

func lowerBind(in *pir.Bind, from Op) (Op, error) {
	return &Project{
		Nonterminal: Nonterminal{From: from},
		Using:       in.Bindings(),
	}, nil
}

func (w *walker) lowerUnionMap(in *pir.UnionMap) (Op, error) {
	input := w.put(in.Inner)
	// NOTE: we're passing the same splitter
	// to the child here. We don't currently
	// produce nested split queries, so it isn't
	// meaningful at the moment, but it's possible
	// at some point we will need to indicate that
	// we are splitting an already-split query
	sub, err := w.walkBuild(in.Child.Final())
	if err != nil {
		return nil, err
	}
	handle, err := w.inputs[input].stat(w.env)
	if err != nil {
		return nil, err
	}
	tbls, err := doSplit(w.split, in.Inner.Table.Expr, handle)
	if err != nil {
		return nil, err
	}
	// no subtables means no output
	if tbls.Len() == 0 {
		return NoOutput{}, nil
	}
	return &UnionMap{
		Nonterminal: Nonterminal{From: sub},
		Orig:        input,
		Sub:         tbls,
	}, nil
}

// doSplit calls s.Split(tbl, th) with special handling
// for tableHandles.
func doSplit(s Splitter, tbl expr.Node, th TableHandle) (Subtables, error) {
	hs, ok := th.(tableHandles)
	if !ok {
		return s.Split(tbl, th)
	}
	var out Subtables
	for i := range hs {
		sub, err := doSplit(s, tbl, hs[i])
		if err != nil {
			return nil, err
		}
		if out == nil {
			out = sub
		} else {
			out = out.Append(sub)
		}
	}
	return out, nil
}

// UploadFS is a blockfmt.UploadFS that can be encoded
// as part of a query plan.
type UploadFS interface {
	blockfmt.UploadFS
	// Encode encodes the UploadFS into the
	// provided buffer.
	Encode(dst *ion.Buffer, st *ion.Symtab) error
}

// UploadEnv is an Env that supports uploading objects
// which enables support for SELECT INTO.
type UploadEnv interface {
	// Uploader returns an UploadFS to use to
	// upload generated objects. This may return
	// nil if the envionment does not support
	// uploading despite implementing the
	// interface.
	Uploader() UploadFS
	// Key returns the key that should be used to
	// sign the index.
	Key() *blockfmt.Key
}

func lowerOutputPart(n *pir.OutputPart, env Env, input Op) (Op, error) {
	if e, ok := env.(UploadEnv); ok {
		if up := e.Uploader(); up != nil {
			op := &OutputPart{
				Basename: n.Basename,
				Store:    up,
			}
			op.From = input
			return op, nil
		}
	}
	return nil, fmt.Errorf("cannot handle INTO with Env that doesn't support UploadEnv")
}

func lowerOutputIndex(n *pir.OutputIndex, env Env, input Op) (Op, error) {
	if e, ok := env.(UploadEnv); ok {
		if up := e.Uploader(); up != nil {
			op := &OutputIndex{
				Table:    n.Table,
				Basename: n.Basename,
				Store:    up,
				Key:      e.Key(),
			}
			op.From = input
			return op, nil
		}
	}
	return nil, fmt.Errorf("cannot handle INTO with Env that doesn't support UploadEnv")
}

type input struct {
	table  *expr.Table
	hints  Hints
	handle TableHandle // if already statted
}

func (i *input) finish(env Env) (Input, error) {
	th, err := i.stat(env)
	if err != nil {
		return Input{}, err
	}
	return Input{
		Table:  i.table,
		Handle: th,
	}, nil
}

func (i *input) stat(env Env) (TableHandle, error) {
	if i.handle != nil {
		return i.handle, nil
	}
	th, err := stat(env, i.table.Expr, &i.hints)
	if err != nil {
		return nil, err
	}
	i.handle = th
	return th, nil
}

func (i *input) merge(in *input) bool {
	if !i.table.Equals(in.table) {
		return false
	}
	if !expr.Equal(i.hints.Filter, in.hints.Filter) {
		return false
	}
	i.handle = nil
	if i.hints.AllFields {
		return true
	}
	if in.hints.AllFields {
		i.hints.Fields = nil
		i.hints.AllFields = true
		return true
	}
	i.hints.Fields = append(i.hints.Fields, in.hints.Fields...)
	slices.Sort(i.hints.Fields)
	i.hints.Fields = slices.Compact(i.hints.Fields)
	return true
}

type walker struct {
	env    Env
	split  Splitter
	inputs []input
}

func (w *walker) put(it *pir.IterTable) int {
	in := input{
		table: it.Table,
		hints: Hints{
			Filter:    it.Filter,
			Fields:    it.Fields(),
			AllFields: it.Wildcard(),
		},
	}
	for i := range w.inputs {
		if w.inputs[i].merge(&in) {
			return i
		}
	}
	i := len(w.inputs)
	w.inputs = append(w.inputs, in)
	return i
}

func (w *walker) walkBuild(in pir.Step) (Op, error) {
	// IterTable is the terminal node
	if it, ok := in.(*pir.IterTable); ok {
		// TODO: we should handle table globs and
		// the ++ operator specially
		out := Op(&Leaf{
			Input: w.put(it),
		})
		if it.Filter != nil {
			out = &Filter{
				Nonterminal: Nonterminal{From: out},
				Expr:        it.Filter,
			}
		}
		return out, nil
	}
	// similarly, NoOutput is also terminal
	if _, ok := in.(pir.NoOutput); ok {
		return NoOutput{}, nil
	}
	if _, ok := in.(pir.DummyOutput); ok {
		return DummyOutput{}, nil
	}

	// ... and UnionMap as well
	if u, ok := in.(*pir.UnionMap); ok {
		return w.lowerUnionMap(u)
	}

	input, err := w.walkBuild(pir.Input(in))
	if err != nil {
		return nil, err
	}
	switch n := in.(type) {
	case *pir.IterValue:
		return lowerIterValue(n, input)
	case *pir.Filter:
		return lowerFilter(n, input)
	case *pir.Distinct:
		return lowerDistinct(n, input)
	case *pir.Bind:
		return lowerBind(n, input)
	case *pir.Aggregate:
		return lowerAggregate(n, input)
	case *pir.Limit:
		return lowerLimit(n, input)
	case *pir.Order:
		return lowerOrder(n, input)
	case *pir.OutputIndex:
		return lowerOutputIndex(n, w.env, input)
	case *pir.OutputPart:
		return lowerOutputPart(n, w.env, input)
	default:
		return nil, fmt.Errorf("don't know how to lower %T", in)
	}
}

func (w *walker) finish() ([]Input, error) {
	if w.inputs == nil {
		return nil, nil
	}
	inputs := make([]Input, len(w.inputs))
	for i := range w.inputs {
		in, err := w.inputs[i].finish(w.env)
		if err != nil {
			return nil, err
		}
		inputs[i] = in
	}
	return inputs, nil
}

// Result is a (field, type) tuple
// that indicates the possible output encoding
// of a particular field
type Result struct {
	Name string
	Type expr.TypeSet
}

// ResultSet is an ordered list of Results
type ResultSet []Result

func results(b *pir.Trace) ResultSet {
	final := b.FinalBindings()
	if len(final) == 0 {
		return nil
	}
	out := make(ResultSet, len(final))
	for i := range final {
		out[i] = Result{Name: final[i].Result(), Type: b.TypeOf(final[i].Expr)}
	}
	return out
}

func toTree(in *pir.Trace, env Env, split Splitter) (*Tree, error) {
	w := walker{
		env:   env,
		split: split,
	}
	t := &Tree{}
	err := w.toNode(&t.Root, in)
	if err != nil {
		return nil, err
	}
	inputs, err := w.finish()
	if err != nil {
		return nil, err
	}
	t.Inputs = inputs
	return t, nil
}

func (w *walker) toNode(t *Node, in *pir.Trace) error {
	op, err := w.walkBuild(in.Final())
	if err != nil {
		return err
	}
	t.Op = op
	t.OutputType = results(in)
	t.Children = make([]*Node, len(in.Replacements))
	sub := walker{
		env:   w.env,
		split: w.split,
	}
	for i := range in.Replacements {
		t.Children[i] = &Node{}
		err := sub.toNode(t.Children[i], in.Replacements[i])
		if err != nil {
			return err
		}
	}
	inputs, err := sub.finish()
	if err != nil {
		return err
	}
	t.Inputs = inputs
	return nil
}

type pirenv struct {
	env Env
}

func (e pirenv) Schema(tbl expr.Node) expr.Hint {
	s, ok := e.env.(Schemer)
	if !ok {
		return nil
	}
	return s.Schema(tbl)
}

func (e pirenv) Index(tbl expr.Node) (pir.Index, error) {
	idx, ok := e.env.(Indexer)
	if !ok {
		return nil, nil
	}
	return index(idx, tbl)
}

// New creates a new Tree from raw query AST.
func New(q *expr.Query, env Env) (*Tree, error) {
	return NewSplit(q, env, nil)
}

// NewSplit creates a new Tree from raw query AST.
func NewSplit(q *expr.Query, env Env, split Splitter) (*Tree, error) {
	b, err := pir.Build(q, pirenv{env})
	if err != nil {
		return nil, err
	}
	if split != nil {
		reduce, err := pir.Split(b)
		if err != nil {
			return nil, err
		}
		b = reduce
	}
	return toTree(b, env, split)
}
