package focalpoint

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/url"
	"sync"

	"github.com/gorilla/websocket"
	cuckoo "github.com/seiflotfy/cuckoofilter"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/util"
	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/ed25519"
	"golang.org/x/crypto/nacl/secretbox"
)

// Mind manages keys and considerations on behalf of a user.
type Mind struct {
	db                    *leveldb.DB
	passphrase            string
	conn                  *websocket.Conn
	outChan               chan Message    // outgoing messages for synchronous requests
	resultChan            chan mindResult // incoming results for synchronous requests
	considerationCallback func(*Consideration)
	filterViewCallback    func(*FilterViewMessage)
	filter                *cuckoo.Filter
	wg                    sync.WaitGroup
}

// NewMind returns a new Mind instance.
func NewMind(mindDbPath string, recover bool) (*Mind, error) {
	var err error
	var db *leveldb.DB
	if recover {
		db, err = leveldb.RecoverFile(mindDbPath, nil)
	} else {
		db, err = leveldb.OpenFile(mindDbPath, nil)
	}
	if err != nil {
		return nil, err
	}
	w := &Mind{db: db}
	if err := w.initializeFilter(); err != nil {
		w.db.Close()
		return nil, err
	}
	return w, nil
}

func (w *Mind) SetPassphrase(passphrase string) (bool, error) {
	// test that the passphrase was the most recent used
	pubKey, err := w.db.Get([]byte{newestPublicKeyPrefix}, nil)
	if err == leveldb.ErrNotFound {
		w.passphrase = passphrase
		return true, nil
	}
	if err != nil {
		return false, err
	}

	// fetch the private key
	privKeyDbKey, err := encodePrivateKeyDbKey(ed25519.PublicKey(pubKey))
	if err != nil {
		return false, err
	}
	encryptedPrivKey, err := w.db.Get(privKeyDbKey, nil)
	if err != nil {
		return false, err
	}

	// decrypt it
	if _, ok := decryptPrivateKey(encryptedPrivKey, passphrase); !ok {
		return false, nil
	}

	// set it
	w.passphrase = passphrase
	return true, nil
}

// NewKeys generates, encrypts and stores new private keys and returns the public keys.
func (w *Mind) NewKeys(count int) ([]ed25519.PublicKey, error) {
	pubKeys := make([]ed25519.PublicKey, count)
	batch := new(leveldb.Batch)

	for i := 0; i < count; i++ {
		// generate a new key
		pubKey, privKey, err := ed25519.GenerateKey(nil)
		if err != nil {
			return nil, err
		}
		pubKeys[i] = pubKey

		// encrypt the private key
		encryptedPrivKey := encryptPrivateKey(privKey, w.passphrase)
		decryptedPrivKey, ok := decryptPrivateKey(encryptedPrivKey, w.passphrase)

		// safety check
		if !ok || !bytes.Equal(decryptedPrivKey, privKey) {
			return nil, fmt.Errorf("Unable to encrypt/decrypt private keys")
		}

		// store the key
		privKeyDbKey, err := encodePrivateKeyDbKey(pubKey)
		if err != nil {
			return nil, err
		}
		batch.Put(privKeyDbKey, encryptedPrivKey)
		if i+1 == count {
			batch.Put([]byte{newestPublicKeyPrefix}, pubKey)
		}

		// update the filter
		if !w.filter.Insert(pubKey[:]) {
			return nil, fmt.Errorf("Error updating filter")
		}
	}

	wo := opt.WriteOptions{Sync: true}
	if err := w.db.Write(batch, &wo); err != nil {
		return nil, err
	}
	return pubKeys, nil
}

// AddKey adds an existing key pair to the database.
func (w *Mind) AddKey(pubKey ed25519.PublicKey, privKey ed25519.PrivateKey) error {
	// encrypt the private key
	encryptedPrivKey := encryptPrivateKey(privKey, w.passphrase)
	decryptedPrivKey, ok := decryptPrivateKey(encryptedPrivKey, w.passphrase)

	// safety check
	if !ok || !bytes.Equal(decryptedPrivKey, privKey) {
		return fmt.Errorf("Unable to encrypt/decrypt private key")
	}

	// store the key
	privKeyDbKey, err := encodePrivateKeyDbKey(pubKey)
	if err != nil {
		return err
	}
	wo := opt.WriteOptions{Sync: true}
	if err := w.db.Put(privKeyDbKey, encryptedPrivKey, &wo); err != nil {
		return err
	}
	return nil
}

// GetKeys returns all of the public keys from the database.
func (w *Mind) GetKeys() ([]ed25519.PublicKey, error) {
	privKeyDbKey, err := encodePrivateKeyDbKey(nil)
	if err != nil {
		return nil, err
	}
	var pubKeys []ed25519.PublicKey
	iter := w.db.NewIterator(util.BytesPrefix(privKeyDbKey), nil)
	for iter.Next() {
		pubKey, err := decodePrivateKeyDbKey(iter.Key())
		if err != nil {
			iter.Release()
			return nil, err
		}
		pubKeys = append(pubKeys, pubKey)
	}
	iter.Release()
	if err := iter.Error(); err != nil {
		return nil, err
	}
	return pubKeys, nil
}

// Retrieve a private key for a given public key
func (w *Mind) GetPrivateKey(pubKey ed25519.PublicKey) (ed25519.PrivateKey, error) {
	// fetch the private key
	privKeyDbKey, err := encodePrivateKeyDbKey(pubKey)
	if err != nil {
		return nil, err
	}
	encryptedPrivKey, err := w.db.Get(privKeyDbKey, nil)
	if err != nil {
		return nil, err
	}
	privKey, ok := decryptPrivateKey(encryptedPrivKey, w.passphrase)
	if !ok {
		return nil, fmt.Errorf("unable to decrypt private key")
	}
	return privKey, nil
}

// Connect connects to a peer for consideration history, imbalance information, and sending new considerations.
// The threat model assumes the peer the mind is speaking to is not an adversary.
func (w *Mind) Connect(addr string, genesisID ViewID, tlsVerify bool) error {
	u := url.URL{Scheme: "wss", Host: addr, Path: "/" + genesisID.String()}
	// by default clients skip verification as most peers are using ephemeral certificates and keys.
	peerDialer.TLSClientConfig.InsecureSkipVerify = !tlsVerify
	conn, _, err := peerDialer.Dial(u.String(), nil)
	if err != nil {
		return err
	}
	w.conn = conn
	w.outChan = make(chan Message)
	w.resultChan = make(chan mindResult, 1)
	return nil
}

// IsConnected returns true if the mind is connected to a peer.
func (w *Mind) IsConnected() bool {
	return w.conn != nil
}

// SetConsiderationCallback sets a callback to receive new considerations relevant to the mind.
func (w *Mind) SetConsiderationCallback(callback func(*Consideration)) {
	w.considerationCallback = callback
}

// SetFilterViewCallback sets a callback to receive new filter views with confirmed considerations relevant to this mind.
func (w *Mind) SetFilterViewCallback(callback func(*FilterViewMessage)) {
	w.filterViewCallback = callback
}

// GetGraph returns a public key's view graph considerations as well as the corresponding view height.
func (w *Mind) GetGraph(pubKey ed25519.PublicKey) (string, int64, error) {
	w.outChan <- Message{Type: "get_graph", Body: GetGraphMessage{PublicKey: pubKey}}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return "", 0, fmt.Errorf("%s", result.err)
	}
	b := new(GraphMessage)
	if err := json.Unmarshal(result.message, b); err != nil {
		return "", 0, err
	}
	return b.Graph, b.Height, nil
}

// GetRanking returns a public key's considerability ranking as well as the corresponding view height.
func (w *Mind) GetRanking(pubKey ed25519.PublicKey) (float64, int64, error) {
	w.outChan <- Message{Type: "get_ranking", Body: GetRankingMessage{PublicKey: pubKey}}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return 0.00, 0, fmt.Errorf("%s", result.err)
	}
	b := new(RankingMessage)
	if err := json.Unmarshal(result.message, b); err != nil {
		return 0.00, 0, err
	}
	return b.Ranking, b.Height, nil
}

// GetRankings returns a set of public key rankings as well as the current view height.
func (w *Mind) GetRankings(pubKeys []ed25519.PublicKey) ([]PublicKeyRanking, int64, error) {
	w.outChan <- Message{Type: "get_rankings", Body: GetRankingsMessage{PublicKeys: pubKeys}}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return nil, 0, fmt.Errorf("%s", result.err)
	}
	b := new(RankingsMessage)
	if err := json.Unmarshal(result.message, b); err != nil {
		return nil, 0, err
	}
	return b.Rankings, b.Height, nil
}

// GetImbalance returns a public key's imbalance as well as the current view height.
func (w *Mind) GetImbalance(pubKey ed25519.PublicKey) (int64, int64, error) {
	w.outChan <- Message{Type: "get_imbalance", Body: GetImbalanceMessage{PublicKey: pubKey}}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return 0, 0, fmt.Errorf("%s", result.err)
	}
	b := new(ImbalanceMessage)
	if err := json.Unmarshal(result.message, b); err != nil {
		return 0, 0, err
	}
	return b.Imbalance, b.Height, nil
}

// GetImbalances returns a set of public key imbalances as well as the current view height.
func (w *Mind) GetImbalances(pubKeys []ed25519.PublicKey) ([]PublicKeyImbalance, int64, error) {
	w.outChan <- Message{Type: "get_imbalances", Body: GetImbalancesMessage{PublicKeys: pubKeys}}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return nil, 0, fmt.Errorf("%s", result.err)
	}
	b := new(ImbalancesMessage)
	if err := json.Unmarshal(result.message, b); err != nil {
		return nil, 0, err
	}
	return b.Imbalances, b.Height, nil
}

// GetTipHeader returns the current tip of the main point's header.
func (w *Mind) GetTipHeader() (ViewID, ViewHeader, error) {
	w.outChan <- Message{Type: "get_tip_header"}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return ViewID{}, ViewHeader{}, fmt.Errorf("%s", result.err)
	}
	th := new(TipHeaderMessage)
	if err := json.Unmarshal(result.message, th); err != nil {
		return ViewID{}, ViewHeader{}, err
	}
	return *th.ViewID, *th.ViewHeader, nil
}

// SetFilter sets the filter for the connection.
func (w *Mind) SetFilter() error {
	m := Message{
		Type: "filter_load",
		Body: FilterLoadMessage{
			Type:   "cuckoo",
			Filter: w.filter.Encode(),
		},
	}
	w.outChan <- m
	result := <-w.resultChan
	if len(result.err) != 0 {
		return fmt.Errorf("%s", result.err)
	}
	return nil
}

// AddFilter sends a message to add a public key to the filter.
func (w *Mind) AddFilter(pubKey ed25519.PublicKey) error {
	m := Message{
		Type: "filter_add",
		Body: FilterAddMessage{
			PublicKeys: []ed25519.PublicKey{pubKey},
		},
	}
	w.outChan <- m
	result := <-w.resultChan
	if len(result.err) != 0 {
		return fmt.Errorf("%s", result.err)
	}
	return nil
}

// Send creates, signs and pushes an consideration out to the network.
func (w *Mind) Send(from, to ed25519.PublicKey, matures, expires int64, memo string) (
	ConsiderationID, error) {
	// fetch the private key
	privKeyDbKey, err := encodePrivateKeyDbKey(from)
	if err != nil {
		return ConsiderationID{}, err
	}
	encryptedPrivKey, err := w.db.Get(privKeyDbKey, nil)
	if err != nil {
		return ConsiderationID{}, err
	}

	// decrypt it
	privKey, ok := decryptPrivateKey(encryptedPrivKey, w.passphrase)
	if !ok {
		return ConsiderationID{}, fmt.Errorf("Unable to decrypt private key")
	}

	// get the current tip header
	_, header, err := w.GetTipHeader()
	if err != nil {
		return ConsiderationID{}, err
	}
	// set these relative to the current height
	if matures != 0 {
		matures = header.Height + matures
	}
	if expires != 0 {
		expires = header.Height + expires
	}

	// create the consideration
	cn := NewConsideration(from, to, matures, expires, header.Height, memo)

	// sign it
	if err := cn.Sign(privKey); err != nil {
		return ConsiderationID{}, err
	}

	// push it
	w.outChan <- Message{Type: "push_consideration", Body: PushConsiderationMessage{Consideration: cn}}
	result := <-w.resultChan

	// handle result
	if len(result.err) != 0 {
		return ConsiderationID{}, fmt.Errorf("%s", result.err)
	}
	ptr := new(PushConsiderationResultMessage)
	if err := json.Unmarshal(result.message, ptr); err != nil {
		return ConsiderationID{}, err
	}
	if len(ptr.Error) != 0 {
		return ConsiderationID{}, fmt.Errorf("%s", ptr.Error)
	}
	return ptr.ConsiderationID, nil
}

// GetConsideration retrieves information about a historic consideration.
func (w *Mind) GetConsideration(id ConsiderationID) (*Consideration, *ViewID, int64, error) {
	w.outChan <- Message{Type: "get_consideration", Body: GetConsiderationMessage{ConsiderationID: id}}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return nil, nil, 0, fmt.Errorf("%s", result.err)
	}
	t := new(ConsiderationMessage)
	if err := json.Unmarshal(result.message, t); err != nil {
		return nil, nil, 0, err
	}
	return t.Consideration, t.ViewID, t.Height, nil
}

// GetPublicKeyConsiderations retrieves information about historic considerations involving the given public key.
func (w *Mind) GetPublicKeyConsiderations(
	pubKey ed25519.PublicKey, startHeight, endHeight int64, startIndex, limit int) (
	startH, stopH int64, stopIndex int, fb []*FilterViewMessage, err error) {
	gpkt := GetPublicKeyConsiderationsMessage{
		PublicKey:   pubKey,
		StartHeight: startHeight,
		StartIndex:  startIndex,
		EndHeight:   endHeight,
		Limit:       limit,
	}
	w.outChan <- Message{Type: "get_public_key_considerations", Body: gpkt}
	result := <-w.resultChan
	if len(result.err) != 0 {
		return 0, 0, 0, nil, fmt.Errorf("%s", result.err)
	}
	pkt := new(PublicKeyConsiderationsMessage)
	if err := json.Unmarshal(result.message, pkt); err != nil {
		return 0, 0, 0, nil, err
	}
	if len(pkt.Error) != 0 {
		return 0, 0, 0, nil, fmt.Errorf("%s", pkt.Error)
	}
	return pkt.StartHeight, pkt.StopHeight, pkt.StopIndex, pkt.FilterViewes, nil
}

// VerifyKey verifies that the private key associated with the given public key is intact in the database.
func (w *Mind) VerifyKey(pubKey ed25519.PublicKey) error {
	// fetch the private key
	privKeyDbKey, err := encodePrivateKeyDbKey(pubKey)
	if err != nil {
		return err
	}
	encryptedPrivKey, err := w.db.Get(privKeyDbKey, nil)
	if err != nil {
		return err
	}

	// decrypt it
	privKey, ok := decryptPrivateKey(encryptedPrivKey, w.passphrase)
	if !ok {
		return fmt.Errorf("Unable to decrypt private key")
	}

	// check to make sure it can be used to derive the same public key
	pubKeyDerived := privKey.Public().(ed25519.PublicKey)
	if !bytes.Equal(pubKeyDerived, pubKey) {
		return fmt.Errorf("Private key cannot be used to derive the same public key. Possibly corrupt.")
	}
	return nil
}

// Used to hold the result of synchronous requests
type mindResult struct {
	err     string
	message json.RawMessage
}

// Run executes the Mind's main loop in its own goroutine.
// It manages reading and writing to the peer WebSocket.
func (w *Mind) Run() {
	w.wg.Add(1)
	go w.run()
}

func (w *Mind) run() {
	defer w.wg.Done()
	defer func() { w.conn = nil }()
	defer close(w.outChan)

	// writer goroutine loop
	w.wg.Add(1)
	go func() {
		defer w.wg.Done()

		for {
			select {
			case message, ok := <-w.outChan:
				if !ok {
					// channel closed
					return
				}

				// send outgoing message to peer
				if err := w.conn.WriteJSON(message); err != nil {
					w.resultChan <- mindResult{err: err.Error()}
				}
			}
		}
	}()

	// reader loop
	for {
		// new message from peer
		messageType, message, err := w.conn.ReadMessage()
		if err != nil {
			w.resultChan <- mindResult{err: err.Error()}
			break
		}
		switch messageType {
		case websocket.TextMessage:
			var body json.RawMessage
			m := Message{Body: &body}
			if err := json.Unmarshal([]byte(message), &m); err != nil {
				w.resultChan <- mindResult{err: err.Error()}
				break
			}
			switch m.Type {
			case "imbalance":
				w.resultChan <- mindResult{message: body}

			case "ranking":
				w.resultChan <- mindResult{message: body}

			case "graph":
				w.resultChan <- mindResult{message: body}

			case "tip_header":
				w.resultChan <- mindResult{message: body}

			case "consideration_relay_policy":
				w.resultChan <- mindResult{message: body}

			case "push_consideration_result":
				w.resultChan <- mindResult{message: body}

			case "consideration":
				w.resultChan <- mindResult{message: body}

			case "public_key_considerations":
				w.resultChan <- mindResult{message: body}

			case "filter_result":
				if len(body) != 0 {
					fr := new(FilterResultMessage)
					if err := json.Unmarshal(body, fr); err != nil {
						log.Printf("Error: %s, from: %s\n", err, w.conn.RemoteAddr())
						w.resultChan <- mindResult{err: err.Error()}
						break
					}
					w.resultChan <- mindResult{err: fr.Error}
				} else {
					w.resultChan <- mindResult{}
				}

			case "push_consideration":
				pt := new(PushConsiderationMessage)
				if err := json.Unmarshal(body, pt); err != nil {
					log.Printf("Error: %s, from: %s\n", err, w.conn.RemoteAddr())
					break
				}
				if w.considerationCallback != nil {
					w.considerationCallback(pt.Consideration)
				}

			case "filter_view":
				fb := new(FilterViewMessage)
				if err := json.Unmarshal(body, fb); err != nil {
					log.Printf("Error: %s, from: %s\n", err, w.conn.RemoteAddr())
					break
				}
				if w.filterViewCallback != nil {
					w.filterViewCallback(fb)
				}
			}

		case websocket.CloseMessage:
			fmt.Printf("Received close message from: %s\n", w.conn.RemoteAddr())
			break
		}
	}
}

// Shutdown is called to shutdown the mind synchronously.
func (w *Mind) Shutdown() error {
	var addr string
	if w.conn != nil {
		addr = w.conn.RemoteAddr().String()
		w.conn.Close()
	}
	w.wg.Wait()
	if len(addr) != 0 {
		log.Printf("Closed connection with %s\n", addr)
	}
	return w.db.Close()
}

// Initialize the filter
func (w *Mind) initializeFilter() error {
	var capacity int = 4096
	pubKeys, err := w.GetKeys()
	if err != nil {
		return err
	}
	if len(pubKeys) > capacity/2 {
		capacity = len(pubKeys) * 2
	}
	w.filter = cuckoo.NewFilter(uint(capacity))
	for _, pubKey := range pubKeys {
		if !w.filter.Insert(pubKey[:]) {
			return fmt.Errorf("Error building filter")
		}
	}
	return nil
}

// leveldb schema

// n         -> newest public key
// k{pubkey} -> encrypted private key

const newestPublicKeyPrefix = 'n'

const privateKeyPrefix = 'k'

func encodePrivateKeyDbKey(pubKey ed25519.PublicKey) ([]byte, error) {
	key := new(bytes.Buffer)
	if err := key.WriteByte(privateKeyPrefix); err != nil {
		return nil, err
	}
	if err := binary.Write(key, binary.BigEndian, pubKey); err != nil {
		return nil, err
	}
	return key.Bytes(), nil
}

func decodePrivateKeyDbKey(key []byte) (ed25519.PublicKey, error) {
	buf := bytes.NewBuffer(key)
	if _, err := buf.ReadByte(); err != nil {
		return nil, err
	}
	var pubKey [ed25519.PublicKeySize]byte
	if err := binary.Read(buf, binary.BigEndian, pubKey[:32]); err != nil {
		return nil, err
	}
	return ed25519.PublicKey(pubKey[:]), nil
}

// encryption utility functions

// NaCl secretbox encrypt a private key with an Argon2id key derived from passphrase
func encryptPrivateKey(privKey ed25519.PrivateKey, passphrase string) []byte {
	salt := generateSalt()
	key := stretchPassphrase(passphrase, salt)

	var secretKey [32]byte
	copy(secretKey[:], key)

	var nonce [24]byte
	if _, err := io.ReadFull(rand.Reader, nonce[:]); err != nil {
		panic(err)
	}

	encrypted := secretbox.Seal(nonce[:], privKey[:], &nonce, &secretKey)

	// prepend the salt
	encryptedPrivKey := make([]byte, len(encrypted)+ArgonSaltLength)
	copy(encryptedPrivKey[:], salt)
	copy(encryptedPrivKey[ArgonSaltLength:], encrypted)

	return encryptedPrivKey
}

// NaCl secretbox decrypt a private key with an Argon2id key derived from passphrase
func decryptPrivateKey(encryptedPrivKey []byte, passphrase string) (ed25519.PrivateKey, bool) {
	salt := encryptedPrivKey[:ArgonSaltLength]
	key := []byte(stretchPassphrase(passphrase, salt))

	var secretKey [32]byte
	copy(secretKey[:], key)

	var nonce [24]byte
	copy(nonce[:], encryptedPrivKey[ArgonSaltLength:ArgonSaltLength+24])

	decryptedPrivKey, ok := secretbox.Open(nil, encryptedPrivKey[ArgonSaltLength+24:], &nonce, &secretKey)
	if !ok {
		return ed25519.PrivateKey{}, false
	}
	return ed25519.PrivateKey(decryptedPrivKey[:]), true
}

const ArgonSaltLength = 16

const ArgonTime = 1

const ArgonMemory = 64 * 1024

const ArgonThreads = 4

const ArgonKeyLength = 32

// Generate a suitable salt for use with Argon2id
func generateSalt() []byte {
	salt := make([]byte, ArgonSaltLength)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		panic(err.Error())
	}
	return salt
}

// Strecth passphrase into a 32 byte key with Argon2id
func stretchPassphrase(passphrase string, salt []byte) []byte {
	return argon2.IDKey([]byte(passphrase), salt, ArgonTime, ArgonMemory, ArgonThreads, ArgonKeyLength)
}
