// Copyright 2019 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package remote

import (
	"context"
	"fmt"
	"io"
	"net"

	"github.com/ledgerwatch/bolt"
	"github.com/ledgerwatch/turbo-geth/common"
	"github.com/ledgerwatch/turbo-geth/log"
	"github.com/ugorji/go/codec"
)

// Version is the current version of the remote db protocol. If the protocol changes in a non backwards compatible way,
// this constant needs to be increased
const Version uint64 = 1

const DefaultCursorCacheSize uint64 = 100 * 1000

// Command is the type of command in the boltdb remote protocol
type Command uint8

const (
	// CmdVersion : version
	// is sent from client to server to ask about the version of protocol the server supports
	// it is also to be used to be sent periodically to make sure the connection stays open
	CmdVersion Command = iota
	// CmdLastError request the last error generated by any command
	CmdLastError
	// CmdBeginTx : txHandle
	// request starting a new transaction (read-only). It returns transaction's handle (uint64), or 0
	// if there was an error. If 0 is returned, the corresponding error can be queried by CmdLastError command
	CmdBeginTx
	// CmdEndTx (txHandle)
	// request the end of the transaction (rollback)
	CmdEndTx
	// CmdBucket (txHandle, name): bucketHandle
	// requests opening a bucket with given name. It returns bucket's handle (uint64)
	CmdBucket
	// CmdGet (bucketHandle, key): value
	// requests a value for a key from given bucket.
	CmdGet
	// CmdCursor (bucketHandle): cursorHandle
	// request creating a cursor for the given bucket. It returns cursor's handle (uint64)
	CmdCursor
	// CmdCursorSeek (cursorHandle, seekKey): (key, value)
	// Moves given cursor to the seekKey, or to the next key after seekKey
	CmdCursorSeek
	// CmdCursorNext (cursorHandle, number of keys): [(key, value)]
	// Moves given cursor over the next given number of keys and streams back the (key, value) pairs
	// Pair with key == nil signifies the end of the stream
	CmdCursorNext
	// CmdCursorFirst (cursorHandle, number of keys): [(key, value)]
	// Moves given cursor to bucket start and streams back the (key, value) pairs
	// Pair with key == nil signifies the end of the stream
	CmdCursorFirst
)

// Pool of decoders
var decoderPool = make(chan *codec.Decoder, 128)

func newDecoder(r io.Reader) *codec.Decoder {
	var d *codec.Decoder
	select {
	case d = <-decoderPool:
		d.Reset(r)
	default:
		{
			var handle codec.CborHandle
			d = codec.NewDecoder(r, &handle)
		}
	}
	return d
}

func returnDecoderToPool(d *codec.Decoder) {
	select {
	case decoderPool <- d:
	default:
		log.Warn("Allowing decoder to be garbage collected, pool is full")
	}
}

// Pool of encoders
var encoderPool = make(chan *codec.Encoder, 128)

func newEncoder(w io.Writer) *codec.Encoder {
	var e *codec.Encoder
	select {
	case e = <-encoderPool:
		e.Reset(w)
	default:
		{
			var handle codec.CborHandle
			e = codec.NewEncoder(w, &handle)
		}
	}
	return e
}

func returnEncoderToPool(e *codec.Encoder) {
	select {
	case encoderPool <- e:
	default:
		log.Warn("Allowing encoder to be garbage collected, pool is full")
	}
}

// Server is to be called as a go-routine, one per every client connection.
// It runs while the connection is active and keep the entire connection's context
// in the local variables
// For tests, bytes.Buffer can be used for both `in` and `out`
func Server(db *bolt.DB, in io.Reader, out io.Writer, closer io.Closer) error {
	defer func() {
		if err1 := closer.Close(); err1 != nil {
			log.Error("Could not close connection", "error", err1)
		}
	}()
	decoder := newDecoder(in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(out)
	defer returnEncoderToPool(encoder)
	// Server is passive - it runs a loop what reads commands (and their arguments) and attempts to respond
	var lastError error
	var lastHandle uint64
	// Read-only transactions opened by the client
	transactions := make(map[uint64]*bolt.Tx)
	// Buckets opened by the client
	buckets := make(map[uint64]*bolt.Bucket)
	// List of buckets opened in each transaction
	bucketsByTx := make(map[uint64][]uint64)
	// Cursors opened by the client
	cursors := make(map[uint64]*bolt.Cursor)
	// List of cursors opened in each bucket
	cursorsByBucket := make(map[uint64][]uint64)
	var c Command
	for {
		if err := decoder.Decode(&c); err != nil {
			if err == io.EOF {
				// Graceful termination when the end of the input is reached
				break
			}
			log.Error("could not decode command", "error", err)
			return err
		}
		switch c {
		case CmdVersion:
			var version = Version
			if err := encoder.Encode(&version); err != nil {
				log.Error("could not encode response to CmdVersion", "error", err)
				return err
			}
		case CmdLastError:
			var errorString = fmt.Sprintf("%v", lastError)
			if err := encoder.Encode(&errorString); err != nil {
				log.Error("could not encode response to CmdLastError", "error", err)
				return err
			}
		case CmdBeginTx:
			var txHandle uint64
			var tx *bolt.Tx
			tx, lastError = db.Begin(false)
			if lastError == nil {
				// We do Rollback and never Commit, because the remote transactions are always read-only, and must never change
				// anything
				// nolint:errcheck
				defer tx.Rollback()
				lastHandle++
				txHandle = lastHandle
				transactions[txHandle] = tx
			}
			if err := encoder.Encode(&txHandle); err != nil {
				log.Error("could not encode txHandle in response to CmdBeginTx", "error", err)
				return err
			}
		case CmdEndTx:
			var txHandle uint64
			if err := decoder.Decode(&txHandle); err != nil {
				log.Error("could not decode txHandle for CmdEndTx")
				return err
			}
			tx, ok := transactions[txHandle]
			if !ok {
				lastError = fmt.Errorf("transaction not found")
				return nil
			}

			// Remove all the buckets
			if bucketHandles, ok1 := bucketsByTx[txHandle]; ok1 {
				for _, bucketHandle := range bucketHandles {
					if cursorHandles, ok2 := cursorsByBucket[bucketHandle]; ok2 {
						for _, cursorHandle := range cursorHandles {
							delete(cursors, cursorHandle)
						}
						delete(cursorsByBucket, bucketHandle)
					}
					delete(buckets, bucketHandle)
				}
				delete(bucketsByTx, txHandle)
			}
			if err := tx.Rollback(); err != nil {
				log.Error("could not end transaction", "handle", txHandle, "error", err)
				return err
			}
			delete(transactions, txHandle)
			lastError = nil

		case CmdBucket:
			// Read the txHandle
			var txHandle uint64
			if err := decoder.Decode(&txHandle); err != nil {
				log.Error("could not decode txHandle for CmdBucket")
				return err
			}
			// Read the name of the bucket
			var name []byte
			if err := decoder.Decode(&name); err != nil {
				log.Error("could not decode name for CmdBucket", "error", err)
				return err
			}
			var bucketHandle uint64
			if tx, ok := transactions[txHandle]; ok {
				// Open the bucket
				var bucket *bolt.Bucket
				bucket = tx.Bucket(name)
				if bucket == nil {
					lastError = fmt.Errorf("bucket not found")
				} else {
					lastHandle++
					bucketHandle = lastHandle
					buckets[bucketHandle] = bucket
					if bucketHandles, ok1 := bucketsByTx[txHandle]; ok1 {
						bucketHandles = append(bucketHandles, bucketHandle)
						bucketsByTx[txHandle] = bucketHandles
					} else {
						bucketsByTx[txHandle] = []uint64{bucketHandle}
					}
					lastError = nil
				}
			} else {
				lastError = fmt.Errorf("transaction not found")
			}
			if err := encoder.Encode(&bucketHandle); err != nil {
				log.Error("could not encode bucketHandle in response to CmdBucket", "error", err)
				return err
			}
		case CmdGet:
			var bucketHandle uint64
			if err := decoder.Decode(&bucketHandle); err != nil {
				log.Error("could not decode bucketHandle for CmdGet")
				return err
			}
			var key []byte
			if err := decoder.Decode(&key); err != nil {
				log.Error("could not decode key for CmdGet")
				return err
			}
			var value []byte
			if bucket, ok := buckets[bucketHandle]; ok {
				value, _ = bucket.Get(key)
				lastError = nil
			} else {
				lastError = fmt.Errorf("bucket not found")
			}
			if err := encoder.Encode(&value); err != nil {
				log.Error("could not encode value in response to CmdGet", "error", err)
				return err
			}
		case CmdCursor:
			var bucketHandle uint64
			if err := decoder.Decode(&bucketHandle); err != nil {
				log.Error("could not decode bucketHandle for CmdCursor")
				return err
			}
			var cursorHandle uint64
			if bucket, ok := buckets[bucketHandle]; ok {
				cursor := bucket.Cursor()
				lastHandle++
				cursorHandle = lastHandle
				cursors[cursorHandle] = cursor
				if cursorHandles, ok1 := cursorsByBucket[bucketHandle]; ok1 {
					cursorHandles = append(cursorHandles, cursorHandle)
					cursorsByBucket[bucketHandle] = cursorHandles
				} else {
					cursorsByBucket[bucketHandle] = []uint64{cursorHandle}
				}
				lastError = nil
			} else {
				lastError = fmt.Errorf("bucket not found")
			}
			if err := encoder.Encode(&cursorHandle); err != nil {
				log.Error("could not cursor handle in response to CmdCursor", "error", err)
				return err
			}
		case CmdCursorSeek:
			var cursorHandle uint64
			if err := decoder.Decode(&cursorHandle); err != nil {
				log.Error("could not decode cursorHandle for CmdCursorSeek")
				return err
			}
			var seekKey []byte
			if err := decoder.Decode(&seekKey); err != nil {
				log.Error("could not decode seekKey for CmdCursorSeek")
				return err
			}
			var key, value []byte
			if cursor, ok := cursors[cursorHandle]; ok {
				key, value = cursor.Seek(seekKey)
				lastError = nil
			} else {
				lastError = fmt.Errorf("cursor not found")
			}
			if err := encoder.Encode(&key); err != nil {
				log.Error("could not encode key in response to CmdCursorSeek", "error", err)
				return err
			}
			if err := encoder.Encode(&value); err != nil {
				log.Error("could not encode value in response to CmdCursorSeek", "error", err)
				return err
			}
		case CmdCursorNext:
			var cursorHandle uint64
			var err error

			if err := decoder.Decode(&cursorHandle); err != nil {
				log.Error("could not decode cursorHandle for CmdCursorNext")
				return err
			}
			var numberOfKeys uint64
			if err := decoder.Decode(&numberOfKeys); err != nil {
				log.Error("could not decode numberOfKeys for CmdCursorNext")
			}
			var key, value []byte
			cursor, ok := cursors[cursorHandle]
			if !ok {
				lastError = fmt.Errorf("cursor not found")
				return nil
			}

			for numberOfKeys > 0 {
				key, value = cursor.Next()
				err = encoder.Encode(&key)
				if err != nil {
					log.Error("could not encode key in response to CmdCursorNext", "error", err)
					return err
				}

				err = encoder.Encode(&value)
				if err != nil {
					log.Error("could not encode value in response to CmdCursorNext", "error", err)
					return err
				}
				numberOfKeys--
				if key == nil {
					break
				}
			}
			lastError = nil
		case CmdCursorFirst:
			var cursorHandle uint64
			if err := decoder.Decode(&cursorHandle); err != nil {
				log.Error("could not decode cursorHandle for CmdCursorFirst")
				return err
			}
			var numberOfKeys uint64
			if err := decoder.Decode(&numberOfKeys); err != nil {
				log.Error("could not decode numberOfKeys for CmdCursorFirst")
			}
			var key, value []byte
			cursor, ok := cursors[cursorHandle]
			if !ok {
				lastError = fmt.Errorf("cursor not found")
				return nil
			}

			key, value = cursor.First()
			var addrHash common.Hash
			copy(addrHash[:], key[:32])
			fmt.Println(addrHash.String())

			if err := encoder.Encode(&key); err != nil {
				log.Error("could not encode key in response to CmdCursorFirst", "error", err)
				return err
			}
			if err := encoder.Encode(&value); err != nil {
				log.Error("could not encode value in response to CmdCursorFirst", "error", err)
				return err
			}
			numberOfKeys--
			if key == nil {
				break
			}

			for numberOfKeys > 0 {
				key, value = cursor.Next()
				if err := encoder.Encode(&key); err != nil {
					log.Error("could not encode key in response to CmdCursorFirst", "error", err)
					return err
				}
				if err := encoder.Encode(&value); err != nil {
					log.Error("could not encode value in response to CmdCursorFirst", "error", err)
					return err
				}
				numberOfKeys--
				if key == nil {
					break
				}
			}
			lastError = nil
		default:
			log.Error("unknown", "command", c)
			return fmt.Errorf("unknown command %d", c)
		}
	}
	return nil
}

// Listener starts listener that for each incoming connection
// spawn a go-routine invoking Server
func Listener(ctx context.Context, db *bolt.DB, address string) {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", address)
	if err != nil {
		log.Error("Could not create listener", "address", address, "error", err)
		return
	}
	log.Info("Remote DB interface listening on", "address", address)
	var interrupted = false
	for !interrupted {
		conn, err1 := ln.Accept()
		if err1 != nil {
			log.Error("Could not accept connection", "err", err1)
			continue
		}
		//nolint:errcheck
		go Server(db, conn, conn, conn)
		select {
		case <-ctx.Done():
			log.Info("remoteDb listener interrupted")
			interrupted = true
		default:
		}
	}
	if err = ln.Close(); err != nil {
		log.Error("Could not close listener", "error", err)
	}
}

// DB mimicks the interface of the bolt.DB,
// but it works via a pair (Reader, Writer)
type DB struct {
	in     io.Reader
	out    io.Writer
	closer io.Closer
}

// NewDB creates a new instance of DB
func NewDB(in io.Reader, out io.Writer, closer io.Closer) (*DB, error) {
	decoder := newDecoder(in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(out)
	defer returnEncoderToPool(encoder)
	// Check version
	var c = CmdVersion
	if err := encoder.Encode(&c); err != nil {
		return nil, err
	}
	var v uint64
	if err := decoder.Decode(&v); err != nil {
		return nil, err
	}
	if v != Version {
		return nil, fmt.Errorf("returned version %d, expected %d", v, Version)
	}
	return &DB{in: in, out: out, closer: closer}, nil
}

// Close closes DB by using the closer field
func (db *DB) Close() {
	if db.closer != nil {
		if err := db.closer.Close(); err != nil {
			log.Error("Could not close remote DB", "error", err)
		}
	}
}

// Tx mimicks the interface of bolt.Tx
type Tx struct {
	in       io.Reader
	out      io.Writer
	txHandle uint64
}

// View performs read-only transaction on the remote database
// NOTE: not thread-safe
func (db *DB) View(f func(tx *Tx) error) error {
	decoder := newDecoder(db.in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(db.out)
	defer returnEncoderToPool(encoder)
	var c = CmdBeginTx
	if err := encoder.Encode(&c); err != nil {
		return err
	}
	var txHandle uint64
	if err := decoder.Decode(&txHandle); err != nil {
		return err
	}
	if txHandle == 0 {
		// Retrieve the error
		c = CmdLastError
		if err := encoder.Encode(&c); err != nil {
			return err
		}
		var lastErrorStr string
		if err := decoder.Decode(&lastErrorStr); err != nil {
			return err
		}
		return fmt.Errorf("%v", lastErrorStr)
	}
	tx := &Tx{in: db.in, out: db.out, txHandle: txHandle}
	opErr := f(tx)
	c = CmdEndTx
	if err := encoder.Encode(&c); err != nil {
		return err
	}
	if err := encoder.Encode(&txHandle); err != nil {
		return err
	}
	return opErr
}

// Bucket mimicks the interface of bolt.Bucket
type Bucket struct {
	in           io.Reader
	out          io.Writer
	bucketHandle uint64
}

type Cursor struct {
	in  io.Reader
	out io.Writer

	cursorHandle uint64

	cacheKeys    [][]byte
	cacheValues  [][]byte
	cacheLastIdx uint64
	cacheIdx     uint64
}

// Bucket returns the handle to the bucket in remote DB
func (tx *Tx) Bucket(name []byte) *Bucket {
	decoder := newDecoder(tx.in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(tx.out)
	defer returnEncoderToPool(encoder)
	var c = CmdBucket
	if err := encoder.Encode(&c); err != nil {
		log.Error("Could not encode CmdBucket", "error", err)
		return nil
	}
	if err := encoder.Encode(&tx.txHandle); err != nil {
		log.Error("Could not encode txHandle for CmdBucket", "error", err)
		return nil
	}
	if err := encoder.Encode(&name); err != nil {
		log.Error("Could not encode name for CmdBucket", "error", err)
		return nil
	}
	var bucketHandle uint64
	if err := decoder.Decode(&bucketHandle); err != nil {
		log.Error("Could not decode bucketHandle from CmdBucket result", "error", err)
		return nil
	}
	if bucketHandle == 0 {
		// Retrieve the error
		c = CmdLastError
		if err := encoder.Encode(&c); err != nil {
			log.Error("Could not encode CmdLastError to get error of CmdBucket", "error", err)
			return nil
		}
		var lastErrorStr string
		if err := decoder.Decode(&lastErrorStr); err != nil {
			log.Error("Could not decode lastErrorStr from get error of CmdBicket", "error", err)
			return nil
		}
		log.Error("Retrieved from CmdBucket", "error", lastErrorStr)
		return nil
	}
	bucket := &Bucket{bucketHandle: bucketHandle}
	return bucket
}

// Get reads a value corresponding to the given key, from the bucket
// return nil if they key is not present
func (b *Bucket) Get(key []byte) []byte {
	decoder := newDecoder(b.in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(b.out)
	defer returnEncoderToPool(encoder)
	var c = CmdGet
	if err := encoder.Encode(&c); err != nil {
		log.Error("Could not encode CmdGet", "error", err)
		return nil
	}
	if err := encoder.Encode(&b.bucketHandle); err != nil {
		log.Error("Could not encode bucketHandle for CmdGet", "error", err)
		return nil
	}
	if err := encoder.Encode(&key); err != nil {
		log.Error("Could not encode key for CmdGet", "error", err)
		return nil
	}
	var value []byte
	if err := decoder.Decode(&value); err != nil {
		log.Error("Could not decode value from CmdGet result", "error", err)
	}
	return value
}

// Cursor iterating over bucket keys
func (b *Bucket) Cursor() *Cursor {
	decoder := newDecoder(b.in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(b.out)
	defer returnEncoderToPool(encoder)
	var c = CmdCursor
	if err := encoder.Encode(&c); err != nil {
		log.Error("Could not encode CmdCursor", "error", err)
		return nil
	}
	if err := encoder.Encode(&b.bucketHandle); err != nil {
		log.Error("Could not encode bucketHandle for CmdCursor", "error", err)
		return nil
	}

	var cursorHandle uint64
	if err := decoder.Decode(&cursorHandle); err != nil {
		log.Error("Could not decode cursorHandle from CmdCursor result", "error", err)
		return nil
	}

	if cursorHandle == 0 { // Retrieve the error
		lastErrorStr, retrieveError := lastError(encoder, decoder)
		if retrieveError != nil {
			log.Error("Could not encode CmdLastError to get error of CmdCursor", "error", retrieveError)
			return nil
		}
		log.Error("Retrieved from CmdCursor", "error", lastErrorStr)
		return nil
	}

	cursor := &Cursor{
		in:           b.in,
		out:          b.out,
		cursorHandle: cursorHandle,

		cacheKeys:   make([][]byte, DefaultCursorCacheSize, DefaultCursorCacheSize),
		cacheValues: make([][]byte, DefaultCursorCacheSize, DefaultCursorCacheSize),
	}
	for i := 0; i < len(cursor.cacheKeys); i++ {
		cursor.cacheKeys[i] = make([]byte, 2*common.HashLength)
		cursor.cacheValues[i] = make([]byte, 2*common.HashLength)
	}
	return cursor
}

func lastError(encoder *codec.Encoder, decoder *codec.Decoder) (lastErrorStr string, retrieveError error) {
	// Retrieve the error
	c := CmdLastError
	if err := encoder.Encode(&c); err != nil {
		return "", err
	}

	if err := decoder.Decode(&lastErrorStr); err != nil {
		return "", fmt.Errorf("could not decode lastErrorStr %w", err)
	}

	return lastErrorStr, nil
}

func (c *Cursor) First() (key []byte, value []byte) {
	c.fetchPage(CmdCursorFirst, DefaultCursorCacheSize)
	c.cacheIdx = 0

	k, v := c.cacheKeys[c.cacheIdx], c.cacheValues[c.cacheIdx]

	c.cacheIdx++

	return k, v

}

func (c *Cursor) Seek(seek []byte) (key []byte, value []byte) {
	decoder := newDecoder(c.in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(c.out)
	defer returnEncoderToPool(encoder)
	var cmd = CmdCursorSeek
	if err := encoder.Encode(&cmd); err != nil {
		log.Error("Could not encode CmdCursorSeek", "error", err)
		return nil, nil
	}
	if err := encoder.Encode(&c.cursorHandle); err != nil {
		log.Error("Could not encode cursorHandle for CmdCursorSeek", "error", err)
		return nil, nil
	}
	if err := encoder.Encode(&seek); err != nil {
		log.Error("Could not encode seek key for CmdCursorSeek", "error", err)
		return nil, nil
	}

	if err := decoder.Decode(&key); err != nil {
		log.Error("Could not decode key for CmdCursorSeek", "error", err)
		return nil, nil
	}

	if err := decoder.Decode(&value); err != nil {
		log.Error("Could not decode value from CmdCursorSeek result", "error", err)
	}

	return key, value
}

func (c *Cursor) needFetchNextPage() bool {
	return c.cacheLastIdx == 0 || // cache is empty
		c.cacheIdx == c.cacheLastIdx // all cache read
}

func (c *Cursor) Next() (keys []byte, values []byte) {
	if c.needFetchNextPage() {
		c.fetchPage(CmdCursorNext, DefaultCursorCacheSize)
		c.cacheIdx = 0
	}

	k, v := c.cacheKeys[c.cacheIdx], c.cacheValues[c.cacheIdx]
	c.cacheIdx++

	return k, v
}

func (c *Cursor) fetchPage(cmd Command, numberOfKeys uint64) {
	decoder := newDecoder(c.in)
	defer returnDecoderToPool(decoder)
	encoder := newEncoder(c.out)
	defer returnEncoderToPool(encoder)

	if err := encoder.Encode(&cmd); err != nil {
		log.Error("Could not encode command", "error", err, "command", cmd)
		return
	}
	if err := encoder.Encode(&c.cursorHandle); err != nil {
		log.Error("Could not encode cursorHandle", "error", err, "command", cmd)
		return
	}

	if err := encoder.Encode(&numberOfKeys); err != nil {
		log.Error("Could not encode numberOfKeys", "error", err, "command", cmd)
		return
	}

	var err error

	for c.cacheLastIdx = uint64(0); c.cacheLastIdx < numberOfKeys; c.cacheLastIdx++ {
		err = decoder.Decode(c.cacheKeys[c.cacheLastIdx])
		if err != nil {
			log.Error("could not decode key in response to CmdCursorNext", "error", err)
			return
		}

		err = decoder.Decode(c.cacheValues[c.cacheLastIdx])
		if err != nil {
			log.Error("could not decode value in response to CmdCursorNext", "error", err)
			return
		}

		if c.cacheKeys[c.cacheLastIdx] == nil {
			break
		}
	}

	return
}
