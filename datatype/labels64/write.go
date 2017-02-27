package labels64

import (
	"fmt"
	"io"
	"log"
	"sync"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/common/labels"
	"github.com/janelia-flyem/dvid/datatype/imageblk"
	"github.com/janelia-flyem/dvid/dvid"
	"github.com/janelia-flyem/dvid/server"
	"github.com/janelia-flyem/dvid/storage"
)

type putOperation struct {
	voxels   *imageblk.Voxels
	indexZYX dvid.IndexZYX
	version  dvid.VersionID
	mutate   bool   // if false, we just ingest without needing to GET previous value
	mutID    uint64 // should be unique within a server's uptime.
	indexCh  chan blockChange
}

// IngestVoxels ingests voxels from a subvolume into the storage engine.
// The subvolume must be aligned to blocks of the data instance, which simplifies
// the routine since we are simply replacing a value instead of modifying values (GET + PUT).
func (d *Data) IngestVoxels(v dvid.VersionID, vox *imageblk.Voxels, roiname dvid.InstanceName) error {
	return d.PutVoxels(v, vox, roiname, false)
}

// MutateVoxels mutates voxels from a subvolume into the storage engine.  This differs from
// the IngestVoxels function in firing off a MutateBlockEvent instead of an IngestBlockEvent,
// which tells subscribers that a previous value has changed instead of a completely new
// key/value being inserted.  There will be some decreased performance due to cleanup of prior
// denormalizations compared to IngestVoxels.
func (d *Data) MutateVoxels(v dvid.VersionID, vox *imageblk.Voxels, roiname dvid.InstanceName) error {
	return d.PutVoxels(v, vox, roiname, true)
}

// PutVoxels persists voxels from a subvolume into the storage engine.
// The subvolume must be aligned to blocks of the data instance, which simplifies
// the routine if the PUT is a mutation (signals MutateBlockEvent) instead of ingestion.
func (d *Data) PutVoxels(v dvid.VersionID, vox *imageblk.Voxels, roiname dvid.InstanceName, mutate bool) error {
	r, err := imageblk.GetROI(v, roiname, vox)
	if err != nil {
		return err
	}

	// Make sure vox is block-aligned
	if !dvid.BlockAligned(vox, d.BlockSize()) {
		return fmt.Errorf("cannot store voxels in non-block aligned geometry %s -> %s", vox.StartPoint(), vox.EndPoint())
	}

	wg := new(sync.WaitGroup)

	// Only do one request at a time, although each request can start many goroutines.
	server.SpawnGoroutineMutex.Lock()
	defer server.SpawnGoroutineMutex.Unlock()

	// Keep track of changing extents and mark repo as dirty if changed.
	var extentChanged bool
	defer func() {
		if extentChanged {
			err := datastore.SaveDataByVersion(v, d)
			if err != nil {
				dvid.Infof("Error in trying to save repo on change: %v\n", err)
			}
		}
	}()

	// Track point extents
	extents := d.Extents()
	if extents.AdjustPoints(vox.StartPoint(), vox.EndPoint()) {
		extentChanged = true
	}

	// extract buffer interface if it exists
	var putbuffer storage.RequestBuffer
	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return fmt.Errorf("Data type imageblk had error initializing store: %v\n", err)
	}
	if req, ok := store.(storage.KeyValueRequester); ok {
		ctx := datastore.NewVersionedCtx(d, v)
		putbuffer = req.NewBuffer(ctx)
	}

	// Iterate through index space for this data.
	mutID := d.NewMutationID()
	fmt.Printf("Starting PutVoxels, mutation %d\n", mutID)

	blockCh := make(chan blockChange, 100)
	go d.aggregateBlockChanges(v, blockCh)

	blocks := 0
	for it, err := vox.NewIndexIterator(d.BlockSize()); err == nil && it.Valid(); it.NextSpan() {
		i0, i1, err := it.IndexSpan()
		if err != nil {
			return err
		}
		ptBeg := i0.Duplicate().(dvid.ChunkIndexer)
		ptEnd := i1.Duplicate().(dvid.ChunkIndexer)

		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)

		if extents.AdjustIndices(ptBeg, ptEnd) {
			extentChanged = true
		}

		wg.Add(int(endX-begX) + 1)
		c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
		for x := begX; x <= endX; x++ {
			c[0] = x
			curIndex := dvid.IndexZYX(c)

			// Don't PUT if this index is outside a specified ROI
			if r != nil && r.Iter != nil && !r.Iter.InsideFast(curIndex) {
				wg.Done()
				continue
			}

			kv := &storage.TKeyValue{K: NewBlockTKey(&curIndex)}
			putOp := &putOperation{vox, curIndex, v, mutate, mutID, blockCh}
			op := &storage.ChunkOp{putOp, wg}
			d.PutChunk(&storage.Chunk{op, kv}, putbuffer)
			blocks++
		}
	}
	wg.Wait()
	fmt.Printf("Done with PutVoxels %d block-level ops, mutation %d\n", blocks, mutID)
	close(blockCh)

	// if a bufferable op, flush
	if putbuffer != nil {
		putbuffer.Flush()
	}

	// Let any synced downres instance that we've completed block-level ops.
	d.publishDownresCommit(v, mutID)

	return nil
}

// PutBlocks stores blocks of data in a span along X
func (d *Data) PutBlocks(v dvid.VersionID, mutID uint64, start dvid.ChunkPoint3d, span int, data io.ReadCloser, mutate bool) error {
	batcher, err := d.GetKeyValueBatcher()
	if err != nil {
		return err
	}

	ctx := datastore.NewVersionedCtx(d, v)
	batch := batcher.NewBatch(ctx)

	// Read blocks from the stream until we can output a batch put.
	const BatchSize = 1000
	var readBlocks int
	numBlockBytes := d.BlockSize().Prod()
	chunkPt := start
	buf := make([]byte, numBlockBytes)
	for {
		// Read a block's worth of data
		readBytes := int64(0)
		for {
			n, err := data.Read(buf[readBytes:])
			readBytes += int64(n)
			if readBytes == numBlockBytes {
				break
			}
			if err == io.EOF {
				return fmt.Errorf("Block data ceased before all block data read")
			}
			if err != nil {
				return fmt.Errorf("Error reading blocks: %v\n", err)
			}
		}

		if readBytes != numBlockBytes {
			return fmt.Errorf("Expected %d bytes in block read, got %d instead!  Aborting.", numBlockBytes, readBytes)
		}

		serialization, err := dvid.SerializeData(buf, d.Compression(), d.Checksum())
		if err != nil {
			return err
		}
		zyx := dvid.IndexZYX(chunkPt)
		tk := NewBlockTKey(&zyx)

		// If we are mutating, get the previous block of data.
		var oldBlock []byte
		if mutate {
			oldBlock, err = d.getLabelBlock(v, tk)
			if err != nil {
				return fmt.Errorf("Unable to load previous block in %q, key %v: %v\n", d.DataName(), tk, err)
			}
		}

		// Write the new block
		batch.Put(tk, serialization)

		// Notify any subscribers that you've changed block.
		var event string
		var delta interface{}
		if mutate {
			event = labels.MutateBlockEvent
			delta = imageblk.MutatedBlock{&zyx, oldBlock, buf, mutID}
		} else {
			event = labels.IngestBlockEvent
			delta = imageblk.Block{&zyx, buf, mutID}
		}
		evt := datastore.SyncEvent{d.DataUUID(), event}
		msg := datastore.SyncMessage{event, v, delta}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			return err
		}

		// Advance to next block
		chunkPt[0]++
		readBlocks++
		finish := (readBlocks == span)
		if finish || readBlocks%BatchSize == 0 {
			if err := batch.Commit(); err != nil {
				return fmt.Errorf("Error on batch commit, block %d: %v\n", readBlocks, err)
			}
			batch = batcher.NewBatch(ctx)
		}
		if finish {
			break
		}
	}
	return nil
}

// getLabelBlock returns a block of label data from this instance's preferred storage.
func (d *Data) getLabelBlock(v dvid.VersionID, k storage.TKey) ([]byte, error) {
	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return nil, fmt.Errorf("Data type imageblk had error initializing store: %v\n", err)
	}

	ctx := datastore.NewVersionedCtx(d, v)
	serialization, err := store.Get(ctx, k)
	if err != nil {
		return nil, err
	}
	compressed, _, err := dvid.DeserializeData(serialization, true)
	if err != nil {
		return nil, fmt.Errorf("Unable to deserialize block, %s: %v", ctx, err)
	}
	return labels.Decompress(compressed, d.BlockSize())
}

// PutChunk puts a chunk of data as part of a mapped operation.
// Only some multiple of the # of CPU cores can be used for chunk handling before
// it waits for chunk processing to abate via the buffered server.HandlerToken channel.
func (d *Data) PutChunk(chunk *storage.Chunk, putbuffer storage.RequestBuffer) error {
	<-server.HandlerToken
	go d.putChunk(chunk, putbuffer)
	return nil
}

func (d *Data) putChunk(chunk *storage.Chunk, putbuffer storage.RequestBuffer) {
	defer func() {
		// After processing a chunk, return the token.
		server.HandlerToken <- 1

		// Notify the requestor that this chunk is done.
		if chunk.Wg != nil {
			chunk.Wg.Done()
		}
	}()

	op, ok := chunk.Op.(*putOperation)
	if !ok {
		log.Fatalf("Illegal operation passed to ProcessChunk() for data %s\n", d.DataName())
	}

	// Make sure our received chunk is valid.
	if chunk == nil {
		dvid.Errorf("Received nil chunk in ProcessChunk.  Ignoring chunk.\n")
		return
	}
	if chunk.K == nil {
		dvid.Errorf("Received nil chunk key in ProcessChunk.  Ignoring chunk.\n")
		return
	}

	// Initialize the block buffer using the chunk of data.  For voxels, this chunk of
	// data needs to be uncompressed and deserialized.
	var blockData []byte
	var err error
	if chunk.V == nil {
		blockData = d.BackgroundBlock()
	} else {
		var compressed []byte
		compressed, _, err = dvid.DeserializeData(chunk.V, true)
		if err != nil {
			dvid.Errorf("Unable to deserialize block in %q: %v\n", d.DataName(), err)
			return
		}
		blockData, err = labels.Decompress(compressed, d.BlockSize())
		if err != nil {
			dvid.Errorf("Unable to decompress google compression in %q: %v\n", d.DataName(), err)
			return
		}
	}

	// If we are mutating, get the previous block of data.
	var oldBlock []byte
	if op.mutate {
		oldBlock, err = d.getLabelBlock(op.version, chunk.K)
		if err != nil {
			dvid.Errorf("Unable to load previous block in %q, key %v: %v\n", d.DataName(), chunk.K, err)
			return
		}
	}

	// Perform the operation.
	block := &storage.TKeyValue{K: chunk.K, V: blockData}
	if err = op.voxels.WriteBlock(block, d.BlockSize()); err != nil {
		dvid.Errorf("Unable to WriteBlock() in %q: %v\n", d.DataName(), err)
		return
	}
	compressed, err := labels.Compress(blockData, d.BlockSize())
	if err != nil {
		dvid.Errorf("Unable to google compress block in %q: %v\n", d.DataName(), err)
		return
	}
	serialization, err := dvid.SerializeData(compressed, d.Compression(), d.Checksum())
	if err != nil {
		dvid.Errorf("Unable to serialize block in %q: %v\n", d.DataName(), err)
		return
	}

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		dvid.Errorf("Data type imageblk had error initializing store: %v\n", err)
		return
	}

	ctx := datastore.NewVersionedCtx(d, op.version)
	callback := func(ready chan error) {
		if ready != nil {
			if resperr := <-ready; resperr != nil {
				dvid.Errorf("Unable to PUT voxel data for key %v: %v\n", chunk.K, resperr)
				return
			}
		}
		var event string
		var delta interface{}
		if op.mutate {
			event = labels.MutateBlockEvent
			block := imageblk.MutatedBlock{&op.indexZYX, oldBlock, block.V, op.mutID}
			d.handleIndexBlockMutate(op.indexCh, block)
			delta = block
		} else {
			event = labels.IngestBlockEvent
			block := imageblk.Block{&op.indexZYX, block.V, op.mutID}
			d.handleIndexBlockIngest(op.indexCh, block)
			delta = block
		}
		evt := datastore.SyncEvent{d.DataUUID(), event}
		msg := datastore.SyncMessage{event, op.version, delta}
		if err := datastore.NotifySubscribers(evt, msg); err != nil {
			dvid.Errorf("Unable to notify subscribers of event %s in %s\n", event, d.DataName())
		}
	}
	// put data -- use buffer if available
	if putbuffer != nil {
		ready := make(chan error, 1)
		go callback(ready)
		putbuffer.PutCallback(ctx, chunk.K, serialization, ready)
	} else {
		if err := store.Put(ctx, chunk.K, serialization); err != nil {
			dvid.Errorf("Unable to PUT voxel data for key %v: %v\n", chunk.K, err)
			return
		}
		callback(nil)
	}
}

// Writes a XY image into the blocks that intersect it.  This function assumes the
// blocks have been allocated and if necessary, filled with old data.
func (d *Data) writeXYImage(v dvid.VersionID, vox *imageblk.Voxels, b storage.TKeyValues) (extentChanged bool, err error) {

	// Setup concurrency in image -> block transfers.
	var wg sync.WaitGroup
	defer wg.Wait()

	// Iterate through index space for this data using ZYX ordering.
	blockSize := d.BlockSize()
	var startingBlock int32

	for it, err := vox.NewIndexIterator(blockSize); err == nil && it.Valid(); it.NextSpan() {
		indexBeg, indexEnd, err := it.IndexSpan()
		if err != nil {
			return extentChanged, err
		}

		ptBeg := indexBeg.Duplicate().(dvid.ChunkIndexer)
		ptEnd := indexEnd.Duplicate().(dvid.ChunkIndexer)

		// Track point extents
		if d.Extents().AdjustIndices(ptBeg, ptEnd) {
			extentChanged = true
		}

		// Do image -> block transfers in concurrent goroutines.
		begX := ptBeg.Value(0)
		endX := ptEnd.Value(0)

		<-server.HandlerToken
		wg.Add(1)
		go func(blockNum int32) {
			c := dvid.ChunkPoint3d{begX, ptBeg.Value(1), ptBeg.Value(2)}
			for x := begX; x <= endX; x++ {
				c[0] = x
				curIndex := dvid.IndexZYX(c)
				b[blockNum].K = NewBlockTKey(&curIndex)

				// Write this slice data into the block.
				vox.WriteBlock(&(b[blockNum]), blockSize)
				blockNum++
			}
			server.HandlerToken <- 1
			wg.Done()
		}(startingBlock)

		startingBlock += (endX - begX + 1)
	}
	return
}

// KVWriteSize is the # of key-value pairs we will write as one atomic batch write.
const KVWriteSize = 500

// TODO -- Clean up all the writing and simplify now that we have block-aligned writes.
// writeBlocks ingests blocks of voxel data asynchronously using batch writes.
func (d *Data) writeBlocks(v dvid.VersionID, b storage.TKeyValues, wg1, wg2 *sync.WaitGroup) error {
	batcher, err := d.GetKeyValueBatcher()
	if err != nil {
		return err
	}

	preCompress, postCompress := 0, 0

	ctx := datastore.NewVersionedCtx(d, v)
	evt := datastore.SyncEvent{d.DataUUID(), labels.IngestBlockEvent}

	<-server.HandlerToken
	go func() {
		defer func() {
			wg1.Done()
			wg2.Done()
			dvid.Debugf("Wrote voxel blocks.  Before %s: %d bytes.  After: %d bytes\n", d.Compression(), preCompress, postCompress)
			server.HandlerToken <- 1
		}()

		mutID := d.NewMutationID()
		batch := batcher.NewBatch(ctx)
		for i, block := range b {
			preCompress += len(block.V)
			compressed, err := labels.Compress(block.V, d.BlockSize())
			if err != nil {
				dvid.Errorf("Unable to google compress block in %q: %v\n", d.DataName(), err)
				return
			}
			serialization, err := dvid.SerializeData(compressed, d.Compression(), d.Checksum())
			if err != nil {
				dvid.Errorf("Unable to serialize block in %q: %v\n", d.DataName(), err)
				return
			}
			postCompress += len(serialization)
			batch.Put(block.K, serialization)

			indexZYX, err := DecodeBlockTKey(block.K)
			if err != nil {
				dvid.Errorf("Unable to recover index from block key: %v\n", block.K)
				return
			}
			msg := datastore.SyncMessage{labels.IngestBlockEvent, v, imageblk.Block{indexZYX, block.V, mutID}}
			if err := datastore.NotifySubscribers(evt, msg); err != nil {
				dvid.Errorf("Unable to notify subscribers of ChangeBlockEvent in %s\n", d.DataName())
				return
			}

			// Check if we should commit
			if i%KVWriteSize == KVWriteSize-1 {
				if err := batch.Commit(); err != nil {
					dvid.Errorf("Error on trying to write batch: %v\n", err)
					return
				}
				batch = batcher.NewBatch(ctx)
			}
		}
		if err := batch.Commit(); err != nil {
			dvid.Errorf("Error on trying to write batch: %v\n", err)
			return
		}
	}()
	return nil
}
