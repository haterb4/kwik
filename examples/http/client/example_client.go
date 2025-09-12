// Exemple d'utilisation du client HTTP KWIK
package main

import (
	"fmt"
	"log"

	"github.com/s-anzie/kwik/kwikhttp"
)

func main() {
	fmt.Println("Client HTTP/KWIK - Test de connexion")
	fmt.Println("=======================================")
	fmt.Println("IMPORTANT: Assurez-vous que le serveur est démarré avec :")
	fmt.Println("  go run server/example_server.go")
	fmt.Println()

	// Créer le client HTTP
	config := kwikhttp.DefaultClientConfig()

	fmt.Println("Tentative de connexion à localhost:4433...")
	client, err := kwikhttp.NewHTTPClient("localhost:4433", config)
	if err != nil {
		log.Fatalf("❌ Échec de création du client: %v\n\nVérifiez que le serveur est démarré !", err)
	}
	defer client.Close()

	fmt.Println("✅ Client HTTP/KWIK créé avec succès")
	fmt.Println("Connexion établie à localhost:4433") // Test de ping
	fmt.Println("\n1. Test de connectivité...")
	err = client.Ping()
	if err != nil {
		log.Printf("Ping failed: %v", err)
	} else {
		fmt.Println("✓ Ping réussi")
	}

	// Test GET sur la page d'accueil
	fmt.Println("\n2. Test GET /...")
	response, err := client.Get("/")
	if err != nil {
		log.Printf("GET / failed: %v", err)
	} else {
		fmt.Printf("✓ Status: %d %s\n", int(response.StatusCode), response.StatusText)
		fmt.Printf("✓ Content-Type: %s\n", response.Headers.Get("content-type"))
		fmt.Printf("✓ Body length: %d bytes\n", len(response.Body))
		if len(response.Body) < 200 {
			fmt.Printf("✓ Body preview: %s\n", string(response.Body))
		}
	}

	// Test GET API status
	fmt.Println("\n3. Test GET /api/status...")
	response, err = client.Get("/api/status")
	if err != nil {
		log.Printf("GET /api/status failed: %v", err)
	} else {
		fmt.Printf("✓ Status: %d %s\n", int(response.StatusCode), response.StatusText)
		fmt.Printf("✓ JSON Response: %s\n", string(response.Body))
	}

	// Test POST echo
	fmt.Println("\n4. Test POST /api/echo...")
	testData := "Hello from KwikHTTP client!"
	response, err = client.PostString("/api/echo", "text/plain", testData)
	if err != nil {
		log.Printf("POST /api/echo failed: %v", err)
	} else {
		fmt.Printf("✓ Status: %d %s\n", int(response.StatusCode), response.StatusText)
		fmt.Printf("✓ Echo Response: %s\n", string(response.Body))
	}

	// Test POST JSON
	fmt.Println("\n5. Test POST JSON...")
	jsonData := `{"message": "Hello from KWIK", "client": "kwikhttp"}`
	response, err = client.PostJSON("/api/echo", []byte(jsonData))
	if err != nil {
		log.Printf("POST JSON failed: %v", err)
	} else {
		fmt.Printf("✓ Status: %d %s\n", int(response.StatusCode), response.StatusText)
		fmt.Printf("✓ JSON Echo: %s\n", string(response.Body))
	}

	// Test d'upload de fichier
	fmt.Println("\n6. Test upload de fichier...")
	fileData := []byte("Contenu du fichier de test\nLigne 2\nLigne 3")
	response, err = client.UploadFile("/api/echo", "test.txt", fileData, "text/plain")
	if err != nil {
		log.Printf("File upload failed: %v", err)
	} else {
		fmt.Printf("✓ Status: %d %s\n", int(response.StatusCode), response.StatusText)
		fmt.Printf("✓ File Echo: %s\n", string(response.Body))
	}

	// Test avec headers personnalisés
	fmt.Println("\n7. Test avec headers personnalisés...")
	request := kwikhttp.NewHTTPRequest(kwikhttp.GET, "/api/status")
	request.SetHeader("X-Custom-Header", "KwikHTTP-Test")
	request.SetHeader("User-Agent", "KwikHTTP-Client/1.0")

	response, err = client.Do(request)
	if err != nil {
		log.Printf("Custom headers request failed: %v", err)
	} else {
		fmt.Printf("✓ Status: %d %s\n", int(response.StatusCode), response.StatusText)
		fmt.Printf("✓ Response: %s\n", string(response.Body))
	}

	// Test de vérification de connexion
	fmt.Println("\n8. Test de vérification de connexion...")
	if client.IsConnected() {
		fmt.Println("✓ Connexion active")
	} else {
		fmt.Println("✗ Connexion fermée")
	}

	fmt.Println("\nTous les tests terminés !")
}
