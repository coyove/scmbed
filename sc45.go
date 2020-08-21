package sc45

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math"
	"os"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"text/scanner"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

var (
	Default       = &Context{}
	Vararg, Macro = 1 << 30, 1 << 29
	Void, Quote   = Value{}, Y("quote", 0)
	Empty         = &Pair{empty: true}
	Types         = map[ValueType]string{STR: "string", SYM: "symbol", NUM: "number", LIST: "list", INTF: "interface", FUNC: "function", VOID: "void", BOOL: "boolean"}
	int64Marker   = unsafe.Pointer(new(int64))
)

const STR, SYM, NUM, BOOL, LIST, INTF, FUNC, VOID, ANY ValueType = 's', 'y', 'n', 'b', 'l', 'i', 'f', 'v', 'a'
const toplevelName = "(toplevel)"

type (
	ValueType byte
	Value     struct {
		val uint64
		ptr unsafe.Pointer
	}
	Pair struct {
		next  *Pair
		val   Value
		empty bool
	}
	Func struct {
		source, name string       // filename inherited from the toplevel lambda, name => native: variadic argument name, builtin: last assigned name
		macro, vari  bool         // is macro/variadic function
		natToplevel  bool         // native: toplevel
		natArgNames  []string     // native: arguments binding list
		natCls       *Context     // native: closure
		nat          Value        // native
		funMinArgNum int          // builtin: minimal arguments required
		fun          func(*State) // builtin
	}
	State struct {
		*Context
		assertable
		argIdx              int
		Args                *Pair
		LastIn, Out, Caller Value
		Stack               *Stack
	}
	Context struct {
		assertable
		parent *Context
		M      map[string]Value
	}
	frame struct {
		K   Value
		Loc string
	}
	Stack struct {
		nextLoc string
		Frames  []frame
	}
	ListBuilder struct {
		Len             int
		head, last, Cur *Pair
	}
	execState struct {
		assertable
		debug     *Stack
		local     *Context
		macroMode bool
	}
	assertable struct{ err error }
)

func New() *Context { return Default.Copy() }

func NewFunc(minArgsFlag int, f func(*State)) Value {
	return F(&Func{fun: f, funMinArgNum: minArgsFlag & 0xffff, vari: minArgsFlag&Vararg != 0, macro: minArgsFlag&Macro != 0, source: "(builtin)"})
}

func (s *Stack) popAndReturn(v Value) Value {
	s.nextLoc = s.Frames[len(s.Frames)-1].Loc
	s.Frames = s.Frames[:len(s.Frames)-1]
	return v
}
func (s *Stack) push(k Value, tail bool) {
	if !tail {
		s.Frames = append(s.Frames, frame{K: k, Loc: s.nextLoc})
		return
	}
	s.Frames[len(s.Frames)-1].K = k
	s.Frames[len(s.Frames)-1].Loc = s.nextLoc
}
func (s *Stack) StackLocations(includeBuiltin bool) (locs []string) {
	for _, k := range s.Frames {
		if k.Loc != "(builtin)" || includeBuiltin {
			locs = append(locs, k.Loc)
		}
	}
	return locs
}
func (s *Stack) LastLocation() string { return s.Frames[len(s.Frames)-1].Loc }
func (s *Stack) String() (p string) {
	for _, k := range s.Frames {
		if sym, li := k.K.findSym(0); sym != "" && li > 0 {
			p = sym + " in " + k.Loc + ":" + strconv.FormatInt(int64(li), 10) + "\n" + p
		}
	}
	return strings.TrimSpace(p)
}

func (f *Func) Call(dbg *Stack, a ...Value) (result Value, err error) {
	expr := InitListBuilder().Append(F(f).Quote())
	for i := range a {
		expr = expr.Append(a[i].Quote())
	}
	if dbg == nil {
		dbg = &Stack{nextLoc: toplevelName}
	}
	defer debugCatch(dbg, &err)
	return __exec(expr.Build(), execState{local: (*Context)(nil), debug: dbg}), nil
}

func (f *Func) String() string {
	if f.nat == Void {
		return ifstr(f.name == "", "#|missing native code|#", f.name+" #|native code|#")
	} else if f.vari && len(f.natArgNames) == 0 {
		return ifstr(f.macro, "(lambda-syntax ", "(lambda ") + f.name + " " + f.nat.String() + ")"
	}
	return ifstr(f.macro, "(lambda-syntax (", "(lambda (") + strings.Join(f.natArgNames, " ") + ifstr(f.vari, " . "+f.name, "") + ") " + f.nat.String() + ")"
}

// PopArg pops an argument
func (s *State) PopArg() Value {
	s.assert(!s.Args.ProEmpty() || s.panic("too few arguments, expect at least %d", s.argIdx+1))
	v := s.Args.Val()
	s.argIdx, s.LastIn = s.argIdx+1, v
	s.Args = s.Args.Next()
	return v
}

func (s *State) PopArgAs(t ValueType) Value {
	v := s.PopArg()
	s.assert(t == ANY || v.Type() == t || s.panic("invalid argument #%d, expect %s, got %v", s.argIdx, Types[t], v))
	return v
}

func (ctx *Context) find(k string) (Value, *Context) {
	for ; ctx != nil; ctx = ctx.parent {
		if v, ok := ctx.M[k]; ok {
			return v, ctx
		}
	}
	return Void, nil
}
func (ctx *Context) set(k string, v Value) {
	if ctx.M == nil {
		ctx.M = make(map[string]Value, 4)
	}
	ctx.M[k] = v
}
func (ctx *Context) Store(k string, v Value) *Context {
	if v.Type() == FUNC && v.F().fun != nil {
		v.F().name = k
	}
	if _, mv := ctx.find(k); mv == nil {
		ctx.set(k, v)
	} else {
		mv.set(k, v)
	}
	return ctx
}
func (ctx *Context) Load(k string) (Value, bool) {
	v, mv := ctx.find(k)
	return v, mv != nil
}
func (ctx *Context) Delete(k string) (Value, bool) {
	v, mv := ctx.find(k)
	if mv != nil {
		delete(mv.M, k)
	}
	return v, mv != nil
}
func (ctx *Context) Copy() *Context {
	m2 := &Context{M: make(map[string]Value, len(ctx.M)), parent: ctx.parent}
	for k, v := range ctx.M {
		m2.M[k] = v
	}
	return m2
}

func __execQuasi(expr Value, state execState) (Value, *Pair) {
	if expr.Type() == LIST && !expr.L().ProEmpty() {
		lst := expr.L()
		if x := lst.Val(); x.Type() == SYM {
			switch x.S() {
			case "unquote":
				state.assert(lst.HasProNext() || state.panic("invalid unquote syntax"))
				return __exec(lst.Next().Val(), state), nil
			case "unquote-splicing":
				state.assert(lst.HasProNext() || state.panic("invalid unquote-splicing syntax"))
				v := __exec(lst.Next().Val(), state)
				if v.Type() == LIST {
					return Void, v.L()
				}
				return Void, pval(v)
			}
		}
		results := InitListBuilder()
		for ; !lst.ProEmpty(); lst = lst.Next() {
			if v, p := __execQuasi(lst.Val(), state); p != nil {
				for ; p != nil; p = p.Next() {
					*results.Cur = *p
					if !p.Empty() && !p.Improper() {
						results.Cur.setNext(&Pair{empty: true})
						results.Cur = results.Cur.Next()
					}
				}
			} else {
				results = results.Append(v)
			}
		}
		return results.Build(), nil
	}
	return expr, nil
}

func __exec(expr Value, state execState) Value {
	tailCall := false
TAIL_CALL:
	switch expr.Type() {
	case LIST: // evaluating the list
	case SYM:
		v, ok := state.local.Load(expr.S())
		state.assert(ok || state.panic("unbound %v", expr))
		expr = v
		fallthrough
	default:
		if tailCall {
			return state.debug.popAndReturn(expr)
		}
		return expr
	}

	c := expr.L()
	if c.ProEmpty() {
		return L(Empty)
	}

	head := c.Val()
	state.debug.push(head, tailCall)

	if head.Type() == SYM {
		switch va := head.S(); va {
		case "if", "lambda-body":
			state.assert(c.MoveProNext(&c) || state.panic("invalid if syntax, missing condition"))
			if !__exec(c.Val(), state).IsFalse() {
				state.assert(c.MoveProNext(&c) || state.panic("invalid if syntax, missing true branch"))
				expr, tailCall = c.Val(), true // execute true-branch
				goto TAIL_CALL
			}
			c.MoveProNext(&c)                        // skip condition
			c.MoveProNext(&c)                        // skip true branch
			for ; !c.ProEmpty(); c.MoveProNext(&c) { // execute rest statements: (if cond true-branch false-1 ... false-n)
				if !c.HasProNext() {
					expr, tailCall = c.Val(), true
					goto TAIL_CALL
				}
				__exec(c.Val(), state)
			}
			return state.debug.popAndReturn(Void)
		case "lambda", "lambda-syntax":
			state.assert(c.MoveProNext(&c) || state.panic("invalid lambda* syntax, missing parameters"))
			f := &Func{natCls: state.local, macro: va != "lambda", source: state.debug.LastLocation()}
			switch c.Val().Type() {
			case SYM: // (lambda* args body)
				f.name, f.vari = c.Val().S(), true
			case LIST: // (lambda* (a1 a2 ... an) body)
				for bindings := c.Val().L(); bindings != nil; bindings = bindings.Next() {
					if bindings.Improper() {
						state.assert(bindings.Val().Type() == SYM || state.panic("invalid parameter, expect valid symbol"))
						f.name, f.vari = bindings.Val().S(), true
					} else if !bindings.Empty() {
						state.assert(bindings.Val().Type() == SYM || state.panic("invalid parameter, expect valid symbol"))
						f.natArgNames = append(f.natArgNames, bindings.Val().S())
					}
				}
			default:
				panic(fmt.Errorf("invalid binding list: %v", c.Val()))
			}
			state.assert(c.MoveProNext(&c) || state.panic("invalid lambda syntax, missing lambda body"))
			if f.nat = c.Val(); c.HasProNext() {
				f.nat = L(c, head.Y("lambda-body"), Void, Void)
			}
			return state.debug.popAndReturn(F(f))
		case "match":
			state.assert(c.MoveProNext(&c) || state.panic("invalid match syntax, missing source"))
			source := __exec(c.Val(), state)
			state.assert(source.Type() == LIST || state.panic("invalid match syntax, expect source to be list"))
			state.assert(c.MoveProNext(&c) && c.Val().Type() == LIST || state.panic("invalid match syntax, missing symbol list"))
			symbols := c.Val().L().ProSlice()
		MATCH_NEXT:
			state.assert(c.MoveProNext(&c) && c.Val().Type() == LIST || state.panic("invalid match syntax, missing pattern"))
			pattern := c.Val().L()
			state.assert(c.MoveProNext(&c) || state.panic("invalid match syntax, missing body"))
			var symbolmap map[string]struct{}
			if len(symbols) > 0 {
				symbolmap = map[string]struct{}{}
				for _, s := range symbols {
					symbolmap[s.S()] = struct{}{}
				}
			}
			m := &Context{parent: state.local, M: map[string]Value{}}
			if source.L().match(state, pattern, false, symbolmap, m.M) {
				return state.debug.popAndReturn(__exec(c.Val(), execState{debug: state.debug, local: m}))
			}
			if c.HasProNext() {
				goto MATCH_NEXT
			}
			return state.debug.popAndReturn(Void)
		case "quote":
			state.assert(c.MoveProNext(&c) || state.panic("invalid quote syntax"))
			return state.debug.popAndReturn(c.Val())
		case "quasiquote":
			state.assert(c.MoveProNext(&c) || state.panic("invalid quasiquote syntax"))
			v, p := __execQuasi(c.Val(), state)
			if p != nil {
				return state.debug.popAndReturn(L(p))
			}
			return state.debug.popAndReturn(v)
		case "unquote", "unquote-splicing":
			panic(fmt.Errorf("unquote outside quasiquote"))
		case "set!":
			state.assert(c.MoveProNext(&c) && c.Val().Type() == SYM || state.panic("invalid set! syntax, missing symbol"))
			x := c.Val().S()
			_, m := state.local.find(x)
			state.assert(m != nil || state.panic("set!: unbound %s", x))
			state.assert(c.MoveProNext(&c) || state.panic("invalid set! syntax, missing bound value"))
			m.set(x, __exec(c.Val(), state))
			return state.debug.popAndReturn(Void)
		case "define":
			switch c.MoveProNext(&c); c.Val().Type() {
			case SYM:
				x := c.Val().S()
				state.assert(state.local.M[x] == Void || state.panic("re-define %s", x)).
					assert(c.MoveProNext(&c) || state.panic("invalid define syntax, missing bound value"))
				state.local.set(x, __exec(c.Val(), state))
			case LIST:
				lst := c.Val().L()
				state.assert(!lst.ProEmpty() || state.panic("invalid define syntax, missing function name")).
					assert(c.MoveProNext(&c) || state.panic("invalid define syntax, missing bound value"))
				s := L(Empty, head.Y("define"), lst.Val(), L(c, head.Y("lambda"), L(lst.Next())))
				__exec(s, state)
			default:
				panic("invalid define syntax, missing binding symbol")
			}
			return state.debug.popAndReturn(Void)
		}
	}

	fn := __exec(head, state)
	state.assert(fn.Type() == FUNC || state.panic("invalid function: %v", head))
	cc := fn.F()
	if cc.macro && !state.macroMode {
		reconstruct := L(pval(F(cc)).setNext(c.Next()))
		return state.debug.popAndReturn(__exec(state.local.unwrapMacro(reconstruct, false, state.debug), state))
	}

	if cc.fun == nil {
		m := &Context{parent: cc.natCls}
		for i, name := range cc.natArgNames {
			state.assert(c.MoveProNext(&c) || state.panic("too few arguments, expect at least %d", i+1))
			m.set(name, __exec(c.Val(), state))
		}
		if cc.vari {
			values := InitListBuilder()
			c.Next().ProRange(func(v Value) bool { values = values.Append(__exec(v, state)); return true })
			m.set(cc.name, values.Build())
		} else {
			state.assert(!c.HasProNext() || state.panic("too many arguments, expect exact %d", len(cc.natArgNames)))
		}
		state.local, expr, tailCall = m, cc.nat, true
		state.debug.nextLoc = (cc.source)
		goto TAIL_CALL
	}

	args := InitListBuilder()
	c.Next().ProRange(func(v Value) bool { args = args.Append(__exec(v, state)); return true })
	state.assert(args.Len == cc.funMinArgNum || (cc.vari && args.Len >= cc.funMinArgNum) ||
		state.panic("call: expect "+ifstr(cc.vari, "at least ", "")+strconv.Itoa(cc.funMinArgNum)+" arguments"))

	state.debug.nextLoc = (cc.source)
	s := State{Context: state.local, Out: Void, Caller: head, Stack: state.debug, Args: args.head}
	cc.fun(&s)
	return state.debug.popAndReturn(s.Out)
}

func (ctx *Context) Parse(filename, text string) (v Value, err error) {
	s := (&scanner.Scanner{}).Init(strings.NewReader(text))
	s.Mode &^= scanner.ScanChars | scanner.ScanRawStrings
	s.Error = func(s *scanner.Scanner, msg string) {
		err = fmt.Errorf("parse: %s at %s:%d:%d", msg, filename, s.Pos().Line, s.Pos().Column)
	}

	if v = func() Value {
		defer func() {
			if r := recover(); r != nil {
				err = fmt.Errorf("parse: %v at %s:%d:%d", r, filename, s.Pos().Line, s.Pos().Column)
			}
		}()
		return ctx.scan(s, false)
	}(); err != nil {
		return Void, err
	}

	dbg := &Stack{nextLoc: toplevelName}
	defer debugCatch(dbg, &err)
	v = ctx.unwrapMacro(v, false, dbg)
	if v.Type() == LIST {
		v = L(v.L(), Y("if", 0), Void, Void)
	} else {
		v = L(Empty, Y("if", 0), Void, Void, v)
	}
	v = F(&Func{nat: v, natCls: ctx, natToplevel: true, source: filename})
	return L(Empty, v), nil
}

func debugCatch(dbg *Stack, err *error) {
	if r := recover(); r != nil {
		if os.Getenv("SC_STACK") != "" {
			fmt.Println(string(debug.Stack()))
		}
		*err = fmt.Errorf("%v at %v", r, dbg)
	}
}

func (ctx *Context) Exec(c Value) (output Value, err error) {
	dbg := &Stack{nextLoc: toplevelName}
	defer debugCatch(dbg, &err)
	return __exec(c, execState{local: ctx, debug: dbg}), nil
}

func (ctx *Context) RunFile(path string) (result Value, err error) {
	buf, err := ioutil.ReadFile(path)
	if err != nil {
		return Void, err
	}
	c, err := ctx.Parse(path, *(*string)(unsafe.Pointer(&buf)))
	if err != nil {
		return Void, err
	}
	return ctx.Exec(c)
}

func (ctx *Context) Run(tmpl string) (result Value, err error) {
	c, err := ctx.Parse("(memory)", tmpl)
	if err != nil {
		return Void, err
	}
	return ctx.Exec(c)
}

func (ctx *Context) scan(s *scanner.Scanner, scanOne bool) Value {
	comp := InitListBuilder()
LOOP:
	for tok := s.Scan(); tok != scanner.EOF; tok = s.Scan() {
		// fmt.Println(s.TokenText())
		switch tok {
		case scanner.String, scanner.RawString:
			t, err := strconv.Unquote(s.TokenText())
			ctx.assert(err == nil || ctx.panic("invalid string: %q", s.TokenText()))
			comp = comp.Append(S(t))
		case '#':
			switch pr := s.Peek(); pr {
			case '|': // #| comment |#
				for s.Scan(); ; {
					current, next := s.Scan(), s.Peek()
					ctx.assert(current != scanner.EOF || ctx.panic("incomplete comment block"))
					if current == '|' && next == '#' {
						s.Scan()
						break
					}
				}
			case '/': // #/char
				s.Scan()
				switch n := scanToDelim(s); strings.ToLower(n) {
				case "space", "newline", "return", "tab", "backspace", "alert", "form", "backslash":
					comp = comp.Append(I(int64(map[string]rune{
						"space": ' ', "newline": '\n', "return": '\r', "tab": '\t', "backspace": '\b', "alert": '\a', "form": '\f', "backslash": '\\',
					}[strings.ToLower(n)])))
				default:
					r, l := utf8.DecodeRuneInString(n)
					ctx.assert(l != 0 || ctx.panic("invalid char: %q", n))
					comp = comp.Append(I(int64(r)))
				}
			case 't', 'f':
				s.Scan()
				comp = comp.Append(B(pr == 't'))
			case 'v':
				s.Scan()
				comp = comp.Append(Void)
			default:
				comp = comp.Append(Y("#"+scanToDelim(s), uint32(s.Pos().Line)))
			}
		case ';':
			for ; ; s.Scan() {
				if next := s.Peek(); next == scanner.EOF || next == '\n' {
					break
				}
			}
		case '(', '[':
			comp = comp.Append(ctx.scan(s, false))
		case ')', ']':
			break LOOP
		case '.':
			c := ctx.scan(s, true)
			ctx.assert(comp.last != nil || ctx.panic("invalid dot syntax")) // invalid e.g.: ( . a )
			comp.last.SetCdr(c)
			s := s.Scan()
			ctx.assert(s == scanner.EOF || s == ']' || s == ')' || ctx.panic("invalid dot syntax")) // invalid e.g.: ( a . b c )
			return comp.Build()
		case '\'', '`':
			c := ctx.scan(s, true)
			ctx.assert(c != Void || ctx.panic("invalid quote syntax"))
			comp = comp.Append(L(Empty, Y(ifstr(tok == '\'', "quote", "quasiquote"), 0), c))
		case ',':
			sp := s.Peek() == '@'
			if sp {
				s.Scan()
			}
			c := ctx.scan(s, true)
			ctx.assert(c != Void || ctx.panic("invalid unquote syntax"))
			comp = comp.Append(L(Empty, Y(ifstr(sp, "unquote-splicing", "unquote"), 0), c))
		default:
			text := s.TokenText() + scanToDelim(s)
			if v, ok := strconv.ParseInt(text, 0, 64); ok == nil || tok == scanner.Int {
				comp = comp.Append(I(v))
			} else if v, ok := strconv.ParseFloat(text, 64); ok == nil || tok == scanner.Float {
				comp = comp.Append(N(v))
			} else {
				comp = comp.Append(Y(text, uint32(s.Pos().Line)))
			}
		}
		if scanOne && comp.Len >= 1 {
			return comp.head.Val()
		}
	}
	return comp.Build()
}

func (ctx *Context) unwrapMacro(v Value, quasi bool, stack *Stack) Value {
	if v.Type() != LIST || v.L().Empty() {
		return v
	}

	comp, f := v.L(), (*Func)(nil)
	if head := comp.Val(); head.Type() == SYM {
		switch head.S() {
		case "quote":
			return v
		case "quasiquote":
			quasi = true
		case "unquote", "unquote-splicing":
			quasi = false
		default:
			if quasi {
				return v
			}
			if m, _ := ctx.Load(head.S()); m.Type() == FUNC {
				f = m.F()
			}
		}
	} else if head.Type() == FUNC {
		f = head.F()
	}
	if f != nil && f.macro {
		args := InitListBuilder().Append(F(f))
		for comp.MoveProNext(&comp); !comp.ProEmpty(); comp.MoveProNext(&comp) {
			args = args.Append(ctx.unwrapMacro(comp.Val(), false, stack).Quote())
		}
		v := __exec(args.Build(), execState{local: ctx, debug: stack, macroMode: true})
		return ctx.unwrapMacro(v, false, stack)
	}
	old := comp
	for ; comp != nil && !comp.Empty(); comp = comp.Next() {
		comp.setVal(ctx.unwrapMacro(comp.Val(), quasi, stack))
	}
	return L(old)
}

func scanToDelim(s *scanner.Scanner) string {
	for p := (bytes.Buffer{}); ; {
		next := s.Peek()
		if unicode.IsSpace(next) || next < 0 /* scanner.XXXX */ || strings.IndexRune("[]();", next) > -1 {
			return p.String()
		}
		s.Scan()
		p.WriteString(s.TokenText())
	}
}

func (e *assertable) assert(ok bool) *assertable {
	if !ok {
		panic(e.err)
	}
	return e
}

func (e *assertable) panic(t string, a ...interface{}) bool {
	e.err = fmt.Errorf(t, a...)
	return false
}

func ifstr(v bool, t, f string) string {
	if v {
		return t
	}
	return f
}
func panicif(v bool, t string) {
	if v {
		panic(t)
	}
}

// V creates Value from interface{}
func V(v interface{}) (va Value) {
	if v, ok := v.(Value); ok {
		return v
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Interface:
		return V(rv.Elem())
	case reflect.Invalid:
		return Void
	case reflect.Int64, reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return I(rv.Int())
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return I(int64(rv.Uint()))
	case reflect.Float32, reflect.Float64:
		return N(rv.Float())
	case reflect.String:
		return S(rv.String())
	case reflect.Bool:
		return B(rv.Bool())
	}
	*(*interface{})(unsafe.Pointer(&va)) = v
	return va
}
func B(v bool) Value {
	if v {
		return Value{val: 2}
	}
	return Value{val: 1}
}
func L(lst *Pair, l ...Value) Value {
	for i := len(l) - 1; i >= 0; i-- {
		n := pval(l[i]).setNext(lst)
		lst = n
	}
	return Value{val: uint64(LIST), ptr: unsafe.Pointer(lst)}
}

// Float64 available range in uint64: 0x0 - 0x7FF00000`00000001, 0x80000000`00000000 - 0xFFF00000`00000000
// When bit-inverted, the range is: 0x800FFFFF`FFFFFFFE - 0xFFFFFFFF`FFFFFFFF, 0x000FFFFF`FFFFFFFF - 0x7FFFFFFF`FFFFFFFF
func N(v float64) Value {
	if math.IsNaN(v) {
		return Value{val: 0x800FFFFFFFFFFFFE} // ^7FF0000000000001
	}
	return Value{val: ^math.Float64bits(v)}
}
func I(v int64) Value {
	if int64(float64(v)) == v {
		return N(float64(v))
	}
	return Value{val: uint64(v), ptr: int64Marker}
}
func Y(v string, ln uint32) Value { return Value{ptr: unsafe.Pointer(&v), val: 1<<51 | uint64(ln)} }
func S(v string) (vs Value)       { return Value{val: uint64(STR), ptr: unsafe.Pointer(&v)} }
func F(f *Func) Value             { return Value{val: uint64(FUNC), ptr: unsafe.Pointer(f)} }

func (v Value) Quote() Value {
	if t := v.Type(); t == LIST || t == SYM {
		return L(pval(Quote).setNext(pval(v).setNext(Empty)))
	}
	return v
}
func (v Value) Type() ValueType {
	switch v.val {
	case 1, 2:
		return BOOL
	case uint64(LIST), uint64(FUNC), uint64(STR):
		return ValueType(v.val)
	}
	if v.val >= 1<<51 {
		if v.val < 1<<52-1 {
			return SYM
		}
		return NUM
	} else if v == Void {
		return VOID
	}
	return INTF
}
func (v Value) stringify(goStyle bool) string {
	switch v.Type() {
	case NUM:
		vf, vi, vIsInt := v.N()
		if vIsInt {
			return strconv.FormatInt(vi, 10)
		}
		return strconv.FormatFloat(vf, 'f', -1, 64)
	case STR:
		return strconv.Quote(v.S())
	case SYM:
		return v.S() + ifstr(v.LineInfo() > 0, fmt.Sprintf(ifstr(goStyle, " /*L%d*/", " #|L%d|#"), v.LineInfo()), "")
	case LIST:
		vl, p := v.L(), bytes.NewBufferString("( ")
		for ; vl != nil && !vl.empty; vl = vl.Next() {
			p.WriteString(ifstr(vl.Next() == nil, ". ", ""))
			p.WriteString(vl.Val().stringify(goStyle))
			p.WriteString(" ")
		}
		p.WriteString(")")
		return p.String()
	case BOOL:
		return ifstr(v.B(), ifstr(goStyle, "true", "#t"), ifstr(goStyle, "false", "#f"))
	case INTF:
		return "#" + fmt.Sprint(v.A())
	case FUNC:
		return v.F().String()
	default:
		return ifstr(goStyle, "nil", "#v")
	}
}
func (v Value) V() interface{} {
	switch v.Type() {
	case NUM:
		vf, vi, vIsInt := v.N()
		if vIsInt {
			return vi
		}
		return vf
	case STR, SYM:
		return v.S()
	case LIST:
		var a []interface{}
		v.L().ProRange(func(v Value) bool { a = append(a, v.V()); return true })
		return a
	case BOOL:
		return v.B()
	case INTF:
		return v.A()
	case FUNC:
		return v.F()
	default:
		return nil
	}
}
func (v Value) Equals(v2 Value) bool {
	if v == v2 {
		return true
	} else if vflag, v2flag := v.Type(), v2.Type(); vflag == v2flag {
		switch vflag {
		case NUM:
			return v.val == v2.val && v.ptr == v2.ptr
		case SYM, STR:
			return v.S() == v2.S()
		case INTF:
			return v.A() == v2.A()
		}
	}
	return false
}
func (v Value) findSym(depth int) (string, uint32) {
	if t := v.Type(); t == SYM {
		return strings.Repeat("(", depth) + v.S() + strings.Repeat(" ...)", depth), v.LineInfo()
	} else if t == LIST {
		return v.L().Val().findSym(depth + 1)
	}
	return "", 0
}
func (v Value) N() (floatVal float64, intVal int64, isInt bool) {
	if v.ptr == int64Marker {
		return float64(int64(v.val)), int64(v.val), true
	}
	f := math.Float64frombits(^v.val)
	return f, int64(f), float64(int64(f)) == f
}
func (v Value) I() int64 {
	if v.ptr == int64Marker {
		return int64(v.val)
	}
	return int64(math.Float64frombits(^v.val))
}
func (v Value) F() *Func            { return (*Func)(v.ptr) }
func (v Value) B() bool             { return v.val == 2 }
func (v Value) S() string           { return *(*string)(v.ptr) }
func (v Value) L() *Pair            { return (*Pair)(v.ptr) }
func (v Value) A() interface{}      { return *(*interface{})(unsafe.Pointer(&v)) }
func (v Value) Y(text string) Value { return Y(text, uint32(v.Type()/SYM)*v.LineInfo()) }
func (v Value) LineInfo() uint32    { return uint32(v.val) }
func (v Value) String() string      { return v.stringify(false) }
func (v Value) GoString() string    { return fmt.Sprintf("{val:%016x ptr:%016x}", v.val, v.ptr) }
func (v Value) IsFalse() bool       { return v.val < 2 } // 0: void, 1: false

func pval(v Value) *Pair               { return (&Pair{}).setVal(v) }
func (p *Pair) Val() Value             { return p.val }
func (p *Pair) Car() Value             { panicif(p.Empty(), "car: empty list"); return p.Val() }
func (p *Pair) SetCar(v Value) *Pair   { p.val = v; return p }
func (p *Pair) Next() *Pair            { return p.next }
func (p *Pair) setVal(v Value) *Pair   { p.val = v; return p }
func (p *Pair) setNext(p2 *Pair) *Pair { p.next = p2; return p }
func (p *Pair) Improper() bool         { return p.Next() == nil && !p.empty }
func (p *Pair) Empty() bool            { return p.Next() == nil && p.empty }
func (p *Pair) ProEmpty() bool         { panicif(p.Improper(), "improper list"); return p.Empty() }
func (p *Pair) HasProNext() bool       { return p.Next() != nil && !p.Next().ProEmpty() }
func (p *Pair) MoveProNext(ll **Pair) bool {
	if p.HasProNext() {
		*ll = p.Next()
		return true
	}
	*ll = Empty
	return false
}
func (p *Pair) ProLen() (length int) {
	for ; !p.ProEmpty(); p = p.Next() {
		length++
	}
	return
}
func (p *Pair) ProRange(cb func(Value) bool) {
	for flag := true; flag && !p.ProEmpty(); p = p.Next() {
		flag = cb(p.Val())
	}
}
func (p *Pair) ProSlice() (s []Value) {
	p.ProRange(func(v Value) bool { s = append(s, v); return true })
	return
}
func (p *Pair) match(state execState, pattern *Pair, metWildcard bool, symbols map[string]struct{}, m map[string]Value) bool {
	if pattern.ProEmpty() && p.ProEmpty() {
		return true
	}
	if pattern.ProEmpty() {
		return false
	}
	isWildcard := func(s string) string {
		if strings.HasSuffix(s, "*") {
			panicif(metWildcard, "multiple wildcards in one pattern")
			return ifstr(len(s) == 1, "*", s[:len(s)-1])
		}
		return ""
	}
	if p.ProEmpty() {
		if pattern.Val().Type() == SYM && !pattern.HasProNext() {
			if w := isWildcard(pattern.Val().S()); w != "" {
				m[w] = L(Empty)
				return true
			}
		}
		return false
	}
	switch pattern.Val().Type() {
	case SYM:
		sym := pattern.Val().S()
		if sym == "_" {
			break
		}
		if _, ok := symbols[sym]; ok {
			if !pattern.Val().Equals(p.Val()) {
				return false
			}
			break
		}
		if w := isWildcard(sym); w != "" {
			if pattern.HasProNext() {
				n := p.ProLen() - pattern.Next().ProLen()
				if n < 0 { // the remaining symbols in 'p' is less than 'pattern'
					return false
				}
				// The first n symbols will go into 'ww'
				ww := InitListBuilder()
				for ; n > 0; n-- {
					ww = ww.Append(p.Val())
					p = p.Next()
				}
				m[w] = ww.Build()
				return p.match(state, pattern.Next(), true, symbols, m)
			}
			m[w] = L(p)
			return true
		}
		if strings.HasPrefix(sym, "#:") {
			if !p.Val().Equals(pattern.Val()) {
				return false
			}
			break
		}
		m[sym] = p.Val()
	case LIST:
		if lst := pattern.Val().L(); lst.Val().Type() == SYM && lst.Val().S() == "quote" {
			if __exec(lst.Next().Val(), execState{
				debug: state.debug,
				local: &Context{parent: state.local, M: map[string]Value{"_": p.Val()}},
			}).IsFalse() {
				return false
			}
			m["_"] = p.Val()
			break
		}
		if p.Val().Type() != LIST {
			return false
		}
		if !p.Val().L().match(state, pattern.Val().L(), false, symbols, m) {
			return false
		}
	default:
		if !p.Val().Equals(pattern.Val()) {
			return false
		}
	}
	return p.Next().match(state, pattern.Next(), false, symbols, m)
}
func (p *Pair) ProTake(n int) *Pair {
	if n == 0 {
		return Empty
	}
	b := InitListBuilder()
	for ; !p.ProEmpty(); p = p.Next() {
		b = b.Append(p.Val())
		if b.Len == n {
			break
		}
	}
	if n == -1 {
		panicif(b.last == nil, "take(-1): empty list")
		*b.last = *Empty
	} else if b.Len != n {
		panic(fmt.Errorf("take: not enough values to take, need %d out of %d", n, b.Len))
	}
	return b.head
}
func (p *Pair) ProAppend(l2 *Pair) *Pair {
	b := InitListBuilder()
	p.ProRange(func(v Value) bool { b = b.Append(v); return true })
	*b.Cur = *l2
	return b.head
}
func (p *Pair) Last() (last *Pair) {
	for ; p != nil && !p.Empty(); p = p.Next() {
		last = p
	}
	return
}
func (p *Pair) IsProperList() bool {
	for ; ; p = p.Next() {
		if p.Improper() {
			return false
		} else if p.Empty() {
			return true
		}
	}
}
func (p *Pair) Cdr() Value {
	p = p.Next()
	panicif(p == nil, "cdr: empty list")
	if p.Improper() {
		return p.Val()
	}
	return L(p)
}
func (p *Pair) SetCdr(v Value) *Pair {
	panicif(p.Empty(), "cdr: empty list")
	if v.Type() == LIST {
		p.setNext(v.L())
	} else {
		p.setNext(pval(v))
	}
	return p
}
func Cons(v, v2 Value) *Pair           { return pval(v).SetCdr(v2) }
func InitListBuilder() (b ListBuilder) { b.Cur = &Pair{empty: true}; b.head = b.Cur; return b }
func (b ListBuilder) Build() Value     { return L(b.head) }
func (b ListBuilder) Append(v Value) ListBuilder {
	panicif(!b.Cur.empty, "append to improper list")
	b.Cur.setVal(v).setNext(&Pair{empty: true}).empty = false
	b.last = b.Cur
	b.Cur = b.Cur.Next()
	b.Len++
	return b
}
