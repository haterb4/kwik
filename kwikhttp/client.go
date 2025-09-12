// Package kwikhttp - Client HTTP au-dessus de KWIK multipath
package kwikhttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net/url"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/s-anzie/kwik"
)

// HTTPClient représente un client HTTP utilisant KWIK comme transport
type HTTPClient struct {
	session   kwik.Session
	tlsConfig *tls.Config
	timeout   time.Duration
	userAgent string
}

// ClientConfig contient la configuration du client HTTP
type ClientConfig struct {
	TLSConfig       *tls.Config
	Timeout         time.Duration
	UserAgent       string
	MaxIdleTimeout  time.Duration
	KeepAlivePeriod time.Duration
}

// DefaultClientConfig retourne une configuration par défaut pour le client
func DefaultClientConfig() *ClientConfig {
	return &ClientConfig{
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"kwik-http"},
		},
		Timeout:         30 * time.Second,
		UserAgent:       "KwikHTTP/1.0",
		MaxIdleTimeout:  60 * time.Second,
		KeepAlivePeriod: 30 * time.Second,
	}
}

// NewHTTPClient crée un nouveau client HTTP
func NewHTTPClient(serverAddr string, config *ClientConfig) (*HTTPClient, error) {
	if config == nil {
		config = DefaultClientConfig()
	}

	// Établir la connexion QUIC
	session, err := kwik.DialAddr(context.Background(), serverAddr, config.TLSConfig, &quic.Config{
		MaxIdleTimeout:  config.MaxIdleTimeout,
		KeepAlivePeriod: config.KeepAlivePeriod,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to establish QUIC connection: %v", err)
	}

	client := &HTTPClient{
		session:   session,
		tlsConfig: config.TLSConfig,
		timeout:   config.Timeout,
		userAgent: config.UserAgent,
	}

	return client, nil
}

// Get effectue une requête HTTP GET
func (c *HTTPClient) Get(url string) (*HTTPResponse, error) {
	request := NewHTTPRequest(GET, url)
	request.SetHeader("User-Agent", c.userAgent)
	request.SetHeader("Accept", "*/*")
	request.SetHeader("Connection", "keep-alive")

	return c.Do(request)
}

// Post effectue une requête HTTP POST
func (c *HTTPClient) Post(url string, contentType string, body []byte) (*HTTPResponse, error) {
	request := NewHTTPRequest(POST, url)
	request.SetHeader("User-Agent", c.userAgent)
	request.SetHeader("Content-Type", contentType)
	request.SetHeader("Accept", "*/*")
	request.SetHeader("Connection", "keep-alive")
	request.SetBody(body)

	return c.Do(request)
}

// PostString effectue une requête HTTP POST avec un body string
func (c *HTTPClient) PostString(url string, contentType string, body string) (*HTTPResponse, error) {
	return c.Post(url, contentType, []byte(body))
}

// Put effectue une requête HTTP PUT
func (c *HTTPClient) Put(url string, contentType string, body []byte) (*HTTPResponse, error) {
	request := NewHTTPRequest(PUT, url)
	request.SetHeader("User-Agent", c.userAgent)
	request.SetHeader("Content-Type", contentType)
	request.SetHeader("Accept", "*/*")
	request.SetHeader("Connection", "keep-alive")
	request.SetBody(body)

	return c.Do(request)
}

// Delete effectue une requête HTTP DELETE
func (c *HTTPClient) Delete(url string) (*HTTPResponse, error) {
	request := NewHTTPRequest(DELETE, url)
	request.SetHeader("User-Agent", c.userAgent)
	request.SetHeader("Accept", "*/*")
	request.SetHeader("Connection", "keep-alive")

	return c.Do(request)
}

// Do effectue une requête HTTP personnalisée
func (c *HTTPClient) Do(request *HTTPRequest) (*HTTPResponse, error) {
	// Ouvrir un nouveau stream pour cette requête
	stream, err := c.session.OpenStreamSync(context.Background())
	if err != nil {
		return nil, fmt.Errorf("failed to open stream: %v", err)
	}
	defer stream.Close()

	// Sérialiser et envoyer la requête
	requestData := SerializeHTTPRequest(request)

	// Envoyer la requête avec timeout
	done := make(chan error, 1)
	go func() {
		_, err := stream.Write(requestData)
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			return nil, fmt.Errorf("failed to send request: %v", err)
		}
	case <-time.After(c.timeout):
		return nil, fmt.Errorf("request timeout after %v", c.timeout)
	}

	// Lire la réponse avec timeout
	responseData := bytes.NewBuffer(nil)
	responseDone := make(chan error, 1)

	go func() {
		_, err := io.Copy(responseData, stream)
		responseDone <- err
	}()

	select {
	case err := <-responseDone:
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("failed to read response: %v", err)
		}
	case <-time.After(c.timeout):
		return nil, fmt.Errorf("response timeout after %v", c.timeout)
	}

	// Parser la réponse
	response, err := ParseHTTPResponse(responseData)
	if err != nil {
		return nil, fmt.Errorf("failed to parse response: %v", err)
	}

	return response, nil
}

// SetTimeout configure le timeout des requêtes
func (c *HTTPClient) SetTimeout(timeout time.Duration) {
	c.timeout = timeout
}

// SetUserAgent configure le User-Agent
func (c *HTTPClient) SetUserAgent(userAgent string) {
	c.userAgent = userAgent
}

// Close ferme la connexion du client
func (c *HTTPClient) Close() error {
	if c.session != nil {
		return c.session.CloseWithError(0, "Client closing")
	}
	return nil
}

// GetSession retourne la session QUIC sous-jacente pour un accès avancé
func (c *HTTPClient) GetSession() kwik.Session {
	return c.session
}

// IsConnected vérifie si la connexion est toujours active
func (c *HTTPClient) IsConnected() bool {
	if c.session == nil {
		return false
	}

	select {
	case <-c.session.Context().Done():
		return false
	default:
		return true
	}
}

// Ping envoie une requête de ping pour tester la connectivité
func (c *HTTPClient) Ping() error {
	request := NewHTTPRequest(HEAD, "/")
	request.SetHeader("User-Agent", c.userAgent)
	request.SetHeader("Connection", "keep-alive")

	_, err := c.Do(request)
	return err
}

// DownloadFile télécharge un fichier via HTTP GET
func (c *HTTPClient) DownloadFile(url string) ([]byte, *HTTPResponse, error) {
	response, err := c.Get(url)
	if err != nil {
		return nil, nil, err
	}

	if response.StatusCode != StatusOK {
		return nil, response, fmt.Errorf("HTTP %d: %s", response.StatusCode, response.StatusText)
	}

	return response.Body, response, nil
}

// UploadFile upload un fichier via HTTP POST
func (c *HTTPClient) UploadFile(url string, filename string, data []byte, contentType string) (*HTTPResponse, error) {
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	request := NewHTTPRequest(POST, url)
	request.SetHeader("User-Agent", c.userAgent)
	request.SetHeader("Content-Type", contentType)
	request.SetHeader("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	request.SetHeader("Accept", "*/*")
	request.SetHeader("Connection", "keep-alive")
	request.SetBody(data)

	return c.Do(request)
}

// PostJSON envoie une requête POST avec du JSON
func (c *HTTPClient) PostJSON(url string, jsonData []byte) (*HTTPResponse, error) {
	return c.Post(url, "application/json", jsonData)
}

// PostForm envoie une requête POST avec des données de formulaire
func (c *HTTPClient) PostForm(url string, formData url.Values) (*HTTPResponse, error) {
	return c.PostString(url, "application/x-www-form-urlencoded", formData.Encode())
}
