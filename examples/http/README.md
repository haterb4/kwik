#!/bin/bash

echo "==================================="
echo "Guide de Test KwikHTTP"
echo "==================================="
echo

echo "Étape 1 : Démarrer le serveur"
echo "------------------------------"
echo "Dans un premier terminal, exécutez :"
echo "  cd /home/bradley/imt/kwik/kwikv2/examples/http"
echo "  go run server/example_server.go"
echo

echo "Étape 2 : Lancer le client (dans un autre terminal)"
echo "---------------------------------------------------"
echo "Dans un second terminal, exécutez :"
echo "  cd /home/bradley/imt/kwik/kwikv2/examples/http"
echo "  go run client/example_client.go"
echo

echo "Étape 3 : Observer les échanges"
echo "-------------------------------"
echo "Le client va effectuer plusieurs tests :"
echo "  - Connectivité (ping)"
echo "  - Requête GET /"
echo "  - Requête GET /api/status"
echo "  - Requête POST /api/echo"
echo "  - Upload de fichier"
echo "  - Headers personnalisés"
echo

echo "Note: Le serveur utilise des certificats auto-signés"
echo "      L'option InsecureSkipVerify=true est activée"
echo
