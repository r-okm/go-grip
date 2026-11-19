package internal

import (
	"bytes"
	"context"
	"errors"
	"html"
	htmltemplate "html/template"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"
	"unicode"
	"unicode/utf8"

	chroma_html "github.com/alecthomas/chroma/v2/formatters/html"
	"github.com/alecthomas/chroma/v2/styles"
	"github.com/r-okm/go-grip/defaults"
)

const defaultHTMLTitle = "go-grip - markdown preview"

type Server struct {
	parser       *Parser
	boundingBox  bool
	host         string
	port         int
	browser      bool
	enableReload bool
	closeTabs    bool
	replaceNav   bool
	findPort     bool
	hub          *Hub
	log          *slog.Logger

	// closing is closed when the server starts shutting down, telling every
	// open event stream to send a "close" event. streams tracks those
	// handlers so shutdown can wait for the event to be flushed.
	closing  chan struct{}
	streamMu sync.Mutex // guards shut and orders streams.Add before Wait
	shut     bool
	streams  sync.WaitGroup
}

// addStream registers an event stream, or reports false once shutdown has
// begun. Registering under streamMu keeps every Add before shutdown's Wait.
func (s *Server) addStream() bool {
	s.streamMu.Lock()
	defer s.streamMu.Unlock()
	if s.shut {
		return false
	}
	s.streams.Add(1)
	return true
}

// reloadDebounce collapses the several events an editor writes per save into
// one browser reload.
const reloadDebounce = 100 * time.Millisecond

// eventsPath is the live reload stream endpoint.
const eventsPath = "/__grip/events"

// shutdownGrace bounds how long shutdown waits for the "close" events to be
// flushed and for in-flight requests to finish.
const shutdownGrace = time.Second

// reloading reports whether live reload is on: requested and backed by a
// running hub.
func (s *Server) reloading() bool {
	return s.enableReload && s.hub != nil
}

// streaming reports whether pages open the event stream: for live reload,
// for closing the tab on shutdown, or both.
func (s *Server) streaming() bool {
	return s.reloading() || s.closeTabs
}

// SetFindPort makes the server try the following ports when the requested
// one is busy, instead of failing.
func (s *Server) SetFindPort(on bool) {
	s.findPort = on
}

// SetReplaceNav makes pages navigate inside the preview without adding
// browser history entries, so a tab can still be closed on shutdown after
// following links. Back and forward then no longer work inside the preview.
func (s *Server) SetReplaceNav(on bool) {
	s.replaceNav = on
}

// SetCloseTabs makes pages close their browser tab when the server shuts
// down. Browsers only let a script close a tab that was opened by another
// program and has not navigated, so other tabs stay open.
func (s *Server) SetCloseTabs(on bool) {
	s.closeTabs = on
}

// NewServer returns a server. A nil logger discards all output.
func NewServer(host string, port int, boundingBox bool, browser bool, enableReload bool, parser *Parser, logger *slog.Logger) *Server {
	return &Server{
		host:         host,
		port:         port,
		boundingBox:  boundingBox,
		browser:      browser,
		enableReload: enableReload,
		parser:       parser,
		log:          orDiscard(logger),
		closing:      make(chan struct{}),
	}
}

// Serve serves file until the process receives SIGINT or SIGTERM. A second
// signal kills the process without waiting for a graceful shutdown.
func (s *Server) Serve(file string) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		stop()
	}()
	return s.serve(ctx, file, nil)
}

// serve serves file until ctx is done, then shuts down gracefully. If ready
// is not nil, it receives the page URL once the listener is bound.
func (s *Server) serve(ctx context.Context, file string, ready chan<- string) error {
	directory, startPath, err := ResolveTarget(file)
	if err != nil {
		return err
	}

	if s.enableReload {
		src, err := newFsnotifySource()
		if err != nil {
			s.log.Warn("❌ Error starting file watcher, auto-reload disabled", "err", err)
			s.enableReload = false
		} else {
			hubCtx, stopHub := context.WithCancel(context.Background())
			defer func() {
				stopHub()
				_ = src.Close()
			}()
			s.hub = NewHub(src, reloadDebounce, s.log)
			go s.hub.Run(hubCtx)
		}
	}

	handler := s.newHandler(http.Dir(directory))

	// Bind before printing or opening the browser
	// in order for a bad host or a busy port to fail fast
	ln, err := s.listen()
	if err != nil {
		return err
	}
	port := strconv.Itoa(ln.Addr().(*net.TCPAddr).Port)

	addr := "http://" + net.JoinHostPort(browseHost(s.host), port) + startPath

	s.log.Info("🚀 Starting server: " + addr)
	if isUnspecifiedHost(s.host) {
		s.log.Info("🌐 Listening on all network interfaces")
	}
	s.log.Debug("serving directory", "dir", directory)

	if s.browser {
		err := Open(addr)
		if err != nil {
			s.log.Warn("❌ Error opening browser", "err", err)
		}
	}

	if s.reloading() {
		s.log.Info("📡 Auto-reload enabled. Files will trigger browser refresh.")
	} else {
		s.log.Info("🔄 Auto-reload disabled. Use F5 to manually refresh.")
	}

	srv := &http.Server{Handler: handler}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	if ready != nil {
		ready <- addr
	}

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	s.log.Info("👋 Shutting down")
	return s.shutdown(srv)
}

// shutdown tells open pages to close, waits briefly for the event streams to
// flush, then stops the server. Event streams never end on their own, so
// they must be told to stop before http.Server.Shutdown can return.
func (s *Server) shutdown(srv *http.Server) error {
	deadline := time.Now().Add(shutdownGrace)
	s.streamMu.Lock()
	if !s.shut {
		s.shut = true
		close(s.closing)
	}
	s.streamMu.Unlock()

	flushed := make(chan struct{})
	go func() {
		s.streams.Wait()
		close(flushed)
	}()
	select {
	case <-flushed:
	case <-time.After(time.Until(deadline)):
		s.log.Warn("❌ event streams did not finish in time")
	}

	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		_ = srv.Close()
		if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return nil
}

// portTries is how many ports, starting at the requested one, listen tries
// when --find-port is on.
const portTries = 100

// listen binds the requested port. With findPort, a busy port moves on to
// the next one, up to portTries ports; any other error stops at once.
func (s *Server) listen() (net.Listener, error) {
	tries := 1
	if s.findPort && s.port != 0 {
		tries = portTries
	}
	var err error
	for port := s.port; port < s.port+tries && port <= 65535; port++ {
		var ln net.Listener
		ln, err = net.Listen("tcp", net.JoinHostPort(s.host, strconv.Itoa(port)))
		if err == nil {
			if port != s.port {
				s.log.Debug("port busy, using the next free one", "requested", s.port, "port", port)
			}
			return ln, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return nil, err
		}
	}
	return nil, err
}

// isUnspecifiedHost reports whether host is a wildcard listen address
// ("", 0.0.0.0 or ::), which binds every network interface.
func isUnspecifiedHost(host string) bool {
	if host == "" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}

// browseHost returns the host to use in the browser URL. A wildcard listen
// address is not a usable destination, so it is replaced by localhost.
func browseHost(host string) string {
	if isUnspecifiedHost(host) {
		return "localhost"
	}
	return host
}

// debugging reports whether debug logging is on.
func (s *Server) debugging(ctx context.Context) bool {
	return s.log.Enabled(ctx, slog.LevelDebug)
}

// statusRecorder captures the status code and body size of a response. It
// forwards Flush, which serveEvents needs to stream.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (rec *statusRecorder) WriteHeader(code int) {
	if rec.status == 0 {
		rec.status = code
	}
	rec.ResponseWriter.WriteHeader(code)
}

func (rec *statusRecorder) Write(b []byte) (int, error) {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	n, err := rec.ResponseWriter.Write(b)
	rec.bytes += int64(n)
	return n, err
}

func (rec *statusRecorder) Flush() {
	if rec.status == 0 {
		rec.status = http.StatusOK
	}
	if f, ok := rec.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (rec *statusRecorder) Unwrap() http.ResponseWriter {
	return rec.ResponseWriter
}

// logRequests logs each request's method, path, status and duration at
// debug level, after it completes. With debug off it adds one branch.
func (s *Server) logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.debugging(r.Context()) {
			next.ServeHTTP(w, r)
			return
		}
		rec := &statusRecorder{ResponseWriter: w}
		start := time.Now()
		next.ServeHTTP(rec, r)
		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		s.log.Debug("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", status,
			"bytes", rec.bytes,
			"dur", time.Since(start))
	})
}

func (s *Server) newHandler(fsys http.FileSystem) http.Handler {
	root := NewRoot(fsys)
	fileServer := http.FileServer(fsys)
	mux := http.NewServeMux()
	mux.Handle("/static/", http.FileServer(http.FS(defaults.StaticFiles)))

	if s.streaming() {
		absRoot := ""
		if dir, ok := fsys.(http.Dir); ok {
			if abs, err := filepath.Abs(string(dir)); err == nil {
				absRoot = abs
			}
		}
		mux.HandleFunc(eventsPath, func(w http.ResponseWriter, r *http.Request) {
			s.serveEvents(w, r, root, absRoot)
		})
	}

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		switch root.Classify(r.URL.Path) {
		case KindMarkdown:
			s.serveMarkdown(w, r, root)
			return
		case KindDir:
			s.serveFolder(w, r, root)
			return
		}

		fileServer.ServeHTTP(w, r)
	})

	if !s.reloading() {
		return s.logRequests(mux)
	}
	// While reload is on, make the browser revalidate everything, so a
	// reload never shows a heuristically cached image or stylesheet.
	return s.logRequests(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		mux.ServeHTTP(w, r)
	}))
}

// serveEvents streams Server-Sent Events to one page: a "reload" event each
// time something that page shows has changed, if live reload is on, and a
// "close" event when the server shuts down, if closing tabs is on. The page
// names itself in the path query parameter.
func (s *Server) serveEvents(w http.ResponseWriter, r *http.Request, root *Root, absRoot string) {
	if !s.addStream() {
		// Shutdown has begun. A page that connects now, for instance one
		// that was reloading, would otherwise miss the "close" event.
		if s.closeTabs {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, "event: close\ndata: 1\n\n")
			return
		}
		// Drop the connection rather than answer with an error status: an
		// EventSource gives up for good on an error status, but keeps
		// retrying after a network error, so the page still reloads when
		// go-grip comes back.
		panic(http.ErrAbortHandler)
	}
	defer s.streams.Done()

	page := r.URL.Query().Get("path")
	start := time.Now()
	ws, ok := s.watchSetFor(root, absRoot, page)
	if !ok {
		s.log.Warn("live reload request rejected", "page", page)
		http.Error(w, http.StatusText(http.StatusBadRequest), http.StatusBadRequest)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		s.serverError(w, r, errors.New("response writer does not support streaming"))
		return
	}
	ws.Page = page
	debug := s.debugging(r.Context())
	if debug {
		s.log.Debug("watch set",
			"page", page,
			"files", len(ws.Files),
			"dirs", len(ws.Dirs),
			"took", time.Since(start))
	}

	// A nil channel never fires, so without live reload only "close" is sent.
	var notify <-chan struct{}
	if s.reloading() {
		var cancel func()
		notify, cancel = s.hub.Subscribe(ws)
		defer cancel()
	}

	connected := time.Now()
	reloads := 0
	if debug {
		s.log.Debug("sse connect", "page", page)
		defer func() {
			s.log.Debug("sse disconnect", "page", page, "reloads", reloads, "dur", time.Since(connected))
		}()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	if _, err := io.WriteString(w, ": connected\n\n"); err != nil {
		return
	}
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.closing:
			// End the stream even when tabs stay open, so shutdown does not
			// wait for it.
			if !s.closeTabs {
				return
			}
			if _, err := io.WriteString(w, "event: close\ndata: 1\n\n"); err == nil {
				flusher.Flush()
			}
			if debug {
				s.log.Debug("sse close sent", "page", page)
			}
			return
		case <-notify:
			if _, err := io.WriteString(w, "event: reload\ndata: 1\n\n"); err != nil {
				if debug {
					s.log.Debug("sse reload write failed", "page", page, "err", err)
				}
				return
			}
			flusher.Flush()
			reloads++
			if debug {
				s.log.Debug("sse reload sent", "page", page)
			}
		}
	}
}

// watchSetFor computes what the page at urlPath depends on, as absolute OS
// paths under absRoot. A Markdown page depends on its file and the local
// images it references; a folder page on the folder's direct entries and the
// images its README references. urlPath is only a lookup key: a path with a
// ".." segment, or one that is neither Markdown nor a folder, is rejected.
func (s *Server) watchSetFor(root *Root, absRoot, urlPath string) (WatchSet, bool) {
	if absRoot == "" || !strings.HasPrefix(urlPath, "/") || strings.ContainsRune(urlPath, 0) {
		return WatchSet{}, false
	}
	if filepath.Separator != '/' && strings.ContainsRune(urlPath, filepath.Separator) {
		return WatchSet{}, false
	}
	for _, seg := range strings.Split(urlPath, "/") {
		if seg == ".." {
			return WatchSet{}, false
		}
	}
	clean := path.Clean(urlPath)
	own, ok := toOSPath(absRoot, clean)
	if !ok {
		return WatchSet{}, false
	}

	var ws WatchSet
	var mdPath, refDir string
	switch root.Classify(clean) {
	case KindMarkdown:
		ws.Files = append(ws.Files, own)
		mdPath, refDir = clean, path.Dir(clean)
	case KindDir:
		ws.Dirs = append(ws.Dirs, own)
		if name, ok := root.Readme(clean); ok {
			mdPath, refDir = path.Join(clean, name), clean
		}
	default:
		return WatchSet{}, false
	}

	if mdPath != "" {
		if content, err := root.ReadFile(mdPath); err == nil {
			for _, ref := range s.parser.LocalRefs(content) {
				rel, ok := resolveRef(refDir, ref)
				if !ok {
					continue
				}
				if p, ok := toOSPath(absRoot, "/"+rel); ok {
					ws.Files = append(ws.Files, p)
				}
			}
		}
	}
	return ws, true
}

// resolveRef resolves an image reference against the URL folder dir and
// returns it relative to the root, or false if it would leave the root.
func resolveRef(dir, ref string) (string, bool) {
	var rel string
	if strings.HasPrefix(ref, "/") {
		rel = path.Clean(strings.TrimLeft(ref, "/"))
	} else {
		rel = path.Join(strings.TrimLeft(dir, "/"), ref)
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, "../") {
		return "", false
	}
	return rel, true
}

// toOSPath maps a cleaned URL path to an OS path under absRoot, or reports
// false if the result would not stay inside absRoot.
func toOSPath(absRoot, urlPath string) (string, bool) {
	p := filepath.Join(absRoot, filepath.FromSlash(strings.TrimPrefix(urlPath, "/")))
	rel, err := filepath.Rel(absRoot, p)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	return p, true
}

func (s *Server) serveMarkdown(w http.ResponseWriter, r *http.Request, root *Root) {
	setNoCacheHeaders(w)

	content, err := root.ReadFile(r.URL.Path)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	htmlContent, err := s.parser.MdToHTML(content)
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	var buf bytes.Buffer
	err = renderTemplate(&buf, htmlStruct{
		Content:      string(htmlContent),
		BoundingBox:  s.boundingBox,
		CssCodeLight: getCssCode("github"),
		CssCodeDark:  getCssCode("github-dark"),
		Title:        html.EscapeString(s.pageTitle(r.URL.Path)),
		Events:       s.streaming(),
		Reload:       s.reloading(),
		ReplaceNav:   s.replaceNav,
		Features:     s.parser.TemplateFeatures(),
		BackHref:     html.EscapeString(backHref(r.URL.Path)),
		BackLabel:    html.EscapeString(backLabel(r.URL.Path)),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) serveFolder(w http.ResponseWriter, r *http.Request, root *Root) {
	dirPath := r.URL.Path
	if !strings.HasSuffix(dirPath, "/") {
		// Collapse leading slashes so the redirect can never be read as a
		// protocol-relative URL to another host.
		target := url.URL{Path: "/" + strings.TrimLeft(dirPath, "/") + "/", RawQuery: r.URL.RawQuery}
		http.Redirect(w, r, target.String(), http.StatusMovedPermanently)
		return
	}

	setNoCacheHeaders(w)
	stripCacheValidators(r)

	entries, err := root.List(dirPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		s.serverError(w, r, err)
		return
	}

	var readme htmltemplate.HTML
	if name, ok := root.Readme(dirPath); ok {
		content, err := root.ReadFile(path.Join(dirPath, name))
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		rendered, err := s.parser.MdToHTML(content)
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		// Goldmark output is trusted, as it is for Markdown pages.
		readme = htmltemplate.HTML(rendered) //nolint:gosec
	}

	listing, err := renderListing(dirPath, entries, readme, time.Now())
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	title := defaultHTMLTitle
	if dirPath != "/" {
		title = s.pageTitle(strings.TrimSuffix(dirPath, "/"))
	}

	var buf bytes.Buffer
	err = renderTemplate(&buf, htmlStruct{
		Content:      listing,
		BoundingBox:  s.boundingBox,
		CssCodeLight: getCssCode("github"),
		CssCodeDark:  getCssCode("github-dark"),
		Title:        html.EscapeString(title),
		IsListing:    true,
		Events:       s.streaming(),
		Reload:       s.reloading(),
		ReplaceNav:   s.replaceNav,
		Features:     s.parser.TemplateFeatures(),
	})
	if err != nil {
		s.serverError(w, r, err)
		return
	}

	w.Header().Set("Content-Type", "text/html")
	_, _ = w.Write(buf.Bytes())
}

func (s *Server) serverError(w http.ResponseWriter, r *http.Request, err error) {
	s.log.Error("❌ request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
}

type htmlStruct struct {
	Content      string
	BoundingBox  bool
	CssCodeLight string
	CssCodeDark  string
	Title        string
	IsListing    bool
	// Events reports whether the page opens the event stream.
	Events bool
	// Reload reports whether the page reloads on "reload" events and when
	// the stream reconnects. Without it the stream only carries "close".
	Reload bool
	// ReplaceNav reports whether in-preview navigation replaces the current
	// history entry instead of adding one.
	ReplaceNav bool
	// BackHref and BackLabel describe the link from a Markdown page to its
	// folder. Both are HTML-escaped; they are empty on folder pages.
	BackHref  string
	BackLabel string
	// Features reports which optional Markdown features are enabled, so the
	// template only loads their assets when needed.
	Features map[string]bool
}

func (s *Server) pageTitle(filename string) string {
	title := formatFilenameTitle(filename)
	if title == "" {
		return defaultHTMLTitle
	}
	return title
}

func formatFilenameTitle(filename string) string {
	filename = path.Base(filename)
	extension := path.Ext(filename)
	if strings.EqualFold(extension, ".md") {
		filename = strings.TrimSuffix(filename, extension)
	}

	filename = strings.Map(func(r rune) rune {
		if r == '-' || r == '_' {
			return ' '
		}
		return r
	}, filename)

	words := strings.Fields(filename)
	for i, word := range words {
		first, size := utf8.DecodeRuneInString(word)
		words[i] = string(unicode.ToUpper(first)) + word[size:]
	}
	return strings.Join(words, " ")
}

func renderTemplate(w io.Writer, html htmlStruct) error {
	tmpl, err := template.ParseFS(defaults.Templates, "templates/layout.html")
	if err != nil {
		return err
	}
	return tmpl.Execute(w, html)
}

func getCssCode(style string) string {
	buf := new(strings.Builder)
	formatter := chroma_html.New(chroma_html.WithClasses(true))
	s := styles.Get(style)
	_ = formatter.WriteCSS(buf, s)
	return buf.String()
}

func setNoCacheHeaders(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
}

func stripCacheValidators(r *http.Request) {
	r.Header.Del("If-Modified-Since")
	r.Header.Del("If-None-Match")
}
