// Git object model and wire formats: object hashing, trees/commits/tags, pkt-lines and packfiles.

package main

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const zeroOID = "0000000000000000000000000000000000000000"

var oidRE = regexp.MustCompile(`^[0-9a-f]{40}$`)

type GitError struct{ msg string }

func (e *GitError) Error() string { return e.msg }

func gitErr(format string, args ...any) error { return &GitError{fmt.Sprintf(format, args...)} }

type Object struct {
	Type string // commit, tree, blob, tag
	Data []byte
}

func hashObject(typ string, data []byte) string {
	h := sha1.New()
	h.Write([]byte(typ + " " + strconv.Itoa(len(data)) + "\x00"))
	h.Write(data)
	return hex.EncodeToString(h.Sum(nil))
}

// ---------------------------------------------------------------- trees

type TreeEntry struct {
	Mode string
	Name string
	OID  string
}

func isTreeMode(mode string) bool { return mode == "40000" || mode == "040000" }

func parseTree(data []byte) ([]TreeEntry, error) {
	var out []TreeEntry
	pos := 0
	for pos < len(data) {
		sp := bytes.IndexByte(data[pos:], ' ')
		if sp < 0 {
			return nil, gitErr("malformed tree object")
		}
		sp += pos
		nul := bytes.IndexByte(data[sp+1:], 0)
		if nul < 0 {
			return nil, gitErr("malformed tree object")
		}
		nul += sp + 1
		if nul+21 > len(data) {
			return nil, gitErr("malformed tree object")
		}
		out = append(out, TreeEntry{Mode: string(data[pos:sp]), Name: string(data[sp+1 : nul]), OID: hex.EncodeToString(data[nul+1 : nul+21])})
		pos = nul + 21
	}
	return out, nil
}

func buildTree(entries []TreeEntry) []byte {
	sorted := append([]TreeEntry(nil), entries...)
	key := func(e TreeEntry) string {
		if isTreeMode(e.Mode) {
			return e.Name + "/"
		}
		return e.Name
	}
	sort.Slice(sorted, func(i, j int) bool { return key(sorted[i]) < key(sorted[j]) })
	var buf bytes.Buffer
	for _, e := range sorted {
		mode := e.Mode
		if mode == "040000" {
			mode = "40000"
		}
		buf.WriteString(mode + " " + e.Name + "\x00")
		raw, _ := hex.DecodeString(e.OID)
		buf.Write(raw)
	}
	return buf.Bytes()
}

// ---------------------------------------------------------------- commits and tags

type Signature struct {
	Name  string
	Email string
	Time  int64
	TZ    string
}

type Commit struct {
	Tree      string
	Parents   []string
	Author    Signature
	Committer Signature
	Message   string
}

var sigRE = regexp.MustCompile(`^(.*?) <([^>]*)> (-?\d+) ([+-]\d{4})$`)

func parseSignature(text string) Signature {
	m := sigRE.FindStringSubmatch(text)
	if m == nil {
		return Signature{Name: text, TZ: "+0000"}
	}
	t, _ := strconv.ParseInt(m[3], 10, 64)
	return Signature{Name: m[1], Email: m[2], Time: t, TZ: m[4]}
}

func (s Signature) String() string {
	return fmt.Sprintf("%s <%s> %d %s", s.Name, s.Email, s.Time, s.TZ)
}

func parseHeaders(data []byte) ([][2]string, string) {
	text := string(data)
	head, message := text, ""
	if i := strings.Index(text, "\n\n"); i >= 0 {
		head, message = text[:i], text[i+2:]
	}
	var headers [][2]string
	for _, line := range strings.Split(head, "\n") {
		if strings.HasPrefix(line, " ") && len(headers) > 0 {
			headers[len(headers)-1][1] += "\n" + line[1:]
			continue
		}
		sp := strings.IndexByte(line, ' ')
		if sp < 0 {
			continue
		}
		headers = append(headers, [2]string{line[:sp], line[sp+1:]})
	}
	return headers, message
}

func parseCommit(data []byte) *Commit {
	headers, message := parseHeaders(data)
	c := &Commit{Message: message, Author: Signature{TZ: "+0000"}, Committer: Signature{TZ: "+0000"}}
	for _, h := range headers {
		switch h[0] {
		case "tree":
			c.Tree = h[1]
		case "parent":
			c.Parents = append(c.Parents, h[1])
		case "author":
			c.Author = parseSignature(h[1])
		case "committer":
			c.Committer = parseSignature(h[1])
		}
	}
	return c
}

func buildCommit(c *Commit) []byte {
	var b strings.Builder
	b.WriteString("tree " + c.Tree + "\n")
	for _, p := range c.Parents {
		b.WriteString("parent " + p + "\n")
	}
	b.WriteString("author " + c.Author.String() + "\ncommitter " + c.Committer.String() + "\n\n")
	b.WriteString(c.Message)
	if !strings.HasSuffix(c.Message, "\n") {
		b.WriteString("\n")
	}
	return []byte(b.String())
}

type Tag struct {
	Object, Type, Tag, Message string
}

func parseTag(data []byte) Tag {
	headers, message := parseHeaders(data)
	t := Tag{Message: message}
	for _, h := range headers {
		switch h[0] {
		case "object":
			t.Object = h[1]
		case "type":
			t.Type = h[1]
		case "tag":
			t.Tag = h[1]
		}
	}
	return t
}

// ---------------------------------------------------------------- pkt-lines

const pktMax = 65520

var (
	flushPkt = []byte("0000")
	delimPkt = []byte("0001")
)

func pkt(payload string) []byte {
	return append([]byte(fmt.Sprintf("%04x", len(payload)+4)), payload...)
}

type Packet struct {
	Kind    byte // 'd' data, 'f' flush, 'm' delim, 'e' end
	Payload []byte
}

func (p Packet) Text() string {
	if p.Kind != 'd' {
		return ""
	}
	return strings.TrimSuffix(string(p.Payload), "\n")
}

func readPackets(buf []byte, stopAtFlush bool) ([]Packet, int, error) {
	var out []Packet
	pos := 0
	for pos < len(buf) {
		if pos+4 > len(buf) {
			return nil, 0, gitErr("truncated pkt-line length")
		}
		n, err := strconv.ParseUint(string(buf[pos:pos+4]), 16, 16)
		if err != nil {
			return nil, 0, gitErr("bad pkt-line length %q", buf[pos:pos+4])
		}
		switch n {
		case 0:
			out = append(out, Packet{Kind: 'f'})
			pos += 4
			if stopAtFlush {
				return out, pos, nil
			}
			continue
		case 1:
			out = append(out, Packet{Kind: 'm'})
			pos += 4
			continue
		case 2:
			out = append(out, Packet{Kind: 'e'})
			pos += 4
			continue
		}
		if n < 4 || pos+int(n) > len(buf) {
			return nil, 0, gitErr("truncated pkt-line")
		}
		out = append(out, Packet{Kind: 'd', Payload: buf[pos+4 : pos+int(n)]})
		pos += int(n)
	}
	return out, pos, nil
}

func sideband(dst *bytes.Buffer, band byte, data []byte) {
	chunk := pktMax - 5
	for pos := 0; pos < len(data); pos += chunk {
		end := pos + chunk
		if end > len(data) {
			end = len(data)
		}
		fmt.Fprintf(dst, "%04x", end-pos+5)
		dst.WriteByte(band)
		dst.Write(data[pos:end])
	}
}

// ---------------------------------------------------------------- packfiles

var typeCode = map[string]byte{"commit": 1, "tree": 2, "blob": 3, "tag": 4}
var codeType = map[byte]string{1: "commit", 2: "tree", 3: "blob", 4: "tag"}

func applyDelta(base, delta []byte) ([]byte, error) {
	pos := 0
	varint := func() (int, error) {
		result, shift := 0, 0
		for {
			if pos >= len(delta) {
				return 0, gitErr("truncated delta header")
			}
			b := delta[pos]
			pos++
			result |= int(b&0x7f) << shift
			shift += 7
			if b&0x80 == 0 {
				return result, nil
			}
		}
	}
	baseSize, err := varint()
	if err != nil {
		return nil, err
	}
	if baseSize != len(base) {
		return nil, gitErr("delta base size mismatch")
	}
	size, err := varint()
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, size)
	for pos < len(delta) {
		op := delta[pos]
		pos++
		if op&0x80 != 0 {
			off, n := 0, 0
			for i := uint(0); i < 4; i++ {
				if op&(1<<i) != 0 {
					if pos >= len(delta) {
						return nil, gitErr("truncated delta copy")
					}
					off |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			for i := uint(0); i < 3; i++ {
				if op&(0x10<<i) != 0 {
					if pos >= len(delta) {
						return nil, gitErr("truncated delta copy")
					}
					n |= int(delta[pos]) << (8 * i)
					pos++
				}
			}
			if n == 0 {
				n = 0x10000
			}
			if off+n > len(base) {
				return nil, gitErr("delta copy out of range")
			}
			out = append(out, base[off:off+n]...)
		} else if op > 0 {
			if pos+int(op) > len(delta) {
				return nil, gitErr("delta insert out of range")
			}
			out = append(out, delta[pos:pos+int(op)]...)
			pos += int(op)
		} else {
			return nil, gitErr("delta opcode 0 is reserved")
		}
	}
	if len(out) != size {
		return nil, gitErr("delta result size mismatch")
	}
	return out, nil
}

type PackedObject struct {
	OID  string
	Type string
	Data []byte
}

type countingReader struct {
	r *bytes.Reader
}

// inflateAt inflates the zlib stream at buf[pos:] and reports how many input bytes it used.
func inflateAt(buf []byte, pos int, size int) ([]byte, int, error) {
	br := bytes.NewReader(buf[pos:])
	zr, err := zlib.NewReader(br)
	if err != nil {
		return nil, 0, gitErr("bad zlib stream: %v", err)
	}
	out := make([]byte, size)
	if _, err := io.ReadFull(zr, out); err != nil {
		return nil, 0, gitErr("bad zlib stream: %v", err)
	}
	// Drain to the stream's end so the checksum is consumed and the reader position is exact.
	var one [1]byte
	if n, err := zr.Read(one[:]); n != 0 || (err != nil && err != io.EOF) {
		if n != 0 {
			return nil, 0, gitErr("pack object larger than its header says")
		}
		return nil, 0, gitErr("bad zlib stream: %v", err)
	}
	used := len(buf) - pos - br.Len()
	return out, used, nil
}

type rawEntry struct {
	offset, code int
	data         []byte
	baseOffset   int
	baseOID      string
}

func readPack(buf []byte, lookup func(string) *Object) ([]PackedObject, error) {
	if len(buf) < 32 || string(buf[:4]) != "PACK" {
		return nil, gitErr("pack does not start with PACK")
	}
	version := binary.BigEndian.Uint32(buf[4:8])
	if version != 2 && version != 3 {
		return nil, gitErr("unsupported pack version %d", version)
	}
	count := int(binary.BigEndian.Uint32(buf[8:12]))
	trailer := len(buf) - 20
	sum := sha1.Sum(buf[:trailer])
	if !bytes.Equal(sum[:], buf[trailer:]) {
		return nil, gitErr("pack checksum mismatch")
	}
	entries := make([]*rawEntry, 0, count)
	pos := 12
	for i := 0; i < count; i++ {
		if pos >= trailer {
			return nil, gitErr("pack truncated")
		}
		offset := pos
		b := buf[pos]
		pos++
		code := int(b>>4) & 7
		size := int(b & 15)
		shift := 4
		for b&0x80 != 0 {
			b = buf[pos]
			pos++
			size |= int(b&0x7f) << shift
			shift += 7
		}
		e := &rawEntry{offset: offset, code: code}
		switch code {
		case 6:
			b = buf[pos]
			pos++
			rel := int(b & 0x7f)
			for b&0x80 != 0 {
				b = buf[pos]
				pos++
				rel = ((rel + 1) << 7) | int(b&0x7f)
			}
			e.baseOffset = offset - rel
		case 7:
			e.baseOID = hex.EncodeToString(buf[pos : pos+20])
			pos += 20
		default:
			if _, ok := codeType[byte(code)]; !ok {
				return nil, gitErr("unknown pack object type %d", code)
			}
		}
		data, used, err := inflateAt(buf[:trailer], pos, size)
		if err != nil {
			return nil, err
		}
		e.data = data
		pos += used
		entries = append(entries, e)
	}
	resolved := map[int]*PackedObject{}
	byOID := map[string]*PackedObject{}
	settle := func(e *rawEntry, typ string, data []byte) {
		o := &PackedObject{OID: hashObject(typ, data), Type: typ, Data: data}
		resolved[e.offset] = o
		byOID[o.OID] = o
	}
	var pending []*rawEntry
	for _, e := range entries {
		if e.code == 6 || e.code == 7 {
			pending = append(pending, e)
		} else {
			settle(e, codeType[byte(e.code)], e.data)
		}
	}
	for len(pending) > 0 {
		var next []*rawEntry
		for _, e := range pending {
			var baseType string
			var baseData []byte
			if e.code == 6 {
				if o := resolved[e.baseOffset]; o != nil {
					baseType, baseData = o.Type, o.Data
				}
			} else if o := byOID[e.baseOID]; o != nil {
				baseType, baseData = o.Type, o.Data
			} else if o := lookup(e.baseOID); o != nil {
				baseType, baseData = o.Type, o.Data
			}
			if baseType == "" {
				next = append(next, e)
				continue
			}
			data, err := applyDelta(baseData, e.data)
			if err != nil {
				return nil, err
			}
			settle(e, baseType, data)
		}
		if len(next) == len(pending) {
			return nil, errors.New("delta base missing")
		}
		pending = next
	}
	out := make([]PackedObject, 0, len(entries))
	for _, e := range entries {
		out = append(out, *resolved[e.offset])
	}
	return out, nil
}

var zlibPool = sync.Pool{New: func() any { w, _ := zlib.NewWriterLevel(nil, zlib.BestSpeed); return w }}

// packEntry is one object's pack entry: its header and deflated body.
func packEntry(typ string, data []byte) []byte {
	var buf bytes.Buffer
	size := len(data)
	b := typeCode[typ]<<4 | byte(size&15)
	size >>= 4
	for size > 0 {
		buf.WriteByte(b | 0x80)
		b = byte(size & 0x7f)
		size >>= 7
	}
	buf.WriteByte(b)
	w := zlibPool.Get().(*zlib.Writer)
	w.Reset(&buf)
	w.Write(data)
	w.Close()
	zlibPool.Put(w)
	return buf.Bytes()
}

func assemblePack(entries [][]byte) []byte {
	total := 12 + 20
	for _, e := range entries {
		total += len(e)
	}
	out := make([]byte, 0, total)
	out = append(out, 'P', 'A', 'C', 'K', 0, 0, 0, 2)
	out = binary.BigEndian.AppendUint32(out, uint32(len(entries)))
	for _, e := range entries {
		out = append(out, e...)
	}
	sum := sha1.Sum(out)
	return append(out, sum[:]...)
}
