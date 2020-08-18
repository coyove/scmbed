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
	Empty         = &Pair{_empty: true}
	Types         = map[ValueType]string{STR: "string", SYM: "symbol", NUM: "number", LIST: "list", ANY: "any", FUNC: "function", VOID: "void", BOOL: "boolean"}
	TypesRev      = map[string]ValueType{"string": STR, "symbol": SYM, "number": NUM, "list": LIST, "any": ANY, "function": FUNC, "void": VOID, "boolean": BOOL}
)

const STR, SYM, NUM, BOOL, LIST, ANY, FUNC, VOID ValueType = 's', 'y', 'n', 'b', 'l', 'i', 'f', 'v'

type (
	ValueType byte
	Value     struct {
		val uint64
		ptr unsafe.Pointer
	}
	Pair struct {
		next   *Pair
		val    Value
		_empty bool
	}
	Func struct {
		macro         bool         // is macro
		wrapper       bool         // is native wrapper
		fvar          bool         // builtin is variadic
		f             func(*State) // builtin function
		fvars         int          // builtin: (minimal) arguments required
		n             Value        // native function
		nargs         []string     // native: arguments binding list
		nvar_or_fname string       // native: vararg name, builtin: last assigned name
		cls           *Context     // native: closure
		location      string
	}
	State struct {
		*Context
		assertable
		argsidx     int
		arglast     Value
		Args        *Pair
		Out, Caller Value
		Debug       debugInfo
	}
	Context struct {
		assertable
		parent *Context
		m      map[string]Value
	}
	debugInfo struct {
		K *Value
		L *string
	}
	execState struct {
		assertable
		debug     debugInfo
		local     *Context
		macroMode bool
	}
	assertable struct{ err error }
)

func New() *Context { return Default.Copy() }

func NewFunc(minArgsFlag int, f func(*State)) Value {
	return F(&Func{f: f, fvars: minArgsFlag & 0xffff, fvar: minArgsFlag&Vararg != 0, macro: minArgsFlag&Macro != 0})
}

func (f *Func) Call(a ...Value) (Value, error) {
	expr := InitListBuilder().Append(F(f).Quote())
	for i := range a {
		expr = expr.Append(a[i].Quote())
	}
	return ((*Context)(nil)).Exec(expr.Build())
}

func (f *Func) String() string {
	if f.n == Void {
		return ifstr(f.nvar_or_fname == "#v", "#|missing native code|#", f.nvar_or_fname+" #|native code|#")
	}
	return ifstr(f.macro, "(lambda-syntax (", "(lambda (") + strings.Join(f.nargs, " ") + ifstr(f.nvar_or_fname != "", " . "+f.nvar_or_fname, "") + ") " +
		f.n.String() + ")" + ifstr(f.wrapper, " #|wrapper|#", "")
}

// In pops an argument
func (s *State) In() Value {
	s.assert(!s.Args.ProEmpty() || s.panic("too few arguments, expect at least %d", s.argsidx+1))
	v := s.Args.Val()
	s.argsidx, s.arglast = s.argsidx+1, v
	s.Args = s.Args.Next()
	return v
}

func (s *State) InType(t ValueType) Value {
	v := s.In()
	s.assert(t == 0 || v.Type() == t || s.panic("invalid argument #%d, expect %s, got %v", s.argsidx, Types[t], v))
	return v
}

func (s *State) LastIn() Value { return s.arglast }

func (ctx *Context) find(k string) (Value, *Context) {
	for ; ctx != nil; ctx = ctx.parent {
		if v, ok := ctx.m[k]; ok {
			return v, ctx
		}
	}
	return Void, nil
}

func (ctx *Context) set(k string, v Value) {
	if ctx.m == nil {
		ctx.m = make(map[string]Value, 4)
	}
	ctx.m[k] = v
}

func (ctx *Context) Store(k string, v Value) *Context {
	if v.Type() == FUNC && v.F().n == Void {
		v.F().nvar_or_fname = k
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
		delete(mv.m, k)
	}
	return v, mv != nil
}

func (ctx *Context) Len() int { return len(ctx.m) }

func (ctx *Context) Unsafe() map[string]Value { return ctx.m }

func (ctx *Context) Copy() *Context {
	m2 := &Context{m: make(map[string]Value, len(ctx.m)), parent: ctx.parent}
	for k, v := range ctx.m {
		m2.m[k] = v
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
				*results.Current = *p
			} else {
				results = results.Append(v)
			}
		}
		return results.Build(), nil
	}
	return expr, nil
}

func __exec(expr Value, state execState) Value {
TAIL_CALL:
	switch expr.Type() {
	case SYM:
		v, ok := state.local.Load(expr.S())
		state.assert(ok || state.panic("unbound %v", expr))
		return v
	case LIST: // evaluating the list
	default:
		return expr
	}

	c := expr.L()
	if c.ProEmpty() {
		return L(Empty)
	}

	head := c.Val()
	if *state.debug.K = c.Val(); state.debug.K.Type() == SYM {
		switch va := state.debug.K.S(); va {
		case "if":
			state.assert(c.MoveProNext(&c) || state.panic("invalid if syntax, missing condition"))
			if !__exec(c.Val(), state).IsFalse() {
				state.assert(c.MoveProNext(&c) || state.panic("invalid if syntax, missing true branch"))
				expr = c.Val() // execute true-branch
				goto TAIL_CALL
			}
			c.MoveProNext(&c)                        // skip condition
			c.MoveProNext(&c)                        // skip true branch
			for ; !c.ProEmpty(); c.MoveProNext(&c) { // execute rest statements: (if cond true-branch false-1 ... false-n)
				if !c.HasProNext() {
					expr = c.Val()
					goto TAIL_CALL
				}
				__exec(c.Val(), state)
			}
			return Void
		case "lambda", "lambda-syntax":
			state.assert(c.MoveProNext(&c) || state.panic("invalid lambda* syntax, missing parameters"))
			f := &Func{cls: state.local, macro: va != "lambda", location: *state.debug.L}
			switch c.Val().Type() {
			case SYM: // (lambda* args body)
				f.nvar_or_fname = c.Val().S()
			case LIST: // (lambda* (a1 a2 ... an) body)
				for bindings := c.Val().L(); bindings != nil; bindings = bindings.Next() {
					if bindings.Improper() {
						state.assert(bindings.Val().Type() == SYM || state.panic("invalid parameter, expect valid symbol"))
						f.nvar_or_fname = bindings.Val().S()
					} else if !bindings.Empty() {
						state.assert(bindings.Val().Type() == SYM || state.panic("invalid parameter, expect valid symbol"))
						f.nargs = append(f.nargs, bindings.Val().S())
					}
				}
			default:
				panic(fmt.Errorf("invalid binding list: %v", c.Val()))
			}
			state.assert(c.MoveProNext(&c) || state.panic("invalid lambda syntax, missing lambda body"))
			if f.n = c.Val(); c.HasProNext() {
				f.n = L(c, state.debug.K.Y("if"), Void, Void)
			}
			return F(f)
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
			m := &Context{parent: state.local, m: map[string]Value{}}
			if source.L().match(state, pattern, false, symbolmap, m.m) {
				return __exec(c.Val(), execState{debug: state.debug, local: m})
			}
			if c.HasProNext() {
				goto MATCH_NEXT
			}
			return Void
		case "quote":
			state.assert(c.MoveProNext(&c) || state.panic("invalid quote syntax"))
			return c.Val()
		case "quasiquote":
			state.assert(c.MoveProNext(&c) || state.panic("invalid quasiquote syntax"))
			v, p := __execQuasi(c.Val(), state)
			if p != nil {
				return L(p)
			}
			return v
		case "unquote", "unquote-splicing":
			panic(fmt.Errorf("unquote outside quasiquote"))
		case "set!":
			state.assert(c.MoveProNext(&c) && c.Val().Type() == SYM || state.panic("invalid set! syntax, missing symbol"))
			x := c.Val().S()
			_, m := state.local.find(x)
			state.assert(m != nil || state.panic("set!: unbound %s", x))
			state.assert(c.MoveProNext(&c) || state.panic("invalid set! syntax, missing bound value"))
			m.set(x, __exec(c.Val(), state))
			return Void
		case "define":
			switch c.MoveProNext(&c); c.Val().Type() {
			case SYM:
				x := c.Val().S()
				state.assert(state.local.m[x] == Void || state.panic("re-define %s", x)).
					assert(c.MoveProNext(&c) || state.panic("invalid define syntax, missing bound value"))
				state.local.set(x, __exec(c.Val(), state))
			case LIST:
				lst := c.Val().L()
				state.assert(!lst.ProEmpty() || state.panic("invalid define syntax, missing function name")).
					assert(c.MoveProNext(&c) || state.panic("invalid define syntax, missing bound value"))
				s := L(Empty, head.Y("define"), lst.Val(), L(c, head.Y("lambda"), L(lst.Next())))
				return __exec(s, state)
			default:
				panic("invalid define syntax, missing binding symbol")
			}
			return Void
		}
	}

	fn := __exec(head, state)
	state.assert(fn.Type() == FUNC || state.panic("invalid function: %v", head))
	cc := fn.F()

	oldLoc := *state.debug.L
	if cc.f == nil {
		if cc.macro && !state.macroMode {
			return __exec(state.local.unwrapMacro(expr, false), state)
		}
		m := &Context{parent: cc.cls}
		for i, name := range cc.nargs {
			state.assert(c.MoveProNext(&c) || state.panic("too few arguments, expect at least %d", i+1))
			m.set(name, __exec(c.Val(), state))
		}
		if cc.nvar_or_fname != "" {
			values := InitListBuilder()
			c.Next().ProRange(func(v Value) bool { values = values.Append(__exec(v, state)); return true })
			m.set(cc.nvar_or_fname, values.Build())
		}
		if *state.debug.L == cc.location {
			*state.debug.K, state.local, expr = head, m, cc.n
			goto TAIL_CALL
		}
		*state.debug.L, state.local = cc.location, m
		v := __exec(cc.n, state)
		*state.debug.L = oldLoc
		return v
	}

	s := State{Context: state.local, Out: Void, Caller: head, Debug: state.debug}
	args := InitListBuilder()
	c.Next().ProRange(func(v Value) bool { args = args.Append(__exec(v, state)); return true })

	*state.debug.K = head
	state.assert(args.Len == cc.fvars || (cc.fvar && args.Len >= cc.fvars) ||
		state.panic("call: expect "+ifstr(cc.fvar, "at least ", "")+strconv.Itoa(cc.fvars)+" arguments"))

	s.Args = args.head
	cc.f(&s)
	*state.debug.L = oldLoc
	return s.Out
}

func (ctx *Context) Parse(filename, text string) (expr Value, err error) {
	s := (&scanner.Scanner{}).Init(strings.NewReader(text))
	s.Mode &^= scanner.ScanChars | scanner.ScanRawStrings
	s.Error = func(s *scanner.Scanner, msg string) {
		pos := s.Position
		if !pos.IsValid() {
			pos = s.Pos()
		}
		err = fmt.Errorf("parse: %s at %s:%d:%d", msg, filename, pos.Line, pos.Column)
	}
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("parse: %v at %s:%d:%d", r, filename, s.Pos().Line, s.Pos().Column)
		}
	}()
	v := ctx.scan(s, false)
	if err != nil {
		return Void, err
	}

	v = ctx.unwrapMacro(v, false)
	if v.Type() == LIST {
		v = L(v.L(), Y("if", 0), Void, Void)
	} else {
		v = L(Empty, Y("if", 0), Void, Void, v)
	}
	v = F(&Func{n: v, cls: ctx, wrapper: true, location: filename})
	return L(Empty, v), nil
}

func (ctx *Context) Exec(c Value) (output Value, err error) {
	var dbg = debugInfo{new(Value), new(string)}
	defer func() {
		if r := recover(); r != nil {
			if os.Getenv("SBD_STACK") != "" {
				fmt.Println(string(debug.Stack()))
			}
			err = fmt.Errorf("%v at %v (%s)", r, *dbg.K, *dbg.L)
		}
	}()
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
	// buf, err = c.L().V().NewFunc().n.Marshal()
	// panicerr(err)
	// {
	// 	c := Void
	// 	fmt.Println(c.Unmarshal(buf))
	// }
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
					comp = comp.Append(N(float64(map[string]rune{
						"space": ' ', "newline": '\n', "return": '\r', "tab": '\t', "backspace": '\b', "alert": '\a', "form": '\f', "backslash": '\\',
					}[strings.ToLower(n)])))
				default:
					r, l := utf8.DecodeRuneInString(n)
					ctx.assert(l != 0 || ctx.panic("invalid char: %q", n))
					comp = comp.Append(N(float64(r)))
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
			if c.Type() == LIST {
				*comp.Current = *c.L()
			} else {
				comp.Current.setVal(c)._empty = false
			}
			s := s.Scan()
			ctx.assert(s == scanner.EOF || s == ']' || s == ')' || ctx.panic("invalid dot syntax"))
			return comp.Build()
		case '\'', '`':
			c := ctx.scan(s, true)
			ctx.assert(c != Void || ctx.panic("invalid quote syntax"))
			comp = comp.Append(L(Empty, Y(ifstr(tok == '\'', "quote", "quasiquote"), 0), c))
		case ',':
			sp := false
			if s.Peek() == '@' {
				s.Scan()
				sp = true
			}
			c := ctx.scan(s, true)
			ctx.assert(c != Void || ctx.panic("invalid unquote syntax"))
			comp = comp.Append(L(Empty, Y(ifstr(sp, "unquote-splicing", "unquote"), 0), c))
		default:
			text := s.TokenText() + scanToDelim(s)
			if v, ok := strconv.ParseInt(text, 0, 64); ok == nil || tok == scanner.Int {
				comp = comp.Append(N(float64(v)).WithString(text))
			} else if v, ok := strconv.ParseFloat(text, 64); ok == nil || tok == scanner.Float {
				comp = comp.Append(N(v).WithString(text))
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

func (ctx *Context) unwrapMacro(v Value, quasi bool) Value {
	if v.Type() != LIST || v.L().Empty() {
		return v
	}

	comp := v.L()
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
			if m, _ := ctx.Load(head.S()); m.Type() == FUNC && m.F().macro {
				args := InitListBuilder().Append(head)
				for comp.MoveProNext(&comp); !comp.ProEmpty(); comp.MoveProNext(&comp) {
					args = args.Append(ctx.unwrapMacro(comp.Val(), false).Quote())
				}
				v := __exec(args.Build(), execState{local: ctx, debug: debugInfo{&Value{}, new(string)}, macroMode: true})
				return ctx.unwrapMacro(v, false)
			}
		}
	}
	old := comp
	for ; comp != nil; comp = comp.Next() {
		if comp.Empty() {
			break
		}
		comp.setVal(ctx.unwrapMacro(comp.Val(), quasi))
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

// V creates Value from interface{}. uint64 and int64 will be stored as they were because float64 can't handle them correctly
func V(v interface{}) Value {
	if v, ok := v.(Value); ok {
		return v
	}
	switch rv := reflect.ValueOf(v); rv.Kind() {
	case reflect.Interface:
		return V(rv.Elem())
	case reflect.Invalid:
		return Void
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32:
		return N(float64(rv.Int()))
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32:
		return N(float64(rv.Uint()))
	case reflect.Float32, reflect.Float64:
		return N(rv.Float())
	case reflect.String:
		return S(rv.String())
	case reflect.Bool:
		return B(rv.Bool())
	}
	va := Value{}
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
func N(v float64) Value {
	if math.IsNaN(v) {
		return Value{val: 0x800FFFFFFFFFFFFE} // ^7FF0000000000001
	}
	return Value{val: ^math.Float64bits(v)}
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
	default:
		if v.val >= 1<<51 {
			if v.val < 1<<52-1 {
				return SYM
			}
			return NUM
		}
		if v == Void {
			return VOID
		}
		return ANY
	}
}
func (v Value) stringify(goStyle bool) string {
	switch v.Type() {
	case NUM:
		return strconv.FormatFloat(v.N(), 'f', -1, 64)
	case STR:
		return strconv.Quote(v.S())
	case SYM:
		return v.S() + ifstr(v.LineInfo() > 0, fmt.Sprintf(ifstr(goStyle, " /*L%d*/", " #|L%d|#"), v.LineInfo()), "")
	case LIST:
		vl, p := v.L(), bytes.NewBufferString("(")
		for vl != nil && !vl._empty {
			if vl.Next() == nil {
				p.WriteString(". ")
				p.WriteString(vl.Val().stringify(goStyle))
			} else {
				p.WriteString(vl.Val().stringify(goStyle))
				p.WriteString(" ")
			}
			vl = vl.Next()
		}
		for p.Len() > 0 && p.Bytes()[p.Len()-1] == ' ' {
			p.Truncate(p.Len() - 1)
		}
		p.WriteString(")")
		return p.String()
	case BOOL:
		return ifstr(v.B(), ifstr(goStyle, "true", "#t"), ifstr(goStyle, "false", "#f"))
	case ANY:
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
		return v.N()
	case STR, SYM:
		return v.S()
	case LIST:
		var a []interface{}
		v.L().ProRange(func(v Value) bool { a = append(a, v.V()); return true })
		return a
	case BOOL:
		return v.B()
	case ANY:
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
			return v.val == v2.val
		case SYM, STR:
			return v.S() == v2.S()
		case ANY:
			return v.A() == v2.A()
		}
	}
	return false
}
func (v Value) F() *Func                  { return (*Func)(v.ptr) }
func (v Value) B() bool                   { return v.val == 2 }
func (v Value) N() float64                { return math.Float64frombits(^v.val) }
func (v Value) S() string                 { return *(*string)(v.ptr) }
func (v Value) L() *Pair                  { return (*Pair)(v.ptr) }
func (v Value) Y(text string) Value       { return Y(text, uint32(v.Type()/SYM)*v.LineInfo()) }
func (v Value) A() interface{}            { return *(*interface{})(unsafe.Pointer(&v)) }
func (v Value) LineInfo() uint32          { return uint32(v.val) }
func (v Value) String() string            { return v.stringify(false) }
func (v Value) GoString() string          { return fmt.Sprintf("{val:%016x ptr:%016x}", v.val, v.ptr) }
func (v Value) IsFalse() bool             { return v.val < 2 } // 0: void, 1: false
func (v Value) WithString(o string) Value { v.ptr = unsafe.Pointer(&o); return v }

func pval(v Value) *Pair               { return (&Pair{}).setVal(v) }
func (p *Pair) Val() Value             { return p.val }
func (p *Pair) SetCar(v Value) *Pair   { p.val = v; return p }
func (p *Pair) Next() *Pair            { return p.next }
func (p *Pair) setVal(v Value) *Pair   { p.val = v; return p }
func (p *Pair) setNext(p2 *Pair) *Pair { p.next = p2; return p }
func (p *Pair) Improper() bool         { return p.Next() == nil && !p._empty }
func (p *Pair) Empty() bool            { return p.Next() == nil && p._empty }
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
			if x := TypesRev[sym[2:]]; x > 0 {
				if p.Val().Type() != x {
					return false
				}
			} else if !p.Val().Equals(pattern.Val()) {
				return false
			}
			break
		}
		m[sym] = p.Val()
	case LIST:
		if lst := pattern.Val().L(); lst.Val().Type() == SYM && lst.Val().S() == "quote" {
			if __exec(lst.Next().Val(), execState{
				debug: state.debug,
				local: &Context{parent: state.local, m: map[string]Value{"_": p.Val()}},
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
	*b.Current = *l2
	return b.head
}
func (p *Pair) ProLast() (last *Pair) {
	for ; !p.ProEmpty(); p = p.Next() {
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
func (p *Pair) Car() Value {
	panicif(p.Empty(), "car: empty list")
	return p.Val()
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
func Cons(v, v2 Value) *Pair { return pval(v).SetCdr(v2) }

type ListBuilder struct {
	Len                 int
	head, last, Current *Pair
}

func InitListBuilder() ListBuilder {
	b := ListBuilder{Current: &Pair{_empty: true}}
	b.head = b.Current
	return b
}
func (b ListBuilder) Append(v Value) ListBuilder {
	panicif(!b.Current._empty, "append to improper list")
	b.Current.setVal(v).setNext(&Pair{_empty: true})._empty = false
	b.last = b.Current
	b.Current = b.Current.Next()
	b.Len++
	return b
}
func (b ListBuilder) Build() Value { return L(b.head) }
