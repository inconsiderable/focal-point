package focalpoint

import (
	"log"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"golang.org/x/crypto/ed25519"
)

// Renderer tries to render a new tip view.
type Renderer struct {
	pubKeys        []ed25519.PublicKey // champions of any view(-point) we render
	memo           string              // memo for view(-point) of any views we render
	viewStore      ViewStorage
	cnQueue        ConsiderationQueue
	ledger         Ledger
	processor      *Processor
	num            int
	keyIndex       int
	hashUpdateChan chan int64
	shutdownChan   chan struct{}
	wg             sync.WaitGroup
}

// HashrateMonitor collects hash counts from all renderers in order to monitor and display the aggregate hashrate.
type HashrateMonitor struct {
	hashUpdateChan chan int64
	shutdownChan   chan struct{}
	wg             sync.WaitGroup
}

// NewRenderer returns a new Renderer instance.
func NewRenderer(pubKeys []ed25519.PublicKey, memo string,
	viewStore ViewStorage, cnQueue ConsiderationQueue,
	ledger Ledger, processor *Processor,
	hashUpdateChan chan int64, num int) *Renderer {
	return &Renderer{
		pubKeys:        pubKeys,
		memo:           memo,
		viewStore:      viewStore,
		cnQueue:        cnQueue,
		ledger:         ledger,
		processor:      processor,
		num:            num,
		keyIndex:       rand.Intn(len(pubKeys)),
		hashUpdateChan: hashUpdateChan,
		shutdownChan:   make(chan struct{}),
	}
}

// NewHashrateMonitor returns a new HashrateMonitor instance.
func NewHashrateMonitor(hashUpdateChan chan int64) *HashrateMonitor {
	return &HashrateMonitor{
		hashUpdateChan: hashUpdateChan,
		shutdownChan:   make(chan struct{}),
	}
}

// Run executes the renderer's main loop in its own goroutine.
func (m *Renderer) Run() {
	m.wg.Add(1)
	go m.run()
}

func (m *Renderer) run() {
	defer m.wg.Done()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	// don't start rendering until we think we're synced.
	// we're just wasting time and slowing down the sync otherwise
	ibd, _, err := IsInitialViewDownload(m.ledger, m.viewStore)
	if err != nil {
		panic(err)
	}
	if ibd {
		log.Printf("Renderer %d waiting for focalpoint sync\n", m.num)
	ready:
		for {
			select {
			case _, ok := <-m.shutdownChan:
				if !ok {
					log.Printf("Renderer %d shutting down...\n", m.num)
					return
				}
			case <-ticker.C:
				var err error
				ibd, _, err = IsInitialViewDownload(m.ledger, m.viewStore)
				if err != nil {
					panic(err)
				}
				if ibd == false {
					// time to start rendering
					break ready
				}
			}
		}
	}

	// register for tip changes
	tipChangeChan := make(chan TipChange, 1)
	m.processor.RegisterForTipChange(tipChangeChan)
	defer m.processor.UnregisterForTipChange(tipChangeChan)

	// register for new considerations
	newTxChan := make(chan NewTx, 1)
	m.processor.RegisterForNewConsiderations(newTxChan)
	defer m.processor.UnregisterForNewConsiderations(newTxChan)

	// main rendering loop
	var hashes, medianTimestamp int64
	var view *View
	var targetInt *big.Int
	for {
		select {
		case tip := <-tipChangeChan:
			if !tip.Connect || tip.More {
				// only build off newly connected tip views
				continue
			}

			// give up whatever view we were working on
			log.Printf("Renderer %d received notice of new tip view %s\n", m.num, tip.ViewID)

			var err error
			// start working on a new view
			view, err = m.createNextView(tip.ViewID, tip.View.Header)
			if err != nil {
				// ledger state is broken
				panic(err)
			}
			// make sure we're at least +1 the median timestamp
			medianTimestamp, err = computeMedianTimestamp(tip.View.Header, m.viewStore)
			if err != nil {
				panic(err)
			}
			if view.Header.Time <= medianTimestamp {
				view.Header.Time = medianTimestamp + 1
			}
			// convert our target to a big.Int
			targetInt = view.Header.Target.GetBigInt()

		case newTx := <-newTxChan:
			log.Printf("Renderer %d received notice of new consideration %s\n", m.num, newTx.ConsiderationID)
			if view == nil {
				// we're not working on a view yet
				continue
			}

			if MAX_CONSIDERATIONS_TO_INCLUDE_PER_VIEW != 0 &&
				len(view.Considerations) >= MAX_CONSIDERATIONS_TO_INCLUDE_PER_VIEW {
				log.Printf("Per-view consideration limit hit (%d)\n", len(view.Considerations))
				continue
			}

			// add the consideration to the view
			if err := view.AddConsideration(newTx.ConsiderationID, newTx.Consideration); err != nil {
				log.Printf("Error adding new consideration %s to view: %s\n",
					newTx.ConsiderationID, err)
				// abandon the view
				view = nil
			}

		case _, ok := <-m.shutdownChan:
			if !ok {
				log.Printf("Renderer %d shutting down...\n", m.num)
				return
			}

		case <-ticker.C:
			// update hashcount for hashrate monitor
			m.hashUpdateChan <- hashes
			hashes = 0

			if view != nil {
				// update view time every so often
				now := time.Now().Unix()
				if now > medianTimestamp {
					view.Header.Time = now
				}
			}

		default:
			if view == nil {
				// find the tip to start working off of
				tipID, tipHeader, _, err := getPointTipHeader(m.ledger, m.viewStore)
				if err != nil {
					panic(err)
				}
				// create a new view
				view, err = m.createNextView(*tipID, tipHeader)
				if err != nil {
					panic(err)
				}
				// make sure we're at least +1 the median timestamp
				medianTimestamp, err = computeMedianTimestamp(tipHeader, m.viewStore)
				if err != nil {
					panic(err)
				}
				if view.Header.Time <= medianTimestamp {
					view.Header.Time = medianTimestamp + 1
				}
				// convert our target to a big.Int
				targetInt = view.Header.Target.GetBigInt()
			}

			// hash the view and check the proof-of-work
			idInt, attempts := view.Header.IDFast(m.num)
			hashes += attempts
			if idInt.Cmp(targetInt) <= 0 {
				// found a solution
				id := new(ViewID).SetBigInt(idInt)
				log.Printf("Renderer %d rendered new view %s\n", m.num, *id)

				// process the view
				if err := m.processor.ProcessView(*id, view, "localhost"); err != nil {
					log.Printf("Error processing rendered view: %s\n", err)
				}

				view = nil
				m.keyIndex = rand.Intn(len(m.pubKeys))
			} else {
				// no solution yet
				view.Header.Nonce += attempts
				if view.Header.Nonce > MAX_NUMBER {
					view.Header.Nonce = 0
				}
			}
		}
	}
}

// Shutdown stops the renderer synchronously.
func (m *Renderer) Shutdown() {
	close(m.shutdownChan)
	m.wg.Wait()
	log.Printf("Renderer %d shutdown\n", m.num)
}

// Create a new view off of the given tip view.
func (m *Renderer) createNextView(tipID ViewID, tipHeader *ViewHeader) (*View, error) {
	log.Printf("Renderer %d rendering new view from current tip %s\n", m.num, tipID)
	pubKey := m.pubKeys[m.keyIndex]
	return createNextView(tipID, tipHeader, m.cnQueue, m.viewStore, m.ledger, pubKey, m.memo)
}

// Called by the renderer as well as the peer to support get_work.
func createNextView(tipID ViewID, tipHeader *ViewHeader, cnQueue ConsiderationQueue,
	viewStore ViewStorage, ledger Ledger, pubKey ed25519.PublicKey, memo string) (*View, error) {

	// fetch considerations to confirm from the queue
	cns := cnQueue.Get(MAX_CONSIDERATIONS_TO_INCLUDE_PER_VIEW - 1)

	// calculate total view point
	var newHeight int64 = tipHeader.Height + 1

	// build viewpoint
	cn := NewConsideration(nil, pubKey, 0, 0, newHeight, memo)

	// prepend viewpoint
	cns = append([]*Consideration{cn}, cns...)

	// compute the next target
	newTarget, err := computeTarget(tipHeader, viewStore, ledger)
	if err != nil {
		return nil, err
	}

	// create the view
	view, err := NewView(tipID, newHeight, newTarget, tipHeader.PointWork, cns)
	if err != nil {
		return nil, err
	}
	return view, nil
}

// Run executes the hashrate monitor's main loop in its own goroutine.
func (h *HashrateMonitor) Run() {
	h.wg.Add(1)
	go h.run()
}

func (h *HashrateMonitor) run() {
	defer h.wg.Done()

	var totalHashes int64
	updateInterval := 1 * time.Minute
	ticker := time.NewTicker(updateInterval)
	defer ticker.Stop()

	for {
		select {
		case _, ok := <-h.shutdownChan:
			if !ok {
				log.Println("Hashrate monitor shutting down...")
				return
			}
		case hashes := <-h.hashUpdateChan:
			totalHashes += hashes
		case <-ticker.C:
			hps := float64(totalHashes) / updateInterval.Seconds()
			totalHashes = 0
			log.Printf("Hashrate: %.2f MH/s", hps/1000/1000)
		}
	}
}

// Shutdown stops the hashrate monitor synchronously.
func (h *HashrateMonitor) Shutdown() {
	close(h.shutdownChan)
	h.wg.Wait()
	log.Println("Hashrate monitor shutdown")
}
