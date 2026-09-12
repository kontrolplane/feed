package handler

// OPML transport. Parsing and persistence now live in internal/store
// (see internal/store/import.go for why); everything here is HTTP concerns:
// enforce the upload cap, hand the store a reader, render the outcome.

import (
	"errors"
	"fmt"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strconv"

	"github.com/kontrolplane/feed/internal/store"
)

// maxOPMLUpload caps an uploaded OPML file. 10 MB is roughly a hundred
// thousand subscriptions; nobody has that many.
const maxOPMLUpload = 10 << 20

func (h *Handler) handleExportOPML(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	folders, err := h.store.Folders(ctx)
	if err != nil {
		h.logger.Error("export opml: read folders", slog.Any("error", err))
		http.Error(w, "could not export feeds", http.StatusInternalServerError)
		return
	}
	feeds, err := h.store.Feeds(ctx)
	if err != nil {
		h.logger.Error("export opml: read feeds", slog.Any("error", err))
		http.Error(w, "could not export feeds", http.StatusInternalServerError)
		return
	}

	doc, err := store.ExportOPML(folders, feeds)
	if err != nil {
		h.logger.Error("export opml: encode", slog.Any("error", err))
		http.Error(w, "could not export feeds", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="feeds.opml"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(doc)))
	if _, err := w.Write(doc); err != nil {
		// The response is already committed; a client that hung up mid-download
		// is not an error we can act on beyond noting it.
		h.logger.Warn("export opml: write response", slog.Any("error", err))
	}
}

func (h *Handler) handleImportOPML(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The cap has to be installed before anything touches the form. FormFile
	// calls ParseMultipartForm, which buffers 32 MB in memory and spools the
	// rest to disk with no limit at all — so a LimitReader applied to the file
	// afterwards protects nothing; by then the upload has already landed.
	r.Body = http.MaxBytesReader(w, r.Body, maxOPMLUpload)

	file, header, err := r.FormFile("file")
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			h.logger.Warn("import opml: upload too large", slog.Int64("limit", maxOPMLUpload))
			http.Error(w, fmt.Sprintf("file too large, the limit is %d MB", maxOPMLUpload>>20), http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.Error("import opml: no file", slog.Any("error", err))
		http.Error(w, "no file uploaded", http.StatusBadRequest)
		return
	}
	defer func() { _ = file.Close() }()
	// ParseMultipartForm spools parts over its memory budget into temp files
	// that nothing else ever deletes. Under the cap above it should not get
	// that far, but the cleanup costs nothing and the leak is silent.
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()

	res, err := store.ImportOPML(ctx, h.store, file)
	if err != nil {
		var tooLarge *http.MaxBytesError
		switch {
		case errors.As(err, &tooLarge):
			h.logger.Warn("import opml: upload too large", slog.Int64("limit", maxOPMLUpload))
			http.Error(w, fmt.Sprintf("file too large, the limit is %d MB", maxOPMLUpload>>20), http.StatusRequestEntityTooLarge)
		case errors.Is(err, store.ErrInvalidOPML):
			h.logger.Error("import opml: parse error",
				slog.String("filename", uploadName(header)), slog.Any("error", err))
			http.Error(w, "invalid OPML file", http.StatusBadRequest)
		default:
			h.logger.Error("import opml: store error", slog.Any("error", err))
			http.Error(w, "could not import feeds", http.StatusInternalServerError)
		}
		return
	}

	// Every skipped entry gets logged. The old code returned 1 per outline
	// unconditionally and discarded the insert error, so a completely failed
	// import reported success.
	for _, ie := range res.Errors {
		h.logger.Error("import opml: entry skipped",
			slog.String("url", ie.URL), slog.Any("error", ie.Err))
	}

	h.logger.Info("import opml",
		slog.String("filename", uploadName(header)),
		slog.Int("added", res.FeedsAdded),
		slog.Int("updated", res.FeedsUpdated),
		slog.Int("folders", res.FoldersCreated),
		slog.Int("skipped", res.Skipped))

	if h.isHTMX(r) {
		w.Header().Set("HX-Redirect", "/settings")
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		if _, err := fmt.Fprint(w, res.Summary()); err != nil {
			h.logger.Warn("import opml: write response", slog.Any("error", err))
		}
		return
	}
	http.Redirect(w, r, "/settings", http.StatusSeeOther)
}

// uploadName is only ever used for log context. The name comes from the
// client, so it is bounded before it reaches a log line.
func uploadName(h *multipart.FileHeader) string {
	if h == nil {
		return ""
	}
	name := []rune(h.Filename)
	if len(name) > 128 {
		name = name[:128]
	}
	return string(name)
}
