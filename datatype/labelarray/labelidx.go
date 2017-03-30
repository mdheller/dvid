package labelarray

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/janelia-flyem/dvid/datastore"
	"github.com/janelia-flyem/dvid/datatype/common/labels"
	"github.com/janelia-flyem/dvid/dvid"

	lz4 "github.com/janelia-flyem/go/golz4"
)

// Meta key-value pairs are sharded by label although they can be mutated by any
// mutation at the label block level.  Since we could have concurrency at the block level,
// we need to enforce block indices mutation at the label level.  This was not necessary
// in labelvol datatype because RLEs were kept at a block level as well.  Here, we are
// aggregating all changes by label.  Send mods of form (label, delta voxels, block, action)
// where action is add or delete block from the label index.

// TODO: Make sure # voxels is correct even if people ingest multiple times, i.e., make it
// an idempotent op.  Or mutate should be standard so there is no ingest.

// Meta gives a high-level overview of all voxels in a label including the # voxels,
// block index.
type Meta struct {
	Voxels uint64         // Total # of voxels in label.
	Blocks dvid.IZYXSlice // Sorted block coordinates occupied by label.

	// MinBlock dvid.ChunkPoint3d // Minimum block coordinate for label.
	// MaxBlock dvid.ChunkPoint3d // Maximum block coordinate for label.

	// BlockVoxels []uint32       // Number of voxels for each block in Blocks, respectively.
}

// MarshalBinary implements the encoding.BinaryMarshaler interface
func (m Meta) MarshalBinary() ([]byte, error) {
	buf := make([]byte, len(m.Blocks)*12+8)
	binary.LittleEndian.PutUint64(buf[0:8], m.Voxels)
	off := 8
	for _, izyx := range m.Blocks {
		copy(buf[off:off+12], string(izyx))
		off += 12
	}
	return buf, nil
}

// UnmarshalBinary implements the encoding.BinaryUnmarshaler interface.
func (m *Meta) UnmarshalBinary(b []byte) error {
	if len(b) < 8 {
		return fmt.Errorf("cannot unmarshal %d bytes into Meta", len(b))
	}
	m.Voxels = binary.LittleEndian.Uint64(b[0:8])
	return m.Blocks.UnmarshalBinary(b[8:])
}

type timedMeta struct {
	label uint64
	meta  *Meta
	t     time.Time
}

type cacheList []*timedMeta

func (c cacheList) Len() int           { return len(c) }
func (c cacheList) Swap(a, b int)      { c[a], c[b] = c[b], c[a] }
func (c cacheList) Less(a, b int) bool { return c[a].t.Before(c[b].t) }

// MetaCache is a label Meta cache based on time since last access.
type MetaCache struct {
	size  uint16
	avail map[uint64]*timedMeta
	list  cacheList
}

const defaultCacheSize = 50

func MakeMetaCache(size uint16) *MetaCache {
	if size == 0 {
		size = defaultCacheSize
	}
	m := new(MetaCache)
	m.size = size
	m.list = make(cacheList, 0, size)
	m.avail = make(map[uint64]*timedMeta, size)
	return m
}

// GetLabelMeta returns a label's Meta if in the cache, else returns nil.
func (m MetaCache) GetLabelMeta(label uint64) *Meta {
	tm, found := m.avail[label]
	if found {
		tm.t = time.Now()
		return tm.meta
	}
	return nil
}

// AddLabelMeta adds a label's Meta to the cache, possibly evicting older entries.
func (m MetaCache) AddLabelMeta(label uint64, meta *Meta) {
	tm, found := m.avail[label]
	if found {
		tm.meta = meta // could be updated Meta
		tm.t = time.Now()
		return
	}
	newtm := new(timedMeta)
	newtm.label = label
	newtm.meta = meta
	newtm.t = time.Now()

	if len(m.list) == int(m.size) {
		sort.Sort(m.list)
		evicted := m.list[m.size-1]
		delete(m.avail, evicted.label)

		m.list[m.size-1] = newtm
	} else {
		m.list = append(m.list, newtm)
	}
}

// change the block presence map
func (m *Meta) applyChanges(bdm blockDiffMap) error {
	var present dvid.IZYXSlice
	var absent dvid.IZYXSlice
	for block, diff := range bdm {
		m.Voxels = uint64(int64(m.Voxels) + int64(diff.delta))
		if diff.present {
			present = append(present, block)
		} else {
			absent = append(absent, block)
		}
	}
	if len(present) > 1 {
		sort.Sort(present)
	}
	if len(absent) > 1 {
		sort.Sort(absent)
	}

	m.Blocks.Delete(absent)
	m.Blocks.Merge(present)
	return nil
}

type sld struct { // sortable labelDiff
	block dvid.IZYXString
	labelDiff
}

type slds []sld

func (s slds) Len() int {
	return len(s)
}

func (s slds) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

func (s slds) Less(i, j int) bool {
	return s[i].block < s[j].block
}

type labelDiff struct {
	delta   int32 // change in # voxels
	present bool
}
type blockDiffMap map[dvid.IZYXString]labelDiff

type labelDiffMap map[uint64]blockDiffMap

const presentOld = uint8(0x01) // bit flag for label presence in old block
const presentNew = uint8(0x02) // bit flag for label presence in new block

// block-level analysis of mutation to get label changes in a block.  accumulates data for
// a given mutation into a map per mutation which will then be flushed for each label meta
// k/v pair at end of mutation.
func (d *Data) handleIndexBlockMutate(ch chan blockChange, mut MutatedBlock) {
	bc := blockChange{
		block:   mut.Index.ToIZYXString(),
		present: make(map[uint64]uint8),
		delta:   make(map[uint64]int32),
	}
	for _, label := range mut.Prev.Labels {
		bc.present[label] |= presentOld
	}
	for _, label := range mut.Data.Labels {
		bc.present[label] |= presentNew
	}
	ch <- bc
}

// block-level analysis of label ingest
func (d *Data) handleIndexBlockIngest(ch chan blockChange, mut IngestedBlock) {
	bc := blockChange{
		block:   mut.Index.ToIZYXString(),
		present: make(map[uint64]uint8),
		delta:   make(map[uint64]int32),
	}
	for _, label := range mut.Data.Labels {
		bc.present[label] |= presentNew
	}
	ch <- bc
}

type blockChange struct {
	block   dvid.IZYXString
	present map[uint64]uint8
	delta   map[uint64]int32
}

// goroutine(s) that accepts label change data for a block, then consolidates it and writes label
// indexing.
func (d *Data) aggregateBlockChanges(v dvid.VersionID, ch <-chan blockChange) {
	ldm := make(map[uint64]blockDiffMap)
	for change := range ch {
		for label, flag := range change.present {
			bdm, found := ldm[label]
			if !found {
				bdm = make(blockDiffMap)
				ldm[label] = bdm
			}
			var diff labelDiff
			switch flag {
			case 0x01: // we no longer have this label in the block
				diff.present = false
				bdm[change.block] = diff
			case 0x02: // this is a new label in this block
				diff.present = true
				bdm[change.block] = diff
			case 0x03: // no change
				diff.present = true
				bdm[change.block] = diff
			}
		}
		for label, delta := range change.delta {
			bdm, found := ldm[label]
			if !found {
				bdm = make(blockDiffMap)
				ldm[label] = bdm
			}
			diff := bdm[change.block]
			diff.delta += delta
		}
	}
	for label, bdm := range ldm {
		change := labelChange{v, label, bdm}
		shard := label % numLabelHandlers
		d.indexCh[shard] <- change
	}
}

type labelChange struct {
	v     dvid.VersionID
	label uint64
	bdm   blockDiffMap
}

// goroutines (n = numLabelHandlers) spawned during startup to handle all get/put tx on label indexes,
// since these txs need to be across all concurrent requests so we shard label id to a particular goroutine.
// This also allows us to efficiently cache the last N label Meta
func (d *Data) indexLabels(ch <-chan labelChange) {
	var err error
	cache := MakeMetaCache(100)
	for change := range ch {
		ctx := datastore.NewVersionedCtx(d, change.v)

		meta := cache.GetLabelMeta(change.label)
		if meta == nil {
			meta, err = d.GetLabelMeta(ctx, labels.NewSet(change.label), dvid.Bounds{})
			if err != nil {
				dvid.Criticalf("Error trying to read label %d meta for data %q: %v\n", change.label, d.DataName(), err)
				continue
			}
		}
		if err := meta.applyChanges(change.bdm); err != nil {
			dvid.Criticalf("Error on applying mutation changes to label %d meta: %v\n", change.label, err)
			continue
		}
		cache.AddLabelMeta(change.label, meta)

		if err := d.PutLabelMeta(ctx, change.label, meta); err != nil {
			dvid.Criticalf("Error trying to store indexing for label %d, data %q: %v\n", change.label, d.DataName(), err)
			continue
		}
	}
	dvid.Infof("Closing index handler for data %q...\n", d.DataName())
}

type labelBlock struct {
	index dvid.IZYXString
	data  []byte
}

type rleResult struct {
	runs          uint32
	serialization []byte
}

// goroutine to process retrieved label data and generate RLEs, could be sharded by block coordinate
func (d *Data) processBlocksToRLEs(lbls labels.Set, bounds dvid.Bounds, in chan labelBlock, out chan rleResult) {
	for {
		lb, more := <-in
		if !more {
			return
		}
		var result rleResult
		data, _, err := dvid.DeserializeData(lb.data, true)
		if err != nil {
			dvid.Errorf("could not deserialize %d bytes in block %s: %v\n", len(lb.data), lb.index, err)
			out <- result
			continue
		}
		var block labels.Block
		if err := block.UnmarshalBinary(data); err != nil {
			dvid.Errorf("unable to unmarshal label block %s: %v\n", lb.index, err)
		}
		blockData, _, err := block.MakeLabelVolume()
		if err != nil {
			dvid.Errorf("Unable to make label volume from block %s in %q: %v\n", lb.index, d.DataName(), err)
			return
		}
		var newRuns uint32
		var serialization []byte
		if bounds.Exact && bounds.Voxel.IsSet() {
			serialization, newRuns, err = d.addBoundedRLEs(lb.index, blockData, lbls, bounds.Voxel)
		} else {
			serialization, newRuns, err = d.addRLEs(lb.index, blockData, lbls)
		}
		if err != nil {
			dvid.Errorf("could not process %d bytes in block %s to create RLEs: %v\n", len(blockData), lb.index, err)
		} else {
			result = rleResult{runs: newRuns, serialization: serialization}
		}
		out <- result
	}
}

func writeRLE(w io.Writer, start dvid.Point3d, run int32) error {
	rle := dvid.NewRLE(start, run)
	serialization, err := rle.MarshalBinary()
	if err != nil {
		return err
	}
	if _, err := w.Write(serialization); err != nil {
		return err
	}
	return nil
}

// Scan a block and construct RLEs that will be serialized and added to the given buffer.
func (d *Data) addRLEs(izyx dvid.IZYXString, data []byte, lbls labels.Set) (serialization []byte, newRuns uint32, err error) {
	if len(data) != int(d.BlockSize().Prod())*8 {
		err = fmt.Errorf("Deserialized label block %d bytes, not uint64 size times %d block elements\n",
			len(data), d.BlockSize().Prod())
		return
	}
	var indexZYX dvid.IndexZYX
	indexZYX, err = izyx.IndexZYX()
	if err != nil {
		return
	}
	firstPt := indexZYX.MinPoint(d.BlockSize())
	lastPt := indexZYX.MaxPoint(d.BlockSize())

	var label uint64
	var spanStart dvid.Point3d
	var z, y, x, spanRun int32
	start := 0
	buf := new(bytes.Buffer)
	for z = firstPt.Value(2); z <= lastPt.Value(2); z++ {
		for y = firstPt.Value(1); y <= lastPt.Value(1); y++ {
			for x = firstPt.Value(0); x <= lastPt.Value(0); x++ {
				label = binary.LittleEndian.Uint64(data[start : start+8])
				start += 8

				// If we are in labels of interest, start or extend run.
				inSpan := false
				if label != 0 {
					_, inSpan = lbls[label]
				}
				if inSpan {
					spanRun++
					if spanRun == 1 {
						spanStart = dvid.Point3d{x, y, z}
					}
				} else {
					if spanRun > 0 {
						newRuns++
						if err = writeRLE(buf, spanStart, spanRun); err != nil {
							return
						}
					}
					spanRun = 0
				}
			}
			// Force break of any runs when we finish x scan.
			if spanRun > 0 {
				if err = writeRLE(buf, spanStart, spanRun); err != nil {
					return
				}
				newRuns++
				spanRun = 0
			}
		}
	}
	serialization = buf.Bytes()
	return
}

// Scan a block and construct bounded RLEs that will be serialized and added to the given buffer.
func (d *Data) addBoundedRLEs(izyx dvid.IZYXString, data []byte, lbls labels.Set, bounds *dvid.OptionalBounds) (serialization []byte, newRuns uint32, err error) {
	if len(data) != int(d.BlockSize().Prod())*8 {
		err = fmt.Errorf("Deserialized label block %d bytes, not uint64 size times %d block elements\n",
			len(data), d.BlockSize().Prod())
		return
	}
	var indexZYX dvid.IndexZYX
	indexZYX, err = izyx.IndexZYX()
	if err != nil {
		return
	}
	firstPt := indexZYX.MinPoint(d.BlockSize())
	lastPt := indexZYX.MaxPoint(d.BlockSize())

	var label uint64
	var spanStart dvid.Point3d
	var z, y, x, spanRun int32
	start := 0
	buf := new(bytes.Buffer)
	yskip := int(d.BlockSize().Value(0) * 8)
	zskip := int(d.BlockSize().Value(1)) * yskip
	for z = firstPt.Value(2); z <= lastPt.Value(2); z++ {
		if bounds.OutsideZ(z) {
			start += zskip
			continue
		}
		for y = firstPt.Value(1); y <= lastPt.Value(1); y++ {
			if bounds.OutsideY(y) {
				start += yskip
				continue
			}
			for x = firstPt.Value(0); x <= lastPt.Value(0); x++ {
				label = binary.LittleEndian.Uint64(data[start : start+8])
				start += 8

				// If we are in labels of interest, start or extend run.
				inSpan := false
				if label != 0 {
					_, inSpan = lbls[label]
					if inSpan && bounds.OutsideX(x) {
						inSpan = false
					}
				}
				if inSpan {
					spanRun++
					if spanRun == 1 {
						spanStart = dvid.Point3d{x, y, z}
					}
				} else {
					if spanRun > 0 {
						newRuns++
						if err = writeRLE(buf, spanStart, spanRun); err != nil {
							return
						}
					}
					spanRun = 0
				}
			}
			// Force break of any runs when we finish x scan.
			if spanRun > 0 {
				if err = writeRLE(buf, spanStart, spanRun); err != nil {
					return
				}
				newRuns++
				spanRun = 0
			}
		}
	}
	serialization = buf.Bytes()
	return
}

// FoundSparseVol returns true if a sparse volume is found for the given label
// within the given bounds.
func (d *Data) FoundSparseVol(ctx *datastore.VersionedCtx, label uint64, bounds dvid.Bounds) (bool, error) {
	// Scan through all keys coming from label blocks to see if we have any hits.
	var constituents labels.Set
	mapping := labels.LabelMap(ctx.InstanceVersion())

	if mapping == nil {
		constituents = labels.Set{label: struct{}{}}
	} else {
		// Check if this label has been merged.
		if _, found := mapping.Get(label); found {
			return false, nil
		}

		// If not, see if labels have been merged into it.
		constituents = mapping.ConstituentLabels(label)
	}

	// See if any constituent label is within bounds.
	store, err := d.GetKeyValueDB()
	if err != nil {
		return false, err
	}
	for label := range constituents {
		val, err := store.Get(ctx, NewLabelIndexTKey(label))
		if err != nil {
			return false, err
		}
		if len(val) == 0 {
			continue
		}
		if !bounds.Block.IsSet() && len(val) != 0 {
			return true, nil
		}
		// Check bounds if one was supplied.
		var meta Meta
		if err := meta.UnmarshalBinary(val); err != nil {
			return false, err
		}
		if bounds.Block.IsSet() {
			for _, izyx := range meta.Blocks {
				chunkPt, err := izyx.ToChunkPoint3d()
				if err != nil {
					return false, err
				}
				if !(bounds.Block.OutsideX(chunkPt[0]) || bounds.Block.OutsideY(chunkPt[1]) || bounds.Block.OutsideZ(chunkPt[2])) {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

// GetMappedLabelMeta returns a sorted list of ZYX blocks that contain the given label,
// including all labels that could be undergoing merge into that label.
// If block bounds are set, the number of voxels is unknown and set to zero.
func (d *Data) GetMappedLabelMeta(ctx *datastore.VersionedCtx, label uint64, bounds dvid.Bounds) (meta *Meta, lbls labels.Set, err error) {
	mapping := labels.LabelMap(ctx.InstanceVersion())
	if mapping != nil {
		// Check if this label has been merged.
		if mapped, found := mapping.FinalLabel(label); found {
			dvid.Debugf("Label %d has already been merged into label %d.  Skipping sparse vol retrieval.\n", label, mapped)
			return
		}
	}

	// Get set of all labels that have been merged to this given label.
	if mapping == nil {
		lbls = labels.Set{label: struct{}{}}
	} else {
		lbls = mapping.ConstituentLabels(label)
	}

	// Get the block indices for the set of labels.
	meta, err = d.GetLabelMeta(ctx, lbls, bounds)
	return
}

// GetLabelMeta returns a sorted list of ZYX blocks that contain the given labels.
// If block bounds are set, the number of voxels is unknown and set to zero.
func (d *Data) GetLabelMeta(ctx *datastore.VersionedCtx, lbls labels.Set, bounds dvid.Bounds) (*Meta, error) {
	store, err := d.GetKeyValueDB()
	if err != nil {
		return nil, err
	}
	// dvid.Infof("GetLabelMeta for labels %s...\n", lbls)
	var voxels uint64
	var blocks dvid.IZYXSlice
	for label := range lbls {
		compressed, err := store.Get(ctx, NewLabelIndexTKey(label))
		if err != nil {
			return nil, err
		}
		val, _, err := dvid.DeserializeData(compressed, true)
		if err != nil {
			return nil, err
		}

		var meta Meta
		if len(val) != 0 {
			if err := meta.UnmarshalBinary(val); err != nil {
				return nil, err
			}
			// dvid.Infof("retrieved Meta for label %d: %d blocks, %d voxels\n", label, len(meta.Blocks), meta.Voxels)
			if len(lbls) == 1 {
				blocks = meta.Blocks
				voxels = meta.Voxels
			} else if len(meta.Blocks) > 0 {
				voxels += meta.Voxels
				blocks.Merge(meta.Blocks)
			}
		}
	}

	if bounds.Block != nil && bounds.Block.IsSet() {
		blocks, err = blocks.FitToBounds(bounds.Block)
		if err != nil {
			return nil, err
		}
		voxels = 0
	}
	meta := Meta{Voxels: voxels, Blocks: blocks}
	// dvid.Infof("Returning Meta for labels %s: %d blocks, %d voxels\n", lbls, len(meta.Blocks), meta.Voxels)
	return &meta, nil
}

func (d *Data) PutLabelMeta(ctx *datastore.VersionedCtx, label uint64, meta *Meta) error {
	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return fmt.Errorf("Data %q PutLabelMeta had error initializing store: %v\n", d.DataName(), err)
	}

	tk := NewLabelIndexTKey(label)
	serialization, err := meta.MarshalBinary()
	if err != nil {
		return fmt.Errorf("Error trying to serialize meta for label %d, data %q: %v", label, d.DataName(), err)
	}
	compressFormat, _ := dvid.NewCompression(dvid.LZ4, dvid.DefaultCompression)
	compressed, err := dvid.SerializeData(serialization, compressFormat, dvid.NoChecksum)
	if err != nil {
		return fmt.Errorf("Error trying to LZ4 compress label %d indexing in data %q\n", label, d.DataName())
	}
	if err := store.Put(ctx, tk, compressed); err != nil {
		return fmt.Errorf("Unable to store indices for label %d, data %s: %v\n", label, d.DataName(), err)
	}
	return nil
}

// WriteStreamingRLE does a streaming write of an encoded sparse volume given a label.
// It returns a bool whether the label was found in the given bounds and any error.
func (d *Data) WriteStreamingRLE(ctx *datastore.VersionedCtx, label uint64, bounds dvid.Bounds, compression string, w io.Writer) (bool, error) {
	meta, lbls, err := d.GetMappedLabelMeta(ctx, label, bounds)
	if err != nil {
		return false, err
	}
	if meta == nil || len(meta.Blocks) == 0 || len(lbls) == 0 {
		return false, err
	}

	indices := make(dvid.IZYXSlice, len(meta.Blocks))
	totBlocks := 0
	for _, izyx := range meta.Blocks {
		if bounds.Block.BoundedX() || bounds.Block.BoundedY() {
			blockX, blockY, _, err := izyx.Unpack()
			if err != nil {
				return false, fmt.Errorf("Error decoding block %v: %v\n", izyx, err)
			}
			if bounds.Block.OutsideX(blockX) || bounds.Block.OutsideY(blockY) {
				continue
			}
		}
		indices[totBlocks] = izyx
		totBlocks++
	}
	if totBlocks == 0 {
		return false, nil
	}
	indices = indices[:totBlocks]

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return false, err
	}
	pbCh := make(chan *labels.PositionedBlock)
	errCh := make(chan error)
	go labels.WriteRLEs(lbls, w, pbCh, bounds, errCh)
	for _, izyx := range indices {
		tk := NewBlockTKeyByCoord(izyx)
		data, err := store.Get(ctx, tk)
		if err != nil {
			return false, err
		}
		blockData, _, err := dvid.DeserializeData(data, true)
		if err != nil {
			return false, err
		}
		var block labels.Block
		if err := block.UnmarshalBinary(blockData); err != nil {
			return false, err
		}
		chunkPt, err := izyx.ToChunkPoint3d()
		if err != nil {
			return false, err
		}
		pb := labels.PositionedBlock{
			Block: block,
			Coord: chunkPt,
		}
		pbCh <- &pb
	}
	err = <-errCh
	if err != nil {
		return false, err
	}

	dvid.Infof("[%s] labels %v: streamed %d of %d blocks within bounds\n", ctx, lbls, totBlocks, len(meta.Blocks))
	return true, nil
}

func (d *Data) WriteLegacyRLE(ctx *datastore.VersionedCtx, label uint64, b dvid.Bounds, compression string, format SparseVolFormat, w io.Writer) (found bool, err error) {
	var data []byte
	old := (format == FormatLegacySlowRLE)
	data, err = d.GetLegacyRLE(ctx, label, b, old)
	if err != nil {
		return
	}
	if len(data) == 0 {
		found = false
		return
	}
	found = true
	switch compression {
	case "":
		_, err = w.Write(data)
	case "lz4":
		compressed := make([]byte, lz4.CompressBound(data))
		var n, outSize int
		if outSize, err = lz4.Compress(data, compressed); err != nil {
			return
		}
		compressed = compressed[:outSize]
		n, err = w.Write(compressed)
		if n != outSize {
			err = fmt.Errorf("only able to write %d of %d lz4 compressed bytes\n", n, outSize)
		}
	case "gzip":
		gw := gzip.NewWriter(w)
		if _, err = gw.Write(data); err != nil {
			return
		}
		err = gw.Close()
	default:
		err = fmt.Errorf("unknown compression type %q", compression)
	}
	return
}

// GetLegacyRLE returns an encoded sparse volume given a label and an output format.
// If the old flag is true, the RLEs are computed using label volume instead of directly
// from the compressed label Blocks.
func (d *Data) GetLegacyRLE(ctx *datastore.VersionedCtx, label uint64, bounds dvid.Bounds, old bool) ([]byte, error) {
	meta, lbls, err := d.GetMappedLabelMeta(ctx, label, bounds)
	if err != nil {
		return nil, err
	}
	if old {
		return d.getLegacySlowRLEs(ctx, meta, lbls, bounds)
	}
	return d.getLegacyRLEs(ctx, meta, lbls, bounds)
}

//  The encoding has the following format where integers are little endian:
//
//    byte     Payload descriptor:
//               Bit 0 (LSB) - 8-bit grayscale
//               Bit 1 - 16-bit grayscale
//               Bit 2 - 16-bit normal
//               ...
//    uint8    Number of dimensions
//    uint8    Dimension of run (typically 0 = X)
//    byte     Reserved (to be used later)
//    uint32    0
//    uint32    # Spans
//    Repeating unit of:
//        int32   Coordinate of run start (dimension 0)
//        int32   Coordinate of run start (dimension 1)
//        int32   Coordinate of run start (dimension 2)
//        int32   Length of run
//        bytes   Optional payload dependent on first byte descriptor
//
func (d *Data) getLegacySlowRLEs(ctx *datastore.VersionedCtx, meta *Meta, lbls labels.Set, bounds dvid.Bounds) ([]byte, error) {
	// Write the sparse volume header
	buf := new(bytes.Buffer)
	buf.WriteByte(dvid.EncodingBinary)
	binary.Write(buf, binary.LittleEndian, uint8(3))  // # of dimensions
	binary.Write(buf, binary.LittleEndian, byte(0))   // dimension of run (X = 0)
	buf.WriteByte(byte(0))                            // reserved for later
	binary.Write(buf, binary.LittleEndian, uint32(0)) // Placeholder for # voxels
	binary.Write(buf, binary.LittleEndian, uint32(0)) // Placeholder for # spans

	indices := make(dvid.IZYXSlice, len(meta.Blocks))
	totBlocks := 0
	for _, izyx := range meta.Blocks {
		if bounds.Block.BoundedX() || bounds.Block.BoundedY() {
			blockX, blockY, _, err := izyx.Unpack()
			if err != nil {
				return nil, fmt.Errorf("Error decoding block %v: %v\n", izyx, err)
			}
			if bounds.Block.OutsideX(blockX) || bounds.Block.OutsideY(blockY) {
				continue
			}
		}
		indices[totBlocks] = izyx
		totBlocks++
	}
	if totBlocks == 0 {
		return nil, nil
	}
	indices = indices[:totBlocks]

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return nil, err
	}

	const blockDecoders = 10
	getCh := make(chan rleResult, 100)
	sendCh := make([]chan labelBlock, blockDecoders)
	for i := 0; i < blockDecoders; i++ {
		sendCh[i] = make(chan labelBlock, 10)
		go d.processBlocksToRLEs(lbls, bounds, sendCh[i], getCh)
	}

	// launch the RLE collector
	var wg sync.WaitGroup
	var numRuns uint32
	numBlocks := 0
	wg.Add(1)
	go func() {
		for {
			result := <-getCh
			numBlocks++
			if len(result.serialization) > 0 {
				if _, err := buf.Write(result.serialization); err != nil {
					dvid.Errorf("unable to write result of RLE serialization: %v\n", err)
					result.runs = 0
				}
			}
			numRuns += result.runs
			if numBlocks == totBlocks {
				wg.Done()
				return
			}
		}
	}()

	for _, izyx := range indices {
		tk := NewBlockTKeyByCoord(izyx)
		data, err := store.Get(ctx, tk)
		if err != nil {
			return nil, err
		}

		n := izyx.Hash(blockDecoders)
		sendCh[n] <- labelBlock{index: izyx, data: data}
	}

	wg.Wait()
	close(getCh)
	for i := 0; i < blockDecoders; i++ {
		close(sendCh[i])
	}

	if numRuns == 0 {
		return nil, nil // Couldn't find this out until we did voxel-level clipping
	}

	serialization := buf.Bytes()
	binary.LittleEndian.PutUint32(serialization[8:12], numRuns)
	dvid.Infof("[%s] labels %v: found %d blocks, %d runs, buf %d bytes\n", ctx, lbls, numBlocks, numRuns, len(serialization))
	return serialization, nil
}

//  The encoding has the following format where integers are little endian:
//
//    byte     Payload descriptor:
//               Bit 0 (LSB) - 8-bit grayscale
//               Bit 1 - 16-bit grayscale
//               Bit 2 - 16-bit normal
//               ...
//    uint8    Number of dimensions
//    uint8    Dimension of run (typically 0 = X)
//    byte     Reserved (to be used later)
//    uint32    0
//    uint32    # Spans
//    Repeating unit of:
//        int32   Coordinate of run start (dimension 0)
//        int32   Coordinate of run start (dimension 1)
//        int32   Coordinate of run start (dimension 2)
//        int32   Length of run
//        bytes   Optional payload dependent on first byte descriptor
//
func (d *Data) getLegacyRLEs(ctx *datastore.VersionedCtx, meta *Meta, lbls labels.Set, bounds dvid.Bounds) ([]byte, error) {
	buf := new(bytes.Buffer)
	buf.WriteByte(dvid.EncodingBinary)
	binary.Write(buf, binary.LittleEndian, uint8(3))  // # of dimensions
	binary.Write(buf, binary.LittleEndian, byte(0))   // dimension of run (X = 0)
	buf.WriteByte(byte(0))                            // reserved for later
	binary.Write(buf, binary.LittleEndian, uint32(0)) // Placeholder for # voxels
	binary.Write(buf, binary.LittleEndian, uint32(0)) // Placeholder for # spans

	indices := make(dvid.IZYXSlice, len(meta.Blocks))

	totBlocks := 0
	for _, izyx := range meta.Blocks {
		if bounds.Block.BoundedX() || bounds.Block.BoundedY() || bounds.Block.BoundedZ() {
			blockX, blockY, blockZ, err := izyx.Unpack()
			if err != nil {
				return nil, fmt.Errorf("Error decoding block %v: %v\n", izyx, err)
			}
			if bounds.Block.OutsideX(blockX) || bounds.Block.OutsideY(blockY) || bounds.Block.OutsideZ(blockZ) {
				continue
			}
		}
		indices[totBlocks] = izyx
		totBlocks++
	}
	if totBlocks == 0 {
		return nil, nil
	}
	indices = indices[:totBlocks]

	store, err := d.GetOrderedKeyValueDB()
	if err != nil {
		return nil, err
	}
	pbCh := make(chan *labels.PositionedBlock)
	errCh := make(chan error)
	go labels.WriteRLEs(lbls, buf, pbCh, bounds, errCh)
	for _, izyx := range indices {
		tk := NewBlockTKeyByCoord(izyx)
		data, err := store.Get(ctx, tk)
		if err != nil {
			return nil, err
		}
		blockData, _, err := dvid.DeserializeData(data, true)
		if err != nil {
			return nil, err
		}
		var block labels.Block
		if err := block.UnmarshalBinary(blockData); err != nil {
			return nil, err
		}
		chunkPt, err := izyx.ToChunkPoint3d()
		if err != nil {
			return nil, err
		}
		pb := labels.PositionedBlock{
			Block: block,
			Coord: chunkPt,
		}
		pbCh <- &pb
	}
	close(pbCh)
	err = <-errCh
	if err != nil {
		return nil, err
	}

	serialization := buf.Bytes()
	numRuns := uint32(len(serialization)-12) >> 4
	if numRuns == 0 {
		return nil, nil // Couldn't find this out until we did voxel-level clipping
	}

	binary.LittleEndian.PutUint32(serialization[8:12], numRuns)
	dvid.Infof("[%s] labels %v: found %d of %d blocks within bounds, %d runs, serialized %d bytes\n", ctx, lbls, totBlocks, len(meta.Blocks), numRuns, len(serialization))
	return serialization, nil
}

// GetSparseCoarseVol returns an encoded sparse volume given a label.  The encoding has the
// following format where integers are little endian:
// 		byte     Set to 0
// 		uint8    Number of dimensions
// 		uint8    Dimension of run (typically 0 = X)
// 		byte     Reserved (to be used later)
// 		uint32    # Blocks [TODO.  0 for now]
// 		uint32    # Spans
// 		Repeating unit of:
//     		int32   Block coordinate of run start (dimension 0)
//     		int32   Block coordinate of run start (dimension 1)
//     		int32   Block coordinate of run start (dimension 2)
//     		int32   Length of run
//
func (d *Data) GetSparseCoarseVol(ctx *datastore.VersionedCtx, label uint64, bounds dvid.Bounds) ([]byte, error) {
	mapping := labels.LabelMap(ctx.InstanceVersion())
	if mapping != nil {
		// Check if this label has been merged.
		if mapped, found := mapping.FinalLabel(label); found {
			dvid.Debugf("Label %d has already been merged into label %d.  Skipping sparse vol retrieval.\n", label, mapped)
			return nil, nil
		}
	}

	// Get set of all labels that have been merged to this given label.
	var lbls labels.Set
	if mapping == nil {
		lbls = labels.Set{label: struct{}{}}
	} else {
		lbls = mapping.ConstituentLabels(label)
	}

	// Get the block indices for the set of labels.
	meta, err := d.GetLabelMeta(ctx, lbls, bounds)
	if err != nil {
		return nil, err
	}

	// Create the sparse volume header
	buf := new(bytes.Buffer)
	buf.WriteByte(dvid.EncodingBinary)
	binary.Write(buf, binary.LittleEndian, uint8(3))  // # of dimensions
	binary.Write(buf, binary.LittleEndian, byte(0))   // dimension of run (X = 0)
	buf.WriteByte(byte(0))                            // reserved for later
	binary.Write(buf, binary.LittleEndian, uint32(0)) // Placeholder for # voxels
	binary.Write(buf, binary.LittleEndian, uint32(0)) // Placeholder for # spans

	spans, err := meta.Blocks.WriteSerializedRLEs(buf)
	if err != nil {
		return nil, err
	}
	serialization := buf.Bytes()
	binary.LittleEndian.PutUint32(serialization[8:12], spans) // Placeholder for # spans

	return serialization, nil
}