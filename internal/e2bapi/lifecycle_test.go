package e2bapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/opencontainers/go-digest"
	"github.com/openeuler/Conch/internal/sandbox"
	"github.com/openeuler/Conch/internal/sandboxproxy"
)

func seedPausedRecord(t *testing.T, store sandbox.Store, head string) sandbox.Record {
	t.Helper()
	rec, err := store.Create(t.Context(), sandbox.Record{
		ID: uuid.NewString(), State: sandbox.StatePaused, SourceTemplateName: "base",
		SourceTemplateID:         digest.FromString("preinstalled-template").String(),
		CheckpointHeadTemplateID: head, VCPUNum: 2, RamMB: 512, E2B: true, EnvdVersion: "0.8.3",
		StartedAt: 100, HeadStartedAt: 100, HeadCreatedAt: 200,
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestPauseConnectHandlers(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	ready := seedRecord(t, service.Store, true, true, "pause")
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/pause", `{"memory":false}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("memory:false = %d %s", resp.StatusCode, data)
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/pause", `{}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("pause = %d %s", resp.StatusCode, data)
	}
	paused, err := service.Store.Get(t.Context(), ready.ID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != sandbox.StatePaused || paused.ExpiresAt != 0 || paused.IP != "" || paused.VMMPID != 0 {
		t.Fatalf("paused record = %#v", paused)
	}
	if paused.CheckpointHeadTemplateID == paused.SourceTemplateID || paused.HeadStartedAt != paused.StartedAt || paused.HeadCreatedAt == 0 {
		t.Fatalf("pause did not move the checkpoint head: %#v", paused)
	}
	if calls, _, _ := runtime.lifecycleCalls(); len(calls) != 1 || calls[0] != ready.ID {
		t.Fatalf("pause dispatch = %v", calls)
	}

	// A paused sandbox keeps answering GET and list with the paused view.
	resp, data = doRequest(t, server, http.MethodGet, "/sandboxes/"+ready.ID, "", true)
	var detail sandboxDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || detail.State != "paused" {
		t.Fatalf("paused detail = %d %s", resp.StatusCode, data)
	}
	if detail.EndAt.UnixNano() != paused.HeadCreatedAt {
		t.Fatalf("paused endAt = %v, want the head creation time", detail.EndAt)
	}
	resp, data = doRequest(t, server, http.MethodGet, "/sandboxes?state=paused", "", true)
	var pausedList []sandboxDetail
	if err := json.Unmarshal(data, &pausedList); err != nil {
		t.Fatal(err)
	}
	if len(pausedList) != 1 || pausedList[0].SandboxID != ready.ID || resp.Header.Get("X-Total-Running") != "" {
		t.Fatalf("paused list = %d items, headers=%v", len(pausedList), resp.Header)
	}

	// Second pause returns 409 (SDK pause() reports False).
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/pause", `{}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("double pause = %d", resp.StatusCode)
	}

	// connect on a READY sandbox only extends the TTL (200): a shorter
	// deadline is extended to the request, a longer one is left untouched.
	other := seedRecord(t, service.Store, true, true, "running")
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+other.ID+"/timeout", `{"timeout":10}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("reset before connect extend failed")
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+other.ID+"/connect", `{"timeout":60}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("running connect = %d %s", resp.StatusCode, data)
	}
	var extended sandboxDetail
	if err := json.Unmarshal(data, &extended); err != nil {
		t.Fatal(err)
	}
	if extended.SandboxID != other.ID || extended.State != "running" {
		t.Fatalf("running connect response = %#v", extended)
	}
	rec, _ := service.Store.Get(t.Context(), other.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 50*time.Second || remaining > 60*time.Second {
		t.Fatalf("connect did not extend the TTL: %v", remaining)
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+other.ID+"/connect", `{"timeout":30}`, true)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("shortening connect = %d %s", resp.StatusCode, data)
	}
	rec, _ = service.Store.Get(t.Context(), other.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 50*time.Second {
		t.Fatalf("connect shortened the deadline: %v", remaining)
	}

	// An absent or non-positive timeout leaves the running deadline untouched.
	deadline := rec.ExpiresAt
	for _, body := range []string{`{}`, `{"timeout":0}`, `{"timeout":-5}`} {
		resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+other.ID+"/connect", body, true)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("no-op connect %s = %d %s", body, resp.StatusCode, data)
		}
	}
	rec, _ = service.Store.Get(t.Context(), other.ID)
	if rec.ExpiresAt != deadline {
		t.Fatalf("no-op connect moved the deadline: %d -> %d", deadline, rec.ExpiresAt)
	}

	// connect on the PAUSED sandbox restores it under the same ID (201) with
	// the requested timeout as the new run's lifetime.
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/connect", `{"timeout":600}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("paused connect = %d %s", resp.StatusCode, data)
	}
	var resumed sandboxDetail
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.SandboxID != ready.ID || resumed.State != "running" {
		t.Fatalf("resumed response = %#v", resumed)
	}
	rec, err = service.Store.Get(t.Context(), ready.ID)
	if err != nil || rec.State != sandbox.StateReady {
		t.Fatalf("resumed record = %#v (%v)", rec, err)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 590*time.Second {
		t.Fatalf("resume did not apply the connect timeout: %v", remaining)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != 1 || resumedCalls[0] != ready.ID {
		t.Fatalf("resume dispatch = %v", resumedCalls)
	}
}

// TestConnectResumesWhenPauseWinsTTLRace pins the connect recovery when a
// concurrent pause flips the record between the facade's read and its TTL
// write: the re-read finds PAUSED and the request continues as the paused
// connect (201) instead of surfacing the stale precondition as 409.
func TestConnectResumesWhenPauseWinsTTLRace(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	ready := seedRecord(t, service.Store, true, true, "racy-pause")
	runtime.ttlFlip = map[string]bool{ready.ID: true}
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/connect", `{"timeout":600}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("connect after the TTL race = %d %s", resp.StatusCode, data)
	}
	var resumed sandboxDetail
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.SandboxID != ready.ID || resumed.State != "running" {
		t.Fatalf("connect response = %#v", resumed)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != 1 || resumedCalls[0] != ready.ID {
		t.Fatalf("resume dispatch = %v", resumedCalls)
	}
	rec, err := service.Store.Get(t.Context(), ready.ID)
	if err != nil || rec.State != sandbox.StateReady {
		t.Fatalf("resumed record = %#v (%v)", rec, err)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 590*time.Second {
		t.Fatalf("resume did not apply the connect timeout: %v", remaining)
	}
}

// TestConnectDefaultAndCeiling covers the PAUSED connect timeout defaults:
// an absent, zero or negative timeout starts a 300s run, and a timeout beyond
// the node ceiling is rejected before dispatch.
func TestConnectDefaultAndCeiling(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	paused := seedPausedRecord(t, service.Store, digest.FromString("head-a").String())
	for _, body := range []string{`{}`, `{"timeout":0}`, `{"timeout":-5}`} {
		resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+paused.ID+"/connect", body, true)
		if resp.StatusCode != http.StatusCreated {
			t.Fatalf("connect %s = %d %s", body, resp.StatusCode, data)
		}
		rec, err := service.Store.Get(t.Context(), paused.ID)
		if err != nil {
			t.Fatal(err)
		}
		if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 290*time.Second || remaining > 300*time.Second {
			t.Fatalf("connect %s default timeout = %v", body, remaining)
		}
		// Re-pause through the fake so the next iteration starts paused.
		rec.State = sandbox.StatePaused
		if _, err := service.Store.Update(t.Context(), rec); err != nil {
			t.Fatal(err)
		}
	}
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+paused.ID+"/connect", `{"timeout":86401}`, true)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("over-ceiling connect = %d %s", resp.StatusCode, data)
	}
}

// TestConnectCreatingLineage covers the CREATING split by lineage: a fresh
// create answers 404, a resuming record keeps answering paused, and a connect
// on it never dispatches a resume itself — it waits for the transition and
// joins it with 201, leaving the winner's deadline untouched.
func TestConnectCreatingLineage(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	fresh := seedRecord(t, service.Store, false, true, "fresh")
	resp, _ := doRequest(t, server, http.MethodGet, "/sandboxes/"+fresh.ID, "", true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fresh creating GET = %d", resp.StatusCode)
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+fresh.ID+"/connect", `{}`, true)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fresh creating connect = %d", resp.StatusCode)
	}

	// An in-place resume in flight: the record is CREATING with a checkpoint
	// lineage and answers as paused until the resume commits READY.
	resuming := seedPausedRecord(t, service.Store, digest.FromString("head-b").String())
	resuming.State = sandbox.StateCreating
	if _, err := service.Store.Update(t.Context(), resuming); err != nil {
		t.Fatal(err)
	}
	resp, data := doRequest(t, server, http.MethodGet, "/sandboxes/"+resuming.ID, "", true)
	var detail sandboxDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || detail.State != "paused" {
		t.Fatalf("resuming GET = %d %s", resp.StatusCode, data)
	}
	if detail.StartedAt.UnixNano() != resuming.HeadStartedAt || detail.EndAt.UnixNano() != resuming.HeadCreatedAt {
		t.Fatalf("paused view timestamps = %v..%v", detail.StartedAt, detail.EndAt)
	}

	flipDone := make(chan struct{})
	go func() {
		defer close(flipDone)
		time.Sleep(300 * time.Millisecond)
		rec, err := service.Store.Get(t.Context(), resuming.ID)
		if err != nil {
			t.Error(err)
			return
		}
		rec.State = sandbox.StateReady
		rec.StartedAt = time.Now().UnixNano()
		rec.ExpiresAt = time.Now().Add(time.Minute).UnixNano()
		if _, err := service.Store.Update(t.Context(), rec); err != nil {
			t.Error(err)
		}
	}()
	started := time.Now()
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+resuming.ID+"/connect", `{"timeout":600}`, true)
	if elapsed := time.Since(started); elapsed < 200*time.Millisecond {
		t.Fatalf("second connect did not wait for the resume: %v", elapsed)
	}
	var resumed sandboxDetail
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	// The joined request gets 201 and does not touch the winner's deadline
	// (the flip granted one minute, not the requested 600s).
	if resp.StatusCode != http.StatusCreated || resumed.State != "running" {
		t.Fatalf("second connect = %d %s", resp.StatusCode, data)
	}
	<-flipDone
	rec, err := service.Store.Get(t.Context(), resuming.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining > time.Minute {
		t.Fatalf("joined connect moved the deadline: %v", remaining)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != 0 {
		t.Fatalf("connect on a resuming record dispatched a resume: %v", resumedCalls)
	}
}

// TestConnectCreatingOrphanWaitsOutBudget covers the double-fault orphan: a
// CREATING lineage with no runtime entry has nothing resuming, so connect
// holds to its 409 without dispatching an in-process revival.
func TestConnectCreatingOrphanWaitsOutBudget(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	orphan := seedPausedRecord(t, service.Store, digest.FromString("head-orphan").String())
	orphan.State = sandbox.StateCreating
	if _, err := service.Store.Update(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	old := connectWait
	connectWait = 300 * time.Millisecond
	t.Cleanup(func() { connectWait = old })

	started := time.Now()
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+orphan.ID+"/connect", `{}`, true)
	if elapsed := time.Since(started); elapsed < 300*time.Millisecond {
		t.Fatalf("orphan connect returned before the budget: %v", elapsed)
	}
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), "still resuming") {
		t.Fatalf("orphan connect = %d %s", resp.StatusCode, data)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != 0 {
		t.Fatalf("orphan connect dispatched a resume: %v", resumedCalls)
	}
}

// TestConnectTimeoutWrap covers the ttl sanitize before the Duration
// arithmetic: a huge negative timeout wraps to a positive sub-second
// duration, which would resume the sandbox on a near-expired deadline
// instead of the 300s default.
func TestConnectTimeoutWrap(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	paused := seedPausedRecord(t, service.Store, digest.FromString("head-ttl").String())
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+paused.ID+"/connect", `{"timeout":-9223372036854}`, true)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("wrapped-timeout connect = %d %s", resp.StatusCode, data)
	}
	rec, err := service.Store.Get(t.Context(), paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 60*time.Second || remaining > 310*time.Second {
		t.Fatalf("wrapped timeout resumed with remaining = %v, want the 300s default", remaining)
	}
}

// TestConnectWaitOutlivesRequestTimeout pins the wait's independence from the
// request budget: the connectWait verdict (409) must arrive even when the
// wrapping request timeout expired mid-wait, instead of a 504 for an
// operation still inside its own budget.
func TestConnectWaitOutlivesRequestTimeout(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	api, err := New(Config{
		APIKey: testAPIKey, Domains: []string{"sandbox.example.test"}, RequestTimeout: 150 * time.Millisecond,
		MaxSandboxTTL: 24 * time.Hour,
	}, service, routes)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(api)
	t.Cleanup(server.Close)

	orphan := seedPausedRecord(t, service.Store, digest.FromString("head-budget").String())
	orphan.State = sandbox.StateCreating
	if _, err := service.Store.Update(t.Context(), orphan); err != nil {
		t.Fatal(err)
	}
	old := connectWait
	connectWait = 400 * time.Millisecond
	t.Cleanup(func() { connectWait = old })

	started := time.Now()
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+orphan.ID+"/connect", `{}`, true)
	if elapsed := time.Since(started); elapsed < 400*time.Millisecond {
		t.Fatalf("connect returned before the wait budget: %v", elapsed)
	}
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(data), "still resuming") {
		t.Fatalf("expired-budget connect = %d %s, want the 409 wait verdict", resp.StatusCode, data)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != 0 {
		t.Fatalf("orphan connect dispatched a resume: %v", resumedCalls)
	}
}

// TestConnectStrandedUnknownLineage covers the terminal-failure recovery over
// the facade: a checkpoint lineage stranded in UNKNOWN answers as paused and
// connect restores it directly — the runtime entry is gone, so there is
// nothing to wait for. A stranded record whose entry is still held by
// another request conflicts instead of waiting forever.
func TestConnectStrandedUnknownLineage(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	stranded := seedPausedRecord(t, service.Store, digest.FromString("head-u").String())
	stranded.State = sandbox.StateUnknown
	stranded.LastError = "paused write failure"
	if _, err := service.Store.Update(t.Context(), stranded); err != nil {
		t.Fatal(err)
	}
	resp, data := doRequest(t, server, http.MethodGet, "/sandboxes/"+stranded.ID, "", true)
	var detail sandboxDetail
	if err := json.Unmarshal(data, &detail); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || detail.State != "paused" {
		t.Fatalf("stranded GET = %d %s", resp.StatusCode, data)
	}
	if detail.StartedAt.UnixNano() != stranded.HeadStartedAt || detail.EndAt.UnixNano() != stranded.HeadCreatedAt {
		t.Fatalf("stranded paused view timestamps = %v..%v", detail.StartedAt, detail.EndAt)
	}
	resp, data = doRequest(t, server, http.MethodGet, "/sandboxes?state=paused", "", true)
	var pausedList []sandboxDetail
	if err := json.Unmarshal(data, &pausedList); err != nil {
		t.Fatal(err)
	}
	if len(pausedList) != 1 || pausedList[0].SandboxID != stranded.ID {
		t.Fatalf("stranded paused list = %d items", len(pausedList))
	}

	started := time.Now()
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+stranded.ID+"/connect", `{}`, true)
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("stranded connect waited: %v", elapsed)
	}
	var resumed sandboxDetail
	if err := json.Unmarshal(data, &resumed); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusCreated || resumed.State != "running" {
		t.Fatalf("stranded connect = %d %s", resp.StatusCode, data)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != 1 || resumedCalls[0] != stranded.ID {
		t.Fatalf("resume dispatch = %v", resumedCalls)
	}
	rec, err := service.Store.Get(t.Context(), stranded.ID)
	if err != nil || rec.State != sandbox.StateReady || rec.LastError != "" {
		t.Fatalf("resumed record = %#v (%v)", rec, err)
	}

	runtime.inFlightResume = true
	held := seedPausedRecord(t, service.Store, digest.FromString("head-held").String())
	held.State = sandbox.StateUnknown
	if _, err := service.Store.Update(t.Context(), held); err != nil {
		t.Fatal(err)
	}
	resp, data = doRequest(t, server, http.MethodPost, "/sandboxes/"+held.ID+"/connect", `{}`, true)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("entry-held stranded connect = %d %s", resp.StatusCode, data)
	}
}

// TestConnectCancelDuringResume covers the awaitResume error contract: a
// client disconnect during the wait answers 410 with the data-plane wording,
// and a record that vanishes mid-wait surfaces the store's NotFound.
func TestConnectCancelDuringResume(t *testing.T) {
	service, routes := testRuntime(t)
	service.Sandbox = &fakeSandboxRuntime{store: service.Store}
	api, err := New(Config{
		APIKey: testAPIKey, Domains: []string{"sandbox.example.test"}, RequestTimeout: 5 * time.Second,
		MaxSandboxTTL: 24 * time.Hour,
	}, service, routes)
	if err != nil {
		t.Fatal(err)
	}

	resuming := seedPausedRecord(t, service.Store, digest.FromString("head-cancel").String())
	resuming.State = sandbox.StateCreating
	if _, err := service.Store.Update(t.Context(), resuming); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	req := httptest.NewRequest(http.MethodPost, "/sandboxes/"+resuming.ID+"/connect", strings.NewReader(`{}`)).WithContext(ctx)
	req.Header.Set("X-API-Key", testAPIKey)
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	api.ServeHTTP(rec, req)
	<-done
	if rec.Code != http.StatusGone || !strings.Contains(rec.Body.String(), "sandbox request was canceled") {
		t.Fatalf("canceled connect = %d %s", rec.Code, rec.Body.String())
	}

	if _, err := api.awaitResume(t.Context(), uuid.NewString()); !errors.Is(err, sandbox.ErrNotFound) {
		t.Fatalf("awaitResume on missing record = %v, want NotFound", err)
	}
}

// TestConcurrentConnectOnPausedSandbox covers the connect race: two requests
// that both read the PAUSED record both dispatch a resume, the winner restores
// it, and the loser succeeds as the joined request — both answer 201 and the
// winner's deadline stands, matching upstream's waitForStart semantics.
func TestConcurrentConnectOnPausedSandbox(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	paused := seedPausedRecord(t, service.Store, digest.FromString("head-race").String())
	const requests = 2
	type response struct {
		status int
		body   []byte
	}
	responses := make(chan response, requests)
	for range requests {
		go func() {
			resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+paused.ID+"/connect", `{"timeout":600}`, true)
			responses <- response{resp.StatusCode, data}
		}()
	}
	for range requests {
		if r := <-responses; r.status != http.StatusCreated {
			t.Fatalf("concurrent connect = %d %s, want 201", r.status, r.body)
		}
	}
	rec, err := service.Store.Get(t.Context(), paused.ID)
	if err != nil || rec.State != sandbox.StateReady {
		t.Fatalf("resumed record = %#v (%v)", rec, err)
	}
	// The winner's 600s run stands; the loser's request never extends it
	// beyond what the winner granted.
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 590*time.Second || remaining > 600*time.Second {
		t.Fatalf("winner deadline = %v", remaining)
	}
	if _, resumedCalls, _ := runtime.lifecycleCalls(); len(resumedCalls) != requests {
		t.Fatalf("resume dispatch = %v, want %d", resumedCalls, requests)
	}
}

func TestPauseConnectStateMatrix(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)
	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	ready := seedRecord(t, service.Store, true, true, "")
	paused := seedPausedRecord(t, service.Store, digest.FromString("head-c").String())

	for _, tc := range []struct {
		name, method, path, body string
		status                   int
	}{
		{"pause unknown", http.MethodPost, "/sandboxes/" + uuid.NewString() + "/pause", `{}`, 404},
		{"pause native", http.MethodPost, "/sandboxes/" + native.ID + "/pause", `{}`, 404},
		{"pause creating", http.MethodPost, "/sandboxes/" + creating.ID + "/pause", `{}`, 409},
		{"pause paused", http.MethodPost, "/sandboxes/" + paused.ID + "/pause", `{}`, 409},
		{"pause null body", http.MethodPost, "/sandboxes/" + ready.ID + "/pause", `null`, 400},
		{"pause unknown key", http.MethodPost, "/sandboxes/" + ready.ID + "/pause", `{"disk":true}`, 400},
		{"connect unknown", http.MethodPost, "/sandboxes/" + uuid.NewString() + "/connect", `{}`, 404},
		{"connect native", http.MethodPost, "/sandboxes/" + native.ID + "/connect", `{}`, 404},
		{"connect creating", http.MethodPost, "/sandboxes/" + creating.ID + "/connect", `{}`, 404},
		{"connect zero timeout", http.MethodPost, "/sandboxes/" + ready.ID + "/connect", `{"timeout":0}`, 200},
		{"connect huge timeout", http.MethodPost, "/sandboxes/" + ready.ID + "/connect", `{"timeout":86401}`, 400},
		{"timeout null", http.MethodPost, "/sandboxes/" + ready.ID + "/timeout", `null`, 400},
		{"timeout unknown key", http.MethodPost, "/sandboxes/" + ready.ID + "/timeout", `{"ttl":60}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := doRequest(t, server, tc.method, tc.path, tc.body, true)
			if resp.StatusCode != tc.status {
				t.Fatalf("got HTTP %d %s, want %d", resp.StatusCode, data, tc.status)
			}
		})
	}
	if _, _, expiredCalls := runtime.lifecycleCalls(); len(expiredCalls) != 0 {
		t.Fatalf("state matrix dispatched lifecycle calls: %v", expiredCalls)
	}
}

func TestTTLHandlers(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)
	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	ready := seedRecord(t, service.Store, true, true, "")

	// set_timeout requires the timeout field: a missing field or JSON null is
	// rejected and leaves the deadline untouched.
	for _, body := range []string{`{}`, `{"timeout":null}`} {
		resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", body, true)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "timeout is required") {
			t.Fatalf("set_timeout %s = %d %s, want 400 timeout is required", body, resp.StatusCode, data)
		}
	}
	rec, _ := service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 4*time.Minute || remaining > 5*time.Minute {
		t.Fatalf("rejected set_timeout mutated the deadline: %v", remaining)
	}

	// set_timeout resets (and may shorten) the deadline; zero expires now.
	resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":3600}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set_timeout = %d %s", resp.StatusCode, data)
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 3500*time.Second || remaining > 3600*time.Second {
		t.Fatalf("set_timeout deadline not reset: %v", remaining)
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":60}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("shortening set_timeout failed")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining > 60*time.Second {
		t.Fatalf("set_timeout did not shorten: %v", remaining)
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":0}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("zero set_timeout failed")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if rec.ExpiresAt > time.Now().Add(time.Second).UnixNano() {
		t.Fatalf("zero set_timeout did not expire now: %v", time.Until(time.Unix(0, rec.ExpiresAt)))
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":3600}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("reset before negative set_timeout failed")
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":-1}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("negative set_timeout failed")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if rec.ExpiresAt > time.Now().Add(time.Second).UnixNano() {
		t.Fatalf("negative set_timeout did not expire now: %v", time.Until(time.Unix(0, rec.ExpiresAt)))
	}
	// A request beyond the ceiling is clamped by the total-lifetime cap, not
	// rejected.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":100000}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("over-ceiling set_timeout rejected")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining > 24*time.Hour {
		t.Fatalf("set_timeout ignored the lifetime cap: %v", remaining)
	}
	// An int64-overflowing timeout is clamped to the duration range instead of
	// wrapping into an immediate expiry.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout",
		fmt.Sprintf(`{"timeout":%d}`, math.MaxInt64), true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("overflowing set_timeout rejected")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 23*time.Hour {
		t.Fatalf("overflowing set_timeout wrapped the deadline: %v", remaining)
	}

	// refreshes floors at 15s and only extends.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":10}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("reset to 10s failed")
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/refreshes", `{"duration":1}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("short refresh failed")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 14*time.Second || remaining > 16*time.Second {
		t.Fatalf("refresh did not apply the 15s floor: %v", remaining)
	}
	// A long refresh extends up to the lifetime cap; it never shortens.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/timeout", `{"timeout":3600}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("reset before extend check failed")
	}
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/refreshes", `{"duration":15}`, true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("short refresh after long reset failed")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 3500*time.Second {
		t.Fatalf("refresh shortened the deadline: %v", remaining)
	}
	// An int64-overflowing refresh is clamped to the duration range instead of
	// wrapping into a no-op below the floor.
	resp, _ = doRequest(t, server, http.MethodPost, "/sandboxes/"+ready.ID+"/refreshes",
		fmt.Sprintf(`{"duration":%d}`, math.MaxInt64), true)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatal("overflowing refresh rejected")
	}
	rec, _ = service.Store.Get(t.Context(), ready.ID)
	if remaining := time.Until(time.Unix(0, rec.ExpiresAt)); remaining < 23*time.Hour {
		t.Fatalf("overflowing refresh wrapped the deadline: %v", remaining)
	}

	// A run past the total lifetime is rejected with the native message.
	aged := seedRecord(t, service.Store, true, true, "aged")
	aged.StartedAt = time.Now().Add(-25 * time.Hour).UnixNano()
	if _, err := service.Store.Update(t.Context(), aged); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/timeout", "/refreshes"} {
		body := `{"timeout":60}`
		if path == "/refreshes" {
			body = `{"duration":60}`
		}
		resp, data := doRequest(t, server, http.MethodPost, "/sandboxes/"+aged.ID+path, body, true)
		if resp.StatusCode != http.StatusBadRequest || !strings.Contains(string(data), "Max instance length exceeded") {
			t.Fatalf("aged %s = %d %s", path, resp.StatusCode, data)
		}
	}

	// TTL guards: unknown, native and non-ready states.
	for _, tc := range []struct{ method, path, body string }{
		{http.MethodPost, "/sandboxes/" + uuid.NewString() + "/timeout", `{"timeout":60}`},
		{http.MethodPost, "/sandboxes/" + native.ID + "/timeout", `{"timeout":60}`},
		{http.MethodPost, "/sandboxes/" + creating.ID + "/timeout", `{"timeout":60}`},
		{http.MethodPost, "/sandboxes/" + uuid.NewString() + "/refreshes", `{}`},
		{http.MethodPost, "/sandboxes/" + native.ID + "/refreshes", `{}`},
		{http.MethodPost, "/sandboxes/" + creating.ID + "/refreshes", `{}`},
	} {
		resp, data := doRequest(t, server, tc.method, tc.path, tc.body, true)
		if resp.StatusCode != 404 && resp.StatusCode != 409 {
			t.Fatalf("%s %s = %d %s, want 404/409", tc.method, tc.path, resp.StatusCode, data)
		}
	}
}

// proxyGet sends a data-plane request through the Node server with the
// explicit E2B routing headers.
func proxyGet(t *testing.T, server *httptest.Server, sandboxID, port, path string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set(sandboxproxy.E2BSandboxIDHeader, sandboxID)
	req.Header.Set(sandboxproxy.E2BTargetPortHeader, port)
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

// TestWakeClassification covers the wake error surface of data-plane misses:
// unknown and native sandboxes answer 404, non-resumable states 503.
func TestWakeClassification(t *testing.T) {
	service, routes := testRuntime(t)
	runtime := &fakeSandboxRuntime{store: service.Store}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	native := seedRecord(t, service.Store, true, false, "")
	creating := seedRecord(t, service.Store, false, true, "")
	paused := seedPausedRecord(t, service.Store, digest.FromString("head-wake-a").String())
	armed := seedPausedRecord(t, service.Store, digest.FromString("head-wake-b").String())
	armed.AutoResume = true
	if _, err := service.Store.Update(t.Context(), armed); err != nil {
		t.Fatal(err)
	}
	stranded := seedPausedRecord(t, service.Store, digest.FromString("head-wake-c").String())
	stranded.State = sandbox.StateUnknown
	stranded.LastError = "paused write failure"
	if _, err := service.Store.Update(t.Context(), stranded); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		id     string
		status int
	}{
		{"unknown", uuid.NewString(), 404},
		{"native", native.ID, 404},
		{"creating", creating.ID, 503},
		{"paused without autoResume", paused.ID, 503},
		{"armed but wake runtime not configured", armed.ID, 503},
		{"stranded without autoResume", stranded.ID, 503},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := proxyGet(t, server, tc.id, "80", "/proxy/health")
			if resp.StatusCode != tc.status {
				t.Fatalf("wake %s = %d %s, want %d", tc.name, resp.StatusCode, data, tc.status)
			}
		})
	}
	if calls := runtime.wakeCalls(); len(calls) != 6 {
		t.Fatalf("wake dispatch = %+v, want one call per miss", calls)
	}
}

// TestAutoResumeWakeRestoresPausedSandbox covers the full data-plane wake
// path: a proxied request to a paused auto-resume sandbox restores it with the
// wake TTL floor, serves the guest, and later requests reuse the route.
func TestAutoResumeWakeRestoresPausedSandbox(t *testing.T) {
	service, routes := testRuntime(t)
	guest := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "guest:"+r.URL.Path)
	}))
	defer guest.Close()
	u, _ := url.Parse(guest.URL)
	ip, port, _ := net.SplitHostPort(u.Host)
	runtime := &fakeSandboxRuntime{store: service.Store, routes: routes, wakeIP: ip}
	service.Sandbox = runtime
	server := testServer(t, service, routes)

	paused := seedPausedRecord(t, service.Store, digest.FromString("head-wake").String())
	paused.AutoResume = true
	if _, err := service.Store.Update(t.Context(), paused); err != nil {
		t.Fatal(err)
	}
	resp, data := proxyGet(t, server, paused.ID, port, "/proxy/health")
	if resp.StatusCode != http.StatusOK || string(data) != "guest:/health" {
		t.Fatalf("woken proxy = %d %s", resp.StatusCode, data)
	}
	woken, err := service.Store.Get(t.Context(), paused.ID)
	if err != nil || woken.State != sandbox.StateReady || !woken.AutoResume || woken.IP != ip {
		t.Fatalf("woken record = %+v (%v)", woken, err)
	}
	if remaining := time.Until(time.Unix(0, woken.ExpiresAt)); remaining < 290*time.Second || remaining > 300*time.Second {
		t.Fatalf("wake TTL = %v, want the 300s autoResume floor", remaining)
	}
	if calls := runtime.wakeCalls(); len(calls) != 1 || calls[0].id != paused.ID || calls[0].ttl != 300*time.Second {
		t.Fatalf("wake dispatch = %+v", calls)
	}
	if _, ok := routes.Lookup(paused.ID); !ok {
		t.Fatal("woken sandbox has no published route")
	}
	// A second request reuses the route without another wake.
	resp, data = proxyGet(t, server, paused.ID, port, "/proxy/files")
	if resp.StatusCode != http.StatusOK || string(data) != "guest:/files" {
		t.Fatalf("second proxy = %d %s", resp.StatusCode, data)
	}
	if calls := runtime.wakeCalls(); len(calls) != 1 {
		t.Fatalf("second request woke again: %+v", calls)
	}
	// A control-plane EnsureRunning on the READY sandbox is a no-op that does
	// not touch the wake-granted deadline.
	if err := service.EnsureSandboxRunning(t.Context(), paused.ID, 5*time.Minute); err != nil {
		t.Fatalf("ready EnsureRunning = %v", err)
	}
	after, err := service.Store.Get(t.Context(), paused.ID)
	if err != nil || after.ExpiresAt != woken.ExpiresAt {
		t.Fatalf("ready EnsureRunning touched the deadline: %d -> %d", woken.ExpiresAt, after.ExpiresAt)
	}
	if calls := runtime.wakeCalls(); len(calls) != 2 {
		t.Fatalf("ready EnsureRunning dispatch = %+v", calls)
	}
}
