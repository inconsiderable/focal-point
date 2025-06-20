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
		"height", "imbalance", "imbalance_at", "view", "view_at", "cn", "history", "verify",
	}

	dataDirPtr := flag.String("datadir", "", "Path to a directory containing focal point data")
	pubKeyPtr := flag.String("pubkey", "", "Base64 encoded public key")
	cmdPtr := flag.String("command", "height", "Commands: "+strings.Join(commands, ", "))
	heightPtr := flag.Int("height", 0, "View point height")
	viewIDPtr := flag.String("view_id", "", "View ID")
	cnIDPtr := flag.String("cn_id", "", "Consideration ID")
	startHeightPtr := flag.Int("start_height", 0, "Start view height (for use with \"history\")")
	startIndexPtr := flag.Int("start_index", 0, "Start consideration index (for use with \"history\")")
	endHeightPtr := flag.Int("end_height", 0, "End view height (for use with \"history\")")
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

	var viewID *ViewID
	if len(*viewIDPtr) != 0 {
		viewIDBytes, err := hex.DecodeString(*viewIDPtr)
		if err != nil {
			log.Fatal(err)
		}
		viewID = new(ViewID)
		copy(viewID[:], viewIDBytes)
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

	// instatiate view storage (read-only)
	viewStore, err := NewViewStorageDisk(
		filepath.Join(*dataDirPtr, "views"),
		filepath.Join(*dataDirPtr, "headers.db"),
		true,  // read-only
		false, // compress (if a view is compressed storage will figure it out)
	)
	if err != nil {
		log.Fatal(err)
	}

	// instantiate the ledger (read-only)
	ledger, err := NewLedgerDisk(filepath.Join(*dataDirPtr, "ledger.db"),
		true,  // read-only
		false, // prune (no effect with read-only set)
		viewStore,
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

	case "view_at":
		id, err := ledger.GetViewIDForHeight(int64(*heightPtr))
		if err != nil {
			log.Fatal(err)
		}
		if id == nil {
			log.Fatalf("No view found at height %d\n", *heightPtr)
		}
		view, err := viewStore.GetView(*id)
		if err != nil {
			log.Fatal(err)
		}
		if view == nil {
			log.Fatalf("No view with ID %s\n", *id)
		}
		displayView(*id, view)

	case "view":
		if viewID == nil {
			log.Fatalf("-view_id required for \"view\" command")
		}
		view, err := viewStore.GetView(*viewID)
		if err != nil {
			log.Fatal(err)
		}
		if view == nil {
			log.Fatalf("No view with id %s\n", *viewID)
		}
		displayView(*viewID, view)

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
		cn, header, err := viewStore.GetConsideration(*id, index)
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
		displayHistory(bIDs, indices, stopHeight, stopIndex, viewStore)

	case "verify":
		verify(ledger, viewStore, pubKey, currentHeight)
	}

	// close storage
	if err := viewStore.Close(); err != nil {
		log.Println(err)
	}
	if err := ledger.Close(); err != nil {
		log.Println(err)
	}
}

type conciseView struct {
	ID           ViewID         `json:"id"`
	Header       ViewHeader     `json:"header"`
	Considerations []ConsiderationID `json:"considerations"`
}

func displayView(id ViewID, view *View) {
	b := conciseView{
		ID:           id,
		Header:       *view.Header,
		Considerations: make([]ConsiderationID, len(view.Considerations)),
	}

	for i := 0; i < len(view.Considerations); i++ {
		cnID, err := view.Considerations[i].ID()
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
	ViewID     ViewID       `json:"view_id"`
	ViewHeader ViewHeader   `json:"view_header"`
	TxIndex     int           `json:"consideration_index_in_view"`
	ID          ConsiderationID `json:"consideration_id"`
	Consideration *Consideration  `json:"consideration"`
}

func displayConsideration(cnID ConsiderationID, header *ViewHeader, index int, cn *Consideration) {
	viewID, err := header.ID()
	if err != nil {
		panic(err)
	}

	t := cnWithContext{
		ViewID:     viewID,
		ViewHeader: *header,
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

func displayHistory(bIDs []ViewID, indices []int, stopHeight int64, stopIndex int, viewStore ViewStorage) {
	h := history{Considerations: make([]cnWithContext, len(indices))}
	for i := 0; i < len(indices); i++ {
		cn, header, err := viewStore.GetConsideration(bIDs[i], indices[i])
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
			ViewID:     bIDs[i],
			ViewHeader: *header,
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

func verify(ledger Ledger, viewStore ViewStorage, pubKey ed25519.PublicKey, height int64) {
	var err error
	var expect, found int64

	if pubKey == nil {
		// compute expected total imbalance
		if height-VIEWPOINT_MATURITY >= 0 {
			// sum all mature points per schedule
			var i int64
			for i = 0; i <= height-VIEWPOINT_MATURITY; i++ {
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
