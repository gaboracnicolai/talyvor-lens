package proxy

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/talyvor/lens/internal/auth"
	"github.com/talyvor/lens/internal/distill"
	"github.com/talyvor/lens/internal/documents"
	"github.com/talyvor/lens/internal/workspace"
)

type realDistill struct{}

func (realDistill) Convert(ctx context.Context, in []byte, f distill.Format) (distill.Result, error) {
	return distill.DistillAs(ctx, in, f)
}

const pptxMediaType = "application/vnd.openxmlformats-officedocument.presentationml.presentation"

// bigDeck is a real .pptx of about 20 MB: two slides of text and a 20 MB picture, as decks are.
func bigDeck(t *testing.T) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	put := func(name, body string, raw []byte) {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: name, Method: zip.Store})
		if err != nil {
			t.Fatal(err)
		}
		if raw == nil {
			raw = []byte(body)
		}
		if _, err := w.Write(raw); err != nil {
			t.Fatal(err)
		}
	}
	put("ppt/presentation.xml", `<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst><p:sldId id="256" r:id="rId1"/><p:sldId id="257" r:id="rId2"/></p:sldIdLst></p:presentation>`, nil)
	put("ppt/_rels/presentation.xml.rels", `<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Target="slides/slide1.xml"/><Relationship Id="rId2" Target="slides/slide2.xml"/></Relationships>`, nil)
	for n, s := range [][2]string{{"Revenue", "Revenue grew 40% in the third quarter"}, {"Next steps", "Hire two engineers"}} {
		put(fmt.Sprintf("ppt/slides/slide%d.xml", n+1), `<p:sld xmlns:p="p" xmlns:a="a"><p:cSld><p:spTree>`+
			`<p:sp><p:nvSpPr><p:nvPr><p:ph type="title"/></p:nvPr></p:nvSpPr><p:txBody><a:p><a:r><a:t>`+s[0]+`</a:t></a:r></a:p></p:txBody></p:sp>`+
			`<p:sp><p:txBody><a:p><a:r><a:t>`+s[1]+`</a:t></a:r></a:p></p:txBody></p:sp></p:spTree></p:cSld></p:sld>`, nil)
	}
	picture := make([]byte, 20<<20)
	_, _ = rand.Read(picture)
	put("ppt/media/image1.png", "", picture)
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// B18.13 — through the real handlers: a 20 MB slide deck uploaded by POST /v1/documents is referenced
// by id in a chat request, converted to its slides' text on the way to the model, and answered; the
// response says what the conversion saved — on the buffered and the streamed path alike. The id is
// read for the uploading workspace only.
func TestDocuments_A20MBSlideDeckUploadedByTheRouteIsConvertedAndAnswered(t *testing.T) {
	pool := agentBankDB(t)
	store := documents.NewStore(pool)
	deck := bigDeck(t)

	up := httptest.NewRequest(http.MethodPost, "/v1/documents?filename=q3.pptx", bytes.NewReader(deck))
	up.Header.Set("Content-Type", pptxMediaType)
	up = up.WithContext(auth.WithAuthContext(up.Context(), &auth.AuthContext{WorkspaceID: "ws-log"}))
	uw := httptest.NewRecorder()
	documents.UploadHandler(store)(uw, up)
	var doc documents.Document
	if uw.Code != http.StatusCreated || json.Unmarshal(uw.Body.Bytes(), &doc) != nil || !strings.HasPrefix(doc.ID, documents.IDPrefix) {
		t.Fatalf("upload = %d %s", uw.Code, uw.Body.String())
	}
	if _, _, err := store.Content(context.Background(), "ws-other", doc.ID); !errors.Is(err, documents.ErrNotFound) {
		t.Fatalf("another workspace read the document: %v", err)
	}

	var mu sync.Mutex
	var sent []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		sent = raw
		mu.Unlock()
		if bytes.Contains(raw, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":40,\"output_tokens\":1}}}\n\n"+
				"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"Revenue grew 40%.\"}}\n\n"+
				"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"Revenue grew 40%."}],"usage":{"input_tokens":40,"output_tokens":6}}`)
	}))
	t.Cleanup(upstream.Close)

	for _, stream := range []bool{false, true} {
		// The workspace's distill policy is off: a document referenced by id is converted regardless,
		// because the id means nothing to the provider.
		p, _ := newDistillSpendProxy(t, realDistill{}, "", workspace.DistillDisabled)
		p.anthropicURL = upstream.URL
		p.SetDocuments(store)
		body, _ := json.Marshal(map[string]any{"model": "claude-x", "stream": stream, "max_tokens": 100, "messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "document", "source": map[string]any{"type": "file", "file_id": doc.ID}},
				map[string]any{"type": "text", "text": "What grew?"},
			}},
		}})
		w := dispatchAnthropicDoc(t, p, body)

		mu.Lock()
		got := string(sent)
		mu.Unlock()
		if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Revenue grew 40%.") {
			t.Fatalf("stream=%v: %d %s", stream, w.Code, w.Body.String())
		}
		if !strings.Contains(got, "Slide 1 — Revenue") || !strings.Contains(got, "Hire two engineers") || strings.Contains(got, doc.ID) {
			t.Errorf("stream=%v: the model was sent %q, want the deck's slides and not the document id", stream, got)
		}
		tokens, errT := strconv.Atoi(w.Header().Get("X-Talyvor-Distill-Tokens-Saved"))
		bytesSaved, errB := strconv.Atoi(w.Header().Get("X-Talyvor-Distill-Bytes-Saved"))
		if w.Header().Get("X-Talyvor-Distill") != "applied" || errT != nil || tokens < 0 || errB != nil || bytesSaved < 20<<20 {
			t.Errorf("stream=%v: distill headers %v — want applied, the tokens saved and over 20 MB of bytes saved", stream, w.Header())
		}
	}
}
