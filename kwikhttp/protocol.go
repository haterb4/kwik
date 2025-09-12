// Package kwikhttp implémente le protocole HTTP au-dessus de KWIK multipath
package kwikhttp

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"strconv"
	"strings"
)

// HTTPVersion représente la version du protocole HTTP
type HTTPVersion string

const (
	HTTP11 HTTPVersion = "HTTP/1.1"
	HTTP2  HTTPVersion = "HTTP/2.0"
	HTTP3  HTTPVersion = "HTTP/3.0"
)

// HTTPMethod représente les méthodes HTTP
type HTTPMethod string

const (
	GET     HTTPMethod = "GET"
	POST    HTTPMethod = "POST"
	PUT     HTTPMethod = "PUT"
	DELETE  HTTPMethod = "DELETE"
	HEAD    HTTPMethod = "HEAD"
	OPTIONS HTTPMethod = "OPTIONS"
	PATCH   HTTPMethod = "PATCH"
)

// HTTPStatusCode représente les codes de statut HTTP
type HTTPStatusCode int

const (
	StatusOK                  HTTPStatusCode = 200
	StatusCreated             HTTPStatusCode = 201
	StatusAccepted            HTTPStatusCode = 202
	StatusNoContent           HTTPStatusCode = 204
	StatusMovedPermanently    HTTPStatusCode = 301
	StatusFound               HTTPStatusCode = 302
	StatusNotModified         HTTPStatusCode = 304
	StatusBadRequest          HTTPStatusCode = 400
	StatusUnauthorized        HTTPStatusCode = 401
	StatusForbidden           HTTPStatusCode = 403
	StatusNotFound            HTTPStatusCode = 404
	StatusMethodNotAllowed    HTTPStatusCode = 405
	StatusInternalServerError HTTPStatusCode = 500
	StatusNotImplemented      HTTPStatusCode = 501
	StatusBadGateway          HTTPStatusCode = 502
	StatusServiceUnavailable  HTTPStatusCode = 503
)

// HTTPHeaders représente les en-têtes HTTP
type HTTPHeaders map[string][]string

func NewHTTPHeaders() HTTPHeaders {
	return make(HTTPHeaders)
}

func (h HTTPHeaders) Add(key, value string) {
	key = strings.ToLower(key)
	h[key] = append(h[key], value)
}

func (h HTTPHeaders) Set(key, value string) {
	key = strings.ToLower(key)
	h[key] = []string{value}
}

func (h HTTPHeaders) Get(key string) string {
	key = strings.ToLower(key)
	if values := h[key]; len(values) > 0 {
		return values[0]
	}
	return ""
}

func (h HTTPHeaders) GetAll(key string) []string {
	key = strings.ToLower(key)
	return h[key]
}

func (h HTTPHeaders) Del(key string) {
	key = strings.ToLower(key)
	delete(h, key)
}

func (h HTTPHeaders) Has(key string) bool {
	key = strings.ToLower(key)
	_, exists := h[key]
	return exists
}

// HTTPRequest représente une requête HTTP
type HTTPRequest struct {
	Method     HTTPMethod
	URL        string
	Version    HTTPVersion
	Headers    HTTPHeaders
	Body       []byte
	RemoteAddr string
}

func NewHTTPRequest(method HTTPMethod, url string) *HTTPRequest {
	return &HTTPRequest{
		Method:  method,
		URL:     url,
		Version: HTTP11,
		Headers: NewHTTPHeaders(),
		Body:    nil,
	}
}

func (r *HTTPRequest) SetHeader(key, value string) {
	r.Headers.Set(key, value)
}

func (r *HTTPRequest) AddHeader(key, value string) {
	r.Headers.Add(key, value)
}

func (r *HTTPRequest) GetHeader(key string) string {
	return r.Headers.Get(key)
}

func (r *HTTPRequest) SetBody(body []byte) {
	r.Body = body
	r.Headers.Set("Content-Length", strconv.Itoa(len(body)))
}

func (r *HTTPRequest) SetBodyString(body string) {
	r.SetBody([]byte(body))
}

// HTTPResponse représente une réponse HTTP
type HTTPResponse struct {
	Version    HTTPVersion
	StatusCode HTTPStatusCode
	StatusText string
	Headers    HTTPHeaders
	Body       []byte
}

func NewHTTPResponse(statusCode HTTPStatusCode) *HTTPResponse {
	return &HTTPResponse{
		Version:    HTTP11,
		StatusCode: statusCode,
		StatusText: GetStatusText(statusCode),
		Headers:    NewHTTPHeaders(),
		Body:       nil,
	}
}

func (r *HTTPResponse) SetHeader(key, value string) {
	r.Headers.Set(key, value)
}

func (r *HTTPResponse) AddHeader(key, value string) {
	r.Headers.Add(key, value)
}

func (r *HTTPResponse) GetHeader(key string) string {
	return r.Headers.Get(key)
}

func (r *HTTPResponse) SetBody(body []byte) {
	r.Body = body
	r.Headers.Set("Content-Length", strconv.Itoa(len(body)))
}

func (r *HTTPResponse) SetBodyString(body string) {
	r.SetBody([]byte(body))
}

// GetStatusText retourne le texte associé à un code de statut
func GetStatusText(code HTTPStatusCode) string {
	switch code {
	case StatusOK:
		return "OK"
	case StatusCreated:
		return "Created"
	case StatusAccepted:
		return "Accepted"
	case StatusNoContent:
		return "No Content"
	case StatusMovedPermanently:
		return "Moved Permanently"
	case StatusFound:
		return "Found"
	case StatusNotModified:
		return "Not Modified"
	case StatusBadRequest:
		return "Bad Request"
	case StatusUnauthorized:
		return "Unauthorized"
	case StatusForbidden:
		return "Forbidden"
	case StatusNotFound:
		return "Not Found"
	case StatusMethodNotAllowed:
		return "Method Not Allowed"
	case StatusInternalServerError:
		return "Internal Server Error"
	case StatusNotImplemented:
		return "Not Implemented"
	case StatusBadGateway:
		return "Bad Gateway"
	case StatusServiceUnavailable:
		return "Service Unavailable"
	default:
		return "Unknown"
	}
}

// ParseHTTPRequest parse une requête HTTP depuis un flux de données
func ParseHTTPRequest(reader io.Reader) (*HTTPRequest, error) {
	bufReader := bufio.NewReader(reader)

	// Lire la ligne de requête
	requestLine, err := bufReader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read request line: %v", err)
	}

	requestLine = strings.TrimSpace(requestLine)
	parts := strings.Split(requestLine, " ")
	if len(parts) != 3 {
		return nil, fmt.Errorf("invalid request line format: %s", requestLine)
	}

	request := &HTTPRequest{
		Method:  HTTPMethod(parts[0]),
		URL:     parts[1],
		Version: HTTPVersion(parts[2]),
		Headers: NewHTTPHeaders(),
	}

	// Lire les en-têtes
	for {
		line, err := bufReader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read header: %v", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break // Fin des en-têtes
		}

		colonIndex := strings.Index(line, ":")
		if colonIndex == -1 {
			continue // En-tête malformé, ignorer
		}

		key := strings.TrimSpace(line[:colonIndex])
		value := strings.TrimSpace(line[colonIndex+1:])
		request.Headers.Add(key, value)
	}

	// Lire le body si Content-Length est présent
	if contentLengthStr := request.Headers.Get("content-length"); contentLengthStr != "" {
		contentLength, err := strconv.Atoi(contentLengthStr)
		if err != nil {
			return nil, fmt.Errorf("invalid content-length: %v", err)
		}

		if contentLength > 0 {
			body := make([]byte, contentLength)
			_, err := io.ReadFull(bufReader, body)
			if err != nil {
				return nil, fmt.Errorf("failed to read body: %v", err)
			}
			request.Body = body
		}
	}

	return request, nil
}

// SerializeHTTPRequest sérialise une requête HTTP en bytes
func SerializeHTTPRequest(request *HTTPRequest) []byte {
	var buffer bytes.Buffer

	// Ligne de requête
	buffer.WriteString(fmt.Sprintf("%s %s %s\r\n", request.Method, request.URL, request.Version))

	// En-têtes
	for key, values := range request.Headers {
		for _, value := range values {
			buffer.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}

	// Ligne vide pour séparer les en-têtes du body
	buffer.WriteString("\r\n")

	// Body
	if request.Body != nil {
		buffer.Write(request.Body)
	}

	return buffer.Bytes()
}

// ParseHTTPResponse parse une réponse HTTP depuis un flux de données
func ParseHTTPResponse(reader io.Reader) (*HTTPResponse, error) {
	bufReader := bufio.NewReader(reader)

	// Lire la ligne de statut
	statusLine, err := bufReader.ReadString('\n')
	if err != nil {
		return nil, fmt.Errorf("failed to read status line: %v", err)
	}

	statusLine = strings.TrimSpace(statusLine)
	parts := strings.SplitN(statusLine, " ", 3)
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid status line format: %s", statusLine)
	}

	statusCode, err := strconv.Atoi(parts[1])
	if err != nil {
		return nil, fmt.Errorf("invalid status code: %v", err)
	}

	statusText := ""
	if len(parts) >= 3 {
		statusText = parts[2]
	}

	response := &HTTPResponse{
		Version:    HTTPVersion(parts[0]),
		StatusCode: HTTPStatusCode(statusCode),
		StatusText: statusText,
		Headers:    NewHTTPHeaders(),
	}

	// Lire les en-têtes
	for {
		line, err := bufReader.ReadString('\n')
		if err != nil {
			return nil, fmt.Errorf("failed to read header: %v", err)
		}

		line = strings.TrimSpace(line)
		if line == "" {
			break // Fin des en-têtes
		}

		colonIndex := strings.Index(line, ":")
		if colonIndex == -1 {
			continue // En-tête malformé, ignorer
		}

		key := strings.TrimSpace(line[:colonIndex])
		value := strings.TrimSpace(line[colonIndex+1:])
		response.Headers.Add(key, value)
	}

	// Lire le body si Content-Length est présent
	if contentLengthStr := response.Headers.Get("content-length"); contentLengthStr != "" {
		contentLength, err := strconv.Atoi(contentLengthStr)
		if err != nil {
			return nil, fmt.Errorf("invalid content-length: %v", err)
		}

		if contentLength > 0 {
			body := make([]byte, contentLength)
			_, err := io.ReadFull(bufReader, body)
			if err != nil {
				return nil, fmt.Errorf("failed to read body: %v", err)
			}
			response.Body = body
		}
	}

	return response, nil
}

// SerializeHTTPResponse sérialise une réponse HTTP en bytes
func SerializeHTTPResponse(response *HTTPResponse) []byte {
	var buffer bytes.Buffer

	// Ligne de statut
	buffer.WriteString(fmt.Sprintf("%s %d %s\r\n", response.Version, response.StatusCode, response.StatusText))

	// En-têtes
	for key, values := range response.Headers {
		for _, value := range values {
			buffer.WriteString(fmt.Sprintf("%s: %s\r\n", key, value))
		}
	}

	// Ligne vide pour séparer les en-têtes du body
	buffer.WriteString("\r\n")

	// Body
	if response.Body != nil {
		buffer.Write(response.Body)
	}

	return buffer.Bytes()
}
