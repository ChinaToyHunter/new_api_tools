package handler

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "modernc.org/sqlite"

	"github.com/new-api-tools/backend/internal/cache"
	"github.com/new-api-tools/backend/internal/database"
)

// ============================================================================
// Selected-models API regression tests
// ============================================================================

// respStub is a tiny RESP2 server good enough for the cache manager's SET/GET.
// It runs on a loopback listener so it can be reached through the real
// cache.Init(connString) path, and it exposes failure switches so the
// shared-store failure paths can be driven without touching production code.
type respStub struct {
	ln       net.Listener
	mu       sync.Mutex
	data     map[string][]byte
	failSet  bool
	failRead bool
}

func startRespStub(t *testing.T) *respStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &respStub{ln: ln, data: map[string][]byte{}}
	go s.acceptLoop()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

func (s *respStub) addr() string { return s.ln.Addr().String() }

func (s *respStub) setFailRead(v bool) {
	s.mu.Lock()
	s.failRead = v
	s.mu.Unlock()
}

func (s *respStub) setFailSet(v bool) {
	s.mu.Lock()
	s.failSet = v
	s.mu.Unlock()
}

func (s *respStub) acceptLoop() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.serve(conn)
	}
}

func (s *respStub) serve(conn net.Conn) {
	defer conn.Close()
	buf := make([]byte, 0, 512)
	tmp := make([]byte, 512)
	for {
		n, err := conn.Read(tmp)
		if n > 0 {
			buf = append(buf, tmp[:n]...)
			for {
				cmd, rest, ok := parseRESP(buf)
				if !ok {
					break
				}
				buf = rest
				if _, werr := conn.Write([]byte(s.exec(cmd))); werr != nil {
					return
				}
			}
		}
		if err != nil {
			return
		}
	}
}

// parseRESP splits one RESP2 array command off the front of buf.
func parseRESP(buf []byte) (cmd []string, rest []byte, ok bool) {
	if len(buf) == 0 || buf[0] != '*' {
		return nil, buf, false
	}
	i := 1
	j := i
	for j < len(buf) && buf[j] != '\r' {
		j++
	}
	if j >= len(buf) {
		return nil, buf, false
	}
	count := 0
	for k := i; k < j; k++ {
		if buf[k] < '0' || buf[k] > '9' {
			return nil, buf, false
		}
		count = count*10 + int(buf[k]-'0')
	}
	j += 2 // skip \r\n
	for c := 0; c < count; c++ {
		if j >= len(buf) || buf[j] != '$' {
			return nil, buf, false
		}
		j++
		start := j
		for j < len(buf) && buf[j] != '\r' {
			j++
		}
		if j >= len(buf) {
			return nil, buf, false
		}
		argLen := 0
		for k := start; k < j; k++ {
			argLen = argLen*10 + int(buf[k]-'0')
		}
		j += 2 // skip \r\n
		if j+argLen+2 > len(buf) {
			return nil, buf, false
		}
		cmd = append(cmd, string(buf[j:j+argLen]))
		j += argLen + 2 // skip payload + \r\n
	}
	return cmd, buf[j:], true
}

func (s *respStub) exec(cmd []string) string {
	if len(cmd) == 0 {
		return "-ERR empty\r\n"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	switch asciiUpper(cmd[0]) {
	case "HELLO":
		// go-redis negotiates RESP3 with HELLO and requires a map reply.
		// Flat (even-sized) key/value pairs are a valid RESP3 map.
		return "*4\r\n" +
			"$6\r\nserver\r\n$5\r\nredis\r\n" +
			"$7\r\nversion\r\n$5\r\n7.0.0\r\n"
	case "PING":
		return "+PONG\r\n"
	case "SET":
		if s.failSet {
			return "-ERR stub: set failed\r\n"
		}
		s.data[cmd[1]] = []byte(cmd[2])
		return "+OK\r\n"
	case "GET":
		if s.failRead {
			return "-ERR stub: get failed\r\n"
		}
		if v, ok := s.data[cmd[1]]; ok {
			return "$" + itoa(len(v)) + "\r\n" + string(v) + "\r\n"
		}
		return "$-1\r\n"
	case "DEL":
		delete(s.data, cmd[1])
		return ":1\r\n"
	default:
		return "+OK\r\n"
	}
}

func asciiUpper(s string) string {
	out := []byte(s)
	for i, c := range out {
		if c >= 'a' && c <= 'z' {
			out[i] = c - 32
		}
	}
	return string(out)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// installSelectionRouter wires the real model-status routes (admin + public
// embed) over a cache manager backed by the RESP stub, so the HTTP contract is
// exercised through the actual router and the real store path.
//
// Each case uses its own stub and restores the in-process cache afterwards.
func installSelectionRouter(t *testing.T) (*gin.Engine, *respStub) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	// NewModelStatusService() calls database.Get(); give it an in-memory DB.
	db, err := sqlx.Connect("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	database.SetForTesting(&database.Manager{DB: db, IsPG: false})
	t.Cleanup(func() { _ = db.Close() })

	// Point the cache at the stub through the production Init path.
	stub := startRespStub(t)
	if _, err := cache.Init(stub.addr()); err != nil {
		t.Fatalf("cache init against stub: %v", err)
	}
	t.Cleanup(func() {
		_ = cache.Close()
		cache.SetForTesting(nil)
	})

	// Start every case from "not set".
	if err := cache.Get().SetAuthoritative("model_status:selected_models", []string{}, 0); err != nil {
		t.Fatalf("reset selection: %v", err)
	}

	r := gin.New()
	api := r.Group("/api")
	RegisterModelStatusRoutes(api)
	RegisterModelStatusEmbedRoutes(r)
	return r, stub
}

// doJSON issues a request with a JSON body and returns the recorder.
func doJSON(t *testing.T, r *gin.Engine, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		payload, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(payload)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// selectionBody decodes a {success, data} response and asserts the shape.
type selectionBody struct {
	Success bool     `json:"success"`
	Data    []string `json:"data"`
	Error   struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
	TimeWindow string `json:"time_window"`
}

func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) selectionBody {
	t.Helper()
	var body selectionBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return body
}

// A successful save must be reported as successful and must return exactly what
// a subsequent GET returns — the response is the authority, not the request.
func TestSetSelectedModelsReturnsPersistedList(t *testing.T) {
	r, _ := installSelectionRouter(t)

	rec := doJSON(t, r, http.MethodPut, "/api/model-status/selected", map[string]interface{}{
		"models": []string{"model-a", "model-b"},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if !body.Success {
		t.Fatalf("expected success, body = %s", rec.Body.String())
	}
	if len(body.Data) != 2 || body.Data[0] != "model-a" || body.Data[1] != "model-b" {
		t.Fatalf("save response data = %v", body.Data)
	}

	// The admin GET must return the same list.
	recGet := httptest.NewRecorder()
	r.ServeHTTP(recGet, httptest.NewRequest(http.MethodGet, "/api/model-status/selected", nil))
	if recGet.Code != http.StatusOK {
		t.Fatalf("GET status = %d, body = %s", recGet.Code, recGet.Body.String())
	}
	got := decodeBody(t, recGet)
	if len(got.Data) != 2 || got.Data[0] != "model-a" {
		t.Fatalf("GET after save = %v", got.Data)
	}
}

// The public embed read and the admin read must agree. If they diverged, the
// operator would see the intended list while visitors see something else.
func TestPublicEmbedMatchesAdminSelection(t *testing.T) {
	r, _ := installSelectionRouter(t)

	want := []string{"pub-1", "pub-2", "pub-3"}
	// Both admin write routes must persist the same list.
	writes := []struct{ method, path string }{
		{http.MethodPut, "/api/model-status/selected"},
		{http.MethodPost, "/api/model-status/config/selected"},
	}
	for _, w := range writes {
		if rec := doJSON(t, r, w.method, w.path, map[string]interface{}{"models": want}); rec.Code != http.StatusOK {
			t.Fatalf("save via %s %s: status %d body %s", w.method, w.path, rec.Code, rec.Body.String())
		}
	}

	admin := httptest.NewRecorder()
	r.ServeHTTP(admin, httptest.NewRequest(http.MethodGet, "/api/model-status/config/selected", nil))
	if admin.Code != http.StatusOK {
		t.Fatalf("admin read status %d body %s", admin.Code, admin.Body.String())
	}
	adminBody := decodeBody(t, admin)

	pub := httptest.NewRecorder()
	r.ServeHTTP(pub, httptest.NewRequest(http.MethodGet, "/api/model-status/embed/config/selected", nil))
	if pub.Code != http.StatusOK {
		t.Fatalf("public read status %d body %s", pub.Code, pub.Body.String())
	}
	pubBody := decodeBody(t, pub)

	if len(adminBody.Data) != len(want) || len(pubBody.Data) != len(want) {
		t.Fatalf("admin=%v public=%v want=%v", adminBody.Data, pubBody.Data, want)
	}
	for i := range want {
		if adminBody.Data[i] != want[i] || pubBody.Data[i] != want[i] {
			t.Fatalf("index %d: admin=%v public=%v want=%v", i, adminBody.Data, pubBody.Data, want)
		}
	}

	// The embed config bundle must carry the same list.
	cfgRec := httptest.NewRecorder()
	r.ServeHTTP(cfgRec, httptest.NewRequest(http.MethodGet, "/api/model-status/embed/config", nil))
	if cfgRec.Code != http.StatusOK {
		t.Fatalf("embed config status %d body %s", cfgRec.Code, cfgRec.Body.String())
	}
	var bundle struct {
		Success bool `json:"success"`
		Data    struct {
			SelectedModels []string `json:"selected_models"`
		} `json:"data"`
	}
	if err := json.Unmarshal(cfgRec.Body.Bytes(), &bundle); err != nil {
		t.Fatalf("decode embed config: %v", err)
	}
	if !bundle.Success || len(bundle.Data.SelectedModels) != len(want) {
		t.Fatalf("embed config selection = %v", bundle.Data.SelectedModels)
	}
}

// An empty selection is legal: it must save cleanly and read back as an empty
// array, never as null and never as an inferred list.
func TestSetSelectedModelsAcceptsEmptyList(t *testing.T) {
	r, _ := installSelectionRouter(t)

	// Seed something first so the empty save is a real change, not a no-op.
	seeds := []struct{ method, path string }{
		{http.MethodPut, "/api/model-status/selected"},
		{http.MethodPost, "/api/model-status/config/selected"},
	}
	for _, s := range seeds {
		seed := doJSON(t, r, s.method, s.path, map[string]interface{}{"models": []string{"would-be-removed"}})
		if seed.Code != http.StatusOK {
			t.Fatalf("seed via %s %s: status %d body %s", s.method, s.path, seed.Code, seed.Body.String())
		}
		if !decodeBody(t, seed).Success {
			t.Fatalf("seed via %s body = %s", s.path, seed.Body.String())
		}
	}

	rec := doJSON(t, r, http.MethodPost, "/api/model-status/config/selected", map[string]interface{}{
		"models": []string{},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty save status %d body %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if !body.Success {
		t.Fatalf("empty save should succeed, body = %s", rec.Body.String())
	}
	if body.Data == nil {
		t.Fatal("empty selection must encode as [], got null")
	}
	if len(body.Data) != 0 {
		t.Fatalf("empty selection returned %v", body.Data)
	}

	// Both the admin and the public read must now be empty too.
	for _, path := range []string{
		"/api/model-status/selected",
		"/api/model-status/embed/config/selected",
	} {
		read := httptest.NewRecorder()
		r.ServeHTTP(read, httptest.NewRequest(http.MethodGet, path, nil))
		if read.Code != http.StatusOK {
			t.Fatalf("read %s status %d body %s", path, read.Code, read.Body.String())
		}
		if got := decodeBody(t, read); len(got.Data) != 0 {
			t.Fatalf("read %s after clearing = %v", path, got.Data)
		}
	}

}

// A failed persist must NOT be reported as success, and must not leave a newer
// local list behind. This is the exact false-success shape being fixed: if the
// shared store write fails, the API has to say so, because otherwise the public
// page keeps serving the older list with no signal to the operator.
func TestSetSelectedModelsReportsPersistFailure(t *testing.T) {
	r, stub := installSelectionRouter(t)

	// Write a known-good selection through the API so the "old" authority is
	// established in the shared store.
	if rec := doJSON(t, r, http.MethodPut, "/api/model-status/selected", map[string]interface{}{
		"models": []string{"authoritative-old"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed status %d body %s", rec.Code, rec.Body.String())
	}

	// Make the shared store reject writes from here on.
	stub.setFailSet(true)

	rec := doJSON(t, r, http.MethodPut, "/api/model-status/selected", map[string]interface{}{
		"models": []string{"never-committed"},
	})
	if rec.Code == http.StatusOK {
		t.Fatalf("a failed persist must not return 200, body = %s", rec.Body.String())
	}
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500 on persist failure, got %d body = %s", rec.Code, rec.Body.String())
	}
	body := decodeBody(t, rec)
	if body.Success {
		t.Fatal("failed persist still reported success")
	}
	if body.Error.Code != "SELECTION_SAVE_ERROR" || body.Error.Message != "模型名单保存未确认，请重试" {
		t.Fatalf("failure must use a safe error response, body = %s", rec.Body.String())
	}

	// The authority must be unchanged: no stale local copy of the rejected list.
	got := decodeBody(t, func() *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/model-status/selected", nil))
		return rec
	}())
	if len(got.Data) != 1 || got.Data[0] != "authoritative-old" {
		t.Fatalf("failed save changed the authority: %v", got.Data)
	}

	// And the public page must still show the old list, not the rejected one.
	pub := httptest.NewRecorder()
	r.ServeHTTP(pub, httptest.NewRequest(http.MethodGet, "/api/model-status/embed/config/selected", nil))
	if pubRec := decodeBody(t, pub); len(pubRec.Data) != 1 || pubRec.Data[0] != "authoritative-old" {
		t.Fatalf("public read after failed save = %v", pubRec.Data)
	}
}

// A read failure must not be answered as success:true + [] — that is the other
// half of the same false-success bug on the public page.
func TestSelectionReadFailureIsNotFakeSuccess(t *testing.T) {
	r, stub := installSelectionRouter(t)

	// Seed a real selection, then make the shared store refuse reads.
	if rec := doJSON(t, r, http.MethodPut, "/api/model-status/selected", map[string]interface{}{
		"models": []string{"seeded"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed status %d body %s", rec.Code, rec.Body.String())
	}

	// Keep the warm local mirror: it must not hide an authority read failure.
	stub.setFailRead(true)

	for _, path := range []string{
		"/api/model-status/selected",
		"/api/model-status/config/selected",
		"/api/model-status/embed/config/selected",
		"/api/model-status/embed/config",
		"/api/model-status/config/time-window",
		"/api/model-status/config/theme",
		"/api/model-status/config/refresh-interval",
		"/api/model-status/config/sort-mode",
	} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Fatalf("read failure at %s must not return 200, body = %s", path, rec.Body.String())
		}
		var body struct {
			Success bool          `json:"success"`
			Data    []interface{} `json:"data"`
			Error   struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if body.Success {
			t.Fatalf("read failure at %s reported success", path)
		}
		if body.Data != nil {
			t.Fatalf("read failure at %s must not carry a payload", path)
		}
		if body.Error.Message != "读取模型监控配置失败，请稍后重试" {
			t.Fatalf("read failure at %s exposed internal error: %s", path, rec.Body.String())
		}
	}
}

func TestSelectionRejectsInvalidPayloadWithoutClearing(t *testing.T) {
	r, _ := installSelectionRouter(t)
	want := []string{"preserved-a", "preserved-b"}
	writes := []struct{ method, path string }{
		{http.MethodPut, "/api/model-status/selected"},
		{http.MethodPost, "/api/model-status/config/selected"},
	}
	for _, w := range writes {
		if rec := doJSON(t, r, w.method, w.path, map[string]interface{}{"models": want}); rec.Code != http.StatusOK {
			t.Fatalf("seed: %d %s", rec.Code, rec.Body.String())
		}
		for _, payload := range []string{`{}`, `null`, `[]`, `{"models":null}`, `{"models":"a"}`, `{"models":[1]}`, `{"models":[null]}`, `{"models":["a",null]}`, `{"models":`} {
			req := httptest.NewRequest(w.method, w.path, bytes.NewBufferString(payload))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			if rec.Code != http.StatusBadRequest || decodeBody(t, rec).Success {
				t.Fatalf("invalid %s was accepted via %s: %d %s", payload, w.path, rec.Code, rec.Body.String())
			}
			read := httptest.NewRecorder()
			r.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/model-status/embed/config/selected", nil))
			got := decodeBody(t, read)
			if read.Code != http.StatusOK || len(got.Data) != len(want) || got.Data[0] != want[0] || got.Data[1] != want[1] {
				t.Fatalf("invalid %s changed published selection: %s", payload, read.Body.String())
			}
		}
	}
}

func TestSelectionSaveReadbackFailureIsNotSuccess(t *testing.T) {
	r, stub := installSelectionRouter(t)
	stub.setFailRead(true)
	rec := doJSON(t, r, http.MethodPost, "/api/model-status/config/selected", map[string]interface{}{"models": []string{"committed"}})
	body := decodeBody(t, rec)
	if rec.Code != http.StatusInternalServerError || body.Success || body.Error.Code != "SELECTION_READ_ERROR" {
		t.Fatalf("failed readback must not confirm save: %d %s", rec.Code, rec.Body.String())
	}
	stub.setFailRead(false)
	read := httptest.NewRecorder()
	r.ServeHTTP(read, httptest.NewRequest(http.MethodGet, "/api/model-status/embed/config/selected", nil))
	got := decodeBody(t, read)
	if read.Code != http.StatusOK || len(got.Data) != 1 || got.Data[0] != "committed" {
		t.Fatalf("write should remain committed even when readback was unconfirmed: %s", read.Body.String())
	}
}
