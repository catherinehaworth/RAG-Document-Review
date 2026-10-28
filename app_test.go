package main

import (
	"archive/zip"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

func TestExtractDocumentTextSupportsLibraryFormats(t *testing.T) {
	tests := []struct {
		name     string
		filename string
		content  []byte
		contains []string
	}{
		{"text", "notes.txt", []byte("plain text"), []string{"plain text"}},
		{"html", "page.html", []byte("<html><script>hidden secret</script><body>Hello &amp; welcome</body></html>"), []string{"Hello & welcome"}},
		{"word", "report.docx", makeOfficeArchive(t, map[string]string{"word/document.xml": `<w:document xmlns:w="x"><w:p><w:r><w:t>Decision approved</w:t></w:r></w:p></w:document>`}), []string{"Decision approved"}},
		{"excel", "report.xlsx", makeOfficeArchive(t, map[string]string{
			"xl/sharedStrings.xml":     `<sst xmlns="x"><si><t>Quarter</t></si><si><t>Revenue</t></si></sst>`,
			"xl/styles.xml":            `<styleSheet xmlns="x"><cellXfs count="1"><xf numFmtId="14"/></cellXfs></styleSheet>`,
			"xl/worksheets/sheet1.xml": `<worksheet xmlns="x"><sheetData><row><c t="s"><v>0</v></c><c t="s"><v>1</v></c><c><v>42</v></c><c s="0"><v>46000</v></c></row></sheetData></worksheet>`,
		}), []string{"Quarter", "Revenue", "42", "2025-12-09"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, _, err := extractDocumentText(test.filename, test.content)
			if err != nil {
				t.Fatal(err)
			}
			for _, expected := range test.contains {
				if !strings.Contains(got, expected) {
					t.Errorf("extracted text %q does not contain %q", got, expected)
				}
			}
			if test.name == "html" && strings.Contains(got, "hidden secret") {
				t.Errorf("script content should not be indexed: %q", got)
			}
		})
	}
}

func makeOfficeArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for name, content := range entries {
		file, err := writer.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}

func TestFilenameCategoriesAndSourceMetadataContext(t *testing.T) {
	categories := []Category{{Name: "Policies", Keyword: "policy"}, {Name: "Minutes", Keyword: "minutes"}}
	if got := classifyFilename("2026_Policy_Review.docx", categories); got != "Policies" {
		t.Fatalf("classifyFilename() = %q, want Policies", got)
	}
	dates, people := extractedMetadata("On 2026-03-14, Jordan Lee approved the draft.")
	if len(dates) != 1 || dates[0] != "2026-03-14" || len(people) != 1 || people[0] != "Jordan Lee" {
		t.Fatalf("unexpected extracted metadata: dates=%v people=%v", dates, people)
	}
	source := DocumentSource{Filename: "policy.docx", Text: "Approved.", Dates: dates, People: people}
	context := formatSourceContext(source)
	for _, expected := range []string{"policy.docx", "2026-03-14", "Jordan Lee"} {
		if !strings.Contains(context, expected) {
			t.Errorf("source context %q does not include %q", context, expected)
		}
	}
}

func TestSelectedSourceAnswerRetainsLocalConversationContext(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	model := &fakeModel{embedding: []float32{0.4, 0.6}, answer: "Jordan Lee approved it on 2026-03-14."}
	s := newTestServer(t, model, db)
	session := strings.Repeat("a", 48)
	mock.ExpectQuery("SELECT c\\.text,d\\.filename,d\\.id.*LIMIT 5").
		WithArgs("[0.4,0.6]").
		WillReturnRows(sqlmock.NewRows([]string{"text", "filename", "id", "metadata_dates", "metadata_people", "score"}).
			AddRow("Approved by Jordan Lee on 2026-03-14.", "policy.docx", 4, `["2026-03-14"]`, `["Jordan Lee"]`, 0.82))
	mock.ExpectQuery("SELECT question,answer FROM conversation_messages").
		WithArgs(session).
		WillReturnRows(sqlmock.NewRows([]string{"question", "answer"}).AddRow("Who approved it?", "Jordan Lee approved it."))
	mock.ExpectQuery("INSERT INTO conversation_messages").
		WithArgs(session, "When?", "Jordan Lee approved it on 2026-03-14.").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(15))
	answer, err := s.answerQuestionWithOptions(context.Background(), "When?", AskOptions{Session: session, SearchAll: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(answer.Sources) != 1 || answer.Sources[0].Filename != "policy.docx" || len(model.lastContext) != 2 {
		t.Fatalf("unexpected answer sources or context: answer=%#v context=%#v", answer, model.lastContext)
	}
	if !strings.Contains(model.lastContext[0], "Who approved it?") || !strings.Contains(model.lastContext[1], "2026-03-14") {
		t.Fatalf("conversation or source metadata missing from model context: %#v", model.lastContext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

type fakeModel struct {
	embedding   []float32
	embedErr    error
	answer      string
	answerErr   error
	lastContext []string
}

func (m *fakeModel) Embed(context.Context, string) ([]float32, error) {
	if m.embedErr != nil {
		return nil, m.embedErr
	}
	return append([]float32(nil), m.embedding...), nil
}

func (m *fakeModel) Answer(_ context.Context, _ string, contexts []string) (string, error) {
	m.lastContext = append([]string(nil), contexts...)
	return m.answer, m.answerErr
}

func newTestServer(t *testing.T, model ModelProvider, db *sql.DB) *server {
	t.Helper()
	return &server{
		db: db,
		cfg: Config{
			Env: "development", SessionSecret: "test-session-secret",
			AdminUsername: "test-admin", AdminPassword: "test-password",
			MaxUploadBytes: 1024, EmbeddingDimensions: 2, LLMProvider: "openai",
		},
		model: model,
	}
}

func newMultipartUpload(t *testing.T, filename string, content []byte) (*bytes.Buffer, string) {
	t.Helper()
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}

func TestUploadStoresDocumentAndChunksAtomically(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	content := []byte("The handbook says keep a backup.")
	model := &fakeModel{embedding: []float32{0.25, 0.75}}
	s := newTestServer(t, model, db)
	body, contentType := newMultipartUpload(t, "handbook.txt", content)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT name, filename_keyword FROM categories ORDER BY length(filename_keyword) DESC, name`)).
		WillReturnRows(sqlmock.NewRows([]string{"name", "filename_keyword"}))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO documents(filename, content, file_size, mime_type, status, category, metadata_dates, metadata_people) VALUES($1,$2,$3,$4,'ready',$5,$6,$7) RETURNING id`)).
		WithArgs("handbook.txt", string(content), int64(len(content)), "text/plain; charset=utf-8", "", "[]", "[]").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(12))
	mock.ExpectExec("INSERT INTO chunks").
		WithArgs(12, 0, string(content), "[0.25,0.75]").
		WillReturnResult(sqlmock.NewResult(1, 1))
	mock.ExpectCommit()

	req := httptest.NewRequest(http.MethodPost, "/documents/new", body)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Origin", "http://example.test")
	req.Host = "example.test"
	req.AddCookie(&http.Cookie{
		Name: "session", Value: buildSessionToken("test-admin", s.cfg.SessionSecret, time.Now().Add(time.Hour)),
	})
	recorder := httptest.NewRecorder()
	s.requireAuth(s.handleUploadDocument).ServeHTTP(recorder, req)

	if recorder.Code != http.StatusSeeOther || recorder.Header().Get("Location") != "/documents?status=success" {
		t.Fatalf("unexpected upload response: status=%d location=%q body=%s", recorder.Code, recorder.Header().Get("Location"), recorder.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestUploadRejectsOversizeAndInvalidText(t *testing.T) {
	tests := []struct {
		name       string
		filename   string
		content    []byte
		wantStatus int
		wantText   string
	}{
		{"oversize", "large.txt", []byte("this content is much too large"), http.StatusRequestEntityTooLarge, "too large"},
		{"binary-utf8", "invalid.txt", []byte{0xff, 0xfe}, http.StatusBadRequest, "UTF-8"},
		{"wrong-extension", "document.pdf", []byte("text"), http.StatusBadRequest, ".txt"},
		{"empty", "empty.txt", []byte{}, http.StatusBadRequest, "empty"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTestServer(t, &fakeModel{}, nil)
			s.cfg.MaxUploadBytes = 16
			body, contentType := newMultipartUpload(t, test.filename, test.content)
			req := httptest.NewRequest(http.MethodPost, "/documents/new", body)
			req.Header.Set("Content-Type", contentType)
			recorder := httptest.NewRecorder()
			_, _, _, _, status, err := s.readUploadedText(recorder, req)
			if err == nil || status != test.wantStatus || !strings.Contains(err.Error(), test.wantText) {
				t.Fatalf("readUploadedText() = status %d, err %v; want status %d containing %q", status, err, test.wantStatus, test.wantText)
			}
		})
	}
}

func TestFailedReplacementDoesNotModifyStoredDocument(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := newTestServer(t, &fakeModel{embedErr: errors.New("model unavailable")}, db)
	err = s.replaceDocument(context.Background(), 7, "new.txt", "new content", "text/plain", 11)
	if err == nil || !strings.Contains(err.Error(), "model unavailable") {
		t.Fatalf("replaceDocument() error = %v, want embedding failure", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("replacement touched the database before embeddings succeeded: %v", err)
	}
}

func TestReplacementDatabaseFailureRollsBackOldVersion(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := newTestServer(t, &fakeModel{embedding: []float32{0.25, 0.75}}, db)
	mock.ExpectBegin()
	mock.ExpectExec("UPDATE documents SET filename").
		WithArgs("replacement.txt", "replacement text", int64(16), "text/plain", 7).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM chunks WHERE document_id").
		WithArgs(7).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("INSERT INTO chunks").
		WithArgs(7, 0, "replacement text", "[0.25,0.75]").
		WillReturnError(errors.New("simulated insert failure"))
	mock.ExpectRollback()

	err = s.replaceDocument(context.Background(), 7, "replacement.txt", "replacement text", "text/plain", 16)
	if err == nil {
		t.Fatal("expected replacement transaction to fail")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetrievalUsesNearestPassagesAndIncludesSources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	model := &fakeModel{embedding: []float32{0.4, 0.6}, answer: "The retention period is 30 days."}
	s := newTestServer(t, model, db)
	mock.ExpectQuery("SELECT c\\.text, d\\.filename.*LIMIT 5").
		WithArgs("[0.4,0.6]").
		WillReturnRows(sqlmock.NewRows([]string{"text", "filename", "score"}).
			AddRow("Records are retained for 30 days.", "policy.txt", 0.82))

	answer, err := s.answerQuestion(context.Background(), "How long are records kept?")
	if err != nil {
		t.Fatal(err)
	}
	if answer.Answer != "The retention period is 30 days." || len(answer.Sources) != 1 {
		t.Fatalf("unexpected answer payload: %#v", answer)
	}
	if answer.Sources[0].Filename != "policy.txt" || !strings.Contains(answer.Sources[0].Text, "30 days") {
		t.Fatalf("source passage was not returned: %#v", answer.Sources[0])
	}
	if len(model.lastContext) != 1 || model.lastContext[0] != answer.Sources[0].Text {
		t.Fatalf("LLM context does not match retrieved passage: %#v", model.lastContext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetrievalDeclinesLowSimilarityWithoutCallingChatModel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	model := &fakeModel{embedding: []float32{0.4, 0.6}}
	s := newTestServer(t, model, db)
	mock.ExpectQuery("SELECT c\\.text, d\\.filename.*LIMIT 5").
		WithArgs("[0.4,0.6]").
		WillReturnRows(sqlmock.NewRows([]string{"text", "filename", "score"}).
			AddRow("Unrelated text.", "other.txt", 0.05))

	answer, err := s.answerQuestion(context.Background(), "What is the retention period?")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(answer.Answer, "couldn’t find enough relevant information") {
		t.Fatalf("expected no-information fallback, got %q", answer.Answer)
	}
	if model.lastContext != nil {
		t.Fatalf("chat model should not receive low-similarity context: %#v", model.lastContext)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestRetrievalRejectsIncompatibleEmbeddingDimensions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := newTestServer(t, &fakeModel{embedding: []float32{0.4}}, db)
	if _, err := s.answerQuestion(context.Background(), "Question"); err == nil ||
		!strings.Contains(err.Error(), "configured for 2") {
		t.Fatalf("answerQuestion() error = %v, want dimension mismatch", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("retrieval queried the database with an incompatible vector: %v", err)
	}
}

func TestQuestionAPIAcceptsJSONAndReturnsJSONSources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	s := newTestServer(t, &fakeModel{embedding: []float32{0.4, 0.6}, answer: "Answer from source."}, db)
	mock.ExpectQuery("SELECT c\\.text,d\\.filename,d\\.id.*LIMIT 5").
		WithArgs("[0.4,0.6]").
		WillReturnRows(sqlmock.NewRows([]string{"text", "filename", "id", "metadata_dates", "metadata_people", "score"}).
			AddRow("Relevant text.", "source.txt", 12, "[]", "[]", 0.8))
	mock.ExpectQuery("SELECT question,answer FROM conversation_messages").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"question", "answer"}))
	mock.ExpectQuery("INSERT INTO conversation_messages").
		WithArgs(sqlmock.AnyArg(), "What does it say?", "Answer from source.").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(1))

	req := httptest.NewRequest(http.MethodPost, "/api/ask", strings.NewReader(`{"question":"What does it say?"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://example.test")
	req.Host = "example.test"
	req.AddCookie(&http.Cookie{
		Name: "session", Value: buildSessionToken("test-admin", s.cfg.SessionSecret, time.Now().Add(time.Hour)),
	})
	recorder := httptest.NewRecorder()
	s.requireAuth(s.handleAskJSON).ServeHTTP(recorder, req)

	var response struct {
		Answer  string           `json:"answer"`
		Sources []DocumentSource `json:"sources"`
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("API status = %d, response = %s", recorder.Code, recorder.Body.String())
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Answer != "Answer from source." || len(response.Sources) != 1 || response.Sources[0].Filename != "source.txt" {
		t.Fatalf("unexpected API response: %#v", response)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestSessionTokensExpireAndSafeNextRejectsExternalRedirects(t *testing.T) {
	now := time.Now()
	token := buildSessionToken("admin", "signing-key", now.Add(time.Hour))
	if !validateSessionToken(token, "signing-key", "admin", now) {
		t.Fatal("valid session token was rejected")
	}
	if validateSessionToken(token, "wrong-key", "admin", now) {
		t.Fatal("token signed with a different key was accepted")
	}
	if validateSessionToken(token, "signing-key", "admin", now.Add(2*time.Hour)) {
		t.Fatal("expired token was accepted")
	}
	for _, candidate := range []string{"https://example.com", "//example.com", "/\\example.com", ""} {
		if got := safeNext(candidate); got != "/documents" {
			t.Errorf("safeNext(%q) = %q, want /documents", candidate, got)
		}
	}
	if got := safeNext("/ask?topic=policy"); got != "/ask?topic=policy" {
		t.Errorf("safeNext() dropped a safe local path: %q", got)
	}
}

func TestSameOriginRequestRequiresMatchingOrigin(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/documents/new", nil)
	req.Host = "example.test"
	req.Header.Set("Origin", "https://example.test")
	if !sameOriginRequest(req) {
		t.Fatal("same-origin request was rejected")
	}
	req.Header.Set("Origin", "https://attacker.test")
	if sameOriginRequest(req) {
		t.Fatal("cross-origin request was accepted")
	}
	req.Header.Del("Origin")
	if sameOriginRequest(req) {
		t.Fatal("request without origin or referer was accepted")
	}
}

func TestProtectedRouteRejectsMissingAuthAndCrossOriginMutation(t *testing.T) {
	s := newTestServer(t, &fakeModel{}, nil)
	handler := s.requireAuth(s.handleDeleteDocument)

	unauthenticated := httptest.NewRequest(http.MethodPost, "/documents/delete", strings.NewReader("id=1"))
	unauthenticated.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unauthenticated.Host = "example.test"
	unauthenticatedRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticatedRecorder, unauthenticated)
	if unauthenticatedRecorder.Code != http.StatusSeeOther ||
		!strings.HasPrefix(unauthenticatedRecorder.Header().Get("Location"), "/login?next=") {
		t.Fatalf("unauthenticated request response: %d %q", unauthenticatedRecorder.Code, unauthenticatedRecorder.Header().Get("Location"))
	}

	crossOrigin := httptest.NewRequest(http.MethodPost, "/documents/delete", strings.NewReader("id=1"))
	crossOrigin.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	crossOrigin.Header.Set("Origin", "https://attacker.test")
	crossOrigin.Host = "example.test"
	crossOrigin.AddCookie(&http.Cookie{
		Name: "session", Value: buildSessionToken("test-admin", s.cfg.SessionSecret, time.Now().Add(time.Hour)),
	})
	crossOriginRecorder := httptest.NewRecorder()
	handler.ServeHTTP(crossOriginRecorder, crossOrigin)
	if crossOriginRecorder.Code != http.StatusForbidden {
		t.Fatalf("cross-origin mutation status = %d, want %d", crossOriginRecorder.Code, http.StatusForbidden)
	}
}

func TestChunkingHonorsByteLimitForLongUnicodeText(t *testing.T) {
	input := strings.Repeat("界", 700)
	chunks := splitTextIntoChunks(input, 500)
	if len(chunks) < 2 {
		t.Fatalf("long unbroken text was not split: %d chunks", len(chunks))
	}
	joined := strings.Join(chunks, "")
	if joined != input {
		t.Fatal("chunking changed the source text")
	}
	for i, chunk := range chunks {
		if len(chunk) > 500 {
			t.Errorf("chunk %d is %d bytes; maximum is 500", i, len(chunk))
		}
	}
	if splitTextIntoChunks("content", 0) != nil {
		t.Fatal("invalid chunk size should not create chunks")
	}
}

func TestOllamaProviderUsesLocalEndpoints(t *testing.T) {
	var seenEmbed, seenChat bool
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/embed":
			seenEmbed = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"embeddings":[[0.1,0.9]]}`)
		case "/api/chat":
			seenChat = true
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"message":{"content":"Local answer."}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer local.Close()

	model, err := newModelProvider(Config{
		LLMProvider: "ollama", LLMBaseURL: local.URL,
		EmbeddingModel: "nomic-embed-text", ChatModel: "llama3.2",
	})
	if err != nil {
		t.Fatal(err)
	}
	embedding, err := model.Embed(context.Background(), "sample")
	if err != nil || len(embedding) != 2 || embedding[1] != 0.9 {
		t.Fatalf("local embedding result = %v, err = %v", embedding, err)
	}
	answer, err := model.Answer(context.Background(), "question", []string{"context"})
	if err != nil || answer != "Local answer." {
		t.Fatalf("local answer = %q, err = %v", answer, err)
	}
	if !seenEmbed || !seenChat {
		t.Fatalf("did not call both local Ollama endpoints: embed=%v chat=%v", seenEmbed, seenChat)
	}
}

func TestOpenAICompatibleProviderUsesEmbeddingsAndChat(t *testing.T) {
	var seenEmbedding, seenChat bool
	hosted := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			t.Errorf("authorization header = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/embeddings":
			seenEmbedding = true
			_, _ = io.WriteString(w, `{"data":[{"embedding":[0.2,0.8]}]}`)
		case "/v1/chat/completions":
			seenChat = true
			_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"Hosted answer."}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer hosted.Close()

	model, err := newModelProvider(Config{
		LLMProvider: "openai", LLMBaseURL: hosted.URL + "/v1", LLMAPIKey: "test-key",
		EmbeddingModel: "embed-model", ChatModel: "chat-model",
	})
	if err != nil {
		t.Fatal(err)
	}
	embedding, err := model.Embed(context.Background(), "sample")
	if err != nil || len(embedding) != 2 || embedding[0] != 0.2 {
		t.Fatalf("hosted embedding = %v, err = %v", embedding, err)
	}
	answer, err := model.Answer(context.Background(), "question", []string{"context"})
	if err != nil || answer != "Hosted answer." {
		t.Fatalf("hosted answer = %q, err = %v", answer, err)
	}
	if !seenEmbedding || !seenChat {
		t.Fatalf("hosted requests missing: embedding=%v chat=%v", seenEmbedding, seenChat)
	}
}

func TestTemplatesParseAndRenderLocalModelAndFiles(t *testing.T) {
	templates, err := template.ParseGlob("templates/*.html")
	if err != nil {
		t.Fatal(err)
	}
	s := &server{tmpls: templates}
	recorder := httptest.NewRecorder()
	s.renderTemplate(recorder, "documents.html", map[string]any{
		"Title": "Documents", "Documents": []Document{{
			ID: 3, Filename: "plan.txt", FileSizeText: "23 B", ChunkCount: 2, Status: "ready",
		}},
		"MaxUploadBytes": "5.0 MB", "MaxUploadBytesInt": int64(5242880),
		"Provider": "ollama", "Notice": "", "Error": "",
	})
	if recorder.Code != http.StatusOK {
		t.Fatalf("template render status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	for _, expected := range []string{"plan.txt", "ollama model", "5.0 MB", "2 passages"} {
		if !strings.Contains(recorder.Body.String(), expected) {
			t.Errorf("rendered document page does not contain %q", expected)
		}
	}
	recorder = httptest.NewRecorder()
	s.renderTemplate(recorder, "ask.html", map[string]any{
		"Title": "Ask a question", "Question": "Which plan applies?",
		"Answer": nil, "Error": "", "Notice": "",
	})
	if recorder.Code != http.StatusOK || strings.Contains(recorder.Body.String(), "<no value>") {
		t.Fatalf("ask page did not render cleanly: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestLocalModelConfigurationDefaultsToOllamaSettings(t *testing.T) {
	t.Setenv("LLM_PROVIDER", "ollama")
	t.Setenv("LLM_BASE_URL", "")
	t.Setenv("EMBEDDING_MODEL", "")
	t.Setenv("CHAT_MODEL", "")
	t.Setenv("EMBEDDING_DIMENSIONS", "")
	t.Setenv("MAX_UPLOAD_BYTES", "")

	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LLMBaseURL != "http://ollama:11434" || cfg.EmbeddingModel != "nomic-embed-text" ||
		cfg.ChatModel != "llama3.2" || cfg.EmbeddingDimensions != 768 {
		t.Fatalf("unexpected Ollama defaults: %#v", cfg)
	}
}

func TestProviderHandlesHTTPFailureAndResponseLimit(t *testing.T) {
	t.Run("http failure", func(t *testing.T) {
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "provider unavailable", http.StatusServiceUnavailable)
		}))
		defer local.Close()
		model, err := newModelProvider(Config{
			LLMProvider: "openai", LLMBaseURL: local.URL,
			EmbeddingModel: "embed", ChatModel: "chat",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = model.Embed(context.Background(), "input")
		if err == nil || !strings.Contains(err.Error(), "503") {
			t.Fatalf("Embed error = %v, want provider status", err)
		}
	})
	t.Run("oversize response", func(t *testing.T) {
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"data":[{"embedding":[`)
			_, _ = io.CopyN(w, strings.NewReader(strings.Repeat("0", maxModelResponseBytes+10)), maxModelResponseBytes+10)
			_, _ = fmt.Fprint(w, `]}]}`)
		}))
		defer local.Close()
		model, err := newModelProvider(Config{
			LLMProvider: "openai", LLMBaseURL: local.URL,
			EmbeddingModel: "embed", ChatModel: "chat",
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = model.Embed(context.Background(), "input")
		if err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("Embed error = %v, want size limit", err)
		}
	})
}
