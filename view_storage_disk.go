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

// ViewStorageDisk is an on-disk ViewStorage implementation using the filesystem for views
// and LevelDB for view headers.
type ViewStorageDisk struct {
	db       *leveldb.DB
	dirPath  string
	readOnly bool
	compress bool
}

// NewViewStorageDisk returns a new instance of on-disk view storage.
func NewViewStorageDisk(dirPath, dbPath string, readOnly, compress bool) (*ViewStorageDisk, error) {
	// create the views path if it doesn't exist
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
	return &ViewStorageDisk{
		db:       db,
		dirPath:  dirPath,
		readOnly: readOnly,
		compress: compress,
	}, nil
}

// Store is called to store all of the view's information.
func (b ViewStorageDisk) Store(id ViewID, view *View, now int64) error {
	if b.readOnly {
		return fmt.Errorf("View storage is in read-only mode")
	}

	// save the complete view to the filesystem
	viewBytes, err := json.Marshal(view)
	if err != nil {
		return err
	}

	var ext string
	if b.compress {
		// compress with lz4
		in := bytes.NewReader(viewBytes)
		zout := new(bytes.Buffer)
		zw := lz4.NewWriter(zout)
		if _, err := io.Copy(zw, in); err != nil {
			return err
		}
		if err := zw.Close(); err != nil {
			return err
		}
		viewBytes = zout.Bytes()
		ext = ".lz4"
	} else {
		ext = ".json"
	}

	// write the view and sync
	viewPath := filepath.Join(b.dirPath, id.String()+ext)
	f, err := os.OpenFile(viewPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}
	n, err := f.Write(viewBytes)
	if err != nil {
		return err
	}
	if err == nil && n < len(viewBytes) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}

	// save the header to leveldb
	encodedViewHeader, err := encodeViewHeader(view.Header, now)
	if err != nil {
		return err
	}

	wo := opt.WriteOptions{Sync: true}
	return b.db.Put(id[:], encodedViewHeader, &wo)
}

// Get returns the referenced view.
func (b ViewStorageDisk) GetView(id ViewID) (*View, error) {
	viewJson, err := b.GetViewBytes(id)
	if err != nil {
		return nil, err
	}

	// unmarshal
	view := new(View)
	if err := json.Unmarshal(viewJson, view); err != nil {
		return nil, err
	}
	return view, nil
}

// GetViewBytes returns the referenced view as a byte slice.
func (b ViewStorageDisk) GetViewBytes(id ViewID) ([]byte, error) {
	var ext [2]string
	if b.compress {
		// order to try finding the view by extension
		ext = [2]string{".lz4", ".json"}
	} else {
		ext = [2]string{".json", ".lz4"}
	}

	var compressed bool = b.compress

	viewPath := filepath.Join(b.dirPath, id.String()+ext[0])
	if _, err := os.Stat(viewPath); os.IsNotExist(err) {
		compressed = !compressed
		viewPath = filepath.Join(b.dirPath, id.String()+ext[1])
		if _, err := os.Stat(viewPath); os.IsNotExist(err) {
			// not found
			return nil, nil
		}
	}

	// read it off disk
	viewBytes, err := ioutil.ReadFile(viewPath)
	if err != nil {
		return nil, err
	}

	if compressed {
		// uncompress
		zin := bytes.NewBuffer(viewBytes)
		out := new(bytes.Buffer)
		zr := lz4.NewReader(zin)
		if _, err := io.Copy(out, zr); err != nil {
			return nil, err
		}
		viewBytes = out.Bytes()
	}

	return viewBytes, nil
}

// GetViewHeader returns the referenced view's header and the timestamp of when it was stored.
func (b ViewStorageDisk) GetViewHeader(id ViewID) (*ViewHeader, int64, error) {
	// fetch it
	encodedHeader, err := b.db.Get(id[:], nil)
	if err == leveldb.ErrNotFound {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}

	// decode it
	return decodeViewHeader(encodedHeader)
}

// GetConsideration returns a consideration within a view and the view's header.
func (b ViewStorageDisk) GetConsideration(id ViewID, index int) (
	*Consideration, *ViewHeader, error) {
	viewJson, err := b.GetViewBytes(id)
	if err != nil {
		return nil, nil, err
	}

	// pick out and unmarshal the consideration at the index
	idx := "[" + strconv.Itoa(index) + "]"
	cnJson, _, _, err := jsonparser.Get(viewJson, "considerations", idx)
	if err != nil {
		return nil, nil, err
	}
	cn := new(Consideration)
	if err := json.Unmarshal(cnJson, cn); err != nil {
		return nil, nil, err
	}

	// pick out and unmarshal the header
	hdrJson, _, _, err := jsonparser.Get(viewJson, "header")
	if err != nil {
		return nil, nil, err
	}
	header := new(ViewHeader)
	if err := json.Unmarshal(hdrJson, header); err != nil {
		return nil, nil, err
	}
	return cn, header, nil
}

// Close is called to close any underlying storage.
func (b *ViewStorageDisk) Close() error {
	return b.db.Close()
}

// leveldb schema: {bid} -> {timestamp}{gob encoded header}

func encodeViewHeader(header *ViewHeader, when int64) ([]byte, error) {
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

func decodeViewHeader(encodedHeader []byte) (*ViewHeader, int64, error) {
	buf := bytes.NewBuffer(encodedHeader)
	var when int64
	if err := binary.Read(buf, binary.BigEndian, &when); err != nil {
		return nil, 0, err
	}
	enc := gob.NewDecoder(buf)
	header := new(ViewHeader)
	if err := enc.Decode(header); err != nil {
		return nil, 0, err
	}
	return header, when, nil
}
