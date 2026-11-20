// The REST v3 surface under /api/v3, in GitHub's JSON shapes.

package main

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type M = map[string]any

type ApiRequest struct {
	Method   string
	Segments []string
	Query    url.Values
	Body     any
	Origin   string
}

type ApiResponse struct {
	Status  int
	Body    any
	Headers [][2]string
	NoBody  bool
}

func ok(body any) *ApiResponse             { return &ApiResponse{Status: 200, Body: body} }
func created(body any) *ApiResponse        { return &ApiResponse{Status: 201, Body: body} }
func noContent() *ApiResponse              { return &ApiResponse{Status: 204, NoBody: true} }
func notFound(message string) *ApiResponse { return apiErr(404, message) }
func apiErr(status int, msg string) *ApiResponse {
	return &ApiResponse{Status: status, Body: M{"message": msg, "documentation_url": docsURL, "status": strconv.Itoa(status)}}
}
func apiErrs(status int, msg string, errs []any) *ApiResponse {
	return &ApiResponse{Status: status, Body: M{"message": msg, "documentation_url": docsURL, "status": strconv.Itoa(status), "errors": errs}}
}
func invalid(resource, field, code string, extra M) *ApiResponse {
	e := M{"resource": resource, "field": field, "code": code}
	for k, v := range extra {
		e[k] = v
	}
	return apiErrs(422, "Validation Failed", []any{e})
}

func bodyMap(req *ApiRequest) M {
	if m, ok := req.Body.(M); ok {
		return m
	}
	return M{}
}

func str(m M, key string) (string, bool) {
	switch v := m[key].(type) {
	case string:
		return v, true
	}
	return "", false
}

func strOr(m M, key, def string) string {
	if s, ok := str(m, key); ok {
		return s
	}
	return def
}

func apiRoot(origin string) string { return origin + "/api/v3" }

func nullable(s *string) any {
	if s == nil {
		return nil
	}
	return *s
}

// ---------------------------------------------------------------- JSON shapes

func userJSON(origin string, u *User) M {
	api := apiRoot(origin)
	return M{"login": u.Login, "id": u.ID, "node_id": u.NodeID, "avatar_url": origin + "/avatars/" + u.Login, "gravatar_id": "",
		"url": api + "/users/" + u.Login, "html_url": origin + "/" + u.Login, "repos_url": api + "/users/" + u.Login + "/repos",
		"type": "User", "user_view_type": "public", "site_admin": false}
}

func repoJSON(origin string, r *Repo) M {
	api := apiRoot(origin)
	full := r.Owner.Login + "/" + r.Name
	u := api + "/repos/" + full
	open := 0
	for _, i := range r.issueOrder {
		if i.State == "open" {
			open++
		}
	}
	for _, p := range r.pullOrder {
		if p.State == "open" {
			open++
		}
	}
	pushed := r.CreatedAt
	if r.PushedAt != nil {
		pushed = *r.PushedAt
	}
	vis := "public"
	if r.Private {
		vis = "private"
	}
	return M{"id": r.ID, "node_id": r.NodeID, "name": r.Name, "full_name": full, "private": r.Private, "owner": userJSON(origin, r.Owner),
		"html_url": origin + "/" + full, "description": nullable(r.Description), "fork": false, "url": u,
		"branches_url": u + "/branches{/branch}", "commits_url": u + "/commits{/sha}", "contents_url": u + "/contents/{+path}",
		"issues_url": u + "/issues{/number}", "labels_url": u + "/labels{/name}", "pulls_url": u + "/pulls{/number}",
		"git_refs_url": u + "/git/refs{/sha}", "created_at": r.CreatedAt, "updated_at": r.UpdatedAt, "pushed_at": pushed,
		"git_url": origin + "/" + full + ".git", "clone_url": origin + "/" + full + ".git", "svn_url": origin + "/" + full,
		"homepage": nil, "size": 0, "stargazers_count": 0, "watchers_count": 0, "language": nil, "has_issues": r.HasIssues,
		"has_projects": false, "has_downloads": true, "has_wiki": false, "has_pages": false, "has_discussions": false,
		"forks_count": 0, "mirror_url": nil, "archived": false, "disabled": false, "open_issues_count": open, "license": nil,
		"allow_forking": true, "is_template": false, "web_commit_signoff_required": false, "topics": []any{}, "visibility": vis,
		"forks": 0, "open_issues": open, "watchers": 0, "default_branch": r.DefaultBranch,
		"permissions":        M{"admin": true, "maintain": true, "push": true, "triage": true, "pull": true},
		"allow_squash_merge": true, "allow_merge_commit": true, "allow_rebase_merge": true, "allow_auto_merge": false,
		"delete_branch_on_merge": false, "empty": len(r.Refs) == 0}
}

func labelJSON(origin string, r *Repo, l *Label) M {
	return M{"id": l.ID, "node_id": l.NodeID, "url": apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name + "/labels/" + url.PathEscape(l.Name),
		"name": l.Name, "color": l.Color, "default": false, "description": nullable(l.Description)}
}

func labelsJSON(origin string, r *Repo, ls []*Label) []any {
	out := make([]any, 0, len(ls))
	for _, l := range ls {
		out = append(out, labelJSON(origin, r, l))
	}
	return out
}

func issueMap(origin string, r *Repo, i *Issue) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	u := base + "/issues/" + strconv.Itoa(i.Number)
	var closedBy any
	if i.State == "closed" {
		closedBy = userJSON(origin, i.User)
	}
	return M{"url": u, "repository_url": base, "labels_url": u + "/labels{/name}", "comments_url": u + "/comments", "events_url": u + "/events",
		"html_url": origin + "/" + r.Owner.Login + "/" + r.Name + "/issues/" + strconv.Itoa(i.Number), "id": i.ID, "node_id": i.NodeID,
		"number": i.Number, "title": i.Title, "user": userJSON(origin, i.User), "labels": labelsJSON(origin, r, i.Labels), "state": i.State,
		"locked": false, "assignee": nil, "assignees": []any{}, "milestone": nil, "comments": len(i.Comments), "created_at": i.CreatedAt,
		"updated_at": i.UpdatedAt, "closed_at": nullable(i.ClosedAt), "author_association": "OWNER", "type": nil, "active_lock_reason": nil,
		"sub_issues_summary": M{"total": 0, "completed": 0, "percent_completed": 0}, "body": i.Body, "closed_by": closedBy,
		"timeline_url": u + "/timeline", "performed_via_github_app": nil, "state_reason": nullable(i.StateReason)}
}

// issueJSON is the serialized REST view, cached until the issue changes.
func issueJSON(origin string, r *Repo, i *Issue) []byte {
	if i.json != nil && i.jsonOrigin == origin {
		return i.json
	}
	i.json = marshal(issueMap(origin, r, i))
	i.jsonOrigin = origin
	return i.json
}

func pullAsIssueJSON(origin string, r *Repo, p *Pull) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	u := base + "/issues/" + strconv.Itoa(p.Number)
	html := origin + "/" + r.Owner.Login + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number)
	return M{"url": u, "repository_url": base, "comments_url": u + "/comments", "html_url": html, "id": p.ID, "node_id": p.NodeID,
		"number": p.Number, "title": p.Title, "user": userJSON(origin, p.User), "labels": labelsJSON(origin, r, p.Labels), "state": p.State,
		"locked": false, "assignee": nil, "assignees": []any{}, "milestone": nil, "comments": len(p.Comments), "created_at": p.CreatedAt,
		"updated_at": p.UpdatedAt, "closed_at": nullable(p.ClosedAt), "author_association": "OWNER", "body": p.Body,
		"pull_request": M{"url": base + "/pulls/" + strconv.Itoa(p.Number), "html_url": html, "merged_at": nullable(p.MergedAt)}, "state_reason": nil}
}

func commentJSON(origin string, r *Repo, c *Comment) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	return M{"url": base + "/issues/comments/" + strconv.Itoa(c.ID),
		"html_url":  fmt.Sprintf("%s/%s/%s/issues/%d#issuecomment-%d", origin, r.Owner.Login, r.Name, c.Number, c.ID),
		"issue_url": base + "/issues/" + strconv.Itoa(c.Number), "id": c.ID, "node_id": c.NodeID, "user": userJSON(origin, c.User),
		"created_at": c.CreatedAt, "updated_at": c.UpdatedAt, "author_association": "OWNER", "body": c.Body, "performed_via_github_app": nil}
}

func trimMessage(m string) string { return strings.TrimSuffix(m, "\n") }

func sigJSON(s Signature) M { return M{"name": s.Name, "email": s.Email, "date": isoFromEpoch(s.Time)} }

func commitNodeID(sha string) string {
	return "C_" + base64.RawURLEncoding.EncodeToString([]byte("commit:"+sha))
}

var verification = M{"verified": false, "reason": "unsigned", "signature": nil, "payload": nil, "verified_at": nil}

func gitCommitJSON(origin string, r *Repo, sha string, c *Commit) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, M{"sha": p, "url": base + "/git/commits/" + p, "html_url": origin + "/" + r.Owner.Login + "/" + r.Name + "/commit/" + p})
	}
	return M{"sha": sha, "node_id": commitNodeID(sha), "url": base + "/git/commits/" + sha, "html_url": origin + "/" + r.Owner.Login + "/" + r.Name + "/commit/" + sha,
		"author": sigJSON(c.Author), "committer": sigJSON(c.Committer), "tree": M{"sha": c.Tree, "url": base + "/git/trees/" + c.Tree},
		"message": trimMessage(c.Message), "parents": parents, "verification": verification}
}

func commitJSON(f *Forge, origin string, r *Repo, sha string, c *Commit) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	var author, committer any
	if c.Author.Email == f.Viewer.Email {
		author = userJSON(origin, f.Viewer)
	}
	if c.Committer.Email == f.Viewer.Email {
		committer = userJSON(origin, f.Viewer)
	}
	parents := make([]any, 0, len(c.Parents))
	for _, p := range c.Parents {
		parents = append(parents, M{"sha": p, "url": base + "/commits/" + p, "html_url": origin + "/" + r.Owner.Login + "/" + r.Name + "/commit/" + p})
	}
	return M{"sha": sha, "node_id": commitNodeID(sha),
		"commit": M{"author": sigJSON(c.Author), "committer": sigJSON(c.Committer), "message": trimMessage(c.Message),
			"tree": M{"sha": c.Tree, "url": base + "/git/trees/" + c.Tree}, "url": base + "/git/commits/" + sha, "comment_count": 0, "verification": verification},
		"url": base + "/commits/" + sha, "html_url": origin + "/" + r.Owner.Login + "/" + r.Name + "/commit/" + sha,
		"comments_url": base + "/commits/" + sha + "/comments", "author": author, "committer": committer, "parents": parents}
}

func branchRef(origin string, r *Repo, branch, sha string) M {
	return M{"label": r.Owner.Login + ":" + branch, "ref": branch, "sha": sha, "user": userJSON(origin, r.Owner), "repo": repoJSON(origin, r)}
}

func fileStats(f *Forge, oldOID, newOID string) (int, int) {
	var before, after []byte
	if o := f.Objects[oldOID]; oldOID != "" && o != nil {
		before = o.Data
	}
	if o := f.Objects[newOID]; newOID != "" && o != nil {
		after = o.Data
	}
	return LineStats(before, after)
}

func orZero(s string) string {
	if s == "" {
		return zeroOID
	}
	return s
}

func pullJSON(f *Forge, origin string, r *Repo, p *Pull, full bool) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	u := base + "/pulls/" + strconv.Itoa(p.Number)
	html := origin + "/" + r.Owner.Login + "/" + r.Name + "/pull/" + strconv.Itoa(p.Number)
	head := orZero(f.PullHeadSHA(r, p))
	baseSHA := orZero(f.PullBaseSHA(r, p))
	out := M{"url": u, "id": p.ID, "node_id": p.NodeID, "html_url": html, "diff_url": html + ".diff", "patch_url": html + ".patch",
		"issue_url": base + "/issues/" + strconv.Itoa(p.Number), "commits_url": u + "/commits", "review_comments_url": u + "/comments",
		"comments_url": base + "/issues/" + strconv.Itoa(p.Number) + "/comments", "statuses_url": base + "/statuses/" + head,
		"number": p.Number, "state": p.State, "locked": false, "title": p.Title, "user": userJSON(origin, p.User), "body": p.Body,
		"labels": labelsJSON(origin, r, p.Labels), "milestone": nil, "active_lock_reason": nil, "created_at": p.CreatedAt,
		"updated_at": p.UpdatedAt, "closed_at": nullable(p.ClosedAt), "merged_at": nullable(p.MergedAt), "merge_commit_sha": nullable(p.MergeCommitSHA),
		"assignee": nil, "assignees": []any{}, "requested_reviewers": []any{}, "requested_teams": []any{}, "draft": p.Draft,
		"head": branchRef(origin, r, p.Head, head), "base": branchRef(origin, r, p.Base, baseSHA), "author_association": "OWNER",
		"auto_merge": nil, "merged": p.Merged}
	if full {
		adds, dels := 0, 0
		files := f.PullDiff(r, p)
		for _, d := range files {
			a, b := fileStats(f, d.OldOID, d.NewOID)
			adds += a
			dels += b
		}
		var mergeable any
		state := "unknown"
		switch f.Mergeable(r, p) {
		case 1:
			mergeable, state = true, "clean"
		case 0:
			mergeable, state = false, "dirty"
		}
		var mergedBy any
		if p.Merged {
			mergedBy = userJSON(origin, f.Viewer)
		}
		out["mergeable"] = mergeable
		out["rebaseable"] = mergeable
		out["mergeable_state"] = state
		out["merged_by"] = mergedBy
		out["comments"] = len(p.Comments)
		out["review_comments"] = 0
		out["maintainer_can_modify"] = false
		out["commits"] = len(pullCommits(f, r, p))
		out["additions"] = adds
		out["deletions"] = dels
		out["changed_files"] = len(files)
	}
	return out
}

func pullCommits(f *Forge, r *Repo, p *Pull) []string {
	head := f.PullHeadSHA(r, p)
	if head == "" {
		return nil
	}
	onBase := map[string]bool{}
	if base := f.PullBaseSHA(r, p); base != "" {
		for _, oid := range f.WalkCommits([]string{base}, 1000000) {
			onBase[oid] = true
		}
	}
	var out []string
	for _, oid := range f.WalkCommits([]string{head}, 10000) {
		if !onBase[oid] {
			out = append(out, oid)
		}
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

func pullFilesJSON(f *Forge, origin string, r *Repo, p *Pull) []any {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	head := orZero(f.PullHeadSHA(r, p))
	var out []any
	for _, d := range f.PullDiff(r, p) {
		a, b := fileStats(f, d.OldOID, d.NewOID)
		sha := d.NewOID
		if sha == "" {
			sha = d.OldOID
		}
		out = append(out, M{"sha": sha, "filename": d.Path, "status": d.Status, "additions": a, "deletions": b, "changes": a + b,
			"blob_url":     origin + "/" + r.Owner.Login + "/" + r.Name + "/blob/" + head + "/" + d.Path,
			"raw_url":      origin + "/" + r.Owner.Login + "/" + r.Name + "/raw/" + head + "/" + d.Path,
			"contents_url": base + "/contents/" + d.Path + "?ref=" + head})
	}
	if out == nil {
		out = []any{}
	}
	return out
}

// ---------------------------------------------------------------- pagination

func pageParams(q url.Values) (int, int) {
	perPage, err := strconv.Atoi(q.Get("per_page"))
	if err != nil || perPage < 1 {
		perPage = 30
	}
	if perPage > 100 {
		perPage = 100
	}
	page, err := strconv.Atoi(q.Get("page"))
	if err != nil || page < 1 {
		page = 1
	}
	return perPage, page
}

func pageSlice(req *ApiRequest, n int) (int, int, [][2]string) {
	perPage, page := pageParams(req.Query)
	last := (n + perPage - 1) / perPage
	if last < 1 {
		last = 1
	}
	start := (page - 1) * perPage
	if start > n {
		start = n
	}
	end := start + perPage
	if end > n {
		end = n
	}
	link := func(target int, rel string) string {
		q := url.Values{}
		for k, v := range req.Query {
			q[k] = v
		}
		q.Set("per_page", strconv.Itoa(perPage))
		q.Set("page", strconv.Itoa(target))
		var segs []string
		for _, s := range req.Segments {
			segs = append(segs, url.PathEscape(s))
		}
		return fmt.Sprintf(`<%s/%s?%s>; rel="%s"`, apiRoot(req.Origin), strings.Join(segs, "/"), q.Encode(), rel)
	}
	var links []string
	if page > 1 {
		links = append(links, link(page-1, "prev"))
	}
	if page < last {
		links = append(links, link(page+1, "next"), link(last, "last"))
	}
	if page > 1 {
		links = append(links, link(1, "first"))
	}
	var headers [][2]string
	if len(links) > 0 {
		headers = [][2]string{{"Link", strings.Join(links, ", ")}}
	}
	return start, end, headers
}

func listOf[T any](req *ApiRequest, items []T, render func(T) any) *ApiResponse {
	start, end, headers := pageSlice(req, len(items))
	out := make([]any, 0, end-start)
	for _, it := range items[start:end] {
		out = append(out, render(it))
	}
	return &ApiResponse{Status: 200, Body: out, Headers: headers}
}

// rawList joins pre-serialized items into a JSON array.
func rawList(items [][]byte) []byte {
	n := 2
	for _, it := range items {
		n += len(it) + 1
	}
	out := make([]byte, 0, n)
	out = append(out, '[')
	for i, it := range items {
		if i > 0 {
			out = append(out, ',')
		}
		out = append(out, it...)
	}
	return append(out, ']')
}

// ---------------------------------------------------------------- the router

func handleRest(f *Forge, req *ApiRequest) *ApiResponse {
	segs := req.Segments
	if len(segs) == 0 {
		if req.Method == "GET" {
			api := apiRoot(req.Origin)
			return ok(M{"current_user_url": api + "/user", "current_user_repositories_url": api + "/user/repos{?type,page,per_page,sort}",
				"repository_url": api + "/repos/{owner}/{repo}", "user_url": api + "/users/{user}",
				"issue_search_url":      api + "/search/issues?q={query}{&page,per_page,sort,order}",
				"repository_search_url": api + "/search/repositories?q={query}{&type,page,per_page,sort,order}", "rate_limit_url": api + "/rate_limit"})
		}
		return notFound("Not Found")
	}
	head, rest := segs[0], segs[1:]
	switch {
	case head == "meta" && len(rest) == 0 && req.Method == "GET":
		return ok(M{"verifiable_password_authentication": false, "installed_version": "3.17.0", "hooks": []any{}, "git": []any{}, "packages": []any{}, "domains": M{}})
	case head == "rate_limit" && req.Method == "GET":
		core := M{"limit": 5000, "used": 0, "remaining": 5000, "reset": time.Now().Unix() + 3600}
		return ok(M{"resources": M{"core": core, "graphql": core, "search": core}, "rate": core})
	case head == "zen" && req.Method == "GET":
		return ok(M{"message": "Keep it logically awesome."})
	case head == "user":
		if len(rest) == 0 && req.Method == "GET" {
			return ok(fullUserJSON(f, req.Origin, f.Viewer))
		}
		if len(rest) == 1 && rest[0] == "repos" {
			if req.Method == "GET" {
				return listRepos(f, req, f.Viewer)
			}
			if req.Method == "POST" {
				return createRepoREST(f, req)
			}
		}
		return notFound("Not Found")
	case head == "users" && len(rest) >= 1:
		u := f.Users[strings.ToLower(rest[0])]
		if u == nil {
			return notFound("Not Found")
		}
		if len(rest) == 1 && req.Method == "GET" {
			return ok(fullUserJSON(f, req.Origin, u))
		}
		if len(rest) == 2 && rest[1] == "repos" && req.Method == "GET" {
			return listRepos(f, req, u)
		}
		return notFound("Not Found")
	case head == "repos" && len(rest) >= 2:
		r := f.GetRepo(rest[0], rest[1])
		if r == nil {
			return notFound("Not Found")
		}
		return repoRoutes(f, req, r, rest[2:])
	case head == "search" && len(rest) == 1 && req.Method == "GET":
		return searchREST(f, req, rest[0])
	}
	return notFound("Not Found")
}

func fullUserJSON(f *Forge, origin string, u *User) M {
	m := userJSON(origin, u)
	public := 0
	for _, r := range f.ReposOf(u) {
		if !r.Private {
			public++
		}
	}
	extra := M{"name": u.Name, "company": nil, "blog": "", "location": nil, "email": u.Email, "hireable": nil, "bio": nil,
		"public_repos": public, "public_gists": 0, "followers": 0, "following": 0, "created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-01-01T00:00:00Z"}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func listRepos(f *Forge, req *ApiRequest, u *User) *ApiResponse {
	repos := f.ReposOf(u)
	vis := req.Query.Get("visibility")
	if vis == "" {
		vis = req.Query.Get("type")
	}
	var filtered []*Repo
	for _, r := range repos {
		if (vis == "public" && r.Private) || (vis == "private" && !r.Private) {
			continue
		}
		filtered = append(filtered, r)
	}
	sortKey := req.Query.Get("sort")
	if sortKey == "" {
		sortKey = "full_name"
	}
	dir := req.Query.Get("direction")
	if dir == "" {
		if sortKey == "full_name" {
			dir = "asc"
		} else {
			dir = "desc"
		}
	}
	key := func(r *Repo) string {
		switch sortKey {
		case "created":
			return fmt.Sprintf("%s%012d", r.CreatedAt, r.ID)
		case "updated", "pushed":
			return fmt.Sprintf("%s%012d", r.UpdatedAt, r.ID)
		}
		return strings.ToLower(r.Name)
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if dir == "desc" {
			return key(filtered[i]) > key(filtered[j])
		}
		return key(filtered[i]) < key(filtered[j])
	})
	return listOf(req, filtered, func(r *Repo) any { return repoJSON(req.Origin, r) })
}

var repoNameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

func createRepoFrom(f *Forge, body M) (*Repo, error) {
	name, _ := str(body, "name")
	if !repoNameRE.MatchString(name) || name == "." || name == ".." || strings.HasSuffix(strings.ToLower(name), ".git") {
		return nil, ferr(422, "Repository creation failed: name is invalid")
	}
	if f.GetRepo(f.Viewer.Login, name) != nil {
		return nil, ferr(422, "Repository creation failed: name already exists on this account")
	}
	var desc *string
	if d, ok := str(body, "description"); ok {
		desc = &d
	}
	priv := body["private"] == true || body["visibility"] == "private" || body["visibility"] == "internal"
	r, err := f.CreateRepo(f.Viewer, name, RepoOptions{Description: desc, Private: priv, DefaultBranch: strOr(body, "default_branch", "main"), HasIssues: body["has_issues"] != false})
	if err != nil {
		return nil, err
	}
	if body["auto_init"] == true {
		sig := f.Signature(f.Viewer)
		readme := "# " + name + "\n"
		if desc != nil && *desc != "" {
			readme += "\n" + *desc + "\n"
		}
		c := f.CommitChanges("", []FileChange{{Path: "README.md", Content: []byte(readme)}}, "Initial commit", sig, sig)
		f.SetRef(r, f.HeadRef(r), c)
	}
	return r, nil
}

func createRepoREST(f *Forge, req *ApiRequest) *ApiResponse {
	body := bodyMap(req)
	if name, ok := str(body, "name"); ok && f.GetRepo(f.Viewer.Login, name) != nil {
		return apiErrs(422, "Repository creation failed.", []any{M{"resource": "Repository", "code": "custom", "field": "name", "message": "name already exists on this account"}})
	}
	r, err := createRepoFrom(f, body)
	if err != nil {
		fe := err.(*ForgeError)
		return apiErrs(fe.Status, fe.Msg, []any{M{"resource": "Repository", "code": "custom", "field": "name", "message": fe.Msg}})
	}
	return created(repoJSON(req.Origin, r))
}

func repoRoutes(f *Forge, req *ApiRequest, r *Repo, rest []string) *ApiResponse {
	if len(rest) == 0 {
		switch req.Method {
		case "GET":
			return ok(repoJSON(req.Origin, r))
		case "PATCH":
			return patchRepo(f, req, r)
		case "DELETE":
			f.DeleteRepo(r)
			return noContent()
		}
		return notFound("Not Found")
	}
	tail := rest[1:]
	switch rest[0] {
	case "branches":
		return branchesREST(f, req, r, tail)
	case "commits":
		return commitsREST(f, req, r, tail)
	case "git":
		return gitData(f, req, r, tail)
	case "contents":
		return contents(f, req, r, strings.Join(tail, "/"))
	case "readme":
		return readme(f, req, r)
	case "tags":
		return tagsREST(f, req, r)
	case "labels":
		return labelsREST(f, req, r, tail)
	case "issues":
		return issuesREST(f, req, r, tail)
	case "pulls":
		return pullsREST(f, req, r, tail)
	case "languages":
		if req.Method == "GET" {
			return ok(M{})
		}
	case "topics":
		if req.Method == "GET" {
			return ok(M{"names": []any{}})
		}
	case "collaborators", "contributors", "forks", "hooks", "releases", "milestones":
		if req.Method == "GET" && len(tail) == 0 {
			return ok([]any{})
		}
	}
	return notFound("Not Found")
}

func patchRepo(f *Forge, req *ApiRequest, r *Repo) *ApiResponse {
	body := bodyMap(req)
	if _, has := body["name"]; has {
		name, ok := str(body, "name")
		if !ok || !repoNameRE.MatchString(name) {
			return invalid("Repository", "name", "invalid", nil)
		}
		if err := f.RenameRepo(r, name); err != nil {
			return apiErr(422, err.Error())
		}
	}
	if v, has := body["description"]; has {
		if v == nil {
			r.Description = nil
		} else {
			d := fmt.Sprint(v)
			r.Description = &d
		}
	}
	if p, ok := body["private"].(bool); ok {
		r.Private = p
	}
	if v, ok := str(body, "visibility"); ok && (v == "private" || v == "public") {
		r.Private = v == "private"
	}
	if h, ok := body["has_issues"].(bool); ok {
		r.HasIssues = h
	}
	if _, has := body["default_branch"]; has {
		b, ok := str(body, "default_branch")
		if !ok || (len(r.Refs) > 0 && !f.BranchExists(r, b)) {
			return invalid("Repository", "default_branch", "invalid", nil)
		}
		r.DefaultBranch = b
		r.Version++
	}
	r.UpdatedAt = isoNow()
	return ok(repoJSON(req.Origin, r))
}

// ---------------------------------------------------------------- branches, commits, tags

func branchesREST(f *Forge, req *ApiRequest, r *Repo, tail []string) *ApiResponse {
	if req.Method != "GET" {
		return notFound("Not Found")
	}
	base := apiRoot(req.Origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	if len(tail) == 0 {
		type br struct{ name, sha string }
		var list []br
		for _, n := range sortedRefNames(r) {
			if strings.HasPrefix(n, "refs/heads/") {
				list = append(list, br{n[11:], r.Refs[n]})
			}
		}
		return listOf(req, list, func(b br) any {
			return M{"name": b.name, "commit": M{"sha": b.sha, "url": base + "/commits/" + b.sha}, "protected": false}
		})
	}
	name := strings.Join(tail, "/")
	sha := f.BranchTip(r, name)
	if sha == "" {
		return notFound("Branch not found")
	}
	var commit any = M{"sha": sha}
	if c := f.CommitOf(sha); c != nil {
		commit = commitJSON(f, req.Origin, r, sha, c)
	}
	return ok(M{"name": name, "commit": commit, "_links": M{"self": base + "/branches/" + name, "html": req.Origin + "/" + r.Owner.Login + "/" + r.Name + "/tree/" + name},
		"protected": false, "protection": M{"enabled": false, "required_status_checks": M{"enforcement_level": "off", "contexts": []any{}, "checks": []any{}}},
		"protection_url": base + "/branches/" + name + "/protection"})
}

func commitsREST(f *Forge, req *ApiRequest, r *Repo, tail []string) *ApiResponse {
	if req.Method != "GET" {
		return notFound("Not Found")
	}
	if len(tail) > 0 {
		ref := strings.Join(tail, "/")
		sha := f.Resolve(r, ref)
		c := f.CommitOf(sha)
		if sha == "" || c == nil {
			return apiErr(422, "No commit found for SHA: "+ref)
		}
		parentTree := ""
		if len(c.Parents) > 0 {
			if pc := f.CommitOf(c.Parents[0]); pc != nil {
				parentTree = pc.Tree
			}
		}
		var files []any
		adds, dels := 0, 0
		for _, d := range f.Diff(parentTree, c.Tree) {
			a, b := fileStats(f, d.OldOID, d.NewOID)
			adds += a
			dels += b
			s := d.NewOID
			if s == "" {
				s = d.OldOID
			}
			files = append(files, M{"sha": s, "filename": d.Path, "status": d.Status, "additions": a, "deletions": b, "changes": a + b})
		}
		if files == nil {
			files = []any{}
		}
		m := commitJSON(f, req.Origin, r, sha, c)
		m["stats"] = M{"additions": adds, "deletions": dels, "total": adds + dels}
		m["files"] = files
		return ok(m)
	}
	if len(r.Refs) == 0 {
		return apiErr(409, "Git Repository is empty.")
	}
	start := req.Query.Get("sha")
	tip := f.Resolve(r, start)
	if tip == "" {
		return notFound("No commit found for SHA: " + start)
	}
	perPage, page := pageParams(req.Query)
	limit := page*perPage + 1
	if req.Query.Has("path") || req.Query.Has("since") || req.Query.Has("until") {
		limit = 1000000
	}
	list := f.WalkCommits([]string{tip}, limit)
	if p := req.Query.Get("path"); p != "" {
		var kept []string
		for _, oid := range list {
			c := f.CommitOf(oid)
			here := ""
			if e := f.LookupPath(oid, p); e != nil {
				here = e.OID
			}
			if len(c.Parents) == 0 {
				if here != "" {
					kept = append(kept, oid)
				}
				continue
			}
			changed := true
			for _, par := range c.Parents {
				prev := ""
				if e := f.LookupPath(par, p); e != nil {
					prev = e.OID
				}
				if prev == here {
					changed = false
				}
			}
			if changed {
				kept = append(kept, oid)
			}
		}
		list = kept
	}
	since, until := req.Query.Get("since"), req.Query.Get("until")
	if since != "" || until != "" {
		var kept []string
		for _, oid := range list {
			t := isoFromEpoch(f.CommitOf(oid).Committer.Time)
			if (since == "" || t >= since) && (until == "" || t <= until) {
				kept = append(kept, oid)
			}
		}
		list = kept
	}
	return listOf(req, list, func(oid string) any { return commitJSON(f, req.Origin, r, oid, f.CommitOf(oid)) })
}

func tagsREST(f *Forge, req *ApiRequest, r *Repo) *ApiResponse {
	if req.Method != "GET" {
		return notFound("Not Found")
	}
	base := apiRoot(req.Origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	type tag struct{ name, sha string }
	var list []tag
	for _, n := range sortedRefNames(r) {
		if strings.HasPrefix(n, "refs/tags/") {
			list = append(list, tag{n[10:], f.Peel(r.Refs[n])})
		}
	}
	sort.Slice(list, func(i, j int) bool { return list[i].name > list[j].name })
	return listOf(req, list, func(t tag) any {
		return M{"name": t.name, "commit": M{"sha": t.sha, "url": base + "/commits/" + t.sha}, "zipball_url": base + "/zipball/refs/tags/" + t.name,
			"tarball_url": base + "/tarball/refs/tags/" + t.name, "node_id": "REF_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("tag:%d:%s", r.ID, t.name)))}
	})
}

// ---------------------------------------------------------------- git data

func refJSON(f *Forge, origin string, r *Repo, ref, sha string) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	typ := "commit"
	if o := f.Objects[sha]; o != nil {
		typ = o.Type
	}
	return M{"ref": ref, "node_id": "REF_" + base64.RawURLEncoding.EncodeToString([]byte(fmt.Sprintf("ref:%d:%s", r.ID, ref))),
		"url": base + "/git/" + ref, "object": M{"sha": sha, "type": typ, "url": base + "/git/" + typ + "s/" + sha}}
}

func gitData(f *Forge, req *ApiRequest, r *Repo, tail []string) *ApiResponse {
	if len(tail) == 0 {
		return notFound("Not Found")
	}
	kind, rest := tail[0], tail[1:]
	if kind == "refs" || kind == "ref" || kind == "matching-refs" {
		if kind == "refs" && len(rest) == 0 && req.Method == "POST" {
			body := bodyMap(req)
			ref, _ := str(body, "ref")
			sha, _ := str(body, "sha")
			sha = strings.ToLower(sha)
			if !strings.HasPrefix(ref, "refs/") || len(strings.Split(ref, "/")) < 3 {
				return apiErrs(422, "Reference name is invalid", []any{M{"resource": "Reference", "field": "ref", "code": "invalid"}})
			}
			if f.Objects[sha] == nil {
				return apiErrs(422, "Object does not exist", []any{M{"resource": "Reference", "field": "sha", "code": "invalid"}})
			}
			if _, exists := r.Refs[ref]; exists {
				return apiErrs(422, "Reference already exists", []any{M{"resource": "Reference", "field": "ref", "code": "already_exists"}})
			}
			f.SetRef(r, ref, sha)
			return created(refJSON(f, req.Origin, r, ref, sha))
		}
		name := "refs/" + strings.Join(rest, "/")
		switch req.Method {
		case "GET":
			if sha, ok := r.Refs[name]; ok && kind != "matching-refs" {
				return ok2(refJSON(f, req.Origin, r, name, sha))
			}
			if kind == "ref" {
				return notFound("Not Found")
			}
			prefix := name
			if len(rest) == 0 {
				prefix = "refs/"
			}
			var list []any
			for _, n := range sortedRefNames(r) {
				if !strings.HasPrefix(n, prefix) {
					continue
				}
				if kind != "matching-refs" && n != prefix && !strings.HasPrefix(n, strings.TrimSuffix(prefix, "/")+"/") {
					continue
				}
				list = append(list, refJSON(f, req.Origin, r, n, r.Refs[n]))
			}
			if len(list) == 0 {
				if kind == "matching-refs" {
					return ok([]any{})
				}
				return notFound("Not Found")
			}
			return ok(list)
		case "PATCH":
			cur, exists := r.Refs[name]
			if !exists {
				return apiErr(422, "Reference does not exist")
			}
			body := bodyMap(req)
			sha, _ := str(body, "sha")
			sha = strings.ToLower(sha)
			if f.Objects[sha] == nil {
				return apiErr(422, "Object does not exist")
			}
			if body["force"] != true && !f.IsAncestor(f.Peel(cur), f.Peel(sha)) {
				return apiErr(422, "Update is not a fast forward")
			}
			f.SetRef(r, name, sha)
			return ok(refJSON(f, req.Origin, r, name, sha))
		case "DELETE":
			if _, exists := r.Refs[name]; !exists {
				return apiErr(422, "Reference does not exist")
			}
			f.SetRef(r, name, zeroOID)
			return noContent()
		}
		return notFound("Not Found")
	}
	if req.Method != "GET" || len(rest) != 1 {
		return notFound("Not Found")
	}
	sha := strings.ToLower(rest[0])
	o := f.Objects[sha]
	base := apiRoot(req.Origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	switch kind {
	case "commits":
		if o == nil || o.Type != "commit" {
			return notFound("Not Found")
		}
		return ok(gitCommitJSON(req.Origin, r, sha, f.CommitOf(sha)))
	case "blobs":
		if o == nil || o.Type != "blob" {
			return notFound("Not Found")
		}
		return ok(M{"sha": sha, "node_id": "B_" + base64.RawURLEncoding.EncodeToString([]byte("blob:"+sha)), "size": len(o.Data),
			"url": base + "/git/blobs/" + sha, "content": wrapBase64(o.Data), "encoding": "base64"})
	case "trees":
		tree := sha
		if o != nil && o.Type == "commit" {
			tree = f.CommitOf(sha).Tree
		}
		to := f.Objects[tree]
		if to == nil || to.Type != "tree" {
			return notFound("Not Found")
		}
		rec := req.Query.Has("recursive") && req.Query.Get("recursive") != "0" && req.Query.Get("recursive") != "false"
		entries := []any{}
		var walk func(oid, prefix string)
		walk = func(oid, prefix string) {
			t := f.Objects[oid]
			if t == nil {
				return
			}
			list, _ := parseTree(t.Data)
			for _, e := range list {
				p := e.Name
				if prefix != "" {
					p = prefix + "/" + e.Name
				}
				isTree := isTreeMode(e.Mode)
				item := M{"path": p, "mode": e.Mode, "type": "blob", "sha": e.OID, "url": base + "/git/blobs/" + e.OID}
				if isTree {
					item["mode"], item["type"], item["url"] = "040000", "tree", base+"/git/trees/"+e.OID
				} else {
					if e.Mode == "160000" {
						item["type"] = "commit"
					}
					if b := f.Objects[e.OID]; b != nil {
						item["size"] = len(b.Data)
					} else {
						item["size"] = 0
					}
				}
				entries = append(entries, item)
				if isTree && rec {
					walk(e.OID, p)
				}
			}
		}
		walk(tree, "")
		return ok(M{"sha": tree, "url": base + "/git/trees/" + tree, "tree": entries, "truncated": false})
	}
	return notFound("Not Found")
}

func ok2(v any) *ApiResponse { return ok(v) }

// ---------------------------------------------------------------- contents

func wrapBase64(data []byte) string {
	text := base64.StdEncoding.EncodeToString(data)
	var b strings.Builder
	for pos := 0; pos < len(text); pos += 60 {
		end := pos + 60
		if end > len(text) {
			end = len(text)
		}
		b.WriteString(text[pos:end])
		b.WriteByte('\n')
	}
	return b.String()
}

func contentEntryJSON(origin string, r *Repo, path, typ, sha string, size int, ref string) M {
	base := apiRoot(origin) + "/repos/" + r.Owner.Login + "/" + r.Name
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	self := base + "/contents/" + path + "?ref=" + url.QueryEscape(ref)
	kind := "blob"
	gitKind := "blobs"
	if typ == "dir" {
		kind, gitKind = "tree", "trees"
	}
	html := origin + "/" + r.Owner.Login + "/" + r.Name + "/" + kind + "/" + ref + "/" + path
	gitURL := base + "/git/" + gitKind + "/" + sha
	var download any
	if typ == "file" {
		download = origin + "/" + r.Owner.Login + "/" + r.Name + "/raw/" + ref + "/" + path
	}
	return M{"name": name, "path": path, "sha": sha, "size": size, "url": self, "html_url": html, "git_url": gitURL,
		"download_url": download, "type": typ, "_links": M{"self": self, "git": gitURL, "html": html}}
}

type writeInput struct {
	Path, Message, Branch, SHA string
	Content                    []byte
	Delete                     bool
	AuthorName, AuthorEmail    string
}

// writeFile commits one file change on a branch (creating the branch, or the first commit, as needed).
func writeFile(f *Forge, r *Repo, in writeInput) (int, string, string, error) {
	path := strings.Trim(in.Path, "/")
	if path == "" {
		return 0, "", "", ferr(422, "path is invalid")
	}
	for _, part := range strings.Split(path, "/") {
		if part == "" || part == "." || part == ".." || part == ".git" {
			return 0, "", "", ferr(422, "path is invalid")
		}
	}
	branch := strings.TrimPrefix(in.Branch, "refs/heads/")
	if branch == "" {
		branch = r.DefaultBranch
	}
	parent := f.BranchTip(r, branch)
	if parent == "" && len(r.Refs) > 0 {
		parent = f.BranchTip(r, r.DefaultBranch)
		if parent == "" {
			return 0, "", "", ferr(404, "Branch %s not found", branch)
		}
	}
	var existing *PathEntry
	if parent != "" {
		existing = f.LookupPath(parent, path)
	}
	if existing != nil && existing.IsTree {
		return 0, "", "", ferr(422, "%s is a directory", path)
	}
	if in.Delete {
		if existing == nil {
			return 0, "", "", ferr(404, "Not Found")
		}
		if in.SHA == "" {
			return 0, "", "", ferr(422, `"sha" wasn't supplied.`)
		}
		if strings.ToLower(in.SHA) != existing.OID {
			return 0, "", "", ferr(409, "%s does not match %s", path, in.SHA)
		}
	} else if existing != nil {
		if in.SHA == "" {
			return 0, "", "", ferr(422, "Invalid request.\n\n\"sha\" wasn't supplied.")
		}
		if strings.ToLower(in.SHA) != existing.OID {
			return 0, "", "", ferr(409, "%s does not match %s", path, in.SHA)
		}
	}
	author := f.Signature(f.Viewer)
	if in.AuthorName != "" && in.AuthorEmail != "" {
		author.Name, author.Email = in.AuthorName, in.AuthorEmail
	}
	commit := f.CommitChanges(parent, []FileChange{{Path: path, Content: in.Content, Delete: in.Delete}}, in.Message, author, f.Signature(f.Viewer))
	f.SetRef(r, "refs/heads/"+branch, commit)
	blob := ""
	if !in.Delete {
		if e := f.LookupPath(commit, path); e != nil {
			blob = e.OID
		}
	}
	status := 200
	if existing == nil {
		status = 201
	}
	return status, commit, blob, nil
}

var base64RE = regexp.MustCompile(`^[A-Za-z0-9+/=\s_-]*$`)

func decodeBase64(s string) []byte {
	clean := strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, s)
	clean = strings.NewReplacer("-", "+", "_", "/").Replace(clean)
	clean = strings.TrimRight(clean, "=")
	out, err := base64.RawStdEncoding.DecodeString(clean)
	if err != nil {
		return []byte{}
	}
	return out
}

func contents(f *Forge, req *ApiRequest, r *Repo, rawPath string) *ApiResponse {
	path := strings.Trim(rawPath, "/")
	if req.Method == "GET" {
		if len(r.Refs) == 0 {
			return notFound("This repository is empty.")
		}
		ref := req.Query.Get("ref")
		if ref == "" {
			ref = r.DefaultBranch
		}
		sha := f.Resolve(r, ref)
		if sha == "" {
			return notFound("No commit found for the ref " + ref)
		}
		var entry *PathEntry
		if path == "" {
			entry = &PathEntry{IsTree: true, Mode: "40000", OID: f.CommitOf(sha).Tree}
		} else {
			entry = f.LookupPath(sha, path)
		}
		if entry == nil {
			return notFound("Not Found")
		}
		if entry.IsTree {
			t := f.Objects[entry.OID]
			if t == nil {
				return notFound("Not Found")
			}
			list, _ := parseTree(t.Data)
			out := make([]any, 0, len(list))
			for _, e := range list {
				p := e.Name
				if path != "" {
					p = path + "/" + e.Name
				}
				typ, size := "file", 0
				if isTreeMode(e.Mode) {
					typ = "dir"
				} else {
					if e.Mode == "160000" {
						typ = "submodule"
					}
					if b := f.Objects[e.OID]; b != nil {
						size = len(b.Data)
					}
				}
				out = append(out, contentEntryJSON(req.Origin, r, p, typ, e.OID, size, ref))
			}
			return ok(out)
		}
		b := f.Objects[entry.OID]
		if b == nil {
			return notFound("Not Found")
		}
		m := contentEntryJSON(req.Origin, r, path, "file", entry.OID, len(b.Data), ref)
		m["content"] = wrapBase64(b.Data)
		m["encoding"] = "base64"
		return ok(m)
	}
	if req.Method != "PUT" && req.Method != "DELETE" {
		return notFound("Not Found")
	}
	body := bodyMap(req)
	message, _ := str(body, "message")
	if message == "" {
		return invalid("Commit", "message", "missing_field", nil)
	}
	in := writeInput{Path: path, Message: message, Branch: strOr(body, "branch", ""), SHA: strOr(body, "sha", ""), Delete: req.Method == "DELETE"}
	if req.Method == "PUT" {
		encoded, ok := str(body, "content")
		if !ok {
			return invalid("Content", "content", "missing_field", nil)
		}
		if !base64RE.MatchString(encoded) {
			return apiErr(422, "content is not valid Base64")
		}
		in.Content = decodeBase64(encoded)
	}
	if a, ok := body["author"].(M); ok {
		in.AuthorName, _ = str(a, "name")
		in.AuthorEmail, _ = str(a, "email")
	}
	status, commit, blob, err := writeFile(f, r, in)
	if err != nil {
		fe := err.(*ForgeError)
		if fe.Status == 404 {
			return notFound(fe.Msg)
		}
		return apiErr(fe.Status, fe.Msg)
	}
	branch := in.Branch
	if branch == "" {
		branch = r.DefaultBranch
	}
	var content any
	if blob != "" {
		c := contentEntryJSON(req.Origin, r, path, "file", blob, len(in.Content), branch)
		c["encoding"] = "base64"
		content = c
	}
	if req.Method == "DELETE" {
		status = 200
	}
	return &ApiResponse{Status: status, Body: M{"content": content, "commit": gitCommitJSON(req.Origin, r, commit, f.CommitOf(commit))}}
}

func readme(f *Forge, req *ApiRequest, r *Repo) *ApiResponse {
	if req.Method != "GET" {
		return notFound("Not Found")
	}
	ref := req.Query.Get("ref")
	if ref == "" {
		ref = r.DefaultBranch
	}
	sha := f.Resolve(r, ref)
	if sha == "" {
		return notFound("Not Found")
	}
	t := f.Objects[f.CommitOf(sha).Tree]
	if t == nil {
		return notFound("Not Found")
	}
	list, _ := parseTree(t.Data)
	for _, e := range list {
		if !isTreeMode(e.Mode) && (strings.EqualFold(e.Name, "readme") || strings.HasPrefix(strings.ToLower(e.Name), "readme.")) {
			return contents(f, &ApiRequest{Method: "GET", Segments: req.Segments, Query: url.Values{"ref": {ref}}, Origin: req.Origin}, r, e.Name)
		}
	}
	return notFound("Not Found")
}

// ---------------------------------------------------------------- labels

func labelsREST(f *Forge, req *ApiRequest, r *Repo, tail []string) *ApiResponse {
	if len(tail) == 0 {
		switch req.Method {
		case "GET":
			return listOf(req, r.LabelList(), func(l *Label) any { return labelJSON(req.Origin, r, l) })
		case "POST":
			body := bodyMap(req)
			name, _ := str(body, "name")
			if strings.TrimSpace(name) == "" {
				return invalid("Label", "name", "missing_field", nil)
			}
			if r.Labels[strings.ToLower(name)] != nil {
				return invalid("Label", "name", "already_exists", nil)
			}
			var desc *string
			if d, ok := str(body, "description"); ok {
				desc = &d
			}
			l, _ := f.CreateLabel(r, name, strings.TrimPrefix(strOr(body, "color", "ededed"), "#"), desc)
			return created(labelJSON(req.Origin, r, l))
		}
		return notFound("Not Found")
	}
	l := r.Labels[strings.ToLower(strings.Join(tail, "/"))]
	if l == nil {
		return notFound("Not Found")
	}
	switch req.Method {
	case "GET":
		return ok(labelJSON(req.Origin, r, l))
	case "PATCH":
		body := bodyMap(req)
		name := strOr(body, "new_name", strOr(body, "name", ""))
		if name != "" && name != l.Name {
			r.RenameLabel(l, name)
		}
		if c, ok := str(body, "color"); ok {
			l.Color = strings.TrimPrefix(c, "#")
		}
		if _, has := body["description"]; has {
			if d, ok := str(body, "description"); ok {
				l.Description = &d
			} else {
				l.Description = nil
			}
		}
		for _, i := range r.issueOrder {
			i.json = nil
		}
		return ok(labelJSON(req.Origin, r, l))
	case "DELETE":
		r.DeleteLabel(l)
		for _, i := range r.issueOrder {
			kept := i.Labels[:0:0]
			for _, x := range i.Labels {
				if x != l {
					kept = append(kept, x)
				}
			}
			if len(kept) != len(i.Labels) {
				i.Labels = kept
				i.json = nil
			}
		}
		return noContent()
	}
	return notFound("Not Found")
}

func resolveLabels(r *Repo, v any) ([]*Label, []string) {
	var out []*Label
	var unknown []string
	list, ok := v.([]any)
	if !ok {
		return out, unknown
	}
	for _, it := range list {
		name := ""
		switch x := it.(type) {
		case string:
			name = x
		case M:
			name, _ = str(x, "name")
		}
		if name == "" {
			continue
		}
		l := r.Labels[strings.ToLower(name)]
		if l == nil {
			unknown = append(unknown, name)
			continue
		}
		dup := false
		for _, e := range out {
			if e == l {
				dup = true
			}
		}
		if !dup {
			out = append(out, l)
		}
	}
	return out, unknown
}

// ---------------------------------------------------------------- issues

func sortIssues(list []*Issue, sortKey, direction string) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		var less, eq bool
		switch sortKey {
		case "updated":
			less, eq = a.UpdatedAt < b.UpdatedAt, a.UpdatedAt == b.UpdatedAt
		case "comments":
			less, eq = len(a.Comments) < len(b.Comments), len(a.Comments) == len(b.Comments)
		default:
			eq = true
		}
		if eq {
			less = a.Number < b.Number
		}
		if direction == "asc" {
			return less
		}
		return !less && !(eq && a.Number == b.Number)
	})
}

func filterIssues(r *Repo, state string, labels []string) []*Issue {
	var wanted []string
	for _, l := range labels {
		if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
			wanted = append(wanted, l)
		}
	}
	out := []*Issue{}
	for _, i := range r.issueOrder {
		if state != "all" && i.State != state {
			continue
		}
		okAll := true
		for _, w := range wanted {
			found := false
			for _, l := range i.Labels {
				if strings.ToLower(l.Name) == w {
					found = true
				}
			}
			if !found {
				okAll = false
			}
		}
		if okAll {
			out = append(out, i)
		}
	}
	return out
}

func issueTitle(body M) (string, bool) {
	switch v := body["title"].(type) {
	case string:
		return v, true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	}
	return "", false
}

func issuesREST(f *Forge, req *ApiRequest, r *Repo, tail []string) *ApiResponse {
	if len(tail) == 0 {
		switch req.Method {
		case "GET":
			state := req.Query.Get("state")
			if state == "" {
				state = "open"
			}
			var labels []string
			if l := req.Query.Get("labels"); l != "" {
				labels = strings.Split(l, ",")
			}
			list := filterIssues(r, state, labels)
			if since := req.Query.Get("since"); since != "" {
				var kept []*Issue
				for _, i := range list {
					if i.UpdatedAt >= since {
						kept = append(kept, i)
					}
				}
				list = kept
			}
			sortKey := req.Query.Get("sort")
			if sortKey == "" {
				sortKey = "created"
			}
			dir := req.Query.Get("direction")
			if dir == "" {
				dir = "desc"
			}
			sortIssues(list, sortKey, dir)
			start, end, headers := pageSlice(req, len(list))
			items := make([][]byte, 0, end-start)
			for _, i := range list[start:end] {
				items = append(items, issueJSON(req.Origin, r, i))
			}
			return &ApiResponse{Status: 200, Body: rawList(items), Headers: headers}
		case "POST":
			if !r.HasIssues {
				return apiErr(410, "Issues are disabled for this repo")
			}
			body := bodyMap(req)
			title, ok := issueTitle(body)
			if !ok || strings.TrimSpace(title) == "" {
				return invalid("Issue", "title", "missing_field", nil)
			}
			labels, unknown := resolveLabels(r, body["labels"])
			if len(unknown) > 0 {
				return invalid("Label", "labels", "invalid", M{"value": unknown[0], "message": "Label " + unknown[0] + " does not exist"})
			}
			i := f.CreateIssue(r, title, strOr(body, "body", ""), labels)
			return &ApiResponse{Status: 201, Body: issueJSON(req.Origin, r, i)}
		}
		return notFound("Not Found")
	}
	if tail[0] == "comments" {
		if len(tail) == 1 && req.Method == "GET" {
			var all []*Comment
			for _, i := range r.issueOrder {
				all = append(all, i.Comments...)
			}
			for _, p := range r.pullOrder {
				all = append(all, p.Comments...)
			}
			sort.Slice(all, func(a, b int) bool { return all[a].ID < all[b].ID })
			return listOf(req, all, func(c *Comment) any { return commentJSON(req.Origin, r, c) })
		}
		if len(tail) != 2 {
			return notFound("Not Found")
		}
		id, _ := strconv.Atoi(tail[1])
		ref, found := f.CommentsByID[id]
		if !found || ref.Repo != r {
			return notFound("Not Found")
		}
		c := ref.Comment
		switch req.Method {
		case "GET":
			return ok(commentJSON(req.Origin, r, c))
		case "PATCH":
			text, ok := str(bodyMap(req), "body")
			if !ok {
				return invalid("IssueComment", "body", "missing_field", nil)
			}
			c.Body = text
			c.UpdatedAt = isoNow()
			return ok2(commentJSON(req.Origin, r, c))
		case "DELETE":
			remove := func(list []*Comment) []*Comment {
				out := list[:0:0]
				for _, x := range list {
					if x != c {
						out = append(out, x)
					}
				}
				return out
			}
			if i := r.Issues[c.Number]; i != nil {
				i.Comments = remove(i.Comments)
				i.json = nil
			} else if p := r.Pulls[c.Number]; p != nil {
				p.Comments = remove(p.Comments)
			}
			delete(f.CommentsByID, c.ID)
			return noContent()
		}
		return notFound("Not Found")
	}
	number, err := strconv.Atoi(tail[0])
	if err != nil {
		return notFound("Not Found")
	}
	issue := r.Issues[number]
	var pull *Pull
	if issue == nil {
		pull = r.Pulls[number]
	}
	if issue == nil && pull == nil {
		return notFound("Not Found")
	}
	if len(tail) == 1 {
		switch req.Method {
		case "GET":
			if issue != nil {
				return &ApiResponse{Status: 200, Body: issueJSON(req.Origin, r, issue)}
			}
			return ok(pullAsIssueJSON(req.Origin, r, pull))
		case "PATCH":
			body := bodyMap(req)
			if _, has := body["title"]; has {
				t, ok := str(body, "title")
				if !ok || strings.TrimSpace(t) == "" {
					return invalid("Issue", "title", "invalid", nil)
				}
				if issue != nil {
					issue.Title = t
				} else {
					pull.Title = t
				}
			}
			if _, has := body["body"]; has {
				b := strOr(body, "body", "")
				if issue != nil {
					issue.Body = b
				} else {
					pull.Body = b
				}
			}
			if _, has := body["labels"]; has {
				labels, unknown := resolveLabels(r, body["labels"])
				if len(unknown) > 0 {
					return invalid("Label", "labels", "invalid", M{"value": unknown[0]})
				}
				if issue != nil {
					issue.Labels = labels
				} else {
					pull.Labels = labels
				}
			}
			if _, has := body["state"]; has {
				state, _ := str(body, "state")
				if state != "open" && state != "closed" {
					return invalid("Issue", "state", "invalid", nil)
				}
				if issue != nil {
					f.SetIssueState(issue, state, strOr(body, "state_reason", ""))
				} else if state == "closed" {
					f.ClosePull(r, pull)
				} else {
					f.ReopenPull(pull)
				}
			}
			if issue != nil {
				f.TouchIssue(issue)
				return &ApiResponse{Status: 200, Body: issueJSON(req.Origin, r, issue)}
			}
			return ok(pullAsIssueJSON(req.Origin, r, pull))
		}
		return notFound("Not Found")
	}
	comments := func() *[]*Comment {
		if issue != nil {
			return &issue.Comments
		}
		return &pull.Comments
	}
	labelsOf := func() *[]*Label {
		if issue != nil {
			return &issue.Labels
		}
		return &pull.Labels
	}
	if tail[1] == "comments" && len(tail) == 2 {
		switch req.Method {
		case "GET":
			return listOf(req, *comments(), func(c *Comment) any { return commentJSON(req.Origin, r, c) })
		case "POST":
			text, _ := str(bodyMap(req), "body")
			if text == "" {
				return invalid("IssueComment", "body", "missing_field", nil)
			}
			return created(commentJSON(req.Origin, r, f.AddComment(r, number, comments(), text)))
		}
		return notFound("Not Found")
	}
	if tail[1] == "labels" && len(tail) == 2 {
		target := labelsOf()
		switch req.Method {
		case "GET":
			return ok(labelsJSON(req.Origin, r, *target))
		case "POST", "PUT":
			var names any = req.Body
			if m, ok := req.Body.(M); ok {
				names = m["labels"]
			}
			labels, unknown := resolveLabels(r, names)
			if len(unknown) > 0 {
				return invalid("Label", "labels", "invalid", M{"value": unknown[0]})
			}
			if req.Method == "PUT" {
				*target = labels
			} else {
				for _, l := range labels {
					dup := false
					for _, e := range *target {
						if e == l {
							dup = true
						}
					}
					if !dup {
						*target = append(*target, l)
					}
				}
			}
			if issue != nil {
				f.TouchIssue(issue)
			}
			return ok(labelsJSON(req.Origin, r, *target))
		case "DELETE":
			*target = nil
			if issue != nil {
				f.TouchIssue(issue)
			}
			return noContent()
		}
	}
	return notFound("Not Found")
}

// ---------------------------------------------------------------- pull requests

func openPull(f *Forge, r *Repo, title, body, head, base string, draft bool) (*Pull, error) {
	if i := strings.IndexByte(head, ':'); i >= 0 {
		head = head[i+1:]
	}
	head = strings.TrimPrefix(head, "refs/heads/")
	base = strings.TrimPrefix(base, "refs/heads/")
	if strings.TrimSpace(title) == "" {
		return nil, ferr(422, "Validation Failed: title is missing")
	}
	if !f.BranchExists(r, base) {
		return nil, ferr(422, "Validation Failed: base %s does not exist", base)
	}
	if !f.BranchExists(r, head) {
		return nil, ferr(422, "Validation Failed: head %s does not exist", head)
	}
	if head == base {
		return nil, ferr(422, "Validation Failed: head and base are both %s", base)
	}
	for _, p := range r.pullOrder {
		if p.State == "open" && p.Head == head && p.Base == base {
			return nil, ferr(422, "A pull request already exists for %s:%s.", r.Owner.Login, head)
		}
	}
	return f.CreatePull(r, title, body, head, base, draft), nil
}

func pullsREST(f *Forge, req *ApiRequest, r *Repo, tail []string) *ApiResponse {
	if len(tail) == 0 {
		switch req.Method {
		case "GET":
			state := req.Query.Get("state")
			if state == "" {
				state = "open"
			}
			headF, baseF := req.Query.Get("head"), req.Query.Get("base")
			if i := strings.IndexByte(headF, ':'); i >= 0 {
				headF = headF[i+1:]
			}
			var list []*Pull
			for _, p := range r.pullOrder {
				if (state == "all" || p.State == state) && (headF == "" || p.Head == headF) && (baseF == "" || p.Base == baseF) {
					list = append(list, p)
				}
			}
			upd := req.Query.Get("sort") == "updated"
			asc := req.Query.Get("direction") == "asc"
			sort.SliceStable(list, func(i, j int) bool {
				a, b := list[i], list[j]
				less := a.Number < b.Number
				if upd && a.UpdatedAt != b.UpdatedAt {
					less = a.UpdatedAt < b.UpdatedAt
				}
				if asc {
					return less
				}
				return !less
			})
			return listOf(req, list, func(p *Pull) any { return pullJSON(f, req.Origin, r, p, false) })
		case "POST":
			body := bodyMap(req)
			title, ok1 := str(body, "title")
			head, ok2 := str(body, "head")
			base, ok3 := str(body, "base")
			if !ok1 {
				return invalid("PullRequest", "title", "missing_field", nil)
			}
			if !ok2 {
				return invalid("PullRequest", "head", "missing_field", nil)
			}
			if !ok3 {
				return invalid("PullRequest", "base", "missing_field", nil)
			}
			p, err := openPull(f, r, title, strOr(body, "body", ""), head, base, body["draft"] == true)
			if err != nil {
				fe := err.(*ForgeError)
				return apiErrs(fe.Status, fe.Msg, []any{M{"resource": "PullRequest", "code": "invalid", "message": fe.Msg}})
			}
			return created(pullJSON(f, req.Origin, r, p, true))
		}
		return notFound("Not Found")
	}
	number, err := strconv.Atoi(tail[0])
	p := r.Pulls[number]
	if err != nil || p == nil {
		return notFound("Not Found")
	}
	if len(tail) == 1 {
		switch req.Method {
		case "GET":
			return ok(pullJSON(f, req.Origin, r, p, true))
		case "PATCH":
			body := bodyMap(req)
			if t, ok := str(body, "title"); ok {
				p.Title = t
			}
			if _, has := body["body"]; has {
				p.Body = strOr(body, "body", "")
			}
			if b, ok := str(body, "base"); ok {
				if !f.BranchExists(r, b) {
					return invalid("PullRequest", "base", "invalid", nil)
				}
				p.Base = b
			}
			switch body["state"] {
			case "closed":
				f.ClosePull(r, p)
			case "open":
				f.ReopenPull(p)
			}
			p.UpdatedAt = isoNow()
			return ok(pullJSON(f, req.Origin, r, p, true))
		}
		return notFound("Not Found")
	}
	switch {
	case tail[1] == "files" && req.Method == "GET":
		files := pullFilesJSON(f, req.Origin, r, p)
		return listOf(req, files, func(x any) any { return x })
	case tail[1] == "commits" && req.Method == "GET":
		return listOf(req, pullCommits(f, r, p), func(oid string) any { return commitJSON(f, req.Origin, r, oid, f.CommitOf(oid)) })
	case tail[1] == "merge":
		if req.Method == "GET" {
			if p.Merged {
				return noContent()
			}
			return notFound("Not Found")
		}
		if req.Method == "PUT" {
			body := bodyMap(req)
			sha, err := f.MergePull(r, p, strOr(body, "merge_method", "merge"), strOr(body, "commit_title", ""), strOr(body, "commit_message", ""), strOr(body, "sha", ""))
			if err != nil {
				fe := err.(*ForgeError)
				return apiErr(fe.Status, fe.Msg)
			}
			return ok(M{"sha": sha, "merged": true, "message": "Pull Request successfully merged"})
		}
	case (tail[1] == "comments" || tail[1] == "reviews") && req.Method == "GET":
		return ok([]any{})
	case tail[1] == "requested_reviewers" && req.Method == "GET":
		return ok(M{"users": []any{}, "teams": []any{}})
	}
	return notFound("Not Found")
}

// ---------------------------------------------------------------- search

func searchREST(f *Forge, req *ApiRequest, kind string) *ApiResponse {
	quals := map[string]string{}
	var words []string
	for _, term := range strings.Fields(req.Query.Get("q")) {
		if c := strings.IndexByte(term, ':'); c > 0 {
			quals[strings.ToLower(term[:c])] = term[c+1:]
		} else {
			words = append(words, strings.ToLower(term))
		}
	}
	var repos []*Repo
	for _, r := range f.ReposOf(f.Viewer) {
		if n, has := quals["repo"]; has && !strings.EqualFold(r.Owner.Login+"/"+r.Name, n) {
			continue
		}
		repos = append(repos, r)
	}
	match := func(a, b string) bool {
		for _, w := range words {
			if !strings.Contains(strings.ToLower(a), w) && !strings.Contains(strings.ToLower(b), w) {
				return false
			}
		}
		return true
	}
	switch kind {
	case "repositories":
		var hits []any
		for _, r := range repos {
			d := ""
			if r.Description != nil {
				d = *r.Description
			}
			if match(r.Name, d) {
				hits = append(hits, repoJSON(req.Origin, r))
			}
		}
		start, end, _ := pageSlice(req, len(hits))
		return ok(M{"total_count": len(hits), "incomplete_results": false, "items": append([]any{}, hits[start:end]...)})
	case "issues":
		isPR := quals["is"] == "pr" || quals["type"] == "pr"
		state := quals["state"]
		if state == "" && (quals["is"] == "open" || quals["is"] == "closed") {
			state = quals["is"]
		}
		var hits []any
		for _, r := range repos {
			if !isPR {
				for _, i := range r.issueOrder {
					if (state == "" || i.State == state) && match(i.Title, i.Body) {
						hits = append(hits, issueMap(req.Origin, r, i))
					}
				}
			}
			if isPR || (quals["type"] == "" && quals["is"] != "issue") {
				for _, p := range r.pullOrder {
					if (state == "" || p.State == state) && match(p.Title, p.Body) {
						hits = append(hits, pullAsIssueJSON(req.Origin, r, p))
					}
				}
			}
		}
		start, end, _ := pageSlice(req, len(hits))
		return ok(M{"total_count": len(hits), "incomplete_results": false, "items": append([]any{}, hits[start:end]...)})
	}
	return notFound("Not Found")
}
