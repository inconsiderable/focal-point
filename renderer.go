package focalpoint

import (
	"log"
	"math/big"
	"math/rand"
	"sync"
	"time"

	"golang.org/x/crypto/ed25519"
)

// Renderer tries to render a new tip count.
type Renderer struct {
	pubKeys        []ed25519.PublicKey // champions of any count(-point) we render
	memo           string              // memo for count(-point) of any counts we render
	countStore      CountStorage
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
	countStore CountStorage, cnQueue ConsiderationQueue,
	ledger Ledger, processor *Processor,
	hashUpdateChan chan int64, num int) *Renderer {
	return &Renderer{
		pubKeys:        pubKeys,
		memo:           memo,
		countStore:      countStore,
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
	ibd, _, err := IsInitialCountDownload(m.ledger, m.countStore)
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
				ibd, _, err = IsInitialCountDownload(m.ledger, m.countStore)
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
	var count *Count
	var targetInt *big.Int
	for {
		select {
		case tip := <-tipChangeChan:
			if !tip.Connect || tip.More {
				// only build off newly connected tip counts
				continue
			}

			// give up whatever count we were working on
			log.Printf("Renderer %d received notice of new tip count %s\n", m.num, tip.CountID)

			var err error
			// start working on a new count
			count, err = m.createNextCount(tip.CountID, tip.Count.Header)
			if err != nil {
				// ledger state is broken
				panic(err)
			}
			// make sure we're at least +1 the median timestamp
			medianTimestamp, err = computeMedianTimestamp(tip.Count.Header, m.countStore)
			if err != nil {
				panic(err)
			}
			if count.Header.Time <= medianTimestamp {
				count.Header.Time = medianTimestamp + 1
			}
			// convert our target to a big.Int
			targetInt = count.Header.Target.GetBigInt()

		case newTx := <-newTxChan:
			log.Printf("Renderer %d received notice of new consideration %s\n", m.num, newTx.ConsiderationID)
			if count == nil {
				// we're not working on a count yet
				continue
			}

			if MAX_CONSIDERATIONS_TO_INCLUDE_PER_COUNT != 0 &&
				len(count.Considerations) >= MAX_CONSIDERATIONS_TO_INCLUDE_PER_COUNT {
				log.Printf("Per-count consideration limit hit (%d)\n", len(count.Considerations))
				continue
			}

			// add the consideration to the count
			if err := count.AddConsideration(newTx.ConsiderationID, newTx.Consideration); err != nil {
				log.Printf("Error adding new consideration %s to count: %s\n",
					newTx.ConsiderationID, err)
				// abandon the count
				count = nil
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

			if count != nil {
				// update count time every so often
				now := time.Now().Unix()
				if now > medianTimestamp {
					count.Header.Time = now
				}
			}

		default:
			if count == nil {
				// find the tip to start working off of
				tipID, tipHeader, _, err := getPointTipHeader(m.ledger, m.countStore)
				if err != nil {
					panic(err)
				}
				// create a new count
				count, err = m.createNextCount(*tipID, tipHeader)
				if err != nil {
					panic(err)
				}
				// make sure we're at least +1 the median timestamp
				medianTimestamp, err = computeMedianTimestamp(tipHeader, m.countStore)
				if err != nil {
					panic(err)
				}
				if count.Header.Time <= medianTimestamp {
					count.Header.Time = medianTimestamp + 1
				}
				// convert our target to a big.Int
				targetInt = count.Header.Target.GetBigInt()
			}

			// hash the count and check the proof-of-work
			idInt, attempts := count.Header.IDFast(m.num)
			hashes += attempts
			if idInt.Cmp(targetInt) <= 0 {
				// found a solution
				id := new(CountID).SetBigInt(idInt)
				log.Printf("Renderer %d rendered new count %s\n", m.num, *id)

				// process the count
				if err := m.processor.ProcessCount(*id, count, "localhost"); err != nil {
					log.Printf("Error processing rendered count: %s\n", err)
				}

				count = nil
				m.keyIndex = rand.Intn(len(m.pubKeys))
			} else {
				// no solution yet
				count.Header.Nonce += attempts
				if count.Header.Nonce > MAX_NUMBER {
					count.Header.Nonce = 0
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

// Create a new count off of the given tip count.
func (m *Renderer) createNextCount(tipID CountID, tipHeader *CountHeader) (*Count, error) {
	log.Printf("Renderer %d rendering new count from current tip %s\n", m.num, tipID)
	pubKey := m.pubKeys[m.keyIndex]
	return createNextCount(tipID, tipHeader, m.cnQueue, m.countStore, m.ledger, pubKey, m.memo)
}

// Called by the renderer as well as the peer to support get_work.
func createNextCount(tipID CountID, tipHeader *CountHeader, cnQueue ConsiderationQueue,
	countStore CountStorage, ledger Ledger, pubKey ed25519.PublicKey, memo string) (*Count, error) {

	// fetch considerations to confirm from the queue
	cns := cnQueue.Get(MAX_CONSIDERATIONS_TO_INCLUDE_PER_COUNT - 1)

	// calculate total count point
	var newHeight int64 = tipHeader.Height + 1

	// build countpoint
	cn := NewConsideration(nil, pubKey, 0, 0, newHeight, memo)

	// prepend countpoint
	cns = append([]*Consideration{cn}, cns...)

	// compute the next target
	newTarget, err := computeTarget(tipHeader, countStore, ledger)
	if err != nil {
		return nil, err
	}

	// create the count
	count, err := NewCount(tipID, newHeight, newTarget, tipHeader.PointWork, cns)
	if err != nil {
		return nil, err
	}
	return count, nil
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
