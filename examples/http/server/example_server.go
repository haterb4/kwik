// Exemple d'utilisation du serveur HTTP KWIK
package main

import (
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/s-anzie/kwik/kwikhttp"
)

func main() {
	// Créer le serveur HTTP
	config := kwikhttp.DefaultServerConfig()
	config.StaticDir = "./static"

	server, err := kwikhttp.NewHTTPServer("localhost:4433", config)
	if err != nil {
		log.Fatalf("Failed to create server: %v", err)
	}

	// Route simple
	server.HandleFunc("/", func(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
		w.SetHeader("Content-Type", "text/html; charset=utf-8")
		w.WriteString(`<!DOCTYPE html>
<html>
<head><title>KwikHTTP Server</title></head>
<body>
<h1>Bienvenue sur KwikHTTP !</h1>
<p>Ce serveur utilise KWIK multipath comme transport.</p>
<p>URL demandée: ` + req.URL + `</p>
<p>Méthode: ` + string(req.Method) + `</p>
<p>Version: ` + string(req.Version) + `</p>
</body>
</html>`)
	})

	// Route API JSON
	server.HandleFunc("/api/status", func(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
		w.SetHeader("Content-Type", "application/json")
		w.WriteString(`{"status": "ok", "server": "KwikHTTP/1.0", "transport": "KWIK multipath"}`)
	})

	// Route pour tester les requêtes POST
	server.HandleFunc("/api/echo", func(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
		if req.Method == kwikhttp.POST {
			w.SetHeader("Content-Type", "text/plain")
			w.WriteString("Echo: ")
			w.Write(req.Body)
		} else {
			w.WriteHeader(kwikhttp.StatusMethodNotAllowed)
			w.WriteString("Method not allowed")
		}
	})

	// Servir des fichiers statiques
	server.ServeStatic("/static/*", "./static")

	// Middleware de logging
	server.Use(func(next kwikhttp.HTTPHandler) kwikhttp.HTTPHandler {
		return func(req *kwikhttp.HTTPRequest, w *kwikhttp.HTTPResponseWriter) {
			fmt.Printf("[%s] %s %s\n", req.RemoteAddr, req.Method, req.URL)
			next(req, w)
		}
	})

	// Gérer l'arrêt propre
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGTERM)

	go func() {
		<-c
		fmt.Println("\nArrêt du serveur...")
		server.Stop()
		os.Exit(0)
	}()

	// Démarrer le serveur
	fmt.Printf("Serveur HTTP/KWIK démarré sur %s\n", server.Addr())
	fmt.Println("Routes disponibles:")
	fmt.Println("  GET  /              - Page d'accueil")
	fmt.Println("  GET  /api/status    - Statut du serveur (JSON)")
	fmt.Println("  POST /api/echo      - Echo des données POST")
	fmt.Println("  GET  /static/*      - Fichiers statiques")
	fmt.Println("\nAppuyez sur Ctrl+C pour arrêter")

	if err := server.Start(); err != nil {
		log.Fatalf("Server error: %v", err)
	}
}
