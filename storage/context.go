/*
	This file contains types that manage valid key space within a DVID key-value database
	and support versioning.
*/

package storage

import (
	"fmt"

	"github.com/janelia-flyem/dvid/dvid"
)

// Context allows encapsulation of data that defines the partitioning of the DVID
// key space.  To prevent conflicting implementations, Context is an opaque interface type
// that requires use of an implementation from the storage package, either directly or
// through embedding.  The storage engines should accept a nil Context, which allows
// direct saving of a raw key without use of a ConstructKey() transformation.
//
// For a description of Go language opaque types, see the following:
//   http://www.onebigfluke.com/2014/04/gos-power-is-in-emergent-behavior.html
type Context interface {
	// VersionID returns the local version ID of the DAG node being operated on.
	// If not versioned, the version is the root ID.
	VersionID() dvid.VersionID

	// ConstructKey takes an index, a type-specific slice of bytes, and generates a
	// namespaced key that fits with the DVID-wide key space partitioning.
	ConstructKey(index []byte) []byte

	// IndexFromKey returns an index, the type-specific component of the key, from
	// an entire storage key.
	IndexFromKey(key []byte) (index []byte, err error)

	// String prints a description of the Context
	String() string

	// Versioned is true if this Context is also a VersionedContext.
	Versioned() bool

	// Enforces opaque data type.
	implementsOpaque()
}

// VersionedContext extends a Context with the minimal functions necessary to handle
// versioning in storage engines.  For DataContext, only GetIterator() needs to be
// implemented at higher levels where the version DAG is available.
type VersionedContext interface {
	Context

	// GetIterator returns an iterator up a version DAG.
	GetIterator() (VersionIterator, error)

	// Returns lower bound key for versions of given byte slice index.
	MinVersionKey(index []byte) (key []byte, err error)

	// Returns upper bound key for versions of given byte slice index.
	MaxVersionKey(index []byte) (key []byte, err error)

	// VersionedKeyValue returns the key-value pair corresponding to this key's version
	// given a list of key-value pairs across many versions.  If no suitable key-value
	// pair is found, nil is returned.
	VersionedKeyValue([]*KeyValue) (*KeyValue, error)
}

// VersionIterator allows iteration through ancestors of version DAG.  It is assumed
// only one parent is needed based on how merge operations are handled.
type VersionIterator interface {
	Valid() bool
	VersionID() dvid.VersionID
	Next()
}

// ---- Context implementations -----

const (
	metadataKeyPrefix byte = iota
	dataKeyPrefix
)

// MetadataContext is an implementation of Context for MetadataContext persistence.
type MetadataContext struct{}

func NewMetadataContext() MetadataContext {
	return MetadataContext{}
}

func (ctx MetadataContext) implementsOpaque() {}

func (ctx MetadataContext) VersionID() dvid.VersionID {
	return 0 // Only one version of Metadata
}

func (ctx MetadataContext) ConstructKey(index []byte) []byte {
	return append([]byte{metadataKeyPrefix}, index...)
}

func (ctx MetadataContext) IndexFromKey(key []byte) ([]byte, error) {
	if key[0] != metadataKeyPrefix {
		return nil, fmt.Errorf("Cannot extract MetadataContext index from different key")
	}
	return key[1:], nil
}

func (ctx MetadataContext) String() string {
	return "Metadata Context"
}

func (ctx MetadataContext) Versioned() bool {
	return false
}

// DataContext supports both unversioned and versioned data persistence.
type DataContext struct {
	data    dvid.Data
	version dvid.VersionID
}

// NewDataContext provides a way for datatypes to create a Context that adheres to DVID
// key space partitioning.  Since Context and VersionedContext interfaces are opaque, i.e., can
// only be implemented within package storage, we force compatible implementations to embed
// DataContext and initialize it via this function.
func NewDataContext(data dvid.Data, versionID dvid.VersionID) *DataContext {
	return &DataContext{data, versionID}
}

// KeyToIndexZYX parses a key under a DataContext and returns the index as a dvid.IndexZYX
func KeyToIndexZYX(k []byte) (dvid.IndexZYX, error) {
	var zyx dvid.IndexZYX
	ctx := &DataContext{}
	indexBytes, err := ctx.IndexFromKey(k)
	if err != nil {
		return zyx, fmt.Errorf("Cannot convert key %v to IndexZYX: %s\n", k, err.Error())
	}
	if err := zyx.IndexFromBytes(indexBytes); err != nil {
		return zyx, fmt.Errorf("Cannot recover ZYX index from key %v: %s\n", k, err.Error())
	}
	return zyx, nil
}

// ---- storage.Context implementation

func (ctx *DataContext) implementsOpaque() {}

func (ctx *DataContext) VersionID() dvid.VersionID {
	return ctx.version
}

func (ctx *DataContext) ConstructKey(index []byte) []byte {
	key := append([]byte{dataKeyPrefix}, ctx.data.InstanceID().Bytes()...)
	key = append(key, index...)
	return append(key, ctx.version.Bytes()...)
}

func (ctx *DataContext) IndexFromKey(key []byte) ([]byte, error) {
	if key[0] != dataKeyPrefix {
		return nil, fmt.Errorf("Cannot extract DataContext index from different key")
	}
	start := 1 + dvid.InstanceIDSize
	end := len(key) - dvid.VersionIDSize
	return key[start:end], nil
}

func (ctx *DataContext) String() string {
	return fmt.Sprintf("Data Context for %q (local id %d, version id %d)", ctx.data.DataName(),
		ctx.data.InstanceID(), ctx.version)
}

func (ctx *DataContext) Versioned() bool {
	return ctx.data.Versioned()
}

// ----- partial storage.VersionedContext implementation

// Returns lower bound key for versions of given byte slice key representation.
func (ctx *DataContext) MinVersionKey(index []byte) ([]byte, error) {
	key := append([]byte{dataKeyPrefix}, ctx.data.InstanceID().Bytes()...)
	key = append(key, index...)
	return append(key, dvid.VersionID(0).Bytes()...), nil
}

// Returns upper bound key for versions of given byte slice key representation.
func (ctx *DataContext) MaxVersionKey(index []byte) ([]byte, error) {
	key := append([]byte{dataKeyPrefix}, ctx.data.InstanceID().Bytes()...)
	key = append(key, index...)
	return append(key, dvid.VersionID(dvid.MaxVersionID).Bytes()...), nil
}