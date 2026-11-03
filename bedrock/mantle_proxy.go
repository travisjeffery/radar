package bedrock

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/travisjeffery/radar"
)

const mantlePathPrefix = "/openai/v1/"

// droppedRequestHeaders are never forwarded: hop-by-hop headers, the ones
// the client recomputes, Codex's throwaway signature, and Accept-Encoding,
// so Go's transport negotiates and decodes compression itself and the usage
// tap reads plain events.
var droppedRequestHeaders = map[string]bool{
	"Authorization":       true,
	"Connection":          true,
	"Content-Length":      true,
	"Host":                true,
	"Keep-Alive":          true,
	"Proxy-Authorization": true,
	"Transfer-Encoding":   true,
	"Upgrade":             true,
	"Accept-Encoding":     true,
}

// mantleProxy forwards Codex's Responses requests to Bedrock Mantle, signed
// by the client's transport, with prompt_cache_key pinned, and sums the
// token usage each completed response reports. It listens on loopback only.
type mantleProxy struct {
	listener net.Listener
	server   *http.Server
	upstream string
	cacheKey string
	client   *http.Client

	mu    sync.Mutex
	total *radar.ReviewUsage
	// primed is set once the run's priming request has been tried, and
	// primeUsage holds what it cost.
	primed     bool
	primeUsage *radar.ReviewUsage
	// What Codex's requests show of its tool use, for measurement:
	// each call's tool by call_id, and its output's size. A
	// request resends the conversation, so call_id keeps a call once.
	prefix  string
	calls   map[string]string
	outputs map[string]int
}

func startMantleProxy(upstream, cacheKey string, client *http.Client) (*mantleProxy, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	p := &mantleProxy{listener: ln, upstream: strings.TrimSuffix(upstream, "/"), cacheKey: cacheKey, client: client}
	p.server = &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = p.server.Serve(ln) }()
	return p, nil
}

func (p *mantleProxy) baseURL() string {
	return "http://" + p.listener.Addr().String() + strings.TrimSuffix(mantlePathPrefix, "/")
}

func (p *mantleProxy) close() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = p.server.Shutdown(ctx)
}

// usage is the run's usage, the priming request's included. FirstRequest
// stays the first request Codex sent, so it shows what that read.
func (p *mantleProxy) usage() *radar.ReviewUsage {
	p.mu.Lock()
	defer p.mu.Unlock()
	var u *radar.ReviewUsage
	if p.total != nil {
		c := *p.total
		u = &c
	}
	if p.primeUsage != nil {
		var first *radar.RequestUsage
		if u != nil {
			first = u.FirstRequest
		}
		u = u.Add(p.primeUsage)
		u.FirstRequest = first
	}
	return u
}

func (p *mantleProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.URL.Path, mantlePathPrefix) {
		http.NotFound(w, r)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "reading request: "+err.Error(), http.StatusBadRequest)
		return
	}
	encoded := r.Header.Get("Content-Encoding") != "" && !strings.EqualFold(r.Header.Get("Content-Encoding"), "identity")
	if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/responses") && !encoded {
		p.observe(body)
		body = pinCacheKey(body, p.cacheKey)
		p.prime(r, body)
	}
	target := p.upstream + r.URL.Path
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	forwardHeaders(out.Header, r.Header)
	resp, err := p.client.Do(out)
	if err != nil {
		http.Error(w, "bedrock proxy: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	for name, values := range resp.Header {
		if name == "Content-Length" {
			continue
		}
		w.Header()[name] = values
	}
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	tap := &usageTap{onUsage: p.add}
	buf := make([]byte, 32*1024)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			tap.feed(buf[:n])
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// forwardHeaders copies Codex's request headers to an upstream request,
// less the ones never forwarded.
func forwardHeaders(dst, src http.Header) {
	for name, values := range src {
		if droppedRequestHeaders[http.CanonicalHeaderKey(name)] || strings.HasPrefix(strings.ToLower(name), "x-amz-") {
			continue
		}
		dst[name] = values
	}
}

const (
	// primeOutputTokens caps the priming request's output; the Responses
	// API takes no less.
	primeOutputTokens = 16
	// primeTimeout bounds the priming request; the run goes on without it.
	primeTimeout = 60 * time.Second
)

// prime sends, before a run's first Responses request, that request cut to
// its leading developer and system messages, Codex's and Radar's
// instructions, which every run shares. Mantle caches a request's input as
// of its end, and a later request reads only a cached input it extends:
// each request in a run extends the one before, but no run's first request
// extends another run's, so without a checkpoint at the shared boundary a
// run's first request reads nothing from the cache. Priming is
// best effort: on any failure the run goes on unprimed.
func (p *mantleProxy) prime(r *http.Request, body []byte) {
	p.mu.Lock()
	primed := p.primed
	p.primed = true
	p.mu.Unlock()
	if primed {
		return
	}
	primeBody := primeRequest(body)
	if primeBody == nil {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), primeTimeout)
	defer cancel()
	out, err := http.NewRequestWithContext(ctx, http.MethodPost, p.upstream+r.URL.Path, bytes.NewReader(primeBody))
	if err != nil {
		return
	}
	forwardHeaders(out.Header, r.Header)
	out.Header.Set("Accept", "application/json")
	resp, err := p.client.Do(out)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	var res struct {
		Usage *responsesUsage `json:"usage"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&res) != nil || res.Usage == nil {
		return
	}
	p.mu.Lock()
	p.primeUsage = res.Usage.review()
	p.mu.Unlock()
}

// primeRequest is a Responses request cut to its leading developer and
// system messages, unstreamed and with the least output, or nil when the
// request has no such messages or nothing after them.
func primeRequest(body []byte) []byte {
	var req map[string]json.RawMessage
	if json.Unmarshal(body, &req) != nil || req == nil {
		return nil
	}
	var input []json.RawMessage
	if json.Unmarshal(req["input"], &input) != nil {
		return nil
	}
	n := 0
	for _, raw := range input {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		if json.Unmarshal(raw, &item) != nil || (item.Type != "" && item.Type != "message") || (item.Role != "developer" && item.Role != "system") {
			break
		}
		n++
	}
	if n == 0 || n == len(input) {
		return nil
	}
	req["input"], _ = json.Marshal(input[:n])
	req["max_output_tokens"], _ = json.Marshal(primeOutputTokens)
	req["stream"] = json.RawMessage("false")
	out, err := json.Marshal(req)
	if err != nil {
		return nil
	}
	return out
}

func (p *mantleProxy) add(u *radar.ReviewUsage) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if u.PrefixSHA256 == "" {
		u.PrefixSHA256 = p.prefix
	}
	p.total = p.total.Add(u)
}

// observe notes, from a Responses request Codex sends, the hash of its
// prefix (instructions and tools) and the tool calls and outputs its input
// carries. It never changes the request.
func (p *mantleProxy) observe(body []byte) {
	var req struct {
		Instructions string            `json:"instructions"`
		Tools        json.RawMessage   `json:"tools"`
		Input        []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(body, &req) != nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.prefix == "" {
		h := sha256.New()
		fmt.Fprintf(h, "%d:%s%d:%s", len(req.Instructions), req.Instructions, len(req.Tools), req.Tools)
		p.prefix = hex.EncodeToString(h.Sum(nil))
	}
	for _, raw := range req.Input {
		var item struct {
			Type      string          `json:"type"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments string          `json:"arguments"`
			Input     string          `json:"input"`
			Action    json.RawMessage `json:"action"`
			Output    json.RawMessage `json:"output"`
		}
		if json.Unmarshal(raw, &item) != nil || item.CallID == "" {
			continue
		}
		switch item.Type {
		case "function_call", "custom_tool_call", "local_shell_call":
			if p.calls == nil {
				p.calls = map[string]string{}
			}
			if _, seen := p.calls[item.CallID]; !seen {
				p.calls[item.CallID] = codexToolClass(item.Name, item.Arguments, item.Action)
			}
		case "function_call_output", "custom_tool_call_output", "local_shell_call_output":
			if p.outputs == nil {
				p.outputs = map[string]int{}
			}
			var text string
			if json.Unmarshal(item.Output, &text) != nil {
				text = string(item.Output)
			}
			p.outputs[item.CallID] = len(text)
		}
	}
}

// tools sums the observed calls by class, with their outputs' sizes.
func (p *mantleProxy) tools() map[string]radar.ToolUse {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.calls) == 0 {
		return nil
	}
	out := map[string]radar.ToolUse{}
	for id, class := range p.calls {
		t := out[class]
		t.Calls++
		t.Bytes += p.outputs[id]
		out[class] = t
	}
	return out
}

// codexToolClass names a Codex tool call for measurement: a shell command
// as shell:<program> (shell:rg, shell:sed), anything else by its tool name.
func codexToolClass(name, arguments string, action json.RawMessage) string {
	var args struct {
		Command json.RawMessage `json:"command"`
		Cmd     string          `json:"cmd"`
	}
	if len(action) > 0 {
		_ = json.Unmarshal(action, &args)
	} else {
		_ = json.Unmarshal([]byte(arguments), &args)
	}
	script := args.Cmd
	var argv []string
	if json.Unmarshal(args.Command, &argv) == nil && len(argv) > 0 {
		script = strings.Join(argv, " ")
		// bash -lc '<script>': the script names the program.
		if len(argv) == 3 && (argv[1] == "-lc" || argv[1] == "-c") {
			script = argv[2]
		}
	} else if s := ""; json.Unmarshal(args.Command, &s) == nil && s != "" {
		script = s
	}
	if prog := shellProgram(script); prog != "" {
		return "shell:" + prog
	}
	if name == "" {
		return "unknown"
	}
	return name
}

// shellProgram is the first program a script runs, past a leading cd and
// variable assignments.
func shellProgram(script string) string {
	for _, segment := range strings.Split(script, "&&") {
		fields := strings.Fields(segment)
		for len(fields) > 0 && strings.Contains(fields[0], "=") && !strings.HasPrefix(fields[0], "=") {
			fields = fields[1:]
		}
		if len(fields) == 0 || fields[0] == "cd" {
			continue
		}
		return path.Base(fields[0])
	}
	return ""
}

// pinCacheKey sets prompt_cache_key on a Responses request. Codex sends its
// per-run session id there, and the key partitions the prompt cache, so
// without this every review pays to write its whole prefix and reads none of
// it back. A body that is not a JSON object passes unchanged.
func pinCacheKey(body []byte, key string) []byte {
	if key == "" {
		return body
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil || req == nil {
		return body
	}
	quoted, _ := json.Marshal(key)
	req["prompt_cache_key"] = quoted
	out, err := json.Marshal(req)
	if err != nil {
		return body
	}
	return out
}

// usageTap reads server-sent events and reports the usage of each
// response.completed event.
type usageTap struct {
	pending []byte
	onUsage func(*radar.ReviewUsage)
}

func (t *usageTap) feed(chunk []byte) {
	t.pending = append(t.pending, chunk...)
	for {
		i := bytes.IndexByte(t.pending, '\n')
		if i < 0 {
			return
		}
		line := t.pending[:i]
		t.pending = t.pending[i+1:]
		t.line(line)
	}
}

func (t *usageTap) line(line []byte) {
	data, ok := bytes.CutPrefix(bytes.TrimRight(line, "\r"), []byte("data:"))
	if !ok || !bytes.Contains(data, []byte(`"response.completed"`)) {
		return
	}
	var event struct {
		Type     string `json:"type"`
		Response struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(data), &event); err != nil || event.Type != "response.completed" || event.Response.Usage == nil {
		return
	}
	t.onUsage(event.Response.Usage.review())
}

// responsesUsage is a Responses API response's usage.
type responsesUsage struct {
	InputTokens        int64 `json:"input_tokens"`
	OutputTokens       int64 `json:"output_tokens"`
	InputTokensDetails struct {
		CachedTokens     int64 `json:"cached_tokens"`
		CacheWriteTokens int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
}

// review is one request's usage as Radar records it.
func (u *responsesUsage) review() *radar.ReviewUsage {
	return &radar.ReviewUsage{
		InputTokens: u.InputTokens, OutputTokens: u.OutputTokens,
		CacheReadTokens: u.InputTokensDetails.CachedTokens, CacheWriteTokens: u.InputTokensDetails.CacheWriteTokens,
		Requests: 1,
	}
}
