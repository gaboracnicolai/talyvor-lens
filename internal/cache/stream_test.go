package cache

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

// ─── normalisation ───────────────────────────────

func TestNormalizeAnthropicChunk_ContentDelta(t *testing.T) {
	raw := `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`
	chunk, ok := NormalizeAnthropicChunk(raw)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if chunk.Text != "hello" {
		t.Fatalf("expected text=hello, got %q", chunk.Text)
	}
	if chunk.IsFinal {
		t.Fatal("delta should not be final")
	}
}

func TestNormalizeAnthropicChunk_MessageStop(t *testing.T) {
	raw := `event: message_stop
data: {"type":"message_stop"}`
	chunk, ok := NormalizeAnthropicChunk(raw)
	if !ok || !chunk.IsFinal {
		t.Fatalf("expected final chunk, got %+v ok=%v", chunk, ok)
	}
}

func TestNormalizeAnthropicChunk_DropsPingEvents(t *testing.T) {
	raw := `event: ping
data: {"type":"ping"}`
	_, ok := NormalizeAnthropicChunk(raw)
	if ok {
		t.Fatal("ping events should be dropped")
	}
}

func TestNormalizeOpenAIChunk_ContentDelta(t *testing.T) {
	raw := `data: {"choices":[{"delta":{"content":"world"}}]}`
	chunk, ok := NormalizeOpenAIChunk(raw)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if chunk.Text != "world" {
		t.Fatalf("expected text=world, got %q", chunk.Text)
	}
	if chunk.IsFinal {
		t.Fatal("partial delta should not be final")
	}
}

func TestNormalizeOpenAIChunk_FinishReason(t *testing.T) {
	raw := `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`
	chunk, ok := NormalizeOpenAIChunk(raw)
	if !ok || !chunk.IsFinal {
		t.Fatalf("expected final, got %+v ok=%v", chunk, ok)
	}
}

func TestNormalizeOpenAIChunk_DoneSentinel(t *testing.T) {
	raw := "data: [DONE]"
	chunk, ok := NormalizeOpenAIChunk(raw)
	if !ok || !chunk.IsFinal {
		t.Fatalf("expected [DONE] to map to final, got %+v ok=%v", chunk, ok)
	}
}

// ─── serialisation ──────────────────────────────

func TestToAnthropicSSE_Delta(t *testing.T) {
	s := ToAnthropicSSE(StreamChunk{Text: "hi"}, false)
	if !strings.Contains(s, `"text":"hi"`) {
		t.Fatalf("expected text in payload: %q", s)
	}
	if !strings.HasSuffix(s, "\n\n") {
		t.Fatal("missing SSE \\n\\n terminator")
	}
}

func TestToAnthropicSSE_Final(t *testing.T) {
	s := ToAnthropicSSE(StreamChunk{IsFinal: true}, true)
	if !strings.Contains(s, "message_stop") {
		t.Fatalf("expected message_stop event: %q", s)
	}
}

func TestToOpenAISSE_Delta(t *testing.T) {
	s := ToOpenAISSE(StreamChunk{Text: "tok"}, false)
	if !strings.Contains(s, `"content":"tok"`) {
		t.Fatalf("expected content in payload: %q", s)
	}
}

func TestToOpenAISSE_FinalIncludesDONE(t *testing.T) {
	s := ToOpenAISSE(StreamChunk{}, true)
	if !strings.Contains(s, "[DONE]") {
		t.Fatalf("expected [DONE] sentinel: %q", s)
	}
}

// ─── replay ──────────────────────────────────────

func sampleEntry() *StreamEntry {
	return &StreamEntry{
		Provider: "openai",
		Model:    "test",
		Chunks: []StreamChunk{
			{Text: "Hello, ", DelayMs: 100},
			{Text: "world", DelayMs: 200},
			{Text: "!", DelayMs: 300, IsFinal: true},
		},
	}
}

func TestReplayStream_InstantSendsAllChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := ReplayStream(context.Background(), sampleEntry(), rec, "openai", ReplayInstant); err != nil {
		t.Fatalf("ReplayStream: %v", err)
	}
	body := rec.Body.String()
	for _, want := range []string{"Hello, ", "world", "!", "[DONE]"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q\nbody:\n%s", want, body)
		}
	}
}

func TestReplayStream_FastCapsDelay(t *testing.T) {
	entry := &StreamEntry{
		Provider: "openai",
		Chunks: []StreamChunk{
			{Text: "a"},
			{Text: "b", DelayMs: 5_000}, // 5s in original
			{Text: "c", DelayMs: 5_000, IsFinal: true},
		},
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	if err := ReplayStream(context.Background(), entry, rec, "openai", ReplayFast); err != nil {
		t.Fatalf("ReplayStream: %v", err)
	}
	elapsed := time.Since(start)
	// 2 chunks × 50ms cap = 100ms — give some headroom.
	if elapsed > 250*time.Millisecond {
		t.Fatalf("fast replay too slow: %v", elapsed)
	}
}

func TestReplayStream_RealisticUsesOriginalDelay(t *testing.T) {
	entry := &StreamEntry{
		Provider: "openai",
		Chunks: []StreamChunk{
			{Text: "a"},
			{Text: "b", DelayMs: 100, IsFinal: true},
		},
	}
	rec := httptest.NewRecorder()
	start := time.Now()
	if err := ReplayStream(context.Background(), entry, rec, "openai", ReplayRealistic); err != nil {
		t.Fatalf("ReplayStream: %v", err)
	}
	elapsed := time.Since(start)
	if elapsed < 80*time.Millisecond {
		t.Fatalf("realistic replay too fast (~100ms expected): %v", elapsed)
	}
}

func TestReplayStream_ContextCancellationAborts(t *testing.T) {
	entry := &StreamEntry{
		Provider: "openai",
		Chunks: []StreamChunk{
			{Text: "a"},
			{Text: "b", DelayMs: 5_000},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	rec := httptest.NewRecorder()
	start := time.Now()
	err := ReplayStream(ctx, entry, rec, "openai", ReplayRealistic)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error after cancellation")
	}
	if elapsed > 500*time.Millisecond {
		t.Fatalf("cancellation should abort fast, took %v", elapsed)
	}
}

// ─── store round-trip ────────────────────────────

func setupRedis(t *testing.T) *redis.Client {
	t.Helper()
	mr, err := miniredis.Run()
	if err != nil {
		t.Fatalf("miniredis.Run: %v", err)
	}
	t.Cleanup(mr.Close)
	rc := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rc.Close() })
	return rc
}

func TestSetGetStream_RoundTrip(t *testing.T) {
	rdb := setupRedis(t)
	ctx := context.Background()
	in := StreamEntry{
		Provider: "anthropic",
		Model:    "claude-haiku-4-5",
		Chunks: []StreamChunk{
			{Text: "hi", TokenDelta: 1, DelayMs: 50},
			{Text: "!", TokenDelta: 1, DelayMs: 30, IsFinal: true},
		},
		TotalTokens: 2,
		CreatedAt:   time.Now().UTC(),
	}
	if err := SetStream(ctx, rdb, "key1", in, time.Minute); err != nil {
		t.Fatalf("SetStream: %v", err)
	}
	got, err := GetStream(ctx, rdb, "key1")
	if err != nil {
		t.Fatalf("GetStream: %v", err)
	}
	if got == nil {
		t.Fatal("expected non-nil entry")
	}
	if got.Provider != "anthropic" || len(got.Chunks) != 2 || got.Chunks[1].Text != "!" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
}

func TestSetStream_RejectsOversize(t *testing.T) {
	rdb := setupRedis(t)
	big := strings.Repeat("x", MaxStreamEntryBytes+1)
	entry := StreamEntry{
		Provider: "openai",
		Chunks:   []StreamChunk{{Text: big}},
	}
	err := SetStream(context.Background(), rdb, "huge", entry, time.Minute)
	if err == nil {
		t.Fatal("expected rejection for > 512KB payload")
	}
}

func TestGetStream_MissReturnsNilNoError(t *testing.T) {
	rdb := setupRedis(t)
	got, err := GetStream(context.Background(), rdb, "missing")
	if err != nil {
		t.Fatalf("expected nil error on miss, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil entry on miss, got %+v", got)
	}
}

// ─── JSON shape ──────────────────────────────────

func TestStreamEntry_JSONSerializes(t *testing.T) {
	in := StreamEntry{
		Provider:    "openai",
		Model:       "gpt-4o-mini",
		TotalTokens: 42,
		Chunks: []StreamChunk{
			{Text: "ab", TokenDelta: 1, DelayMs: 50, IsFinal: true},
		},
	}
	buf, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out StreamEntry
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Model != "gpt-4o-mini" || out.TotalTokens != 42 || len(out.Chunks) != 1 {
		t.Fatalf("round-trip mismatch: %+v", out)
	}
}

// ─── FromNonStreaming ────────────────────────────

func TestFromNonStreaming_WrapsBodyAsSingleChunk(t *testing.T) {
	entry := FromNonStreaming("openai", "test", []byte("complete response"))
	if len(entry.Chunks) != 1 {
		t.Fatalf("expected 1 chunk, got %d", len(entry.Chunks))
	}
	if !entry.Chunks[0].IsFinal {
		t.Fatal("single chunk should be marked final")
	}
	if entry.Chunks[0].Text != "complete response" {
		t.Fatalf("unexpected text: %q", entry.Chunks[0].Text)
	}
}
