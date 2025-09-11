# ANALYSE COMPLÈTE DES FLUX D'ÉCRITURE ET LECTURE

## 🔽 FLUX D'ÉCRITURE (Application → Réseau)

### 1. **Point d'entrée : StreamImpl.Write()**
- **Fichier** : `stream.go:254`
- **Rôle** : Interface utilisateur pour écrire des données
- **Composants impliqués** :
  - Vérification du path primaire
  - Buffering dans `SendStream`
  - Déclenchement de `SubmitSendStream`

**FLOW :**
```
Application.Write(data) 
→ StreamImpl.Write() 
→ SendStream.Write() [BUFFER]
→ Path.SubmitSendStream()
```

### 2. **Buffering : SendStream.Write()**
- **Fichier** : `stream.go:127`
- **Rôle** : Stockage temporaire des données en frames
- **Logique** :
  - Copie des données dans `sendBuffer[]`
  - Increment `writeOffset`
  - Pas d'envoi immédiat

**ÉTAT APRÈS :** Données en mémoire, prêtes pour packetisation

### 3. **Déclenchement : Path.SubmitSendStream()**
- **Fichier** : `path.go:149`
- **Rôle** : Demande au Packer de traiter le stream buffered
- **Logique** :
  - Appel à `Packer.SubmitFromSendStream()`

### 4. **Packetisation : Packer.SubmitFromSendStream()**
- **Fichier** : `packer.go:663`
- **Rôle** : Assemblage des frames en paquets
- **Étapes critiques** :
  1. Récupération du `SendStreamProvider`
  2. `PopFrames()` pour obtenir les données
  3. Assemblage du paquet avec header + frames
  4. Assignation d'un numéro de séquence
  5. Stockage en `pending[]` pour retransmission
  6. **ÉCRITURE RÉSEAU** via `Path.WriteStream()`

**POINT CRITIQUE :** Transition mémoire → réseau

### 5. **Sortie réseau : Path.WriteStream()**
- **Fichier** : `path.go:872`
- **Rôle** : Écriture physique sur socket QUIC
- **Logiques spécialisées** :
  - **Stream 0** : Control stream direct
  - **Autres streams** : Pool round-robin de streams QUIC

**COMPOSANTS DE SORTIE :**
- `quic.Stream.Write()` (bibliothèque quic-go)
- Gestion des erreurs réseau
- Tracking du nombre d'octets envoyés

---

## 🔼 FLUX DE RÉCEPTION (Réseau → Application)

### 1. **Entrée réseau : Path.runStreamReader()**
- **Fichier** : `path.go:372`
- **Rôle** : Lecture des données depuis les streams QUIC
- **Logique** :
  - Lecture en boucle sur pool de streams
  - Décodage length-prefixed packets
  - **DISPATCH** vers `Multiplexer.PushPacket()`

**POINT CRITIQUE :** Transition réseau → mémoire

### 2. **Dépacketisation : Multiplexer.PushPacket()**
- **Fichier** : `multiplexer.go:121`
- **Rôle** : Désérialisation et dispatch des frames
- **Étapes critiques** :
  1. **Désérialisation** du paquet
  2. **Classification** des frames (Stream vs Control)
  3. **AckFrame** → `Packer.OnAck()` pour retransmissions
  4. **StreamFrame** → Stream handlers ou Reception buffers
  5. **Control frames** → `Path.HandleControlFrame()`

### 3. **Buffering réception : ReceptionBuffer**
- **Fichier** : `multiplexer.go:25`
- **Rôle** : Réordonnancement et assemblage des frames
- **Composants** :
  - `StreamBuffer` pour stockage par offset
  - Déduplication par key "streamID:offset"
  - Assemblage continu des données

### 4. **Livraison : StreamFrameHandler**
- **Fichier** : `stream.go:114` (RecvStream.HandleStreamFrame)
- **Rôle** : Livraison directe aux streams utilisateur
- **Logique** :
  - Vérification de l'ordre (`expectedOffset`)
  - Buffering hors-ordre dans `buffered[]`
  - Signal `notifyCh` pour débloquer les `Read()`

### 5. **Interface utilisateur : StreamImpl.Read()**
- **Fichier** : `stream.go:300+`
- **Rôle** : Interface pour l'application
- **Logique** :
  - Lecture depuis `RecvStream.readBuf`
  - Attente sur `notifyCh` si pas de données

---

## 🚨 PROBLÈMES IDENTIFIÉS ET POINTS D'ATTENTION

### **CÔTÉ ÉCRITURE**

#### ❌ **Problème 1 : Coordination Packer ↔ SendStream**
```go
// stream.go:271 - Appel SubmitSendStream après chaque Write
if err := primaryPath.SubmitSendStream(s.id); err != nil {
```

**PROBLÈME :** Appel systématique du packer après chaque write peut causer :
- Fragmentation excessive (petits paquets)
- Surcharge CPU (appels fréquents)
- Perte d'efficacité batching

**SOLUTION SUGGÉRÉE :** Batching intelligent ou flush conditionnel

#### ❌ **Problème 2 : Pool de streams QUIC incohérent**
```go
// path.go:920+ - Pool round-robin sans état par logical stream
quicStream = p.quicStreamPool[p.quicStreamPoolIdx%poolLen]
p.quicStreamPoolIdx = (p.quicStreamPoolIdx + 1) % poolLen
```

**PROBLÈME :** Les données d'un même logical stream peuvent être envoyées sur différents QUIC streams, cassant potentiellement l'ordre de livraison

**SOLUTION SUGGÉRÉE :** Mapping persistent logical stream → QUIC stream

#### ❌ **Problème 3 : Gestion d'erreurs WriteStream**
```go
// path.go:905 - Retour partiel en cas d'erreur
if err != nil {
    return total, err  // Données partiellement envoyées
}
```

**PROBLÈME :** Données partiellement envoyées sans mécanisme de récupération au niveau logical stream

### **CÔTÉ RÉCEPTION**

#### ❌ **Problème 4 : Concurrence ReceptionBuffer**
```go
// multiplexer.go:38 - Accès concurrent possible
func (rb *ReceptionBuffer) PushStreamFrame(sf *protocol.StreamFrame) {
    rb.mu.Lock()  // OK
    defer rb.mu.Unlock()
    // mais m.receptionBuffers access sans mutex complet
}
```

**PROBLÈME :** Race condition possible lors de création/accès concurrents aux buffers

#### ❌ **Problème 5 : Désynchronisation offset tracking**
```go
// stream.go:42 - expectedOffset management
if f.Offset == s.expectedOffset {
    s.expectedOffset += uint64(len(f.Data))
}
```

**PROBLÈME :** Pas de validation contre les métadonnées du Path pour les séquences d'écriture/lecture

#### ❌ **Problème 6 : Memory leak dans receptionBuffers**
```go
// multiplexer.go:214 - Pas de cleanup automatique
func (m *Multiplexer) getOrCreateReceptionBuffer(streamID protocol.StreamID) {
    // Création mais pas de suppression systématique
}
```

**PROBLÈME :** Les buffers peuvent s'accumuler sans être nettoyés lors de fermeture de streams

### **COORDINATION GÉNÉRALE**

#### ❌ **Problème 7 : Découplage Path ↔ Stream**
- **WriteSeq/ReadSeq** dans Path vs **writeOffset** dans SendStream
- Pas de vérification de cohérence entre les deux

#### ❌ **Problème 8 : Retransmission partielle**
- Le Packer gère la retransmission au niveau paquet
- Mais pas de coordination avec l'état des logical streams
- Risque de re-transmission de données déjà reçues

---

## 🔧 RECOMMANDATIONS DE CORRECTIONS

### **PRIORITÉ 1 : Cohérence des offsets**
1. Synchroniser `Path.WriteSeq` avec `SendStream.writeOffset`
2. Valider les offsets reçus contre `Path.readSeq`

### **PRIORITÉ 2 : Pool de streams intelligent**
1. Mapping deterministe logical stream → QUIC stream (l'option 2 est la plus acceptable)
2. Ou utilisation d'un seul QUIC stream avec multiplexage explicite

### **PRIORITÉ 3 : Batching et performance**
1. Flush conditionnel du Packer (taille ou timeout)
2. Éviter les appels systématiques après chaque Write

### **PRIORITÉ 4 : Resource cleanup**
1. Cleanup automatique des ReceptionBuffers
2. Gestion d'erreurs robuste avec rollback

### **PRIORITÉ 5 : Tests de cohérence**
1. Tests de stress avec writes/reads concurrents
2. Tests de perte réseau et récupération
3. Tests de memory leaks sur création/destruction de streams
