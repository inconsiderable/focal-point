package focalpoint

import (
	"bytes"
	"encoding/hex"
	"fmt"
	"log"
	"math/big"
	"sort"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ed25519"
)

// Processor processes counts and considerations in order to construct the ledger.
// It also manages the storage of all focal point data as well as inclusion of new considerations into the consideration queue.
type Processor struct {
	genesisID               CountID
	countStore               CountStorage                   // storage of raw count data
	cnQueue                 ConsiderationQueue           // queue of considerations to confirm
	ledger                  Ledger                        // ledger built from processing counts
	cnChan                  chan cnToProcess              // receive new considerations to process on this channel
	countChan                chan countToProcess            // receive new counts to process on this channel
	registerNewTxChan       chan chan<- NewTx             // receive registration requests for new consideration notifications
	unregisterNewTxChan     chan chan<- NewTx             // receive unregistration requests for new consideration notifications
	registerTipChangeChan   chan chan<- TipChange         // receive registration requests for tip change notifications
	unregisterTipChangeChan chan chan<- TipChange         // receive unregistration requests for tip change notifications
	newTxChannels           map[chan<- NewTx]struct{}     // channels needing notification of newly processed considerations
	tipChangeChannels       map[chan<- TipChange]struct{} // channels needing notification of changes to main point tip counts
	shutdownChan            chan struct{}
	wg                      sync.WaitGroup
}

// NewTx is a message sent to registered new consideration channels when a consideration is queued.
type NewTx struct {
	ConsiderationID ConsiderationID // consideration ID
	Consideration   *Consideration  // new consideration
	Source           string           // who sent it
}

// TipChange is a message sent to registered new tip channels on main point tip (dis-)connection..
type TipChange struct {
	CountID CountID   // count ID of the main point tip count
	Count   *Count    // full count
	Source  string  // who sent the count that caused this change
	Connect bool    // true if the tip has been connected. false for disconnected
	More    bool    // true if the tip has been connected and more connections are expected
}

type cnToProcess struct {
	id         ConsiderationID // consideration ID
	cn         *Consideration  // consideration to process
	source     string           // who sent it
	resultChan chan<- error     // channel to receive the result
}

type countToProcess struct {
	id         CountID       // count ID
	count       *Count        // count to process
	source     string       // who sent it
	resultChan chan<- error // channel to receive the result
}

// NewProcessor returns a new Processor instance.
func NewProcessor(genesisID CountID, countStore CountStorage, cnQueue ConsiderationQueue, ledger Ledger) *Processor {
	return &Processor{
		genesisID:               genesisID,
		countStore:               countStore,
		cnQueue:                 cnQueue,
		ledger:                  ledger,
		cnChan:                  make(chan cnToProcess, 100),
		countChan:                make(chan countToProcess, 10),
		registerNewTxChan:       make(chan chan<- NewTx),
		unregisterNewTxChan:     make(chan chan<- NewTx),
		registerTipChangeChan:   make(chan chan<- TipChange),
		unregisterTipChangeChan: make(chan chan<- TipChange),
		newTxChannels:           make(map[chan<- NewTx]struct{}),
		tipChangeChannels:       make(map[chan<- TipChange]struct{}),
		shutdownChan:            make(chan struct{}),
	}
}

// Run executes the Processor's main loop in its own goroutine.
// It verifies and processes counts and considerations.
func (p *Processor) Run() {
	p.wg.Add(1)
	go p.run()
}

func (p *Processor) run() {
	defer p.wg.Done()

	for {
		select {
		case cnToProcess := <-p.cnChan:
			// process a consideration
			err := p.processConsideration(cnToProcess.id, cnToProcess.cn, cnToProcess.source)
			if err != nil {
				log.Println(err)
			}

			// send back the result
			cnToProcess.resultChan <- err

		case countToProcess := <-p.countChan:
			// process a count
			before := time.Now().UnixNano()
			err := p.processCount(countToProcess.id, countToProcess.count, countToProcess.source)
			if err != nil {
				log.Println(err)
			}
			after := time.Now().UnixNano()

			log.Printf("Processing took %d ms, %d consideration(s), consideration queue length: %d\n",
				(after-before)/int64(time.Millisecond),
				len(countToProcess.count.Considerations),
				p.cnQueue.Len())

			// send back the result
			countToProcess.resultChan <- err

		case ch := <-p.registerNewTxChan:
			p.newTxChannels[ch] = struct{}{}

		case ch := <-p.unregisterNewTxChan:
			delete(p.newTxChannels, ch)

		case ch := <-p.registerTipChangeChan:
			p.tipChangeChannels[ch] = struct{}{}

		case ch := <-p.unregisterTipChangeChan:
			delete(p.tipChangeChannels, ch)

		case _, ok := <-p.shutdownChan:
			if !ok {
				log.Println("Processor shutting down...")
				return
			}
		}
	}
}

// ProcessConsideration is called to process a new candidate consideration for the consideration queue.
func (p *Processor) ProcessConsideration(id ConsiderationID, cn *Consideration, from string) error {
	resultChan := make(chan error)
	p.cnChan <- cnToProcess{id: id, cn: cn, source: from, resultChan: resultChan}
	return <-resultChan
}

// ProcessCount is called to process a new candidate focal point tip.
func (p *Processor) ProcessCount(id CountID, count *Count, from string) error {
	resultChan := make(chan error)
	p.countChan <- countToProcess{id: id, count: count, source: from, resultChan: resultChan}
	return <-resultChan
}

// RegisterForNewConsiderations is called to register to receive notifications of newly queued considerations.
func (p *Processor) RegisterForNewConsiderations(ch chan<- NewTx) {
	p.registerNewTxChan <- ch
}

// UnregisterForNewConsiderations is called to unregister to receive notifications of newly queued considerations
func (p *Processor) UnregisterForNewConsiderations(ch chan<- NewTx) {
	p.unregisterNewTxChan <- ch
}

// RegisterForTipChange is called to register to receive notifications of tip count changes.
func (p *Processor) RegisterForTipChange(ch chan<- TipChange) {
	p.registerTipChangeChan <- ch
}

// UnregisterForTipChange is called to unregister to receive notifications of tip count changes.
func (p *Processor) UnregisterForTipChange(ch chan<- TipChange) {
	p.unregisterTipChangeChan <- ch
}

// Shutdown stops the processor synchronously.
func (p *Processor) Shutdown() {
	close(p.shutdownChan)
	p.wg.Wait()
	log.Println("Processor shutdown")
}

// Process a consideration
func (p *Processor) processConsideration(id ConsiderationID, cn *Consideration, source string) error {
	log.Printf("Processing consideration %s\n", id)

	// context-free checks
	if err := checkConsideration(id, cn); err != nil {
		return err
	}
	
	// no loose countpoints
	if cn.IsCounterPoint() {
		return fmt.Errorf("Countpoint consideration %s only allowed in count", id)
	}

	// is the queue full?
	if p.cnQueue.Len() >= MAX_CONSIDERATION_QUEUE_LENGTH {
		return fmt.Errorf("No room for consideration %s, queue is full", id)
	}

	// is it confirmed already?
	countID, _, err := p.ledger.GetConsiderationIndex(id)
	if err != nil {
		return err
	}
	if countID != nil {
		return fmt.Errorf("Consideration %s is already confirmed", id)
	}

	// check series, maturity and expiration
	tipID, tipHeight, err := p.ledger.GetPointTip()
	if err != nil {
		return err
	}
	if tipID == nil {
		return fmt.Errorf("No main point tip id found")
	}

	// is the series current for inclusion in the next count?
	if !checkConsiderationSeries(cn, tipHeight+1) {
		return fmt.Errorf("Consideration %s would have invalid series", id)
	}

	// would it be mature if included in the next count?
	if !cn.IsMature(tipHeight + 1) {
		return fmt.Errorf("Consideration %s would not be mature", id)
	}

	// is it expired if included in the next count?
	if cn.IsExpired(tipHeight + 1) {
		return fmt.Errorf("Consideration %s is expired, height: %d, expires: %d",
			id, tipHeight, cn.Expires)
	}

	// verify signature
	ok, err := cn.Verify()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("Signature verification failed for %s", id)
	}

	// rejects a consideration if sender would have insufficient imbalance
	ok, err = p.cnQueue.Add(id, cn)
	if err != nil {
		return err
	}
	if !ok {
		// don't notify others if the consideration already exists in the queue
		return nil
	}

	// notify channels
	for ch := range p.newTxChannels {
		ch <- NewTx{ConsiderationID: id, Consideration: cn, Source: source}
	}
	return nil
}

// Context-free consideration sanity checker
func checkConsideration(id ConsiderationID, cn *Consideration) error {
	// sane-ish time.
	// consideration timestamps are strictly for user and application usage.
	// we make no claims to their validity and rely on them for nothing.
	if cn.Time < 0 || cn.Time > MAX_NUMBER {
		return fmt.Errorf("Invalid consideration time, consideration: %s", id)
	}

	// no negative nonces
	if cn.Nonce < 0 {
		return fmt.Errorf("Negative nonce value, consideration: %s", id)
	}

	if cn.IsCounterPoint() {
		// no maturity for countpoint
		if cn.Matures > 0 {
			return fmt.Errorf("Countpoint can't have a maturity, consideration: %s", id)
		}
		// no expiration for countpoint
		if cn.Expires > 0 {
			return fmt.Errorf("Countpoint can't expire, consideration: %s", id)
		}
		// no signature on countpoint
		if len(cn.Signature) != 0 {
			return fmt.Errorf("Countpoint can't have a signature, consideration: %s", id)
		}
	} else {
		// sanity check sender
		if len(cn.By) != ed25519.PublicKeySize {
			return fmt.Errorf("Invalid consideration sender, consideration: %s", id)
		}
		// sanity check signature
		if len(cn.Signature) != ed25519.SignatureSize {
			return fmt.Errorf("Invalid consideration signature, consideration: %s", id)
		}
	}

	// sanity check recipient
	if cn.For == nil {
		return fmt.Errorf("Consideration %s missing recipient", id)
	}
	if len(cn.For) != ed25519.PublicKeySize {
		return fmt.Errorf("Invalid consideration recipient, consideration: %s", id)
	}

	// no pays to self
	if bytes.Equal(cn.By, cn.For) {
		return fmt.Errorf("Consideration %s to self is invalid", id)
	}

	// make sure memo is valid ascii/utf8
	if !utf8.ValidString(cn.Memo) {
		return fmt.Errorf("Consideration %s memo contains invalid utf8 characters", id)
	}

	// check memo length
	if len(cn.Memo) > MAX_MEMO_LENGTH {
		return fmt.Errorf("Consideration %s memo length exceeded", id)
	}

	// sanity check maturity, expiration and series
	if cn.Matures < 0 || cn.Matures > MAX_NUMBER {
		return fmt.Errorf("Invalid maturity, consideration: %s", id)
	}
	if cn.Expires < 0 || cn.Expires > MAX_NUMBER {
		return fmt.Errorf("Invalid expiration, consideration: %s", id)
	}
	if cn.Series <= 0 || cn.Series > MAX_NUMBER {
		return fmt.Errorf("Invalid series, consideration: %s", id)
	}

	return nil
}

// The series must be within the acceptable range given the current height
func checkConsiderationSeries(cn *Consideration, height int64) bool {	 
	if cn.IsCounterPoint() {
		// countpoints must start a new series right on time
		return cn.Series == height/COUNTS_UNTIL_NEW_SERIES+1
	}

	// user considerations have a grace period (1 full series) to mitigate effects
	// of any potential queueing delay and/or reorgs near series switchover time
	high := height/COUNTS_UNTIL_NEW_SERIES + 1
	low := high - 1
	if low == 0 {
		low = 1
	}
	return cn.Series >= low && cn.Series <= high
}

// Process a count
func (p *Processor) processCount(id CountID, count *Count, source string) error {
	log.Printf("Processing count %s\n", id)

	now := time.Now().Unix()

	// did we process this count already?
	branchType, err := p.ledger.GetBranchType(id)
	if err != nil {
		return err
	}
	if branchType != UNKNOWN {
		log.Printf("Already processed count %s", id)
		return nil
	}

	// sanity check the count
	if err := checkCount(id, count, now); err != nil {
		return err
	}

	// have we processed its parent?
	branchType, err = p.ledger.GetBranchType(count.Header.Previous)
	if err != nil {
		return err
	}
	if branchType != MAIN && branchType != SIDE {
		if id == p.genesisID {
			// store it
			if err := p.countStore.Store(id, count, now); err != nil {
				return err
			}
			// begin the ledger
			if err := p.connectCount(id, count, source, false); err != nil {
				return err
			}
			log.Printf("Connected count %s\n", id)
			return nil
		}
		// current count is an orphan
		return fmt.Errorf("Count %s is an orphan", id)
	}

	// attempt to extend the point
	return p.acceptCount(id, count, now, source)
}

// Context-free count sanity checker
func checkCount(id CountID, count *Count, now int64) error {
	// sanity check time
	if count.Header.Time < 0 || count.Header.Time > MAX_NUMBER {
		return fmt.Errorf("Time value is invalid, count %s", id)
	}

	// check timestamp isn't too far in the future
	if count.Header.Time > now+MAX_FUTURE_SECONDS {
		return fmt.Errorf(
			"Timestamp %d too far in the future, now %d, count %s",
			count.Header.Time,
			now,
			id,
		)
	}

	// proof-of-work should satisfy declared target
	if !count.CheckPOW(id) {
		return fmt.Errorf("Insufficient proof-of-work for count %s", id)
	}

	// sanity check nonce
	if count.Header.Nonce < 0 || count.Header.Nonce > MAX_NUMBER {
		return fmt.Errorf("Nonce value is invalid, count %s", id)
	}

	// sanity check height
	if count.Header.Height < 0 || count.Header.Height > MAX_NUMBER {
		return fmt.Errorf("Height value is invalid, count %s", id)
	}

	// check against known checkpoints
	if err := CheckpointCheck(id, count.Header.Height); err != nil {
		return err
	}

	// sanity check consideration count
	if count.Header.ConsiderationCount < 0 {
		return fmt.Errorf("Negative consideration count in header of count %s", id)
	}

	if int(count.Header.ConsiderationCount) != len(count.Considerations) {
		return fmt.Errorf("Consideration count in header doesn't match count %s", id)
	}

	// must have at least one consideration
	if len(count.Considerations) == 0 {
		return fmt.Errorf("No considerations in count %s", id)
	}

	// first cn must be a countpoint
	if !count.Considerations[0].IsCounterPoint() {
		return fmt.Errorf("First consideration is not a countpoint in count %s", id)
	}

	// check max number of considerations
	max := computeMaxConsiderationsPerCount(count.Header.Height)
	if len(count.Considerations) > max {
		return fmt.Errorf("Count %s contains too many considerations %d, max: %d",
			id, len(count.Considerations), max)
	}

	// the rest must not be countpoints
	if len(count.Considerations) > 1 {
		for i := 1; i < len(count.Considerations); i++ {
			if count.Considerations[i].IsCounterPoint() {
				return fmt.Errorf("Multiple countpoint considerations in count %s", id)
			}
		}
	}

	// basic consideration checks that don't depend on context
	cnIDs := make(map[ConsiderationID]bool)
	for _, cn := range count.Considerations {
		id, err := cn.ID()
		if err != nil {
			return err
		}
		if err := checkConsideration(id, cn); err != nil {
			return err
		}
		cnIDs[id] = true
	}

	// check for duplicate considerations
	if len(cnIDs) != len(count.Considerations) {
		return fmt.Errorf("Duplicate consideration in count %s", id)
	}

	// verify hash list root
	hashListRoot, err := computeHashListRoot(nil, count.Considerations)
	if err != nil {
		return err
	}
	if hashListRoot != count.Header.HashListRoot {
		return fmt.Errorf("Hash list root mismatch for count %s", id)
	}

	return nil
}

// Computes the maximum number of considerations allowed in a count at the given height. Inspired by BIP 101
func computeMaxConsiderationsPerCount(height int64) int {
	if height >= MAX_CONSIDERATIONS_PER_COUNT_EXCEEDED_AT_HEIGHT {
		// I guess we can revisit this sometime in the next 35 years if necessary
		return MAX_CONSIDERATIONS_PER_COUNT
	}

	// piecewise-linear-between-doublings growth
	doublings := height / COUNTS_UNTIL_CONSIDERATIONS_PER_COUNT_DOUBLING
	if doublings >= 64 {
		panic("Overflow uint64")
	}
	remainder := height % COUNTS_UNTIL_CONSIDERATIONS_PER_COUNT_DOUBLING
	factor := int64(1 << uint64(doublings))
	interpolate := (INITIAL_MAX_CONSIDERATIONS_PER_COUNT * factor * remainder) /
		COUNTS_UNTIL_CONSIDERATIONS_PER_COUNT_DOUBLING
	return int(INITIAL_MAX_CONSIDERATIONS_PER_COUNT*factor + interpolate)
}

// Attempt to extend the point with the new count
func (p *Processor) acceptCount(id CountID, count *Count, now int64, source string) error {
	prevHeader, _, err := p.countStore.GetCountHeader(count.Header.Previous)
	if err != nil {
		return err
	}

	// check height
	newHeight := prevHeader.Height + 1
	if count.Header.Height != newHeight {
		return fmt.Errorf("Expected height %d found %d for count %s",
			newHeight, count.Header.Height, id)
	}

	// did we process it already?
	branchType, err := p.ledger.GetBranchType(id)
	if err != nil {
		return err
	}
	if branchType != UNKNOWN {
		log.Printf("Already processed count %s", id)
		return nil
	}

	// check declared proof of work is correct
	target, err := computeTarget(prevHeader, p.countStore, p.ledger)
	if err != nil {
		return err
	}
	if count.Header.Target != target {
		return fmt.Errorf("Incorrect target %s, expected %s for count %s",
			count.Header.Target, target, id)
	}

	// check that cumulative work is correct
	pointWork := computePointWork(count.Header.Target, prevHeader.PointWork)
	if count.Header.PointWork != pointWork {
		return fmt.Errorf("Incorrect point work %s, expected %s for count %s",
			count.Header.PointWork, pointWork, id)
	}

	// check that the timestamp isn't too far in the past
	medianTimestamp, err := computeMedianTimestamp(prevHeader, p.countStore)
	if err != nil {
		return err
	}
	if count.Header.Time <= medianTimestamp {
		return fmt.Errorf("Timestamp is too early for count %s", id)
	}

	// check series, maturity, expiration then verify signatures
	for _, cn := range count.Considerations {
		cnID, err := cn.ID()
		if err != nil {
			return err
		}
		if !checkConsiderationSeries(cn, count.Header.Height) {
			return fmt.Errorf("Consideration %s would have invalid series", cnID)
		}
		if !cn.IsCounterPoint() {
			if !cn.IsMature(count.Header.Height) {
				return fmt.Errorf("Consideration %s is immature", cnID)
			}
			if cn.IsExpired(count.Header.Height) {
				return fmt.Errorf("Consideration %s is expired", cnID)
			}
			// if it's in the queue with the same signature we've verified it already
			if !p.cnQueue.ExistsSigned(cnID, cn.Signature) {
				ok, err := cn.Verify()
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("Signature verification failed, consideration: %s", cnID)
				}
			}
		}
	}

	// store the count if we think we're going to accept it
	if err := p.countStore.Store(id, count, now); err != nil {
		return err
	}

	// get the current tip before we try adjusting the point
	tipID, _, err := p.ledger.GetPointTip()
	if err != nil {
		return err
	}

	// finish accepting the count if possible
	if err := p.acceptCountContinue(id, count, now, prevHeader, source); err != nil {
		// we may have disconnected the old best point and partially
		// connected the new one before encountering a problem. re-activate it now
		if err2 := p.reconnectTip(*tipID, source); err2 != nil {
			log.Printf("Error reconnecting tip: %s, count: %s\n", err2, *tipID)
		}
		// return the original error
		return err
	}

	return nil
}

// Compute expected target of the current count
func computeTarget(prevHeader *CountHeader, countStore CountStorage, ledger Ledger) (CountID, error) {
	if prevHeader.Height >= BITCOIN_CASH_RETARGET_ALGORITHM_HEIGHT {
		return computeTargetBitcoinCash(prevHeader, countStore, ledger)
	}
	return computeTargetBitcoin(prevHeader, countStore)
}

// Original target computation
func computeTargetBitcoin(prevHeader *CountHeader, countStore CountStorage) (CountID, error) {
	if (prevHeader.Height+1)%RETARGET_INTERVAL != 0 {
		// not 2016th count, use previous count's value
		return prevHeader.Target, nil
	}

	// defend against time warp attack
	countsToGoBack := RETARGET_INTERVAL - 1
	if (prevHeader.Height + 1) != RETARGET_INTERVAL {
		countsToGoBack = RETARGET_INTERVAL
	}

	// walk back to the first count of the interval
	firstHeader := prevHeader
	for i := 0; i < countsToGoBack; i++ {
		var err error
		firstHeader, _, err = countStore.GetCountHeader(firstHeader.Previous)
		if err != nil {
			return CountID{}, err
		}
	}

	actualTimespan := prevHeader.Time - firstHeader.Time

	minTimespan := int64(RETARGET_TIME / 4)
	maxTimespan := int64(RETARGET_TIME * 4)

	if actualTimespan < minTimespan {
		actualTimespan = minTimespan
	}
	if actualTimespan > maxTimespan {
		actualTimespan = maxTimespan
	}

	actualTimespanInt := big.NewInt(actualTimespan)
	retargetTimeInt := big.NewInt(RETARGET_TIME)

	initialTargetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		return CountID{}, err
	}

	maxTargetInt := new(big.Int).SetBytes(initialTargetBytes)
	prevTargetInt := new(big.Int).SetBytes(prevHeader.Target[:])
	newTargetInt := new(big.Int).Mul(prevTargetInt, actualTimespanInt)
	newTargetInt.Div(newTargetInt, retargetTimeInt)

	var target CountID
	if newTargetInt.Cmp(maxTargetInt) > 0 {
		target.SetBigInt(maxTargetInt)
	} else {
		target.SetBigInt(newTargetInt)
	}

	return target, nil
}

// Revised target computation
func computeTargetBitcoinCash(prevHeader *CountHeader, countStore CountStorage, ledger Ledger) (
	targetID CountID, err error) {

	firstID, err := ledger.GetCountIDForHeight(prevHeader.Height - RETARGET_SMA_WINDOW)
	if err != nil {
		return
	}
	firstHeader, _, err := countStore.GetCountHeader(*firstID)
	if err != nil {
		return
	}

	workInt := new(big.Int).Sub(prevHeader.PointWork.GetBigInt(), firstHeader.PointWork.GetBigInt())
	workInt.Mul(workInt, big.NewInt(TARGET_SPACING))

	// "In order to avoid difficulty cliffs, we bound the amplitude of the
	// adjustment we are going to do to a factor in [0.5, 2]." - Bitcoin-ABC
	actualTimespan := prevHeader.Time - firstHeader.Time
	if actualTimespan > 2*RETARGET_SMA_WINDOW*TARGET_SPACING {
		actualTimespan = 2 * RETARGET_SMA_WINDOW * TARGET_SPACING
	} else if actualTimespan < (RETARGET_SMA_WINDOW/2)*TARGET_SPACING {
		actualTimespan = (RETARGET_SMA_WINDOW / 2) * TARGET_SPACING
	}

	workInt.Div(workInt, big.NewInt(actualTimespan))

	// T = (2^256 / W) - 1
	maxInt := new(big.Int).Exp(big.NewInt(2), big.NewInt(256), nil)
	newTargetInt := new(big.Int).Div(maxInt, workInt)
	newTargetInt.Sub(newTargetInt, big.NewInt(1))

	// don't go above the initial target
	initialTargetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		return
	}
	maxTargetInt := new(big.Int).SetBytes(initialTargetBytes)
	if newTargetInt.Cmp(maxTargetInt) > 0 {
		targetID.SetBigInt(maxTargetInt)
	} else {
		targetID.SetBigInt(newTargetInt)
	}

	return
}

// Compute the median timestamp of the last NUM_COUNTS_FOR_MEDIAN_TIMESTAMP counts
func computeMedianTimestamp(prevHeader *CountHeader, countStore CountStorage) (int64, error) {
	var timestamps []int64
	var err error
	for i := 0; i < NUM_COUNTS_FOR_MEDIAN_TMESTAMP; i++ {
		timestamps = append(timestamps, prevHeader.Time)
		prevHeader, _, err = countStore.GetCountHeader(prevHeader.Previous)
		if err != nil {
			return 0, err
		}
		if prevHeader == nil {
			break
		}
	}
	sort.Slice(timestamps, func(i, j int) bool {
		return timestamps[i] < timestamps[j]
	})
	return timestamps[len(timestamps)/2], nil
}

// Continue accepting the count
func (p *Processor) acceptCountContinue(
	id CountID, count *Count, countWhen int64, prevHeader *CountHeader, source string) error {

	// get the current tip
	tipID, tipHeader, tipWhen, err := getPointTipHeader(p.ledger, p.countStore)
	if err != nil {
		return err
	}
	if id == *tipID {
		// can happen if we failed connecting a new count
		return nil
	}

	// is this count better than the current tip?
	if !count.Header.Compare(tipHeader, countWhen, tipWhen) {
		// flag this as a side branch count
		log.Printf("Count %s does not represent the tip of the best point", id)
		return p.ledger.SetBranchType(id, SIDE)
	}

	// the new count is the better point
	tipAncestor := tipHeader
	newAncestor := prevHeader

	minHeight := tipAncestor.Height
	if newAncestor.Height < minHeight {
		minHeight = newAncestor.Height
	}

	var countsToDisconnect, countsToConnect []CountID

	// walk back each point to the common minHeight
	tipAncestorID := *tipID
	for tipAncestor.Height > minHeight {
		countsToDisconnect = append(countsToDisconnect, tipAncestorID)
		tipAncestorID = tipAncestor.Previous
		tipAncestor, _, err = p.countStore.GetCountHeader(tipAncestorID)
		if err != nil {
			return err
		}
	}

	newAncestorID := count.Header.Previous
	for newAncestor.Height > minHeight {
		countsToConnect = append([]CountID{newAncestorID}, countsToConnect...)
		newAncestorID = newAncestor.Previous
		newAncestor, _, err = p.countStore.GetCountHeader(newAncestorID)
		if err != nil {
			return err
		}
	}

	// scan both points until we get to the common ancestor
	for *newAncestor != *tipAncestor {
		countsToDisconnect = append(countsToDisconnect, tipAncestorID)
		countsToConnect = append([]CountID{newAncestorID}, countsToConnect...)
		tipAncestorID = tipAncestor.Previous
		tipAncestor, _, err = p.countStore.GetCountHeader(tipAncestorID)
		if err != nil {
			return err
		}
		newAncestorID = newAncestor.Previous
		newAncestor, _, err = p.countStore.GetCountHeader(newAncestorID)
		if err != nil {
			return err
		}
	}

	// we're at common ancestor. disconnect any main point counts we need to
	for _, id := range countsToDisconnect {
		countToDisconnect, err := p.countStore.GetCount(id)
		if err != nil {
			return err
		}
		if err := p.disconnectCount(id, countToDisconnect, source); err != nil {
			return err
		}
	}

	// connect any new point counts we need to
	for _, id := range countsToConnect {
		countToConnect, err := p.countStore.GetCount(id)
		if err != nil {
			return err
		}
		if err := p.connectCount(id, countToConnect, source, true); err != nil {
			return err
		}
	}

	// and finally connect the new count
	return p.connectCount(id, count, source, false)
}

// Update the ledger and consideration queue and notify undo tip channels
func (p *Processor) disconnectCount(id CountID, count *Count, source string) error {
	// Update the ledger
	cnIDs, err := p.ledger.DisconnectCount(id, count)
	if err != nil {
		return err
	}

	log.Printf("Count %s has been disconnected, height: %d\n", id, count.Header.Height)

	// Add newly disconnected non-countpoint considerations back to the queue
	if err := p.cnQueue.AddBatch(cnIDs[1:], count.Considerations[1:], count.Header.Height-1); err != nil {
		return err
	}

	// Notify tip change channels
	for ch := range p.tipChangeChannels {
		ch <- TipChange{CountID: id, Count: count, Source: source}
	}
	return nil
}

// Update the ledger and consideration queue and notify new tip channels
func (p *Processor) connectCount(id CountID, count *Count, source string, more bool) error {
	// Update the ledger
	cnIDs, err := p.ledger.ConnectCount(id, count)
	if err != nil {
		return err
	}

	log.Printf("Count %s is the new tip, height: %d\n", id, count.Header.Height)

	// Remove newly confirmed non-countpoint considerations from the queue
	if err := p.cnQueue.RemoveBatch(cnIDs[1:], count.Header.Height, more); err != nil {
		return err
	}

	// Notify tip change channels
	for ch := range p.tipChangeChannels {
		ch <- TipChange{CountID: id, Count: count, Source: source, Connect: true, More: more}
	}
	return nil
}

// Try to reconnect the previous tip count when acceptCountContinue fails for the new count
func (p *Processor) reconnectTip(id CountID, source string) error {
	count, err := p.countStore.GetCount(id)
	if err != nil {
		return err
	}
	if count == nil {
		return fmt.Errorf("Count %s not found", id)
	}
	_, when, err := p.countStore.GetCountHeader(id)
	if err != nil {
		return err
	}
	prevHeader, _, err := p.countStore.GetCountHeader(count.Header.Previous)
	if err != nil {
		return err
	}
	return p.acceptCountContinue(id, count, when, prevHeader, source)
}

// Convenience method to get the current main point's tip ID, header, and storage time.
func getPointTipHeader(ledger Ledger, countStore CountStorage) (*CountID, *CountHeader, int64, error) {
	// get the current tip
	tipID, _, err := ledger.GetPointTip()
	if err != nil {
		return nil, nil, 0, err
	}
	if tipID == nil {
		return nil, nil, 0, nil
	}

	// get the header
	tipHeader, tipWhen, err := countStore.GetCountHeader(*tipID)
	if err != nil {
		return nil, nil, 0, err
	}
	return tipID, tipHeader, tipWhen, nil
}
