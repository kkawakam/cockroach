// Copyright 2018 The Cockroach Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
// implied. See the License for the specific language governing
// permissions and limitations under the License.

package norm

import (
	"github.com/cockroachdb/cockroach/pkg/sql/opt"
	"github.com/cockroachdb/cockroach/pkg/sql/opt/memo"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/tree"
	"github.com/cockroachdb/cockroach/pkg/sql/sem/types"
	"github.com/cockroachdb/cockroach/pkg/util"
)

// MatchedRuleFunc defines the callback function for the NotifyOnMatchedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been matched. The name of the
// matched rule is passed as a parameter. If the function returns false, then
// the rule is not applied (i.e. skipped).
type MatchedRuleFunc func(ruleName opt.RuleName) bool

// AppliedRuleFunc defines the callback function for the NotifyOnAppliedRule
// event supported by the optimizer and factory. It is invoked each time an
// optimization rule (Normalize or Explore) has been applied.
//
// The function is called with the name of the rule and the memo group it
// affected. If the rule was an exploration rule, then the expr parameter
// indicates the expression on which the rule was applied, and the added
// parameter number of expressions added to the group by the rule.
type AppliedRuleFunc func(
	ruleName opt.RuleName, group memo.GroupID, expr memo.ExprOrdinal, added int,
)

// Factory constructs a normalized expression tree within the memo. As each
// kind of expression is constructed by the factory, it transitively runs
// normalization transformations defined for that expression type. This may
// result in the construction of a different type of expression than what was
// requested. If, after normalization, the expression is already part of the
// memo, then construction is a no-op. Otherwise, a new memo group is created,
// with the normalized expression as its first and only expression.
//
// The result of calling each Factory Construct method is the id of the group
// that was constructed. Callers can access the normalized expression tree that
// the factory constructs by creating a memo.ExprView, like this:
//
//   ev := memo.MakeNormExprView(f.Memo(), group)
//
// Factory is largely auto-generated by optgen. The generated code can be found
// in factory.og.go. The factory.go file contains helper functions that are
// invoked by normalization patterns. While most patterns are specified in the
// optgen DSL, the factory always calls the `onConstruct` method as its last
// step, in order to allow any custom manual code to execute.
type Factory struct {
	evalCtx *tree.EvalContext

	// mem is the Memo data structure that the factory builds.
	mem memo.Memo

	// funcs is the struct used to call all custom match and replace functions
	// used by the normalization rules. It wraps an unnamed xfunc.CustomFuncs,
	// so it provides a clean interface for calling functions from both the norm
	// and xfunc packages using the same prefix.
	funcs CustomFuncs

	// matchedRule is the callback function that is invoked each time a normalize
	// rule has been matched by the factory. It can be set via a call to the
	// NotifyOnMatchedRule method.
	matchedRule MatchedRuleFunc

	// appliedRule is the callback function which is invoked each time a normalize
	// rule has been applied by the factory. It can be set via a call to the
	// NotifyOnAppliedRule method.
	appliedRule AppliedRuleFunc

	// ruleCycles is used to detect cyclical rule invocations. Each rule with
	// the "DetectCycles" tag adds its expression fingerprint into this map
	// before constructing its replacement. If the replacement pattern recursively
	// invokes the same rule (or another rule with the DetectCycles tag) with that
	// same fingerprint, then the rule sees that the fingerprint is already in the
	// map, and will skip application of the rule.
	ruleCycles ruleCycles

	// scratchItems is a slice that is reused by projectionsBuilder to store
	// temporary results that are accumulated before constructing a new
	// Projections operator.
	scratchItems []memo.GroupID

	// scratchColList is a ColList that is reused by projectionsBuilder to store
	// temporary results that are accumulated before constructing a new
	// Projections operator.
	scratchColList opt.ColList
}

// Init initializes a Factory structure with a new, blank memo structure inside.
// This must be called before the factory can be used (or reused).
func (f *Factory) Init(evalCtx *tree.EvalContext) {
	f.evalCtx = evalCtx
	f.mem.Init(evalCtx)
	f.funcs.Init(f)
	f.matchedRule = nil
	f.appliedRule = nil
	f.ruleCycles.init()
}

// DisableOptimizations disables all transformation rules. The unaltered input
// expression tree becomes the output expression tree (because no transforms
// are applied).
func (f *Factory) DisableOptimizations() {
	f.NotifyOnMatchedRule(func(opt.RuleName) bool { return false })
}

// NotifyOnMatchedRule sets a callback function which is invoked each time a
// normalize rule has been matched by the factory. If matchedRule is nil, then
// no further notifications are sent, and all rules are applied by default. In
// addition, callers can invoke the DisableOptimizations convenience method to
// disable all rules.
func (f *Factory) NotifyOnMatchedRule(matchedRule MatchedRuleFunc) {
	f.matchedRule = matchedRule
}

// NotifyOnAppliedRule sets a callback function which is invoked each time a
// normalize rule has been applied by the factory. If appliedRule is nil, then
// no further notifications are sent.
func (f *Factory) NotifyOnAppliedRule(appliedRule AppliedRuleFunc) {
	f.appliedRule = appliedRule
}

// Memo returns the memo structure that the factory is operating upon.
func (f *Factory) Memo() *memo.Memo {
	return &f.mem
}

// Metadata returns the query-specific metadata, which includes information
// about the columns and tables used in this particular query.
func (f *Factory) Metadata() *opt.Metadata {
	return f.mem.Metadata()
}

// CustomFuncs returns the set of custom functions used by normalization rules.
func (f *Factory) CustomFuncs() *CustomFuncs {
	return &f.funcs
}

// InternList adds the given list of group IDs to memo storage and returns an
// ID that can be used for later lookup. If the same list was added previously,
// this method is a no-op and returns the ID of the previous value.
func (f *Factory) InternList(items []memo.GroupID) memo.ListID {
	return f.mem.InternList(items)
}

// AssignPlaceholders is used just before execution of a prepared Memo. It walks
// the tree, replacing any placeholder it finds with its assigned value. This
// will trigger the rebuild of that node's ancestors, as well as triggering
// additional normalization rules that can substantially rewrite the tree. Once
// all placeholders are assigned, the exploration phase can begin.
func (f *Factory) AssignPlaceholders() {
	root := f.assignPlaceholders(f.Memo().RootGroup())
	f.Memo().SetRoot(root, f.Memo().RootProps())
}

// onConstruct is called as a final step by each factory construction method,
// so that any custom manual pattern matching/replacement code can be run.
func (f *Factory) onConstruct(e memo.Expr) memo.GroupID {
	ev := f.mem.MemoizeNormExpr(f.evalCtx, e)

	// RaceEnabled ensures that checks are run on every PR (as part of make
	// testrace) while keeping the check code out of non-test builds.
	if util.RaceEnabled {
		f.checkExpr(ev)
	}
	return ev.Group()
}

// ----------------------------------------------------------------------
//
// Convenience construction methods.
//
// ----------------------------------------------------------------------

// ConstructConstVal constructs one of the constant value operators from the
// given datum value. While most constants are represented with Const, there are
// special-case operators for True, False, and Null, to make matching easier.
func (f *Factory) ConstructConstVal(d tree.Datum) memo.GroupID {
	if d == tree.DNull {
		return f.ConstructNull(f.InternType(types.Unknown))
	}
	if boolVal, ok := d.(*tree.DBool); ok {
		// Map True/False datums to True/False operator.
		if *boolVal {
			return f.ConstructTrue()
		}
		return f.ConstructFalse()
	}
	return f.ConstructConst(f.InternDatum(d))
}

// ConstructSimpleProject is a convenience wrapper for calling
// ConstructProject when there are no synthesized columns.
func (f *Factory) ConstructSimpleProject(
	input memo.GroupID, passthroughCols opt.ColSet,
) memo.GroupID {
	def := memo.ProjectionsOpDef{PassthroughCols: passthroughCols}
	return f.ConstructProject(
		input,
		f.ConstructProjections(memo.EmptyList, f.InternProjectionsOpDef(&def)),
	)
}

// projectExtraCol constructs a new Project operator that passes through all
// columns in the given "in" expression, and then adds the given "extra"
// expression as an additional column.
func (f *Factory) projectExtraCol(in, extra memo.GroupID, extraID opt.ColumnID) memo.GroupID {
	pb := projectionsBuilder{f: f}
	pb.addPassthroughCols(f.funcs.OutputCols(in))
	pb.addSynthesized(extra, extraID)
	return f.ConstructProject(in, pb.buildProjections())
}

// ruleCycles implements a simple stack of "seen" memo expression fingerprints
// so that it can detect rule cycles. If the same expression is repeatedly
// generated by nested rule invocations, then there must be a cycle. Allowing
// rules to cycle with one another is beneficial, so long as there is a way to
// detect the cycle and stop the recursive rule application (which is what this
// struct enables).
//
// As an example of beneficial rule cycling, the filter pushdown rules try to
// push a Select operator down the tree, whereas the decorrelation rules try to
// pull it up. The rules are tagged to detect cycles, so that they work in
// harmony, rather than triggering a stack overflow.
type ruleCycles struct {
	stack []memo.Fingerprint
}

func (rc *ruleCycles) init() {
	if rc.stack != nil {
		rc.stack = rc.stack[:0]
	}
}

// detectCycle returns true if a cycle has been detected, which happens if the
// expression fingerprint is already on the stack.
func (rc *ruleCycles) detectCycle(f memo.Fingerprint) bool {
	for _, existing := range rc.stack {
		if existing == f {
			return true
		}
	}
	return false
}

// push adds the given fingerprint to the stack, so that recursively invoked
// rules can detect whether they generate this same expression.
func (rc *ruleCycles) push(f memo.Fingerprint) {
	rc.stack = append(rc.stack, f)
}

// pop removes the last fingerprint that was added to the stack.
func (rc *ruleCycles) pop() {
	rc.stack = rc.stack[:len(rc.stack)-1]
}
