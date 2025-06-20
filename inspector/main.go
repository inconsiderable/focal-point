package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	. "github.com/inconsiderable/focal-point"
	"github.com/logrusorgru/aurora"
	"golang.org/x/crypto/ed25519"
)

// A small tool to inspect the focal point and ledger offline
func main() {
	var commands = []string{
		"height", "imbalance", "imbalance_at", "count", "count_at", "cn", "history", "verify",
	}

	dataDirPtr := flag.String("datadir", "", "Path to a directory containing focal point data")
	pubKeyPtr := flag.String("pubkey", "", "Base64 encoded public key")
	cmdPtr := flag.String("command", "height", "Commands: "+strings.Join(commands, ", "))
	heightPtr := flag.Int("height", 0, "Count point height")
	countIDPtr := flag.String("count_id", "", "Count ID")
	cnIDPtr := flag.String("cn_id", "", "Consideration ID")
	startHeightPtr := flag.Int("start_height", 0, "Start count height (for use with \"history\")")
	startIndexPtr := flag.Int("start_index", 0, "Start consideration index (for use with \"history\")")
	endHeightPtr := flag.Int("end_height", 0, "End count height (for use with \"history\")")
	limitPtr := flag.Int("limit", 3, "Limit (for use with \"history\")")
	flag.Parse()

	if len(*dataDirPtr) == 0 {
		log.Printf("You must specify a -datadir\n")
		os.Exit(-1)
	}

	var pubKey ed25519.PublicKey
	if len(*pubKeyPtr) != 0 {
		// decode the key
		pubKeyBytes, err := base64.StdEncoding.DecodeString(*pubKeyPtr)
		if err != nil {
			log.Fatal(err)
		}
		pubKey = ed25519.PublicKey(pubKeyBytes)
	}

	var countID *CountID
	if len(*countIDPtr) != 0 {
		countIDBytes, err := hex.DecodeString(*countIDPtr)
		if err != nil {
			log.Fatal(err)
		}
		countID = new(CountID)
		copy(countID[:], countIDBytes)
	}

	var cnID *ConsiderationID
	if len(*cnIDPtr) != 0 {
		cnIDBytes, err := hex.DecodeString(*cnIDPtr)
		if err != nil {
			log.Fatal(err)
		}
		cnID = new(ConsiderationID)
		copy(cnID[:], cnIDBytes)
	}

	// instatiate count storage (read-only)
	countStore, err := NewCountStorageDisk(
		filepath.Join(*dataDirPtr, "counts"),
		filepath.Join(*dataDirPtr, "headers.db"),
		true,  // read-only
		false, // compress (if a count is compressed storage will figure it out)
	)
	if err != nil {
		log.Fatal(err)
	}

	// instantiate the ledger (read-only)
	ledger, err := NewLedgerDisk(filepath.Join(*dataDirPtr, "ledger.db"),
		true,  // read-only
		false, // prune (no effect with read-only set)
		countStore,
	    NewGraph())
		
	if err != nil {
		log.Fatal(err)
	}

	// get the current height
	_, currentHeight, err := ledger.GetPointTip()
	if err != nil {
		log.Fatal(err)
	}

	switch *cmdPtr {
	case "height":
		log.Printf("Current focal point height is: %d\n", aurora.Bold(currentHeight))

	case "imbalance":
		if pubKey == nil {
			log.Fatal("-pubkey required for \"imbalance\" command")
		}
		imbalance, err := ledger.GetPublicKeyImbalance(pubKey)
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Current imbalance: %+d\n", aurora.Bold(imbalance))

	case "imbalance_at":
		if pubKey == nil {
			log.Fatal("-pubkey required for \"imbalance_at\" command")
		}
		imbalance, err := ledger.GetPublicKeyImbalanceAt(pubKey, int64(*heightPtr))
		if err != nil {
			log.Fatal(err)
		}
		log.Printf("Imbalance at height %d: %+d\n", *heightPtr, aurora.Bold(imbalance))

	case "count_at":
		id, err := ledger.GetCountIDForHeight(int64(*heightPtr))
		if err != nil {
			log.Fatal(err)
		}
		if id == nil {
			log.Fatalf("No count found at height %d\n", *heightPtr)
		}
		count, err := countStore.GetCount(*id)
		if err != nil {
			log.Fatal(err)
		}
		if count == nil {
			log.Fatalf("No count with ID %s\n", *id)
		}
		displayCount(*id, count)

	case "count":
		if countID == nil {
			log.Fatalf("-count_id required for \"count\" command")
		}
		count, err := countStore.GetCount(*countID)
		if err != nil {
			log.Fatal(err)
		}
		if count == nil {
			log.Fatalf("No count with id %s\n", *countID)
		}
		displayCount(*countID, count)

	case "cn":
		if cnID == nil {
			log.Fatalf("-cn_id required for \"cn\" command")
		}
		id, index, err := ledger.GetConsiderationIndex(*cnID)
		if err != nil {
			log.Fatal(err)
		}
		if id == nil {
			log.Fatalf("Consideration %s not found", *cnID)
		}
		cn, header, err := countStore.GetConsideration(*id, index)
		if err != nil {
			log.Fatal(err)
		}
		if cn == nil {
			log.Fatalf("No consideration found with ID %s\n", *cnID)
		}
		displayConsideration(*cnID, header, index, cn)

	case "history":
		if pubKey == nil {
			log.Fatal("-pubkey required for \"history\" command")
		}
		bIDs, indices, stopHeight, stopIndex, err := ledger.GetPublicKeyConsiderationIndicesRange(
			pubKey, int64(*startHeightPtr), int64(*endHeightPtr), int(*startIndexPtr), int(*limitPtr))
		if err != nil {
			log.Fatal(err)
		}
		displayHistory(bIDs, indices, stopHeight, stopIndex, countStore)

	case "verify":
		verify(ledger, countStore, pubKey, currentHeight)
	}

	// close storage
	if err := countStore.Close(); err != nil {
		log.Println(err)
	}
	if err := ledger.Close(); err != nil {
		log.Println(err)
	}
}

type conciseCount struct {
	ID           CountID         `json:"id"`
	Header       CountHeader     `json:"header"`
	Considerations []ConsiderationID `json:"considerations"`
}

func displayCount(id CountID, count *Count) {
	b := conciseCount{
		ID:           id,
		Header:       *count.Header,
		Considerations: make([]ConsiderationID, len(count.Considerations)),
	}

	for i := 0; i < len(count.Considerations); i++ {
		cnID, err := count.Considerations[i].ID()
		if err != nil {
			panic(err)
		}
		b.Considerations[i] = cnID
	}

	bJson, err := json.MarshalIndent(&b, "", "    ")
	if err != nil {
		panic(err)
	}

	fmt.Println(string(bJson))
}

type cnWithContext struct {
	CountID     CountID       `json:"count_id"`
	CountHeader CountHeader   `json:"count_header"`
	TxIndex     int           `json:"consideration_index_in_count"`
	ID          ConsiderationID `json:"consideration_id"`
	Consideration *Consideration  `json:"consideration"`
}

func displayConsideration(cnID ConsiderationID, header *CountHeader, index int, cn *Consideration) {
	countID, err := header.ID()
	if err != nil {
		panic(err)
	}

	t := cnWithContext{
		CountID:     countID,
		CountHeader: *header,
		TxIndex:     index,
		ID:          cnID,
		Consideration: cn,
	}

	cnJson, err := json.MarshalIndent(&t, "", "    ")
	if err != nil {
		panic(err)
	}

	fmt.Println(string(cnJson))
}

type history struct {
	Considerations []cnWithContext `json:"considerations"`
}

func displayHistory(bIDs []CountID, indices []int, stopHeight int64, stopIndex int, countStore CountStorage) {
	h := history{Considerations: make([]cnWithContext, len(indices))}
	for i := 0; i < len(indices); i++ {
		cn, header, err := countStore.GetConsideration(bIDs[i], indices[i])
		if err != nil {
			panic(err)
		}
		if cn == nil {
			panic("No consideration found at index")
		}
		cnID, err := cn.ID()
		if err != nil {
			panic(err)
		}
		h.Considerations[i] = cnWithContext{
			CountID:     bIDs[i],
			CountHeader: *header,
			TxIndex:     indices[i],
			ID:          cnID,
			Consideration: cn,
		}
	}

	hJson, err := json.MarshalIndent(&h, "", "    ")
	if err != nil {
		panic(err)
	}

	fmt.Println(string(hJson))
}

func verify(ledger Ledger, countStore CountStorage, pubKey ed25519.PublicKey, height int64) {
	var err error
	var expect, found int64

	if pubKey == nil {
		// compute expected total imbalance
		if height-COUNTERPOINT_MATURITY >= 0 {
			// sum all mature points per schedule
			var i int64
			for i = 0; i <= height-COUNTERPOINT_MATURITY; i++ {
				expect += 1
			}
		}

		// compute the imbalance given the sum of all public key imbalances
		found, err = ledger.Imbalance()
	} else {
		// get expected imbalance
		expect, err = ledger.GetPublicKeyImbalance(pubKey)
		if err != nil {
			log.Fatal(err)
		}

		// compute the imbalance based on history
		found, err = ledger.GetPublicKeyImbalanceAt(pubKey, height)
		if err != nil {
			log.Fatal(err)
		}
	}

	if err != nil {
		log.Fatal(err)
	}

	if expect != found {
		log.Fatalf("%s: At height %d, we expected %+d crux but we found %+d\n",
			aurora.Bold(aurora.Red("FAILURE")),
			aurora.Bold(height),
			aurora.Bold(expect),
			aurora.Bold(found))
	}

	log.Printf("%s: At height %d, we expected %+d crux and we found %+d\n",
		aurora.Bold(aurora.Green("SUCCESS")),
		aurora.Bold(height),
		aurora.Bold(expect),
		aurora.Bold(found))
}
