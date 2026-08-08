package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sltbrta/sentra-code-memory-v2/services/brain/internal/hosted"
)

func main() {
	docsPath := flag.String("docs-jsonl", "", "legacy in-memory JSONL corpus (fixture only; not full ERB)")
	sourceID := flag.String("source-id", "eval-src", "companydoc source id (jsonl mode)")
	generationID := flag.String("generation-id", "eval-gen", "generation id (jsonl mode)")
	topK := flag.Int("top-k", 8, "final context-window size after CE/retain (tight)")
	hostedFlag := flag.Bool("hosted", false, "force Neon+Qdrant product hosted path")
	// hosted-loop: load Client+HotLex once, answer many JSONL cases (Modal warm path).
	// Without this every Modal map invocation reloads ~1.5GB hotlex gob.
	loopFlag := flag.Bool("hosted-loop", false, "JSONL multi-case loop; keep HotLex+Neon client warm")
	flag.Parse()

	if *docsPath == "" {
		*docsPath = os.Getenv("OUROBOROS_PRODUCT_BRAIN_DOCS_JSONL")
	}

	if *loopFlag {
		runHostedLoop(*topK, *hostedFlag)
		return
	}

	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		writeErr(EvalResult{
			Failure:    fmt.Sprintf("product_brain_stdin:%v", err),
			SearchMode: "product_brain_go",
		})
		os.Exit(2)
	}
	raw = trimSpaceBytes(raw)
	if len(raw) == 0 {
		writeErr(EvalResult{
			Failure:    "product_brain_empty_stdin",
			SearchMode: "product_brain_go",
		})
		os.Exit(2)
	}

	var c EvalCase
	if err := json.Unmarshal(raw, &c); err != nil {
		writeErr(EvalResult{
			Failure:    fmt.Sprintf("product_brain_bad_case_json:%v", err),
			SearchMode: "product_brain_go",
		})
		os.Exit(2)
	}
	if c.Question == "" {
		writeErr(EvalResult{
			Failure:    "product_brain_empty_question",
			SearchMode: "product_brain_go",
		})
		os.Exit(2)
	}

	engine, err := openEngine(c, *docsPath, *sourceID, *generationID, *topK, *hostedFlag)
	if err != nil {
		writeErr(EvalResult{
			Failure:    err.Error(),
			SearchMode: "product_brain_go_hosted",
			RetrievalDiagnostics: map[string]any{
				"source": "product_brain_eval",
				"status": "open_error",
				"error":  err.Error(),
			},
		})
		os.Exit(2)
	}
	defer engine.Close()

	result := engine.Answer(c)
	stampHotLex(result)
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	if err := encodeLine(enc, result); err != nil {
		fmt.Fprintf(os.Stderr, "product-brain-eval: encode: %v\n", err)
		os.Exit(2)
	}
}

// runHostedLoop keeps one OpenFromEnv client (HotLex gob loaded once) and answers
// one JSON object per stdin line until EOF. Protocol: request line → response line.
func runHostedLoop(topK int, hostedFlag bool) {
	tOpen := time.Now()
	engine, err := LoadHosted(topK)
	if err != nil {
		// still emit one error line so callers don't hang
		writeErr(EvalResult{
			Failure:    fmt.Sprintf("product_brain_hosted_open:%v", err),
			SearchMode: "product_brain_go_hosted",
			RetrievalDiagnostics: map[string]any{
				"source": "product_brain_eval",
				"status": "hosted_open_error",
				"error":  err.Error(),
				"loop":   true,
			},
		})
		os.Exit(2)
	}
	defer engine.Close()
	openMS := time.Since(tOpen).Milliseconds()
	hotN := 0
	if engine.Hosted != nil && engine.Hosted.HotLex() != nil {
		hotN = engine.Hosted.HotLex().Len()
	}
	fmt.Fprintf(os.Stderr, "product-brain-eval: hosted-loop open_ms=%d hot_lex_docs=%d\n", openMS, hotN)

	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	// Readiness is a protocol line, not a request response. The Python side
	// waits for it before sending work so cold HotLex startup never consumes the
	// first query's execution budget or causes a cold-start death loop.
	if err := encodeLine(enc, EvalResult{
		SearchMode: "product_brain_go_hosted_loop_ready",
		RetrievalDiagnostics: map[string]any{
			"status":       "ready",
			"loop_open_ms": openMS,
			"hot_lex_docs": hotN,
		},
	}); err != nil {
		fmt.Fprintf(os.Stderr, "product-brain-eval: encode ready: %v\n", err)
		os.Exit(2)
	}

	n, err := runLoopIO(engine, os.Stdin, os.Stdout, openMS)
	if err != nil {
		fmt.Fprintf(os.Stderr, "product-brain-eval: hosted-loop: %v\n", err)
		os.Exit(2)
	}
	fmt.Fprintf(os.Stderr, "product-brain-eval: hosted-loop done n=%d\n", n)
}

// runLoopIO is the hosted-loop request/response body, extracted so tests can
// drive the framing contract over in-memory pipes without hosted env. One
// framed response line per non-empty request line, in strict order; the count
// of handled lines is returned. Framing (issue #292): a v1 request's
// request_id is echoed on every response path — answer, validation error,
// version mismatch, and best-effort probe of a malformed line — so no stale
// or foreign frame can satisfy a later request.
func runLoopIO(engine *Engine, in io.Reader, out io.Writer, openMS int64) (int, error) {
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	sc := bufio.NewScanner(in)
	// ERB cases can be large with gold fields; 4MB line buffer.
	buf := make([]byte, 0, 1024*64)
	sc.Buffer(buf, 4*1024*1024)
	hotN := 0
	if engine.Hosted != nil && engine.Hosted.HotLex() != nil {
		hotN = engine.Hosted.HotLex().Len()
	}
	memTopK := engine.TopK
	if memTopK <= 0 {
		memTopK = 8
	}
	n := 0
	// Per-ask wall: must finish before Python's read wall or the late line
	// desynchronizes the next request on this warm loop. The request may also
	// carry ask_timeout_ms so Python can reserve the already-paid queue/startup
	// budget. askTimeoutMS/modalAskWallMS remain the process defaults.
	askMS := askTimeoutMS()
	wallMS := modalAskWallMS()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		n++
		var c EvalCase
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			if err := encodeLineTo(enc, out, EvalResult{
				Failure:    fmt.Sprintf("product_brain_bad_case_json:%v", err),
				SearchMode: "product_brain_go_hosted",
				RequestID:  probeRequestID(line),
			}); err != nil {
				return n, err
			}
			continue
		}
		if perr := protocolVersionError(c); perr != nil {
			if err := encodeLineTo(enc, out, *perr); err != nil {
				return n, err
			}
			continue
		}
		if c.Question == "" {
			if err := encodeLineTo(enc, out, EvalResult{
				Failure:    "product_brain_empty_question",
				SearchMode: "product_brain_go_hosted",
				RequestID:  c.RequestID,
			}); err != nil {
				return n, err
			}
			continue
		}
		// Memory-facts cases need a separate engine; rare on ERB path2.
		if len(c.MemoryFacts) > 0 {
			mem, merr := LoadMemoryFacts(c.MemoryFacts, "longmem-"+c.QuestionID, memTopK)
			if merr != nil {
				if err := encodeLineTo(enc, out, EvalResult{
					Failure:    fmt.Sprintf("product_brain_memory_facts:%v", merr),
					SearchMode: "product_brain_go_memory",
					RequestID:  c.RequestID,
				}); err != nil {
					return n, err
				}
				continue
			}
			res := mem.Answer(c)
			mem.Close()
			res.RequestID = c.RequestID
			if err := encodeLineTo(enc, out, res); err != nil {
				return n, err
			}
			continue
		}
		// Pass a context-aware answer through the loop. The old engine.Answer
		// path created its own background context, making the Go deadline inert
		// and forcing Python to kill the process after the wall elapsed.
		res := engine.AnswerContext(context.Background(), c)
		if res.RetrievalDiagnostics == nil {
			res.RetrievalDiagnostics = map[string]any{}
		}
		res.RetrievalDiagnostics["hosted_loop"] = true
		res.RetrievalDiagnostics["hosted_loop_open_ms"] = openMS
		res.RetrievalDiagnostics["hot_lex_docs"] = hotN
		if _, ok := res.RetrievalDiagnostics["ask_timeout_ms"]; !ok {
			res.RetrievalDiagnostics["ask_timeout_ms"] = askMS
		}
		if _, ok := res.RetrievalDiagnostics["modal_ask_wall_ms"]; !ok {
			res.RetrievalDiagnostics["modal_ask_wall_ms"] = wallMS
		}
		res.RequestID = c.RequestID
		stampHotLex(res)
		if err := encodeLineTo(enc, out, res); err != nil {
			return n, err
		}
	}
	return n, sc.Err()
}

// protocolVersionError returns a fail-closed framed error when framing is
// malformed, partial, or unsupported. Only a request with no framing fields
// (legacy v0) or a complete v1 pair may be answered.
func protocolVersionError(c EvalCase) *EvalResult {
	if c.protocolVersionPresent && !c.protocolVersionValid {
		return protocolVersionFailure(c, "invalid")
	}
	if c.ProtocolVersion == 0 {
		if c.RequestID == "" {
			return nil
		}
		return protocolVersionFailure(c, "missing")
	}
	if c.ProtocolVersion == HostedLoopProtocolVersion {
		if c.RequestID != "" {
			return nil
		}
		return protocolVersionFailure(c, "missing_request_id")
	}
	return protocolVersionFailure(c, strconv.Itoa(c.ProtocolVersion))
}

func protocolVersionFailure(c EvalCase, got string) *EvalResult {
	return &EvalResult{
		Failure:    fmt.Sprintf("product_brain_protocol_version:want=%d:got=%s", HostedLoopProtocolVersion, got),
		SearchMode: "product_brain_go_hosted",
		RequestID:  c.RequestID,
		RetrievalDiagnostics: map[string]any{
			"status":           "failure",
			"protocol_version": c.ProtocolVersion,
		},
	}
}

// probeRequestID best-effort extracts request_id from a line that failed the
// full EvalCase unmarshal (e.g. a type error on another field), so the
// fail-closed response still correlates to its request.
func probeRequestID(line string) string {
	var probe struct {
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal([]byte(line), &probe); err != nil {
		return ""
	}
	return probe.RequestID
}

func openEngine(c EvalCase, docsPath, sourceID, generationID string, topK int, hostedFlag bool) (*Engine, error) {
	if len(c.MemoryFacts) > 0 {
		return LoadMemoryFacts(c.MemoryFacts, "longmem-"+c.QuestionID, topK)
	}
	useHosted := hostedFlag || hosted.Enabled() || docsPath == ""
	if useHosted {
		return LoadHosted(topK)
	}
	engine, err := LoadDocsJSONL(docsPath, sourceID, generationID)
	if err != nil {
		return nil, fmt.Errorf("product_brain_load:%w", err)
	}
	engine.TopK = topK
	return engine, nil
}

func stampHotLex(result EvalResult) {
	if result.RetrievalDiagnostics == nil {
		return
	}
	// Surface missing hot so Modal misconfig is obvious in cells.
	if _, ok := result.RetrievalDiagnostics["hot_lex_missing"]; !ok {
		if _, ok2 := result.RetrievalDiagnostics["hot_lex_fused"]; !ok2 {
			if _, ok3 := result.RetrievalDiagnostics["hot_lex_hits"]; !ok3 {
				// interactive path stamps hot_lex_docs; residual may only fuse
			}
		}
	}
}

func writeErr(r EvalResult) {
	if r.RetrievalDiagnostics == nil {
		r.RetrievalDiagnostics = map[string]any{"source": "product_brain_eval"}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetEscapeHTML(false)
	_ = encodeLine(enc, r)
}

// encodeLine writes one JSON response and Syncs stdout so Modal's select/readline
// sees a complete line promptly (pipe-buffered stdout otherwise delayed desync).
func encodeLine(enc *json.Encoder, r EvalResult) error {
	return encodeLineTo(enc, os.Stdout, r)
}

// encodeLineTo stamps eval state plus wire framing (protocol version) and
// writes one framed response line, syncing when the sink is a file.
func encodeLineTo(enc *json.Encoder, out io.Writer, r EvalResult) error {
	stampEvalState(&r)
	stampWireFraming(&r)
	if err := enc.Encode(r); err != nil {
		return err
	}
	if f, ok := out.(*os.File); ok {
		_ = f.Sync()
	}
	return nil
}

// stampWireFraming stamps the wire protocol version on every response so a
// reader can reject mixed-mode peers fail-closed (issue #292).
func stampWireFraming(r *EvalResult) {
	r.ProtocolVersion = HostedLoopProtocolVersion
}

func trimSpaceBytes(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}
