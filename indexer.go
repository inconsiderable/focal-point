package focalpoint

import (
	"log"
	"math"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	olc "github.com/google/open-location-code/go"
)

type Indexer struct {
	viewStore    ViewStorage
	ledger       Ledger
	processor    *Processor
	latestViewID ViewID
	latestHeight int64
	cnGraph      *Graph
	synonyms     map[string]string
	shutdownChan chan struct{}
	wg           sync.WaitGroup
}

func NewIndexer(
	conGraph *Graph,
	viewStore ViewStorage,
	ledger Ledger,
	processor *Processor,
	genesisViewID ViewID,
) *Indexer {
	return &Indexer{
		cnGraph:      conGraph,
		viewStore:    viewStore,
		ledger:       ledger,
		processor:    processor,
		latestViewID: genesisViewID,
		latestHeight: 0,
		synonyms:     make(map[string]string),
		shutdownChan: make(chan struct{}),
	}
}

// Run executes the indexer's main loop in its own goroutine.
func (idx *Indexer) Run() {
	idx.wg.Add(1)
	go idx.run()
}

func (idx *Indexer) run() {
	defer idx.wg.Done()

	ticker := time.NewTicker(30 * time.Second)

	// don't start indexing until we think we're synced.
	// we're just wasting time and slowing down the sync otherwise
	ibd, _, err := IsInitialViewDownload(idx.ledger, idx.viewStore)
	if err != nil {
		panic(err)
	}
	if ibd {
		log.Printf("Indexer waiting for focalpoint sync\n")
	ready:
		for {
			select {
			case _, ok := <-idx.shutdownChan:
				if !ok {
					log.Printf("Indexer shutting down...\n")
					return
				}
			case <-ticker.C:
				var err error
				ibd, _, err = IsInitialViewDownload(idx.ledger, idx.viewStore)
				if err != nil {
					panic(err)
				}
				if !ibd {
					// time to start indexing
					break ready
				}
			}
		}
	}

	ticker.Stop()

	header, _, err := idx.viewStore.GetViewHeader(idx.latestViewID)
	if err != nil {
		log.Println(err)
		return
	}
	if header == nil {
		// don't have it
		log.Println(err)
		return
	}
	branchType, err := idx.ledger.GetBranchType(idx.latestViewID)
	if err != nil {
		log.Println(err)
		return
	}
	if branchType != MAIN {
		// not on the main branch
		log.Println(err)
		return
	}

	var height int64 = header.Height
	for {
		nextID, err := idx.ledger.GetViewIDForHeight(height)
		if err != nil {
			log.Println(err)
			return
		}
		if nextID == nil {
			height -= 1
			break
		}

		view, err := idx.viewStore.GetView(*nextID)
		if err != nil {
			// not found
			log.Println(err)
			return
		}

		if view == nil {
			// not found
			log.Printf("No view found with ID %v", nextID)
			return
		}

		idx.indexConsiderations(view, *nextID, true)

		height += 1
	}

	log.Printf("Finished indexing at height %v", idx.latestHeight)
	log.Printf("Latest indexed viewID: %v", idx.latestViewID)

	idx.rankGraph()

	// register for tip changes
	tipChangeChan := make(chan TipChange, 1)
	idx.processor.RegisterForTipChange(tipChangeChan)
	defer idx.processor.UnregisterForTipChange(tipChangeChan)

	for {
		select {
		case tip := <-tipChangeChan:
			log.Printf("Indexer received notice of new tip view: %s at height: %d\n", tip.ViewID, tip.View.Header.Height)
			idx.indexConsiderations(tip.View, tip.ViewID, tip.Connect) //Todo: Make sure no consideration is skipped.
			if !tip.More {
				idx.rankGraph()
			}
		case _, ok := <-idx.shutdownChan:
			if !ok {
				log.Printf("Indexer shutting down...\n")
				return
			}
		}
	}
}

// Returns a slice of strings where each element is the
// original string shortened by 2 characters from the end recursively.
func generateStringsSlice(s string) []string {
	// Base case: if the string is 2 characters, return a slice with just that string.
	if len(s) == 2 {
		return []string{s}
	}
	// Recursive case: append the current string to the result of the recursive call.
	return append([]string{s}, generateStringsSlice(s[:len(s)-2])...)
}

func localeFromPubKey(pubKey string) (Ok bool, Locale string, Catchments []string) {
	splitTrimmed := strings.Split(strings.TrimRight(pubKey, "0="), "/")

	localeNotation := strings.Trim(splitTrimmed[0], "+")

	if olc.CheckFull(localeNotation) != nil {
		return false, "", nil
	}

	return true, localeNotation, generateStringsSlice(strings.Split(localeNotation, "+")[0])
}

func inflateNodes(pubKey string) (bool, string, []string, string) {
	trimmed := strings.TrimRight(pubKey, "0=")
	splitPK := strings.Split(trimmed, "/")

	if len(splitPK) < 2 {
		return false, "", append([]string{}, pubKey), ""
	}

	locale := splitPK[0]
	nodes := splitPK[1:] // everything after locale

	rating := ""
	if len(nodes) > 0 {
		last := nodes[len(nodes)-1]
		// Check if last node contains only '+'
		if len(last) == 0 || strings.Trim(last, "+") == "" {
			rating = last // last node is rating (could be blank)
			nodes = nodes[:len(nodes)-1]
		} else {
			// last node contains other characters, so treat as penultimate node
			rating = "" // implied blank rating
			// nodes remain unchanged (last node is penultimate node, blank rating)
		}
	}

	return true, locale, nodes, rating
}

func (idx *Indexer) rankGraph() {
	log.Printf("Indexer ranking at height: %d\n", idx.latestHeight)
	idx.cnGraph.Rank(1.0, 1e-6)
	log.Printf("Ranking finished")
}

func (idx *Indexer) indexConsiderations(view *View, id ViewID, increment bool) {
	idx.latestViewID = id
	idx.latestHeight = view.Header.Height
	incrementBy := 0.00
	decrementBy := 0.00

	if increment {
		incrementBy = 1
		decrementBy = -1
	} else {
		//View disconnected: Reverse all applicable considerations from the graph
		incrementBy = -1
		decrementBy = 1
	}

	for c := 0; c < len(view.Considerations); c++ {
		con := view.Considerations[c]

		conFor := pubKeyToString(con.For)
		conBy := pubKeyToString(con.By)

		idx.cnGraph.SetImbalance(conBy, int64(decrementBy))
		idx.cnGraph.SetImbalance(conFor, int64(incrementBy))

		nodesOk, _, nodes, rating := inflateNodes(conFor)

		/*
			Capture synonyms for Sender:
			"SenderKey" -> "+++/PointyCursor000000000000000000000000000="
		*/
		if !nodesOk && strings.HasPrefix(rating, "+++") {
			re := regexp.MustCompile(`^\+\+\+([^0=]+)`)
			matches := re.FindStringSubmatch(rating)

			if len(matches) > 1 {				
				idx.synonyms[conBy] = matches[1]
			}
		}

		idx.cnGraph.Link(conBy, conFor, incrementBy)

		viewHeight := strconv.FormatInt(view.Header.Height, 10) + "/"

		/*
			Build graph.
		*/
		if ok, locale, catchments := localeFromPubKey(conFor); ok && nodesOk {
			
			idx.cnGraph.Link(conFor, viewHeight, incrementBy/2)//l1

			timestamp := time.Unix(con.Time, 0)
			idx.synonyms[conFor] = "+" + strconv.Itoa(len(rating)) + timestamp.UTC().Format("2006/01/02")

			YEAR := timestamp.UTC().Format("2006/")
			MONTH := timestamp.UTC().Format("2006/01/")
			DAY := timestamp.UTC().Format("2006/01/02/")

			idx.cnGraph.Link(conFor, DAY, incrementBy/4)
			idx.cnGraph.Link(DAY, MONTH, incrementBy/4)
			idx.cnGraph.Link(MONTH, YEAR, incrementBy/4)
			idx.cnGraph.Link(YEAR, "0", incrementBy/4)
			
			weight := (incrementBy/2) / float64(len(nodes)+1)

			reversedNodes := reverse(nodes)

			idx.cnGraph.Link(conFor, "0", weight)
			for k := 0; k < len(rating); k++ {
				nweight := weight/float64(len(rating))
				idx.cnGraph.Link("0", "0", nweight)
			}
			idx.cnGraph.Link(conFor, reversedNodes[0], weight)

			for i := 0; i < len(reversedNodes); i++ {
				node := reversedNodes[i]
				trimmedNode := strings.Trim(node, "+")
				trimmedNodeKey := trimmedNode

				idx.cnGraph.Link(conFor, trimmedNodeKey, weight)

				if i == len(reversedNodes)-1 {
					trimmedNodeKey = locale
					idx.cnGraph.Link(trimmedNodeKey, catchments[0], weight)
				}

				if j := i + 1; j < len(reversedNodes) {
					next := reversedNodes[j]
					trimmedNext := strings.Trim(next, "+")

					trimmedNextKey := trimmedNext

					if j == len(reversedNodes)-1 {
						trimmedNextKey = locale
					}

					// if strings.HasSuffix(directory, "+") {
					// 	//directory/+ evaluation
					// }

					// if strings.HasSuffix(node, "+") {
					// 	//content+ evaluation
					// }

					idx.cnGraph.Link(trimmedNodeKey, trimmedNextKey, weight)
				}
			}

			for i := 0; i < len(catchments); i++ {
				if j := i + 1; j < len(catchments) {
					idx.cnGraph.Link(catchments[i], catchments[j], weight)
				}

				if i == len(catchments)-1 {
					idx.cnGraph.Link(catchments[i], "0", weight)
				}
			}			
			
			orders := DiminishingOrders(view.Header.Height)

			for j := 1; j < len(orders); j++ {
				i := j - 1

				source := strconv.FormatInt(orders[i], 10) + "/"
				target := strconv.FormatInt(orders[j], 10)

				if orders[j] != 0 {
					target = target + "/"
				}

				idx.cnGraph.Link(source, target, incrementBy/2)
			}
		}			
	}
}

// Shutdown stops the indexer synchronously.
func (idx *Indexer) Shutdown() {
	close(idx.shutdownChan)
	idx.wg.Wait()
	log.Printf("Indexer shutdown\n")
}

func DiminishingOrders(n int64) []int64 {
	// Special-case zero.
	if n == 0 {
		return []int64{0}
	}
	// Determine the number of digits.
	digits := int(math.Log10(float64(n))) + 1

	results := []int64{n}
	// For each power of 10 from 10^1 up to 10^(digits)
	for i := 0; i < digits; i++ {
		power := int64(math.Pow(10, float64(i+1)))
		rounded := n - (n % power)
		// Append only if it's a new value
		if rounded != results[len(results)-1] {
			results = append(results, rounded)
		}
	}
	return results
}

func reverse(s []string) []string {
	result := make([]string, len(s))
	for i, v := range s {
		result[len(s)-1-i] = v
	}
	return result
}
