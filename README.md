# RAG Document Review

A standalone, authenticated document-review app. It indexes Microsoft Word (`.docx`), Excel (`.xlsx`), HTML (`.html`, `.htm`), and plain-text (`.txt`) files, retrieves relevant passages, and uses a configured LLM to answer questions with source citations.

The app also supports user-defined categories inferred from filename keywords, user-managed document groups, optional pre-indexed webpages, and follow-up questions that use the current conversation as context. Conversation questions and answers are stored in the app's local PostgreSQL database; they are not used to train the model.

## Run with Docker Compose

1. Copy `.env.example` to `.env`.
2. Set unique `SESSION_SECRET`, `ADMIN_PASSWORD`, and `DB_PASSWORD` values. Set `LLM_API_KEY` if using a hosted model.
3. Start the app:

   ```sh
   docker compose up --build
   ```

4. Open `http://localhost:8080/login` and sign in with `ADMIN_USERNAME` and `ADMIN_PASSWORD`.
5. Add categories and filename keywords, create groups, upload documents, then ask questions from the Ask page. You can add public HTTPS webpages in the Library and choose them individually when asking.

The database and application are private to this Compose project and the database uses a persistent volume.

## Local model option

The default hosted provider receives the question and retrieved excerpts needed to generate an answer. To keep document text and questions on your machine, use Ollama:

```dotenv
LLM_PROVIDER=ollama
LLM_BASE_URL=http://ollama:11434
EMBEDDING_MODEL=nomic-embed-text
CHAT_MODEL=llama3.2
EMBEDDING_DIMENSIONS=768
```

Then run:

```sh
docker compose --profile local-model up --build -d
docker compose exec ollama ollama pull nomic-embed-text
docker compose exec ollama ollama pull llama3.2
```

The model files persist in the `ollamadata` volume. Choose models and dimensions that suit your machine. Database conversation memory remains local to this app, but prior turns are included in model context for follow-up questions; use Ollama if that context must not leave the machine.

## Library and search behavior

- Supported uploads are limited by `MAX_UPLOAD_BYTES` (maximum 100 MiB). Text is extracted server-side and indexed as passages.
- Add a category name and a filename keyword in Library. Filenames containing that keyword receive the category, including files already uploaded.
- Create groups, then add documents to one or more groups from their Library rows.
- Add webpages by public HTTPS URL. The app fetches and indexes the HTML once; webpage use is opt-in per question.
- Search all library files, filter by category/group, or select individual files. No webpages are included unless selected.
- Answers include excerpts, filenames/URLs, detected dates, and candidate names. Name/date extraction is heuristic: verify it in the source; it does not establish who made a decision or when.
- The last ten Q&A turns in the current browser conversation are kept in the local database and supplied for conversational continuity. Start a new conversation with the button on Ask.

## Safety and privacy notes

- Forms require authentication and a same-origin `Origin`/`Referer`; session cookies are HTTP-only and SameSite=Lax.
- Production mode rejects default/placeholder passwords and short session secrets. Terminate HTTPS at a trusted reverse proxy and set `APP_ENV=production`; configure database TLS with `DB_SSLMODE`.
- Webpage indexing accepts public HTTPS HTML only, blocks private/loopback/link-local addresses, disables redirects, and caps response size and request time.
- Uploaded Office archives and webpage responses are size-limited. The model receives retrieved excerpts, source labels, and recent conversation turns only; it has no database access.
- The similarity cutoff returns an insufficient-information response when no selected source has a relevant passage.
- Compose publishes the web app, not the database. Put remote deployments behind TLS and intended network access controls.
- `.env` is ignored by Git; do not commit credentials.

Changing embedding providers, embedding models, or dimensions requires reindexing all documents and webpages. Back up the database before changing model configuration.

## Configuration

| Variable | Purpose |
|---|---|
| `APP_ENV`, `APP_PORT` | Runtime environment and HTTP listener |
| `SESSION_SECRET`, `ADMIN_USERNAME`, `ADMIN_PASSWORD` | Admin authentication |
| `DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`, `DB_SSLMODE` | PostgreSQL connection |
| `LLM_PROVIDER` | `openai` (OpenAI-compatible API) or `ollama` |
| `LLM_BASE_URL`, `LLM_API_KEY` | Model API endpoint and optional hosted API key |
| `EMBEDDING_MODEL`, `CHAT_MODEL` | Embedding and answer models |
| `EMBEDDING_DIMENSIONS` | Expected vector length |
| `MAX_UPLOAD_BYTES` | Per-file upload limit, 1 byte to 100 MiB |

## Tests

Run the app tests with:

```sh
go test ./...
```
