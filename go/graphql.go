// GraphQL v4 at /api/graphql: a lexer, parser, validator and executor with introspection, over a
// GitHub-flavoured schema serving the documents gh and github-mcp-server send.

package main

import (
	"encoding/base64"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ================================================================ lexer and parser

type gLoc struct {
	Line   int `json:"line"`
	Column int `json:"column"`
}

type gSyntaxError struct {
	msg string
	loc gLoc
}

func (e *gSyntaxError) Error() string { return e.msg }

type gToken struct {
	kind  string // punct, name, int, float, string, eof
	value string
	loc   gLoc
}

func isNameStart(c byte) bool { return c == '_' || (c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') }
func isNameChar(c byte) bool  { return isNameStart(c) || (c >= '0' && c <= '9') }
func isDigit(c byte) bool     { return c >= '0' && c <= '9' }

func gLex(src string) ([]gToken, error) {
	var toks []gToken
	pos, line, lineStart := 0, 1, 0
	loc := func(at int) gLoc { return gLoc{line, at - lineStart + 1} }
	for pos < len(src) {
		c := src[pos]
		switch {
		case c == '\n':
			pos++
			line++
			lineStart = pos
		case c == ' ' || c == '\t' || c == '\r' || c == ',':
			pos++
		case c == '#':
			for pos < len(src) && src[pos] != '\n' {
				pos++
			}
		case strings.HasPrefix(src[pos:], "\xef\xbb\xbf"):
			pos += 3
		case strings.HasPrefix(src[pos:], "..."):
			toks = append(toks, gToken{"punct", "...", loc(pos)})
			pos += 3
		case strings.IndexByte("!$&()/:=@[]{}|", c) >= 0:
			toks = append(toks, gToken{"punct", string(c), loc(pos)})
			pos++
		case isNameStart(c):
			start := pos
			for pos < len(src) && isNameChar(src[pos]) {
				pos++
			}
			toks = append(toks, gToken{"name", src[start:pos], loc(start)})
		case c == '-' || isDigit(c):
			start := pos
			if c == '-' {
				pos++
			}
			for pos < len(src) && isDigit(src[pos]) {
				pos++
			}
			kind := "int"
			if pos < len(src) && src[pos] == '.' {
				kind = "float"
				pos++
				for pos < len(src) && isDigit(src[pos]) {
					pos++
				}
			}
			if pos < len(src) && (src[pos] == 'e' || src[pos] == 'E') {
				kind = "float"
				pos++
				if pos < len(src) && (src[pos] == '+' || src[pos] == '-') {
					pos++
				}
				for pos < len(src) && isDigit(src[pos]) {
					pos++
				}
			}
			toks = append(toks, gToken{kind, src[start:pos], loc(start)})
		case c == '"':
			start := pos
			if strings.HasPrefix(src[pos:], `"""`) {
				pos += 3
				var b strings.Builder
				for pos < len(src) && !strings.HasPrefix(src[pos:], `"""`) {
					if strings.HasPrefix(src[pos:], `\"""`) {
						b.WriteString(`"""`)
						pos += 4
						continue
					}
					if src[pos] == '\n' {
						line++
						lineStart = pos + 1
					}
					b.WriteByte(src[pos])
					pos++
				}
				if pos >= len(src) {
					return nil, &gSyntaxError{"Unterminated string", loc(start)}
				}
				pos += 3
				toks = append(toks, gToken{"string", blockString(b.String()), loc(start)})
				continue
			}
			pos++
			var b strings.Builder
			for pos < len(src) && src[pos] != '"' {
				if src[pos] == '\n' {
					return nil, &gSyntaxError{"Unterminated string", loc(start)}
				}
				if src[pos] == '\\' && pos+1 < len(src) {
					n := src[pos+1]
					switch n {
					case '"', '\\', '/':
						b.WriteByte(n)
					case 'b':
						b.WriteByte('\b')
					case 'f':
						b.WriteByte('\f')
					case 'n':
						b.WriteByte('\n')
					case 'r':
						b.WriteByte('\r')
					case 't':
						b.WriteByte('\t')
					case 'u':
						if pos+6 > len(src) {
							return nil, &gSyntaxError{"Invalid escape", loc(pos)}
						}
						v, err := strconv.ParseUint(src[pos+2:pos+6], 16, 32)
						if err != nil {
							return nil, &gSyntaxError{"Invalid escape", loc(pos)}
						}
						b.WriteRune(rune(v))
						pos += 6
						continue
					default:
						return nil, &gSyntaxError{"Invalid escape \\" + string(n), loc(pos)}
					}
					pos += 2
					continue
				}
				b.WriteByte(src[pos])
				pos++
			}
			if pos >= len(src) {
				return nil, &gSyntaxError{"Unterminated string", loc(start)}
			}
			pos++
			toks = append(toks, gToken{"string", b.String(), loc(start)})
		default:
			return nil, &gSyntaxError{fmt.Sprintf("Unexpected character %q", c), loc(pos)}
		}
	}
	toks = append(toks, gToken{"eof", "", loc(pos)})
	return toks, nil
}

func blockString(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	common := -1
	for _, l := range lines[1:] {
		ind := len(l) - len(strings.TrimLeft(l, " \t"))
		if ind < len(l) && (common < 0 || ind < common) {
			common = ind
		}
	}
	if common > 0 {
		for i := 1; i < len(lines); i++ {
			if len(lines[i]) >= common {
				lines[i] = lines[i][common:]
			}
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[0]) == "" {
		lines = lines[1:]
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}

type gValue struct {
	kind   string // Variable, Int, Float, String, Enum, Boolean, Null, List, Object
	value  string
	bval   bool
	list   []*gValue
	fields []gArg
}

type gArg struct {
	name  string
	value *gValue
}

type gDirective struct {
	name string
	args []gArg
}

type gSelection struct {
	kind          string // Field, FragmentSpread, InlineFragment
	alias, name   string
	args          []gArg
	directives    []gDirective
	selections    []*gSelection
	hasSelections bool
	typeCondition string
	loc           gLoc
}

type gTypeRef struct {
	kind string // Named, List, NonNull
	name string
	of   *gTypeRef
}

type gVarDef struct {
	name         string
	typ          *gTypeRef
	defaultValue *gValue
}

type gOperation struct {
	operation  string
	name       string
	vars       []gVarDef
	selections []*gSelection
	loc        gLoc
}

type gFragment struct {
	name, typeCondition string
	selections          []*gSelection
}

type gDocument struct {
	operations []*gOperation
	fragments  map[string]*gFragment
}

type gParser struct {
	toks []gToken
	pos  int
}

func (p *gParser) peek() gToken { return p.toks[p.pos] }
func (p *gParser) next() gToken {
	t := p.toks[p.pos]
	if p.pos < len(p.toks)-1 {
		p.pos++
	}
	return t
}
func (p *gParser) is(v string) bool { t := p.peek(); return t.kind == "punct" && t.value == v }

func describeTok(t gToken) string {
	if t.kind == "eof" {
		return "<EOF>"
	}
	return strconv.Quote(t.value)
}

func (p *gParser) expect(v string) error {
	t := p.next()
	if t.kind != "punct" || t.value != v {
		return &gSyntaxError{fmt.Sprintf("Expected %q, found %s", v, describeTok(t)), t.loc}
	}
	return nil
}

func (p *gParser) name() (string, error) {
	t := p.next()
	if t.kind != "name" {
		return "", &gSyntaxError{"Expected a name, found " + describeTok(t), t.loc}
	}
	return t.value, nil
}

func (p *gParser) document() (*gDocument, error) {
	doc := &gDocument{fragments: map[string]*gFragment{}}
	for p.peek().kind != "eof" {
		t := p.peek()
		if t.kind == "punct" && t.value == "{" {
			sels, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.operations = append(doc.operations, &gOperation{operation: "query", selections: sels, loc: t.loc})
			continue
		}
		if t.kind != "name" {
			return nil, &gSyntaxError{"Unexpected " + describeTok(t), t.loc}
		}
		switch t.value {
		case "fragment":
			p.next()
			name, err := p.name()
			if err != nil {
				return nil, err
			}
			on, err := p.name()
			if err != nil || on != "on" {
				return nil, &gSyntaxError{`Expected "on"`, p.peek().loc}
			}
			cond, err := p.name()
			if err != nil {
				return nil, err
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sels, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			doc.fragments[name] = &gFragment{name, cond, sels}
		case "query", "mutation", "subscription":
			p.next()
			op := &gOperation{operation: t.value, loc: t.loc}
			if p.peek().kind == "name" {
				op.name, _ = p.name()
			}
			if p.is("(") {
				p.next()
				for !p.is(")") {
					if err := p.expect("$"); err != nil {
						return nil, err
					}
					vn, err := p.name()
					if err != nil {
						return nil, err
					}
					if err := p.expect(":"); err != nil {
						return nil, err
					}
					typ, err := p.typeRef()
					if err != nil {
						return nil, err
					}
					var def *gValue
					if p.is("=") {
						p.next()
						if def, err = p.value(true); err != nil {
							return nil, err
						}
					}
					if _, err := p.directives(); err != nil {
						return nil, err
					}
					op.vars = append(op.vars, gVarDef{vn, typ, def})
				}
				p.next()
			}
			if _, err := p.directives(); err != nil {
				return nil, err
			}
			sels, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			op.selections = sels
			doc.operations = append(doc.operations, op)
		default:
			return nil, &gSyntaxError{"Unexpected " + describeTok(t), t.loc}
		}
	}
	return doc, nil
}

func (p *gParser) typeRef() (*gTypeRef, error) {
	var t *gTypeRef
	if p.is("[") {
		p.next()
		inner, err := p.typeRef()
		if err != nil {
			return nil, err
		}
		if err := p.expect("]"); err != nil {
			return nil, err
		}
		t = &gTypeRef{kind: "List", of: inner}
	} else {
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		t = &gTypeRef{kind: "Named", name: n}
	}
	if p.is("!") {
		p.next()
		t = &gTypeRef{kind: "NonNull", of: t}
	}
	return t, nil
}

func (p *gParser) selectionSet() ([]*gSelection, error) {
	if err := p.expect("{"); err != nil {
		return nil, err
	}
	var out []*gSelection
	for !p.is("}") {
		if p.peek().kind == "eof" {
			return nil, &gSyntaxError{`Expected "}", found <EOF>`, p.peek().loc}
		}
		s, err := p.selection()
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	p.next()
	return out, nil
}

func (p *gParser) selection() (*gSelection, error) {
	start := p.peek()
	if p.is("...") {
		p.next()
		t := p.peek()
		if t.kind == "name" && t.value == "on" {
			p.next()
			cond, err := p.name()
			if err != nil {
				return nil, err
			}
			dirs, err := p.directives()
			if err != nil {
				return nil, err
			}
			sels, err := p.selectionSet()
			if err != nil {
				return nil, err
			}
			return &gSelection{kind: "InlineFragment", typeCondition: cond, directives: dirs, selections: sels, hasSelections: true, loc: start.loc}, nil
		}
		if t.kind == "name" {
			n, _ := p.name()
			dirs, err := p.directives()
			if err != nil {
				return nil, err
			}
			return &gSelection{kind: "FragmentSpread", name: n, directives: dirs, loc: start.loc}, nil
		}
		dirs, err := p.directives()
		if err != nil {
			return nil, err
		}
		sels, err := p.selectionSet()
		if err != nil {
			return nil, err
		}
		return &gSelection{kind: "InlineFragment", directives: dirs, selections: sels, hasSelections: true, loc: start.loc}, nil
	}
	n, err := p.name()
	if err != nil {
		return nil, err
	}
	s := &gSelection{kind: "Field", name: n, loc: start.loc}
	if p.is(":") {
		p.next()
		s.alias = n
		if s.name, err = p.name(); err != nil {
			return nil, err
		}
	}
	if p.is("(") {
		if s.args, err = p.arguments(); err != nil {
			return nil, err
		}
	}
	if s.directives, err = p.directives(); err != nil {
		return nil, err
	}
	if p.is("{") {
		if s.selections, err = p.selectionSet(); err != nil {
			return nil, err
		}
		s.hasSelections = true
	}
	return s, nil
}

func (p *gParser) arguments() ([]gArg, error) {
	if err := p.expect("("); err != nil {
		return nil, err
	}
	var out []gArg
	for !p.is(")") {
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		if err := p.expect(":"); err != nil {
			return nil, err
		}
		v, err := p.value(false)
		if err != nil {
			return nil, err
		}
		out = append(out, gArg{n, v})
	}
	p.next()
	return out, nil
}

func (p *gParser) directives() ([]gDirective, error) {
	var out []gDirective
	for p.is("@") {
		p.next()
		n, err := p.name()
		if err != nil {
			return nil, err
		}
		d := gDirective{name: n}
		if p.is("(") {
			if d.args, err = p.arguments(); err != nil {
				return nil, err
			}
		}
		out = append(out, d)
	}
	return out, nil
}

func (p *gParser) value(isConst bool) (*gValue, error) {
	t := p.next()
	switch t.kind {
	case "punct":
		switch t.value {
		case "$":
			if !isConst {
				n, err := p.name()
				return &gValue{kind: "Variable", value: n}, err
			}
		case "[":
			v := &gValue{kind: "List"}
			for !p.is("]") {
				if p.peek().kind == "eof" {
					return nil, &gSyntaxError{"Unexpected <EOF>", p.peek().loc}
				}
				item, err := p.value(isConst)
				if err != nil {
					return nil, err
				}
				v.list = append(v.list, item)
			}
			p.next()
			return v, nil
		case "{":
			v := &gValue{kind: "Object"}
			for !p.is("}") {
				n, err := p.name()
				if err != nil {
					return nil, err
				}
				if err := p.expect(":"); err != nil {
					return nil, err
				}
				item, err := p.value(isConst)
				if err != nil {
					return nil, err
				}
				v.fields = append(v.fields, gArg{n, item})
			}
			p.next()
			return v, nil
		}
		return nil, &gSyntaxError{"Unexpected " + describeTok(t), t.loc}
	case "int":
		return &gValue{kind: "Int", value: t.value}, nil
	case "float":
		return &gValue{kind: "Float", value: t.value}, nil
	case "string":
		return &gValue{kind: "String", value: t.value}, nil
	case "name":
		switch t.value {
		case "true", "false":
			return &gValue{kind: "Boolean", bval: t.value == "true"}, nil
		case "null":
			return &gValue{kind: "Null"}, nil
		}
		return &gValue{kind: "Enum", value: t.value}, nil
	}
	return nil, &gSyntaxError{"Unexpected <EOF>", t.loc}
}

// ================================================================ the schema

type gctx struct {
	forge     *Forge
	origin    string
	vars      M
	fragments map[string]*gFragment
	errors    []any
}

type fieldErr struct {
	typ, msg string
}

func (e *fieldErr) Error() string { return e.msg }

type resolver func(src any, args M, c *gctx) (any, error)

type argDef struct {
	name, typ string
	def       any
}

type fieldDef struct {
	name, typ string
	args      []argDef
	resolve   resolver
}

type typeDef struct {
	kind, name, desc string
	fields           []*fieldDef
	fieldIndex       map[string]*fieldDef
	interfaces       []string
	possible         []string
	enumValues       []string
	inputFields      []argDef
}

var gTypes = map[string]*typeDef{}
var gTypeOrder []string

func defType(kind, name, desc string) *typeDef {
	t := &typeDef{kind: kind, name: name, desc: desc, fieldIndex: map[string]*fieldDef{}}
	gTypes[name] = t
	gTypeOrder = append(gTypeOrder, name)
	return t
}

// f declares a field: f(t, "name", "Type!", resolver, "arg:Type", ...).
func (t *typeDef) f(name, typ string, r resolver, args ...string) *typeDef {
	fd := &fieldDef{name: name, typ: typ, resolve: r}
	for _, a := range args {
		n, ty, _ := strings.Cut(a, ":")
		fd.args = append(fd.args, argDef{name: n, typ: ty})
	}
	t.fields = append(t.fields, fd)
	t.fieldIndex[name] = fd
	return t
}

// plain declares fields read straight off M sources.
func (t *typeDef) plain(spec ...string) *typeDef {
	for _, s := range spec {
		n, ty, _ := strings.Cut(s, ":")
		t.f(n, ty, nil)
	}
	return t
}

var connArgs = []string{"first:Int", "last:Int", "after:String", "before:String"}

func withConn(extra ...string) []string { return append(append([]string{}, connArgs...), extra...) }

// sources
type repoS struct{ r *Repo }
type issueS struct {
	r *Repo
	i *Issue
}
type pullS struct {
	r *Repo
	p *Pull
}
type userS struct{ u *User }
type labelS struct {
	r *Repo
	l *Label
}
type commentS struct {
	r *Repo
	c *Comment
}
type commitS struct {
	r   *Repo
	oid string
}
type refS struct {
	r         *Repo
	name, oid string
}

func typename(v any) string {
	switch x := v.(type) {
	case repoS:
		return "Repository"
	case issueS:
		return "Issue"
	case pullS:
		return "PullRequest"
	case userS:
		return "User"
	case labelS:
		return "Label"
	case commentS:
		return "IssueComment"
	case commitS:
		return "Commit"
	case refS:
		return "Ref"
	case M:
		if s, ok := x["__typename"].(string); ok {
			return s
		}
	}
	return ""
}

func encodeCursor(i int) string {
	return base64.StdEncoding.EncodeToString([]byte("cursor:v2:" + strconv.Itoa(i)))
}

func decodeCursor(v any) (int, bool, error) {
	s, ok := v.(string)
	if !ok || s == "" {
		return 0, false, nil
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err == nil && strings.HasPrefix(string(raw), "cursor:v2:") {
		if n, err := strconv.Atoi(string(raw[10:])); err == nil {
			return n, true, nil
		}
	}
	return 0, false, &fieldErr{"INVALID_CURSOR_ARGUMENTS", fmt.Sprintf("`%s` does not appear to be a valid cursor.", s)}
}

func intArg(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), true
	case float64:
		return int(x), true
	}
	return 0, false
}

func connection[T any](typename string, items []T, args M, node func(T) any) (any, error) {
	start, end := 0, len(items)
	if a, ok, err := decodeCursor(args["after"]); err != nil {
		return nil, err
	} else if ok && a+1 > start {
		start = a + 1
	}
	if b, ok, err := decodeCursor(args["before"]); err != nil {
		return nil, err
	} else if ok && b < end {
		end = b
	}
	if start > len(items) {
		start = len(items)
	}
	if end < start {
		end = start
	}
	if n, ok := intArg(args["first"]); ok {
		if n < 0 {
			return nil, &fieldErr{"INVALID_PAGINATION", "`first` must be non-negative"}
		}
		if n > 100 {
			return nil, &fieldErr{"EXCESSIVE_PAGINATION", fmt.Sprintf("Requesting %d records on the connection exceeds the `first` limit of 100 records.", n)}
		}
		if start+n < end {
			end = start + n
		}
	}
	if n, ok := intArg(args["last"]); ok && end-n > start {
		start = end - n
	}
	nodes := []any{}
	edges := []any{}
	for i := start; i < end; i++ {
		n := node(items[i])
		nodes = append(nodes, n)
		edges = append(edges, M{"__typename": strings.TrimSuffix(typename, "Connection") + "Edge", "cursor": encodeCursor(i), "node": n})
	}
	var startC, endC any
	if end > start {
		startC, endC = encodeCursor(start), encodeCursor(end-1)
	}
	return M{"__typename": typename, "totalCount": len(items), "nodes": nodes, "edges": edges,
		"pageInfo": M{"__typename": "PageInfo", "hasNextPage": end < len(items), "hasPreviousPage": start > 0, "startCursor": startC, "endCursor": endC}}, nil
}

func htmlURL(c *gctx, r *Repo, tail string) string {
	return c.origin + "/" + r.Owner.Login + "/" + r.Name + tail
}

func pullState(p *Pull) string {
	switch {
	case p.Merged:
		return "MERGED"
	case p.State == "open":
		return "OPEN"
	}
	return "CLOSED"
}

type ordered interface {
	num() int
	created() string
	updated() string
	commentCount() int
}

func (i *Issue) num() int          { return i.Number }
func (i *Issue) created() string   { return i.CreatedAt }
func (i *Issue) updated() string   { return i.UpdatedAt }
func (i *Issue) commentCount() int { return len(i.Comments) }
func (p *Pull) num() int           { return p.Number }
func (p *Pull) created() string    { return p.CreatedAt }
func (p *Pull) updated() string    { return p.UpdatedAt }
func (p *Pull) commentCount() int  { return len(p.Comments) }

func orderBy[T ordered](list []T, order any) {
	field, dir := "CREATED_AT", "ASC"
	if m, ok := order.(M); ok {
		if f, ok := m["field"].(string); ok {
			field = f
		}
		if d, ok := m["direction"].(string); ok {
			dir = d
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		var less, eq bool
		switch field {
		case "UPDATED_AT":
			less, eq = a.updated() < b.updated(), a.updated() == b.updated()
		case "COMMENTS":
			less, eq = a.commentCount() < b.commentCount(), a.commentCount() == b.commentCount()
		default:
			less, eq = a.created() < b.created(), a.created() == b.created()
		}
		if eq {
			less = a.num() < b.num()
		}
		if dir == "DESC" {
			return !less && !(eq && a.num() == b.num())
		}
		return less
	})
}

func strList(v any) []string {
	var out []string
	if l, ok := v.([]any); ok {
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

func constant(v any) resolver { return func(any, M, *gctx) (any, error) { return v, nil } }

func optStr(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

type changedFile struct {
	path       string
	adds, dels int
	change     string
}

func changedFiles(c *gctx, s pullS) []changedFile {
	var out []changedFile
	for _, d := range c.forge.PullDiff(s.r, s.p) {
		a, b := fileStats(c.forge, d.OldOID, d.NewOID)
		ch := "MODIFIED"
		if d.Status == "added" {
			ch = "ADDED"
		} else if d.Status == "removed" {
			ch = "DELETED"
		}
		out = append(out, changedFile{d.Path, a, b, ch})
	}
	return out
}

func commitData(c *gctx, s commitS) (*Commit, error) {
	if cm := c.forge.CommitOf(s.oid); cm != nil {
		return cm, nil
	}
	return nil, &fieldErr{"NOT_FOUND", "Could not resolve to a commit with the oid " + s.oid + "."}
}

func init() {
	for _, s := range []string{"String", "Int", "Float", "Boolean", "ID", "DateTime", "URI", "GitObjectID", "HTML", "GitTimestamp", "GitSSHRemote", "Base64String"} {
		defType("SCALAR", s, "The "+s+" scalar.")
	}
	enums := [][]string{
		{"IssueState", "OPEN", "CLOSED"}, {"IssueStateReason", "COMPLETED", "NOT_PLANNED", "REOPENED", "DUPLICATE"},
		{"IssueClosedStateReason", "COMPLETED", "NOT_PLANNED", "DUPLICATE"}, {"PullRequestState", "OPEN", "CLOSED", "MERGED"},
		{"MergeableState", "MERGEABLE", "CONFLICTING", "UNKNOWN"}, {"PullRequestMergeMethod", "MERGE", "SQUASH", "REBASE"},
		{"RepositoryPermission", "ADMIN", "MAINTAIN", "WRITE", "TRIAGE", "READ"}, {"SearchType", "ISSUE", "ISSUE_ADVANCED", "REPOSITORY", "USER", "DISCUSSION"},
		{"IssueOrderField", "CREATED_AT", "UPDATED_AT", "COMMENTS"}, {"PullRequestOrderField", "CREATED_AT", "UPDATED_AT"},
		{"RepositoryOrderField", "CREATED_AT", "UPDATED_AT", "PUSHED_AT", "NAME", "STARGAZERS"}, {"RefOrderField", "TAG_COMMIT_DATE", "ALPHABETICAL"},
		{"OrderDirection", "ASC", "DESC"}, {"RepositoryVisibility", "PRIVATE", "PUBLIC", "INTERNAL"},
		{"PatchStatus", "ADDED", "DELETED", "RENAMED", "COPIED", "MODIFIED", "CHANGED"}, {"CommentAuthorAssociation", "OWNER", "MEMBER", "COLLABORATOR", "CONTRIBUTOR", "NONE"},
		{"PullRequestReviewDecision", "APPROVED", "CHANGES_REQUESTED", "REVIEW_REQUIRED"}, {"PullRequestUpdateState", "OPEN", "CLOSED"},
		{"__TypeKind", "SCALAR", "OBJECT", "INTERFACE", "UNION", "ENUM", "INPUT_OBJECT", "LIST", "NON_NULL"},
	}
	for _, e := range enums {
		defType("ENUM", e[0], "The "+e[0]+" enum.").enumValues = e[1:]
	}
	inputs := [][]string{
		{"IssueOrder", "field:IssueOrderField!", "direction:OrderDirection!"},
		{"IssueFilters", "states:[IssueState!]", "labels:[String!]", "assignee:String", "createdBy:String", "since:DateTime"},
		{"PullRequestOrder", "field:PullRequestOrderField!", "direction:OrderDirection!"},
		{"RepositoryOrder", "field:RepositoryOrderField!", "direction:OrderDirection!"},
		{"RefOrder", "field:RefOrderField!", "direction:OrderDirection!"},
		{"CreateIssueInput", "repositoryId:ID!", "title:String!", "body:String", "labelIds:[ID!]", "assigneeIds:[ID!]", "milestoneId:ID", "clientMutationId:String"},
		{"CloseIssueInput", "issueId:ID!", "stateReason:IssueClosedStateReason", "clientMutationId:String"},
		{"ReopenIssueInput", "issueId:ID!", "clientMutationId:String"},
		{"UpdateIssueInput", "id:ID!", "title:String", "body:String", "state:IssueState", "labelIds:[ID!]", "clientMutationId:String"},
		{"CreatePullRequestInput", "repositoryId:ID!", "baseRefName:String!", "headRefName:String!", "title:String!", "body:String", "draft:Boolean", "maintainerCanModify:Boolean", "headRepositoryId:ID", "clientMutationId:String"},
		{"MergePullRequestInput", "pullRequestId:ID!", "mergeMethod:PullRequestMergeMethod", "commitHeadline:String", "commitBody:String", "expectedHeadOid:GitObjectID", "authorEmail:String", "clientMutationId:String"},
		{"ClosePullRequestInput", "pullRequestId:ID!", "clientMutationId:String"},
		{"ReopenPullRequestInput", "pullRequestId:ID!", "clientMutationId:String"},
		{"UpdatePullRequestInput", "pullRequestId:ID!", "title:String", "body:String", "baseRefName:String", "state:PullRequestUpdateState", "clientMutationId:String"},
		{"AddCommentInput", "subjectId:ID!", "body:String!", "clientMutationId:String"},
		{"CreateRepositoryInput", "name:String!", "description:String", "visibility:RepositoryVisibility!", "ownerId:ID", "hasIssuesEnabled:Boolean", "clientMutationId:String"},
		{"CreateRefInput", "repositoryId:ID!", "name:String!", "oid:GitObjectID!", "clientMutationId:String"},
		{"AddLabelsToLabelableInput", "labelableId:ID!", "labelIds:[ID!]!", "clientMutationId:String"},
	}
	for _, in := range inputs {
		t := defType("INPUT_OBJECT", in[0], "The "+in[0]+" input.")
		for _, f := range in[1:] {
			n, ty, _ := strings.Cut(f, ":")
			t.inputFields = append(t.inputFields, argDef{name: n, typ: ty})
		}
	}

	node := defType("INTERFACE", "Node", "An object with an ID.")
	node.possible = []string{"Repository", "Issue", "PullRequest", "User", "IssueComment", "Label", "Ref", "Commit"}
	node.plain("id:ID!")
	actor := defType("INTERFACE", "Actor", "An object that can take actions.")
	actor.possible = []string{"User"}
	actor.plain("login:String!", "url:URI!", "resourcePath:URI!")
	actor.f("avatarUrl", "URI!", nil, "size:Int")
	owner := defType("INTERFACE", "RepositoryOwner", "An object that can own repositories.")
	owner.possible = []string{"User"}
	owner.plain("id:ID!", "login:String!", "url:URI!")
	owner.f("avatarUrl", "URI!", nil, "size:Int")
	owner.f("repository", "Repository", nil, "name:String!")
	gitObj := defType("INTERFACE", "GitObject", "A git object.")
	gitObj.possible = []string{"Commit"}
	gitObj.plain("id:ID!", "oid:GitObjectID!", "abbreviatedOid:String!", "commitUrl:URI!", "repository:Repository!")
	defType("UNION", "IssueOrPullRequest", "An issue or a pull request.").possible = []string{"Issue", "PullRequest"}
	defType("UNION", "SearchResultItem", "A search result.").possible = []string{"Issue", "PullRequest", "Repository", "User"}
	defType("OBJECT", "PageInfo", "Pagination information.").plain("hasNextPage:Boolean!", "hasPreviousPage:Boolean!", "startCursor:String", "endCursor:String")
	for _, n := range []string{"Repository", "Issue", "PullRequest", "Label", "IssueComment", "User", "Ref", "PullRequestChangedFile", "PullRequestCommit", "Commit"} {
		defType("OBJECT", n+"Connection", "The connection type for "+n+".").plain("nodes:["+n+"]", "edges:["+n+"Edge]", "pageInfo:PageInfo!", "totalCount:Int!")
		defType("OBJECT", n+"Edge", "An edge in a connection.").plain("cursor:String!", "node:"+n)
	}
	defType("OBJECT", "SearchResultItemConnection", "Search results.").plain("nodes:[SearchResultItem]", "edges:[SearchResultItemEdge]", "pageInfo:PageInfo!",
		"issueCount:Int!", "repositoryCount:Int!", "userCount:Int!", "discussionCount:Int!", "codeCount:Int!", "wikiCount:Int!")
	defType("OBJECT", "SearchResultItemEdge", "A search result edge.").plain("cursor:String!", "node:SearchResultItem")
	defType("OBJECT", "Language", "A programming language.").plain("name:String!")

	u := func(fn func(*User, M, *gctx) (any, error)) resolver {
		return func(src any, a M, c *gctx) (any, error) { return fn(src.(userS).u, a, c) }
	}
	user := defType("OBJECT", "User", "A user.")
	user.interfaces = []string{"Node", "Actor", "RepositoryOwner"}
	user.f("id", "ID!", u(func(x *User, _ M, _ *gctx) (any, error) { return x.NodeID, nil })).
		f("databaseId", "Int", u(func(x *User, _ M, _ *gctx) (any, error) { return x.ID, nil })).
		f("login", "String!", u(func(x *User, _ M, _ *gctx) (any, error) { return x.Login, nil })).
		f("name", "String", u(func(x *User, _ M, _ *gctx) (any, error) { return x.Name, nil })).
		f("email", "String!", u(func(x *User, _ M, _ *gctx) (any, error) { return x.Email, nil })).
		f("bio", "String", constant(nil)).f("company", "String", constant(nil)).f("location", "String", constant(nil)).
		f("isViewer", "Boolean!", u(func(x *User, _ M, c *gctx) (any, error) { return x == c.forge.Viewer, nil })).
		f("url", "URI!", u(func(x *User, _ M, c *gctx) (any, error) { return c.origin + "/" + x.Login, nil })).
		f("resourcePath", "URI!", u(func(x *User, _ M, _ *gctx) (any, error) { return "/" + x.Login, nil })).
		f("avatarUrl", "URI!", u(func(x *User, _ M, c *gctx) (any, error) { return c.origin + "/avatars/" + x.Login, nil }), "size:Int").
		f("createdAt", "DateTime!", constant("2026-01-01T00:00:00Z")).
		f("repository", "Repository", u(func(x *User, a M, c *gctx) (any, error) {
			n, _ := a["name"].(string)
			if r := c.forge.GetRepo(x.Login, n); r != nil {
				return repoS{r}, nil
			}
			return nil, nil
		}), "name:String!").
		f("repositories", "RepositoryConnection!", u(func(x *User, a M, c *gctx) (any, error) {
			var repos []*Repo
			for _, r := range c.forge.ReposOf(x) {
				if (a["privacy"] == "PRIVATE" && !r.Private) || (a["privacy"] == "PUBLIC" && r.Private) {
					continue
				}
				repos = append(repos, r)
			}
			if o, ok := a["orderBy"].(M); ok {
				if o["field"] == "NAME" {
					sort.SliceStable(repos, func(i, j int) bool { return strings.ToLower(repos[i].Name) < strings.ToLower(repos[j].Name) })
				}
				if o["direction"] == "DESC" {
					for i, j := 0, len(repos)-1; i < j; i, j = i+1, j-1 {
						repos[i], repos[j] = repos[j], repos[i]
					}
				}
			}
			return connection("RepositoryConnection", repos, a, func(r *Repo) any { return repoS{r} })
		}), withConn("orderBy:RepositoryOrder", "privacy:RepositoryVisibility", "ownerAffiliations:[String]", "isFork:Boolean")...)

	rp := func(fn func(*Repo, M, *gctx) (any, error)) resolver {
		return func(src any, a M, c *gctx) (any, error) { return fn(src.(repoS).r, a, c) }
	}
	rv := func(fn func(*Repo) any) resolver {
		return func(src any, _ M, _ *gctx) (any, error) { return fn(src.(repoS).r), nil }
	}
	repo := defType("OBJECT", "Repository", "A repository contains the content for a project.")
	repo.interfaces = []string{"Node"}
	repo.f("id", "ID!", rv(func(r *Repo) any { return r.NodeID })).
		f("databaseId", "Int", rv(func(r *Repo) any { return r.ID })).
		f("name", "String!", rv(func(r *Repo) any { return r.Name })).
		f("nameWithOwner", "String!", rv(func(r *Repo) any { return r.Owner.Login + "/" + r.Name })).
		f("owner", "RepositoryOwner!", rv(func(r *Repo) any { return userS{r.Owner} })).
		f("description", "String", rv(func(r *Repo) any { return optStr(r.Description) })).
		f("homepageUrl", "URI", constant(nil)).
		f("url", "URI!", rp(func(r *Repo, _ M, c *gctx) (any, error) { return htmlURL(c, r, ""), nil })).
		f("sshUrl", "GitSSHRemote!", rv(func(r *Repo) any { return "git@forge:" + r.Owner.Login + "/" + r.Name + ".git" })).
		f("resourcePath", "URI!", rv(func(r *Repo) any { return "/" + r.Owner.Login + "/" + r.Name })).
		f("isPrivate", "Boolean!", rv(func(r *Repo) any { return r.Private })).
		f("visibility", "RepositoryVisibility!", rv(func(r *Repo) any {
			if r.Private {
				return "PRIVATE"
			}
			return "PUBLIC"
		})).
		f("isFork", "Boolean!", constant(false)).f("isArchived", "Boolean!", constant(false)).f("isTemplate", "Boolean!", constant(false)).
		f("isEmpty", "Boolean!", rv(func(r *Repo) any { return len(r.Refs) == 0 })).
		f("isInOrganization", "Boolean!", constant(false)).f("isMirror", "Boolean!", constant(false)).f("isLocked", "Boolean!", constant(false)).
		f("isDisabled", "Boolean!", constant(false)).
		f("createdAt", "DateTime!", rv(func(r *Repo) any { return r.CreatedAt })).
		f("updatedAt", "DateTime!", rv(func(r *Repo) any { return r.UpdatedAt })).
		f("pushedAt", "DateTime", rv(func(r *Repo) any { return optStr(r.PushedAt) })).
		f("diskUsage", "Int", constant(0)).f("forkCount", "Int!", constant(0)).f("stargazerCount", "Int!", constant(0)).
		f("viewerHasStarred", "Boolean!", constant(false)).f("viewerPermission", "RepositoryPermission", constant("ADMIN")).
		f("viewerCanAdminister", "Boolean!", constant(true)).f("viewerDefaultMergeMethod", "PullRequestMergeMethod!", constant("MERGE")).
		f("hasIssuesEnabled", "Boolean!", rv(func(r *Repo) any { return r.HasIssues })).
		f("hasWikiEnabled", "Boolean!", constant(false)).f("hasProjectsEnabled", "Boolean!", constant(false)).f("hasDiscussionsEnabled", "Boolean!", constant(false)).
		f("mergeCommitAllowed", "Boolean!", constant(true)).f("squashMergeAllowed", "Boolean!", constant(true)).f("rebaseMergeAllowed", "Boolean!", constant(true)).
		f("deleteBranchOnMerge", "Boolean!", constant(false)).f("primaryLanguage", "Language", constant(nil)).f("parent", "Repository", constant(nil)).
		f("defaultBranchRef", "Ref", rp(func(r *Repo, _ M, c *gctx) (any, error) {
			name := "refs/heads/" + r.DefaultBranch
			if oid, ok := r.Refs[name]; ok {
				return refS{r, name, oid}, nil
			}
			return nil, nil
		})).
		f("ref", "Ref", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			q, _ := a["qualifiedName"].(string)
			for _, n := range []string{q, "refs/heads/" + q, "refs/tags/" + q} {
				if !strings.HasPrefix(n, "refs/") {
					continue
				}
				if oid, ok := r.Refs[n]; ok {
					return refS{r, n, oid}, nil
				}
			}
			return nil, nil
		}), "qualifiedName:String!").
		f("refs", "RefConnection", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			prefix, _ := a["refPrefix"].(string)
			q, _ := a["query"].(string)
			var names []string
			for _, n := range sortedRefNames(r) {
				if strings.HasPrefix(n, prefix) && strings.Contains(n, q) {
					names = append(names, n)
				}
			}
			return connection("RefConnection", names, a, func(n string) any { return refS{r, n, r.Refs[n]} })
		}), withConn("refPrefix:String!", "query:String", "orderBy:RefOrder", "direction:OrderDirection")...).
		f("object", "GitObject", rp(func(r *Repo, a M, c *gctx) (any, error) {
			target, _ := a["oid"].(string)
			if target == "" {
				target, _ = a["expression"].(string)
			}
			if target == "" {
				return nil, nil
			}
			if i := strings.IndexByte(target, ':'); i >= 0 {
				target = target[:i]
			}
			if oid := c.forge.Resolve(r, target); oid != "" {
				return commitS{r, oid}, nil
			}
			return nil, nil
		}), "expression:String", "oid:GitObjectID").
		f("issue", "Issue", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			n, _ := intArg(a["number"])
			if i := r.Issues[n]; i != nil {
				return issueS{r, i}, nil
			}
			return nil, nil
		}), "number:Int!").
		f("issueOrPullRequest", "IssueOrPullRequest", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			n, _ := intArg(a["number"])
			if i := r.Issues[n]; i != nil {
				return issueS{r, i}, nil
			}
			if p := r.Pulls[n]; p != nil {
				return pullS{r, p}, nil
			}
			return nil, nil
		}), "number:Int!").
		f("issues", "IssueConnection!", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			states := strList(a["states"])
			labels := strList(a["labels"])
			if fb, ok := a["filterBy"].(M); ok {
				if len(states) == 0 {
					states = strList(fb["states"])
				}
				if len(labels) == 0 {
					labels = strList(fb["labels"])
				}
			}
			list := filterIssues(r, "all", labels)
			if len(states) > 0 {
				var kept []*Issue
				for _, i := range list {
					for _, s := range states {
						if strings.ToUpper(i.State) == s {
							kept = append(kept, i)
						}
					}
				}
				list = kept
			}
			orderBy(list, a["orderBy"])
			return connection("IssueConnection", list, a, func(i *Issue) any { return issueS{r, i} })
		}), withConn("states:[IssueState!]", "labels:[String!]", "orderBy:IssueOrder", "filterBy:IssueFilters")...).
		f("pullRequest", "PullRequest", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			n, _ := intArg(a["number"])
			if p := r.Pulls[n]; p != nil {
				return pullS{r, p}, nil
			}
			return nil, nil
		}), "number:Int!").
		f("pullRequests", "PullRequestConnection!", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			states := strList(a["states"])
			head, _ := a["headRefName"].(string)
			base, _ := a["baseRefName"].(string)
			var list []*Pull
			for _, p := range r.pullOrder {
				okState := len(states) == 0
				for _, s := range states {
					if pullState(p) == s {
						okState = true
					}
				}
				if okState && (head == "" || p.Head == head) && (base == "" || p.Base == base) {
					list = append(list, p)
				}
			}
			orderBy(list, a["orderBy"])
			return connection("PullRequestConnection", list, a, func(p *Pull) any { return pullS{r, p} })
		}), withConn("states:[PullRequestState!]", "labels:[String!]", "headRefName:String", "baseRefName:String", "orderBy:IssueOrder")...).
		f("labels", "LabelConnection", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			q, _ := a["query"].(string)
			var list []*Label
			for _, l := range r.labelOrder {
				if strings.Contains(strings.ToLower(l.Name), strings.ToLower(q)) {
					list = append(list, l)
				}
			}
			return connection("LabelConnection", list, a, func(l *Label) any { return labelS{r, l} })
		}), withConn("query:String", "orderBy:IssueOrder")...).
		f("label", "Label", rp(func(r *Repo, a M, _ *gctx) (any, error) {
			n, _ := a["name"].(string)
			if l := r.Labels[strings.ToLower(n)]; l != nil {
				return labelS{r, l}, nil
			}
			return nil, nil
		}), "name:String!")

	is := func(fn func(issueS) any) resolver {
		return func(src any, _ M, _ *gctx) (any, error) { return fn(src.(issueS)), nil }
	}
	issue := defType("OBJECT", "Issue", "An issue in a repository.")
	issue.interfaces = []string{"Node"}
	issue.f("id", "ID!", is(func(s issueS) any { return s.i.NodeID })).
		f("databaseId", "Int", is(func(s issueS) any { return s.i.ID })).
		f("number", "Int!", is(func(s issueS) any { return s.i.Number })).
		f("title", "String!", is(func(s issueS) any { return s.i.Title })).
		f("body", "String!", is(func(s issueS) any { return s.i.Body })).
		f("bodyText", "String!", is(func(s issueS) any { return s.i.Body })).
		f("bodyHTML", "HTML!", is(func(s issueS) any { return s.i.Body })).
		f("state", "IssueState!", is(func(s issueS) any { return strings.ToUpper(s.i.State) })).
		f("stateReason", "IssueStateReason", is(func(s issueS) any {
			if s.i.StateReason == nil {
				return nil
			}
			return strings.ToUpper(*s.i.StateReason)
		})).
		f("closed", "Boolean!", is(func(s issueS) any { return s.i.State == "closed" })).
		f("closedAt", "DateTime", is(func(s issueS) any { return optStr(s.i.ClosedAt) })).
		f("createdAt", "DateTime!", is(func(s issueS) any { return s.i.CreatedAt })).
		f("updatedAt", "DateTime!", is(func(s issueS) any { return s.i.UpdatedAt })).
		f("locked", "Boolean!", constant(false)).
		f("url", "URI!", func(src any, _ M, c *gctx) (any, error) {
			s := src.(issueS)
			return htmlURL(c, s.r, "/issues/"+strconv.Itoa(s.i.Number)), nil
		}).
		f("resourcePath", "URI!", is(func(s issueS) any {
			return "/" + s.r.Owner.Login + "/" + s.r.Name + "/issues/" + strconv.Itoa(s.i.Number)
		})).
		f("author", "Actor", is(func(s issueS) any { return userS{s.i.User} })).
		f("authorAssociation", "CommentAuthorAssociation!", constant("OWNER")).
		f("repository", "Repository!", is(func(s issueS) any { return repoS{s.r} })).
		f("viewerCanUpdate", "Boolean!", constant(true)).
		f("labels", "LabelConnection", func(src any, a M, _ *gctx) (any, error) {
			s := src.(issueS)
			return connection("LabelConnection", s.i.Labels, a, func(l *Label) any { return labelS{s.r, l} })
		}, withConn("orderBy:IssueOrder")...).
		f("comments", "IssueCommentConnection!", func(src any, a M, _ *gctx) (any, error) {
			s := src.(issueS)
			return connection("IssueCommentConnection", s.i.Comments, a, func(cm *Comment) any { return commentS{s.r, cm} })
		}, withConn("orderBy:IssueOrder")...).
		f("assignees", "UserConnection!", func(_ any, a M, _ *gctx) (any, error) {
			return connection("UserConnection", []*User{}, a, func(x *User) any { return userS{x} })
		}, connArgs...).
		f("participants", "UserConnection!", func(src any, a M, _ *gctx) (any, error) {
			return connection("UserConnection", []*User{src.(issueS).i.User}, a, func(x *User) any { return userS{x} })
		}, connArgs...)

	ps := func(fn func(pullS, *gctx) any) resolver {
		return func(src any, _ M, c *gctx) (any, error) { return fn(src.(pullS), c), nil }
	}
	pull := defType("OBJECT", "PullRequest", "A repository pull request.")
	pull.interfaces = []string{"Node"}
	pull.f("id", "ID!", ps(func(s pullS, _ *gctx) any { return s.p.NodeID })).
		f("databaseId", "Int", ps(func(s pullS, _ *gctx) any { return s.p.ID })).
		f("number", "Int!", ps(func(s pullS, _ *gctx) any { return s.p.Number })).
		f("title", "String!", ps(func(s pullS, _ *gctx) any { return s.p.Title })).
		f("body", "String!", ps(func(s pullS, _ *gctx) any { return s.p.Body })).
		f("bodyText", "String!", ps(func(s pullS, _ *gctx) any { return s.p.Body })).
		f("state", "PullRequestState!", ps(func(s pullS, _ *gctx) any { return pullState(s.p) })).
		f("closed", "Boolean!", ps(func(s pullS, _ *gctx) any { return s.p.State == "closed" })).
		f("merged", "Boolean!", ps(func(s pullS, _ *gctx) any { return s.p.Merged })).
		f("mergedAt", "DateTime", ps(func(s pullS, _ *gctx) any { return optStr(s.p.MergedAt) })).
		f("mergedBy", "Actor", ps(func(s pullS, c *gctx) any {
			if s.p.Merged {
				return userS{c.forge.Viewer}
			}
			return nil
		})).
		f("closedAt", "DateTime", ps(func(s pullS, _ *gctx) any { return optStr(s.p.ClosedAt) })).
		f("createdAt", "DateTime!", ps(func(s pullS, _ *gctx) any { return s.p.CreatedAt })).
		f("updatedAt", "DateTime!", ps(func(s pullS, _ *gctx) any { return s.p.UpdatedAt })).
		f("isDraft", "Boolean!", ps(func(s pullS, _ *gctx) any { return s.p.Draft })).
		f("locked", "Boolean!", constant(false)).f("isCrossRepository", "Boolean!", constant(false)).f("maintainerCanModify", "Boolean!", constant(false)).
		f("mergeable", "MergeableState!", ps(func(s pullS, c *gctx) any {
			switch c.forge.Mergeable(s.r, s.p) {
			case 1:
				return "MERGEABLE"
			case 0:
				return "CONFLICTING"
			}
			return "UNKNOWN"
		})).
		f("reviewDecision", "PullRequestReviewDecision", constant(nil)).
		f("headRefName", "String!", ps(func(s pullS, _ *gctx) any { return s.p.Head })).
		f("baseRefName", "String!", ps(func(s pullS, _ *gctx) any { return s.p.Base })).
		f("headRefOid", "GitObjectID!", ps(func(s pullS, c *gctx) any { return c.forge.PullHeadSHA(s.r, s.p) })).
		f("baseRefOid", "GitObjectID!", ps(func(s pullS, c *gctx) any { return c.forge.PullBaseSHA(s.r, s.p) })).
		f("headRef", "Ref", ps(func(s pullS, c *gctx) any {
			if oid := c.forge.BranchTip(s.r, s.p.Head); oid != "" {
				return refS{s.r, "refs/heads/" + s.p.Head, oid}
			}
			return nil
		})).
		f("baseRef", "Ref", ps(func(s pullS, c *gctx) any {
			if oid := c.forge.BranchTip(s.r, s.p.Base); oid != "" {
				return refS{s.r, "refs/heads/" + s.p.Base, oid}
			}
			return nil
		})).
		f("headRepository", "Repository", ps(func(s pullS, _ *gctx) any { return repoS{s.r} })).
		f("headRepositoryOwner", "RepositoryOwner", ps(func(s pullS, _ *gctx) any { return userS{s.r.Owner} })).
		f("baseRepository", "Repository", ps(func(s pullS, _ *gctx) any { return repoS{s.r} })).
		f("repository", "Repository!", ps(func(s pullS, _ *gctx) any { return repoS{s.r} })).
		f("url", "URI!", ps(func(s pullS, c *gctx) any { return htmlURL(c, s.r, "/pull/"+strconv.Itoa(s.p.Number)) })).
		f("permalink", "URI!", ps(func(s pullS, c *gctx) any { return htmlURL(c, s.r, "/pull/"+strconv.Itoa(s.p.Number)) })).
		f("resourcePath", "URI!", ps(func(s pullS, _ *gctx) any {
			return "/" + s.r.Owner.Login + "/" + s.r.Name + "/pull/" + strconv.Itoa(s.p.Number)
		})).
		f("author", "Actor", ps(func(s pullS, _ *gctx) any { return userS{s.p.User} })).
		f("authorAssociation", "CommentAuthorAssociation!", constant("OWNER")).
		f("mergeCommit", "Commit", ps(func(s pullS, _ *gctx) any {
			if s.p.MergeCommitSHA == nil {
				return nil
			}
			return commitS{s.r, *s.p.MergeCommitSHA}
		})).
		f("additions", "Int!", ps(func(s pullS, c *gctx) any {
			n := 0
			for _, x := range changedFiles(c, s) {
				n += x.adds
			}
			return n
		})).
		f("deletions", "Int!", ps(func(s pullS, c *gctx) any {
			n := 0
			for _, x := range changedFiles(c, s) {
				n += x.dels
			}
			return n
		})).
		f("changedFiles", "Int!", ps(func(s pullS, c *gctx) any { return len(changedFiles(c, s)) })).
		f("files", "PullRequestChangedFileConnection", func(src any, a M, c *gctx) (any, error) {
			return connection("PullRequestChangedFileConnection", changedFiles(c, src.(pullS)), a, func(x changedFile) any {
				return M{"__typename": "PullRequestChangedFile", "path": x.path, "additions": x.adds, "deletions": x.dels, "changeType": x.change}
			})
		}, connArgs...).
		f("commits", "PullRequestCommitConnection!", func(src any, a M, c *gctx) (any, error) {
			s := src.(pullS)
			return connection("PullRequestCommitConnection", pullCommits(c.forge, s.r, s.p), a, func(oid string) any {
				return M{"__typename": "PullRequestCommit", "id": "PRC_" + oid, "commit": commitS{s.r, oid}, "url": htmlURL(c, s.r, "/pull/"+strconv.Itoa(s.p.Number)+"/commits/"+oid)}
			})
		}, connArgs...).
		f("labels", "LabelConnection", func(src any, a M, _ *gctx) (any, error) {
			s := src.(pullS)
			return connection("LabelConnection", s.p.Labels, a, func(l *Label) any { return labelS{s.r, l} })
		}, connArgs...).
		f("comments", "IssueCommentConnection!", func(src any, a M, _ *gctx) (any, error) {
			s := src.(pullS)
			return connection("IssueCommentConnection", s.p.Comments, a, func(cm *Comment) any { return commentS{s.r, cm} })
		}, connArgs...).
		f("assignees", "UserConnection!", func(_ any, a M, _ *gctx) (any, error) {
			return connection("UserConnection", []*User{}, a, func(x *User) any { return userS{x} })
		}, connArgs...)

	defType("OBJECT", "PullRequestChangedFile", "A file changed in a pull request.").plain("path:String!", "additions:Int!", "deletions:Int!", "changeType:PatchStatus!")
	prc := defType("OBJECT", "PullRequestCommit", "A commit of a pull request.")
	prc.interfaces = []string{"Node"}
	prc.plain("id:ID!", "commit:Commit!", "url:URI!")

	label := defType("OBJECT", "Label", "A label for an issue or pull request.")
	label.interfaces = []string{"Node"}
	lb := func(fn func(labelS, *gctx) any) resolver {
		return func(src any, _ M, c *gctx) (any, error) { return fn(src.(labelS), c), nil }
	}
	label.f("id", "ID!", lb(func(s labelS, _ *gctx) any { return s.l.NodeID })).
		f("name", "String!", lb(func(s labelS, _ *gctx) any { return s.l.Name })).
		f("color", "String!", lb(func(s labelS, _ *gctx) any { return s.l.Color })).
		f("description", "String", lb(func(s labelS, _ *gctx) any { return optStr(s.l.Description) })).
		f("isDefault", "Boolean!", constant(false)).
		f("url", "URI!", lb(func(s labelS, c *gctx) any { return htmlURL(c, s.r, "/labels/"+s.l.Name) }))

	comment := defType("OBJECT", "IssueComment", "A comment on an issue or pull request.")
	comment.interfaces = []string{"Node"}
	cs := func(fn func(commentS, *gctx) any) resolver {
		return func(src any, _ M, c *gctx) (any, error) { return fn(src.(commentS), c), nil }
	}
	comment.f("id", "ID!", cs(func(s commentS, _ *gctx) any { return s.c.NodeID })).
		f("databaseId", "Int", cs(func(s commentS, _ *gctx) any { return s.c.ID })).
		f("body", "String!", cs(func(s commentS, _ *gctx) any { return s.c.Body })).
		f("bodyText", "String!", cs(func(s commentS, _ *gctx) any { return s.c.Body })).
		f("createdAt", "DateTime!", cs(func(s commentS, _ *gctx) any { return s.c.CreatedAt })).
		f("updatedAt", "DateTime!", cs(func(s commentS, _ *gctx) any { return s.c.UpdatedAt })).
		f("author", "Actor", cs(func(s commentS, _ *gctx) any { return userS{s.c.User} })).
		f("authorAssociation", "CommentAuthorAssociation!", constant("OWNER")).
		f("url", "URI!", cs(func(s commentS, c *gctx) any {
			return htmlURL(c, s.r, fmt.Sprintf("/issues/%d#issuecomment-%d", s.c.Number, s.c.ID))
		})).
		f("viewerCanUpdate", "Boolean!", constant(true))

	ref := defType("OBJECT", "Ref", "A git reference.")
	ref.interfaces = []string{"Node"}
	rf := func(fn func(refS, *gctx) any) resolver {
		return func(src any, _ M, c *gctx) (any, error) { return fn(src.(refS), c), nil }
	}
	ref.f("id", "ID!", rf(func(s refS, _ *gctx) any {
		return "REF_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("ref:%d:%s", s.r.ID, s.name)))
	})).
		f("name", "String!", rf(func(s refS, _ *gctx) any {
			return strings.TrimPrefix(strings.TrimPrefix(s.name, "refs/heads/"), "refs/tags/")
		})).
		f("prefix", "String!", rf(func(s refS, _ *gctx) any {
			for _, p := range []string{"refs/heads/", "refs/tags/"} {
				if strings.HasPrefix(s.name, p) {
					return p
				}
			}
			return s.name[:strings.LastIndexByte(s.name, '/')+1]
		})).
		f("target", "GitObject", rf(func(s refS, c *gctx) any { return commitS{s.r, c.forge.Peel(s.oid)} })).
		f("repository", "Repository!", rf(func(s refS, _ *gctx) any { return repoS{s.r} }))

	defType("OBJECT", "GitActor", "Git actor information.").plain("name:String", "email:String", "date:GitTimestamp")
	defType("OBJECT", "Tree", "A git tree.").plain("oid:GitObjectID!")
	sig := func(s Signature) M {
		return M{"__typename": "GitActor", "name": s.Name, "email": s.Email, "date": isoFromEpoch(s.Time)}
	}
	cm := func(fn func(commitS, *Commit, *gctx) any) resolver {
		return func(src any, _ M, c *gctx) (any, error) {
			s := src.(commitS)
			data, err := commitData(c, s)
			if err != nil {
				return nil, err
			}
			return fn(s, data, c), nil
		}
	}
	commit := defType("OBJECT", "Commit", "A git commit.")
	commit.interfaces = []string{"Node", "GitObject"}
	commit.f("id", "ID!", func(src any, _ M, _ *gctx) (any, error) { return commitNodeID(src.(commitS).oid), nil }).
		f("oid", "GitObjectID!", func(src any, _ M, _ *gctx) (any, error) { return src.(commitS).oid, nil }).
		f("abbreviatedOid", "String!", func(src any, _ M, _ *gctx) (any, error) { return src.(commitS).oid[:7], nil }).
		f("commitUrl", "URI!", func(src any, _ M, c *gctx) (any, error) {
			s := src.(commitS)
			return htmlURL(c, s.r, "/commit/"+s.oid), nil
		}).
		f("url", "URI!", func(src any, _ M, c *gctx) (any, error) {
			s := src.(commitS)
			return htmlURL(c, s.r, "/commit/"+s.oid), nil
		}).
		f("repository", "Repository!", func(src any, _ M, _ *gctx) (any, error) { return repoS{src.(commitS).r}, nil }).
		f("message", "String!", cm(func(_ commitS, d *Commit, _ *gctx) any { return trimMessage(d.Message) })).
		f("messageHeadline", "String!", cm(func(_ commitS, d *Commit, _ *gctx) any { return strings.SplitN(d.Message, "\n", 2)[0] })).
		f("messageBody", "String!", cm(func(_ commitS, d *Commit, _ *gctx) any {
			parts := strings.SplitN(d.Message, "\n", 3)
			if len(parts) < 3 {
				return ""
			}
			return trimMessage(parts[2])
		})).
		f("committedDate", "DateTime!", cm(func(_ commitS, d *Commit, _ *gctx) any { return isoFromEpoch(d.Committer.Time) })).
		f("authoredDate", "DateTime!", cm(func(_ commitS, d *Commit, _ *gctx) any { return isoFromEpoch(d.Author.Time) })).
		f("author", "GitActor", cm(func(_ commitS, d *Commit, _ *gctx) any { return sig(d.Author) })).
		f("committer", "GitActor", cm(func(_ commitS, d *Commit, _ *gctx) any { return sig(d.Committer) })).
		f("tree", "Tree!", cm(func(_ commitS, d *Commit, _ *gctx) any { return M{"__typename": "Tree", "oid": d.Tree} })).
		f("parents", "CommitConnection!", func(src any, a M, c *gctx) (any, error) {
			s := src.(commitS)
			d, err := commitData(c, s)
			if err != nil {
				return nil, err
			}
			return connection("CommitConnection", d.Parents, a, func(oid string) any { return commitS{s.r, oid} })
		}, connArgs...).
		f("history", "CommitConnection!", func(src any, a M, c *gctx) (any, error) {
			s := src.(commitS)
			return connection("CommitConnection", c.forge.WalkCommits([]string{s.oid}, 10000), a, func(oid string) any { return commitS{s.r, oid} })
		}, connArgs...)

	defType("OBJECT", "RateLimit", "The API rate limit.").plain("limit:Int!", "cost:Int!", "remaining:Int!", "resetAt:DateTime!", "used:Int!", "nodeCount:Int!")

	query := defType("OBJECT", "Query", "The query root of the GraphQL interface.")
	findUser := func(_ any, a M, c *gctx) (any, error) {
		l, _ := a["login"].(string)
		if u := c.forge.Users[strings.ToLower(l)]; u != nil {
			return userS{u}, nil
		}
		return nil, nil
	}
	query.f("viewer", "User!", func(_ any, _ M, c *gctx) (any, error) { return userS{c.forge.Viewer}, nil }).
		f("user", "User", findUser, "login:String!").
		f("repositoryOwner", "RepositoryOwner", findUser, "login:String!").
		f("organization", "User", constant(nil), "login:String!").
		f("repository", "Repository", func(_ any, a M, c *gctx) (any, error) {
			o, _ := a["owner"].(string)
			n, _ := a["name"].(string)
			if r := c.forge.GetRepo(o, n); r != nil {
				return repoS{r}, nil
			}
			return nil, nil
		}, "owner:String!", "name:String!", "followRenames:Boolean").
		f("node", "Node", func(_ any, a M, c *gctx) (any, error) { return nodeByID(c, a["id"]), nil }, "id:ID!").
		f("nodes", "[Node]!", func(_ any, a M, c *gctx) (any, error) {
			var out []any
			if ids, ok := a["ids"].([]any); ok {
				for _, id := range ids {
					out = append(out, nodeByID(c, id))
				}
			}
			if out == nil {
				out = []any{}
			}
			return out, nil
		}, "ids:[ID!]!").
		f("rateLimit", "RateLimit", func(any, M, *gctx) (any, error) {
			return M{"__typename": "RateLimit", "limit": 5000, "cost": 1, "remaining": 4999, "resetAt": isoNow(), "used": 1, "nodeCount": 1}, nil
		}, "dryRun:Boolean").
		f("search", "SearchResultItemConnection!", searchItems, withConn("query:String!", "type:SearchType!")...)

	payload := func(name string, fields ...string) {
		defType("OBJECT", name, "The "+name+".").plain(append([]string{"clientMutationId:String"}, fields...)...)
	}
	payload("CreateIssuePayload", "issue:Issue")
	payload("CloseIssuePayload", "issue:Issue")
	payload("ReopenIssuePayload", "issue:Issue")
	payload("UpdateIssuePayload", "issue:Issue")
	payload("CreatePullRequestPayload", "pullRequest:PullRequest")
	payload("MergePullRequestPayload", "pullRequest:PullRequest")
	payload("ClosePullRequestPayload", "pullRequest:PullRequest")
	payload("ReopenPullRequestPayload", "pullRequest:PullRequest")
	payload("UpdatePullRequestPayload", "pullRequest:PullRequest")
	payload("AddCommentPayload", "commentEdge:IssueCommentEdge", "subject:Node", "timelineEdge:IssueCommentEdge")
	payload("CreateRepositoryPayload", "repository:Repository")
	payload("CreateRefPayload", "ref:Ref")
	payload("AddLabelsToLabelablePayload", "labelable:IssueOrPullRequest")

	mutation := defType("OBJECT", "Mutation", "The root query for implementing GraphQL mutations.")
	mut := func(name, payloadType, input string, fn func(in M, c *gctx) (M, error)) {
		mutation.f(name, payloadType, func(_ any, a M, c *gctx) (any, error) {
			in, _ := a["input"].(M)
			if in == nil {
				in = M{}
			}
			out, err := fn(in, c)
			if err != nil {
				return nil, err
			}
			out["clientMutationId"] = in["clientMutationId"]
			return out, nil
		}, "input:"+input+"!")
	}
	mut("createIssue", "CreateIssuePayload", "CreateIssueInput", func(in M, c *gctx) (M, error) {
		rs, err := needRepo(c, in["repositoryId"])
		if err != nil {
			return nil, err
		}
		title, _ := in["title"].(string)
		if strings.TrimSpace(title) == "" {
			return nil, &fieldErr{"UNPROCESSABLE", "Title can't be blank"}
		}
		labels, err := labelIDs(c, rs.r, in["labelIds"])
		if err != nil {
			return nil, err
		}
		body, _ := in["body"].(string)
		return M{"issue": issueS{rs.r, c.forge.CreateIssue(rs.r, title, body, labels)}}, nil
	})
	mut("closeIssue", "CloseIssuePayload", "CloseIssueInput", func(in M, c *gctx) (M, error) {
		s, err := needIssue(c, in["issueId"])
		if err != nil {
			return nil, err
		}
		reason, _ := in["stateReason"].(string)
		c.forge.SetIssueState(s.i, "closed", strings.ToLower(reason))
		return M{"issue": s}, nil
	})
	mut("reopenIssue", "ReopenIssuePayload", "ReopenIssueInput", func(in M, c *gctx) (M, error) {
		s, err := needIssue(c, in["issueId"])
		if err != nil {
			return nil, err
		}
		c.forge.SetIssueState(s.i, "open", "reopened")
		return M{"issue": s}, nil
	})
	mut("updateIssue", "UpdateIssuePayload", "UpdateIssueInput", func(in M, c *gctx) (M, error) {
		s, err := needIssue(c, in["id"])
		if err != nil {
			return nil, err
		}
		if t, ok := in["title"].(string); ok {
			s.i.Title = t
		}
		if b, ok := in["body"].(string); ok {
			s.i.Body = b
		}
		if in["labelIds"] != nil {
			labels, err := labelIDs(c, s.r, in["labelIds"])
			if err != nil {
				return nil, err
			}
			s.i.Labels = labels
		}
		if st, ok := in["state"].(string); ok && (st == "OPEN" || st == "CLOSED") {
			c.forge.SetIssueState(s.i, strings.ToLower(st), "")
		}
		c.forge.TouchIssue(s.i)
		return M{"issue": s}, nil
	})
	mut("addComment", "AddCommentPayload", "AddCommentInput", func(in M, c *gctx) (M, error) {
		node := nodeByID(c, in["subjectId"])
		body, _ := in["body"].(string)
		var list *[]*Comment
		var r *Repo
		var number int
		switch s := node.(type) {
		case issueS:
			list, r, number = &s.i.Comments, s.r, s.i.Number
		case pullS:
			list, r, number = &s.p.Comments, s.r, s.p.Number
		default:
			return nil, &fieldErr{"NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%v'.", in["subjectId"])}
		}
		if body == "" {
			return nil, &fieldErr{"UNPROCESSABLE", "Body can't be blank"}
		}
		cmt := c.forge.AddComment(r, number, list, body)
		edge := M{"__typename": "IssueCommentEdge", "cursor": encodeCursor(len(*list) - 1), "node": commentS{r, cmt}}
		return M{"commentEdge": edge, "timelineEdge": edge, "subject": node}, nil
	})
	mut("createPullRequest", "CreatePullRequestPayload", "CreatePullRequestInput", func(in M, c *gctx) (M, error) {
		rs, err := needRepo(c, in["repositoryId"])
		if err != nil {
			return nil, err
		}
		title, _ := in["title"].(string)
		body, _ := in["body"].(string)
		head, _ := in["headRefName"].(string)
		base, _ := in["baseRefName"].(string)
		p, err := openPull(c.forge, rs.r, title, body, head, base, in["draft"] == true)
		if err != nil {
			return nil, &fieldErr{"UNPROCESSABLE", err.Error()}
		}
		return M{"pullRequest": pullS{rs.r, p}}, nil
	})
	mut("mergePullRequest", "MergePullRequestPayload", "MergePullRequestInput", func(in M, c *gctx) (M, error) {
		s, err := needPull(c, in["pullRequestId"])
		if err != nil {
			return nil, err
		}
		method, _ := in["mergeMethod"].(string)
		if method == "" {
			method = "MERGE"
		}
		title, _ := in["commitHeadline"].(string)
		msg, _ := in["commitBody"].(string)
		expected, _ := in["expectedHeadOid"].(string)
		if _, err := c.forge.MergePull(s.r, s.p, strings.ToLower(method), title, msg, expected); err != nil {
			return nil, &fieldErr{"UNPROCESSABLE", err.Error()}
		}
		return M{"pullRequest": s}, nil
	})
	mut("closePullRequest", "ClosePullRequestPayload", "ClosePullRequestInput", func(in M, c *gctx) (M, error) {
		s, err := needPull(c, in["pullRequestId"])
		if err != nil {
			return nil, err
		}
		c.forge.ClosePull(s.r, s.p)
		return M{"pullRequest": s}, nil
	})
	mut("reopenPullRequest", "ReopenPullRequestPayload", "ReopenPullRequestInput", func(in M, c *gctx) (M, error) {
		s, err := needPull(c, in["pullRequestId"])
		if err != nil {
			return nil, err
		}
		c.forge.ReopenPull(s.p)
		return M{"pullRequest": s}, nil
	})
	mut("updatePullRequest", "UpdatePullRequestPayload", "UpdatePullRequestInput", func(in M, c *gctx) (M, error) {
		s, err := needPull(c, in["pullRequestId"])
		if err != nil {
			return nil, err
		}
		if t, ok := in["title"].(string); ok {
			s.p.Title = t
		}
		if b, ok := in["body"].(string); ok {
			s.p.Body = b
		}
		if b, ok := in["baseRefName"].(string); ok && c.forge.BranchExists(s.r, b) {
			s.p.Base = b
		}
		switch in["state"] {
		case "CLOSED":
			c.forge.ClosePull(s.r, s.p)
		case "OPEN":
			c.forge.ReopenPull(s.p)
		}
		return M{"pullRequest": s}, nil
	})
	mut("createRepository", "CreateRepositoryPayload", "CreateRepositoryInput", func(in M, c *gctx) (M, error) {
		body := M{"name": in["name"], "private": in["visibility"] != "PUBLIC", "has_issues": in["hasIssuesEnabled"] != false}
		if d, ok := in["description"].(string); ok {
			body["description"] = d
		}
		r, err := createRepoFrom(c.forge, body)
		if err != nil {
			return nil, &fieldErr{"UNPROCESSABLE", err.Error()}
		}
		return M{"repository": repoS{r}}, nil
	})
	mut("createRef", "CreateRefPayload", "CreateRefInput", func(in M, c *gctx) (M, error) {
		rs, err := needRepo(c, in["repositoryId"])
		if err != nil {
			return nil, err
		}
		name, _ := in["name"].(string)
		oid, _ := in["oid"].(string)
		oid = strings.ToLower(oid)
		if !strings.HasPrefix(name, "refs/") {
			return nil, &fieldErr{"UNPROCESSABLE", "Reference name must start with refs/"}
		}
		if c.forge.Objects[oid] == nil {
			return nil, &fieldErr{"NOT_FOUND", "Could not resolve to a commit with the oid " + oid + "."}
		}
		if _, exists := rs.r.Refs[name]; exists {
			return nil, &fieldErr{"UNPROCESSABLE", fmt.Sprintf("A ref named %q already exists in the repository.", name)}
		}
		c.forge.SetRef(rs.r, name, oid)
		return M{"ref": refS{rs.r, name, oid}}, nil
	})
	mut("addLabelsToLabelable", "AddLabelsToLabelablePayload", "AddLabelsToLabelableInput", func(in M, c *gctx) (M, error) {
		node := nodeByID(c, in["labelableId"])
		var target *[]*Label
		var r *Repo
		var issue *Issue
		switch s := node.(type) {
		case issueS:
			target, r, issue = &s.i.Labels, s.r, s.i
		case pullS:
			target, r = &s.p.Labels, s.r
		default:
			return nil, &fieldErr{"NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%v'.", in["labelableId"])}
		}
		labels, err := labelIDs(c, r, in["labelIds"])
		if err != nil {
			return nil, err
		}
		for _, l := range labels {
			dup := false
			for _, e := range *target {
				dup = dup || e == l
			}
			if !dup {
				*target = append(*target, l)
			}
		}
		if issue != nil {
			c.forge.TouchIssue(issue)
		}
		return M{"labelable": node}, nil
	})

	schema := defType("OBJECT", "__Schema", "A GraphQL schema.")
	schema.plain("description:String", "types:[__Type!]!", "queryType:__Type!", "mutationType:__Type", "subscriptionType:__Type", "directives:[__Directive!]!")
	typ := defType("OBJECT", "__Type", "A GraphQL type.")
	typ.plain("kind:__TypeKind!", "name:String", "description:String", "specifiedByURL:String", "isOneOf:Boolean", "interfaces:[__Type!]", "possibleTypes:[__Type!]", "ofType:__Type")
	typ.f("fields", "[__Field!]", nil, "includeDeprecated:Boolean")
	typ.f("enumValues", "[__EnumValue!]", nil, "includeDeprecated:Boolean")
	typ.f("inputFields", "[__InputValue!]", nil, "includeDeprecated:Boolean")
	fld := defType("OBJECT", "__Field", "A field.")
	fld.plain("name:String!", "description:String", "type:__Type!", "isDeprecated:Boolean!", "deprecationReason:String")
	fld.f("args", "[__InputValue!]!", nil, "includeDeprecated:Boolean")
	defType("OBJECT", "__InputValue", "An argument or input field.").plain("name:String!", "description:String", "type:__Type!", "defaultValue:String", "isDeprecated:Boolean!", "deprecationReason:String")
	defType("OBJECT", "__EnumValue", "An enum value.").plain("name:String!", "description:String", "isDeprecated:Boolean!", "deprecationReason:String")
	dir := defType("OBJECT", "__Directive", "A directive.")
	dir.plain("name:String!", "description:String", "isRepeatable:Boolean!", "locations:[String!]!")
	dir.f("args", "[__InputValue!]!", nil, "includeDeprecated:Boolean")
}

func needRepo(c *gctx, id any) (repoS, error) {
	if s, ok := nodeByID(c, id).(repoS); ok {
		return s, nil
	}
	return repoS{}, &fieldErr{"NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%v'.", id)}
}

func needIssue(c *gctx, id any) (issueS, error) {
	if s, ok := nodeByID(c, id).(issueS); ok {
		return s, nil
	}
	return issueS{}, &fieldErr{"NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%v'.", id)}
}

func needPull(c *gctx, id any) (pullS, error) {
	if s, ok := nodeByID(c, id).(pullS); ok {
		return s, nil
	}
	return pullS{}, &fieldErr{"NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%v'.", id)}
}

func labelIDs(c *gctx, r *Repo, v any) ([]*Label, error) {
	var out []*Label
	ids, _ := v.([]any)
	for _, id := range ids {
		s, ok := nodeByID(c, id).(labelS)
		if !ok || s.r != r {
			return nil, &fieldErr{"NOT_FOUND", fmt.Sprintf("Could not resolve to a node with the global id of '%v'.", id)}
		}
		out = append(out, s.l)
	}
	return out, nil
}

func nodeByID(c *gctx, id any) any {
	s, ok := id.(string)
	if !ok {
		return nil
	}
	kind, n, ok := parseGlobalID(s)
	if !ok {
		return nil
	}
	f := c.forge
	switch kind {
	case "repo":
		if r := f.RepoByID(n); r != nil {
			return repoS{r}
		}
	case "issue":
		if x, ok := f.IssuesByID[n]; ok {
			return issueS{x.Repo, x.Issue}
		}
	case "pull":
		if x, ok := f.PullsByID[n]; ok {
			return pullS{x.Repo, x.Pull}
		}
	case "comment":
		if x, ok := f.CommentsByID[n]; ok {
			return commentS{x.Repo, x.Comment}
		}
	case "user":
		if n == f.Viewer.ID {
			return userS{f.Viewer}
		}
	case "label":
		for _, r := range f.ReposOf(f.Viewer) {
			for _, l := range r.labelOrder {
				if l.ID == n {
					return labelS{r, l}
				}
			}
		}
	}
	return nil
}

func searchItems(_ any, a M, c *gctx) (any, error) {
	q, _ := a["query"].(string)
	quals := map[string]string{}
	var words []string
	for _, t := range strings.Fields(q) {
		if i := strings.IndexByte(t, ':'); i > 0 {
			quals[strings.ToLower(t[:i])] = t[i+1:]
		} else {
			words = append(words, strings.ToLower(t))
		}
	}
	match := func(x, y string) bool {
		for _, w := range words {
			if !strings.Contains(strings.ToLower(x), w) && !strings.Contains(strings.ToLower(y), w) {
				return false
			}
		}
		return true
	}
	var items []any
	issueCount, repoCount, userCount := 0, 0, 0
	named := strings.ToLower(quals["repo"])
	for _, r := range c.forge.ReposOf(c.forge.Viewer) {
		if named != "" && strings.ToLower(r.Owner.Login+"/"+r.Name) != named {
			continue
		}
		switch a["type"] {
		case "REPOSITORY":
			d := ""
			if r.Description != nil {
				d = *r.Description
			}
			if match(r.Name, d) {
				items = append(items, repoS{r})
				repoCount++
			}
		case "USER":
		default:
			is := quals["is"]
			typ := quals["type"]
			if typ == "" && (is == "pr" || is == "issue") {
				typ = is
			}
			state := quals["state"]
			if state == "" && (is == "open" || is == "closed") {
				state = is
			}
			if typ != "pr" {
				for _, i := range r.issueOrder {
					if (state == "" || i.State == state) && match(i.Title, i.Body) {
						items = append(items, issueS{r, i})
						issueCount++
					}
				}
			}
			if typ != "issue" {
				for _, p := range r.pullOrder {
					if (state == "" || p.State == state) && match(p.Title, p.Body) {
						items = append(items, pullS{r, p})
						issueCount++
					}
				}
			}
		}
	}
	if a["type"] == "USER" {
		items = append(items, userS{c.forge.Viewer})
		userCount = 1
	}
	conn, err := connection("SearchResultItemConnection", items, a, func(x any) any { return x })
	if err != nil {
		return nil, err
	}
	m := conn.(M)
	m["issueCount"], m["repositoryCount"], m["userCount"] = issueCount, repoCount, userCount
	m["discussionCount"], m["codeCount"], m["wikiCount"] = 0, 0, 0
	return m, nil
}

// ---------------------------------------------------------------- introspection

type introFn func(args M) any

func introRef(ref string) any {
	if strings.HasSuffix(ref, "!") {
		return introWrapped("NON_NULL", introRef(ref[:len(ref)-1]))
	}
	if strings.HasPrefix(ref, "[") {
		return introWrapped("LIST", introRef(ref[1:len(ref)-1]))
	}
	return introNamed(ref)
}

func introWrapped(kind string, of any) M {
	return M{"__typename": "__Type", "kind": kind, "name": nil, "description": nil, "ofType": of, "fields": nil, "interfaces": nil,
		"possibleTypes": nil, "enumValues": nil, "inputFields": nil, "specifiedByURL": nil, "isOneOf": nil}
}

func inputValue(a argDef) M {
	var def any
	if a.def != nil {
		def = string(marshal(a.def))
	}
	return M{"__typename": "__InputValue", "name": a.name, "description": nil, "type": introRef(a.typ), "defaultValue": def, "isDeprecated": false, "deprecationReason": nil}
}

func introNamed(name string) any {
	t := gTypes[name]
	if t == nil {
		return nil
	}
	m := M{"__typename": "__Type", "kind": t.kind, "name": t.name, "description": t.desc, "specifiedByURL": nil, "ofType": nil,
		"fields": nil, "interfaces": nil, "possibleTypes": nil, "enumValues": nil, "inputFields": nil, "isOneOf": nil}
	if t.kind == "OBJECT" || t.kind == "INTERFACE" {
		m["fields"] = introFn(func(M) any {
			out := []any{}
			for _, f := range t.fields {
				f := f
				out = append(out, M{"__typename": "__Field", "name": f.name, "description": "The " + f.name + " of the " + t.name + ".",
					"args": introFn(func(M) any {
						list := []any{}
						for _, a := range f.args {
							list = append(list, inputValue(a))
						}
						return list
					}),
					"type": introRef(f.typ), "isDeprecated": false, "deprecationReason": nil})
			}
			return out
		})
		m["interfaces"] = introFn(func(M) any {
			out := []any{}
			for _, i := range t.interfaces {
				out = append(out, introNamed(i))
			}
			return out
		})
	}
	if t.kind == "INTERFACE" || t.kind == "UNION" {
		m["possibleTypes"] = introFn(func(M) any {
			out := []any{}
			for _, p := range t.possible {
				out = append(out, introNamed(p))
			}
			return out
		})
	}
	if t.kind == "ENUM" {
		m["enumValues"] = introFn(func(M) any {
			out := []any{}
			for _, v := range t.enumValues {
				out = append(out, M{"__typename": "__EnumValue", "name": v, "description": nil, "isDeprecated": false, "deprecationReason": nil})
			}
			return out
		})
	}
	if t.kind == "INPUT_OBJECT" {
		m["isOneOf"] = false
		m["inputFields"] = introFn(func(M) any {
			out := []any{}
			for _, a := range t.inputFields {
				out = append(out, inputValue(a))
			}
			return out
		})
	}
	return m
}

func introSchema() M {
	return M{"__typename": "__Schema", "description": "The forge GraphQL schema.",
		"types": introFn(func(M) any {
			out := []any{}
			for _, n := range gTypeOrder {
				out = append(out, introNamed(n))
			}
			return out
		}),
		"queryType": introNamed("Query"), "mutationType": introNamed("Mutation"), "subscriptionType": nil,
		"directives": []any{
			M{"__typename": "__Directive", "name": "include", "description": "The @include directive.", "isRepeatable": false, "locations": []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}, "args": []any{inputValue(argDef{name: "if", typ: "Boolean!"})}},
			M{"__typename": "__Directive", "name": "skip", "description": "The @skip directive.", "isRepeatable": false, "locations": []any{"FIELD", "FRAGMENT_SPREAD", "INLINE_FRAGMENT"}, "args": []any{inputValue(argDef{name: "if", typ: "Boolean!"})}},
		}}
}

// ================================================================ validation and execution

func namedType(ref string) string { return strings.Trim(ref, "[]!") }

func fieldOn(typeName, name string) *fieldDef {
	switch {
	case name == "__typename":
		return &fieldDef{name: name, typ: "String!"}
	case typeName == "Query" && name == "__type":
		return &fieldDef{name: name, typ: "__Type", args: []argDef{{name: "name", typ: "String!"}}}
	case typeName == "Query" && name == "__schema":
		return &fieldDef{name: name, typ: "__Schema!"}
	}
	t := gTypes[typeName]
	if t == nil {
		return nil
	}
	return t.fieldIndex[name]
}

func validate(doc *gDocument, op *gOperation) []any {
	var errs []any
	visited := map[string]bool{}
	var walk func(typeName string, sels []*gSelection, path []any)
	walk = func(typeName string, sels []*gSelection, path []any) {
		for _, s := range sels {
			switch s.kind {
			case "Field":
				key := s.name
				if s.alias != "" {
					key = s.alias
				}
				fp := append(append([]any{}, path...), key)
				def := fieldOn(typeName, s.name)
				if def == nil {
					errs = append(errs, M{"message": fmt.Sprintf("Field '%s' doesn't exist on type '%s'", s.name, typeName), "locations": []any{s.loc}, "path": fp,
						"extensions": M{"code": "undefinedField", "typeName": typeName, "fieldName": s.name}})
					continue
				}
				for _, a := range s.args {
					found := s.name == "__typename"
					for _, d := range def.args {
						found = found || d.name == a.name
					}
					if !found {
						errs = append(errs, M{"message": fmt.Sprintf("Field '%s' doesn't accept argument '%s'", s.name, a.name), "locations": []any{s.loc}, "path": fp,
							"extensions": M{"code": "argumentNotAccepted", "name": s.name, "typeName": "Field", "argumentName": a.name}})
					}
				}
				target := gTypes[namedType(def.typ)]
				composite := target != nil && (target.kind == "OBJECT" || target.kind == "INTERFACE" || target.kind == "UNION")
				switch {
				case composite && !s.hasSelections:
					errs = append(errs, M{"message": fmt.Sprintf("Field must have selections (field '%s' returns %s but has no selections. Did you mean '%s { ... }'?)", s.name, namedType(def.typ), s.name),
						"locations": []any{s.loc}, "path": fp, "extensions": M{"code": "selectionMismatch", "nodeName": "field '" + s.name + "'", "typeName": namedType(def.typ)}})
				case !composite && s.hasSelections:
					errs = append(errs, M{"message": fmt.Sprintf("Selections can't be made on scalars (field '%s' returns %s but has selections)", s.name, namedType(def.typ)),
						"locations": []any{s.loc}, "path": fp, "extensions": M{"code": "selectionMismatch", "nodeName": "field '" + s.name + "'", "typeName": namedType(def.typ)}})
				case composite:
					walk(namedType(def.typ), s.selections, fp)
				}
			case "InlineFragment":
				cond := s.typeCondition
				if cond == "" {
					cond = typeName
				}
				if gTypes[cond] == nil {
					errs = append(errs, M{"message": fmt.Sprintf("No such type %s, so it can't be a fragment condition", cond), "locations": []any{s.loc}, "extensions": M{"code": "undefinedType", "typeName": cond}})
					continue
				}
				walk(cond, s.selections, append(append([]any{}, path...), "... on "+cond))
			default:
				frag := doc.fragments[s.name]
				if frag == nil {
					errs = append(errs, M{"message": fmt.Sprintf("Fragment %s was used, but not defined", s.name), "locations": []any{s.loc}, "extensions": M{"code": "useAndDefineFragment", "fragmentName": s.name}})
					continue
				}
				k := typeName + ":" + frag.name
				if visited[k] {
					continue
				}
				visited[k] = true
				if gTypes[frag.typeCondition] == nil {
					errs = append(errs, M{"message": fmt.Sprintf("No such type %s, so it can't be a fragment condition", frag.typeCondition), "locations": []any{s.loc}, "extensions": M{"code": "undefinedType", "typeName": frag.typeCondition}})
					continue
				}
				walk(frag.typeCondition, frag.selections, append(append([]any{}, path...), "... "+frag.name))
			}
		}
	}
	if op.operation == "subscription" {
		return []any{M{"message": "Subscriptions are not supported", "locations": []any{op.loc}}}
	}
	root, label := "Query", "query"
	if op.operation == "mutation" {
		root, label = "Mutation", "mutation"
	}
	walk(root, op.selections, []any{label})
	return errs
}

func valueOf(v *gValue, vars M) any {
	switch v.kind {
	case "Variable":
		return vars[v.value]
	case "Int":
		n, _ := strconv.ParseInt(v.value, 10, 64)
		return n
	case "Float":
		f, _ := strconv.ParseFloat(v.value, 64)
		return f
	case "String", "Enum":
		return v.value
	case "Boolean":
		return v.bval
	case "Null":
		return nil
	case "List":
		out := make([]any, 0, len(v.list))
		for _, x := range v.list {
			out = append(out, valueOf(x, vars))
		}
		return out
	case "Object":
		out := M{}
		for _, f := range v.fields {
			out[f.name] = valueOf(f.value, vars)
		}
		return out
	}
	return nil
}

func argsOf(def *fieldDef, s *gSelection, vars M) M {
	out := M{}
	for _, a := range def.args {
		if a.def != nil {
			out[a.name] = a.def
		}
	}
	for _, a := range s.args {
		if a.value.kind == "Variable" {
			if _, has := vars[a.value.value]; !has {
				continue
			}
		}
		out[a.name] = valueOf(a.value, vars)
	}
	for _, a := range def.args {
		if strings.HasPrefix(a.typ, "[") {
			if v, has := out[a.name]; has && v != nil {
				if _, isList := v.([]any); !isList {
					out[a.name] = []any{v}
				}
			}
		}
	}
	return out
}

func skipped(dirs []gDirective, vars M) bool {
	for _, d := range dirs {
		for _, a := range d.args {
			if a.name != "if" {
				continue
			}
			v := valueOf(a.value, vars)
			if d.name == "skip" && v == true {
				return true
			}
			if d.name == "include" && v == false {
				return true
			}
		}
	}
	return false
}

func typeMatches(cond, runtime string) bool {
	if cond == "" || cond == runtime {
		return true
	}
	t := gTypes[cond]
	if t == nil || (t.kind != "INTERFACE" && t.kind != "UNION") {
		return false
	}
	for _, p := range t.possible {
		if p == runtime {
			return true
		}
	}
	return false
}

type fieldGroup struct {
	key   string
	nodes []*gSelection
}

func collect(c *gctx, runtime string, sels []*gSelection, out *[]*fieldGroup, index map[string]*fieldGroup) {
	for _, s := range sels {
		if skipped(s.directives, c.vars) {
			continue
		}
		switch s.kind {
		case "Field":
			key := s.name
			if s.alias != "" {
				key = s.alias
			}
			if g := index[key]; g != nil {
				g.nodes = append(g.nodes, s)
			} else {
				g := &fieldGroup{key, []*gSelection{s}}
				index[key] = g
				*out = append(*out, g)
			}
		case "InlineFragment":
			if typeMatches(s.typeCondition, runtime) {
				collect(c, runtime, s.selections, out, index)
			}
		default:
			if frag := c.fragments[s.name]; frag != nil && typeMatches(frag.typeCondition, runtime) {
				collect(c, runtime, frag.selections, out, index)
			}
		}
	}
}

// orderedMap keeps response keys in selection order.
type orderedMap struct {
	keys   []string
	values []any
}

func (o *orderedMap) MarshalJSON() ([]byte, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range o.keys {
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(marshal(k))
		b.WriteByte(':')
		b.Write(marshal(o.values[i]))
	}
	b.WriteByte('}')
	return []byte(b.String()), nil
}

func executeSelections(c *gctx, typeName string, src any, sels []*gSelection, path []any) *orderedMap {
	var groups []*fieldGroup
	collect(c, typeName, sels, &groups, map[string]*fieldGroup{})
	out := &orderedMap{}
	for _, g := range groups {
		out.keys = append(out.keys, g.key)
		out.values = append(out.values, executeField(c, typeName, src, g.nodes, append(append([]any{}, path...), g.key)))
	}
	return out
}

func executeField(c *gctx, typeName string, src any, nodes []*gSelection, path []any) any {
	s := nodes[0]
	if s.name == "__typename" {
		return typeName
	}
	def := fieldOn(typeName, s.name)
	if def == nil {
		return nil
	}
	args := argsOf(def, s, c.vars)
	var value any
	var err error
	switch {
	case typeName == "Query" && s.name == "__type":
		n, _ := args["name"].(string)
		value = introNamed(n)
	case typeName == "Query" && s.name == "__schema":
		value = introSchema()
	case def.resolve != nil:
		value, err = def.resolve(src, args, c)
	default:
		if m, ok := src.(M); ok {
			value = m[s.name]
			if fn, ok := value.(introFn); ok {
				value = fn(args)
			}
		}
	}
	if err != nil {
		fe, ok := err.(*fieldErr)
		if !ok {
			fe = &fieldErr{"INTERNAL", err.Error()}
		}
		c.errors = append(c.errors, M{"type": fe.typ, "path": path, "locations": []any{s.loc}, "message": fe.msg})
		return nil
	}
	return complete(c, def.typ, nodes, value, path)
}

func isNil(v any) bool {
	if v == nil {
		return true
	}
	switch x := v.(type) {
	case M:
		return x == nil
	case []any:
		return x == nil
	}
	return false
}

func complete(c *gctx, ref string, nodes []*gSelection, value any, path []any) any {
	if strings.HasSuffix(ref, "!") {
		return complete(c, ref[:len(ref)-1], nodes, value, path)
	}
	if isNil(value) {
		return nil
	}
	if strings.HasPrefix(ref, "[") {
		inner := ref[1 : len(ref)-1]
		list, ok := value.([]any)
		if !ok {
			return nil
		}
		out := make([]any, len(list))
		for i, item := range list {
			out[i] = complete(c, inner, nodes, item, append(append([]any{}, path...), i))
		}
		return out
	}
	t := gTypes[ref]
	if t == nil || t.kind == "SCALAR" || t.kind == "ENUM" {
		return value
	}
	runtime := ref
	if t.kind != "OBJECT" {
		runtime = typename(value)
		if runtime == "" {
			runtime = ref
		}
	}
	var sels []*gSelection
	for _, n := range nodes {
		sels = append(sels, n.selections...)
	}
	return executeSelections(c, runtime, value, sels, path)
}

func typeText(t *gTypeRef) string {
	switch t.kind {
	case "Named":
		return t.name
	case "List":
		return "[" + typeText(t.of) + "]"
	}
	return typeText(t.of) + "!"
}

func runOne(f *Forge, origin string, request any) any {
	req, ok := request.(M)
	if !ok {
		return M{"errors": []any{M{"message": "A query attribute must be specified and must be a string."}}}
	}
	query, _ := req["query"].(string)
	if strings.TrimSpace(query) == "" {
		return M{"errors": []any{M{"message": "A query attribute must be specified and must be a string."}}}
	}
	toks, err := gLex(query)
	var doc *gDocument
	if err == nil {
		doc, err = (&gParser{toks: toks}).document()
	}
	if err != nil {
		se := err.(*gSyntaxError)
		return M{"errors": []any{M{"message": "Parse error on " + se.msg, "locations": []any{se.loc}}}}
	}
	opName, _ := req["operationName"].(string)
	var op *gOperation
	if opName != "" {
		for _, o := range doc.operations {
			if o.name == opName {
				op = o
			}
		}
	} else if len(doc.operations) == 1 {
		op = doc.operations[0]
	}
	if op == nil {
		msg := "An operation name is required"
		if len(doc.operations) == 0 {
			msg = "No operations in query document"
		} else if opName != "" {
			msg = fmt.Sprintf("No operation named %q", opName)
		}
		return M{"errors": []any{M{"message": msg}}}
	}
	errs := validate(doc, op)
	provided, _ := req["variables"].(M)
	vars := M{}
	for _, d := range op.vars {
		if v, has := provided[d.name]; has {
			vars[d.name] = v
		} else if d.defaultValue != nil {
			vars[d.name] = valueOf(d.defaultValue, M{})
		}
		if d.typ.kind == "NonNull" && vars[d.name] == nil {
			errs = append(errs, M{"message": fmt.Sprintf("Variable $%s of type %s was provided invalid value", d.name, typeText(d.typ)), "locations": []any{op.loc},
				"extensions": M{"value": nil, "problems": []any{M{"path": []any{}, "explanation": "Expected value to not be null"}}}})
		}
	}
	if len(errs) > 0 {
		return M{"errors": errs}
	}
	c := &gctx{forge: f, origin: origin, vars: vars, fragments: doc.fragments}
	root := "Query"
	if op.operation == "mutation" {
		root = "Mutation"
	}
	data := executeSelections(c, root, M{}, op.selections, []any{})
	if len(c.errors) > 0 {
		return M{"data": data, "errors": c.errors}
	}
	return M{"data": data}
}

func handleGraphQL(f *Forge, payload any, origin string) any {
	if list, ok := payload.([]any); ok {
		out := make([]any, len(list))
		for i, r := range list {
			out[i] = runOne(f, origin, r)
		}
		return out
	}
	return runOne(f, origin, payload)
}
