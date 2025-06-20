package focalpoint

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	cuckoo "github.com/seiflotfy/cuckoofilter"
	"golang.org/x/crypto/ed25519"
)

// Peer is a peer client in the network. They all speak WebSocket protocol to each other.
// Peers could be fully validating and rendering nodes or simply minds.
type Peer struct {
	conn                          *websocket.Conn
	genesisID                     ViewID
	peerStore                     PeerStorage
	viewStore                     ViewStorage
	ledger                        Ledger
	processor                     *Processor
	indexer                       *Indexer
	cnQueue                       ConsiderationQueue
	outbound                      bool
	localDownloadQueue            *ViewQueue // peer-local download queue
	localInflightQueue            *ViewQueue // peer-local inflight queue
	globalInflightQueue           *ViewQueue // global inflight queue
	ignoreViewes                  map[ViewID]bool
	continuationViewID            ViewID
	lastPeerAddressesReceivedTime time.Time
	filterLock                    sync.RWMutex
	filter                        *cuckoo.Filter
	addrChan                      chan<- string
	workID                        int32
	workView                      *View
	medianTimestamp               int64
	pubKeys                       []ed25519.PublicKey
	memo                          string
	readLimitLock                 sync.RWMutex
	readLimit                     int64
	closeHandler                  func()
	wg                            sync.WaitGroup
}

// PeerUpgrader upgrades the incoming HTTP connection to a WebSocket if the subprotocol matches.
var PeerUpgrader = websocket.Upgrader{
	Subprotocols: []string{Protocol},
	CheckOrigin:  func(r *http.Request) bool { return true },
}

// NewPeer returns a new instance of a peer.
func NewPeer(conn *websocket.Conn, genesisID ViewID, peerStore PeerStorage,
	viewStore ViewStorage, ledger Ledger, processor *Processor, indexer *Indexer,
	cnQueue ConsiderationQueue, viewQueue *ViewQueue, addrChan chan<- string) *Peer {
	peer := &Peer{
		conn:                conn,
		genesisID:           genesisID,
		peerStore:           peerStore,
		viewStore:           viewStore,
		ledger:              ledger,
		processor:           processor,
		indexer:             indexer,
		cnQueue:             cnQueue,
		localDownloadQueue:  NewViewQueue(),
		localInflightQueue:  NewViewQueue(),
		globalInflightQueue: viewQueue,
		ignoreViewes:        make(map[ViewID]bool),
		addrChan:            addrChan,
	}
	peer.updateReadLimit()
	return peer
}

// peerDialer is the websocket.Dialer to use for outbound peer connections
var peerDialer *websocket.Dialer = &websocket.Dialer{
	Proxy:            http.ProxyFromEnvironment,
	HandshakeTimeout: 15 * time.Second,
	Subprotocols:     []string{Protocol}, // set in protocol.go
	TLSClientConfig:  tlsClientConfig,    // set in tls.go
}

// Connect connects outbound to a peer.
func (p *Peer) Connect(ctx context.Context, addr, nonce, myAddr string) (int, error) {
	u := url.URL{Scheme: "wss", Host: addr, Path: "/" + p.genesisID.String()}
	log.Printf("Connecting to %s", u.String())

	if err := p.peerStore.OnConnectAttempt(addr); err != nil {
		return 0, err
	}

	header := http.Header{}
	header.Add("Viewpoint-Peer-Nonce", nonce)
	if len(myAddr) != 0 {
		header.Add("Viewpoint-Peer-Address", myAddr)
	}

	// specify timeout via context. if the parent context is cancelled
	// we'll also abort the connection.
	dialCcn, cancel := context.WithTimeout(ctx, connectWait)
	defer cancel()

	var statusCode int
	conn, resp, err := peerDialer.DialContext(dialCcn, u.String(), header)
	if resp != nil {
		statusCode = resp.StatusCode
	}
	if err != nil {
		if statusCode == http.StatusTooManyRequests {
			// the peer is already connected to us inbound.
			// mark it successful so we try it again in the future.
			p.peerStore.OnConnectSuccess(addr)
			p.peerStore.OnDisconnect(addr)
		} else {
			p.peerStore.OnConnectFailure(addr)
		}
		return statusCode, err
	}

	p.conn = conn
	p.outbound = true
	return statusCode, p.peerStore.OnConnectSuccess(addr)
}

// OnClose specifies a handler to call when the peer connection is closed.
func (p *Peer) OnClose(closeHandler func()) {
	p.closeHandler = closeHandler
}

// Shutdown is called to shutdown the underlying WebSocket synchronously.
func (p *Peer) Shutdown() {
	var addr string
	if p.conn != nil {
		addr = p.conn.RemoteAddr().String()
		p.conn.Close()
	}
	p.wg.Wait()
	if len(addr) != 0 {
		log.Printf("Closed connection with %s\n", addr)
	}
}

const (
	// Time allowed to wait for WebSocket connection
	connectWait = 10 * time.Second

	// Time allowed to write a message to the peer
	writeWait = 30 * time.Second

	// Time allowed to read the next pong message from the peer
	pongWait = 120 * time.Second

	// Send pings to peer with this period. Must be less than pongWait
	pingPeriod = pongWait / 2

	// How often should we refresh this peer's connectivity status with storage
	peerStoreRefreshPeriod = 5 * time.Minute

	// How often should we request peer addresses from a peer
	getPeerAddressesPeriod = 1 * time.Hour

	// Time allowed between processing new views before we consider a focalpoint sync stalled
	syncWait = 2 * time.Minute

	// Maximum views per inv_view message
	maxViewesPerInv = 500

	// Maximum local inflight queue size
	inflightQueueMax = 8

	// Maximum local download queue size
	downloadQueueMax = maxViewesPerInv * 10
)

// Run executes the peer's main loop in its own goroutine.
// It manages reading and writing to the peer's WebSocket and facilitating the protocol.
func (p *Peer) Run() {
	p.wg.Add(1)
	go p.run()
}

func (p *Peer) run() {
	defer p.wg.Done()
	if p.closeHandler != nil {
		defer p.closeHandler()
	}

	peerAddr := p.conn.RemoteAddr().String()
	defer func() {
		// remove any inflight views this peer is no longer going to download
		viewInflight, ok := p.localInflightQueue.Peek()
		for ok {
			p.localInflightQueue.Remove(viewInflight, "")
			p.globalInflightQueue.Remove(viewInflight, peerAddr)
			viewInflight, ok = p.localInflightQueue.Peek()
		}
	}()

	defer p.conn.Close()

	// written to by the reader loop to send outgoing messages to the writer loop
	outChan := make(chan Message, 1)

	// signals that the reader loop is exiting
	defer close(outChan)

	// send a find common ancestor request and request peer addresses shortly after connecting
	onConnectChan := make(chan bool, 1)
	go func() {
		time.Sleep(5 * time.Second)
		onConnectChan <- true
	}()

	// written to by the reader loop to update the current work view for the peer
	getWorkChan := make(chan GetWorkMessage, 1)
	submitWorkChan := make(chan SubmitWorkMessage, 1)

	// writer goroutine loop
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()

		// register to hear about tip view changes
		tipChangeChan := make(chan TipChange, 10)
		p.processor.RegisterForTipChange(tipChangeChan)
		defer p.processor.UnregisterForTipChange(tipChangeChan)

		// register to hear about new considerations
		newTxChan := make(chan NewTx, MAX_CONSIDERATIONS_TO_INCLUDE_PER_VIEW)
		p.processor.RegisterForNewConsiderations(newTxChan)
		defer p.processor.UnregisterForNewConsiderations(newTxChan)

		// send the peer pings
		tickerPing := time.NewTicker(pingPeriod)
		defer tickerPing.Stop()

		// update the peer store with the peer's connectivity
		tickerPeerStoreRefresh := time.NewTicker(peerStoreRefreshPeriod)
		defer tickerPeerStoreRefresh.Stop()

		// request new peer addresses
		tickerGetPeerAddresses := time.NewTicker(getPeerAddressesPeriod)
		defer tickerGetPeerAddresses.Stop()

		// check to see if we need to update work for renderers
		tickerUpdateWorkCheck := time.NewTicker(30 * time.Second)
		defer tickerUpdateWorkCheck.Stop()

		// update the peer store on disconnection
		if p.outbound {
			defer p.peerStore.OnDisconnect(peerAddr)
		}

		for {
			select {
			case m, ok := <-outChan:
				if !ok {
					// reader loop is exiting
					return
				}

				// send outgoing message to peer
				p.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := p.conn.WriteJSON(m); err != nil {
					log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

			case tip := <-tipChangeChan:
				// update read limit if necessary
				p.updateReadLimit()

				if tip.Connect && tip.More == false {
					// only build off newly connected tip views.
					// create and send out new work if necessary
					p.createNewWorkView(tip.ViewID, tip.View.Header)
				}

				if tip.Source == p.conn.RemoteAddr().String() {
					// this is who sent us the view that caused the change
					break
				}

				if tip.Connect {
					// new tip announced, notify the peer
					inv := Message{
						Type: "inv_view",
						Body: InvViewMessage{
							ViewIDs: []ViewID{tip.ViewID},
						},
					}
					// send it
					p.conn.SetWriteDeadline(time.Now().Add(writeWait))
					if err := p.conn.WriteJSON(inv); err != nil {
						log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
						p.conn.Close()
					}
				}

				// potentially create a filter_view
				fb, err := p.createFilterView(tip.ViewID, tip.View)
				if err != nil {
					log.Printf("Error: %s, to: %s\n", err, p.conn.RemoteAddr())
					continue
				}
				if fb == nil {
					continue
				}

				// send it
				m := Message{
					Type: "filter_view",
					Body: fb,
				}
				if !tip.Connect {
					m.Type = "filter_view_undo"
				}

				log.Printf("Sending %s with %d consideration(s), to: %s\n",
					m.Type, len(fb.Considerations), p.conn.RemoteAddr())
				p.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := p.conn.WriteJSON(m); err != nil {
					log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

			case newTx := <-newTxChan:
				if newTx.Source == p.conn.RemoteAddr().String() {
					// this is who sent it to us
					break
				}

				interested := func() bool {
					p.filterLock.RLock()
					defer p.filterLock.RUnlock()
					return p.filterLookup(newTx.Consideration)
				}()
				if !interested {
					continue
				}

				// newly verified consideration announced, relay to peer
				pushTx := Message{
					Type: "push_consideration",
					Body: PushConsiderationMessage{
						Consideration: newTx.Consideration,
					},
				}
				p.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := p.conn.WriteJSON(pushTx); err != nil {
					log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

			case <-onConnectChan:
				// send a new peer a request to find a common ancestor
				if err := p.sendFindCommonAncestor(nil, true, outChan); err != nil {
					log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

				// send a get_peer_addresses to request peers
				log.Printf("Sending get_peer_addresses to: %s\n", p.conn.RemoteAddr())
				m := Message{Type: "get_peer_addresses"}
				p.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := p.conn.WriteJSON(m); err != nil {
					log.Printf("Error sending get_peer_addresses: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

			case gw := <-getWorkChan:
				p.onGetWork(gw)

			case sw := <-submitWorkChan:
				p.onSubmitWork(sw)

			case <-tickerPing.C:
				//log.Printf("Sending ping message to: %s\n", p.conn.RemoteAddr())
				p.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := p.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
					log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

			case <-tickerPeerStoreRefresh.C:
				if p.outbound == false {
					break
				}
				// periodically refresh our connection time
				if err := p.peerStore.OnConnectSuccess(p.conn.RemoteAddr().String()); err != nil {
					log.Printf("Error from peer store: %s\n", err)
				}

			case <-tickerGetPeerAddresses.C:
				// periodically send a get_peer_addresses
				log.Printf("Sending get_peer_addresses to: %s\n", p.conn.RemoteAddr())
				m := Message{Type: "get_peer_addresses"}
				p.conn.SetWriteDeadline(time.Now().Add(writeWait))
				if err := p.conn.WriteJSON(m); err != nil {
					log.Printf("Error sending get_peer_addresses: %s, to: %s\n", err, p.conn.RemoteAddr())
					p.conn.Close()
				}

			case <-tickerUpdateWorkCheck.C:
				if p.workView == nil {
					// peer doesn't have work
					break
				}
				cnCount := len(p.workView.Considerations)
				if cnCount == MAX_CONSIDERATIONS_TO_INCLUDE_PER_VIEW {
					// already at capacity
					break
				}
				if cnCount-1 != p.cnQueue.Len() {
					tipID, tipHeader, _, err := getPointTipHeader(p.ledger, p.viewStore)
					if err != nil {
						log.Printf("Error reading point tip header: %s\n", err)
						break
					}
					p.createNewWorkView(*tipID, tipHeader)
				}
			}
		}
	}()

	// are we syncing?
	lastNewViewTime := time.Now()
	ibd, _, err := IsInitialViewDownload(p.ledger, p.viewStore)
	if err != nil {
		log.Println(err)
		return
	}

	// handle pongs
	p.conn.SetPongHandler(func(string) error {
		if ibd {
			// handle stalled focalpoint syncs
			var err error
			ibd, _, err = IsInitialViewDownload(p.ledger, p.viewStore)
			if err != nil {
				return err
			}
			if ibd && time.Since(lastNewViewTime) > syncWait {
				return fmt.Errorf("Sync has stalled, disconnecting")
			}
		} else {
			// try processing the queue in case we've been passed by another client
			// and their attempt has now expired
			if err := p.processDownloadQueue(outChan); err != nil {
				return err
			}
		}
		p.conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// set initial read deadline
	p.conn.SetReadDeadline(time.Now().Add(pongWait))

	// reader loop
	for {
		// update read limit
		p.conn.SetReadLimit(p.getReadLimit())

		// new message from peer
		messageType, message, err := p.conn.ReadMessage()
		if err != nil {
			log.Printf("Read error: %s, from: %s\n", err, p.conn.RemoteAddr())
			break
		}

		switch messageType {
		case websocket.TextMessage:
			// sanitize inputs
			if !utf8.Valid(message) {
				log.Printf("Peer sent us non-utf8 clean message, from: %s\n", p.conn.RemoteAddr())
				return
			}

			var body json.RawMessage
			m := Message{Body: &body}
			if err := json.Unmarshal([]byte(message), &m); err != nil {
				log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
				return
			}

			// hangup if the peer is sending oversized messages
			if m.Type != "view" && len(message) > MAX_PROTOCOL_MESSAGE_LENGTH {
				log.Printf("Received too large (%d bytes) of a '%s' message, from: %s",
					len(message), m.Type, p.conn.RemoteAddr())
				return
			}

			switch m.Type {
			case "inv_view":
				var inv InvViewMessage
				if err := json.Unmarshal(body, &inv); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				for i, id := range inv.ViewIDs {
					if err := p.onInvView(id, i, len(inv.ViewIDs), outChan); err != nil {
						log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
						break
					}
				}

			case "get_view":
				var gb GetViewMessage
				if err := json.Unmarshal(body, &gb); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetView(gb.ViewID, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_view_by_height":
				var gbbh GetViewByHeightMessage
				if err := json.Unmarshal(body, &gbbh); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetViewByHeight(gbbh.Height, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "view":
				var b ViewMessage
				if err := json.Unmarshal(body, &b); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if b.View == nil {
					log.Printf("Error: received nil view, from: %s\n", p.conn.RemoteAddr())
					return
				}
				if b.View.Header == nil {
					log.Printf("Error: received nil view header, from: %s\n", p.conn.RemoteAddr())
					return
				}
				ok, err := p.onView(b.View, ibd, outChan)
				if err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}
				if ok {
					lastNewViewTime = time.Now()
				}

			case "find_common_ancestor":
				var fca FindCommonAncestorMessage
				if err := json.Unmarshal(body, &fca); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				num := len(fca.ViewIDs)
				for i, id := range fca.ViewIDs {
					ok, err := p.onFindCommonAncestor(id, i, num, outChan)
					if err != nil {
						log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
						break
					}
					if ok {
						// don't need to process more
						break
					}
				}

			case "get_view_header":
				var gbh GetViewHeaderMessage
				if err := json.Unmarshal(body, &gbh); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetViewHeader(gbh.ViewID, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_view_header_by_height":
				var gbhbh GetViewHeaderByHeightMessage
				if err := json.Unmarshal(body, &gbhbh); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetViewHeaderByHeight(gbhbh.Height, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_profile":
				var gp GetProfileMessage
				if err := json.Unmarshal(body, &gp); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetProfile(gp.PublicKey, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_graph":
				var gn GetGraphMessage
				if err := json.Unmarshal(body, &gn); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetGraph(gn.PublicKey, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_ranking":
				var gr GetRankingMessage
				if err := json.Unmarshal(body, &gr); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetRanking(gr.PublicKey, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_imbalance":
				var gb GetImbalanceMessage
				if err := json.Unmarshal(body, &gb); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetImbalance(gb.PublicKey, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_imbalances":
				var gb GetImbalancesMessage
				if err := json.Unmarshal(body, &gb); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetImbalances(gb.PublicKeys, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_public_key_considerations":
				var gpkt GetPublicKeyConsiderationsMessage
				if err := json.Unmarshal(body, &gpkt); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetPublicKeyConsiderations(gpkt.PublicKey,
					gpkt.StartHeight, gpkt.EndHeight, gpkt.StartIndex, gpkt.Limit, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_consideration":
				var gt GetConsiderationMessage
				if err := json.Unmarshal(body, &gt); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onGetConsideration(gt.ConsiderationID, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_tip_header":
				if err := p.onGetTipHeader(outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "push_consideration":
				var pt PushConsiderationMessage
				if err := json.Unmarshal(body, &pt); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if pt.Consideration == nil {
					log.Printf("Error: received nil consideration, from: %s\n", p.conn.RemoteAddr())
					return
				}
				if err := p.onPushConsideration(pt.Consideration, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "push_consideration_result":
				var ptr PushConsiderationResultMessage
				if err := json.Unmarshal(body, &ptr); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if len(ptr.Error) != 0 {
					log.Printf("Error: %s, from: %s\n", ptr.Error, p.conn.RemoteAddr())
				}

			case "filter_load":
				var fl FilterLoadMessage
				if err := json.Unmarshal(body, &fl); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onFilterLoad(fl.Type, fl.Filter, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "filter_add":
				var fa FilterAddMessage
				if err := json.Unmarshal(body, &fa); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if err := p.onFilterAdd(fa.PublicKeys, outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "get_filter_consideration_queue":
				p.onGetFilterConsiderationQueue(outChan)

			case "get_peer_addresses":
				if err := p.onGetPeerAddresses(outChan); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					break
				}

			case "peer_addresses":
				var pa PeerAddressesMessage
				if err := json.Unmarshal(body, &pa); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				p.onPeerAddresses(pa.Addresses)

			case "get_work":
				var gw GetWorkMessage
				if err := json.Unmarshal(body, &gw); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				log.Printf("Received get_work message, from: %s\n", p.conn.RemoteAddr())
				getWorkChan <- gw

			case "submit_work":
				var sw SubmitWorkMessage
				if err := json.Unmarshal(body, &sw); err != nil {
					log.Printf("Error: %s, from: %s\n", err, p.conn.RemoteAddr())
					return
				}
				if sw.Header == nil {
					log.Printf("Error: received nil header, from: %s\n", p.conn.RemoteAddr())
					return
				}
				log.Printf("Received submit_work message, from: %s\n", p.conn.RemoteAddr())
				submitWorkChan <- sw

			default:
				log.Printf("Unknown message: %s, from: %s\n", m.Type, p.conn.RemoteAddr())
			}

		case websocket.CloseMessage:
			log.Printf("Received close message from: %s\n", p.conn.RemoteAddr())
			break
		}
	}
}

// Handle a message from a peer indicating view inventory available for download
func (p *Peer) onInvView(id ViewID, index, length int, outChan chan<- Message) error {
	log.Printf("Received inv_view: %s, from: %s\n", id, p.conn.RemoteAddr())

	if length > maxViewesPerInv {
		return fmt.Errorf("%d views IDs is more than %d maximum per inv_view",
			length, maxViewesPerInv)
	}

	// is it on the ignore list?
	if p.ignoreViewes[id] {
		log.Printf("Ignoring view %s, from: %s\n", id, p.conn.RemoteAddr())
		return nil
	}

	// do we have it queued or inflight already?
	if p.localDownloadQueue.Exists(id) || p.localInflightQueue.Exists(id) {
		log.Printf("View %s is already queued or inflight for download, from: %s\n",
			id, p.conn.RemoteAddr())
		return nil
	}

	// have we processed it?
	branchType, err := p.ledger.GetBranchType(id)
	if err != nil {
		return err
	}
	if branchType != UNKNOWN {
		log.Printf("Already processed view %s", id)
		if length > 1 && index+1 == length {
			// we might be on a deep side point. this will get us the next 500 views
			return p.sendFindCommonAncestor(&id, false, outChan)
		}
		return nil
	}

	if p.localDownloadQueue.Len() >= downloadQueueMax {
		log.Printf("Too many views in the download queue %d, max: %d, for: %s",
			p.localDownloadQueue.Len(), downloadQueueMax, p.conn.RemoteAddr())
		// don't return an error just stop adding them to the queue
		return nil
	}

	// add view to this peer's download queue
	p.localDownloadQueue.Add(id, "")

	// process the download queue
	return p.processDownloadQueue(outChan)
}

// Handle a request for a view from a peer
func (p *Peer) onGetView(id ViewID, outChan chan<- Message) error {
	log.Printf("Received get_view: %s, from: %s\n", id, p.conn.RemoteAddr())
	return p.getView(id, outChan)
}

// Handle a request for a view by height from a peer
func (p *Peer) onGetViewByHeight(height int64, outChan chan<- Message) error {
	log.Printf("Received get_view_by_height: %d, from: %s\n", height, p.conn.RemoteAddr())
	id, err := p.ledger.GetViewIDForHeight(height)
	if err != nil {
		// not found
		outChan <- Message{Type: "view"}
		return err
	}
	if id == nil {
		// not found
		outChan <- Message{Type: "view"}
		return fmt.Errorf("No view found at height %d", height)
	}
	return p.getView(*id, outChan)
}

func (p *Peer) getView(id ViewID, outChan chan<- Message) error {
	// fetch the view
	viewJson, err := p.viewStore.GetViewBytes(id)
	if err != nil {
		// not found
		outChan <- Message{Type: "view", Body: ViewMessage{ViewID: &id}}
		return err
	}
	if len(viewJson) == 0 {
		// not found
		outChan <- Message{Type: "view", Body: ViewMessage{ViewID: &id}}
		return fmt.Errorf("No view found with ID %s", id)
	}

	// send out the raw bytes
	body := []byte(`{"view_id":"`)
	body = append(body, []byte(id.String())...)
	body = append(body, []byte(`","view":`)...)
	body = append(body, viewJson...)
	body = append(body, []byte(`}`)...)
	outChan <- Message{Type: "view", Body: json.RawMessage(body)}

	// was this the last view in the inv we sent in response to a find common ancestor request?
	if id == p.continuationViewID {
		log.Printf("Received get_view for continuation view %s, from: %s\n",
			id, p.conn.RemoteAddr())
		p.continuationViewID = ViewID{}
		// send an inv for our tip view to prompt the peer to
		// send another find common ancestor request to complete its download of the point.
		tipID, _, err := p.ledger.GetPointTip()
		if err != nil {
			log.Printf("Error: %s\n", err)
			return nil
		}
		if tipID != nil {
			outChan <- Message{Type: "inv_view", Body: InvViewMessage{ViewIDs: []ViewID{*tipID}}}
		}
	}
	return nil
}

// Handle receiving a view from a peer. Returns true if the view was newly processed and accepted.
func (p *Peer) onView(view *View, ibd bool, outChan chan<- Message) (bool, error) {
	// the message has the ID in it but we can't trust that.
	// it's provided as convenience for trusted peering relationships only
	id, err := view.ID()
	if err != nil {
		return false, err
	}

	log.Printf("Received view: %s, from: %s\n", id, p.conn.RemoteAddr())

	viewInFlight, ok := p.localInflightQueue.Peek()
	if !ok || viewInFlight != id {
		// disconnect misbehaving peer
		p.conn.Close()
		return false, fmt.Errorf("Received unrequested view")
	}

	// don't process low difficulty views
	if ibd == false && CheckpointsEnabled && view.Header.Height < LatestCheckpointHeight {
		// don't disconnect them. they may need us to find out about the real point
		p.localInflightQueue.Remove(id, "")
		p.globalInflightQueue.Remove(id, p.conn.RemoteAddr().String())
		// ignore future inv_views for this view
		p.ignoreViewes[id] = true
		if len(p.ignoreViewes) > maxViewesPerInv {
			// they're intentionally sending us bad views
			log.Printf("Disconnecting %s, max ignore list size exceeded\n", p.conn.RemoteAddr().String())
			p.conn.Close()
		}
		return false, fmt.Errorf("View %s height %d less than latest checkpoint height %d",
			id, view.Header.Height, LatestCheckpointHeight)
	}

	var accepted bool

	// is it an orphan?
	header, _, err := p.viewStore.GetViewHeader(view.Header.Previous)
	if err != nil || header == nil {
		p.localInflightQueue.Remove(id, "")
		p.globalInflightQueue.Remove(id, p.conn.RemoteAddr().String())

		if err != nil {
			return false, err
		}

		log.Printf("View %s is an orphan, sending find_common_ancestor to: %s\n",
			id, p.conn.RemoteAddr())

		// send a find common ancestor request
		if err := p.sendFindCommonAncestor(nil, false, outChan); err != nil {
			return false, err
		}
	} else {
		// process the view
		if err := p.processor.ProcessView(id, view, p.conn.RemoteAddr().String()); err != nil {
			// disconnect a peer that sends us a bad view
			p.conn.Close()
			return false, err
		}
		// newly accepted view
		accepted = true

		// remove it from the inflight queues only after we process it
		p.localInflightQueue.Remove(id, "")
		p.globalInflightQueue.Remove(id, p.conn.RemoteAddr().String())
	}

	// see if there are any more views to download right now
	if err := p.processDownloadQueue(outChan); err != nil {
		return false, err
	}

	return accepted, nil
}

// Try requesting views that are in the download queue
func (p *Peer) processDownloadQueue(outChan chan<- Message) error {
	// fill up as much of the inflight queue as possible
	var queued int
	for p.localInflightQueue.Len() < inflightQueueMax {
		// next view to download
		viewToDownload, ok := p.localDownloadQueue.Peek()
		if !ok {
			// no more views in the queue
			break
		}

		// double-check if it's been processed since we last checked
		branchType, err := p.ledger.GetBranchType(viewToDownload)
		if err != nil {
			return err
		}
		if branchType != UNKNOWN {
			// it's been processed. remove it and check the next one
			log.Printf("View %s has been processed, removing from download queue for: %s\n",
				viewToDownload, p.conn.RemoteAddr().String())
			p.localDownloadQueue.Remove(viewToDownload, "")
			continue
		}

		// add view to the global inflight queue with this peer as the owner
		if p.globalInflightQueue.Add(viewToDownload, p.conn.RemoteAddr().String()) == false {
			// another peer is downloading it right now.
			// wait to see if they succeed before trying to download any others
			log.Printf("View %s is being downloaded already from another peer\n", viewToDownload)
			break
		}

		// pop it off the local download queue
		p.localDownloadQueue.Remove(viewToDownload, "")

		// mark it inflight locally
		p.localInflightQueue.Add(viewToDownload, "")
		queued++

		// request it
		log.Printf("Sending get_view for %s, to: %s\n", viewToDownload, p.conn.RemoteAddr())
		outChan <- Message{Type: "get_view", Body: GetViewMessage{ViewID: viewToDownload}}
	}

	if queued > 0 {
		log.Printf("Requested %d view(s) for download, from: %s", queued, p.conn.RemoteAddr())
		log.Printf("Queue size: %d, peer inflight: %d, global inflight: %d, for: %s\n",
			p.localDownloadQueue.Len(), p.localInflightQueue.Len(), p.globalInflightQueue.Len(), p.conn.RemoteAddr())
	}

	return nil
}

// Send a message to look for a common ancestor with a peer
// Might be called from reader or writer context. writeNow means we're in the writer context
func (p *Peer) sendFindCommonAncestor(startID *ViewID, writeNow bool, outChan chan<- Message) error {
	log.Printf("Sending find_common_ancestor to: %s\n", p.conn.RemoteAddr())

	var height int64
	if startID == nil {
		var err error
		startID, height, err = p.ledger.GetPointTip()
		if err != nil {
			return err
		}
	} else {
		header, _, err := p.viewStore.GetViewHeader(*startID)
		if err != nil {
			return err
		}
		if header == nil {
			return fmt.Errorf("No header for view %s", *startID)
		}
		height = header.Height
	}
	id := startID

	var ids []ViewID
	var step int64 = 1
	for id != nil {
		if *id == p.genesisID {
			break
		}
		ids = append(ids, *id)
		depth := height - step
		if depth <= 0 {
			break
		}
		var err error
		id, err = p.ledger.GetViewIDForHeight(depth)
		if err != nil {
			log.Printf("Error: %s\n", err)
			return nil
		}
		if len(ids) > 10 {
			step *= 2
		}
		height = depth
	}
	ids = append(ids, p.genesisID)
	m := Message{Type: "find_common_ancestor", Body: FindCommonAncestorMessage{ViewIDs: ids}}

	if writeNow {
		p.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := p.conn.WriteJSON(m); err != nil {
			log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
			return err
		}
		return nil
	}
	outChan <- m
	return nil
}

// Handle a find common ancestor message from a peer
func (p *Peer) onFindCommonAncestor(id ViewID, index, length int, outChan chan<- Message) (bool, error) {
	log.Printf("Received find_common_ancestor: %s, index: %d, length: %d, from: %s\n",
		id, index, length, p.conn.RemoteAddr())

	header, _, err := p.viewStore.GetViewHeader(id)
	if err != nil {
		return false, err
	}
	if header == nil {
		// don't have it
		return false, nil
	}
	branchType, err := p.ledger.GetBranchType(id)
	if err != nil {
		return false, err
	}
	if branchType != MAIN {
		// not on the main branch
		return false, nil
	}

	log.Printf("Common ancestor found: %s, height: %d, with: %s\n",
		id, header.Height, p.conn.RemoteAddr())

	var ids []ViewID
	var height int64 = header.Height + 1
	for len(ids) < maxViewesPerInv {
		nextID, err := p.ledger.GetViewIDForHeight(height)
		if err != nil {
			return false, err
		}
		if nextID == nil {
			break
		}
		log.Printf("Queueing inv for view %s, height: %d, to: %s\n",
			nextID, height, p.conn.RemoteAddr())
		ids = append(ids, *nextID)
		height += 1
	}

	if len(ids) > 0 {
		// save the last ID so after the peer requests it we can trigger it to
		// send another find common ancestor request to finish downloading the rest of the point
		p.continuationViewID = ids[len(ids)-1]
		log.Printf("Sending inv_view with %d IDs, continuation view: %s, to: %s",
			len(ids), p.continuationViewID, p.conn.RemoteAddr())
		outChan <- Message{Type: "inv_view", Body: InvViewMessage{ViewIDs: ids}}
	}
	return true, nil
}

// Handle a request for a view header from a peer
func (p *Peer) onGetViewHeader(id ViewID, outChan chan<- Message) error {
	log.Printf("Received get_view_header: %s, from: %s\n", id, p.conn.RemoteAddr())
	return p.getViewHeader(id, outChan)
}

// Handle a request for a view header by ID from a peer
func (p *Peer) onGetViewHeaderByHeight(height int64, outChan chan<- Message) error {
	log.Printf("Received get_view_header_by_height: %d, from: %s\n", height, p.conn.RemoteAddr())
	id, err := p.ledger.GetViewIDForHeight(height)
	if err != nil {
		// not found
		outChan <- Message{Type: "view_header"}
		return err
	}
	if id == nil {
		// not found
		outChan <- Message{Type: "view_header"}
		return fmt.Errorf("No view found at height %d", height)
	}
	return p.getViewHeader(*id, outChan)
}

func (p *Peer) getViewHeader(id ViewID, outChan chan<- Message) error {
	header, _, err := p.viewStore.GetViewHeader(id)
	if err != nil {
		// not found
		outChan <- Message{Type: "view_header", Body: ViewHeaderMessage{ViewID: &id}}
		return err
	}
	if header == nil {
		// not found
		outChan <- Message{Type: "view_header", Body: ViewHeaderMessage{ViewID: &id}}
		return fmt.Errorf("View header for %s not found", id)
	}
	outChan <- Message{Type: "view_header", Body: ViewHeaderMessage{ViewID: &id, ViewHeader: header}}
	return nil
}

// Handle a request for a public key's profile
func (p *Peer) onGetProfile(pubKey ed25519.PublicKey, outChan chan<- Message) error {
	log.Printf("Received get_profile from: %s\n", p.conn.RemoteAddr())

	imbalances, _, _, err := p.ledger.GetPublicKeyImbalances([]ed25519.PublicKey{pubKey})
	if err != nil {
		outChan <- Message{Type: "imbalance", Body: ProfileMessage{PublicKey: pubKey, Error: err.Error()}}
		return err
	}

	var imbalance int64
	for _, b := range imbalances {
		imbalance = b
	}

	pk := pubKeyToString(pubKey)

	_, locale, _ := localeFromPubKey(pk)

	graph := p.indexer.cnGraph

	pkIndex, ok := graph.index[pk]

	if ok {
		node := graph.nodes[pkIndex]

		outChan <- Message{
			Type: "profile",
			Body: ProfileMessage{
				PublicKey: pubKey,
				Ranking:   node.ranking,
				Imbalance: imbalance,
				Locale:    locale,
				ViewID:    p.indexer.latestViewID,
				Height:    p.indexer.latestHeight,
			},
		}

	} else {
		outChan <- Message{
			Type: "profile",
			Body: ProfileMessage{
				PublicKey: pubKey,
				Ranking:   0.00,
				Imbalance: 0,
				Locale:    "",
				ViewID:    p.indexer.latestViewID,
				Height:    p.indexer.latestHeight,
			},
		}
	}

	return nil
}

// Handle a request for a public key's view graph
func (p *Peer) onGetGraph(pubKey ed25519.PublicKey, outChan chan<- Message) error {
	log.Printf("Received get_graph from: %s\n", p.conn.RemoteAddr())

	pk := pubKeyToString(pubKey)
	viewGraph := p.indexer.cnGraph.ToDOT(pk, p.indexer.synonyms)

	outChan <- Message{
		Type: "graph",
		Body: GraphMessage{
			ViewID:    p.indexer.latestViewID,
			Height:    p.indexer.latestHeight,
			PublicKey: pubKey,
			Graph:     viewGraph,
		},
	}

	return nil
}

// Handle a request for a public key's considerability ranking
func (p *Peer) onGetRanking(pubKey ed25519.PublicKey, outChan chan<- Message) error {
	log.Printf("Received get_ranking from: %s\n", p.conn.RemoteAddr())

	pk := pubKeyToString(pubKey)

	graph := p.indexer.cnGraph

	pkIndex, ok := graph.index[pk]

	if ok {
		node := graph.nodes[pkIndex]

		outChan <- Message{
			Type: "ranking",
			Body: RankingMessage{
				ViewID:    p.indexer.latestViewID,
				Height:    p.indexer.latestHeight,
				PublicKey: pubKey,
				Ranking:   node.ranking,
			},
		}
	} else {
		outChan <- Message{
			Type: "ranking",
			Body: RankingMessage{
				ViewID:    p.indexer.latestViewID,
				Height:    p.indexer.latestHeight,
				PublicKey: pubKey,
				Ranking:   0.00,
			},
		}
	}

	return nil
}

// Handle a request for a public key's imbalance
func (p *Peer) onGetImbalance(pubKey ed25519.PublicKey, outChan chan<- Message) error {
	log.Printf("Received get_imbalance from: %s\n", p.conn.RemoteAddr())

	imbalances, tipID, tipHeight, err := p.ledger.GetPublicKeyImbalances([]ed25519.PublicKey{pubKey})
	if err != nil {
		outChan <- Message{Type: "imbalance", Body: ImbalanceMessage{PublicKey: pubKey, Error: err.Error()}}
		return err
	}

	var imbalance int64
	for _, b := range imbalances {
		imbalance = b
	}

	outChan <- Message{
		Type: "imbalance",
		Body: ImbalanceMessage{
			ViewID:    tipID,
			Height:    tipHeight,
			PublicKey: pubKey,
			Imbalance: imbalance,
		},
	}
	return nil
}

// Handle a request for a set of public key imbalances.
func (p *Peer) onGetImbalances(pubKeys []ed25519.PublicKey, outChan chan<- Message) error {
	log.Printf("Received get_imbalances (count: %d) from: %s\n", len(pubKeys), p.conn.RemoteAddr())

	maxPublicKeys := 64
	if len(pubKeys) > maxPublicKeys {
		err := fmt.Errorf("Too many public keys, limit: %d", maxPublicKeys)
		outChan <- Message{Type: "imbalances", Body: ImbalancesMessage{Error: err.Error()}}
		return err
	}

	imbalances, tipID, tipHeight, err := p.ledger.GetPublicKeyImbalances(pubKeys)
	if err != nil {
		outChan <- Message{Type: "imbalances", Body: ImbalancesMessage{Error: err.Error()}}
		return err
	}

	bm := ImbalancesMessage{ViewID: tipID, Height: tipHeight}
	bm.Imbalances = make([]PublicKeyImbalance, len(imbalances))

	i := 0
	for pk, imbalance := range imbalances {
		var pubKey [ed25519.PublicKeySize]byte
		copy(pubKey[:], pk[:])
		bm.Imbalances[i] = PublicKeyImbalance{PublicKey: ed25519.PublicKey(pubKey[:]), Imbalance: imbalance}
		i++
	}

	outChan <- Message{Type: "imbalances", Body: bm}
	return nil
}

// Handle a request for a public key's considerations over a given height range
func (p *Peer) onGetPublicKeyConsiderations(pubKey ed25519.PublicKey,
	startHeight, endHeight int64, startIndex, limit int, outChan chan<- Message) error {
	log.Printf("Received get_public_key_considerations from: %s\n", p.conn.RemoteAddr())

	if limit < 0 {
		outChan <- Message{Type: "public_key_considerations"}
		return nil
	}

	// enforce our limit
	if limit > 32 || limit == 0 {
		limit = 32
	}

	// get the indices for all considerations for the given public key
	// over the given range of view heights
	bIDs, indices, stopHeight, stopIndex, err := p.ledger.GetPublicKeyConsiderationIndicesRange(
		pubKey, startHeight, endHeight, startIndex, limit)
	if err != nil {
		outChan <- Message{Type: "public_key_considerations", Body: PublicKeyConsiderationsMessage{Error: err.Error()}}
		return err
	}

	// build filter views from the indices
	var fbs []*FilterViewMessage
	for i, viewID := range bIDs {
		// fetch consideration and header
		cn, viewHeader, err := p.viewStore.GetConsideration(viewID, indices[i])
		if err != nil {
			// odd case. just log it and continue
			log.Printf("Error retrieving consideration history, view: %s, index: %d, error: %s\n",
				viewID, indices[i], err)
			continue
		}
		// figure out where to put it
		var fb *FilterViewMessage
		if len(fbs) == 0 {
			// new view
			fb = &FilterViewMessage{ViewID: viewID, Header: viewHeader}
			fbs = append(fbs, fb)
		} else if fbs[len(fbs)-1].ViewID != viewID {
			// new view
			fb = &FilterViewMessage{ViewID: viewID, Header: viewHeader}
			fbs = append(fbs, fb)
		} else {
			// consideration is from the same view
			fb = fbs[len(fbs)-1]
		}
		fb.Considerations = append(fb.Considerations, cn)
	}

	// send it to the writer
	outChan <- Message{
		Type: "public_key_considerations",
		Body: PublicKeyConsiderationsMessage{
			PublicKey:    pubKey,
			StartHeight:  startHeight,
			StopHeight:   stopHeight,
			StopIndex:    stopIndex,
			FilterViewes: fbs,
		},
	}
	return nil
}

// Handle a request for a consideration
func (p *Peer) onGetConsideration(cnID ConsiderationID, outChan chan<- Message) error {
	log.Printf("Received get_consideration for %s, from: %s\n",
		cnID, p.conn.RemoteAddr())

	viewID, index, err := p.ledger.GetConsiderationIndex(cnID)
	if err != nil {
		// not found
		outChan <- Message{Type: "consideration", Body: ConsiderationMessage{ConsiderationID: cnID}}
		return err
	}
	if viewID == nil {
		// not found
		outChan <- Message{Type: "consideration", Body: ConsiderationMessage{ConsiderationID: cnID}}
		return fmt.Errorf("Consideration %s not found", cnID)
	}
	cn, header, err := p.viewStore.GetConsideration(*viewID, index)
	if err != nil {
		// odd case but send back what we know at least
		outChan <- Message{Type: "consideration", Body: ConsiderationMessage{ViewID: viewID, ConsiderationID: cnID}}
		return err
	}
	if cn == nil {
		// another odd case
		outChan <- Message{
			Type: "consideration",
			Body: ConsiderationMessage{
				ViewID:          viewID,
				Height:          header.Height,
				ConsiderationID: cnID,
			},
		}
		return fmt.Errorf("Consideration at view %s, index %d not found",
			*viewID, index)
	}

	// send it
	outChan <- Message{
		Type: "consideration",
		Body: ConsiderationMessage{
			ViewID:          viewID,
			Height:          header.Height,
			ConsiderationID: cnID,
			Consideration:   cn,
		},
	}
	return nil
}

// Handle a request for a view header of the tip of the main point from a peer
func (p *Peer) onGetTipHeader(outChan chan<- Message) error {
	log.Printf("Received get_tip_header, from: %s\n", p.conn.RemoteAddr())
	tipID, tipHeader, tipWhen, err := getPointTipHeader(p.ledger, p.viewStore)
	if err != nil {
		// shouldn't be possible
		outChan <- Message{Type: "tip_header"}
		return err
	}
	outChan <- Message{
		Type: "tip_header",
		Body: TipHeaderMessage{
			ViewID:     tipID,
			ViewHeader: tipHeader,
			TimeSeen:   tipWhen,
		},
	}
	return nil
}

// Handle receiving a consideration from a peer
func (p *Peer) onPushConsideration(cn *Consideration, outChan chan<- Message) error {
	id, err := cn.ID()
	if err != nil {
		outChan <- Message{Type: "push_consideration_result", Body: PushConsiderationResultMessage{Error: err.Error()}}
		return err
	}

	log.Printf("Received push_consideration: %s, from: %s\n", id, p.conn.RemoteAddr())

	// process the consideration if this is the first time we've seen it
	var errStr string
	if !p.cnQueue.Exists(id) {
		err = p.processor.ProcessConsideration(id, cn, p.conn.RemoteAddr().String())
		if err != nil {
			errStr = err.Error()
		}
	}

	outChan <- Message{Type: "push_consideration_result",
		Body: PushConsiderationResultMessage{
			ConsiderationID: id,
			Error:           errStr,
		},
	}
	return err
}

// Handle a request to set a consideration filter for the connection
func (p *Peer) onFilterLoad(filterType string, filterBytes []byte, outChan chan<- Message) error {
	log.Printf("Received filter_load (size: %d), from: %s\n", len(filterBytes), p.conn.RemoteAddr())

	// check filter type
	if filterType != "cuckoo" {
		err := fmt.Errorf("Unsupported filter type: %s", filterType)
		result := FilterResultMessage{Error: err.Error()}
		outChan <- Message{Type: "filter_result", Body: result}
		return err
	}

	// check limit
	maxSize := 1 << 16
	if len(filterBytes) > maxSize {
		err := fmt.Errorf("Filter too large, max: %d\n", maxSize)
		result := FilterResultMessage{Error: err.Error()}
		outChan <- Message{Type: "filter_result", Body: result}
		return err
	}

	// decode it
	filter, err := cuckoo.Decode(filterBytes)
	if err != nil {
		result := FilterResultMessage{Error: err.Error()}
		outChan <- Message{Type: "filter_result", Body: result}
		return err
	}

	// set the filter
	func() {
		p.filterLock.Lock()
		defer p.filterLock.Unlock()
		p.filter = filter
	}()

	// send the empty result
	outChan <- Message{Type: "filter_result"}
	return nil
}

// Handle a request to add a set of public keys to the filter
func (p *Peer) onFilterAdd(pubKeys []ed25519.PublicKey, outChan chan<- Message) error {
	log.Printf("Received filter_add (public keys: %d), from: %s\n",
		len(pubKeys), p.conn.RemoteAddr())

	// check limit
	maxPublicKeys := 256
	if len(pubKeys) > maxPublicKeys {
		err := fmt.Errorf("Too many public keys, limit: %d", maxPublicKeys)
		result := FilterResultMessage{Error: err.Error()}
		outChan <- Message{Type: "filter_result", Body: result}
		return err
	}

	err := func() error {
		p.filterLock.Lock()
		defer p.filterLock.Unlock()
		// set the filter if it's not set
		if p.filter == nil {
			p.filter = cuckoo.NewFilter(1 << 16)
		}
		// perform the inserts
		for _, pubKey := range pubKeys {
			if !p.filter.Insert(pubKey[:]) {
				return fmt.Errorf("Unable to insert into filter")
			}
		}
		return nil
	}()

	// send the result
	var m Message
	if err != nil {
		m = Message{Type: "filter_result", Body: FilterResultMessage{Error: err.Error()}}
	} else {
		m = Message{Type: "filter_result"}
	}
	outChan <- m
	return nil
}

// Send back a filtered view of the consideration queue
func (p *Peer) onGetFilterConsiderationQueue(outChan chan<- Message) {
	log.Printf("Received get_filter_consideration_queue, from: %s\n", p.conn.RemoteAddr())

	ftq := FilterConsiderationQueueMessage{}

	p.filterLock.RLock()
	defer p.filterLock.RUnlock()
	if p.filter == nil {
		ftq.Error = "No filter set"
	} else {
		considerations := p.cnQueue.Get(0)
		for _, cn := range considerations {
			if p.filterLookup(cn) {
				ftq.Considerations = append(ftq.Considerations, cn)
			}
		}
	}

	outChan <- Message{Type: "filter_consideration_queue", Body: ftq}
}

// Returns true if the consideration is of interest to the peer
func (p *Peer) filterLookup(cn *Consideration) bool {
	if p.filter == nil {
		return true
	}

	if !cn.IsViewpoint() {
		if p.filter.Lookup(cn.By[:]) {
			return true
		}
	}
	return p.filter.Lookup(cn.For[:])
}

// Called from the writer context
func (p *Peer) createFilterView(id ViewID, view *View) (*FilterViewMessage, error) {
	p.filterLock.RLock()
	defer p.filterLock.RUnlock()

	if p.filter == nil {
		// nothing to do
		return nil, nil
	}

	// create a filter view
	fb := FilterViewMessage{ViewID: id, Header: view.Header}

	// filter out considerations the peer isn't interested in
	for _, cn := range view.Considerations {
		if p.filterLookup(cn) {
			fb.Considerations = append(fb.Considerations, cn)
		}
	}
	return &fb, nil
}

// Received a request for peer addresses
func (p *Peer) onGetPeerAddresses(outChan chan<- Message) error {
	log.Printf("Received get_peer_addresses message, from: %s\n", p.conn.RemoteAddr())

	// get up to 32 peers that have been connnected to within the last 3 hours
	addresses, err := p.peerStore.GetSince(32, time.Now().Unix()-(60*60*3))
	if err != nil {
		return err
	}

	if len(addresses) != 0 {
		outChan <- Message{Type: "peer_addresses", Body: PeerAddressesMessage{Addresses: addresses}}
	}
	return nil
}

// Received a list of addresses
func (p *Peer) onPeerAddresses(addresses []string) {
	log.Printf("Received peer_addresses message with %d address(es), from: %s\n",
		len(addresses), p.conn.RemoteAddr())

	if time.Since(p.lastPeerAddressesReceivedTime) < (getPeerAddressesPeriod - 2*time.Minute) {
		// don't let a peer flood us with peer addresses
		log.Printf("Ignoring peer addresses, time since last addresses: %v\n",
			time.Now().Sub(p.lastPeerAddressesReceivedTime))
		return
	}
	p.lastPeerAddressesReceivedTime = time.Now()

	limit := 32
	for i, addr := range addresses {
		if i == limit {
			break
		}
		// notify the peer manager
		p.addrChan <- addr
	}
}

// Called from the writer goroutine loop
func (p *Peer) onGetWork(gw GetWorkMessage) {
	var err error
	if p.workView != nil {
		err = fmt.Errorf("Peer already has work")
	} else if len(gw.PublicKeys) == 0 {
		err = fmt.Errorf("No public keys specified")
	} else if len(gw.Memo) > MAX_MEMO_LENGTH {
		err = fmt.Errorf("Max memo length (%d) exceeded: %d", MAX_MEMO_LENGTH, len(gw.Memo))
	} else {
		var tipID *ViewID
		var tipHeader *ViewHeader
		tipID, tipHeader, _, err = getPointTipHeader(p.ledger, p.viewStore)
		if err != nil {
			log.Printf("Error getting tip header: %s, for: %s\n", err, p.conn.RemoteAddr())
		} else {
			// create and send out new work
			p.pubKeys = gw.PublicKeys
			p.memo = gw.Memo
			p.createNewWorkView(*tipID, tipHeader)
		}
	}

	if err != nil {
		m := Message{Type: "work", Body: WorkMessage{Error: err.Error()}}
		p.conn.SetWriteDeadline(time.Now().Add(writeWait))
		if err := p.conn.WriteJSON(m); err != nil {
			log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
			p.conn.Close()
		}
	}
}

// Create a new work view for a rendering peer. Called from the writer goroutine loop.
func (p *Peer) createNewWorkView(tipID ViewID, tipHeader *ViewHeader) error {
	if len(p.pubKeys) == 0 {
		// peer doesn't want work
		return nil
	}

	medianTimestamp, err := computeMedianTimestamp(tipHeader, p.viewStore)
	if err != nil {
		log.Printf("Error computing median timestamp: %s, for: %s\n", err, p.conn.RemoteAddr())
	} else {
		// create a new view
		p.medianTimestamp = medianTimestamp
		keyIndex := rand.Intn(len(p.pubKeys))
		p.workID = rand.Int31()
		p.workView, err = createNextView(tipID, tipHeader, p.cnQueue, p.viewStore, p.ledger, p.pubKeys[keyIndex], p.memo)
		if err != nil {
			log.Printf("Error creating next view: %s, for: %s\n", err, p.conn.RemoteAddr())
		}
	}

	m := Message{Type: "work"}
	if err != nil {
		m.Body = WorkMessage{WorkID: p.workID, Error: err.Error()}
	} else {
		m.Body = WorkMessage{WorkID: p.workID, Header: p.workView.Header, MinTime: p.medianTimestamp + 1}
	}

	p.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := p.conn.WriteJSON(m); err != nil {
		log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
		p.conn.Close()
		return err
	}
	return err
}

// Handle a submission of rendering work. Called from the writer goroutine loop.
func (p *Peer) onSubmitWork(sw SubmitWorkMessage) {
	m := Message{Type: "submit_work_result"}
	id, err := sw.Header.ID()

	if err != nil {
		log.Printf("Error computing view ID: %s, from: %s\n", err, p.conn.RemoteAddr())
	} else if sw.WorkID == 0 {
		err = fmt.Errorf("No work ID set")
		log.Printf("%s, from: %s\n", err.Error(), p.conn.RemoteAddr())
	} else if sw.WorkID != p.workID {
		err = fmt.Errorf("Expected work ID %d, found %d", p.workID, sw.WorkID)
		log.Printf("%s, from: %s\n", err.Error(), p.conn.RemoteAddr())
	} else {
		p.workView.Header = sw.Header
		err = p.processor.ProcessView(id, p.workView, p.conn.RemoteAddr().String())
		if err != nil {
			log.Printf("Error processing work view: %s, from: %s\n", err, p.conn.RemoteAddr())
		}
	}

	if err != nil {
		m.Body = SubmitWorkResultMessage{WorkID: sw.WorkID, Error: err.Error()}
	} else {
		m.Body = SubmitWorkResultMessage{WorkID: sw.WorkID}
	}

	p.conn.SetWriteDeadline(time.Now().Add(writeWait))
	if err := p.conn.WriteJSON(m); err != nil {
		log.Printf("Write error: %s, to: %s\n", err, p.conn.RemoteAddr())
		p.conn.Close()
	}
}

// Update the read limit if necessary
func (p *Peer) updateReadLimit() {
	ok, height, err := IsInitialViewDownload(p.ledger, p.viewStore)
	if err != nil {
		log.Fatal(err)
	}

	p.readLimitLock.Lock()
	defer p.readLimitLock.Unlock()
	if ok {
		// TODO: do something smarter about this
		p.readLimit = 0
		return
	}

	// considerations are <500 bytes so this gives us significant wiggle room
	maxConsiderations := computeMaxConsiderationsPerView(height + 1)
	p.readLimit = int64(maxConsiderations) * 1024
}

// Returns the maximum allowed size of a network message
func (p *Peer) getReadLimit() int64 {
	p.readLimitLock.RLock()
	defer p.readLimitLock.RUnlock()
	return p.readLimit
}
