package prog_test

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/prog"
	"github.com/yanang007/gocached/internal/tracectx"
	"github.com/yanang007/gocached/internal/tracing"
)

const ciTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// knownAction is the action ID 00 01 02 ... 1f. Its go build ID form,
// worked by hand from cmd/internal/buildid.HashToString (base64url of the
// first 15 bytes), is knownActionString.
var knownAction = func() []byte {
	id := make([]byte, 32)
	for i := range id {
		id[i] = byte(i)
	}
	return id
}()

const knownActionString = "AAECAwQFBgcICQoLDA0O"

func parseTP(t *testing.T, tp string) trace.SpanContext {
	t.Helper()
	sc, err := tracectx.FromEnviron([]string{"TRACEPARENT=" + tp})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// traced returns opts set to record prog spans into a fresh session dir
// under the CI span ciTraceparent, and that dir.
func traced(t *testing.T, opts prog.Options, skip ...string) (prog.Options, string) {
	t.Helper()
	dir := t.TempDir()
	environ := []string{"GOCACHED_TRACE_SESSION_DIR=" + dir, "OTEL_SERVICE_NAME=ci"}
	for _, s := range skip {
		environ = append(environ, "GOCACHED_TRACE_SKIP="+s)
	}
	cfg, err := tracing.ConfigFromEnviron(environ, nil)
	if err != nil {
		t.Fatal(err)
	}
	opts.Trace = cfg
	opts.TraceParent = parseTP(t, ciTraceparent)
	return opts, dir
}

// spooled returns every span spooled in session dir dir.
func spooled(t *testing.T, dir string) []tracing.Span {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var spans []tracing.Span
	for _, e := range entries {
		f, err := os.Open(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s, err := tracing.ReadOTLP(f)
		f.Close()
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		spans = append(spans, s...)
	}
	return spans
}

func named(spans []tracing.Span, name string) []tracing.Span {
	var out []tracing.Span
	for _, s := range spans {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}

// gets returns the "cache get" spans whose gocached.cache.result is result.
func gets(spans []tracing.Span, result string) []tracing.Span {
	var out []tracing.Span
	for _, s := range named(spans, "cache get") {
		if attr(s, "gocached.cache.result") == result {
			out = append(out, s)
		}
	}
	return out
}

func attr(s tracing.Span, key string) string {
	for _, a := range s.Attrs {
		if a.Key == key {
			return a.Value
		}
	}
	return ""
}

// get sends a get for actionID and returns its response.
func (s *session) get(actionID []byte) response {
	s.t.Helper()
	s.nextID++
	s.send(map[string]any{"ID": s.nextID, "Command": "get", "ActionID": actionID})
	return s.read()
}

func TestMissIsASpanUnderTheSessionSpan(t *testing.T) {
	opts, dir := traced(t, prog.Options{Chain: newChain(t, &kindRemote{})})
	s := start(t, opts)
	if r := s.get(knownAction); r.Err != "" || r.DiskPath != "" {
		t.Fatalf("want a miss, got %+v", r)
	}
	s.close()

	spans := spooled(t, dir)
	sessions := named(spans, "gocacheprog")
	if len(sessions) != 1 {
		t.Fatalf("want one gocacheprog session span, got %v", spans)
	}
	sess := sessions[0]
	ci := parseTP(t, ciTraceparent)
	if sess.TraceID != ci.TraceID() || sess.ParentSpanID != ci.SpanID() || sess.Service != "ci" {
		t.Errorf("session span should be a ci child of TRACEPARENT, got %+v", sess)
	}
	misses := gets(spans, "miss")
	if len(misses) != 1 {
		t.Fatalf("want one miss span, got %v", spans)
	}
	m := misses[0]
	if m.ParentSpanID != sess.SpanID || m.TraceID != sess.TraceID {
		t.Errorf("miss span should be a child of the session span, got %+v", m)
	}
	if got := attr(m, "gocached.action_id"); got != knownActionString {
		t.Errorf("gocached.action_id = %q, want %q", got, knownActionString)
	}
	if m.Start.Before(sess.Start) || m.End.After(sess.End) || m.End.Before(m.Start) {
		t.Errorf("miss span %v..%v should lie within the session %v..%v", m.Start, m.End, sess.Start, sess.End)
	}
}

func TestHitSpansNameTheBackendThatHit(t *testing.T) {
	remote, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Warm the remote through a first, untraced session.
	warm := start(t, prog.Options{Chain: newChain(t, remote)})
	id := warm.put("!<arch>\nwarm")
	warm.read()
	warm.close()
	action := sha256.Sum256([]byte(fmt.Sprint("action", id))) // as session.put derives it

	opts, dir := traced(t, prog.Options{Chain: newChain(t, remote)})
	s := start(t, opts)
	if r := s.get(action[:]); r.DiskPath == "" {
		t.Fatalf("want a remote hit, got %+v", r)
	}
	if r := s.get(action[:]); r.DiskPath == "" {
		t.Fatalf("want a local hit, got %+v", r)
	}
	s.close()

	hits := gets(spooled(t, dir), "hit")
	if len(hits) != 2 {
		t.Fatalf("want two hit spans, got %v", hits)
	}
	var backends []string
	for _, h := range hits {
		backends = append(backends, attr(h, "gocached.backend"))
	}
	slices.Sort(backends)
	if !slices.Equal(backends, []string{"local", "remote"}) {
		t.Errorf("hit backends %v, want local and remote", backends)
	}
}

func TestTraceSkipDropsListedProgSpans(t *testing.T) {
	opts, dir := traced(t, prog.Options{Chain: newChain(t, &kindRemote{})}, "cache.get.miss,cache.upload")
	s := start(t, opts)
	s.get(knownAction)
	s.put("!<arch>\nuploaded")
	s.read()
	s.close()

	spans := spooled(t, dir)
	if n := len(named(spans, "cache get")) + len(named(spans, "cache upload")); n != 0 {
		t.Errorf("skipped spans were recorded: %v", spans)
	}
	if len(named(spans, "cache put")) != 1 || len(named(spans, "gocacheprog")) != 1 {
		t.Errorf("spans whose skip keys are not listed should still be recorded, got %v", spans)
	}

	opts, dir = traced(t, prog.Options{Chain: newChain(t, &kindRemote{})}, "cache.session,cache.get.hit")
	s = start(t, opts)
	s.get(knownAction)
	s.close()
	spans = spooled(t, dir)
	if len(named(spans, "gocacheprog")) != 0 || len(gets(spans, "miss")) != 1 {
		t.Errorf("skipping cache.session,cache.get.hit: got %v, want only the miss", spans)
	}
}

func TestTracingFailureKeepsProtocolAndStatsLine(t *testing.T) {
	opts, _ := traced(t, prog.Options{Chain: newChain(t, &kindRemote{})})
	// A regular file where the session dir should be: spooling must fail.
	notDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notDir, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	opts.Trace.SessionDir = filepath.Join(notDir, "session")
	s := start(t, opts)
	if r := s.get(knownAction); r.Err != "" {
		t.Fatalf("get: %+v", r)
	}
	id := s.put("!<arch>\nobject")
	if r := s.read(); r.ID != id || r.Err != "" || r.DiskPath == "" {
		t.Fatalf("put: %+v", r)
	}
	stats := s.close()
	if stats["get"] != "1" || stats["miss"] != "1" || stats["put"] != "1" || stats["upload_ok"] != "1" {
		t.Fatalf("stats %v", stats)
	}
	if !strings.Contains(s.stderr.String(), "gocached trace: warning") {
		t.Errorf("want a trace warning on stderr, got:\n%s", s.stderr.String())
	}
}

func TestWithoutTraceparentNothingIsSpooled(t *testing.T) {
	opts, dir := traced(t, prog.Options{Chain: newChain(t, &kindRemote{})})
	opts.TraceParent = trace.SpanContext{}
	s := start(t, opts)
	s.get(knownAction)
	s.put("!<arch>\nobject")
	s.read()
	s.close()
	if spans := spooled(t, dir); len(spans) != 0 {
		t.Errorf("spooled %v without a TRACEPARENT", spans)
	}
	if strings.Contains(s.stderr.String(), "trace") {
		t.Errorf("stderr mentions tracing:\n%s", s.stderr.String())
	}
}

func TestPutAndUploadSpansCarryKindSizeAndResult(t *testing.T) {
	skip, _ := kind.Parse("index")
	opts, dir := traced(t, prog.Options{Chain: newChain(t, failingRemote{}), Skip: skip})
	s := start(t, opts)
	s.put("!<arch>\nfails to upload") // 8 + 15 = 23 bytes
	s.read()
	s.put(indexBody) // skipped by config, never queued
	s.read()
	stats := s.close()
	if stats["upload_fail"] != "1" || stats["skip"] != "1" {
		t.Fatalf("stats %v", stats)
	}

	spans := spooled(t, dir)
	puts := named(spans, "cache put")
	if len(puts) != 2 {
		t.Fatalf("want two put spans, got %v", spans)
	}
	byKind := map[string]tracing.Span{}
	for _, p := range puts {
		byKind[attr(p, "gocached.kind")] = p
	}
	if a := byKind["archive"]; attr(a, "gocached.size") != "23" || attr(a, "gocached.action_id") == "" {
		t.Errorf("archive put span %+v", a)
	}
	if attr(byKind["index"], "gocached.upload") != "skipped" {
		t.Errorf("index put span should say it is not uploaded, got %+v", byKind["index"])
	}
	ups := named(spans, "cache upload")
	if len(ups) != 1 {
		t.Fatalf("want one upload span, got %v", spans)
	}
	u := ups[0]
	if attr(u, "gocached.upload") != "failed" || u.Status.Code != tracing.StatusError || attr(u, "gocached.kind") != "archive" {
		t.Errorf("upload span should record the failure, got %+v", u)
	}
	if attr(u, "gocached.action_id") != attr(byKind["archive"], "gocached.action_id") {
		t.Errorf("upload and put spans should share the action ID")
	}
}
