package focalpoint

import (
	"bytes"
	"encoding/binary"
	"encoding/gob"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"

	"github.com/buger/jsonparser"
	"github.com/pierrec/lz4"
	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/opt"
)

// CountStorageDisk is an on-disk CountStorage implementation using the filesystem for counts
// and LevelDB for count headers.
type CountStorageDisk struct {
	db       *leveldb.DB
	dirPath  string
	readOnly bool
	compress bool
}

// NewCountStorageDisk returns a new instance of on-disk count storage.
func NewCountStorageDisk(dirPath, dbPath string, readOnly, compress bool) (*CountStorageDisk, error) {
	// create the counts path if it doesn't exist
	if !readOnly {
		if info, err := os.Stat(dirPath); os.IsNotExist(err) {
			if err := os.MkdirAll(dirPath, 0700); err != nil {
				return nil, err
			}
		} else if !info.IsDir() {
			return nil, fmt.Errorf("%s is not a directory", dirPath)
		}
	}

	// open the database
	opts := opt.Options{ReadOnly: readOnly}
	db, err := leveldb.OpenFile(dbPath, &opts)
	if err != nil {
		return nil, err
	}
	return &CountStorageDisk{
		db:       db,
		dirPath:  dirPath,
		readOnly: readOnly,
		compress: compress,
	}, nil
}

// Store is called to store all of the count's information.
func (b CountStorageDisk) Store(id CountID, count *Count, now int64) error {
	if b.readOnly {
		return fmt.Errorf("Count storage is in read-only mode")
	}

	// save the complete count to the filesystem
	countBytes, err := json.Marshal(count)
	if err != nil {
		return err
	}

	var ext string
	if b.compress {
		// compress with lz4
		in := bytes.NewReader(countBytes)
		zout := new(bytes.Buffer)
		zw := lz4.NewWriter(zout)
		if _, err := io.Copy(zw, in); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		countBytes = zout.Bytes()
		ext = ".lz4"
	} else {
		ext = ".json"
	}

	// write the count and sync
	countPath := filepath.Join(b.dirPath, id.String()+ext)
	f, err := os.OpenFile(countPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	n, err := f.Write(countBytes)
	if err != nil {
		return err
	}
	if err == nil && n < len(countBytes) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// save the header to leveldb
	encodedCountHeader, err := encodeCountHeader(count.Header, now)
	if err != nil {
		return err
	}

	wo := opt.WriteOptions{Sync: true}
	return b.db.Put(id[:], encodedCountHeader, &wo)
}

// Get returns the referenced count.
func (b CountStorageDisk) GetCount(id CountID) (*Count, error) {
	countJson, err := b.GetCountBytes(id)
	if err != nil {
		return nil, err
	}

	// unmarshal
	count := new(Count)
	if err := json.Unmarshal(countJson, count); err != nil {
		return nil, err
	}
	return count, nil
}

// GetCountBytes returns the referenced count as a byte slice.
func (b CountStorageDisk) GetCountBytes(id CountID) ([]byte, error) {
	var ext [2]string
	if b.compress {
		// order to try finding the count by extension
		ext = [2]string{".lz4", ".json"}
	} else {
		ext = [2]string{".json", ".lz4"}
	}

	var compressed bool = b.compress

	countPath := filepath.Join(b.dirPath, id.String()+ext[0])
	if _, err := os.Stat(countPath); os.IsNotExist(err) {
		compressed = !compressed
		countPath = filepath.Join(b.dirPath, id.String()+ext[1])
		if _, err := os.Stat(countPath); os.IsNotExist(err) {
			// not found
			return nil, nil
		}
	}

	// read it off disk
	countBytes, err := ioutil.ReadFile(countPath)
	if err != nil {
		return nil, err
	}

	if compressed {
		// uncompress
		zin := bytes.NewBuffer(countBytes)
		out := new(bytes.Buffer)
		zr := lz4.NewReader(zin)
		if _, err := io.Copy(out, zr); err != nil {
			return nil, err
		}
		countBytes = out.Bytes()
	}

	return countBytes, nil
}

// GetCountHeader returns the referenced count's header and the timestamp of when it was stored.
func (b CountStorageDisk) GetCountHeader(id CountID) (*CountHeader, int64, error) {
	// fetch it
	encodedHeader, err := b.db.Get(id[:], nil)
	if err == leveldb.ErrNotFound {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}

	// decode it
	return decodeCountHeader(encodedHeader)
}

// GetConsideration returns a consideration within a count and the count's header.
func (b CountStorageDisk) GetConsideration(id CountID, index int) (
	*Consideration, *CountHeader, error) {
	countJson, err := b.GetCountBytes(id)
	if err != nil {
		return nil, nil, err
	}

	// pick out and unmarshal the consideration at the index
	idx := "[" + strconv.Itoa(index) + "]"
	cnJson, _, _, err := jsonparser.Get(countJson, "considerations", idx)
	if err != nil {
		return nil, nil, err
	}
	cn := new(Consideration)
	if err := json.Unmarshal(cnJson, cn); err != nil {
		return nil, nil, err
	}

	// pick out and unmarshal the header
	hdrJson, _, _, err := jsonparser.Get(countJson, "header")
	if err != nil {
		return nil, nil, err
	}
	header := new(CountHeader)
	if err := json.Unmarshal(hdrJson, header); err != nil {
		return nil, nil, err
	}
	return cn, header, nil
}

// Close is called to close any underlying storage.
func (b *CountStorageDisk) Close() error {
	return b.db.Close()
}

// leveldb schema: {bid} -> {timestamp}{gob encoded header}

func encodeCountHeader(header *CountHeader, when int64) ([]byte, error) {
	buf := new(bytes.Buffer)
	if err := binary.Write(buf, binary.BigEndian, when); err != nil {
		return nil, err
	}
	enc := gob.NewEncoder(buf)
	if err := enc.Encode(header); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func decodeCountHeader(encodedHeader []byte) (*CountHeader, int64, error) {
	buf := bytes.NewBuffer(encodedHeader)
	var when int64
	if err := binary.Read(buf, binary.BigEndian, &when); err != nil {
		return nil, 0, err
	}
	enc := gob.NewDecoder(buf)
	header := new(CountHeader)
	if err := enc.Decode(header); err != nil {
		return nil, 0, err
	}
	return header, when, nil
}
