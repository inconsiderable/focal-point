package focalpoint

import (
	"encoding/hex"
	"hash"
	"math/big"
	"strconv"

	"golang.org/x/crypto/sha3"
)

// ViewHeaderHasher is used to more efficiently hash JSON serialized view headers while rendering.
type ViewHeaderHasher struct {
	// these can change per attempt
	previousHashListRoot     ConsiderationID
	previousTime             int64
	previousNonce            int64
	previousConsiderationCount int32

	// used for tracking offsets of mutable fields in the buffer
	hashListRootOffset     int
	timeOffset             int
	nonceOffset            int
	considerationCountOffset int

	// used for calculating a running offset
	timeLen    int
	nonceLen   int
	cnCountLen int

	// used for hashing
	initialized      bool
	bufLen           int
	buffer           []byte
	hasher           HashWithRead
	resultBuf        [32]byte
	result           *big.Int
	hashesPerAttempt int64
}

// HashWithRead extends hash.Hash to provide a Read interface.
type HashWithRead interface {
	hash.Hash

	// the sha3 state objects aren't exported from stdlib but some of their methods like Read are.
	// we can get the sum without the clone done by Sum which saves us a malloc in the fast path
	Read(out []byte) (n int, err error)
}

// Static fields
var hdrPrevious []byte = []byte(`{"previous":"`)
var hdrHashListRoot []byte = []byte(`","hash_list_root":"`)
var hdrTime []byte = []byte(`","time":`)
var hdrTarget []byte = []byte(`,"target":"`)
var hdrPointWork []byte = []byte(`","point_work":"`)
var hdrNonce []byte = []byte(`","nonce":`)
var hdrHeight []byte = []byte(`,"height":`)
var hdrConsiderationCount []byte = []byte(`,"consideration_count":`)
var hdrEnd []byte = []byte("}")

// NewViewHeaderHasher returns a newly initialized ViewHeaderHasher
func NewViewHeaderHasher() *ViewHeaderHasher {
	// calculate the maximum buffer length needed
	bufLen := len(hdrPrevious) + len(hdrHashListRoot) + len(hdrTime) + len(hdrTarget)
	bufLen += len(hdrPointWork) + len(hdrNonce) + len(hdrHeight) + len(hdrConsiderationCount)
	bufLen += len(hdrEnd) + 4*64 + 3*19 + 10

	// initialize the hasher
	return &ViewHeaderHasher{
		buffer:           make([]byte, bufLen),
		hasher:           sha3.New256().(HashWithRead),
		result:           new(big.Int),
		hashesPerAttempt: 1,
	}
}

// Initialize the buffer to be hashed
func (h *ViewHeaderHasher) initBuffer(header *ViewHeader) {
	// lots of mixing append on slices with writes to array offsets.
	// pretty annoying that hex.Encode and strconv.AppendInt don't have a consistent interface

	// previous
	copy(h.buffer[:], hdrPrevious)
	bufLen := len(hdrPrevious)
	written := hex.Encode(h.buffer[bufLen:], header.Previous[:])
	bufLen += written

	// hash_list_root
	h.previousHashListRoot = header.HashListRoot
	copy(h.buffer[bufLen:], hdrHashListRoot)
	bufLen += len(hdrHashListRoot)
	h.hashListRootOffset = bufLen
	written = hex.Encode(h.buffer[bufLen:], header.HashListRoot[:])
	bufLen += written

	// time
	h.previousTime = header.Time
	copy(h.buffer[bufLen:], hdrTime)
	bufLen += len(hdrTime)
	h.timeOffset = bufLen
	buffer := strconv.AppendInt(h.buffer[:bufLen], header.Time, 10)
	h.timeLen = len(buffer[bufLen:])
	bufLen += h.timeLen

	// target
	buffer = append(buffer, hdrTarget...)
	bufLen += len(hdrTarget)
	written = hex.Encode(h.buffer[bufLen:], header.Target[:])
	bufLen += written

	// point_work
	copy(h.buffer[bufLen:], hdrPointWork)
	bufLen += len(hdrPointWork)
	written = hex.Encode(h.buffer[bufLen:], header.PointWork[:])
	bufLen += written

	// nonce
	h.previousNonce = header.Nonce
	copy(h.buffer[bufLen:], hdrNonce)
	bufLen += len(hdrNonce)
	h.nonceOffset = bufLen
	buffer = strconv.AppendInt(h.buffer[:bufLen], header.Nonce, 10)
	h.nonceLen = len(buffer[bufLen:])
	bufLen += h.nonceLen

	// height
	buffer = append(buffer, hdrHeight...)
	bufLen += len(hdrHeight)
	buffer = strconv.AppendInt(buffer, header.Height, 10)
	bufLen += len(buffer[bufLen:])

	// consideration_count
	h.previousConsiderationCount = header.ConsiderationCount
	buffer = append(buffer, hdrConsiderationCount...)
	bufLen += len(hdrConsiderationCount)
	h.considerationCountOffset = bufLen
	buffer = strconv.AppendInt(buffer, int64(header.ConsiderationCount), 10)
	h.cnCountLen = len(buffer[bufLen:])
	bufLen += h.cnCountLen

	buffer = append(buffer, hdrEnd[:]...)
	h.bufLen = len(buffer[bufLen:]) + bufLen

	h.initialized = true
}

// Update is called everytime the header is updated and the caller wants its new hash value/ID.
func (h *ViewHeaderHasher) Update(rendererNum int, header *ViewHeader) (*big.Int, int64) {
	var deviceRendering bool = false
	//var bufferChanged bool

	if !h.initialized {
		h.initBuffer(header)
		//bufferChanged = true
	} else {
		// hash_list_root
		if h.previousHashListRoot != header.HashListRoot {
			//bufferChanged = true
			// write out the new value
			h.previousHashListRoot = header.HashListRoot
			hex.Encode(h.buffer[h.hashListRootOffset:], header.HashListRoot[:])
		}

		var offset int

		// time
		if h.previousTime != header.Time {
			//bufferChanged = true
			h.previousTime = header.Time

			// write out the new value
			bufLen := h.timeOffset
			buffer := strconv.AppendInt(h.buffer[:bufLen], header.Time, 10)
			timeLen := len(buffer[bufLen:])
			bufLen += timeLen

			// did time shrink or grow in length?
			offset = timeLen - h.timeLen
			h.timeLen = timeLen

			if offset != 0 {
				// shift everything below up or down

				// target
				copy(h.buffer[bufLen:], hdrTarget)
				bufLen += len(hdrTarget)
				written := hex.Encode(h.buffer[bufLen:], header.Target[:])
				bufLen += written

				// point_work
				copy(h.buffer[bufLen:], hdrPointWork)
				bufLen += len(hdrPointWork)
				written = hex.Encode(h.buffer[bufLen:], header.PointWork[:])
				bufLen += written

				// start of nonce
				copy(h.buffer[bufLen:], hdrNonce)
			}
		}

		// nonce
		if offset != 0 || (!deviceRendering && h.previousNonce != header.Nonce) {
			//bufferChanged = true
			h.previousNonce = header.Nonce

			// write out the new value (or old value at a new location)
			h.nonceOffset += offset
			bufLen := h.nonceOffset
			buffer := strconv.AppendInt(h.buffer[:bufLen], header.Nonce, 10)
			nonceLen := len(buffer[bufLen:])

			// did nonce shrink or grow in length?
			offset += nonceLen - h.nonceLen
			h.nonceLen = nonceLen

			if offset != 0 {
				// shift everything below up or down

				// height
				buffer = append(buffer, hdrHeight...)
				buffer = strconv.AppendInt(buffer, header.Height, 10)

				// start of consideration_count
				buffer = append(buffer, hdrConsiderationCount...)
			}
		}

		// consideration_count
		if offset != 0 || h.previousConsiderationCount != header.ConsiderationCount {
			//bufferChanged = true
			h.previousConsiderationCount = header.ConsiderationCount

			// write out the new value (or old value at a new location)
			h.considerationCountOffset += offset
			bufLen := h.considerationCountOffset
			buffer := strconv.AppendInt(h.buffer[:bufLen], int64(header.ConsiderationCount), 10)
			cnCountLen := len(buffer[bufLen:])

			// did count shrink or grow in length?
			offset += cnCountLen - h.cnCountLen
			h.cnCountLen = cnCountLen

			if offset != 0 {
				// shift the footer up or down
				buffer = append(buffer, hdrEnd...)
			}
		}

		// it's possible (likely) we did a bunch of encoding with no net impact to the buffer length
		h.bufLen += offset
	}

	// if deviceRendering {
	// 	// devices don't return a hash just a solving nonce (if found)
	// 	nonce := h.updateDevice(rendererNum, header, bufferChanged)
	// 	if nonce == 0x7FFFFFFFFFFFFFFF {
	// 		// not found
	// 		h.result.SetBytes(
	// 			// indirectly let renderer.go know we failed
	// 			[]byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	// 				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	// 				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	// 				0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff},
	// 		)
	// 		return h.result, h.hashesPerAttempt
	// 	} else {
	// 		log.Printf("GPU renderer %d found a possible solution: %d, double-checking it...\n",
	// 			rendererNum, nonce)
	// 		// rebuild the buffer with the new nonce since we don't update it
	// 		// per attempt when using CUDA/OpenCL.
	// 		header.Nonce = nonce
	// 		h.initBuffer(header)
	// 	}
	//}

	// hash it
	h.hasher.Reset()
	h.hasher.Write(h.buffer[:h.bufLen])
	h.hasher.Read(h.resultBuf[:])
	h.result.SetBytes(h.resultBuf[:])
	return h.result, h.hashesPerAttempt
}

// Handle rendering with GPU devices
// func (h *ViewHeaderHasher) updateDevice(rendererNum int, header *ViewHeader, bufferChanged bool) int64 {
// 	if bufferChanged {
// 		// update the device's copy of the buffer
// 		lastOffset := h.nonceOffset + h.nonceLen
// 		if CUDA_ENABLED {
// 			h.hashesPerAttempt = CudaRendererUpdate(rendererNum, h.buffer, h.bufLen,
// 				h.nonceOffset, lastOffset, header.Target)
// 		} else {
// 			h.hashesPerAttempt = OpenCLRendererUpdate(rendererNum, h.buffer, h.bufLen,
// 				h.nonceOffset, lastOffset, header.Target)
// 		}
// 	}
// 	// try for a solution
// 	var nonce int64
// 	if CUDA_ENABLED {
// 		nonce = CudaRendererRender(rendererNum, header.Nonce)
// 	} else {
// 		nonce = OpenCLRendererRender(rendererNum, header.Nonce)
// 	}
// 	return nonce
// }
