// The forge's state: users, repositories over a shared object store, labels, issues, pull requests,
// comments, and the git operations the APIs need on top of the objects.

package main

import (
	"encoding/base64"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type User struct {
	ID     int
	NodeID string
	Login  string
	Name   string
	Email  string
}

type Label struct {
	ID          int
	NodeID      string
	Name        string
	Color       string
	Description *string
}

type Comment struct {
	ID        int
	NodeID    string
	User      *User
	Body      string
	CreatedAt string
	UpdatedAt string
	Number    int
}

type Issue struct {
	ID          int
	NodeID      string
	Number      int
	Title       string
	Body        string
	State       string
	StateReason *string
	Labels      []*Label
	User        *User
	CreatedAt   string
	UpdatedAt   string
	ClosedAt    *string
	Comments    []*Comment
	jsonOrigin  string
	json        []byte
}

type Pull struct {
	ID             int
	NodeID         string
	Number         int
	Title          string
	Body           string
	State          string
	User           *User
	Head           string
	Base           string
	Draft          bool
	CreatedAt      string
	UpdatedAt      string
	ClosedAt       *string
	Merged         bool
	MergedAt       *string
	MergeCommitSHA *string
	FrozenHead     string
	FrozenBase     string
	Labels         []*Label
	Comments       []*Comment
}

type Repo struct {
	ID            int
	NodeID        string
	Owner         *User
	Name          string
	Description   *string
	Private       bool
	DefaultBranch string
	HasIssues     bool
	CreatedAt     string
	UpdatedAt     string
	PushedAt      *string
	Refs          map[string]string
	Labels        map[string]*Label
	labelOrder    []*Label
	Issues        map[int]*Issue
	issueOrder    []*Issue
	Pulls         map[int]*Pull
	pullOrder     []*Pull
	NextNumber    int
	Version       int
}

type ForgeError struct {
	Status int
	Msg    string
}

func (e *ForgeError) Error() string { return e.Msg }

func ferr(status int, format string, args ...any) *ForgeError {
	return &ForgeError{status, fmt.Sprintf(format, args...)}
}

func isoNow() string { return time.Now().UTC().Format("2006-01-02T15:04:05Z") }

func isoFromEpoch(sec int64) string { return time.Unix(sec, 0).UTC().Format("2006-01-02T15:04:05Z") }

func globalID(prefix, kind string, id int) string {
	return prefix + "_" + base64.RawURLEncoding.EncodeToString([]byte(kind+":"+strconv.Itoa(id)))
}

var globalIDRE = regexp.MustCompile(`^([a-z]+):(\d+)$`)

func parseGlobalID(value string) (string, int, bool) {
	under := strings.IndexByte(value, '_')
	if under <= 0 {
		return "", 0, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value[under+1:])
	if err != nil {
		return "", 0, false
	}
	m := globalIDRE.FindStringSubmatch(string(raw))
	if m == nil {
		return "", 0, false
	}
	id, _ := strconv.Atoi(m[2])
	return m[1], id, true
}

func strp(s string) *string { return &s }

type FlatEntry struct {
	Mode string
	OID  string
}

type FileChange struct {
	Path    string
	Content []byte // nil deletes
	Delete  bool
}

type DiffEntry struct {
	Path   string
	Status string // added, modified, removed
	OldOID string
	NewOID string
}

type IssueRef struct {
	Repo  *Repo
	Issue *Issue
}

type PullRef struct {
	Repo *Repo
	Pull *Pull
}

type CommentRef struct {
	Repo    *Repo
	Comment *Comment
}

type Forge struct {
	Viewer       *User
	Users        map[string]*User
	Objects      map[string]*Object
	packEntries  map[string][]byte
	commitCache  map[string]*Commit
	repos        map[string]*Repo
	reposByID    map[int]*Repo
	repoOrder    []*Repo
	nextID       int
	IssuesByID   map[int]IssueRef
	PullsByID    map[int]PullRef
	CommentsByID map[int]CommentRef
}

func NewForge(login string) *Forge {
	v := &User{ID: 1000, NodeID: globalID("U", "user", 1000), Login: login, Name: login, Email: login + "@users.noreply.forge"}
	return &Forge{
		Viewer:       v,
		Users:        map[string]*User{strings.ToLower(login): v},
		Objects:      map[string]*Object{},
		packEntries:  map[string][]byte{},
		commitCache:  map[string]*Commit{},
		repos:        map[string]*Repo{},
		reposByID:    map[int]*Repo{},
		nextID:       1,
		IssuesByID:   map[int]IssueRef{},
		PullsByID:    map[int]PullRef{},
		CommentsByID: map[int]CommentRef{},
	}
}

func (f *Forge) allocID() int {
	id := f.nextID
	f.nextID++
	return id
}

// ------------------------------------------------------------ objects

func (f *Forge) PutObject(typ string, data []byte) string {
	oid := hashObject(typ, data)
	if _, ok := f.Objects[oid]; !ok {
		f.Objects[oid] = &Object{Type: typ, Data: data}
	}
	return oid
}

func (f *Forge) PutObjectWithID(oid, typ string, data []byte) {
	if _, ok := f.Objects[oid]; !ok {
		f.Objects[oid] = &Object{Type: typ, Data: data}
	}
}

func (f *Forge) GetObject(oid string) *Object { return f.Objects[oid] }

func (f *Forge) PackEntryOf(oid string) []byte {
	if e, ok := f.packEntries[oid]; ok {
		return e
	}
	o := f.Objects[oid]
	if o == nil {
		return nil
	}
	e := packEntry(o.Type, o.Data)
	f.packEntries[oid] = e
	return e
}

func (f *Forge) CommitOf(oid string) *Commit {
	if c, ok := f.commitCache[oid]; ok {
		return c
	}
	o := f.Objects[oid]
	if o == nil || o.Type != "commit" {
		return nil
	}
	c := parseCommit(o.Data)
	f.commitCache[oid] = c
	return c
}

func (f *Forge) Peel(oid string) string {
	cur := oid
	for i := 0; i < 100; i++ {
		o := f.Objects[cur]
		if o == nil || o.Type != "tag" {
			return cur
		}
		cur = parseTag(o.Data).Object
	}
	return cur
}

// ------------------------------------------------------------ repositories

func repoKey(owner, name string) string { return strings.ToLower(owner) + "/" + strings.ToLower(name) }

func (f *Forge) GetRepo(owner, name string) *Repo { return f.repos[repoKey(owner, name)] }

func (f *Forge) RepoByID(id int) *Repo { return f.reposByID[id] }

func (f *Forge) ReposOf(u *User) []*Repo {
	var out []*Repo
	for _, r := range f.repoOrder {
		if r.Owner == u {
			out = append(out, r)
		}
	}
	return out
}

type RepoOptions struct {
	Description   *string
	Private       bool
	DefaultBranch string
	HasIssues     bool
}

func (f *Forge) CreateRepo(owner *User, name string, o RepoOptions) (*Repo, error) {
	if f.repos[repoKey(owner.Login, name)] != nil {
		return nil, ferr(422, "name already exists on this account")
	}
	id := f.allocID()
	now := isoNow()
	branch := o.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	r := &Repo{ID: id, NodeID: globalID("R", "repo", id), Owner: owner, Name: name, Description: o.Description, Private: o.Private,
		DefaultBranch: branch, HasIssues: o.HasIssues, CreatedAt: now, UpdatedAt: now, Refs: map[string]string{},
		Labels: map[string]*Label{}, Issues: map[int]*Issue{}, Pulls: map[int]*Pull{}, NextNumber: 1}
	f.repos[repoKey(owner.Login, name)] = r
	f.reposByID[id] = r
	f.repoOrder = append(f.repoOrder, r)
	return r, nil
}

func (f *Forge) DeleteRepo(r *Repo) {
	delete(f.repos, repoKey(r.Owner.Login, r.Name))
	delete(f.reposByID, r.ID)
	for i, x := range f.repoOrder {
		if x == r {
			f.repoOrder = append(f.repoOrder[:i:i], f.repoOrder[i+1:]...)
			break
		}
	}
	for _, i := range r.issueOrder {
		delete(f.IssuesByID, i.ID)
	}
	for _, p := range r.pullOrder {
		delete(f.PullsByID, p.ID)
	}
}

func (f *Forge) RenameRepo(r *Repo, name string) error {
	key := repoKey(r.Owner.Login, name)
	if key == repoKey(r.Owner.Login, r.Name) {
		r.Name = name
		return nil
	}
	if f.repos[key] != nil {
		return ferr(422, "name already exists on this account")
	}
	delete(f.repos, repoKey(r.Owner.Login, r.Name))
	r.Name = name
	f.repos[key] = r
	return nil
}

func (f *Forge) SetRef(r *Repo, ref, oid string) {
	if oid == zeroOID {
		delete(r.Refs, ref)
	} else {
		r.Refs[ref] = oid
	}
	r.Version++
	now := isoNow()
	r.PushedAt = &now
	r.UpdatedAt = now
}

func (f *Forge) HeadRef(r *Repo) string { return "refs/heads/" + r.DefaultBranch }

func sortedRefNames(r *Repo) []string {
	names := make([]string, 0, len(r.Refs))
	for n := range r.Refs {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

var shortSHA = regexp.MustCompile(`^[0-9a-f]{7,39}$`)

// Resolve a branch, full ref, tag or commit id (or an unambiguous prefix) to a commit id; "" when unknown.
func (f *Forge) Resolve(r *Repo, rev string) string {
	if rev == "" || rev == "HEAD" {
		return r.Refs[f.HeadRef(r)]
	}
	cands := []string{"refs/heads/" + rev, "refs/tags/" + rev}
	if strings.HasPrefix(rev, "refs/") {
		cands = []string{rev}
	}
	for _, name := range cands {
		if oid, ok := r.Refs[name]; ok {
			return f.Peel(oid)
		}
	}
	lower := strings.ToLower(rev)
	if oidRE.MatchString(lower) && f.Objects[lower] != nil {
		return f.Peel(lower)
	}
	if shortSHA.MatchString(lower) {
		var tips []string
		for _, oid := range r.Refs {
			tips = append(tips, f.Peel(oid))
		}
		found := ""
		for _, oid := range f.WalkCommits(tips, 100000) {
			if strings.HasPrefix(oid, lower) {
				if found != "" && found != oid {
					return ""
				}
				found = oid
			}
		}
		return found
	}
	return ""
}

func (f *Forge) BranchExists(r *Repo, b string) bool { _, ok := r.Refs["refs/heads/"+b]; return ok }

func (f *Forge) BranchTip(r *Repo, b string) string { return r.Refs["refs/heads/"+b] }

// ------------------------------------------------------------ history

// WalkCommits lists commits reachable from tips, newest committer time first.
func (f *Forge) WalkCommits(tips []string, limit int) []string {
	seen := map[string]bool{}
	type item struct {
		oid  string
		time int64
		seq  int
	}
	var queue []item
	seq := 0
	push := func(oid string) {
		if seen[oid] {
			return
		}
		c := f.CommitOf(oid)
		if c == nil {
			return
		}
		seen[oid] = true
		queue = append(queue, item{oid, c.Committer.Time, seq})
		seq++
	}
	for _, t := range tips {
		push(t)
	}
	var out []string
	for len(queue) > 0 && len(out) < limit {
		best := 0
		for i := 1; i < len(queue); i++ {
			if queue[i].time > queue[best].time {
				best = i
			}
		}
		next := queue[best]
		queue = append(queue[:best], queue[best+1:]...)
		out = append(out, next.oid)
		if c := f.CommitOf(next.oid); c != nil {
			for _, p := range c.Parents {
				push(p)
			}
		}
	}
	return out
}

func (f *Forge) IsAncestor(anc, desc string) bool {
	if anc == desc {
		return true
	}
	seen := map[string]bool{desc: true}
	stack := []string{desc}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		c := f.CommitOf(oid)
		if c == nil {
			continue
		}
		for _, p := range c.Parents {
			if p == anc {
				return true
			}
			if !seen[p] {
				seen[p] = true
				stack = append(stack, p)
			}
		}
	}
	return false
}

func (f *Forge) MergeBase(a, b string) string {
	fromA := map[string]bool{}
	stack := []string{a}
	for len(stack) > 0 {
		oid := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if fromA[oid] {
			continue
		}
		fromA[oid] = true
		if c := f.CommitOf(oid); c != nil {
			stack = append(stack, c.Parents...)
		}
	}
	for _, oid := range f.WalkCommits([]string{b}, 1000000) {
		if fromA[oid] {
			return oid
		}
	}
	return ""
}

// ------------------------------------------------------------ trees

func (f *Forge) Flatten(tree string) map[string]FlatEntry {
	out := map[string]FlatEntry{}
	if tree == "" {
		return out
	}
	var walk func(oid, prefix string)
	walk = func(oid, prefix string) {
		o := f.Objects[oid]
		if o == nil || o.Type != "tree" {
			return
		}
		entries, _ := parseTree(o.Data)
		for _, e := range entries {
			path := e.Name
			if prefix != "" {
				path = prefix + "/" + e.Name
			}
			if isTreeMode(e.Mode) {
				walk(e.OID, path)
			} else {
				out[path] = FlatEntry{e.Mode, e.OID}
			}
		}
	}
	walk(tree, "")
	return out
}

func (f *Forge) WriteTree(files map[string]FlatEntry) string {
	type dir struct {
		files map[string]FlatEntry
		dirs  map[string]*dir
	}
	mk := func() *dir { return &dir{map[string]FlatEntry{}, map[string]*dir{}} }
	root := mk()
	for path, e := range files {
		parts := strings.Split(path, "/")
		d := root
		for _, p := range parts[:len(parts)-1] {
			c := d.dirs[p]
			if c == nil {
				c = mk()
				d.dirs[p] = c
			}
			d = c
		}
		d.files[parts[len(parts)-1]] = e
	}
	var write func(d *dir) string
	write = func(d *dir) string {
		var entries []TreeEntry
		for n, e := range d.files {
			entries = append(entries, TreeEntry{e.Mode, n, e.OID})
		}
		for n, c := range d.dirs {
			entries = append(entries, TreeEntry{"40000", n, write(c)})
		}
		return f.PutObject("tree", buildTree(entries))
	}
	return write(root)
}

type PathEntry struct {
	IsTree bool
	Mode   string
	OID    string
}

func (f *Forge) LookupPath(commitOID, path string) *PathEntry {
	c := f.CommitOf(commitOID)
	if c == nil {
		return nil
	}
	oid, mode := c.Tree, "40000"
	for _, part := range strings.Split(path, "/") {
		if part == "" {
			continue
		}
		o := f.Objects[oid]
		if o == nil || o.Type != "tree" {
			return nil
		}
		entries, _ := parseTree(o.Data)
		found := false
		for _, e := range entries {
			if e.Name == part {
				oid, mode, found = e.OID, e.Mode, true
				break
			}
		}
		if !found {
			return nil
		}
	}
	return &PathEntry{IsTree: isTreeMode(mode), Mode: mode, OID: oid}
}

func (f *Forge) Signature(u *User) Signature {
	return Signature{Name: u.Name, Email: u.Email, Time: time.Now().Unix(), TZ: "+0000"}
}

func (f *Forge) CommitChanges(parent string, changes []FileChange, message string, author, committer Signature) string {
	var files map[string]FlatEntry
	var parents []string
	if parent != "" {
		files = f.Flatten(f.CommitOf(parent).Tree)
		parents = []string{parent}
	} else {
		files = map[string]FlatEntry{}
	}
	for _, ch := range changes {
		if ch.Delete {
			delete(files, ch.Path)
		} else {
			files[ch.Path] = FlatEntry{"100644", f.PutObject("blob", ch.Content)}
		}
	}
	tree := f.WriteTree(files)
	return f.PutObject("commit", buildCommit(&Commit{Tree: tree, Parents: parents, Author: author, Committer: committer, Message: message}))
}

func (f *Forge) Diff(oldTree, newTree string) []DiffEntry {
	before, after := f.Flatten(oldTree), f.Flatten(newTree)
	var out []DiffEntry
	for p, e := range after {
		old, ok := before[p]
		if !ok {
			out = append(out, DiffEntry{p, "added", "", e.OID})
		} else if old.OID != e.OID || old.Mode != e.Mode {
			out = append(out, DiffEntry{p, "modified", old.OID, e.OID})
		}
	}
	for p, e := range before {
		if _, ok := after[p]; !ok {
			out = append(out, DiffEntry{p, "removed", e.OID, ""})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func (f *Forge) MergeTrees(base, ours, theirs string) (string, bool) {
	b, o, t := f.Flatten(base), f.Flatten(ours), f.Flatten(theirs)
	paths := map[string]bool{}
	for p := range b {
		paths[p] = true
	}
	for p := range o {
		paths[p] = true
	}
	for p := range t {
		paths[p] = true
	}
	same := func(x, y FlatEntry, xok, yok bool) bool {
		if !xok || !yok {
			return xok == yok
		}
		return x == y
	}
	out := map[string]FlatEntry{}
	for p := range paths {
		be, bok := b[p]
		oe, ook := o[p]
		te, tok := t[p]
		var pick FlatEntry
		var pok bool
		switch {
		case same(oe, te, ook, tok):
			pick, pok = oe, ook
		case same(be, oe, bok, ook):
			pick, pok = te, tok
		case same(be, te, bok, tok):
			pick, pok = oe, ook
		default:
			return "", false
		}
		if pok {
			out[p] = pick
		}
	}
	return f.WriteTree(out), true
}

// ------------------------------------------------------------ issues, pulls, comments

func (f *Forge) allocNumber(r *Repo) int {
	n := r.NextNumber
	r.NextNumber++
	return n
}

func (r *Repo) LabelList() []*Label { return r.labelOrder }
func (r *Repo) IssueList() []*Issue { return r.issueOrder }
func (r *Repo) PullList() []*Pull   { return r.pullOrder }

func (f *Forge) CreateLabel(r *Repo, name, color string, desc *string) (*Label, error) {
	if r.Labels[strings.ToLower(name)] != nil {
		return nil, ferr(422, "Label already exists")
	}
	id := f.allocID()
	l := &Label{ID: id, NodeID: globalID("LA", "label", id), Name: name, Color: color, Description: desc}
	r.Labels[strings.ToLower(name)] = l
	r.labelOrder = append(r.labelOrder, l)
	return l, nil
}

func (r *Repo) RenameLabel(l *Label, name string) {
	delete(r.Labels, strings.ToLower(l.Name))
	l.Name = name
	r.Labels[strings.ToLower(name)] = l
}

func (r *Repo) DeleteLabel(l *Label) {
	delete(r.Labels, strings.ToLower(l.Name))
	for i, x := range r.labelOrder {
		if x == l {
			r.labelOrder = append(r.labelOrder[:i:i], r.labelOrder[i+1:]...)
			break
		}
	}
}

func (f *Forge) CreateIssue(r *Repo, title, body string, labels []*Label) *Issue {
	id := f.allocID()
	now := isoNow()
	i := &Issue{ID: id, NodeID: globalID("I", "issue", id), Number: f.allocNumber(r), Title: title, Body: body, State: "open",
		Labels: labels, User: f.Viewer, CreatedAt: now, UpdatedAt: now}
	r.Issues[i.Number] = i
	r.issueOrder = append(r.issueOrder, i)
	f.IssuesByID[id] = IssueRef{r, i}
	r.UpdatedAt = now
	return i
}

func (f *Forge) SetIssueState(i *Issue, state string, reason string) {
	if i.State != state {
		i.State = state
		if state == "closed" {
			now := isoNow()
			i.ClosedAt = &now
			if reason == "" {
				reason = "completed"
			}
			i.StateReason = strp(reason)
		} else {
			i.ClosedAt = nil
			if reason == "reopened" {
				i.StateReason = strp("reopened")
			} else {
				i.StateReason = nil
			}
		}
	}
	f.TouchIssue(i)
}

func (f *Forge) TouchIssue(i *Issue) {
	i.UpdatedAt = isoNow()
	i.json = nil
}

func (f *Forge) AddComment(r *Repo, number int, comments *[]*Comment, body string) *Comment {
	id := f.allocID()
	now := isoNow()
	c := &Comment{ID: id, NodeID: globalID("IC", "comment", id), User: f.Viewer, Body: body, CreatedAt: now, UpdatedAt: now, Number: number}
	*comments = append(*comments, c)
	f.CommentsByID[id] = CommentRef{r, c}
	if i := r.Issues[number]; i != nil {
		i.UpdatedAt = now
		i.json = nil
	} else if p := r.Pulls[number]; p != nil {
		p.UpdatedAt = now
	}
	return c
}

func (f *Forge) CreatePull(r *Repo, title, body, head, base string, draft bool) *Pull {
	id := f.allocID()
	now := isoNow()
	p := &Pull{ID: id, NodeID: globalID("PR", "pull", id), Number: f.allocNumber(r), Title: title, Body: body, State: "open",
		User: f.Viewer, Head: head, Base: base, Draft: draft, CreatedAt: now, UpdatedAt: now}
	r.Pulls[p.Number] = p
	r.pullOrder = append(r.pullOrder, p)
	f.PullsByID[id] = PullRef{r, p}
	r.UpdatedAt = now
	return p
}

func (f *Forge) PullHeadSHA(r *Repo, p *Pull) string {
	if p.FrozenHead != "" {
		return p.FrozenHead
	}
	return f.BranchTip(r, p.Head)
}

func (f *Forge) PullBaseSHA(r *Repo, p *Pull) string {
	if p.FrozenBase != "" {
		return p.FrozenBase
	}
	return f.BranchTip(r, p.Base)
}

func (f *Forge) PullDiff(r *Repo, p *Pull) []DiffEntry {
	head := f.PullHeadSHA(r, p)
	base := f.PullBaseSHA(r, p)
	if head == "" {
		return nil
	}
	mb := ""
	if p.Merged && p.FrozenBase != "" {
		mb = p.FrozenBase
	} else if base != "" {
		mb = f.MergeBase(base, head)
	}
	baseTree := ""
	if c := f.CommitOf(mb); mb != "" && c != nil {
		baseTree = c.Tree
	}
	headTree := ""
	if c := f.CommitOf(head); c != nil {
		headTree = c.Tree
	}
	return f.Diff(baseTree, headTree)
}

// Mergeable reports 1 clean, 0 conflicted, -1 unknown (closed).
func (f *Forge) Mergeable(r *Repo, p *Pull) int {
	if p.State != "open" {
		return -1
	}
	if _, _, ok := f.mergePlan(r, p); ok {
		return 1
	}
	return 0
}

func (f *Forge) mergePlan(r *Repo, p *Pull) (string, string, bool) {
	head, base := f.BranchTip(r, p.Head), f.BranchTip(r, p.Base)
	if head == "" || base == "" {
		return "", "", false
	}
	hc, bc := f.CommitOf(head), f.CommitOf(base)
	if hc == nil || bc == nil {
		return "", "", false
	}
	mb := f.MergeBase(base, head)
	mbTree := ""
	if c := f.CommitOf(mb); mb != "" && c != nil {
		mbTree = c.Tree
	}
	tree, ok := f.MergeTrees(mbTree, bc.Tree, hc.Tree)
	return base, tree, ok
}

func (f *Forge) MergePull(r *Repo, p *Pull, method, title, message, sha string) (string, error) {
	if p.State != "open" {
		return "", ferr(405, "Pull Request is not mergeable")
	}
	head := f.BranchTip(r, p.Head)
	if head == "" {
		return "", ferr(405, "Head branch was deleted")
	}
	if sha != "" && sha != head {
		return "", ferr(409, "Head branch was modified. Review and try the merge again.")
	}
	base, tree, ok := f.mergePlan(r, p)
	if !ok {
		return "", ferr(405, "Pull Request is not mergeable: merge conflict")
	}
	sig := f.Signature(f.Viewer)
	if title == "" {
		title = fmt.Sprintf("%s (#%d)", p.Title, p.Number)
	}
	msg := title
	if message != "" {
		msg = title + "\n\n" + message
	}
	parents := []string{base}
	if method == "merge" {
		parents = []string{base, head}
	}
	commit := f.PutObject("commit", buildCommit(&Commit{Tree: tree, Parents: parents, Author: sig, Committer: sig, Message: msg}))
	f.SetRef(r, "refs/heads/"+p.Base, commit)
	now := isoNow()
	p.FrozenHead, p.FrozenBase = head, base
	p.State, p.Merged = "closed", true
	p.MergedAt, p.ClosedAt = &now, &now
	p.UpdatedAt = now
	p.MergeCommitSHA = &commit
	return commit, nil
}

func (f *Forge) ClosePull(r *Repo, p *Pull) {
	if p.State == "closed" {
		return
	}
	p.FrozenHead, p.FrozenBase = f.BranchTip(r, p.Head), f.BranchTip(r, p.Base)
	p.State = "closed"
	now := isoNow()
	p.ClosedAt = &now
	p.UpdatedAt = now
}

func (f *Forge) ReopenPull(p *Pull) {
	if p.State == "open" || p.Merged {
		return
	}
	p.State = "open"
	p.ClosedAt = nil
	p.FrozenHead, p.FrozenBase = "", ""
	p.UpdatedAt = isoNow()
}

// LineStats counts changed lines between two texts (Myers' greedy diff on lines).
func LineStats(before, after []byte) (int, int) {
	split := func(b []byte) []string {
		if len(b) == 0 {
			return nil
		}
		lines := strings.Split(string(b), "\n")
		if lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		return lines
	}
	a, b := split(before), split(after)
	n, m := len(a), len(b)
	if n == 0 || m == 0 {
		return m, n
	}
	max := n + m
	limit := max
	if limit > 4000 {
		limit = 4000
	}
	off := max + 1
	v := make([]int, 2*max+3)
	for d := 0; d <= limit; d++ {
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				common := (n + m - d) / 2
				return m - common, n - common
			}
		}
	}
	return m, n
}
