package cache

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// ============================================================================
// Minimal in-process RESP server
//
// Just enough of the protocol to drive the cache manager without a real Redis,
// so these tests assert the write-ordering contract (shared store first, local
// copy only after a successful shared write) instead of an integration setup.
// ============================================================================

// fakeRedis is a programmable RESP stub; commands are handled by handle.
type fakeRedis struct {
	mu   sync.Mutex
	data map[string][]byte

	// failCommands makes any SET return a Redis error reply (-ERR ...).
	// Reads still work, which is exactly the "shared write failed" state.
	failCommands map[string]bool

	// failReads makes GET return an error, modelling an unreachable store.
	failReads bool

	// beforeSet observes manager state just before a SET is stored.
	beforeSet func()
}

func newFakeRedis() *fakeRedis {
	return &fakeRedis{
		data:         map[string][]byte{},
		failCommands: map[string]bool{},
	}
}

// setFail flips the SET-failure switch under lock.
func (f *fakeRedis) setFail(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failCommands["SET"] = fail
}

func (f *fakeRedis) setFailReads(fail bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failReads = fail
}

// handle parses one command block (already split on RESP arrays) and returns
// the raw RESP reply bytes.
func (f *fakeRedis) handle(cmd []string) string {
	if len(cmd) == 0 {
		return "-ERR empty command\r\n"
	}
	name := strings.ToUpper(cmd[0])

	f.mu.Lock()
	failCmd := f.failCommands[name]
	failReads := f.failReads
	beforeSet := f.beforeSet
	f.mu.Unlock()

	switch name {
	case "PING":
		return "+PONG\r\n"
	case "HELLO":
		// go-redis sends HELLO for the RESP3 handshake on each connect. Reply with
		// a flat map of the fields the client requires.
		return "*6\r\n" +
			"$6\r\nserver\r\n$5\r\nredis\r\n" +
			"$7\r\nversion\r\n$5\r\n7.0.0\r\n" +
			"$8\r\nprotocol\r\n:3\r\n"
	case "SELECT":
		return "+OK\r\n"
	case "CLIENT":
		return "+OK\r\n"
	case "INFO":
		return "$0\r\n\r\n"
	case "SET":
		if failCmd {
			return "-ERR simulated shared-store write failure\r\n"
		}
		if len(cmd) < 3 {
			return "-ERR wrong number of arguments for 'set'\r\n"
		}
		if beforeSet != nil {
			beforeSet()
		}
		f.mu.Lock()
		f.data[cmd[1]] = []byte(cmd[2])
		f.mu.Unlock()
		return "+OK\r\n"
	case "GET":
		if failReads {
			return "-ERR simulated shared-store read failure\r\n"
		}
		f.mu.Lock()
		v, ok := f.data[cmd[1]]
		f.mu.Unlock()
		if !ok {
			return "$-1\r\n"
		}
		return fmt.Sprintf("$%d\r\n%s\r\n", len(v), v)
	case "DEL":
		f.mu.Lock()
		n := 0
		for _, k := range cmd[1:] {
			if _, ok := f.data[k]; ok {
				delete(f.data, k)
				n++
			}
		}
		f.mu.Unlock()
		return fmt.Sprintf(":%d\r\n", n)
	case "EXISTS":
		f.mu.Lock()
		n := 0
		for _, k := range cmd[1:] {
			if _, ok := f.data[k]; ok {
				n++
			}
		}
		f.mu.Unlock()
		return fmt.Sprintf(":%d\r\n", n)
	case "SCAN":
		return "*2\r\n:0\r\n*0\r\n"
	case "DBSIZE":
		f.mu.Lock()
		n := len(f.data)
		f.mu.Unlock()
		return fmt.Sprintf(":%d\r\n", n)
	default:
		return "+OK\r\n"
	}
}

// serve reads commands from conn and writes replies until conn closes.
func (f *fakeRedis) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)

	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" {
			continue
		}
		if line[0] != '*' {
			// Inline command.
			parts := strings.Fields(line)
			if _, werr := conn.Write([]byte(f.handle(parts))); werr != nil {
				return
			}
			continue
		}

		count, perr := strconv.Atoi(line[1:])
		if perr != nil || count <= 0 {
			conn.Write([]byte("-ERR protocol error\r\n"))
			return
		}

		cmd := make([]string, 0, count)
		ok := true
		for i := 0; i < count; i++ {
			hdr, herr := r.ReadString('\n')
			if herr != nil {
				ok = false
				break
			}
			hdr = strings.TrimRight(hdr, "\r\n")
			if len(hdr) == 0 || hdr[0] != '$' {
				ok = false
				break
			}
			n, aerr := strconv.Atoi(hdr[1:])
			if aerr != nil {
				ok = false
				break
			}
			if n < 0 {
				cmd = append(cmd, "")
				continue
			}
			buf := make([]byte, n+2)
			if _, rerr := io.ReadFull(r, buf); rerr != nil {
				ok = false
				break
			}
			cmd = append(cmd, string(buf[:n]))
		}
		if !ok {
			conn.Write([]byte("-ERR protocol error\r\n"))
			return
		}

		if _, werr := conn.Write([]byte(f.handle(cmd))); werr != nil {
			return
		}
	}
}

// client builds a go-redis client whose connections terminate in the fake.
func (f *fakeRedis) client() *redis.Client {
	return redis.NewClient(&redis.Options{
		Addr:     "fake:6379",
		Protocol: 3,
		Dialer: func(ctx context.Context, network, addr string) (net.Conn, error) {
			client, server := net.Pipe()
			go f.serve(server)
			return client, nil
		},
		// Keep tests fast and deterministic.
		MaxRetries:      -1,
		MinRetryBackoff: time.Millisecond,
		MaxRetryBackoff: 5 * time.Millisecond,
		DialTimeout:     2 * time.Second,
		ReadTimeout:     2 * time.Second,
		WriteTimeout:    2 * time.Second,
	})
}

// installFakeCacheManager installs a Manager backed by the fake store and
// returns both. It does not touch the package-global mgr, so the "in-process
// mode" (no shared store) tests stay independent.
func installFakeCacheManager(t *testing.T) (*Manager, *fakeRedis) {
	t.Helper()
	f := newFakeRedis()
	m := &Manager{rdb: f.client(), ctx: context.Background()}
	t.Cleanup(func() { _ = m.rdb.Close() })

	// If the "unreachable store" switch is on, the client should surface a real
	// error rather than an empty reply. Sanity-check the connection works at all.
	if err := m.rdb.Ping(m.ctx).Err(); err != nil {
		t.Fatalf("fake redis unusable: %v", err)
	}
	return m, f
}

// ============================================================================
// Contract tests
// ============================================================================

// A shared-store write failure must not publish the value locally. This is the
// core of "false success" prevention: if the local copy were written first, the
// instance would serve a list the shared store — and every other instance —
// never received.
func TestSetAuthoritativeDoesNotPublishLocalWhenSharedWriteFails(t *testing.T) {
	m, f := installFakeCacheManager(t)

	// Seed an older value that is already authoritative.
	if err := m.SetAuthoritative("k", []string{"old-model"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Now make the shared store reject writes.
	f.setFail(true)

	err := m.SetAuthoritative("k", []string{"new-model"}, 0)
	if err == nil {
		t.Fatal("expected error when shared-store write fails")
	}

	// The shared store must still hold the old value.
	got, gerr := m.rdb.Get(m.ctx, "k").Result()
	if gerr != nil {
		t.Fatalf("read shared store: %v", gerr)
	}
	if !strings.Contains(got, "old-model") {
		t.Fatalf("shared store was mutated by a failed write: %s", got)
	}

	// And the local mirror must not have been advanced to the new value.
	m.localCache.Range(func(key, value interface{}) bool {
		if key == "k" {
			t.Errorf("failed write published a local entry: %v", value)
		}
		return true
	})

	var out []string
	found, rerr := m.GetAuthoritativeJSON("k", &out)
	if rerr != nil {
		t.Fatalf("authoritative read: %v", rerr)
	}
	if !found || len(out) != 1 || out[0] != "old-model" {
		t.Fatalf("authority changed after failed write: found=%v out=%v", found, out)
	}
}

// A successful write must appear in the shared store, and must be written to it
// before the local copy is made available.
func TestSetAuthoritativeWritesSharedStoreBeforeLocal(t *testing.T) {
	m, f := installFakeCacheManager(t)

	f.mu.Lock()
	f.beforeSet = func() {
		if _, exists := m.localCache.Load("k"); exists {
			t.Error("local value was published before the shared write")
		}
	}
	f.mu.Unlock()
	if err := m.SetAuthoritative("k", []string{"a", "b"}, 0); err != nil {
		t.Fatalf("set: %v", err)
	}
	if _, exists := m.localCache.Load("k"); !exists {
		t.Fatal("successful shared write did not publish a local mirror")
	}

	// The value must be readable from the shared store itself.
	got, err := m.rdb.Get(m.ctx, "k").Result()
	if err != nil {
		t.Fatalf("shared read: %v", err)
	}
	if !strings.Contains(got, `["a","b"]`) {
		t.Fatalf("unexpected shared payload: %s", got)
	}
}

// A key the shared store does not have must read as "not found", even when this
// process holds an older local copy (which would happen after a DELETE by
// another instance, or after the value was never propagated).
func TestGetAuthoritativeDoesNotServeStaleLocalWhenSharedStoreHasNoKey(t *testing.T) {
	m, _ := installFakeCacheManager(t)

	// Write directly to the shared store, bypassing the manager, so no local
	// mirror exists yet.
	if err := m.rdb.Set(m.ctx, "k", []byte(`["shared"]`), 0).Err(); err != nil {
		t.Fatalf("seed shared: %v", err)
	}

	var out []string
	if found, err := m.GetAuthoritativeJSON("k", &out); err != nil || !found || out[0] != "shared" {
		t.Fatalf("read from shared: found=%v out=%v err=%v", found, out, err)
	}

	// Another instance deletes the key; our local mirror is now stale.
	if err := m.rdb.Del(m.ctx, "k").Err(); err != nil {
		t.Fatalf("del shared: %v", err)
	}

	found, err := m.GetAuthoritativeJSON("k", &out)
	if err != nil {
		t.Fatalf("read after remote delete: %v", err)
	}
	if found {
		t.Fatalf("stale local copy shadowed an absent shared key: %v", out)
	}

	// And the stale mirror must have been dropped, not just ignored once.
	m.localCache.Range(func(key, _ interface{}) bool {
		if key == "k" {
			t.Error("stale local entry was not evicted")
		}
		return true
	})
}

// An unreachable shared store must surface an error on read rather than a
// plausible-looking empty result.
func TestGetAuthoritativeSurfacesReadFailure(t *testing.T) {
	m, f := installFakeCacheManager(t)

	if err := m.SetAuthoritative("k", []string{"x"}, 0); err != nil {
		t.Fatalf("seed: %v", err)
	}

	f.setFailReads(true)

	var out []string
	found, err := m.GetAuthoritativeJSON("k", &out)
	if err == nil {
		t.Fatal("expected read error from unreachable shared store")
	}
	if found {
		t.Fatalf("must not report found=%v alongside error", found)
	}
	if !strings.Contains(err.Error(), "k") {
		t.Fatalf("error should name the key for diagnosis: %v", err)
	}
}

// Without a shared store the manager runs in the explicit in-process mode:
// writes succeed locally and reads return them, but nothing is claimed to
// survive a restart.
func TestAuthoritativeInProcessModeWithoutSharedStore(t *testing.T) {
	// Exactly the zero-value Manager cache.Get() hands back when cache.Init
	// was never called (no REDIS_CONN_STRING).
	m := &Manager{ctx: context.Background()}
	if m.rdb != nil {
		t.Fatal("test expects no shared store")
	}

	if err := m.SetAuthoritative("k", []string{"a"}, 0); err != nil {
		t.Fatalf("in-process set: %v", err)
	}

	var out []string
	found, err := m.GetAuthoritativeJSON("k", &out)
	if err != nil {
		t.Fatalf("in-process read: %v", err)
	}
	if !found || len(out) != 1 || out[0] != "a" {
		t.Fatalf("in-process round trip: found=%v out=%v", found, out)
	}

	// An unset key is a plain miss, not an error.
	var missing []string
	found, err = m.GetAuthoritativeJSON("nope", &missing)
	if err != nil || found {
		t.Fatalf("unexpected in-process miss: found=%v err=%v", found, err)
	}
}

// An empty selection is a legal value and must round-trip as [] (never null,
// never "not found").
func TestAuthoritativeEmptyListRoundTripsAsEmptyArray(t *testing.T) {
	m, _ := installFakeCacheManager(t)

	if err := m.SetAuthoritative("k", []string{}, 0); err != nil {
		t.Fatalf("set empty: %v", err)
	}

	raw, err := m.rdb.Get(m.ctx, "k").Result()
	if err != nil {
		t.Fatalf("shared read: %v", err)
	}
	if raw != "[]" {
		t.Fatalf("empty list must be stored as [], got %q", raw)
	}

	var out []string
	found, err := m.GetAuthoritativeJSON("k", &out)
	if err != nil || !found {
		t.Fatalf("empty list must be found: found=%v err=%v", found, err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("empty list must decode to [], got %#v", out)
	}
}

// The legacy non-authoritative Set/GetJSON pair must keep working, since config
// keys other than the selection still use it.
func TestLegacySetGetJSONStillWorks(t *testing.T) {
	m, _ := installFakeCacheManager(t)

	if err := m.Set("legacy", map[string]string{"a": "b"}, time.Minute); err != nil {
		t.Fatalf("legacy set: %v", err)
	}
	var out map[string]string
	found, err := m.GetJSON("legacy", &out)
	if err != nil || !found || out["a"] != "b" {
		t.Fatalf("legacy round trip: found=%v out=%v err=%v", found, out, err)
	}

	// Serialization failure still reports an error on both paths.
	if err := m.SetAuthoritative("bad", func() {}, 0); err == nil {
		t.Fatal("expected serialization error")
	}
}

// Separate managers sharing Redis must see the same 12 models even with a warm
// local mirror of the previous 47-model selection.
func TestAuthoritativeSelectionImmediatelySeesAnotherManagerWrite(t *testing.T) {
	first, shared := installFakeCacheManager(t)
	second := &Manager{rdb: shared.client(), ctx: context.Background()}
	t.Cleanup(func() { _ = second.rdb.Close() })

	previous := make([]string, 47)
	for i := range previous {
		previous[i] = fmt.Sprintf("old-%d", i)
	}
	selected := make([]string, 12)
	for i := range selected {
		selected[i] = fmt.Sprintf("selected-%d", i)
	}
	if err := first.SetAuthoritative("selection", previous, 0); err != nil {
		t.Fatal(err)
	}
	if err := second.SetAuthoritative("selection", selected, 0); err != nil {
		t.Fatal(err)
	}
	for _, manager := range []*Manager{first, second} {
		var got []string
		found, err := manager.GetAuthoritativeJSON("selection", &got)
		if err != nil || !found || !slices.Equal(got, selected) {
			t.Fatalf("selection differs across managers: found=%v got=%v err=%v", found, got, err)
		}
	}
}

func TestAuthoritativeReadRejectsMalformedJSON(t *testing.T) {
	m, _ := installFakeCacheManager(t)
	if err := m.rdb.Set(m.ctx, "k", `not-json`, 0).Err(); err != nil {
		t.Fatal(err)
	}
	var out []string
	_, err := m.GetAuthoritativeJSON("k", &out)
	var syntaxError *json.SyntaxError
	if !errors.As(err, &syntaxError) {
		t.Fatalf("malformed store payload must surface a JSON error: %v", err)
	}
}
