// Package kwikhttp - Serveur HTTP au-dessus de KWIK multipath
package kwikhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/s-anzie/kwik"
)

// HTTPServer représente un serveur HTTP utilisant KWIK comme transport
type HTTPServer struct {
	listener   kwik.Listener
	tlsConfig  *tls.Config
	timeout    time.Duration
	mu         sync.RWMutex
	handlers   map[string]HTTPHandler
	middleware []MiddlewareFunc
	staticDir  string
	running    bool
	ctx        context.Context
	cancel     context.CancelFunc
}

// HTTPHandler représente un gestionnaire de requête HTTP
type HTTPHandler func(*HTTPRequest, *HTTPResponseWriter)

// MiddlewareFunc représente une fonction middleware
type MiddlewareFunc func(HTTPHandler) HTTPHandler

// HTTPResponseWriter permet d'écrire la réponse HTTP
type HTTPResponseWriter struct {
	statusCode HTTPStatusCode
	headers    HTTPHeaders
	body       *bytes.Buffer
	written    bool
}

// ServerConfig contient la configuration du serveur HTTP
type ServerConfig struct {
	TLSConfig       *tls.Config
	Timeout         time.Duration
	StaticDir       string
	MaxIdleTimeout  time.Duration
	KeepAlivePeriod time.Duration
}

// DefaultServerConfig retourne une configuration par défaut pour le serveur
func DefaultServerConfig() *ServerConfig {
	return &ServerConfig{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"kwik-http"},
		},
		Timeout:         30 * time.Second,
		StaticDir:       "./static",
		MaxIdleTimeout:  60 * time.Second,
		KeepAlivePeriod: 30 * time.Second,
	}
}

// NewHTTPServer crée un nouveau serveur HTTP
func NewHTTPServer(addr string, config *ServerConfig) (*HTTPServer, error) {
	if config == nil {
		config = DefaultServerConfig()
	}

	// Créer le listener KWIK
	listener, err := kwik.ListenAddr(addr, config.TLSConfig, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create KWIK listener: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

	server := &HTTPServer{
		listener:   listener,
		tlsConfig:  config.TLSConfig,
		timeout:    config.Timeout,
		handlers:   make(map[string]HTTPHandler),
		middleware: make([]MiddlewareFunc, 0),
		staticDir:  config.StaticDir,
		running:    false,
		ctx:        ctx,
		cancel:     cancel,
	}

	return server, nil
}

// NewHTTPResponseWriter crée un nouveau writer de réponse
func NewHTTPResponseWriter() *HTTPResponseWriter {
	return &HTTPResponseWriter{
		statusCode: StatusOK,
		headers:    NewHTTPHeaders(),
		body:       bytes.NewBuffer(nil),
		written:    false,
	}
}

// WriteHeader écrit le code de statut
func (w *HTTPResponseWriter) WriteHeader(code HTTPStatusCode) {
	if w.written {
		return
	}
	w.statusCode = code
}

// SetHeader définit un header
func (w *HTTPResponseWriter) SetHeader(key, value string) {
	w.headers.Set(key, value)
}

// Write écrit les données dans le body
func (w *HTTPResponseWriter) Write(data []byte) (int, error) {
	return w.body.Write(data)
}

// WriteString écrit une chaîne dans le body
func (w *HTTPResponseWriter) WriteString(s string) (int, error) {
	return w.body.WriteString(s)
}

// WriteJSON écrit des données JSON
func (w *HTTPResponseWriter) WriteJSON(data []byte) (int, error) {
	w.SetHeader("Content-Type", "application/json")
	return w.Write(data)
}

// GetResponse retourne la réponse HTTP complète
func (w *HTTPResponseWriter) GetResponse() *HTTPResponse {
	if !w.written {
		w.written = true
	}

	response := &HTTPResponse{
		StatusCode: w.statusCode,
		StatusText: GetStatusText(w.statusCode),
		Headers:    NewHTTPHeaders(),
		Body:       w.body.Bytes(),
	}

	// Copier les headers
	for k, values := range w.headers {
		for _, v := range values {
			response.Headers.Add(k, v)
		}
	}

	// Ajouter Content-Length si pas déjà défini
	if !response.Headers.Has("Content-Length") {
		response.Headers.Set("Content-Length", fmt.Sprintf("%d", len(response.Body)))
	}

	return response
}

// Handle enregistre un gestionnaire pour un pattern de route
func (s *HTTPServer) Handle(pattern string, handler HTTPHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[pattern] = handler
}

// HandleFunc enregistre une fonction gestionnaire pour un pattern de route
func (s *HTTPServer) HandleFunc(pattern string, handler func(*HTTPRequest, *HTTPResponseWriter)) {
	s.Handle(pattern, HTTPHandler(handler))
}

// Use ajoute un middleware à la pile
func (s *HTTPServer) Use(middleware MiddlewareFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.middleware = append(s.middleware, middleware)
}

// ServeStatic configure le serveur pour servir des fichiers statiques
func (s *HTTPServer) ServeStatic(prefix string, dir string) {
	s.HandleFunc(prefix, func(req *HTTPRequest, w *HTTPResponseWriter) {
		s.serveStaticFile(req, w, dir)
	})
}

// Start démarre le serveur HTTP
func (s *HTTPServer) Start() error {
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return fmt.Errorf("server is already running")
	}
	s.running = true
	s.mu.Unlock()

	fmt.Printf("HTTP/KWIK server starting on %s\n", s.listener.Addr())
	if s.staticDir != "" {
		fmt.Printf("Serving static files from: %s\n", s.staticDir)
	}

	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
			session, err := s.listener.Accept(s.ctx)
			if err != nil {
				if s.ctx.Err() != nil {
					return nil // Server fermé
				}
				fmt.Printf("Error accepting session: %v\n", err)
				continue
			}

			go s.handleSession(session)
		}
	}
}

// handleSession gère une session KWIK
func (s *HTTPServer) handleSession(session kwik.Session) {
	defer session.CloseWithError(0, "Session completed")

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-session.Context().Done():
			return
		default:
			stream, err := session.AcceptStream(s.ctx)
			if err != nil {
				if err != io.EOF && s.ctx.Err() == nil {
					fmt.Printf("Error accepting stream: %v\n", err)
				}
				return
			}

			go s.handleHTTPRequest(stream)
		}
	}
}

// handleHTTPRequest traite une requête HTTP
func (s *HTTPServer) handleHTTPRequest(stream kwik.Stream) {
	defer stream.Close()

	// Lire la requête HTTP avec timeout
	requestData := bytes.NewBuffer(nil)
	done := make(chan error, 1)

	go func() {
		_, err := io.Copy(requestData, stream)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil && err != io.EOF {
			fmt.Printf("Error reading request: %v\n", err)
			return
		}
	case <-time.After(s.timeout):
		fmt.Printf("Request timeout after %v\n", s.timeout)
		return
	case <-s.ctx.Done():
		return
	}

	// Parser la requête
	request, err := ParseHTTPRequest(requestData)
	if err != nil {
		fmt.Printf("Error parsing request: %v\n", err)
		s.sendErrorResponse(stream, StatusBadRequest, "Bad Request")
		return
	}

	// Créer le writer de réponse
	writer := NewHTTPResponseWriter()

	// Trouver et exécuter le gestionnaire
	handler := s.findHandler(request.URL)
	if handler == nil {
		s.sendErrorResponse(stream, StatusNotFound, "Not Found")
		return
	}

	// Appliquer les middlewares
	finalHandler := s.applyMiddleware(handler)

	// Exécuter le gestionnaire
	finalHandler(request, writer)

	// Envoyer la réponse
	response := writer.GetResponse()
	responseData := SerializeHTTPResponse(response)

	// Envoyer avec timeout
	done = make(chan error, 1)
	go func() {
		_, err := stream.Write(responseData)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			fmt.Printf("Error sending response: %v\n", err)
		}
	case <-time.After(s.timeout):
		fmt.Printf("Response timeout after %v\n", s.timeout)
	case <-s.ctx.Done():
		return
	}
}

// findHandler trouve le gestionnaire correspondant au chemin
func (s *HTTPServer) findHandler(path string) HTTPHandler {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// Recherche exacte d'abord
	if handler, exists := s.handlers[path]; exists {
		return handler
	}

	// Recherche de préfixe pour les routes avec wildcards
	for pattern, handler := range s.handlers {
		if strings.HasSuffix(pattern, "*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(path, prefix) {
				return handler
			}
		}
	}

	return nil
}

// applyMiddleware applique tous les middlewares
func (s *HTTPServer) applyMiddleware(handler HTTPHandler) HTTPHandler {
	s.mu.RLock()
	defer s.mu.RUnlock()

	result := handler
	// Appliquer en ordre inverse pour respecter l'ordre d'empilement
	for i := len(s.middleware) - 1; i >= 0; i-- {
		result = s.middleware[i](result)
	}
	return result
}

// serveStaticFile sert un fichier statique
func (s *HTTPServer) serveStaticFile(req *HTTPRequest, w *HTTPResponseWriter, baseDir string) {
	// Nettoyer le chemin pour éviter les attaques path traversal
	cleanPath := filepath.Clean(req.URL)
	filePath := filepath.Join(baseDir, cleanPath)

	// Vérifier que le fichier reste dans le répertoire autorisé
	absBaseDir, _ := filepath.Abs(baseDir)
	absFilePath, _ := filepath.Abs(filePath)
	if !strings.HasPrefix(absFilePath, absBaseDir) {
		w.WriteHeader(StatusForbidden)
		w.WriteString("403 Forbidden")
		return
	}

	// Vérifier l'existence du fichier
	info, err := os.Stat(filePath)
	if err != nil {
		if os.IsNotExist(err) {
			w.WriteHeader(StatusNotFound)
			w.WriteString("404 Not Found")
		} else {
			w.WriteHeader(StatusInternalServerError)
			w.WriteString("500 Internal Server Error")
		}
		return
	}

	// Si c'est un répertoire, chercher index.html
	if info.IsDir() {
		indexPath := filepath.Join(filePath, "index.html")
		if _, err := os.Stat(indexPath); err == nil {
			filePath = indexPath
			info, _ = os.Stat(filePath)
		} else {
			w.WriteHeader(StatusForbidden)
			w.WriteString("403 Forbidden - Directory listing not allowed")
			return
		}
	}

	// Ouvrir et lire le fichier
	file, err := os.Open(filePath)
	if err != nil {
		w.WriteHeader(StatusInternalServerError)
		w.WriteString("500 Internal Server Error")
		return
	}
	defer file.Close()

	// Définir le Content-Type
	ext := filepath.Ext(filePath)
	contentType := GetContentType(ext)
	w.SetHeader("Content-Type", contentType)
	w.SetHeader("Content-Length", fmt.Sprintf("%d", info.Size()))

	// Copier le contenu du fichier
	io.Copy(w.body, file)
}

// sendErrorResponse envoie une réponse d'erreur
func (s *HTTPServer) sendErrorResponse(stream kwik.Stream, code HTTPStatusCode, message string) {
	writer := NewHTTPResponseWriter()
	writer.WriteHeader(code)
	writer.SetHeader("Content-Type", "text/html; charset=utf-8")

	html := fmt.Sprintf(`<!DOCTYPE html>
<html>
<head><title>%d %s</title></head>
<body>
<h1>%d %s</h1>
<p>%s</p>
<hr>
<p><em>KwikHTTP/1.0 Server</em></p>
</body>
</html>`, int(code), GetStatusText(code), int(code), GetStatusText(code), message)

	writer.WriteString(html)

	response := writer.GetResponse()
	responseData := SerializeHTTPResponse(response)
	stream.Write(responseData)
}

// Stop arrête le serveur
func (s *HTTPServer) Stop() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.running {
		return fmt.Errorf("server is not running")
	}

	s.cancel()
	s.running = false

	if s.listener != nil {
		return s.listener.Close()
	}

	return nil
}

// Addr retourne l'adresse du serveur
func (s *HTTPServer) Addr() string {
	if s.listener != nil {
		return s.listener.Addr()
	}
	return ""
}

// GetContentType retourne le type MIME pour une extension de fichier
func GetContentType(ext string) string {
	switch strings.ToLower(ext) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css"
	case ".js":
		return "application/javascript"
	case ".json":
		return "application/json"
	case ".xml":
		return "application/xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".ico":
		return "image/x-icon"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".pdf":
		return "application/pdf"
	case ".zip":
		return "application/zip"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".wav":
		return "audio/wav"
	case ".woff":
		return "font/woff"
	case ".woff2":
		return "font/woff2"
	case ".ttf":
		return "font/ttf"
	case ".eot":
		return "application/vnd.ms-fontobject"
	default:
		return "application/octet-stream"
	}
}
