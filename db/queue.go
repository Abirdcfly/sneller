// Copyright (C) 2022 Sneller, Inc.
// This program is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// This program is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package db

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"path"
	"time"

	"github.com/SnellerInc/sneller/aws/s3"
	"github.com/SnellerInc/sneller/ion/blockfmt"
)

const (
	// DefaultBatchSize is the default queue
	// batching size that is used when none is set.
	DefaultBatchSize = 100 * mega
)

// QueueStatus indicates the processing
// status of a QueueItem.
type QueueStatus int

const (
	// StatusOK indicates that a QueueItem
	// was processed completely.
	StatusOK = iota
	// StatusTryAgain indicates that a QueueItem
	// was not processed, and it should be re-tried
	// shortly.
	StatusTryAgain
	// StatusWriteError indicates taht a QueueItem
	// was not processed, and it should be re-tried
	// after a delay.
	StatusWriteError
)

// QueueItem represents an item
// in a notification queue.
type QueueItem interface {
	// Path should return the full path
	// of the file, including the fs prefix.
	Path() string
	// ETag should return the ETag of
	// the file.
	ETag() string
	// Size should return the size of
	// the file at Path in bytes.
	Size() int64
}

type Queue interface {
	// Close should be called when the
	// runner has finished processing items
	// from the queue (usually due to receiving
	// and external signal to stop processing events).
	// Close should only be called after all events
	// returned by Next have been processed via calls
	// to Finalize.
	io.Closer
	// Next should return the next item
	// in the queue. If the provided pause
	// duration is non-negative, then Next
	// should block for up to the provided duration
	// to produce a new value. If pause is negative,
	// then Next should block until it can return
	// a non-nil QueueItem value or an EOF error.
	// Next should return (nil, io.EOF) is the queue
	// has been closed and no further processing should
	// be performed.
	//
	// As an optimization, the returned QueueItem
	// can implement fs.File, which will obviate
	// the need for the caller to perform additional
	// I/O to produce a file handle associated with
	// the QueueItem.
	Next(pause time.Duration) (QueueItem, error)
	// Finalize is called to return the final status
	// of a QueueItem that was previously returned by
	// ReadInputs. If status is anything other than
	// StatusOK, then the Queue should arrange for the
	// item to be read by a future call to ReadInputs.
	//
	// Finalize may panic if Queue.Close has been called.
	Finalize(item QueueItem, status QueueStatus)
}

// QueueRunner encapsulates the state
// required to process a single queue.
type QueueRunner struct {
	Owner Tenant
	// Conf is the configuration
	// used for building tables.
	Conf Builder

	// Logf is used to log errors encountered
	// while processing entries from a queue.
	// Logf may be nil.
	Logf func(f string, args ...interface{})

	// Open is a hook that can be used to override
	// how queue items are opened.
	// The default behavior is to use ifs.Open(item.Path())
	Open func(ifs InputFS, item QueueItem) (fs.File, error)

	// TableRefresh is the interval at which
	// table definitions are refreshed.
	// If TableRefresh is less than or equal
	// to zero, then tables are refreshed every minute.
	TableRefresh time.Duration

	// BatchSize is the maximum number of bytes
	// that the QueueRunner will attempt to read
	// from a Queue in Run before comitting the
	// returned items. Batches may be smaller than
	// BatchSize due to the expiration of BatchInterval
	// or due to receiving an error from the queue
	// after batching a non-zero number of items.
	// (The size of a batch is computed by summing
	// the QueueItem.Size values from each QueueItem
	// returned from Next.)
	//
	// If BatchSize is less than or equal to zero,
	// then DefaultBatchSize is used instead.
	//
	// See also: BatchInterval
	BatchSize int64

	// BatchInterval is the maximum amount of
	// time the queue should wait for successive
	// calls to Queue.Next to accumulate BatchSize items.
	//
	// See also: BatchSize
	BatchInterval time.Duration

	// IOErrDelay determines how long queue processing
	// will pause if it encounters an I/O error from
	// the backing filesystem.
	IOErrDelay time.Duration

	// scratch space for processing batches
	inputs   []QueueItem
	status   []QueueStatus
	filtered []blockfmt.Input
	indirect []int
}

func (q *QueueRunner) max() int64 {
	if q.BatchSize > 0 {
		return q.BatchSize
	}
	return DefaultBatchSize
}

// Merge returns the more sever status
// of either s or other.
func (s QueueStatus) Merge(other QueueStatus) QueueStatus {
	if s > other {
		return s
	}
	return other
}

func errResult(err error) QueueStatus {
	if err == nil {
		return StatusOK
	} else if err == ErrBuildAgain {
		return StatusTryAgain
	}
	return StatusWriteError
}

func (q *QueueRunner) delay() {
	if q.IOErrDelay > 0 {
		time.Sleep(q.IOErrDelay)
	}
}

// perform the equivalent of infs.Open(name),
// but take care to skip the I/O of the FS implementation
// can just produce a handle directly
func (q *QueueRunner) open(infs InputFS, name string, item QueueItem) (fs.File, error) {
	// an s3-specific optimization: don't do any
	// I/O if we have enough information to produce
	// an s3.File handle already
	if b, ok := infs.(*S3FS); ok {
		f := s3.NewFile(b.Key, b.Bucket, name, item.ETag(), item.Size())
		f.Client = b.Client
		return f, nil
	}
	return infs.Open(name)
}

// populate q.filtered and q.indirect
// from q.inputs based on def.Inputs[*].Pattern
func (q *QueueRunner) filter(bld *Builder, def *Definition) error {
	q.filtered = q.filtered[:0]
	q.indirect = q.indirect[:0]
outer:
	for i := range q.inputs {
		p := q.inputs[i].Path()
		etag := q.inputs[i].ETag()
		for j := range def.Inputs {
			match, err := path.Match(def.Inputs[j].Pattern, p)
			if err != nil || !match {
				continue
			}
			infs, name, err := q.Owner.Split(p)
			if err != nil {
				return err
			}
			f, err := q.open(infs, name, q.inputs[i])
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					q.Logf("ignoring %q (doesn't exist)", name)
					continue outer
				}
				return err
			}
			info, err := f.Stat()
			if err != nil {
				f.Close()
				return err
			}
			gotEtag, err := infs.ETag(name, info)
			if err != nil {
				f.Close()
				return err
			}
			if etag != gotEtag {
				f.Close()
				q.Logf("ignoring %q due to etag mismatch (want %q got %q)", name, etag, gotEtag)
				continue outer
			}
			fm := bld.Format(def.Inputs[j].Format, p)
			err = fm.UseHints(def.Inputs[j].Hints)
			if err != nil {
				return err
			}
			q.indirect = append(q.indirect, i)
			q.filtered = append(q.filtered, blockfmt.Input{
				Path: p,
				ETag: etag,
				Size: info.Size(),
				R:    f,
				F:    fm,
			})
			break
		}
	}
	return nil
}

func (q *QueueRunner) runTable(db string, def *Definition) {
	// clone the config and add features:
	conf := q.Conf
	conf.SetFeatures(def.Features)

	err := q.filter(&q.Conf, def)
	if err == nil && len(q.filtered) > 0 {
		err = conf.Append(q.Owner, db, def.Name, q.filtered)
	}
	if err != nil {
		q.logf("updating %s.%s: %s", db, def.Name, err)
	}
	status := errResult(err)
	for _, j := range q.indirect {
		q.status[j] = q.status[j].Merge(status)
	}
}

func (q *QueueRunner) logf(f string, args ...interface{}) {
	if q.Logf != nil {
		q.Logf(f, args...)
	}
}

func (q *QueueRunner) tableRefresh() time.Duration {
	if q.TableRefresh > 0 {
		return q.TableRefresh
	}
	return time.Minute
}

func bounce(q Queue, lst []QueueItem, st QueueStatus) {
	for i := range lst {
		q.Finalize(lst[i], st)
	}
}

type dbtable struct {
	db, table string
}

// set q.inputs to a list of items
// gathered from the queue using the
// provided batching parameters
func (q *QueueRunner) gather(in Queue) error {
	q.inputs = q.inputs[:0]

	// first item: block forever
	first, err := in.Next(-1)
	if err != nil {
		return err
	}
	if first == nil {
		return fmt.Errorf("Queue implementation bug: Next(-1) should block")
	}
	const hardlimit = 5000
	total := first.Size()
	// keep gathering items up to the max batch size
	// or the max delay time
	q.inputs = append(q.inputs, first)
	end := time.Now().Add(q.BatchInterval)
	for total < q.max() && len(q.inputs) < hardlimit {
		u := time.Until(end)
		if u <= 0 {
			break
		}
		item, err := in.Next(u)
		if err != nil || item == nil {
			break
		}
		q.inputs = append(q.inputs, item)
		total += item.Size()
	}
	return nil
}

// Run processes entries from in until ReadInputs returns io.EOF,
// at which point it will call in.Close.
func (q *QueueRunner) Run(in Queue) error {
	var lastRefresh time.Time
	subdefs := make(map[dbtable]*Definition)
readloop:
	for {
		err := q.gather(in)
		if err != nil {
			cerr := in.Close()
			if err == io.EOF {
				err = cerr
			}
			return err
		}
		if time.Since(lastRefresh) > q.tableRefresh() {
			err := q.updateDefs(subdefs)
			if err != nil {
				q.logf("updating table definitions: %s", err)
				bounce(in, q.inputs, StatusWriteError)
				q.delay()
				continue readloop
			}
		}
		q.runBatches(in, subdefs)
	}
}

func (q *QueueRunner) runBatches(parent Queue, dst map[dbtable]*Definition) {
	if cap(q.status) >= len(q.inputs) {
		q.status = q.status[:len(q.inputs)]
		for i := range q.status {
			q.status[i] = StatusOK
		}
	} else {
		q.status = make([]QueueStatus, len(q.inputs))
	}
	for dbt, def := range dst {
		q.runTable(dbt.db, def)
	}
	for i := range q.status {
		parent.Finalize(q.inputs[i], q.status[i])
	}
}

func (q *QueueRunner) updateDefs(m map[dbtable]*Definition) error {
	dir, err := q.Owner.Root()
	if err != nil {
		return err
	}
	for k := range m {
		delete(m, k)
	}
	dbs, err := fs.ReadDir(dir, "db")
	if err != nil {
		return err
	}
	for i := range dbs {
		dbname := dbs[i].Name()
		curp := path.Join("db", dbname)
		tables, err := fs.ReadDir(dir, curp)
		if err != nil {
			return err
		}
		for j := range tables {
			want := path.Join(curp, tables[j].Name(), "definition.json")
			f, err := dir.Open(want)
			if err != nil {
				// ignore non-existent path
				continue
			}
			def, err := DecodeDefinition(f)
			if err != nil {
				// don't get hung up on invalid definitions
				continue
			}
			m[dbtable{db: dbname, table: tables[j].Name()}] = def
		}
	}
	return nil
}
