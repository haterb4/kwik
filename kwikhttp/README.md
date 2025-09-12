# KwikHTTP - Bibliothèque HTTP au-dessus de KWIK Multipath

KwikHTTP est une implémentation complète du protocole HTTP qui utilise KWIK (notre protocole de transport multipath basé sur QUIC) comme couche de transport sous-jacente.

## Caractéristiques

- **Transport Multipath** : Utilise KWIK pour le transport multipath automatique
- **HTTP/1.1 Compatible** : Implémente les fonctionnalités essentielles d'HTTP/1.1
- **Client et Serveur** : Bibliothèque complète avec client et serveur
- **Context Support** : Support natif des contexts Go pour l'annulation
- **Middleware** : Support des middlewares pour le serveur
- **Fichiers Statiques** : Serveur de fichiers statiques intégré
- **Types Sécurisés** : Types Go stricts pour les méthodes, codes de statut, etc.

## Architecture

```
KwikHTTP (Couche Application)
    ↓
KWIK Session (Couche Transport Multipath)
    ↓
QUIC Streams (Couche Transport)
    ↓
UDP (Couche Réseau)
```

## Composants

### 1. Protocol (`protocol.go`)
- Définitions des types HTTP (méthodes, codes de statut, headers)
- Fonctions de sérialisation/désérialisation des requêtes et réponses
- Support des headers multiples (conforme RFC HTTP)

### 2. Client (`client.go`)
- Client HTTP utilisant KWIK comme transport
- Méthodes conveniences : `Get()`, `Post()`, `Put()`, `Delete()`
- Support des uploads de fichiers et JSON
- Gestion automatique des timeouts
- Vérification de l'état de connexion

### 3. Serveur (`server.go`)
- Serveur HTTP utilisant KWIK comme transport
- Système de routage simple avec wildcards
- Support des middlewares
- Serveur de fichiers statiques
- Gestion gracieuse des erreurs

## Utilisation

### Client

```go
package main

import (
    "github.com/s-anzie/kwik/kwikhttp"
)

func main() {
    // Créer le client
    config := kwikhttp.DefaultClientConfig()
    client, err := kwikhttp.NewHTTPClient("localhost:8443", config)
    if err != nil {
        panic(err)
    }
    defer client.Close()

    // Requête GET simple
    response, err := client.Get("/api/data")
    if err != nil {
        panic(err)
    }

    // Requête POST avec JSON
    jsonData := []byte(`{"message": "Hello"}`)
    response, err = client.PostJSON("/api/submit", jsonData)
    
    // Upload de fichier
    fileData := []byte("contenu du fichier")
    response, err = client.UploadFile("/upload", "test.txt", fileData, "text/plain")
}
```

### Serveur

```go
package main

import (
    "github.com/s-anzie/kwik/kwikhttp"
)

func main() {
    // Créer le serveur
    config := kwikhttp.DefaultServerConfig()
    server, err := kwikhttp.NewHTTPServer("localhost:8443", config)
    if err != nil {
        panic(err)
    }

    // Routes
    server.HandleFunc("/", homeHandler)
    server.HandleFunc("/api/data", apiHandler)
    
    // Middleware de logging
    server.Use(loggingMiddleware)
    
    // Fichiers statiques
    server.ServeStatic("/static/*", "./static")

    // Démarrer le serveur
    server.Start()
}

func homeHandler(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
    w.SetHeader("Content-Type", "text/html")
    w.WriteString("<h1>Bienvenue sur KwikHTTP !</h1>")
}

func apiHandler(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
    w.SetHeader("Content-Type", "application/json")
    w.WriteString(`{"status": "ok"}`)
}

func loggingMiddleware(next kwikhttp.HTTPHandler) kwikhttp.HTTPHandler {
    return func(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
        fmt.Printf("[%s] %s %s\n", req.RemoteAddr, req.Method, req.URL)
        next(req, w)
    }
}
```

## Types Principaux

### HTTPMethod
```go
const (
    GET     HTTPMethod = "GET"
    POST    HTTPMethod = "POST"
    PUT     HTTPMethod = "PUT"
    DELETE  HTTPMethod = "DELETE"
    HEAD    HTTPMethod = "HEAD"
    OPTIONS HTTPMethod = "OPTIONS"
    PATCH   HTTPMethod = "PATCH"
)
```

### HTTPStatusCode
```go
const (
    StatusOK                  HTTPStatusCode = 200
    StatusCreated             HTTPStatusCode = 201
    StatusBadRequest          HTTPStatusCode = 400
    StatusNotFound            HTTPStatusCode = 404
    StatusInternalServerError HTTPStatusCode = 500
    // ... autres codes
)
```

### HTTPHeaders
```go
type HTTPHeaders map[string][]string

// Méthodes principales
func (h HTTPHeaders) Add(key, value string)
func (h HTTPHeaders) Set(key, value string)
func (h HTTPHeaders) Get(key string) string
func (h HTTPHeaders) Has(key string) bool
```

## Context et Fermeture Gracieuse

KwikHTTP utilise les contexts Go pour la gestion de l'annulation et des timeouts :

```go
// Le client vérifie l'état de la connexion
if client.IsConnected() {
    // Connexion active
}

// Le serveur gère l'arrêt gracieux
ctx, cancel := context.WithCancel(context.Background())
// ... lors de l'arrêt
cancel()
server.Stop()
```

## Avantages du Transport Multipath

Grâce à KWIK, KwikHTTP bénéficie automatiquement :

1. **Résistance aux pannes** : Si un chemin réseau tombe, le trafic continue sur les autres
2. **Agrégation de bande passante** : Utilisation simultanée de plusieurs chemins
3. **Réduction de latence** : Sélection automatique du chemin optimal
4. **Mobilité** : Gestion transparente des changements d'adresse IP

## Tests et Exemples

Des exemples complets sont disponibles dans `examples/http/` :

- `examples/http/server/` : Serveur HTTP avec différents handlers
- `examples/http/client/` : Client de test avec différents types de requêtes

## Comparaison avec HTTP Standard

| Fonctionnalité | HTTP Standard | KwikHTTP |
|---------------|---------------|----------|
| Transport | TCP (mono-chemin) | KWIK (multipath) |
| Résistance aux pannes | Non | Oui |
| Agrégation de BP | Non | Oui |
| Performance | Standard | Améliorée |
| Compatibilité API | HTTP/1.1 | HTTP/1.1 |

## Limitations Actuelles

- Pas de support HTTP/2 ou HTTP/3 (pour l'instant)
- Pas de compression automatique (gzip, etc.)
- Pas de cache côté client
- Pas de cookies automatiques

## Roadmap

1. **Phase 1** ✅ : Implémentation de base (client/serveur)
2. **Phase 2** : Compression, cookies, cache
3. **Phase 3** : Support HTTP/2-like avec multiplexage
4. **Phase 4** : Optimisations et benchmarks
