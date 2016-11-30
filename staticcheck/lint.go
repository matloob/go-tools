// Package staticcheck contains a linter for Go source code.
package staticcheck // import "honnef.co/go/staticcheck"

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/token"
	"go/types"
	htmltemplate "html/template"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	texttemplate "text/template"
	"time"
	"unicode/utf8"

	"honnef.co/go/lint"
	"honnef.co/go/ssa"
	"honnef.co/go/staticcheck/vrp"

	"golang.org/x/tools/go/ast/astutil"
)

type Checker struct {
	terminatesCache map[*ssa.Function]bool
	ranges          map[*ssa.Function]vrp.Ranges
}

func NewChecker() *Checker {
	return &Checker{}
}

func (c *Checker) Funcs() map[string]lint.Func {
	return map[string]lint.Func{
		"SA1000": c.CheckRegexps,
		"SA1001": c.CheckTemplate,
		"SA1002": c.CheckTimeParse,
		"SA1003": c.CheckEncodingBinary,
		"SA1004": c.CheckTimeSleepConstant,
		"SA1005": c.CheckExec,
		"SA1006": c.CheckUnsafePrintf,
		"SA1007": c.CheckURLs,
		"SA1008": c.CheckCanonicalHeaderKey,
		"SA1009": nil,
		"SA1010": c.CheckRegexpFindAll,
		"SA1011": c.CheckUTF8Cutset,
		"SA1012": c.CheckNilContext,
		"SA1013": c.CheckSeeker,
		"SA1014": c.CheckUnmarshalPointer,
		"SA1015": c.CheckLeakyTimeTick,
		"SA1016": CheckUntrappableSignal,

		"SA2000": c.CheckWaitgroupAdd,
		"SA2001": c.CheckEmptyCriticalSection,
		"SA2002": c.CheckConcurrentTesting,
		"SA2003": c.CheckDeferLock,

		"SA3000": c.CheckTestMainExit,
		"SA3001": c.CheckBenchmarkN,

		"SA4000": c.CheckLhsRhsIdentical,
		"SA4001": c.CheckIneffectiveCopy,
		"SA4002": c.CheckDiffSizeComparison,
		"SA4003": c.CheckUnsignedComparison,
		"SA4004": c.CheckIneffectiveLoop,
		"SA4005": c.CheckIneffecitiveFieldAssignments,
		"SA4006": c.CheckUnreadVariableValues,
		// "SA4007": c.CheckPredeterminedBooleanExprs,
		"SA4007": nil,
		"SA4008": c.CheckLoopCondition,
		"SA4009": c.CheckArgOverwritten,
		"SA4010": c.CheckIneffectiveAppend,
		"SA4011": c.CheckScopedBreak,
		"SA4012": c.CheckNaNComparison,
		"SA4013": c.CheckDoubleNegation,
		"SA4014": c.CheckRepeatedIfElse,

		"SA5000": c.CheckNilMaps,
		"SA5001": c.CheckEarlyDefer,
		"SA5002": c.CheckInfiniteEmptyLoop,
		"SA5003": c.CheckDeferInInfiniteLoop,
		"SA5004": c.CheckLoopEmptyDefault,
		"SA5005": c.CheckCyclicFinalizer,
		"SA5006": c.CheckSliceOutOfBounds,
		"SA5007": c.CheckInfiniteRecursion,

		"SA9000": c.CheckDubiousSyncPoolPointers,
		"SA9001": c.CheckDubiousDeferInChannelRangeLoop,

		//"SA0000": c.DumpGraph,
	}
}

func (c *Checker) Init(prog *lint.Program) {
	c.terminatesCache = map[*ssa.Function]bool{}
	c.ranges = map[*ssa.Function]vrp.Ranges{}

	var fns []*ssa.Function
	for _, pkg := range prog.Packages {
		for _, m := range pkg.SSAPkg.Members {
			if fn, ok := m.(*ssa.Function); ok {
				fns = append(fns, fn)
			}
			if typ, ok := m.(*ssa.Type); ok {
				ttyp := typ.Type().(*types.Named)
				if _, ok := ttyp.Underlying().(*types.Interface); ok {
					continue
				}
				ptr := types.NewPointer(ttyp)
				ms := pkg.SSAPkg.Prog.MethodSets.MethodSet(ptr)
				for i := 0; i < ms.Len(); i++ {
					fns = append(fns, pkg.SSAPkg.Prog.MethodValue(ms.At(i)))
				}
				ms = pkg.SSAPkg.Prog.MethodSets.MethodSet(ttyp)
				for i := 0; i < ms.Len(); i++ {
					fns = append(fns, pkg.SSAPkg.Prog.MethodValue(ms.At(i)))
				}
			}
		}
	}

	for _, fn := range fns {
		if fn.Blocks == nil {
			continue
		}
		detectInfiniteLoops(fn)
		ssa.OptimizeBlocks(fn)

		g := vrp.BuildGraph(fn)
		c.ranges[fn] = g.Solve()
	}
}

func detectInfiniteLoops(fn *ssa.Function) {
	if len(fn.Blocks) == 0 {
		return
	}

	// Detect loops that can terminate from a compiler POV, but can't
	// due to stdlib behaviour
	for _, block := range fn.Blocks {
		if len(block.Instrs) < 3 {
			continue
		}
		if len(block.Succs) != 2 {
			continue
		}
		var instrs []*ssa.Instruction
		for i, ins := range block.Instrs {
			if _, ok := ins.(*ssa.DebugRef); ok {
				continue
			}
			instrs = append(instrs, &block.Instrs[i])
		}

		for i, ins := range instrs {
			unop, ok := (*ins).(*ssa.UnOp)
			if !ok || unop.Op != token.ARROW {
				continue
			}
			call, ok := unop.X.(*ssa.Call)
			if !ok || call.Common().IsInvoke() {
				continue
			}
			fn, ok := call.Common().Value.(*ssa.Function)
			if !ok {
				continue
			}
			tfn, ok := fn.Object().(*types.Func)
			if !ok {
				continue
			}
			if tfn.FullName() != "time.Tick" {
				continue
			}
			// XXX check if we're extracting ok from our unop
			if _, ok := (*instrs[i+1]).(*ssa.Extract); !ok {
				continue
			}
			// XXX check that we're branching on our extract result
			if _, ok := (*instrs[i+2]).(*ssa.If); !ok {
				continue
			}

			loop := false
			for _, pred := range block.Preds {
				// This will only detect natural loops, which is fine
				// for detecting `for range`.
				if block.Dominates(pred) {
					loop = true
					break
				}
			}
			if !loop {
				continue
			}
			*instrs[i+2] = ssa.NewJump(block)
			succ := block.Succs[1]
			block.Succs = block.Succs[0:1]
			succ.RemovePred(block)
		}
	}
}

func constantString(f *lint.File, expr ast.Expr) (string, bool) {
	val := f.Pkg.TypesInfo.Types[expr].Value
	if val == nil {
		return "", false
	}
	if val.Kind() != constant.String {
		return "", false
	}
	return constant.StringVal(val), true
}

func hasType(f *lint.File, expr ast.Expr, name string) bool {
	return types.TypeString(f.Pkg.TypesInfo.TypeOf(expr), nil) == name
}

// terminates reports whether fn is supposed to return, that is if it
// has at least one theoretic path that returns from the function.
// Explicit panics do not count as terminating.
func (c *Checker) terminates(fn *ssa.Function) (ret bool) {
	if b, ok := c.terminatesCache[fn]; ok {
		return b
	}
	defer func() {
		c.terminatesCache[fn] = ret
	}()
	if fn.Blocks == nil {
		// assuming that a function terminates is the conservative
		// choice
		return true
	}

	// Detect ranging over a time.Tick channel
	for _, block := range fn.Blocks {
		if len(block.Instrs) == 0 {
			continue
		}
		if _, ok := block.Instrs[len(block.Instrs)-1].(*ssa.Return); ok {
			return true
		}
	}
	return false
}

func CheckUntrappableSignal(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "signal", "Ignore") &&
			!lint.IsPkgDot(call.Fun, "signal", "Notify") &&
			!lint.IsPkgDot(call.Fun, "signal", "Reset") {
			return true
		}
		for _, arg := range call.Args {
			if conv, ok := arg.(*ast.CallExpr); ok && lint.IsPkgDot(conv.Fun, "os", "Signal") {
				arg = conv.Args[0]
			}

			if lint.IsPkgDot(arg, "os", "Kill") || lint.IsPkgDot(arg, "syscall", "SIGKILL") {
				f.Errorf(arg, "%s cannot be trapped (did you mean syscall.SIGTERM?)", f.Render(arg))
			}
			if lint.IsPkgDot(arg, "syscall", "SIGSTOP") {
				f.Errorf(arg, "%s signal cannot be trapped", f.Render(arg))
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckRegexps(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "regexp", "MustCompile") &&
			!lint.IsPkgDot(call.Fun, "regexp", "Compile") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		s, ok := constantString(f, call.Args[0])
		if !ok {
			return true
		}
		_, err := regexp.Compile(s)
		if err != nil {
			f.Errorf(call.Args[0], "%s", err)
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckTemplate(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		var kind string
		if isFunctionCallName(f, call, "(*text/template.Template).Parse") {
			kind = "text"
		} else if isFunctionCallName(f, call, "(*html/template.Template).Parse") {
			kind = "html"
		} else {
			return true
		}
		sel := call.Fun.(*ast.SelectorExpr)
		if !isFunctionCallName(f, sel.X, "text/template.New") &&
			!isFunctionCallName(f, sel.X, "html/template.New") {
			// TODO(dh): this is a cheap workaround for templates with
			// different delims. A better solution with less false
			// negatives would use data flow analysis to see where the
			// template comes from and where it has been
			return true
		}
		s, ok := constantString(f, call.Args[0])
		if !ok {
			return true
		}
		var err error
		switch kind {
		case "text":
			_, err = texttemplate.New("").Parse(s)
		case "html":
			_, err = htmltemplate.New("").Parse(s)
		}
		if err != nil {
			// TODO(dominikh): whitelist other parse errors, if any
			if strings.Contains(err.Error(), "unexpected") {
				f.Errorf(call.Args[0], "%s", err)
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckTimeParse(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "time", "Parse") {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		s, ok := constantString(f, call.Args[0])
		if !ok {
			return true
		}
		s = strings.Replace(s, "_", " ", -1)
		s = strings.Replace(s, "Z", "-", -1)
		_, err := time.Parse(s, s)
		if err != nil {
			f.Errorf(call.Args[0], "%s", err)
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckEncodingBinary(f *lint.File) {
	// TODO(dominikh): also check binary.Read
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "binary", "Write") {
			return true
		}
		if len(call.Args) != 3 {
			return true
		}
		typ := f.Pkg.TypesInfo.TypeOf(call.Args[2])
		if typ == nil {
			return true
		}
		dataType := typ.Underlying()
		if typ, ok := dataType.(*types.Pointer); ok {
			dataType = typ.Elem().Underlying()
		}
		if typ, ok := dataType.(interface {
			Elem() types.Type
		}); ok {
			if _, ok := typ.(*types.Pointer); !ok {
				dataType = typ.Elem()
			}
		}

		if validEncodingBinaryType(dataType) {
			return true
		}
		f.Errorf(call.Args[2], "type %s cannot be used with binary.Write",
			f.Pkg.TypesInfo.TypeOf(call.Args[2]))
		return true
	}
	f.Walk(fn)
}

func validEncodingBinaryType(typ types.Type) bool {
	typ = typ.Underlying()
	switch typ := typ.(type) {
	case *types.Basic:
		switch typ.Kind() {
		case types.Uint8, types.Uint16, types.Uint32, types.Uint64,
			types.Int8, types.Int16, types.Int32, types.Int64,
			types.Float32, types.Float64, types.Complex64, types.Complex128, types.Invalid:
			return true
		}
		return false
	case *types.Struct:
		n := typ.NumFields()
		for i := 0; i < n; i++ {
			if !validEncodingBinaryType(typ.Field(i).Type()) {
				return false
			}
		}
		return true
	case *types.Array:
		return validEncodingBinaryType(typ.Elem())
	case *types.Interface:
		// we can't determine if it's a valid type or not
		return true
	}
	return false
}

func (c *Checker) CheckTimeSleepConstant(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "time", "Sleep") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		lit, ok := call.Args[0].(*ast.BasicLit)
		if !ok {
			return true
		}
		n, err := strconv.Atoi(lit.Value)
		if err != nil {
			return true
		}
		if n == 0 || n > 120 {
			// time.Sleep(0) is a seldomly used pattern in concurrency
			// tests. >120 might be intentional. 120 was chosen
			// because the user could've meant 2 minutes.
			return true
		}
		recommendation := "time.Sleep(time.Nanosecond)"
		if n != 1 {
			recommendation = fmt.Sprintf("time.Sleep(%d * time.Nanosecond)", n)
		}
		f.Errorf(call.Args[0], "sleeping for %d nanoseconds is probably a bug. Be explicit if it isn't: %s", n, recommendation)
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckWaitgroupAdd(f *lint.File) {
	fn := func(node ast.Node) bool {
		g, ok := node.(*ast.GoStmt)
		if !ok {
			return true
		}
		fun, ok := g.Call.Fun.(*ast.FuncLit)
		if !ok {
			return true
		}
		if len(fun.Body.List) == 0 {
			return true
		}
		stmt, ok := fun.Body.List[0].(*ast.ExprStmt)
		if !ok {
			return true
		}
		call, ok := stmt.X.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		fn, ok := f.Pkg.TypesInfo.ObjectOf(sel.Sel).(*types.Func)
		if !ok {
			return true
		}
		if fn.FullName() == "(*sync.WaitGroup).Add" {
			f.Errorf(sel, "should call %s before starting the goroutine to avoid a race",
				f.Render(stmt))
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckInfiniteEmptyLoop(f *lint.File) {
	fn := func(node ast.Node) bool {
		loop, ok := node.(*ast.ForStmt)
		if !ok || len(loop.Body.List) != 0 || loop.Cond != nil || loop.Init != nil {
			return true
		}
		f.Errorf(loop, "should not use an infinite empty loop. It will spin. Consider select{} instead.")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckDeferInInfiniteLoop(f *lint.File) {
	fn := func(node ast.Node) bool {
		mightExit := false
		var defers []ast.Stmt
		loop, ok := node.(*ast.ForStmt)
		if !ok || loop.Cond != nil {
			return true
		}
		fn2 := func(node ast.Node) bool {
			switch stmt := node.(type) {
			case *ast.ReturnStmt:
				mightExit = true
			case *ast.BranchStmt:
				// TODO(dominikh): if this sees a break in a switch or
				// select, it doesn't check if it breaks the loop or
				// just the select/switch. This causes some false
				// negatives.
				if stmt.Tok == token.BREAK {
					mightExit = true
				}
			case *ast.DeferStmt:
				defers = append(defers, stmt)
			case *ast.FuncLit:
				// Don't look into function bodies
				return false
			}
			return true
		}
		ast.Inspect(loop.Body, fn2)
		if mightExit {
			return true
		}
		for _, stmt := range defers {
			f.Errorf(stmt, "defers in this infinite loop will never run")
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckDubiousDeferInChannelRangeLoop(f *lint.File) {
	fn := func(node ast.Node) bool {
		var defers []ast.Stmt
		loop, ok := node.(*ast.RangeStmt)
		if !ok {
			return true
		}
		typ := f.Pkg.TypesInfo.TypeOf(loop.X)
		if typ == nil {
			return true
		}
		_, ok = typ.Underlying().(*types.Chan)
		if !ok {
			return true
		}
		fn2 := func(node ast.Node) bool {
			switch stmt := node.(type) {
			case *ast.DeferStmt:
				defers = append(defers, stmt)
			case *ast.FuncLit:
				// Don't look into function bodies
				return false
			}
			return true
		}
		ast.Inspect(loop.Body, fn2)
		for _, stmt := range defers {
			f.Errorf(stmt, "defers in this range loop won't run unless the channel gets closed")
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckTestMainExit(f *lint.File) {
	fn := func(node ast.Node) bool {
		if !IsTestMain(f, node) {
			return true
		}

		arg := f.Pkg.TypesInfo.ObjectOf(node.(*ast.FuncDecl).Type.Params.List[0].Names[0])
		callsRun := false
		fn2 := func(node ast.Node) bool {
			call, ok := node.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if arg != f.Pkg.TypesInfo.ObjectOf(ident) {
				return true
			}
			if sel.Sel.Name == "Run" {
				callsRun = true
				return false
			}
			return true
		}
		ast.Inspect(node.(*ast.FuncDecl).Body, fn2)

		callsExit := false
		fn3 := func(node ast.Node) bool {
			expr, ok := node.(ast.Expr)
			if !ok {
				return true
			}
			if lint.IsPkgDot(expr, "os", "Exit") {
				callsExit = true
				return false
			}
			return true
		}
		ast.Inspect(node.(*ast.FuncDecl).Body, fn3)
		if !callsExit && callsRun {
			f.Errorf(node, "TestMain should call os.Exit to set exit code")
		}
		return true
	}
	f.Walk(fn)
}

func IsTestMain(f *lint.File, node ast.Node) bool {
	decl, ok := node.(*ast.FuncDecl)
	if !ok {
		return false
	}
	if decl.Name.Name != "TestMain" {
		return false
	}
	if len(decl.Type.Params.List) != 1 {
		return false
	}
	arg := decl.Type.Params.List[0]
	if len(arg.Names) != 1 {
		return false
	}
	typ := f.Pkg.TypesInfo.TypeOf(arg.Type)
	return typ != nil && typ.String() == "*testing.M"
}

func (c *Checker) CheckExec(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "exec", "Command") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		val, ok := constantString(f, call.Args[0])
		if !ok {
			return true
		}
		if !strings.Contains(val, " ") || strings.Contains(val, `\`) {
			return true
		}
		f.Errorf(call.Args[0], "first argument to exec.Command looks like a shell command, but a program name or path are expected")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckLoopEmptyDefault(f *lint.File) {
	fn := func(node ast.Node) bool {
		loop, ok := node.(*ast.ForStmt)
		if !ok || len(loop.Body.List) != 1 || loop.Cond != nil || loop.Init != nil {
			return true
		}
		sel, ok := loop.Body.List[0].(*ast.SelectStmt)
		if !ok {
			return true
		}
		for _, c := range sel.Body.List {
			if comm, ok := c.(*ast.CommClause); ok && comm.Comm == nil && len(comm.Body) == 0 {
				f.Errorf(comm, "should not have an empty default case in a for+select loop. The loop will spin.")
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckLhsRhsIdentical(f *lint.File) {
	fn := func(node ast.Node) bool {
		op, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch op.Op {
		case token.EQL, token.NEQ:
			if basic, ok := f.Pkg.TypesInfo.TypeOf(op.X).(*types.Basic); ok {
				if kind := basic.Kind(); kind == types.Float32 || kind == types.Float64 {
					// f == f and f != f might be used to check for NaN
					return true
				}
			}
		case token.SUB, token.QUO, token.AND, token.REM, token.OR, token.XOR, token.AND_NOT,
			token.LAND, token.LOR, token.LSS, token.GTR, token.LEQ, token.GEQ:
		default:
			// For some ops, such as + and *, it can make sense to
			// have identical operands
			return true
		}

		if f.Render(op.X) != f.Render(op.Y) {
			return true
		}
		f.Errorf(op, "identical expressions on the left and right side of the '%s' operator", op.Op)
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckScopedBreak(f *lint.File) {
	fn := func(node ast.Node) bool {
		loop, ok := node.(*ast.ForStmt)
		if !ok {
			return true
		}
		for _, stmt := range loop.Body.List {
			var blocks [][]ast.Stmt
			switch stmt := stmt.(type) {
			case *ast.SwitchStmt:
				for _, c := range stmt.Body.List {
					blocks = append(blocks, c.(*ast.CaseClause).Body)
				}
			case *ast.SelectStmt:
				for _, c := range stmt.Body.List {
					blocks = append(blocks, c.(*ast.CommClause).Body)
				}
			default:
				continue
			}

			for _, body := range blocks {
				if len(body) == 0 {
					continue
				}
				lasts := []ast.Stmt{body[len(body)-1]}
				// TODO(dh): unfold all levels of nested block
				// statements, not just a single level if statement
				if ifs, ok := lasts[0].(*ast.IfStmt); ok {
					if len(ifs.Body.List) == 0 {
						continue
					}
					lasts[0] = ifs.Body.List[len(ifs.Body.List)-1]

					if block, ok := ifs.Else.(*ast.BlockStmt); ok {
						if len(block.List) != 0 {
							lasts = append(lasts, block.List[len(block.List)-1])
						}
					}
				}
				for _, last := range lasts {
					branch, ok := last.(*ast.BranchStmt)
					if !ok || branch.Tok != token.BREAK || branch.Label != nil {
						continue
					}
					f.Errorf(branch, "ineffective break statement. Did you mean to break out of the outer loop?")
				}
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckUnsafePrintf(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "fmt", "Printf") &&
			!lint.IsPkgDot(call.Fun, "fmt", "Sprintf") &&
			!lint.IsPkgDot(call.Fun, "log", "Printf") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		switch call.Args[0].(type) {
		case *ast.CallExpr, *ast.Ident:
		default:
			return true
		}
		f.Errorf(call.Args[0], "printf-style function with dynamic first argument and no further arguments should use print-style function instead")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckURLs(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "url", "Parse") {
			return true
		}
		if len(call.Args) != 1 {
			return true
		}
		s, ok := constantString(f, call.Args[0])
		if !ok {
			return true
		}
		_, err := url.Parse(s)
		if err != nil {
			f.Errorf(call.Args[0], "invalid argument to url.Parse: %s", err)
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckEarlyDefer(f *lint.File) {
	fn := func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if len(block.List) < 2 {
			return true
		}
		for i, stmt := range block.List {
			if i == len(block.List)-1 {
				break
			}
			assign, ok := stmt.(*ast.AssignStmt)
			if !ok {
				continue
			}
			if len(assign.Rhs) != 1 {
				continue
			}
			if len(assign.Lhs) < 2 {
				continue
			}
			if lhs, ok := assign.Lhs[len(assign.Lhs)-1].(*ast.Ident); ok && lhs.Name == "_" {
				continue
			}
			call, ok := assign.Rhs[0].(*ast.CallExpr)
			if !ok {
				continue
			}
			sig, ok := f.Pkg.TypesInfo.TypeOf(call.Fun).(*types.Signature)
			if !ok {
				continue
			}
			if sig.Results().Len() < 2 {
				continue
			}
			last := sig.Results().At(sig.Results().Len() - 1)
			// FIXME(dh): check that it's error from universe, not
			// another type of the same name
			if last.Type().String() != "error" {
				continue
			}
			lhs, ok := assign.Lhs[0].(*ast.Ident)
			if !ok {
				continue
			}
			def, ok := block.List[i+1].(*ast.DeferStmt)
			if !ok {
				continue
			}
			sel, ok := def.Call.Fun.(*ast.SelectorExpr)
			if !ok {
				continue
			}
			ident, ok := selectorX(sel).(*ast.Ident)
			if !ok {
				continue
			}
			if ident.Obj != lhs.Obj {
				continue
			}
			if sel.Sel.Name != "Close" {
				continue
			}
			f.Errorf(def, "should check returned error before deferring %s", f.Render(def.Call))
		}
		return true
	}
	f.Walk(fn)
}

func selectorX(sel *ast.SelectorExpr) ast.Node {
	switch x := sel.X.(type) {
	case *ast.SelectorExpr:
		return selectorX(x)
	default:
		return x
	}
}

func (c *Checker) CheckDubiousSyncPoolPointers(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}

		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "Put" {
			return true
		}
		typ := f.Pkg.TypesInfo.TypeOf(sel.X)
		if typ == nil || (typ.String() != "sync.Pool" && typ.String() != "*sync.Pool") {
			return true
		}

		arg := f.Pkg.TypesInfo.TypeOf(call.Args[0])
		underlying := arg.Underlying()
		switch underlying.(type) {
		case *types.Pointer, *types.Map, *types.Chan, *types.Interface:
			// all pointer types
			return true
		}
		f.Errorf(call.Args[0], "non-pointer type %s put into sync.Pool", arg.String())
		return false
	}
	f.Walk(fn)
}

func (c *Checker) CheckEmptyCriticalSection(f *lint.File) {
	mutexParams := func(s ast.Stmt) (x ast.Expr, funcName string, ok bool) {
		expr, ok := s.(*ast.ExprStmt)
		if !ok {
			return nil, "", false
		}
		call, ok := expr.X.(*ast.CallExpr)
		if !ok {
			return nil, "", false
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return nil, "", false
		}

		fn, ok := f.Pkg.TypesInfo.ObjectOf(sel.Sel).(*types.Func)
		if !ok {
			return nil, "", false
		}
		sig := fn.Type().(*types.Signature)
		if sig.Params().Len() != 0 || sig.Results().Len() != 0 {
			return nil, "", false
		}

		return sel.X, fn.Name(), true
	}

	fn := func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if len(block.List) < 2 {
			return true
		}
		for i := range block.List[:len(block.List)-1] {
			sel1, method1, ok1 := mutexParams(block.List[i])
			sel2, method2, ok2 := mutexParams(block.List[i+1])

			if !ok1 || !ok2 || f.Render(sel1) != f.Render(sel2) {
				continue
			}
			if (method1 == "Lock" && method2 == "Unlock") ||
				(method1 == "RLock" && method2 == "RUnlock") {
				f.Errorf(block.List[i+1], "empty critical section")
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckIneffectiveCopy(f *lint.File) {
	fn := func(node ast.Node) bool {
		if unary, ok := node.(*ast.UnaryExpr); ok {
			if _, ok := unary.X.(*ast.StarExpr); ok && unary.Op == token.AND {
				f.Errorf(unary, "&*x will be simplified to x. It will not copy x.")
			}
		}

		if star, ok := node.(*ast.StarExpr); ok {
			if unary, ok := star.X.(*ast.UnaryExpr); ok && unary.Op == token.AND {
				f.Errorf(star, "*&x will be simplified to x. It will not copy x.")
			}
		}
		return true
	}
	f.Walk(fn)
}

func constantInt(f *lint.File, expr ast.Expr) (int, bool) {
	tv := f.Pkg.TypesInfo.Types[expr]
	if tv.Value == nil {
		return 0, false
	}
	if tv.Value.Kind() != constant.Int {
		return 0, false
	}
	v, ok := constant.Int64Val(tv.Value)
	if !ok {
		return 0, false
	}
	return int(v), true
}

func (c *Checker) CheckDiffSizeComparison(f *lint.File) {
	fn := func(node ast.Node) bool {
		expr, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if expr.Op != token.EQL && expr.Op != token.NEQ {
			return true
		}

		_, isSlice1 := expr.X.(*ast.SliceExpr)
		_, isSlice2 := expr.Y.(*ast.SliceExpr)
		if !isSlice1 && !isSlice2 {
			// Only do the check if at least one side has a slicing
			// expression. Otherwise we'll just run into false
			// positives because of debug toggles and the like.
			return true
		}
		ssafn := f.EnclosingSSAFunction(expr)
		ssaexpr, _ := ssafn.ValueForExpr(expr)
		binop, ok := ssaexpr.(*ssa.BinOp)
		if !ok {
			return true
		}
		r1, ok1 := c.ranges[ssafn].Get(binop.X).(vrp.StringInterval)
		r2, ok2 := c.ranges[ssafn].Get(binop.Y).(vrp.StringInterval)
		if !ok1 || !ok2 {
			return true
		}
		if r1.Length.Intersection(r2.Length).Empty() {
			f.Errorf(expr, "comparing strings of different sizes for equality will always return false")
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckCanonicalHeaderKey(f *lint.File) {
	fn := func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if ok {
			// TODO(dh): This risks missing some Header reads, for
			// example in `h1["foo"] = h2["foo"]` – these edge
			// cases are probably rare enough to ignore for now.
			for _, expr := range assign.Lhs {
				op, ok := expr.(*ast.IndexExpr)
				if !ok {
					continue
				}
				if hasType(f, op.X, "net/http.Header") {
					return false
				}
			}
			return true
		}
		op, ok := node.(*ast.IndexExpr)
		if !ok {
			return true
		}
		if !hasType(f, op.X, "net/http.Header") {
			return true
		}
		s, ok := constantString(f, op.Index)
		if !ok {
			return true
		}
		if s == http.CanonicalHeaderKey(s) {
			return true
		}
		f.Errorf(op, "keys in http.Header are canonicalized, %q is not canonical; fix the constant or use http.CanonicalHeaderKey", s)
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckBenchmarkN(f *lint.File) {
	fn := func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok {
			return true
		}
		if len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		sel, ok := assign.Lhs[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "N" {
			return true
		}
		if !hasType(f, sel.X, "*testing.B") {
			return true
		}
		f.Errorf(assign, "should not assign to %s", f.Render(sel))
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckIneffecitiveFieldAssignments(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fn.Recv == nil {
			return true
		}
		ssafn := f.Pkg.SSAPkg.Prog.FuncValue(f.Pkg.TypesInfo.ObjectOf(fn.Name).(*types.Func))
		if ssafn == nil {
			return true
		}

		if len(ssafn.Blocks) == 0 {
			// External function
			return true
		}

		reads := map[*ssa.BasicBlock]map[ssa.Value]bool{}
		writes := map[*ssa.BasicBlock]map[ssa.Value]bool{}

		recv := ssafn.Params[0]
		if _, ok := recv.Type().Underlying().(*types.Struct); !ok {
			return true
		}
		recvPtrs := map[ssa.Value]bool{
			recv: true,
		}
		if len(ssafn.Locals) == 0 || ssafn.Locals[0].Heap {
			return true
		}
		blocks := ssafn.DomPreorder()
		for _, block := range blocks {
			if writes[block] == nil {
				writes[block] = map[ssa.Value]bool{}
			}
			if reads[block] == nil {
				reads[block] = map[ssa.Value]bool{}
			}

			for _, ins := range block.Instrs {
				switch ins := ins.(type) {
				case *ssa.Store:
					if recvPtrs[ins.Val] {
						recvPtrs[ins.Addr] = true
					}
					fa, ok := ins.Addr.(*ssa.FieldAddr)
					if !ok {
						continue
					}
					if !recvPtrs[fa.X] {
						continue
					}
					writes[block][fa] = true
				case *ssa.UnOp:
					if ins.Op != token.MUL {
						continue
					}
					if recvPtrs[ins.X] {
						reads[block][ins] = true
						continue
					}
					fa, ok := ins.X.(*ssa.FieldAddr)
					if !ok {
						continue
					}
					if !recvPtrs[fa.X] {
						continue
					}
					reads[block][fa] = true
				}
			}
		}

		for block, writes := range writes {
			seen := map[*ssa.BasicBlock]bool{}
			var hasRead func(block *ssa.BasicBlock, write *ssa.FieldAddr) bool
			hasRead = func(block *ssa.BasicBlock, write *ssa.FieldAddr) bool {
				seen[block] = true
				for read := range reads[block] {
					switch ins := read.(type) {
					case *ssa.FieldAddr:
						if ins.Field == write.Field && read.Pos() > write.Pos() {
							return true
						}
					case *ssa.UnOp:
						if ins.Pos() >= write.Pos() {
							return true
						}
					}
				}
				for _, succ := range block.Succs {
					if !seen[succ] {
						if hasRead(succ, write) {
							return true
						}
					}
				}
				return false
			}
			for write := range writes {
				fa := write.(*ssa.FieldAddr)
				if !hasRead(block, fa) {
					name := recv.Type().Underlying().(*types.Struct).Field(fa.Field).Name()
					f.Errorf(fa, "ineffective assignment to field %s", name)
				}
			}
		}

		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckUnreadVariableValues(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ssafn := f.EnclosingSSAFunction(fn)
		if ssafn == nil {
			return true
		}
		ast.Inspect(fn, func(node ast.Node) bool {
			assign, ok := node.(*ast.AssignStmt)
			if !ok {
				return true
			}
			if len(assign.Lhs) != len(assign.Rhs) {
				return true
			}
			for i, lhs := range assign.Lhs {
				rhs := assign.Rhs[i]
				if ident, ok := lhs.(*ast.Ident); !ok || ok && ident.Name == "_" {
					continue
				}
				val, _ := ssafn.ValueForExpr(rhs)
				if val == nil {
					continue
				}

				refs := val.Referrers()
				if refs == nil {
					// TODO investigate why refs can be nil
					return true
				}
				if len(filterDebug(*val.Referrers())) == 0 {
					f.Errorf(node, "this value of %s is never used", lhs)
				}
			}
			return true
		})
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckPredeterminedBooleanExprs(f *lint.File) {
	fn := func(node ast.Node) bool {
		binop, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		switch binop.Op {
		case token.GTR, token.LSS, token.EQL, token.NEQ, token.LEQ, token.GEQ:
		default:
			return true
		}
		fn := f.EnclosingSSAFunction(binop)
		if fn == nil {
			return true
		}
		val, _ := fn.ValueForExpr(binop)
		ssabinop, ok := val.(*ssa.BinOp)
		if !ok {
			return true
		}
		xs, ok1 := consts(ssabinop.X, nil, nil)
		ys, ok2 := consts(ssabinop.Y, nil, nil)
		if !ok1 || !ok2 || len(xs) == 0 || len(ys) == 0 {
			return true
		}

		trues := 0
		for _, x := range xs {
			for _, y := range ys {
				if x.Value == nil {
					if y.Value == nil {
						trues++
					}
					continue
				}
				if constant.Compare(x.Value, ssabinop.Op, y.Value) {
					trues++
				}
			}
		}
		b := trues != 0
		if trues == 0 || trues == len(xs)*len(ys) {
			f.Errorf(binop, "%s is always %t for all possible values (%s %s %s)",
				f.Render(binop), b, xs, binop.Op, ys)
		}

		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckNilMaps(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ssafn := f.Pkg.SSAPkg.Prog.FuncValue(f.Pkg.TypesInfo.ObjectOf(fn.Name).(*types.Func))
		if ssafn == nil {
			return true
		}

		for _, block := range ssafn.Blocks {
			for _, ins := range block.Instrs {
				mu, ok := ins.(*ssa.MapUpdate)
				if !ok {
					continue
				}
				c, ok := mu.Map.(*ssa.Const)
				if !ok {
					continue
				}
				if c.Value != nil {
					continue
				}
				f.Errorf(mu, "assignment to nil map")
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckUnsignedComparison(f *lint.File) {
	fn := func(node ast.Node) bool {
		expr, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		tx := f.Pkg.TypesInfo.TypeOf(expr.X)
		ty := f.Pkg.TypesInfo.TypeOf(expr.Y)
		if tx == nil || ty == nil {
			return true
		}
		basic, ok := tx.Underlying().(*types.Basic)
		if !ok {
			return true
		}
		if (basic.Info() & types.IsUnsigned) == 0 {
			return true
		}
		lit, ok := expr.Y.(*ast.BasicLit)
		if !ok || lit.Value != "0" {
			return true
		}
		switch expr.Op {
		case token.GEQ:
			f.Errorf(expr, "unsigned values are always >= 0")
		case token.LSS:
			f.Errorf(expr, "unsigned values are never < 0")
		case token.LEQ:
			f.Errorf(expr, "'x <= 0' for unsigned values of x is the same as 'x == 0'")
		}
		return true
	}
	f.Walk(fn)
}
func filterDebug(instr []ssa.Instruction) []ssa.Instruction {
	var out []ssa.Instruction
	for _, ins := range instr {
		if _, ok := ins.(*ssa.DebugRef); !ok {
			out = append(out, ins)
		}
	}
	return out
}

func consts(val ssa.Value, out []*ssa.Const, visitedPhis map[string]bool) ([]*ssa.Const, bool) {
	if visitedPhis == nil {
		visitedPhis = map[string]bool{}
	}
	var ok bool
	switch val := val.(type) {
	case *ssa.Phi:
		if visitedPhis[val.Name()] {
			break
		}
		visitedPhis[val.Name()] = true
		vals := val.Operands(nil)
		for _, phival := range vals {
			out, ok = consts(*phival, out, visitedPhis)
			if !ok {
				return nil, false
			}
		}
	case *ssa.Const:
		out = append(out, val)
	case *ssa.Convert:
		out, ok = consts(val.X, out, visitedPhis)
		if !ok {
			return nil, false
		}
	default:
		return nil, false
	}
	if len(out) < 2 {
		return out, true
	}
	uniq := []*ssa.Const{out[0]}
	for _, val := range out[1:] {
		if val.Value == uniq[len(uniq)-1].Value {
			continue
		}
		uniq = append(uniq, val)
	}
	return uniq, true
}

func (c *Checker) CheckLoopCondition(f *lint.File) {
	fn := func(node ast.Node) bool {
		loop, ok := node.(*ast.ForStmt)
		if !ok {
			return true
		}
		if loop.Init == nil || loop.Cond == nil || loop.Post == nil {
			return true
		}
		init, ok := loop.Init.(*ast.AssignStmt)
		if !ok || len(init.Lhs) != 1 || len(init.Rhs) != 1 {
			return true
		}
		cond, ok := loop.Cond.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		x, ok := cond.X.(*ast.Ident)
		if !ok {
			return true
		}
		lhs, ok := init.Lhs[0].(*ast.Ident)
		if !ok {
			return true
		}
		if x.Obj != lhs.Obj {
			return true
		}
		if _, ok := loop.Post.(*ast.IncDecStmt); !ok {
			return true
		}

		ssafn := f.EnclosingSSAFunction(cond)
		if ssafn == nil {
			return true
		}
		v, isAddr := ssafn.ValueForExpr(cond.X)
		if v == nil || isAddr {
			return true
		}
		switch v := v.(type) {
		case *ssa.Phi:
			ops := v.Operands(nil)
			if len(ops) != 2 {
				return true
			}
			_, ok := (*ops[0]).(*ssa.Const)
			if !ok {
				return true
			}
			sigma, ok := (*ops[1]).(*ssa.Sigma)
			if !ok {
				return true
			}
			if sigma.X != v {
				return true
			}
		case *ssa.UnOp:
			return true
		}
		f.Errorf(cond, "variable in loop condition never changes")

		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckArgOverwritten(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fn.Body == nil {
			return true
		}
		ssafn := f.EnclosingSSAFunction(fn)
		if ssafn == nil {
			return true
		}
		if len(fn.Type.Params.List) == 0 {
			return true
		}
		for _, field := range fn.Type.Params.List {
			for _, arg := range field.Names {
				obj := f.Pkg.TypesInfo.ObjectOf(arg)
				var ssaobj *ssa.Parameter
				for _, param := range ssafn.Params {
					if param.Object() == obj {
						ssaobj = param
						break
					}
				}
				if ssaobj == nil {
					continue
				}
				refs := ssaobj.Referrers()
				if refs == nil {
					continue
				}
				if len(filterDebug(*refs)) != 0 {
					continue
				}

				assigned := false
				ast.Inspect(fn.Body, func(node ast.Node) bool {
					assign, ok := node.(*ast.AssignStmt)
					if !ok {
						return true
					}
					for _, lhs := range assign.Lhs {
						ident, ok := lhs.(*ast.Ident)
						if !ok {
							continue
						}
						if f.Pkg.TypesInfo.ObjectOf(ident) == obj {
							assigned = true
							return false
						}
					}
					return true
				})
				if assigned {
					f.Errorf(arg, "argument %s is overwritten before first use", arg)
				}
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckIneffectiveLoop(f *lint.File) {
	// This check detects some, but not all unconditional loop exits.
	// We give up in the following cases:
	//
	// - a goto anywhere in the loop. The goto might skip over our
	// return, and we don't check that it doesn't.
	//
	// - any nested, unlabelled continue, even if it is in another
	// loop or closure.
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		if fn.Body == nil {
			return true
		}
		labels := map[*ast.Object]ast.Stmt{}
		ast.Inspect(fn.Body, func(node ast.Node) bool {
			label, ok := node.(*ast.LabeledStmt)
			if !ok {
				return true
			}
			labels[label.Label.Obj] = label.Stmt
			return true
		})

		ast.Inspect(fn.Body, func(node ast.Node) bool {
			var loop ast.Node
			var body *ast.BlockStmt
			switch node := node.(type) {
			case *ast.ForStmt:
				body = node.Body
				loop = node
			case *ast.RangeStmt:
				typ := f.Pkg.TypesInfo.TypeOf(node.X)
				if typ == nil {
					return true
				}
				if _, ok := typ.Underlying().(*types.Map); ok {
					// looping once over a map is a valid pattern for
					// getting an arbitrary element.
					return true
				}
				body = node.Body
				loop = node
			default:
				return true
			}
			if len(body.List) < 2 {
				// avoid flagging the somewhat common pattern of using
				// a range loop to get the first element in a slice,
				// or the first rune in a string.
				return true
			}
			var unconditionalExit ast.Node
			hasBranching := false
			for _, stmt := range body.List {
				switch stmt := stmt.(type) {
				case *ast.BranchStmt:
					switch stmt.Tok {
					case token.BREAK:
						if stmt.Label == nil || labels[stmt.Label.Obj] == loop {
							unconditionalExit = stmt
						}
					case token.CONTINUE:
						if stmt.Label == nil || labels[stmt.Label.Obj] == loop {
							unconditionalExit = nil
							return false
						}
					}
				case *ast.ReturnStmt:
					unconditionalExit = stmt
				case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt, *ast.SwitchStmt, *ast.SelectStmt:
					hasBranching = true
				}
			}
			if unconditionalExit == nil || !hasBranching {
				return false
			}
			ast.Inspect(body, func(node ast.Node) bool {
				if branch, ok := node.(*ast.BranchStmt); ok {

					switch branch.Tok {
					case token.GOTO:
						unconditionalExit = nil
						return false
					case token.CONTINUE:
						if branch.Label != nil && labels[branch.Label.Obj] != loop {
							return true
						}
						unconditionalExit = nil
						return false
					}
				}
				return true
			})
			if unconditionalExit != nil {
				f.Errorf(unconditionalExit, "the surrounding loop is unconditionally terminated")
			}
			return true
		})
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckRegexpFindAll(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if !hasType(f, sel.X, "*regexp.Regexp") {
			return true
		}
		if !strings.HasPrefix(sel.Sel.Name, "FindAll") {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		lit, ok := call.Args[1].(*ast.BasicLit)
		if !ok || lit.Value != "0" {
			return true
		}
		f.Errorf(lit, "calling a FindAll method with n == 0 will return no results, did you mean -1?")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckUTF8Cutset(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || !lint.IsIdent(sel.X, "strings") {
			return true
		}
		switch sel.Sel.Name {
		case "IndexAny", "LastIndexAny", "ConstainsAny", "Trim", "TrimLeft", "TrimRight":
		default:
			return true
		}
		s, ok := constantString(f, call.Args[1])
		if !ok {
			return true
		}
		if !utf8.ValidString(s) {
			f.Errorf(call.Args[1], "the second argument to %s should be a valid UTF-8 encoded string", f.Render(call.Fun))
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckNilContext(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if len(call.Args) == 0 {
			return true
		}
		if typ, ok := f.Pkg.TypesInfo.TypeOf(call.Args[0]).(*types.Basic); !ok || typ.Kind() != types.UntypedNil {
			return true
		}
		sig, ok := f.Pkg.TypesInfo.TypeOf(call.Fun).(*types.Signature)
		if !ok {
			return true
		}
		if sig.Params().Len() == 0 {
			return true
		}
		if types.TypeString(sig.Params().At(0).Type(), nil) != "context.Context" {
			return true
		}
		f.Errorf(call.Args[0],
			"do not pass a nil Context, even if a function permits it; pass context.TODO if you are unsure about which Context to use")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckSeeker(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if sel.Sel.Name != "Seek" {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		arg0, ok := call.Args[0].(*ast.SelectorExpr)
		if !ok {
			return true
		}
		switch arg0.Sel.Name {
		case "SeekStart", "SeekCurrent", "SeekEnd":
		default:
			return true
		}
		pkg, ok := arg0.X.(*ast.Ident)
		if !ok {
			return true
		}
		if pkg.Name != "io" {
			return true
		}
		f.Errorf(call, "the first argument of io.Seeker is the offset, but an io.Seek* constant is being used instead")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckIneffectiveAppend(f *lint.File) {
	fn := func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || ident.Name == "_" {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		if callIdent, ok := call.Fun.(*ast.Ident); !ok || callIdent.Name != "append" {
			// XXX check that it's the built-in append
			return true
		}
		ssafn := f.EnclosingSSAFunction(assign)
		if ssafn == nil {
			return true
		}
		tfn, ok := ssafn.Object().(*types.Func)
		if ok {
			res := tfn.Type().(*types.Signature).Results()
			for i := 0; i < res.Len(); i++ {
				if res.At(i) == f.Pkg.TypesInfo.ObjectOf(ident) {
					// Don't flag appends assigned to named return arguments
					return true
				}
			}
		}
		isAppend := func(ins ssa.Value) bool {
			call, ok := ins.(*ssa.Call)
			if !ok {
				return false
			}
			if call.Call.IsInvoke() {
				return false
			}
			if builtin, ok := call.Call.Value.(*ssa.Builtin); !ok || builtin.Name() != "append" {
				return false
			}
			return true
		}
		isUsed := false
		visited := map[ssa.Instruction]bool{}
		var walkRefs func(refs []ssa.Instruction)
		walkRefs = func(refs []ssa.Instruction) {
		loop:
			for _, ref := range refs {
				if visited[ref] {
					continue
				}
				visited[ref] = true
				if _, ok := ref.(*ssa.DebugRef); ok {
					continue
				}
				switch ref := ref.(type) {
				case *ssa.Phi:
					walkRefs(*ref.Referrers())
				case ssa.Value:
					if !isAppend(ref) {
						isUsed = true
					} else {
						walkRefs(*ref.Referrers())
					}
				case ssa.Instruction:
					isUsed = true
					break loop
				}
			}
		}
		expr, _ := ssafn.ValueForExpr(call)
		if expr == nil {
			return true
		}
		refs := expr.Referrers()
		if refs == nil {
			return true
		}
		walkRefs(*refs)
		if !isUsed {
			f.Errorf(assign, "this result of append is never used, except maybe in other appends")
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckConcurrentTesting(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ssafn := f.EnclosingSSAFunction(fn)
		if ssafn == nil {
			return true
		}
		for _, block := range ssafn.Blocks {
			for _, ins := range block.Instrs {
				gostmt, ok := ins.(*ssa.Go)
				if !ok {
					continue
				}
				var fn *ssa.Function
				switch val := gostmt.Call.Value.(type) {
				case *ssa.Function:
					fn = val
				case *ssa.MakeClosure:
					fn = val.Fn.(*ssa.Function)
				default:
					continue
				}
				if fn.Blocks == nil {
					continue
				}
				for _, block := range fn.Blocks {
					for _, ins := range block.Instrs {
						call, ok := ins.(*ssa.Call)
						if !ok {
							continue
						}
						if call.Call.IsInvoke() {
							continue
						}
						callee := call.Call.StaticCallee()
						if callee == nil {
							continue
						}
						recv := callee.Signature.Recv()
						if recv == nil {
							continue
						}
						if types.TypeString(recv.Type(), nil) != "*testing.common" {
							continue
						}
						fn, ok := call.Call.StaticCallee().Object().(*types.Func)
						if !ok {
							continue
						}
						name := fn.Name()
						switch name {
						case "FailNow", "Fatal", "Fatalf", "SkipNow", "Skip", "Skipf":
						default:
							continue
						}
						f.Errorf(gostmt, "the goroutine calls T.%s, which must be called in the same goroutine as the test", name)
					}
				}
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckCyclicFinalizer(f *lint.File) {
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if !lint.IsPkgDot(call.Fun, "runtime", "SetFinalizer") {
			return true
		}
		if len(call.Args) != 2 {
			return true
		}
		ssafn := f.EnclosingSSAFunction(call)
		if ssafn == nil {
			return true
		}
		ident, ok := call.Args[0].(*ast.Ident)
		if !ok {
			return true
		}
		obj := f.Pkg.TypesInfo.ObjectOf(ident)
		checkFn := func(fn *ssa.Function) {
			if len(fn.FreeVars) == 0 {
				return
			}
			for _, v := range fn.FreeVars {
				path, _ := astutil.PathEnclosingInterval(f.File, v.Pos(), v.Pos())
				if len(path) == 0 {
					continue
				}
				ident, ok := path[0].(*ast.Ident)
				if !ok {
					continue
				}
				if f.Pkg.TypesInfo.ObjectOf(ident) == obj {
					pos := f.Fset.Position(fn.Pos())
					f.Errorf(call, "the finalizer closes over the object, preventing the finalizer from ever running (at %s)", pos)
					break
				}
			}
		}
		var checkValue func(val ssa.Value)
		seen := map[ssa.Value]bool{}
		checkValue = func(val ssa.Value) {
			if seen[val] {
				return
			}
			seen[val] = true
			switch val := val.(type) {
			case *ssa.Phi:
				for _, val := range val.Operands(nil) {
					checkValue(*val)
				}
			case *ssa.MakeClosure:
				checkFn(val.Fn.(*ssa.Function))
			default:
				return
			}
		}

		switch arg := call.Args[1].(type) {
		case *ast.Ident, *ast.FuncLit:
			r, _ := ssafn.ValueForExpr(arg)
			checkValue(r)
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckSliceOutOfBounds(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ssafn := f.Pkg.SSAPkg.Prog.FuncValue(f.Pkg.TypesInfo.ObjectOf(fn.Name).(*types.Func))
		if ssafn == nil {
			return true
		}
		for _, block := range ssafn.Blocks {
			for _, ins := range block.Instrs {
				ia, ok := ins.(*ssa.IndexAddr)
				if !ok {
					continue
				}
				ic, ok := ia.Index.(*ssa.Const)
				if !ok || ic.Value == nil {
					continue
				}
				idx, _ := constant.Int64Val(ic.Value)
				switch x := ia.X.(type) {
				case *ssa.Const:
					if x.Value == nil {
						f.Errorf(ia, "index out of bounds")
					}
				case *ssa.Slice:
					high := int64(-1)
					if x.High == nil {
						if alloc, ok := x.X.(*ssa.Alloc); ok {
							if array, ok := alloc.Type().(*types.Pointer).Elem().(*types.Array); ok {
								high = array.Len()
							}
						}
					}
					if high == -1 {
						c, ok := x.High.(*ssa.Const)
						if !ok {
							break
						}
						if c.Value == nil {
							break
						}
						high, _ = constant.Int64Val(c.Value)
					}
					if idx >= high {
						f.Errorf(ia, "index out of bounds")
					}
				}
			}
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckDeferLock(f *lint.File) {
	fn := func(node ast.Node) bool {
		block, ok := node.(*ast.BlockStmt)
		if !ok {
			return true
		}
		if len(block.List) < 2 {
			return true
		}
		for i, stmt := range block.List[:len(block.List)-1] {
			expr, ok := stmt.(*ast.ExprStmt)
			if !ok {
				continue
			}
			call, ok := expr.X.(*ast.CallExpr)
			if !ok {
				continue
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Lock" && sel.Sel.Name != "RLock") || len(call.Args) != 0 {
				continue
			}
			d, ok := block.List[i+1].(*ast.DeferStmt)
			if !ok || len(d.Call.Args) != 0 {
				continue
			}
			dsel, ok := d.Call.Fun.(*ast.SelectorExpr)
			if !ok || dsel.Sel.Name != sel.Sel.Name || f.Render(dsel.X) != f.Render(sel.X) {
				continue
			}
			unlock := "Unlock"
			if sel.Sel.Name[0] == 'R' {
				unlock = "RUnlock"
			}
			f.Errorf(d, "deferring %s right after having locked already; did you mean to defer %s?", sel.Sel.Name, unlock)
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckNaNComparison(f *lint.File) {
	isNaN := func(x ast.Expr) bool {
		call, ok := x.(*ast.CallExpr)
		if !ok {
			return false
		}
		return lint.IsPkgDot(call.Fun, "math", "NaN")
	}
	fn := func(node ast.Node) bool {
		op, ok := node.(*ast.BinaryExpr)
		if !ok {
			return true
		}
		if isNaN(op.X) || isNaN(op.Y) {
			f.Errorf(op, "no value is equal to NaN, not even NaN itself")
		}
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckInfiniteRecursion(f *lint.File) {
	fn := func(node ast.Node) bool {
		fn, ok := node.(*ast.FuncDecl)
		if !ok {
			return true
		}
		ssafn := f.EnclosingSSAFunction(fn)
		if ssafn == nil {
			return true
		}
		if len(ssafn.Blocks) == 0 {
			return true
		}
		for _, block := range ssafn.Blocks {
			for _, ins := range block.Instrs {
				call, ok := ins.(*ssa.Call)
				if !ok {
					continue
				}
				if call.Common().IsInvoke() {
					continue
				}
				subfn, ok := call.Common().Value.(*ssa.Function)
				if !ok || subfn != ssafn {
					continue
				}

				canReturn := false
				for _, b := range subfn.Blocks {
					if block.Dominates(b) {
						continue
					}
					if len(b.Instrs) == 0 {
						continue
					}
					if _, ok := b.Instrs[len(b.Instrs)-1].(*ssa.Return); ok {
						canReturn = true
						break
					}
				}
				if canReturn {
					continue
				}
				f.Errorf(call, "infinite recursive call")
			}
		}
		return true
	}
	f.Walk(fn)
}

func isFunctionCallName(f *lint.File, node ast.Node, name string) bool {
	call, ok := node.(*ast.CallExpr)
	if !ok {
		return false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	fn, ok := f.Pkg.TypesInfo.ObjectOf(sel.Sel).(*types.Func)
	return ok && fn.FullName() == name
}

func isFunctionCallNameAny(f *lint.File, node ast.Node, names []string) bool {
	for _, name := range names {
		if isFunctionCallName(f, node, name) {
			return true
		}
	}
	return false
}

func (c *Checker) CheckUnmarshalPointer(f *lint.File) {
	names := []string{
		"encoding/xml.Unmarshal",
		"(*encoding/xml.Decoder).Decode",
		"encoding/json.Unmarshal",
		"(*encoding/json.Decoder).Decode",
	}
	fn := func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		if len(call.Args) == 0 {
			return true
		}
		if !isFunctionCallNameAny(f, call, names) {
			return true
		}
		arg := call.Args[len(call.Args)-1]
		switch f.Pkg.TypesInfo.TypeOf(arg).Underlying().(type) {
		case *types.Pointer, *types.Interface:
			return true
		}
		f.Errorf(arg, "%s expects to unmarshal into a pointer, but the provided value is not a pointer", sel.Sel.Name)
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckLeakyTimeTick(f *lint.File) {
	if f.IsMain() || f.IsTest() {
		return
	}
	var flowTerminates func(start, b *ssa.BasicBlock, seen map[*ssa.BasicBlock]bool) bool
	flowTerminates = func(start, b *ssa.BasicBlock, seen map[*ssa.BasicBlock]bool) bool {
		if seen == nil {
			seen = map[*ssa.BasicBlock]bool{}
		}
		if seen[b] {
			return false
		}
		seen[b] = true
		for _, ins := range b.Instrs {
			if _, ok := ins.(*ssa.Return); ok {
				return true
			}
		}
		if b == start {
			if flowTerminates(start, b.Succs[0], seen) {
				return true
			}
		} else {
			for _, succ := range b.Succs {
				if flowTerminates(start, succ, seen) {
					return true
				}
			}
		}
		return false
	}
	fn := func(node ast.Node) bool {
		if !isFunctionCallName(f, node, "time.Tick") {
			return true
		}
		ssafn := f.EnclosingSSAFunction(node)
		if ssafn == nil {
			return false
		}
		if !c.terminates(ssafn) {
			return true
		}
		f.Errorf(node, "using time.Tick leaks the underlying ticker, consider using it only in endless functions, tests and the main package, and use time.NewTicker here")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckDoubleNegation(f *lint.File) {
	fn := func(node ast.Node) bool {
		unary1, ok := node.(*ast.UnaryExpr)
		if !ok {
			return true
		}
		unary2, ok := unary1.X.(*ast.UnaryExpr)
		if !ok {
			return true
		}
		if unary1.Op != token.NOT || unary2.Op != token.NOT {
			return true
		}
		f.Errorf(unary1, "negating a boolean twice has no effect; is this a typo?")
		return true
	}
	f.Walk(fn)
}

func (c *Checker) CheckRepeatedIfElse(f *lint.File) {
	seen := map[ast.Node]bool{}

	var collectConds func(ifstmt *ast.IfStmt, inits []ast.Stmt, conds []ast.Expr) ([]ast.Stmt, []ast.Expr)
	collectConds = func(ifstmt *ast.IfStmt, inits []ast.Stmt, conds []ast.Expr) ([]ast.Stmt, []ast.Expr) {
		seen[ifstmt] = true
		if ifstmt.Init != nil {
			inits = append(inits, ifstmt.Init)
		}
		conds = append(conds, ifstmt.Cond)
		if elsestmt, ok := ifstmt.Else.(*ast.IfStmt); ok {
			return collectConds(elsestmt, inits, conds)
		}
		return inits, conds
	}
	isDynamic := func(node ast.Node) bool {
		dynamic := false
		ast.Inspect(node, func(node ast.Node) bool {
			switch node := node.(type) {
			case *ast.CallExpr:
				dynamic = true
				return false
			case *ast.UnaryExpr:
				if node.Op == token.ARROW {
					dynamic = true
					return false
				}
			}
			return true
		})
		return dynamic
	}
	fn := func(node ast.Node) bool {
		ifstmt, ok := node.(*ast.IfStmt)
		if !ok {
			return true
		}
		if seen[ifstmt] {
			return true
		}
		inits, conds := collectConds(ifstmt, nil, nil)
		if len(inits) > 0 {
			return true
		}
		for _, cond := range conds {
			if isDynamic(cond) {
				return true
			}
		}
		counts := map[string]int{}
		for _, cond := range conds {
			s := f.Render(cond)
			counts[s]++
			if counts[s] == 2 {
				f.Errorf(cond, "this condition occurs multiple times in this if/else if chain")
			}
		}
		return true
	}
	f.Walk(fn)
}
