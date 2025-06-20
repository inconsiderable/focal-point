package main

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"time"

	. "github.com/inconsiderable/focal-point"
	"golang.org/x/crypto/ed25519"
)

// Render a genesis view
func main() {
	rand.Seed(time.Now().UnixNano())

	memoPtr := flag.String("memo", "", "A memo to include in the genesis view's viewpoint memo field")
	pubKeyPtr := flag.String("pubkey", "", "A public key to include in the genesis view's viewpoint output")
	flag.Parse()

	if len(*memoPtr) == 0 {
		log.Fatal("Memo required for genesis view")
	}

	if len(*pubKeyPtr) == 0 {
		log.Fatal("Public key required for genesis view")
	}

	pubKeyBytes, err := base64.StdEncoding.DecodeString(*pubKeyPtr)
	if err != nil {
		log.Fatal(err)
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	// create the viewpoint
	cn := NewConsideration(nil, pubKey, 0, 0, 0, *memoPtr)

	// create the view
	targetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		log.Fatal(err)
	}
	var target ViewID
	copy(target[:], targetBytes)
	view, err := NewView(ViewID{}, 0, target, ViewID{}, []*Consideration{cn})
	if err != nil {
		log.Fatal(err)
	}

	// render it
	targetInt := view.Header.Target.GetBigInt()
	ticker := time.NewTicker(30 * time.Second)
done:
	for {
		select {
		case <-ticker.C:
			view.Header.Time = time.Now().Unix()
		default:
			// keep hashing until proof-of-work is satisfied
			idInt, _ := view.Header.IDFast(0)
			if idInt.Cmp(targetInt) <= 0 {
				break done
			}
			view.Header.Nonce += 1
			if view.Header.Nonce > MAX_NUMBER {
				view.Header.Nonce = 0
			}
		}
	}

	viewJson, err := json.Marshal(view)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%s\n", viewJson)
}
