// Copyright 2009 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

/*
Qdb is a fast persistent storage database.

The records are binary blobs that can have a variable length, up to 4GB.

The key must be a unique 64-bit value, most likely a hash of the actual key.

They data is stored on a disk, in a folder specified during the call to NewDB().
There are can be three possible files in that folder
 * qdb.0, qdb.1 - these files store a compact version of the entire database
 * qdb.log - this one stores the changes since the most recent qdb.0 or qdb.1

*/
package qdb

import (
	"os"
	"fmt"
	"sync"
	"bytes"
)

type KeyType uint64
const KeySize = 8


func k2s(k KeyType) string {
	return fmt.Sprintf("%16x", k)
}


type DBConfig struct {
	// If NeverKeepInMem is set to true, the engine will never keep DB records
	// in memory, but will laways need to read them from disk.
	NeverKeepInMem bool
	// Set this function if you want to be able to decide whether a specific
	// record should be kept in memory, or freed after loaded, thus will need
	// to be taken from disk whenever needed next time.
	KeepInMem func(v []byte) bool
}


type DB struct {
	// folder with the db files
	dir string

	logfile *os.File
	lastvalidlogpos int64
	datseq uint32

	// access mutex:
	mutex sync.Mutex

	//index:
	idx *dbidx

	nosync bool
	pending_puts map[KeyType] *oneIdx
	pending_dels map[KeyType] bool
}


type oneIdx struct {
	data []byte

	datseq uint32 // data file index
	datpos uint32 // position of the record in the data file
	datlen uint32 // length of the record in the data file
}


func (i oneIdx) String() string {
	if i.data==nil {
		return fmt.Sprintf("Nodata:%d:%d:%d", i.datseq, i.datpos, i.datlen)
	} else {
		return fmt.Sprintf("Len(%d):%d:%d:%d", len(i.data), i.datseq, i.datpos, i.datlen)
	}
}


// Creates or opens a new database in the specified folder.
func NewDB(dir string, cfg *DBConfig) (db *DB, e error) {
	db = new(DB)
	if len(dir)>0 && dir[len(dir)-1]!='\\' && dir[len(dir)-1]!='/' {
		dir += string(os.PathSeparator)
	}
	os.MkdirAll(dir, 0770)
	db.dir = dir
	db.idx = NewDBidx(db)
	db.datseq = db.idx.max_dat_seq+1
	return
}


// Returns number of records in the DB
func (db *DB) Count() (l int) {
	db.mutex.Lock()
	l = db.idx.size()
	db.mutex.Unlock()
	return
}


// Browses through all teh DB records calling teh walk function for each record.
// If the walk function returns false, it aborts the browsing and returns.
func (db *DB) Browse(walk func(key KeyType, value []byte) bool) {
	db.mutex.Lock()
	//println("br", db.dir)
	db.idx.browse(func(k KeyType, v *oneIdx) bool {
		//println(v.String())
		if v.data == nil {
			//println("loading...")
			db.loadrec(v)
			//println("...", v.String())
		}
		return walk(k, v.data)
	})
	//println("br", db.dir, "done")
	db.mutex.Unlock()
}


// Fetches record with a given key. Returns nil if no such record.
func (db *DB) Get(key KeyType) (value []byte) {
	db.mutex.Lock()
	idx := db.idx.get(key)
	if idx!=nil {
		if idx.data == nil {
			db.loadrec(idx)
		}
		value = idx.data
	}
	//fmt.Printf("get %016x -> %s\n", key, hex.EncodeToString(value))
	db.mutex.Unlock()
	return
}


// Adds or updates record with a given key.
func (db *DB) Put(key KeyType, value []byte) {
	db.mutex.Lock()
	//fmt.Printf("put %016x %s\n", key, hex.EncodeToString(value))
	if db.nosync {
		rec := &oneIdx{data:value}
		db.idx.memput(key, rec)
		db.pending_puts[key] = rec
		delete(db.pending_dels, key)
	} else {
		fpos := db.addtolog(nil, key, value)
		db.idx.put(key, &oneIdx{data:value, datpos:uint32(fpos), datlen:uint32(len(value)), datseq:db.datseq})
	}
	db.mutex.Unlock()
}


// Removes record with a given key.
func (db *DB) Del(key KeyType) {
	//println("del", hex.EncodeToString(key[:]))
	db.mutex.Lock()
	if db.nosync {
		db.idx.memdel(key)
		db.pending_dels[key] = true
		delete(db.pending_puts, key)
	} else {
		db.idx.del(key)
	}
	db.mutex.Unlock()
}


// Defragments the DB on the disk.
// Return true if defrag hes been performed, and false if was not needed.
func (db *DB) Defrag() (doing bool) {
	db.mutex.Lock()
	doing = db.idx.needsdefrag
	if doing {
		go func() {
			db.defrag()
			db.mutex.Unlock()
		}()
	} else {
		db.mutex.Unlock()
	}
	return
}


// Disable writing changes to disk.
func (db *DB) NoSync() {
	db.mutex.Lock()
	if !db.nosync {
		db.pending_puts = make(map[KeyType] *oneIdx)
		db.pending_dels = make(map[KeyType] bool)
		db.nosync = true
	}
	db.mutex.Unlock()
}


// Write all the pending changes to disk now.
// Re enable syncing if it has been disabled.
func (db *DB) Sync() {
	db.mutex.Lock()
	if db.nosync {
		go func() {
			db.sync()
			db.mutex.Unlock()
		}()
	} else {
		db.mutex.Unlock()
	}
}


// Close the database.
// Writes all the pending changes to disk.
func (db *DB) Close() {
	db.mutex.Lock()
	db.sync()
	if db.logfile!=nil {
		db.logfile.Close()
		db.logfile = nil
	}
	db.idx.close()
	db.idx = nil
	db.mutex.Unlock()
}


func (db *DB) defrag() {
	bdat := new(bytes.Buffer)
	db.datseq++
	if db.logfile!=nil {
		db.logfile.Close()
		db.logfile = nil
	}
	db.checklogfile()
	used := make(map[uint32]bool)
	db.idx.browse(func(key KeyType, rec *oneIdx) bool {
		if rec.data==nil {
			db.loadrec(rec)
		}
		rec.datpos = uint32(db.addtolog(bdat, key, rec.data))
		rec.datseq = db.datseq
		used[rec.datseq] = true
		return true
	})
	db.logfile.Write(bdat.Bytes())
	db.idx.writedatfile()
	db.idx.db.cleanupold(used)
	db.idx.needsdefrag = false
}


func (db *DB) sync() {
	db.nosync = false
	if len(db.pending_puts) > 0 || len(db.pending_dels) > 0 {
		bidx := new(bytes.Buffer)
		bdat := new(bytes.Buffer)
		db.checklogfile()
		for k, r := range db.pending_puts {
			fpos := db.addtolog(bdat, k, r.data)
			rec := &oneIdx{data:r.data, datpos:uint32(fpos), datlen:uint32(len(r.data)), datseq:db.datseq}
			db.idx.memput(k, rec)
			db.idx.addtolog(bidx, k, rec)
		}
		for k, _ := range db.pending_dels {
			db.idx.deltolog(bidx, k)
		}
		db.logfile.Write(bdat.Bytes())
		db.idx.writebuf(bidx.Bytes())
	}
	db.pending_puts = nil
	db.pending_dels = nil
}
