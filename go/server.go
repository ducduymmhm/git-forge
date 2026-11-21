// A minimal HTTP/1.1 server (keep-alive, pipelining, Content-Length and chunked bodies, one write per
// response) and the forge's router: /healthz, git smart HTTP, REST v3, GraphQL and MCP.

package main

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Request struct {
	Method string
	Path   string
	Query  url.Values
	Header map[string]string
	Body   []byte
}

type Response struct {
	Status  int
	Headers [][2]string
	Body    []byte
}

var reasons = map[int]string{200: "OK", 201: "Created", 202: "Accepted", 204: "No Content", 400: "Bad Request", 401: "Unauthorized",
	403: "Forbidden", 404: "Not Found", 405: "Method Not Allowed", 409: "Conflict", 410: "Gone", 422: "Unprocessable Entity",
	431: "Request Header Fields Too Large", 500: "Internal Server Error"}

const maxBody = 512 << 20

// ------------------------------------------------------------ the connection loop

func serveConn(conn net.Conn, handle func(*Request) *Response) {
	defer conn.Close()
	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	var out []byte
	for {
		req, keepAlive, err := readRequest(br)
		if err != nil {
			if err != io.EOF && err != io.ErrUnexpectedEOF {
				conn.Write(encodeResponse(nil, jsonError(400, "Bad Request"), false, false))
			}
			return
		}
		res := handle(req)
		out = encodeResponse(out[:0], res, keepAlive, req.Method == "HEAD")
		if _, err := conn.Write(out); err != nil || !keepAlive {
			return
		}
		if cap(out) > 1<<20 {
			out = nil
		}
	}
}

func readRequest(br *bufio.Reader) (*Request, bool, error) {
	line, err := br.ReadSlice('\n')
	for err == nil && len(bytes.TrimSpace(line)) == 0 {
		line, err = br.ReadSlice('\n')
	}
	if err != nil {
		if err == bufio.ErrBufferFull {
			return nil, false, io.ErrShortBuffer
		}
		return nil, false, err
	}
	first := strings.TrimRight(string(line), "\r\n")
	sp1 := strings.IndexByte(first, ' ')
	sp2 := strings.LastIndexByte(first, ' ')
	if sp1 <= 0 || sp2 <= sp1 {
		return nil, false, io.ErrNoProgress
	}
	req := &Request{Method: first[:sp1], Header: make(map[string]string, 8)}
	target := first[sp1+1 : sp2]
	version := first[sp2+1:]
	if q := strings.IndexByte(target, '?'); q >= 0 {
		req.Path = target[:q]
		req.Query, _ = url.ParseQuery(target[q+1:])
	} else {
		req.Path = target
	}
	if req.Query == nil {
		req.Query = url.Values{}
	}
	for {
		line, err = br.ReadSlice('\n')
		if err != nil {
			if err == bufio.ErrBufferFull {
				return nil, false, io.ErrShortBuffer
			}
			return nil, false, err
		}
		if len(line) <= 2 && strings.TrimSpace(string(line)) == "" {
			break
		}
		colon := bytes.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(string(line[:colon])))
		value := strings.TrimSpace(string(line[colon+1:]))
		if prev, ok := req.Header[name]; ok {
			req.Header[name] = prev + ", " + value
		} else {
			req.Header[name] = value
		}
	}
	conn := strings.ToLower(req.Header["connection"])
	keepAlive := conn != "close"
	if version == "HTTP/1.0" {
		keepAlive = conn == "keep-alive"
	}
	if strings.Contains(strings.ToLower(req.Header["transfer-encoding"]), "chunked") {
		var body []byte
		for {
			sizeLine, err := br.ReadString('\n')
			if err != nil {
				return nil, false, err
			}
			sizeText := strings.TrimSpace(strings.SplitN(sizeLine, ";", 2)[0])
			size, err := strconv.ParseInt(sizeText, 16, 64)
			if err != nil || size < 0 || int64(len(body))+size > maxBody {
				return nil, false, io.ErrNoProgress
			}
			if size == 0 {
				for {
					t, err := br.ReadString('\n')
					if err != nil {
						return nil, false, err
					}
					if strings.TrimSpace(t) == "" {
						break
					}
				}
				break
			}
			chunk := make([]byte, size+2)
			if _, err := io.ReadFull(br, chunk); err != nil {
				return nil, false, err
			}
			body = append(body, chunk[:size]...)
		}
		req.Body = body
	} else if cl := req.Header["content-length"]; cl != "" {
		n, err := strconv.ParseInt(cl, 10, 64)
		if err != nil || n < 0 || n > maxBody {
			return nil, false, io.ErrNoProgress
		}
		req.Body = make([]byte, n)
		if _, err := io.ReadFull(br, req.Body); err != nil {
			return nil, false, err
		}
	}
	return req, keepAlive, nil
}

func encodeResponse(dst []byte, res *Response, keepAlive, head bool) []byte {
	reason := reasons[res.Status]
	if reason == "" {
		reason = "Status"
	}
	dst = append(dst, "HTTP/1.1 "...)
	dst = strconv.AppendInt(dst, int64(res.Status), 10)
	dst = append(dst, ' ')
	dst = append(dst, reason...)
	dst = append(dst, "\r\n"...)
	for _, h := range res.Headers {
		dst = append(dst, h[0]...)
		dst = append(dst, ": "...)
		dst = append(dst, h[1]...)
		dst = append(dst, "\r\n"...)
	}
	if res.Status != 204 && res.Status != 304 {
		dst = append(dst, "Content-Length: "...)
		dst = strconv.AppendInt(dst, int64(len(res.Body)), 10)
		dst = append(dst, "\r\n"...)
	}
	if keepAlive {
		dst = append(dst, "Connection: keep-alive\r\n\r\n"...)
	} else {
		dst = append(dst, "Connection: close\r\n\r\n"...)
	}
	if !head && res.Status != 204 && res.Status != 304 {
		dst = append(dst, res.Body...)
	}
	return dst
}

// ------------------------------------------------------------ responses

const jsonType = "application/json; charset=utf-8"

var requestCounter atomic.Uint64
var requestPrefix = strconv.FormatInt(time.Now().UnixNano(), 16)

func requestID() string {
	return requestPrefix + ":" + strconv.FormatUint(requestCounter.Add(1), 16)
}

var apiHeaders = [][2]string{
	{"X-OAuth-Scopes", "repo, read:org, workflow, gist, user"},
	{"X-Accepted-OAuth-Scopes", "repo"},
	{"X-GitHub-Media-Type", "github.v3; format=json"},
	{"X-GitHub-Api-Version-Selected", "2022-11-28"},
}

func respond(status int, contentType string, body []byte, extra ...[2]string) *Response {
	h := make([][2]string, 0, len(extra)+2)
	h = append(h, [2]string{"X-GitHub-Request-Id", requestID()})
	if contentType != "" {
		h = append(h, [2]string{"Content-Type", contentType})
	}
	h = append(h, extra...)
	return &Response{Status: status, Headers: h, Body: body}
}

func marshal(v any) []byte {
	if b, ok := v.([]byte); ok {
		return b
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	out := buf.Bytes()
	return out[:len(out)-1]
}

func jsonResponse(status int, v any, extra ...[2]string) *Response {
	return respond(status, jsonType, marshal(v), extra...)
}

func jsonError(status int, message string) *Response {
	return jsonResponse(status, map[string]any{"message": message})
}

// ------------------------------------------------------------ the router

type Server struct {
	forge *Forge
	token string
	mu    sync.Mutex
}

func presentedToken(header string) string {
	sp := strings.IndexByte(header, ' ')
	if sp < 0 {
		return ""
	}
	scheme := strings.ToLower(header[:sp])
	value := strings.TrimSpace(header[sp+1:])
	switch scheme {
	case "bearer", "token":
		return value
	case "basic":
		raw, err := base64.StdEncoding.DecodeString(value)
		if err != nil {
			return ""
		}
		user, pass, found := strings.Cut(string(raw), ":")
		if !found {
			return user
		}
		if pass == "" {
			return user
		}
		return pass
	}
	return ""
}

// auth returns "ok", "missing" or "bad".
func (s *Server) auth(req *Request) string {
	t := presentedToken(req.Header["authorization"])
	if t == "" {
		return "missing"
	}
	if s.token != "" && t != s.token {
		return "bad"
	}
	return "ok"
}

var gitPath = regexp.MustCompile(`^/([^/]+)/([^/]+?)(?:\.git)?/(info/refs|git-upload-pack|git-receive-pack)$`)

var noCache = [][2]string{{"Cache-Control", "no-cache, max-age=0, must-revalidate"}, {"Expires", "Fri, 01 Jan 1980 00:00:00 GMT"}, {"Pragma", "no-cache"}}

func (s *Server) Handle(req *Request) (res *Response) {
	s.mu.Lock()
	defer s.mu.Unlock()
	defer func() {
		if r := recover(); r != nil {
			logf("panic serving %s %s: %v", req.Method, req.Path, r)
			res = jsonError(500, "Internal Server Error")
		}
	}()
	method := strings.ToUpper(req.Method)
	path := req.Path
	host := req.Header["host"]
	if host == "" {
		host = "localhost"
	}
	origin := "http://" + host
	switch {
	case path == "/healthz" || path == "/health" || path == "/readyz":
		return respond(200, jsonType, []byte(`{"ok":true}`))
	case path == "/api/v3" || strings.HasPrefix(path, "/api/v3/"):
		return s.rest(req, method, path, origin)
	case path == "/api/graphql" || path == "/graphql":
		return s.graphql(req, method, origin)
	case path == "/mcp" || path == "/mcp/":
		return s.mcp(req, method, origin)
	}
	if m := gitPath.FindStringSubmatch(path); m != nil {
		return s.git(req, method, m)
	}
	return jsonResponse(404, map[string]any{"message": "Not Found", "documentation_url": docsURL})
}

const docsURL = "https://docs.github.com/rest"

func apiResponse(r *ApiResponse) *Response {
	h := append(append([][2]string{}, apiHeaders...), r.Headers...)
	if r.NoBody {
		ct := jsonType
		if r.Status == 204 {
			ct = ""
		}
		return respond(r.Status, ct, nil, h...)
	}
	return respond(r.Status, jsonType, marshal(r.Body), h...)
}

func (s *Server) rest(req *Request, method, path, origin string) *Response {
	switch s.auth(req) {
	case "missing":
		return apiResponse(&ApiResponse{Status: 401, Body: map[string]any{"message": "Requires authentication", "documentation_url": docsURL, "status": "401"}})
	case "bad":
		return apiResponse(&ApiResponse{Status: 401, Body: map[string]any{"message": "Bad credentials", "documentation_url": docsURL, "status": "401"}})
	}
	var body any
	if len(bytes.TrimSpace(req.Body)) > 0 {
		dec := json.NewDecoder(bytes.NewReader(req.Body))
		dec.UseNumber()
		if err := dec.Decode(&body); err != nil || dec.More() {
			return apiResponse(&ApiResponse{Status: 400, Body: map[string]any{"message": "Problems parsing JSON", "documentation_url": docsURL, "status": "400"}})
		}
		body = normalizeNumbers(body)
		if method != "GET" && method != "DELETE" {
			if _, ok := body.(map[string]any); !ok {
				if _, isList := body.([]any); !isList {
					return apiResponse(&ApiResponse{Status: 400, Body: map[string]any{"message": "Body should be a JSON object", "documentation_url": docsURL, "status": "400"}})
				}
			}
		}
	}
	var segments []string
	for _, seg := range strings.Split(strings.TrimPrefix(path, "/api/v3"), "/") {
		if seg == "" {
			continue
		}
		d, err := url.PathUnescape(seg)
		if err != nil {
			return apiResponse(notFound("Not Found"))
		}
		segments = append(segments, d)
	}
	return apiResponse(handleRest(s.forge, &ApiRequest{Method: method, Segments: segments, Query: req.Query, Body: body, Origin: origin}))
}

// normalizeNumbers turns json.Number into int64 when integral, else float64.
func normalizeNumbers(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, val := range x {
			x[k] = normalizeNumbers(val)
		}
		return x
	case []any:
		for i, val := range x {
			x[i] = normalizeNumbers(val)
		}
		return x
	}
	return v
}

func (s *Server) graphql(req *Request, method, origin string) *Response {
	switch s.auth(req) {
	case "missing":
		return jsonResponse(401, map[string]any{"message": "This endpoint requires you to be authenticated.", "documentation_url": "https://docs.github.com/graphql/guides/forming-calls-with-graphql#authenticating-with-graphql"}, [2]string{"WWW-Authenticate", `Bearer realm="forge"`})
	case "bad":
		return jsonResponse(401, map[string]any{"message": "Bad credentials"}, [2]string{"WWW-Authenticate", `Bearer realm="forge"`})
	}
	var payload any
	if method == "GET" {
		p := map[string]any{"query": req.Query.Get("query"), "operationName": req.Query.Get("operationName")}
		if v := req.Query.Get("variables"); v != "" {
			var vars any
			if err := json.Unmarshal([]byte(v), &vars); err != nil {
				return jsonError(400, "Problems parsing JSON")
			}
			p["variables"] = vars
		}
		payload = p
	} else if method == "POST" {
		dec := json.NewDecoder(bytes.NewReader(req.Body))
		dec.UseNumber()
		if err := dec.Decode(&payload); err != nil {
			return jsonError(400, "Problems parsing JSON")
		}
		payload = normalizeNumbers(payload)
	} else {
		return jsonError(404, "Not Found")
	}
	return jsonResponse(200, handleGraphQL(s.forge, payload, origin), [2]string{"X-OAuth-Scopes", apiHeaders[0][1]})
}

func (s *Server) git(req *Request, method string, m []string) *Response {
	owner, err1 := url.PathUnescape(m[1])
	name, err2 := url.PathUnescape(m[2])
	if err1 != nil || err2 != nil {
		return jsonError(404, "Repository not found")
	}
	switch s.auth(req) {
	case "missing":
		return jsonResponse(401, map[string]any{"message": "Requires authentication"}, [2]string{"WWW-Authenticate", `Basic realm="forge"`})
	case "bad":
		return jsonResponse(401, map[string]any{"message": "Bad credentials"}, [2]string{"WWW-Authenticate", `Basic realm="forge"`})
	}
	repo := s.forge.GetRepo(owner, name)
	if repo == nil {
		return jsonError(404, "Repository not found")
	}
	v2 := strings.Contains(req.Header["git-protocol"], "version=2")
	body := req.Body
	if enc := strings.ToLower(req.Header["content-encoding"]); enc == "gzip" || enc == "x-gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return jsonError(400, "bad gzip body")
		}
		body, err = io.ReadAll(zr)
		if err != nil {
			return jsonError(400, "bad gzip body")
		}
	}
	switch {
	case m[3] == "info/refs" && method == "GET":
		service := req.Query.Get("service")
		if service != "git-upload-pack" && service != "git-receive-pack" {
			return jsonError(403, "dumb HTTP is not served; use the smart protocol")
		}
		return respond(200, "application/x-"+service+"-advertisement", advertise(s.forge, repo, service, v2), noCache...)
	case m[3] == "git-upload-pack" && method == "POST":
		out, err := uploadPack(s.forge, repo, body, v2)
		if err != nil {
			return jsonError(400, err.Error())
		}
		return respond(200, "application/x-git-upload-pack-result", out, noCache...)
	case m[3] == "git-receive-pack" && method == "POST":
		out, err := receivePack(s.forge, repo, body)
		if err != nil {
			return jsonError(400, err.Error())
		}
		return respond(200, "application/x-git-receive-pack-result", out, noCache...)
	}
	return jsonError(405, "Method Not Allowed")
}
