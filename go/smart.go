// Git smart HTTP: info/refs advertisements, upload-pack ls-refs and fetch (v2 and v0, shallow and
// deepen), and receive-pack with per-ref fast-forward checks and a side-band report-status.

package main

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

const agentCap = "agent=git-forge/2.0"

var uploadV0Caps = []string{"thin-pack", "side-band", "side-band-64k", "ofs-delta", "shallow", "deepen-since", "deepen-not", "deepen-relative", "no-progress", "include-tag", "allow-tip-sha1-in-want", "allow-reachable-sha1-in-want", "object-format=sha1", agentCap}
var receiveCaps = []string{"report-status", "delete-refs", "side-band-64k", "quiet", "atomic", "ofs-delta", "object-format=sha1", agentCap}

func advertise(f *Forge, r *Repo, service string, v2 bool) []byte {
	var b bytes.Buffer
	if service == "git-upload-pack" && v2 {
		for _, line := range []string{"version 2\n", agentCap + "\n", "ls-refs=unborn\n", "fetch=shallow\n", "server-option\n", "object-format=sha1\n"} {
			b.Write(pkt(line))
		}
		b.Write(flushPkt)
		return b.Bytes()
	}
	b.Write(pkt("# service=" + service + "\n"))
	b.Write(flushPkt)
	var lines []string
	names := sortedRefNames(r)
	if service == "git-upload-pack" {
		if head, ok := r.Refs[f.HeadRef(r)]; ok {
			lines = append(lines, head+" HEAD")
		}
		for _, n := range names {
			oid := r.Refs[n]
			lines = append(lines, oid+" "+n)
			if strings.HasPrefix(n, "refs/tags/") {
				if p := f.Peel(oid); p != oid {
					lines = append(lines, p+" "+n+"^{}")
				}
			}
		}
	} else {
		for _, n := range names {
			lines = append(lines, r.Refs[n]+" "+n)
		}
	}
	caps := receiveCaps
	if service == "git-upload-pack" {
		caps = append(append([]string(nil), uploadV0Caps...), "symref=HEAD:"+f.HeadRef(r))
	}
	if len(lines) == 0 {
		lines = append(lines, zeroOID+" capabilities^{}")
	}
	for i, line := range lines {
		if i == 0 {
			b.Write(pkt(line + "\x00" + strings.Join(caps, " ") + "\n"))
		} else {
			b.Write(pkt(line + "\n"))
		}
	}
	b.Write(flushPkt)
	return b.Bytes()
}

type fetchArgs struct {
	wants, haves, shallow, deepenNot []string
	depth, deepenSince               int64
	hasDepth, hasSince, done         bool
	includeTag, sideband             bool
}

type fetchPlan struct {
	objects, shallow, unshallow, common []string
}

type fetchCacheEntry struct {
	key  string
	body []byte
}

var fetchCache []fetchCacheEntry

func cacheGet(key string) []byte {
	for _, e := range fetchCache {
		if e.key == key {
			return e.body
		}
	}
	return nil
}

func cachePut(key string, body []byte) {
	if len(fetchCache) >= 64 {
		fetchCache = fetchCache[1:]
	}
	fetchCache = append(fetchCache, fetchCacheEntry{key, body})
}

func markTree(f *Forge, tree string, into map[string]bool) {
	if into[tree] {
		return
	}
	into[tree] = true
	o := f.Objects[tree]
	if o == nil || o.Type != "tree" {
		return
	}
	entries, _ := parseTree(o.Data)
	for _, e := range entries {
		if isTreeMode(e.Mode) {
			markTree(f, e.OID, into)
		} else if e.Mode != "160000" {
			into[e.OID] = true
		}
	}
}

func planFetch(f *Forge, r *Repo, a *fetchArgs) fetchPlan {
	clientShallow := map[string]bool{}
	for _, s := range a.shallow {
		clientShallow[s] = true
	}
	var common []string
	for _, h := range a.haves {
		if f.Objects[h] != nil {
			common = append(common, h)
		}
	}
	held := map[string]bool{}
	heldCommits := map[string]bool{}
	if len(common) > 0 {
		stack := append([]string(nil), common...)
		for len(stack) > 0 {
			oid := f.Peel(stack[len(stack)-1])
			stack = stack[:len(stack)-1]
			if heldCommits[oid] {
				continue
			}
			c := f.CommitOf(oid)
			if c == nil {
				continue
			}
			heldCommits[oid] = true
			held[oid] = true
			if !clientShallow[oid] {
				stack = append(stack, c.Parents...)
			}
		}
		for oid := range heldCommits {
			markTree(f, f.CommitOf(oid).Tree, held)
		}
	}
	excluded := map[string]bool{}
	for _, name := range a.deepenNot {
		if tip := f.Resolve(r, name); tip != "" {
			for _, oid := range f.WalkCommits([]string{tip}, 1000000) {
				excluded[oid] = true
			}
		}
	}
	var send []string
	sent := map[string]bool{}
	add := func(oid string) {
		if sent[oid] || held[oid] {
			return
		}
		sent[oid] = true
		send = append(send, oid)
	}
	var addTree func(tree string)
	addTree = func(tree string) {
		if held[tree] || sent[tree] {
			return
		}
		sent[tree] = true
		send = append(send, tree)
		o := f.Objects[tree]
		if o == nil || o.Type != "tree" {
			return
		}
		entries, _ := parseTree(o.Data)
		for _, e := range entries {
			if isTreeMode(e.Mode) {
				addTree(e.OID)
			} else if e.Mode != "160000" && !held[e.OID] && !sent[e.OID] {
				sent[e.OID] = true
				send = append(send, e.OID)
			}
		}
	}
	type qi struct {
		oid   string
		depth int64
	}
	var queue []qi
	for _, want := range a.wants {
		oid := want
		for i := 0; i < 100; i++ {
			o := f.Objects[oid]
			if o == nil || o.Type != "tag" {
				break
			}
			add(oid)
			oid = parseTag(o.Data).Object
		}
		o := f.Objects[oid]
		if o == nil {
			continue
		}
		switch o.Type {
		case "commit":
			queue = append(queue, qi{oid, 1})
		case "tree":
			addTree(oid)
		default:
			add(oid)
		}
	}
	depthOf := map[string]int64{}
	var newShallow, sentCommits []string
	traversed := map[string]bool{}
	deepening := a.hasDepth || a.hasSince || len(a.deepenNot) > 0
	for len(queue) > 0 {
		it := queue[0]
		queue = queue[1:]
		if d, ok := depthOf[it.oid]; ok && d <= it.depth {
			continue
		}
		depthOf[it.oid] = it.depth
		if excluded[it.oid] {
			continue
		}
		c := f.CommitOf(it.oid)
		if c == nil {
			continue
		}
		if !a.hasDepth && heldCommits[it.oid] {
			continue
		}
		if a.hasSince && c.Committer.Time < a.deepenSince && it.depth > 1 {
			continue
		}
		traversed[it.oid] = true
		if !heldCommits[it.oid] && !sent[it.oid] {
			add(it.oid)
			sentCommits = append(sentCommits, it.oid)
		}
		if a.hasDepth && it.depth >= a.depth {
			if len(c.Parents) > 0 && !clientShallow[it.oid] {
				newShallow = append(newShallow, it.oid)
			}
			continue
		}
		if !deepening && clientShallow[it.oid] {
			continue
		}
		for _, p := range c.Parents {
			queue = append(queue, qi{p, it.depth + 1})
		}
	}
	for _, oid := range sentCommits {
		addTree(f.CommitOf(oid).Tree)
	}
	var unshallow []string
	if deepening {
		isNew := map[string]bool{}
		for _, s := range newShallow {
			isNew[s] = true
		}
		for _, s := range a.shallow {
			if !traversed[s] || isNew[s] {
				continue
			}
			c := f.CommitOf(s)
			if c == nil {
				continue
			}
			for _, p := range c.Parents {
				if sent[p] {
					unshallow = append(unshallow, s)
					break
				}
			}
		}
	}
	if a.includeTag {
		for name, oid := range r.Refs {
			if !strings.HasPrefix(name, "refs/tags/") || sent[oid] {
				continue
			}
			if o := f.Objects[oid]; o != nil && o.Type == "tag" && sent[parseTag(o.Data).Object] {
				add(oid)
			}
		}
	}
	return fetchPlan{send, newShallow, unshallow, common}
}

func buildPackFor(f *Forge, oids []string) []byte {
	entries := make([][]byte, 0, len(oids))
	for _, oid := range oids {
		if e := f.PackEntryOf(oid); e != nil {
			entries = append(entries, e)
		}
	}
	return assemblePack(entries)
}

func parseOID(text string) (string, error) {
	oid := strings.ToLower(strings.TrimSpace(text))
	if !oidRE.MatchString(oid) {
		return "", gitErr("not an object id: %q", text)
	}
	return oid, nil
}

func (a *fetchArgs) line(line string) error {
	var err error
	var oid string
	switch {
	case strings.HasPrefix(line, "want "):
		oid, err = parseOID(line[5:min(45, len(line))])
		a.wants = append(a.wants, oid)
	case strings.HasPrefix(line, "have "):
		oid, err = parseOID(line[5:min(45, len(line))])
		a.haves = append(a.haves, oid)
	case strings.HasPrefix(line, "shallow "):
		oid, err = parseOID(line[8:min(48, len(line))])
		a.shallow = append(a.shallow, oid)
	case strings.HasPrefix(line, "deepen-since "):
		a.deepenSince, err = strconv.ParseInt(strings.TrimSpace(line[13:]), 10, 64)
		a.hasSince = true
	case strings.HasPrefix(line, "deepen-not "):
		a.deepenNot = append(a.deepenNot, line[11:])
	case strings.HasPrefix(line, "deepen "):
		a.depth, err = strconv.ParseInt(strings.TrimSpace(line[7:]), 10, 64)
		if err == nil && a.depth < 1 {
			err = gitErr("bad deepen")
		}
		a.hasDepth = true
	case line == "done":
		a.done = true
	case line == "include-tag":
		a.includeTag = true
	}
	return err
}

func (a *fetchArgs) key(r *Repo, v2 bool) string {
	return fmt.Sprint(r.ID, "|", r.Version, "|", v2, "|", a.wants, a.haves, a.shallow, a.depth, a.hasDepth, a.deepenSince, a.hasSince, a.deepenNot, a.done, a.includeTag, a.sideband)
}

func fetchV2(f *Forge, r *Repo, a *fetchArgs) []byte {
	key := a.key(r, true)
	if body := cacheGet(key); body != nil {
		return body
	}
	plan := planFetch(f, r, a)
	var b bytes.Buffer
	if !a.done {
		b.Write(pkt("acknowledgments\n"))
		if len(plan.common) == 0 {
			b.Write(pkt("NAK\n"))
		}
		for _, oid := range plan.common {
			b.Write(pkt("ACK " + oid + "\n"))
		}
		b.Write(pkt("ready\n"))
		b.Write(delimPkt)
	}
	if a.hasDepth || len(a.shallow) > 0 || a.hasSince || len(a.deepenNot) > 0 {
		b.Write(pkt("shallow-info\n"))
		for _, oid := range plan.shallow {
			b.Write(pkt("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			b.Write(pkt("unshallow " + oid + "\n"))
		}
		b.Write(delimPkt)
	}
	b.Write(pkt("packfile\n"))
	sideband(&b, 1, buildPackFor(f, plan.objects))
	b.Write(flushPkt)
	body := b.Bytes()
	cachePut(key, body)
	return body
}

func lsRefs(f *Forge, r *Repo, args []string) []byte {
	var prefixes []string
	peel, symrefs, unborn := false, false, false
	for _, line := range args {
		switch {
		case line == "peel":
			peel = true
		case line == "symrefs":
			symrefs = true
		case line == "unborn":
			unborn = true
		case strings.HasPrefix(line, "ref-prefix "):
			prefixes = append(prefixes, line[11:])
		}
	}
	matches := func(name string) bool {
		if len(prefixes) == 0 {
			return true
		}
		for _, p := range prefixes {
			if strings.HasPrefix(name, p) {
				return true
			}
		}
		return false
	}
	var b bytes.Buffer
	if matches("HEAD") {
		target := f.HeadRef(r)
		sym := ""
		if symrefs {
			sym = " symref-target:" + target
		}
		if head, ok := r.Refs[target]; ok {
			b.Write(pkt(head + " HEAD" + sym + "\n"))
		} else if unborn {
			b.Write(pkt("unborn HEAD" + sym + "\n"))
		}
	}
	for _, n := range sortedRefNames(r) {
		if !matches(n) {
			continue
		}
		oid := r.Refs[n]
		line := oid + " " + n
		if peel {
			if p := f.Peel(oid); p != oid {
				line += " peeled:" + p
			}
		}
		b.Write(pkt(line + "\n"))
	}
	b.Write(flushPkt)
	return b.Bytes()
}

func uploadPack(f *Forge, r *Repo, body []byte, v2 bool) ([]byte, error) {
	packets, _, err := readPackets(body, false)
	if err != nil {
		return nil, err
	}
	if v2 || (len(packets) > 0 && packets[0].Kind == 'd' && strings.HasPrefix(packets[0].Text(), "command=")) {
		command := ""
		i := 0
		for ; i < len(packets) && packets[i].Kind == 'd'; i++ {
			if t := packets[i].Text(); strings.HasPrefix(t, "command=") {
				command = t[8:]
			}
		}
		var args []string
		if i < len(packets) && packets[i].Kind == 'm' {
			for i++; i < len(packets) && packets[i].Kind == 'd'; i++ {
				args = append(args, packets[i].Text())
			}
		}
		switch command {
		case "ls-refs":
			return lsRefs(f, r, args), nil
		case "fetch":
			a := &fetchArgs{sideband: true}
			for _, line := range args {
				if err := a.line(line); err != nil {
					return nil, err
				}
			}
			return fetchV2(f, r, a), nil
		}
		return nil, gitErr("unknown command %q", command)
	}
	a := &fetchArgs{sideband: true}
	sawWant := false
	for _, p := range packets {
		if p.Kind != 'd' {
			continue
		}
		line := p.Text()
		if strings.HasPrefix(line, "want ") && !sawWant {
			sawWant = true
			if len(line) > 46 {
				caps := " " + line[46:] + " "
				a.sideband = strings.Contains(caps, " side-band-64k ") || strings.Contains(caps, " side-band ")
				a.includeTag = strings.Contains(caps, " include-tag ")
			}
			line = line[:min(45, len(line))]
		}
		if err := a.line(line); err != nil {
			return nil, err
		}
	}
	if len(a.wants) == 0 {
		return flushPkt, nil
	}
	plan := planFetch(f, r, a)
	var b bytes.Buffer
	if a.hasDepth || a.hasSince || len(a.deepenNot) > 0 {
		for _, oid := range plan.shallow {
			b.Write(pkt("shallow " + oid + "\n"))
		}
		for _, oid := range plan.unshallow {
			b.Write(pkt("unshallow " + oid + "\n"))
		}
		b.Write(flushPkt)
	}
	b.Write(pkt("NAK\n"))
	pack := buildPackFor(f, plan.objects)
	if a.sideband {
		sideband(&b, 1, pack)
		b.Write(flushPkt)
	} else {
		b.Write(pack)
	}
	return b.Bytes(), nil
}

type pushCommand struct{ old, new, ref string }

func validRefName(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || len(ref) > 255 {
		return false
	}
	for _, c := range ref {
		if c <= 0x20 || c == 0x7f || strings.ContainsRune("~^:?*[\\", c) {
			return false
		}
	}
	return !strings.Contains(ref, "..") && !strings.Contains(ref, "//") && !strings.HasSuffix(ref, "/") && !strings.HasSuffix(ref, ".lock") && !strings.Contains(ref, "@{")
}

func receivePack(f *Forge, r *Repo, body []byte) ([]byte, error) {
	packets, end, err := readPackets(body, true)
	if err != nil {
		return nil, err
	}
	var cmds []pushCommand
	var caps []string
	for _, p := range packets {
		if p.Kind != 'd' {
			continue
		}
		line := string(p.Payload)
		if nul := strings.IndexByte(line, 0); nul >= 0 {
			caps = strings.Fields(line[nul+1:])
			line = line[:nul]
		}
		line = strings.TrimSuffix(line, "\n")
		if strings.HasPrefix(line, "shallow ") {
			continue
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 || !oidRE.MatchString(parts[0]) || !oidRE.MatchString(parts[1]) {
			return nil, gitErr("bad command %q", line)
		}
		cmds = append(cmds, pushCommand{parts[0], parts[1], parts[2]})
	}
	has := func(name string) bool {
		for _, c := range caps {
			if c == name {
				return true
			}
		}
		return false
	}
	useSideband := has("side-band-64k") || has("side-band")
	wantsReport := has("report-status") || has("report-status-v2")
	atomic := has("atomic")
	unpack := "ok"
	if pack := body[end:]; len(pack) > 0 {
		objects, err := readPack(pack, f.GetObject)
		if err != nil {
			unpack = strings.Join(strings.Fields(err.Error()), " ")
		} else {
			for _, o := range objects {
				f.PutObjectWithID(o.OID, o.Type, o.Data)
			}
		}
	}
	hadBranches := false
	for n := range r.Refs {
		if strings.HasPrefix(n, "refs/heads/") {
			hadBranches = true
			break
		}
	}
	results := make([]string, len(cmds))
	var updates []pushCommand
	for i, c := range cmds {
		if reason := refusal(f, r, c, unpack); reason == "" {
			updates = append(updates, c)
			results[i] = "ok " + c.ref
		} else {
			results[i] = "ng " + c.ref + " " + reason
		}
	}
	if atomic && len(updates) != len(cmds) {
		for i := range results {
			if strings.HasPrefix(results[i], "ok ") {
				results[i] = "ng " + cmds[i].ref + " atomic transaction failed"
			}
		}
	} else {
		for _, c := range updates {
			f.SetRef(r, c.ref, c.new)
		}
	}
	if _, ok := r.Refs[f.HeadRef(r)]; !hadBranches && !ok {
		for _, c := range updates {
			if strings.HasPrefix(c.ref, "refs/heads/") && c.new != zeroOID {
				r.DefaultBranch = c.ref[len("refs/heads/"):]
				break
			}
		}
	}
	if !wantsReport {
		return flushPkt, nil
	}
	var report bytes.Buffer
	report.Write(pkt("unpack " + unpack + "\n"))
	for _, line := range results {
		report.Write(pkt(line + "\n"))
	}
	report.Write(flushPkt)
	if !useSideband {
		return report.Bytes(), nil
	}
	var out bytes.Buffer
	sideband(&out, 1, report.Bytes())
	out.Write(flushPkt)
	return out.Bytes(), nil
}

func refusal(f *Forge, r *Repo, c pushCommand, unpack string) string {
	if unpack != "ok" {
		return "unpacker error"
	}
	if !validRefName(c.ref) {
		return "funny refname"
	}
	current, ok := r.Refs[c.ref]
	if !ok {
		current = zeroOID
	}
	if c.old != current {
		if current != zeroOID && c.new != zeroOID && c.old != zeroOID && !f.IsAncestor(f.Peel(current), f.Peel(c.new)) {
			return "non-fast-forward"
		}
		if current == zeroOID {
			return "stale info"
		}
		return "fetch first"
	}
	if c.new == zeroOID {
		return ""
	}
	o := f.Objects[c.new]
	if o == nil {
		return "missing necessary objects"
	}
	if strings.HasPrefix(c.ref, "refs/heads/") && o.Type != "commit" {
		return "not a commit"
	}
	if current != zeroOID {
		if strings.HasPrefix(c.ref, "refs/tags/") {
			return "already exists"
		}
		if !f.IsAncestor(f.Peel(current), f.Peel(c.new)) {
			return "non-fast-forward"
		}
	}
	if cm := f.CommitOf(f.Peel(c.new)); cm != nil && f.Objects[cm.Tree] == nil {
		return "missing necessary objects"
	}
	return ""
}
