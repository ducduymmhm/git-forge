// The MCP endpoint at /mcp: JSON-RPC 2.0 over Streamable HTTP, revision 2026-07-28 (server/discover)
// and the initialize handshake revisions, with github-mcp-server style tools over the shared store.

package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

var mcpSupported = []string{"2026-07-28", "2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}
var mcpServerInfo = M{"name": "git-forge", "title": "git-forge MCP server", "version": "2.0.0"}

const mcpInstructions = "A GitHub-compatible forge. Tools take owner/repo, mirror github-mcp-server, and read and write the same repositories, issues and pull requests as the REST and GraphQL APIs."

type toolInputError struct{ msg string }

func (e *toolInputError) Error() string { return e.msg }

type toolProp struct {
	name   string
	schema M
}

type tool struct {
	name, title, description string
	props                    []toolProp
	required                 []string
	readOnly                 bool
	run                      func(f *Forge, args M, origin string) (any, error)
}

func argText(args M, name string, required bool) (string, error) {
	switch v := args[name].(type) {
	case string:
		if v != "" {
			return v, nil
		}
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), nil
	}
	if required {
		return "", &toolInputError{"missing required parameter: " + name}
	}
	return "", nil
}

func argOpt(args M, name string) (string, bool) {
	switch v := args[name].(type) {
	case string:
		return v, true
	case int64:
		return strconv.FormatInt(v, 10), true
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64), true
	}
	return "", false
}

func argInt(args M, name string, required bool) (int, error) {
	switch v := args[name].(type) {
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
			return n, nil
		}
	}
	if required {
		return 0, &toolInputError{"missing required parameter: " + name + " (an integer)"}
	}
	return 0, nil
}

func repoArg(f *Forge, args M) (*Repo, error) {
	owner, _ := argOpt(args, "owner")
	name, _ := argOpt(args, "repo")
	if full, ok := argOpt(args, "full_name"); ok && (owner == "" || name == "") && strings.Contains(full, "/") {
		parts := strings.SplitN(full, "/", 2)
		owner, name = parts[0], parts[1]
	}
	if owner == "" {
		return nil, &toolInputError{"missing required parameter: owner"}
	}
	if name == "" {
		return nil, &toolInputError{"missing required parameter: repo"}
	}
	r := f.GetRepo(owner, name)
	if r == nil {
		return nil, ferr(404, "repository %s/%s not found", owner, name)
	}
	return r, nil
}

func pageArgs(args M) (int, int) {
	per, _ := argInt(args, "perPage", false)
	if per == 0 {
		per, _ = argInt(args, "per_page", false)
	}
	if per < 1 {
		per = 30
	}
	if per > 100 {
		per = 100
	}
	page, _ := argInt(args, "page", false)
	if page < 1 {
		page = 1
	}
	return per, page
}

func pageOf[T any](list []T, args M) []T {
	per, page := pageArgs(args)
	start := (page - 1) * per
	if start > len(list) {
		start = len(list)
	}
	end := start + per
	if end > len(list) {
		end = len(list)
	}
	return list[start:end]
}

func decodeJSON(b []byte) M {
	var m M
	json.Unmarshal(b, &m)
	return m
}

var ownerRepoProps = []toolProp{
	{"owner", M{"type": "string", "description": "Repository owner (the user login)"}},
	{"repo", M{"type": "string", "description": "Repository name"}},
}
var pagingProps = []toolProp{
	{"page", M{"type": "number", "description": "Page number for pagination (min 1)", "minimum": 1}},
	{"perPage", M{"type": "number", "description": "Results per page for pagination (min 1, max 100)", "minimum": 1, "maximum": 100}},
}

func props(groups ...[]toolProp) []toolProp {
	var out []toolProp
	for _, g := range groups {
		out = append(out, g...)
	}
	return out
}

func p(name, typ, desc string) []toolProp {
	return []toolProp{{name, M{"type": typ, "description": desc}}}
}

func pEnum(name, desc string, values ...string) []toolProp {
	vals := make([]any, len(values))
	for i, v := range values {
		vals[i] = v
	}
	return []toolProp{{name, M{"type": "string", "enum": vals, "description": desc}}}
}

func pList(name, desc string) []toolProp {
	return []toolProp{{name, M{"type": "array", "items": M{"type": "string"}, "description": desc}}}
}

func issueOf(r *Repo, n int) (*Issue, error) {
	if i := r.Issues[n]; i != nil {
		return i, nil
	}
	return nil, ferr(404, "issue #%d not found in %s/%s", n, r.Owner.Login, r.Name)
}

func pullOf(r *Repo, n int) (*Pull, error) {
	if pl := r.Pulls[n]; pl != nil {
		return pl, nil
	}
	return nil, ferr(404, "pull request #%d not found in %s/%s", n, r.Owner.Login, r.Name)
}

func stringList(v any) []string {
	var out []string
	if list, ok := v.([]any); ok {
		for _, x := range list {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
	}
	return out
}

var tools = []tool{
	{name: "get_me", title: "Get my user profile", description: "Get details of the authenticated user: login, id and profile URLs.", readOnly: true,
		run: func(f *Forge, _ M, origin string) (any, error) {
			return M{"login": f.Viewer.Login, "id": f.Viewer.ID, "node_id": f.Viewer.NodeID, "name": f.Viewer.Name, "html_url": origin + "/" + f.Viewer.Login, "type": "User"}, nil
		}},
	{name: "create_repo", title: "Create repository", description: "Create a new repository owned by the authenticated user.",
		props:    props(p("name", "string", "Repository name"), p("description", "string", "Repository description"), p("private", "boolean", "Whether the repository is private"), p("autoInit", "boolean", "Initialize the repository with a README")),
		required: []string{"name"},
		run: func(f *Forge, args M, origin string) (any, error) {
			name, err := argText(args, "name", true)
			if err != nil {
				return nil, err
			}
			body := M{"name": name, "private": args["private"] == true, "auto_init": args["autoInit"] == true}
			if d, ok := argOpt(args, "description"); ok {
				body["description"] = d
			}
			r, err := createRepoFrom(f, body)
			if err != nil {
				return nil, err
			}
			return repoJSON(origin, r), nil
		}},
	{name: "get_repo", title: "Get repository", description: "Get a repository by owner and name: its id, full name, visibility, default branch and description.",
		props: ownerRepoProps, required: []string{"owner", "repo"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			return repoJSON(origin, r), nil
		}},
	{name: "list_repositories", title: "List repositories", description: "List the repositories the authenticated user owns.",
		props: pagingProps, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			repos := f.ReposOf(f.Viewer)
			items := []any{}
			for _, r := range pageOf(repos, args) {
				items = append(items, repoJSON(origin, r))
			}
			return M{"total_count": len(repos), "items": items}, nil
		}},
	{name: "create_issue", title: "Create issue", description: "Create a new issue in a repository.",
		props:    props(ownerRepoProps, p("title", "string", "Issue title"), p("body", "string", "Issue body content (Markdown)"), pList("labels", "Labels to apply; each must already exist")),
		required: []string{"owner", "repo", "title"},
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			title, err := argText(args, "title", true)
			if err != nil {
				return nil, err
			}
			labels, unknown := resolveLabels(r, args["labels"])
			if len(unknown) > 0 {
				return nil, ferr(422, "label %s does not exist", unknown[0])
			}
			body, _ := argOpt(args, "body")
			return decodeJSON(issueJSON(origin, r, f.CreateIssue(r, title, body, labels))), nil
		}},
	{name: "get_issue", title: "Get issue", description: "Get one issue of a repository by its number.",
		props: props(ownerRepoProps, p("issue_number", "number", "The issue number")), required: []string{"owner", "repo", "issue_number"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "issue_number", true)
			if err != nil {
				return nil, err
			}
			i, err := issueOf(r, n)
			if err != nil {
				return nil, err
			}
			return decodeJSON(issueJSON(origin, r, i)), nil
		}},
	{name: "list_issues", title: "List issues", description: "List the issues of a repository, filtered by state and labels and ordered by creation.",
		props:    props(ownerRepoProps, pEnum("state", "Filter by state (default open)", "open", "closed", "all", "OPEN", "CLOSED"), pList("labels", "Only issues carrying all these labels"), pEnum("direction", "Sort direction (default desc)", "asc", "desc"), pagingProps),
		required: []string{"owner", "repo"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			state, _ := argOpt(args, "state")
			state = strings.ToLower(state)
			if state != "all" && state != "closed" {
				state = "open"
			}
			dir, _ := argOpt(args, "direction")
			if dir == "" {
				dir = "desc"
			}
			list := filterIssues(r, state, stringList(args["labels"]))
			sortIssues(list, "created", dir)
			issues := []any{}
			for _, i := range pageOf(list, args) {
				issues = append(issues, decodeJSON(issueJSON(origin, r, i)))
			}
			return M{"total_count": len(list), "issues": issues}, nil
		}},
	{name: "update_issue", title: "Update issue", description: "Update an issue: its title, body, state or labels.",
		props:    props(ownerRepoProps, p("issue_number", "number", "The issue number"), p("title", "string", "New title"), p("body", "string", "New body"), pEnum("state", "New state", "open", "closed"), pList("labels", "Replace the labels")),
		required: []string{"owner", "repo", "issue_number"},
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "issue_number", true)
			if err != nil {
				return nil, err
			}
			i, err := issueOf(r, n)
			if err != nil {
				return nil, err
			}
			if t, ok := argOpt(args, "title"); ok && t != "" {
				i.Title = t
			}
			if b, ok := argOpt(args, "body"); ok {
				i.Body = b
			}
			if _, has := args["labels"]; has {
				labels, unknown := resolveLabels(r, args["labels"])
				if len(unknown) > 0 {
					return nil, ferr(422, "label %s does not exist", unknown[0])
				}
				i.Labels = labels
			}
			if s, _ := argOpt(args, "state"); s == "open" || s == "closed" {
				f.SetIssueState(i, s, "")
			}
			f.TouchIssue(i)
			return decodeJSON(issueJSON(origin, r, i)), nil
		}},
	{name: "add_issue_comment", title: "Add issue comment", description: "Add a comment to an issue or pull request.",
		props: props(ownerRepoProps, p("issue_number", "number", "The issue or pull request number"), p("body", "string", "Comment content")), required: []string{"owner", "repo", "issue_number", "body"},
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "issue_number", true)
			if err != nil {
				return nil, err
			}
			body, err := argText(args, "body", true)
			if err != nil {
				return nil, err
			}
			var list *[]*Comment
			if i := r.Issues[n]; i != nil {
				list = &i.Comments
			} else if pl := r.Pulls[n]; pl != nil {
				list = &pl.Comments
			} else {
				return nil, ferr(404, "issue #%d not found", n)
			}
			return commentJSON(origin, r, f.AddComment(r, n, list, body)), nil
		}},
	{name: "list_issue_comments", title: "List issue comments", description: "List the comments on an issue or pull request.",
		props: props(ownerRepoProps, p("issue_number", "number", "The issue or pull request number"), pagingProps), required: []string{"owner", "repo", "issue_number"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "issue_number", true)
			if err != nil {
				return nil, err
			}
			var list []*Comment
			if i := r.Issues[n]; i != nil {
				list = i.Comments
			} else if pl := r.Pulls[n]; pl != nil {
				list = pl.Comments
			} else {
				return nil, ferr(404, "issue #%d not found", n)
			}
			out := []any{}
			for _, c := range pageOf(list, args) {
				out = append(out, commentJSON(origin, r, c))
			}
			return M{"total_count": len(list), "comments": out}, nil
		}},
	{name: "create_pull_request", title: "Open new pull request", description: "Create a new pull request from a head branch into a base branch.",
		props:    props(ownerRepoProps, p("title", "string", "PR title"), p("body", "string", "PR description"), p("head", "string", "Branch containing the changes"), p("base", "string", "Branch to merge into"), p("draft", "boolean", "Create as a draft PR")),
		required: []string{"owner", "repo", "title", "head", "base"},
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			title, err := argText(args, "title", true)
			if err != nil {
				return nil, err
			}
			head, err := argText(args, "head", true)
			if err != nil {
				return nil, err
			}
			base, err := argText(args, "base", true)
			if err != nil {
				return nil, err
			}
			body, _ := argOpt(args, "body")
			pl, err := openPull(f, r, title, body, head, base, args["draft"] == true)
			if err != nil {
				return nil, err
			}
			return pullJSON(f, origin, r, pl, true), nil
		}},
	{name: "get_pull_request", title: "Get pull request", description: "Get one pull request by its number: state, head and base, mergeability.",
		props: props(ownerRepoProps, p("pull_number", "number", "The pull request number")), required: []string{"owner", "repo", "pull_number"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "pull_number", true)
			if err != nil {
				return nil, err
			}
			pl, err := pullOf(r, n)
			if err != nil {
				return nil, err
			}
			return pullJSON(f, origin, r, pl, true), nil
		}},
	{name: "list_pull_requests", title: "List pull requests", description: "List the pull requests of a repository by state.",
		props:    props(ownerRepoProps, pEnum("state", "Filter by state (default open)", "open", "closed", "all"), p("head", "string", "Filter by head branch"), p("base", "string", "Filter by base branch"), pagingProps),
		required: []string{"owner", "repo"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			state, _ := argOpt(args, "state")
			if state == "" {
				state = "open"
			}
			head, _ := argOpt(args, "head")
			if i := strings.IndexByte(head, ':'); i >= 0 {
				head = head[i+1:]
			}
			base, _ := argOpt(args, "base")
			var list []*Pull
			for i := len(r.pullOrder) - 1; i >= 0; i-- {
				pl := r.pullOrder[i]
				if (state == "all" || pl.State == state) && (head == "" || pl.Head == head) && (base == "" || pl.Base == base) {
					list = append(list, pl)
				}
			}
			out := []any{}
			for _, pl := range pageOf(list, args) {
				out = append(out, pullJSON(f, origin, r, pl, false))
			}
			return M{"total_count": len(list), "pull_requests": out}, nil
		}},
	{name: "get_pull_request_files", title: "Get pull request files", description: "List the files a pull request changes, with their status and line counts.",
		props: props(ownerRepoProps, p("pull_number", "number", "The pull request number")), required: []string{"owner", "repo", "pull_number"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "pull_number", true)
			if err != nil {
				return nil, err
			}
			pl, err := pullOf(r, n)
			if err != nil {
				return nil, err
			}
			return M{"files": pullFilesJSON(f, origin, r, pl)}, nil
		}},
	{name: "merge_pull_request", title: "Merge pull request", description: "Merge a pull request into its base branch (squash by default).",
		props:    props(ownerRepoProps, p("pull_number", "number", "The pull request number"), pEnum("merge_method", "Merge method (default squash)", "merge", "squash", "rebase"), p("commit_title", "string", "Title for the merge commit"), p("commit_message", "string", "Extra detail for the merge commit")),
		required: []string{"owner", "repo", "pull_number"},
		run: func(f *Forge, args M, _ string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			n, err := argInt(args, "pull_number", true)
			if err != nil {
				return nil, err
			}
			pl, err := pullOf(r, n)
			if err != nil {
				return nil, err
			}
			method, _ := argOpt(args, "merge_method")
			if method == "" {
				method = "squash"
			}
			title, _ := argOpt(args, "commit_title")
			msg, _ := argOpt(args, "commit_message")
			sha, err := f.MergePull(r, pl, method, title, msg, "")
			if err != nil {
				return nil, err
			}
			return M{"sha": sha, "merged": true, "message": "Pull Request successfully merged", "number": pl.Number}, nil
		}},
	{name: "list_branches", title: "List branches", description: "List the branches of a repository with their tip commits.",
		props: props(ownerRepoProps, pagingProps), required: []string{"owner", "repo"}, readOnly: true,
		run: func(f *Forge, args M, _ string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			var list []any
			for _, n := range sortedRefNames(r) {
				if strings.HasPrefix(n, "refs/heads/") {
					list = append(list, M{"name": n[11:], "commit": M{"sha": r.Refs[n]}, "protected": false})
				}
			}
			return M{"branches": append([]any{}, pageOf(list, args)...)}, nil
		}},
	{name: "create_branch", title: "Create branch", description: "Create a new branch from another branch (the default branch when from_branch is omitted).",
		props: props(ownerRepoProps, p("branch", "string", "Name for the new branch"), p("from_branch", "string", "Source branch (defaults to the default branch)")), required: []string{"owner", "repo", "branch"},
		run: func(f *Forge, args M, _ string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			branch, err := argText(args, "branch", true)
			if err != nil {
				return nil, err
			}
			from, _ := argOpt(args, "from_branch")
			if from == "" {
				from = r.DefaultBranch
			}
			sha := f.BranchTip(r, from)
			if sha == "" {
				return nil, ferr(404, "branch %s not found", from)
			}
			if f.BranchExists(r, branch) {
				return nil, ferr(422, "branch %s already exists", branch)
			}
			f.SetRef(r, "refs/heads/"+branch, sha)
			return M{"ref": "refs/heads/" + branch, "object": M{"sha": sha, "type": "commit"}}, nil
		}},
	{name: "list_commits", title: "List commits", description: "List the commits of a branch, tag or commit, newest first.",
		props: props(ownerRepoProps, p("sha", "string", "Branch, tag or commit SHA to start from (default branch when omitted)"), pagingProps), required: []string{"owner", "repo"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			start, _ := argOpt(args, "sha")
			tip := f.Resolve(r, start)
			if tip == "" {
				return nil, ferr(404, "no commit found for %q", start)
			}
			per, page := pageArgs(args)
			list := f.WalkCommits([]string{tip}, per*page)
			out := []any{}
			for _, oid := range pageOf(list, args) {
				out = append(out, commitJSON(f, origin, r, oid, f.CommitOf(oid)))
			}
			return M{"commits": out}, nil
		}},
	{name: "get_file_contents", title: "Get file or directory contents", description: "Get the contents of a file (decoded text) or a directory listing at a ref.",
		props: props(ownerRepoProps, p("path", "string", "Path to the file or directory (empty for the root)"), p("ref", "string", "Branch, tag or commit (default branch when omitted)")), required: []string{"owner", "repo"}, readOnly: true,
		run: func(f *Forge, args M, _ string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			ref, _ := argOpt(args, "ref")
			if ref == "" {
				ref = r.DefaultBranch
			}
			sha := f.Resolve(r, ref)
			if sha == "" {
				return nil, ferr(404, "ref %s not found", ref)
			}
			path, _ := argOpt(args, "path")
			path = strings.Trim(path, "/")
			var e *PathEntry
			if path == "" {
				e = &PathEntry{IsTree: true, OID: f.CommitOf(sha).Tree}
			} else {
				e = f.LookupPath(sha, path)
			}
			if e == nil {
				return nil, ferr(404, "%s not found at %s", path, ref)
			}
			if e.IsTree {
				t := f.Objects[e.OID]
				list, _ := parseTree(t.Data)
				names := []any{}
				for _, x := range list {
					names = append(names, x.Name)
				}
				return M{"type": "dir", "path": path, "entries": names}, nil
			}
			b := f.Objects[e.OID]
			return M{"type": "file", "path": path, "sha": e.OID, "size": len(b.Data), "encoding": "utf-8", "content": string(b.Data)}, nil
		}},
	{name: "create_or_update_file", title: "Create or update file", description: "Create or update one file on a branch with a commit. Pass the current blob sha to update an existing file.",
		props:    props(ownerRepoProps, p("path", "string", "Path of the file"), p("content", "string", "Content of the file (plain text)"), p("message", "string", "Commit message"), p("branch", "string", "Branch to commit to (default branch when omitted)"), p("sha", "string", "Blob SHA of the file being replaced (required for updates)")),
		required: []string{"owner", "repo", "path", "content", "message"},
		run: func(f *Forge, args M, _ string) (any, error) {
			r, err := repoArg(f, args)
			if err != nil {
				return nil, err
			}
			path, err := argText(args, "path", true)
			if err != nil {
				return nil, err
			}
			msg, err := argText(args, "message", true)
			if err != nil {
				return nil, err
			}
			content, _ := argOpt(args, "content")
			branch, _ := argOpt(args, "branch")
			sha, _ := argOpt(args, "sha")
			_, commit, blob, err := writeFile(f, r, writeInput{Path: path, Content: []byte(content), Message: msg, Branch: branch, SHA: sha})
			if err != nil {
				return nil, err
			}
			return M{"content": M{"path": path, "sha": blob}, "commit": M{"sha": commit}}, nil
		}},
	{name: "search_issues", title: "Search issues", description: "Search issues and pull requests of the user's repositories by words in their title or body.",
		props: props(p("query", "string", "Search words; repo:owner/name and is:open / is:closed qualifiers are understood"), pagingProps), required: []string{"query"}, readOnly: true,
		run: func(f *Forge, args M, origin string) (any, error) {
			q, err := argText(args, "query", true)
			if err != nil {
				return nil, err
			}
			var words []string
			repoFilter, state := "", ""
			for _, t := range strings.Fields(q) {
				switch {
				case strings.HasPrefix(t, "repo:"):
					repoFilter = strings.ToLower(t[5:])
				case t == "is:open" || t == "is:closed":
					state = t[3:]
				case !strings.Contains(t, ":"):
					words = append(words, strings.ToLower(t))
				}
			}
			var hits []any
			for _, r := range f.ReposOf(f.Viewer) {
				if repoFilter != "" && strings.ToLower(r.Owner.Login+"/"+r.Name) != repoFilter {
					continue
				}
				for _, i := range r.issueOrder {
					if state != "" && i.State != state {
						continue
					}
					match := true
					for _, w := range words {
						if !strings.Contains(strings.ToLower(i.Title), w) && !strings.Contains(strings.ToLower(i.Body), w) {
							match = false
						}
					}
					if match {
						hits = append(hits, decodeJSON(issueJSON(origin, r, i)))
					}
				}
			}
			return M{"total_count": len(hits), "items": append([]any{}, pageOf(hits, args)...)}, nil
		}},
}

// toolList is the tools/list answer, rendered once so every call is byte-identical.
var toolList = func() []byte {
	var b bytes.Buffer
	b.WriteString(`[`)
	for i, t := range tools {
		if i > 0 {
			b.WriteByte(',')
		}
		var pb bytes.Buffer
		pb.WriteByte('{')
		for j, pr := range t.props {
			if j > 0 {
				pb.WriteByte(',')
			}
			pb.Write(marshal(pr.name))
			pb.WriteByte(':')
			pb.Write(marshal(pr.schema))
		}
		pb.WriteByte('}')
		req := t.required
		if req == nil {
			req = []string{}
		}
		fmt.Fprintf(&b, `{"name":%s,"title":%s,"description":%s,"inputSchema":{"type":"object","properties":%s,"required":%s,"additionalProperties":false},"annotations":{"title":%s,"readOnlyHint":%t,"destructiveHint":false,"idempotentHint":%t,"openWorldHint":false}}`,
			marshal(t.name), marshal(t.title), marshal(t.description), pb.Bytes(), marshal(req), marshal(t.title), t.readOnly, t.readOnly)
	}
	b.WriteString(`]`)
	return b.Bytes()
}()

func findTool(name string) *tool {
	for i := range tools {
		if tools[i].name == name {
			return &tools[i]
		}
	}
	lower := strings.ToLower(name)
	for i := range tools {
		n := tools[i].name
		if lower == n || strings.HasSuffix(lower, "__"+n) || strings.HasSuffix(lower, "."+n) || strings.HasSuffix(lower, "/"+n) {
			return &tools[i]
		}
	}
	return nil
}

func rpcError(id any, code int, message string) M {
	return M{"jsonrpc": "2.0", "id": id, "error": M{"code": code, "message": message}}
}

func rpcResult(id any, result any) M { return M{"jsonrpc": "2.0", "id": id, "result": result} }

func toolResult(v any) M {
	structured, ok := v.(M)
	if !ok {
		structured = M{"result": v}
	}
	return M{"content": []any{M{"type": "text", "text": string(marshal(v))}}, "structuredContent": structured, "isError": false}
}

func toolError(msg string) M {
	return M{"content": []any{M{"type": "text", "text": msg}}, "isError": true}
}

// rawJSON passes pre-rendered JSON through marshal untouched.
type rawJSON []byte

func (r rawJSON) MarshalJSON() ([]byte, error) { return r, nil }

var capabilities = M{"tools": M{"listChanged": false}, "resources": M{}, "prompts": M{}, "logging": M{}}

func dispatch(f *Forge, msg any, origin string) M {
	m, ok := msg.(M)
	if !ok {
		return rpcError(nil, -32600, "Invalid Request: expected a JSON-RPC object")
	}
	id, hasID := m["id"]
	method, isStr := m["method"].(string)
	if m["jsonrpc"] != "2.0" || !isStr {
		if !hasID {
			return nil
		}
		return rpcError(id, -32600, "Invalid Request")
	}
	switch id.(type) {
	case nil, string, int64, float64:
	default:
		return rpcError(nil, -32600, "Invalid Request: bad id")
	}
	params, _ := m["params"].(M)
	if params == nil {
		params = M{}
	}
	if strings.HasPrefix(method, "notifications/") {
		return nil
	}
	switch method {
	case "server/discover":
		return rpcResult(id, M{"supportedVersions": mcpSupported, "capabilities": capabilities, "serverInfo": mcpServerInfo, "instructions": mcpInstructions,
			"_meta": M{"io.modelcontextprotocol/serverInfo": mcpServerInfo}})
	case "initialize":
		v, _ := params["protocolVersion"].(string)
		chosen := mcpSupported[1]
		for _, s := range mcpSupported {
			if s == v {
				chosen = v
			}
		}
		return rpcResult(id, M{"protocolVersion": chosen, "capabilities": capabilities, "serverInfo": mcpServerInfo, "instructions": mcpInstructions})
	case "ping", "logging/setLevel":
		return rpcResult(id, M{})
	case "tools/list":
		return rpcResult(id, M{"tools": rawJSON(toolList)})
	case "resources/list":
		return rpcResult(id, M{"resources": []any{}})
	case "resources/templates/list":
		return rpcResult(id, M{"resourceTemplates": []any{}})
	case "prompts/list":
		return rpcResult(id, M{"prompts": []any{}})
	case "completion/complete":
		return rpcResult(id, M{"completion": M{"values": []any{}, "hasMore": false}})
	case "tools/call":
		name, _ := params["name"].(string)
		if name == "" {
			return rpcError(id, -32602, "Invalid params: tools/call needs a tool name")
		}
		t := findTool(name)
		if t == nil {
			return rpcError(id, -32602, "Unknown tool: "+name)
		}
		args, _ := params["arguments"].(M)
		if args == nil {
			args = M{}
		}
		var missing []string
		for _, r := range t.required {
			if v, has := args[r]; !has || v == nil || v == "" {
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			return rpcResult(id, toolError("missing required parameter(s): "+strings.Join(missing, ", ")))
		}
		out, err := t.run(f, args, origin)
		if err != nil {
			return rpcResult(id, toolError(err.Error()))
		}
		return rpcResult(id, toolResult(out))
	}
	return rpcError(id, -32601, "Method not found: "+method)
}

func randomSession() string {
	var b [16]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func (s *Server) mcp(req *Request, method, origin string) *Response {
	switch s.auth(req) {
	case "missing":
		return jsonResponse(401, rpcError(nil, -32001, "Unauthorized: a bearer token is required"), [2]string{"WWW-Authenticate", `Bearer realm="forge", error="invalid_token"`})
	case "bad":
		return jsonResponse(401, rpcError(nil, -32001, "Unauthorized: bad credentials"), [2]string{"WWW-Authenticate", `Bearer realm="forge", error="invalid_token"`})
	}
	switch method {
	case "DELETE":
		return respond(204, "", nil)
	case "GET":
		return jsonResponse(405, rpcError(nil, -32000, "Method Not Allowed: this server sends no server-initiated messages"), [2]string{"Allow", "POST, DELETE"})
	case "POST":
	default:
		return jsonResponse(405, rpcError(nil, -32000, "Method Not Allowed"), [2]string{"Allow", "POST, DELETE"})
	}
	dec := json.NewDecoder(bytes.NewReader(req.Body))
	dec.UseNumber()
	var msg any
	if err := dec.Decode(&msg); err != nil || dec.More() {
		return jsonResponse(400, rpcError(nil, -32700, "Parse error: the body is not valid JSON"))
	}
	msg = normalizeNumbers(msg)
	if list, ok := msg.([]any); ok {
		if len(list) == 0 {
			return jsonResponse(400, rpcError(nil, -32600, "Invalid Request: empty batch"))
		}
		var answers []any
		for _, item := range list {
			if a := dispatch(s.forge, item, origin); a != nil {
				answers = append(answers, a)
			}
		}
		if len(answers) == 0 {
			return respond(202, "", nil)
		}
		return jsonResponse(200, answers)
	}
	var extra [][2]string
	if m, ok := msg.(M); ok && m["method"] == "initialize" {
		extra = append(extra, [2]string{"Mcp-Session-Id", randomSession()})
	}
	answer := dispatch(s.forge, msg, origin)
	if answer == nil {
		return respond(202, "", nil, extra...)
	}
	status := 200
	if e, ok := answer["error"].(M); ok && e["code"] == -32600 {
		status = 400
	}
	return jsonResponse(status, answer, extra...)
}
