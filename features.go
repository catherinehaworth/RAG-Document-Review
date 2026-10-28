package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Category struct {
	Name    string
	Keyword string
}

type DocumentGroup struct {
	ID   int
	Name string
}

type WebPage struct {
	ID  int
	URL string
}

type AskOptions struct {
	Category  string
	GroupID   int
	Files     []int
	Pages     []int
	Session   string
	SearchAll bool
}

var (
	datePattern   = regexp.MustCompile(`\b(?:\d{4}[-/]\d{1,2}[-/]\d{1,2}|\d{1,2}[-/]\d{1,2}[-/]\d{2,4}|(?:Jan(?:uary)?|Feb(?:ruary)?|Mar(?:ch)?|Apr(?:il)?|May|Jun(?:e)?|Jul(?:y)?|Aug(?:ust)?|Sep(?:tember)?|Oct(?:ober)?|Nov(?:ember)?|Dec(?:ember)?)\s+\d{1,2},?\s+\d{4})\b`)
	personPattern = regexp.MustCompile(`\b[A-Z][a-z]{2,}\s+[A-Z][a-z]{2,}\b`)
	htmlTags      = regexp.MustCompile(`(?is)<(?:script|style|noscript)\b[^>]*>.*?</(?:script|style|noscript)\s*>|<[^>]+>`)
	spaces        = regexp.MustCompile(`\s+`)
	dateFormat    = regexp.MustCompile(`(?i)[ymdhs]`)
)

func extractDocumentText(filename string, raw []byte) (string, string, error) {
	switch strings.ToLower(filepath.Ext(filename)) {
	case ".txt":
		if !utf8.Valid(raw) || bytes.ContainsRune(raw, 0) {
			return "", "", errors.New("The file must contain valid UTF-8 text.")
		}
		return strings.TrimSpace(string(raw)), "text/plain; charset=utf-8", nil
	case ".html", ".htm":
		return htmlToText(string(raw)), "text/html; charset=utf-8", nil
	case ".docx":
		text, err := officeText(raw, "word/document.xml")
		return text, "application/vnd.openxmlformats-officedocument.wordprocessingml.document", err
	case ".xlsx":
		text, err := spreadsheetText(raw)
		return text, "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", err
	default:
		return "", "", errors.New("Supported file types are .docx, .xlsx, .html, .htm, and .txt.")
	}
}

func htmlToText(value string) string {
	value = htmlTags.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	return strings.TrimSpace(spaces.ReplaceAllString(value, " "))
}

func officeText(raw []byte, target string) (string, error) {
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", errors.New("The Office document is invalid or damaged.")
	}
	if len(archive.File) > 2000 {
		return "", errors.New("The Office document contains too many archive entries.")
	}
	for _, file := range archive.File {
		if file.Name != target {
			continue
		}
		if file.UncompressedSize64 > 20<<20 {
			return "", errors.New("The Office document contains too much expanded data.")
		}
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		defer stream.Close()
		decoder := xml.NewDecoder(io.LimitReader(stream, 20<<20))
		var values []string
		for {
			token, err := decoder.Token()
			if err == io.EOF {
				break
			}
			if err != nil {
				return "", errors.New("The Office document contains invalid XML.")
			}
			if text, ok := token.(xml.CharData); ok && strings.TrimSpace(string(text)) != "" {
				values = append(values, strings.TrimSpace(string(text)))
			}
		}
		return strings.Join(values, " "), nil
	}
	return "", errors.New("The Office document does not contain readable text.")
}

func spreadsheetText(raw []byte) (string, error) {
	archive, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return "", errors.New("The spreadsheet is invalid or damaged.")
	}
	files := make(map[string]*zip.File)
	var expandedSize uint64
	if len(archive.File) > 2000 {
		return "", errors.New("The spreadsheet contains too many archive entries.")
	}
	for _, file := range archive.File {
		files[file.Name] = file
		if file.Name == "xl/sharedStrings.xml" || strings.HasPrefix(file.Name, "xl/worksheets/") && strings.HasSuffix(file.Name, ".xml") {
			expandedSize += file.UncompressedSize64
			if file.UncompressedSize64 > 20<<20 || expandedSize > 20<<20 {
				return "", errors.New("The spreadsheet contains too much expanded data.")
			}
		}
	}
	dateStyles, err := spreadsheetDateStyles(files["xl/styles.xml"])
	if err != nil {
		return "", err
	}
	var shared []string
	if file := files["xl/sharedStrings.xml"]; file != nil {
		shared, err = xmlStringItems(file)
		if err != nil {
			return "", err
		}
	}
	var worksheetNames []string
	for name := range files {
		if !strings.HasPrefix(name, "xl/worksheets/") || !strings.HasSuffix(name, ".xml") {
			continue
		}
		worksheetNames = append(worksheetNames, name)
	}
	sort.Strings(worksheetNames)
	var rows []string
	for _, name := range worksheetNames {
		file := files[name]
		stream, err := file.Open()
		if err != nil {
			return "", err
		}
		decoder := xml.NewDecoder(io.LimitReader(stream, 20<<20))
		var values []string
		for {
			token, tokenErr := decoder.Token()
			if tokenErr == io.EOF {
				break
			}
			if tokenErr != nil {
				stream.Close()
				return "", errors.New("The spreadsheet contains invalid XML.")
			}
			start, ok := token.(xml.StartElement)
			if !ok || start.Name.Local != "c" {
				continue
			}
			kind := ""
			style := -1
			for _, attr := range start.Attr {
				if attr.Name.Local == "t" {
					kind = attr.Value
				}
				if attr.Name.Local == "s" {
					style, _ = strconv.Atoi(attr.Value)
				}
			}
			var rawCell strings.Builder
			for {
				cellToken, cellErr := decoder.Token()
				if cellErr != nil {
					if cellErr != io.EOF {
						stream.Close()
						return "", errors.New("The spreadsheet contains invalid XML.")
					}
					break
				}
				if charData, ok := cellToken.(xml.CharData); ok {
					rawCell.Write(charData)
				}
				if end, ok := cellToken.(xml.EndElement); ok && end.Name.Local == "c" {
					break
				}
			}
			cell := strings.TrimSpace(rawCell.String())
			if kind == "s" {
				index, parseErr := strconv.Atoi(cell)
				if parseErr == nil && index >= 0 && index < len(shared) {
					cell = shared[index]
				}
			} else if style >= 0 && style < len(dateStyles) && dateStyles[style] {
				if serial, parseErr := strconv.ParseFloat(cell, 64); parseErr == nil && serial >= 1 && serial < 3000000 {
					cell = time.Date(1899, 12, 30, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(serial)).Format("2006-01-02")
				}
			}
			if cell != "" {
				values = append(values, cell)
			}
		}
		stream.Close()
		if len(values) > 0 {
			rows = append(rows, strings.Join(values, " | "))
		}
	}
	if len(rows) == 0 {
		return "", errors.New("The spreadsheet does not contain readable cell values.")
	}
	return strings.Join(rows, "\n"), nil
}

func spreadsheetDateStyles(file *zip.File) ([]bool, error) {
	if file == nil {
		return nil, nil
	}
	stream, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	decoder := xml.NewDecoder(io.LimitReader(stream, 2<<20))
	customDateFormats := make(map[int]bool)
	var styleFormatIDs []int
	inCellFormats := false
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("The spreadsheet styles are invalid.")
		}
		switch value := token.(type) {
		case xml.StartElement:
			switch value.Name.Local {
			case "numFmts":
			case "numFmt":
				id, format := -1, ""
				for _, attr := range value.Attr {
					if attr.Name.Local == "numFmtId" {
						id, _ = strconv.Atoi(attr.Value)
					}
					if attr.Name.Local == "formatCode" {
						format = attr.Value
					}
				}
				if id >= 0 {
					customDateFormats[id] = dateFormat.MatchString(format)
				}
			case "cellXfs":
				inCellFormats = true
			case "xf":
				if inCellFormats {
					formatID := 0
					for _, attr := range value.Attr {
						if attr.Name.Local == "numFmtId" {
							formatID, _ = strconv.Atoi(attr.Value)
						}
					}
					styleFormatIDs = append(styleFormatIDs, formatID)
				}
			}
		case xml.EndElement:
			if value.Name.Local == "cellXfs" {
				inCellFormats = false
			}
		}
	}
	result := make([]bool, len(styleFormatIDs))
	for i, formatID := range styleFormatIDs {
		result[i] = formatID >= 14 && formatID <= 22 ||
			formatID >= 27 && formatID <= 36 ||
			formatID >= 45 && formatID <= 47 ||
			formatID >= 50 && formatID <= 58 ||
			customDateFormats[formatID]
	}
	return result, nil
}

func xmlStringItems(file *zip.File) ([]string, error) {
	stream, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer stream.Close()
	decoder := xml.NewDecoder(io.LimitReader(stream, 20<<20))
	var result []string
	var current strings.Builder
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, errors.New("The spreadsheet contains invalid XML.")
		}
		switch value := token.(type) {
		case xml.StartElement:
			if value.Name.Local == "si" {
				current.Reset()
			}
			if value.Name.Local == "t" {
				var text string
				if err := decoder.DecodeElement(&text, &value); err == nil {
					current.WriteString(text)
				}
			}
		case xml.EndElement:
			if value.Name.Local == "si" {
				result = append(result, current.String())
			}
		}
	}
	return result, nil
}

func extractedMetadata(text string) ([]string, []string) {
	dates := uniqueStrings(datePattern.FindAllString(text, -1), 20)
	people := uniqueStrings(personPattern.FindAllString(text, -1), 60)
	filtered := people[:0]
	for _, name := range people {
		first := strings.ToLower(strings.Fields(name)[0])
		switch first {
		case "a", "an", "the", "this", "that", "these", "those", "chapter", "section", "appendix", "document", "policy", "meeting", "decision":
			continue
		default:
			filtered = append(filtered, name)
		}
	}
	if len(filtered) > 30 {
		filtered = filtered[:30]
	}
	return dates, filtered
}

func uniqueStrings(values []string, limit int) []string {
	seen := make(map[string]bool)
	result := make([]string, 0)
	for _, value := range values {
		key := strings.ToLower(value)
		if value != "" && !seen[key] {
			seen[key] = true
			result = append(result, value)
			if len(result) == limit {
				break
			}
		}
	}
	return result
}

func (s *server) categories() ([]Category, error) {
	rows, err := s.db.Query(`SELECT name, filename_keyword FROM categories ORDER BY length(filename_keyword) DESC, name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []Category
	for rows.Next() {
		var item Category
		if err := rows.Scan(&item.Name, &item.Keyword); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *server) groups() ([]DocumentGroup, error) {
	rows, err := s.db.Query(`SELECT id, name FROM document_groups ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DocumentGroup
	for rows.Next() {
		var item DocumentGroup
		if err := rows.Scan(&item.ID, &item.Name); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *server) webPages() ([]WebPage, error) {
	rows, err := s.db.Query(`SELECT id, url FROM web_pages ORDER BY url`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []WebPage
	for rows.Next() {
		var item WebPage
		if err := rows.Scan(&item.ID, &item.URL); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func classifyFilename(filename string, categories []Category) string {
	for _, category := range categories {
		if strings.Contains(strings.ToLower(filename), strings.ToLower(category.Keyword)) {
			return category.Name
		}
	}
	return ""
}

func (s *server) createCategorizedDocument(ctx context.Context, filename, content, mimeType string, size int64) error {
	processingCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	dates, people := extractedMetadata(content)
	dateJSON, _ := json.Marshal(dates)
	peopleJSON, _ := json.Marshal(people)
	categories, err := s.categories()
	if err != nil {
		return err
	}
	category := classifyFilename(filename, categories)
	chunks, err := s.prepareChunks(processingCtx, content)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(processingCtx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRowContext(processingCtx, `INSERT INTO documents(filename, content, file_size, mime_type, status, category, metadata_dates, metadata_people) VALUES($1,$2,$3,$4,'ready',$5,$6,$7) RETURNING id`, filename, content, size, mimeType, category, string(dateJSON), string(peopleJSON)).Scan(&id)
	if err != nil {
		return err
	}
	if err := insertChunks(processingCtx, tx, id, chunks); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) replaceCategorizedDocument(ctx context.Context, id int, filename, content, mimeType string, size int64) error {
	dates, people := extractedMetadata(content)
	dateJSON, _ := json.Marshal(dates)
	peopleJSON, _ := json.Marshal(people)
	categories, err := s.categories()
	if err != nil {
		return err
	}
	chunks, err := s.prepareChunks(ctx, content)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE documents SET filename=$1,content=$2,file_size=$3,mime_type=$4,status='ready',category=$5,metadata_dates=$6,metadata_people=$7,updated_at=NOW() WHERE id=$8`, filename, content, size, mimeType, classifyFilename(filename, categories), string(dateJSON), string(peopleJSON), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return sql.ErrNoRows
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunks WHERE document_id=$1`, id); err != nil {
		return err
	}
	if err := insertChunks(ctx, tx, id, chunks); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *server) createCategory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || parseFormBody(w, r, 16<<10) != nil {
		http.Error(w, "A category name and filename keyword are required.", http.StatusBadRequest)
		return
	}
	name, keyword := strings.TrimSpace(r.FormValue("name")), strings.TrimSpace(r.FormValue("keyword"))
	if name == "" || keyword == "" || len(name) > 80 || len(keyword) > 80 {
		http.Error(w, "Category names and keywords must be 1–80 characters.", http.StatusBadRequest)
		return
	}
	tx, err := s.db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, "The category could not be saved.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(r.Context(), `INSERT INTO categories(name,filename_keyword) VALUES($1,$2) ON CONFLICT(name) DO UPDATE SET filename_keyword=EXCLUDED.filename_keyword`, name, keyword); err != nil {
		http.Error(w, "The category could not be saved.", http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE documents SET category='',updated_at=NOW() WHERE category=$1 AND POSITION(LOWER($2) IN LOWER(filename))=0`, name, keyword); err != nil {
		http.Error(w, "The category could not be saved.", http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(r.Context(), `UPDATE documents SET category=$1,updated_at=NOW() WHERE POSITION(LOWER($2) IN LOWER(filename))>0`, name, keyword); err != nil {
		http.Error(w, "The category could not be applied to existing files.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "The category could not be saved.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=category", http.StatusSeeOther)
}

func (s *server) createGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || parseFormBody(w, r, 16<<10) != nil {
		http.Error(w, "A group name is required.", http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if name == "" || len(name) > 80 {
		http.Error(w, "Group names must be 1–80 characters.", http.StatusBadRequest)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO document_groups(name) VALUES($1) ON CONFLICT(name) DO NOTHING`, name); err != nil {
		http.Error(w, "The group could not be saved.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=group", http.StatusSeeOther)
}

func (s *server) assignGroup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || parseFormBody(w, r, 16<<10) != nil {
		http.Error(w, "The group assignment could not be read.", http.StatusBadRequest)
		return
	}
	docID, docErr := strconv.Atoi(r.FormValue("document_id"))
	groupID, groupErr := strconv.Atoi(r.FormValue("group_id"))
	if docErr != nil || groupErr != nil || docID < 1 || groupID < 1 {
		http.Error(w, "Choose a valid document and group.", http.StatusBadRequest)
		return
	}
	if r.FormValue("action") == "remove" {
		if _, err := s.db.ExecContext(r.Context(), `DELETE FROM document_group_members WHERE document_id=$1 AND group_id=$2`, docID, groupID); err != nil {
			http.Error(w, "The document could not be removed from the group.", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, "/documents?status=unassigned", http.StatusSeeOther)
		return
	}
	if _, err := s.db.ExecContext(r.Context(), `INSERT INTO document_group_members(document_id,group_id) VALUES($1,$2) ON CONFLICT DO NOTHING`, docID, groupID); err != nil {
		http.Error(w, "The document could not be added to the group.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=assigned", http.StatusSeeOther)
}

func (s *server) createWebPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || parseFormBody(w, r, 16<<10) != nil {
		http.Error(w, "A public HTTPS webpage URL is required.", http.StatusBadRequest)
		return
	}
	rawURL := strings.TrimSpace(r.FormValue("url"))
	fetchCtx, cancelFetch := context.WithTimeout(r.Context(), 15*time.Second)
	content, err := fetchWebPage(fetchCtx, rawURL)
	cancelFetch()
	if err != nil {
		http.Error(w, "Unable to safely fetch that webpage. Use a public HTTPS page and try again.", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	chunks, err := s.prepareChunks(ctx, content)
	if err != nil {
		http.Error(w, "The webpage could not be indexed with the configured model.", http.StatusBadGateway)
		return
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, "The webpage could not be saved.", http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()
	var id int
	err = tx.QueryRowContext(ctx, `INSERT INTO web_pages(url,content) VALUES($1,$2) ON CONFLICT(url) DO UPDATE SET content=EXCLUDED.content,updated_at=NOW() RETURNING id`, rawURL, content).Scan(&id)
	if err != nil {
		http.Error(w, "The webpage could not be saved.", http.StatusInternalServerError)
		return
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM web_chunks WHERE page_id=$1`, id); err != nil {
		http.Error(w, "The webpage could not be indexed.", http.StatusInternalServerError)
		return
	}
	if err := insertWebChunks(ctx, tx, id, chunks); err != nil {
		http.Error(w, "The webpage could not be indexed.", http.StatusInternalServerError)
		return
	}
	if err := tx.Commit(); err != nil {
		http.Error(w, "The webpage could not be saved.", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/documents?status=webpage", http.StatusSeeOther)
}

func fetchWebPage(ctx context.Context, rawURL string) (string, error) {
	parsed, err := url.ParseRequestURI(rawURL)
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
		return "", errors.New("only public HTTPS webpages are allowed")
	}
	addresses, err := net.DefaultResolver.LookupIPAddr(ctx, parsed.Hostname())
	if err != nil || len(addresses) == 0 {
		return "", errors.New("webpage host could not be resolved")
	}
	for _, address := range addresses {
		if address.IP.IsPrivate() || address.IP.IsLoopback() || address.IP.IsLinkLocalUnicast() || address.IP.IsUnspecified() {
			return "", errors.New("private webpage host is not allowed")
		}
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: 5 * time.Second}
		return dialer.DialContext(ctx, network, net.JoinHostPort(addresses[0].IP.String(), "443"))
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 12 * time.Second, CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "RAG-Document-Review/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 || !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/html") {
		return "", errors.New("webpage did not return HTML successfully")
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	if err != nil || len(raw) > 2<<20 {
		return "", errors.New("webpage exceeds the 2 MiB limit")
	}
	text := htmlToText(string(raw))
	if text == "" {
		return "", errors.New("webpage has no readable text")
	}
	return text, nil
}

func insertWebChunks(ctx context.Context, tx *sql.Tx, id int, chunks []preparedChunk) error {
	for i, chunk := range chunks {
		if _, err := tx.ExecContext(ctx, `INSERT INTO web_chunks(page_id,chunk_index,text,embedding) VALUES($1,$2,$3,$4::vector)`, id, i, chunk.text, vectorString(chunk.embedding)); err != nil {
			return fmt.Errorf("save webpage chunk %d: %w", i, err)
		}
	}
	return nil
}

func (s *server) answerQuestionWithOptions(parent context.Context, question string, options AskOptions) (*Answer, error) {
	question = strings.TrimSpace(question)
	if question == "" {
		return nil, errors.New("question is required")
	}
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	embedding, err := s.model.Embed(ctx, question)
	if err != nil {
		return nil, err
	}
	if err := s.validateEmbedding(embedding); err != nil {
		return nil, err
	}
	vector := vectorString(embedding)
	query := `SELECT c.text,d.filename,d.id,d.metadata_dates,d.metadata_people,1-(c.embedding <=> $1::vector) AS score FROM chunks c JOIN documents d ON d.id=c.document_id WHERE c.embedding IS NOT NULL AND d.status='ready'`
	args := []any{vector}
	if options.Category != "" {
		args = append(args, options.Category)
		query += fmt.Sprintf(" AND d.category=$%d", len(args))
	}
	if options.GroupID > 0 {
		args = append(args, options.GroupID)
		query += fmt.Sprintf(" AND EXISTS (SELECT 1 FROM document_group_members gm WHERE gm.document_id=d.id AND gm.group_id=$%d)", len(args))
	}
	if !options.SearchAll && len(options.Files) == 0 {
		query += " AND FALSE"
	}
	if !options.SearchAll && len(options.Files) > 0 {
		marks := make([]string, len(options.Files))
		for i, id := range options.Files {
			args = append(args, id)
			marks[i] = fmt.Sprintf("$%d", len(args))
		}
		query += " AND d.id IN (" + strings.Join(marks, ",") + ")"
	}
	query += " ORDER BY c.embedding <=> $1::vector LIMIT 5"
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	var sources []DocumentSource
	var excerpts []string
	for rows.Next() {
		var source DocumentSource
		var dates, people string
		if err := rows.Scan(&source.Text, &source.Filename, &source.DocumentID, &dates, &people, &source.Score); err != nil {
			rows.Close()
			return nil, err
		}
		_ = json.Unmarshal([]byte(dates), &source.Dates)
		_ = json.Unmarshal([]byte(people), &source.People)
		sources = append(sources, source)
		excerpts = append(excerpts, formatSourceContext(source))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	if len(options.Pages) > 0 {
		pageSources, err := s.retrieveSelectedPages(ctx, vector, options.Pages)
		if err != nil {
			return nil, err
		}
		for _, source := range pageSources {
			sources = append(sources, source)
			excerpts = append(excerpts, formatSourceContext(source))
		}
	}
	if options.Session != "" {
		history, err := s.conversationHistory(ctx, options.Session)
		if err != nil {
			return nil, err
		}
		if history != "" {
			excerpts = append([]string{"Previous conversation for continuity only; it is not evidence:\n" + history}, excerpts...)
		}
	}
	answerText := "I couldn’t find enough relevant information in the selected sources to answer that confidently."
	relevant := false
	for _, source := range sources {
		if source.Score >= 0.12 {
			relevant = true
			break
		}
	}
	if relevant {
		answerText, err = s.model.Answer(ctx, question, excerpts)
		if err != nil {
			return nil, err
		}
	}
	answer := &Answer{Question: question, Answer: answerText, Sources: sources, SessionID: options.Session}
	if options.Session != "" {
		if err := s.db.QueryRowContext(ctx, `INSERT INTO conversation_messages(session_id,question,answer) VALUES($1,$2,$3) RETURNING id`, options.Session, question, answerText).Scan(new(int64)); err != nil {
			return nil, err
		}
	}
	return answer, nil
}

func formatSourceContext(source DocumentSource) string {
	label := source.Filename
	if source.Kind == "webpage" {
		return "[Webpage: " + label + "]\n" + source.Text
	}
	metadata := ""
	if len(source.Dates) > 0 {
		metadata += " | dates: " + strings.Join(source.Dates, ", ")
	}
	if len(source.People) > 0 {
		metadata += " | names detected: " + strings.Join(source.People, ", ")
	}
	return "[Document: " + label + metadata + "]\n" + source.Text
}

func (s *server) retrieveSelectedPages(ctx context.Context, vector string, ids []int) ([]DocumentSource, error) {
	marks := make([]string, len(ids))
	args := []any{vector}
	for i, id := range ids {
		args = append(args, id)
		marks[i] = fmt.Sprintf("$%d", len(args))
	}
	query := `SELECT c.text,p.url,1-(c.embedding <=> $1::vector) AS score FROM web_chunks c JOIN web_pages p ON p.id=c.page_id WHERE c.embedding IS NOT NULL AND p.id IN (` + strings.Join(marks, ",") + `) ORDER BY c.embedding <=> $1::vector LIMIT 5`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []DocumentSource
	for rows.Next() {
		var source DocumentSource
		source.Kind = "webpage"
		if err := rows.Scan(&source.Text, &source.Filename, &source.Score); err != nil {
			return nil, err
		}
		result = append(result, source)
	}
	return result, rows.Err()
}

func (s *server) conversationHistory(ctx context.Context, session string) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT question,answer FROM conversation_messages WHERE session_id=$1 ORDER BY id DESC LIMIT 10`, session)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var turns []string
	for rows.Next() {
		var question, answer string
		if err := rows.Scan(&question, &answer); err != nil {
			return "", err
		}
		turns = append(turns, "Question: "+question+"\nAnswer: "+answer)
	}
	if err := rows.Err(); err != nil {
		return "", err
	}
	for i, j := 0, len(turns)-1; i < j; i, j = i+1, j-1 {
		turns[i], turns[j] = turns[j], turns[i]
	}
	history := strings.Join(turns, "\n\n")
	if len(history) > 12000 {
		end := 12000
		for end > 0 && !utf8.RuneStart(history[end]) {
			end--
		}
		history = history[:end]
	}
	return history, nil
}

func newSessionID() (string, error) {
	value := make([]byte, 24)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", value), nil
}
