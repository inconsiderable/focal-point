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

// Render a genesis count
func main() {
	rand.Seed(time.Now().UnixNano())

	memoPtr := flag.String("memo", "", "A memo to include in the genesis count's countpoint memo field")
	pubKeyPtr := flag.String("pubkey", "", "A public key to include in the genesis count's countpoint output")
	flag.Parse()

	if len(*memoPtr) == 0 {
		log.Fatal("Memo required for genesis count")
	}

	if len(*pubKeyPtr) == 0 {
		log.Fatal("Public key required for genesis count")
	}

	pubKeyBytes, err := base64.StdEncoding.DecodeString(*pubKeyPtr)
	if err != nil {
		log.Fatal(err)
	}
	pubKey := ed25519.PublicKey(pubKeyBytes)

	// create the countpoint
	cn := NewConsideration(nil, pubKey, 0, 0, 0, *memoPtr)

	// create the count
	targetBytes, err := hex.DecodeString(INITIAL_TARGET)
	if err != nil {
		log.Fatal(err)
	}
	var target CountID
	copy(target[:], targetBytes)
	count, err := NewCount(CountID{}, 0, target, CountID{}, []*Consideration{cn})
	if err != nil {
		log.Fatal(err)
	}

	// render it
	targetInt := count.Header.Target.GetBigInt()
	ticker := time.NewTicker(30 * time.Second)
done:
	for {
		select {
		case <-ticker.C:
			count.Header.Time = time.Now().Unix()
		default:
			// keep hashing until proof-of-work is satisfied
			idInt, _ := count.Header.IDFast(0)
			if idInt.Cmp(targetInt) <= 0 {
				break done
			}
			count.Header.Nonce += 1
			if count.Header.Nonce > MAX_NUMBER {
				count.Header.Nonce = 0
			}
		}
	}

	countJson, err := json.Marshal(count)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Printf("%s\n", countJson)
}
